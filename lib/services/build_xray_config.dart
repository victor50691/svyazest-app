import 'dart:convert';

import 'vpn_uri_parser.dart';

/// Builds a minimal Xray JSON config: one local HTTP inbound (so the Dart
/// side can drive the probe through plain HttpClient.findProxy -- Dart has
/// no built-in SOCKS5 client) and one outbound for the parsed key. No
/// `dialerProxy`/upstream chaining like the server side's
/// services/vpn_checker.py uses: there is no residential-proxy hop here,
/// this device's own mobile connection is the vantage point.
///
/// The outbound shapes below are ported field-for-field from that module's
/// generate_xray_config, because the core embedded here (xraylib.aar) is the
/// same Xray v26.3.27 build the bot runs: same key, same outbound, same
/// verdict regardless of which vantage point ran the check. Each of those
/// fields exists because omitting it produced a *false* verdict, not an
/// error -- see the comments at each site.
Map<String, dynamic> buildXrayConfig(ParsedVpnKey key, int localPort) {
  return {
    'log': {'loglevel': 'warning'},
    // Resolved by the core's own dialer, i.e. through the cellular-bound
    // sockets; 'localhost' = the system resolver as a last resort.
    'dns': {
      'servers': ['8.8.8.8', '1.1.1.1', 'localhost'],
      'queryStrategy': 'UseIPv4',
    },
    'inbounds': [
      {
        'listen': '127.0.0.1',
        'port': localPort,
        'protocol': 'http',
        'settings': {},
      },
    ],
    'outbounds': [_buildOutbound(key)],
  };
}

Map<String, dynamic> _buildOutbound(ParsedVpnKey key) {
  final p = key.params;
  final host = key.address ?? '';

  switch (key.protocol) {
    case 'vless':
      final user = <String, dynamic>{'id': key.uuid, 'encryption': 'none'};
      // XTLS flow (xtls-rprx-vision): required by most modern VLESS+REALITY
      // keys. Without it the handshake still completes and no traffic ever
      // flows, which reads as "blocked" when the key is fine.
      if ((p['flow'] ?? '').isNotEmpty) user['flow'] = p['flow'];
      // VLESS Encryption (encryption=mlkem768x25519plus...): hardcoding
      // 'none' against a server that requires it fails every connection.
      if ((p['encryption'] ?? '').isNotEmpty) user['encryption'] = p['encryption'];
      final stream = <String, dynamic>{};
      _applyTransport(stream, key.network, p, host);
      final security = p['security'] ?? 'none';
      if (security == 'reality') {
        _applyReality(stream, p, host);
      } else if (security == 'tls') {
        _applyTls(stream, p, host);
      }
      final outbound = <String, dynamic>{
        'protocol': 'vless',
        'settings': {
          'vnext': [
            {'address': host, 'port': key.port, 'users': [user]},
          ],
        },
        'streamSettings': stream,
      };
      _applyMux(outbound, p);
      return outbound;

    case 'vmess':
      final stream = <String, dynamic>{};
      _applyTransport(stream, key.network, p, host);
      if (p['security'] == 'tls') _applyTls(stream, p, host);
      final outbound = <String, dynamic>{
        'protocol': 'vmess',
        'settings': {
          'vnext': [
            {
              'address': host,
              'port': key.port,
              'users': [
                {'id': key.uuid, 'alterId': key.alterId, 'security': key.vmessSecurity},
              ],
            },
          ],
        },
        'streamSettings': stream,
      };
      _applyMux(outbound, p);
      return outbound;

    case 'trojan':
      final stream = <String, dynamic>{};
      _applyTransport(stream, key.network, p, host);
      // Trojan is TLS-by-definition in its spec -- share links normally omit
      // security= entirely and it still means TLS, unlike VLESS where
      // absence means plaintext. Only an explicit security=none turns it
      // off; security=reality gets real REALITY settings instead of a
      // plain-TLS handshake into a void.
      final security = p['security'] ?? 'tls';
      if (security == 'reality') {
        _applyReality(stream, p, host);
      } else if (security != 'none') {
        _applyTls(stream, p, host);
      } else {
        stream['security'] = 'none';
      }
      final outbound = <String, dynamic>{
        'protocol': 'trojan',
        'settings': {
          'servers': [
            {'address': host, 'port': key.port, 'password': key.password},
          ],
        },
        'streamSettings': stream,
      };
      _applyMux(outbound, p);
      return outbound;

    case 'shadowsocks':
      final stream = <String, dynamic>{};
      _applyTransport(stream, key.network, p, host);
      if (p['security'] == 'tls') _applyTls(stream, p, host);
      return {
        'protocol': 'shadowsocks',
        'settings': {
          'servers': [
            {
              'address': host,
              'port': key.port,
              'method': key.method ?? 'aes-256-gcm',
              'password': key.password ?? '',
            },
          ],
        },
        'streamSettings': stream,
      };

    case 'hysteria2':
      // Xray's Hysteria2 outbound is protocol "hysteria" (NOT "hysteria2" --
      // that name is rejected as an unknown protocol), transport network
      // "hysteria", and per HysteriaClientConfig (infra/conf/hysteria.go)
      // `settings` takes only {address, port, version}: a servers[] array
      // there panics its Build() on a nil pointer, and the auth string
      // belongs in streamSettings.hysteriaSettings instead. Hysteria2 rides
      // the core's HTTP/3 QUIC transport, so ALPN must be h3 on both ends --
      // the server forces it itself (tls.WithNextProto("h3")), so it is set
      // explicitly here rather than left to library defaults.
      //
      // Reachable from this app and not from the bot's PoPs: the check runs
      // over UDP on the phone's cellular link, which the residential SOCKS5
      // pool cannot carry at all.
      return {
        'protocol': 'hysteria',
        'settings': {'address': host, 'port': key.port, 'version': 2},
        'streamSettings': {
          'network': 'hysteria',
          'security': 'tls',
          'tlsSettings': {
            'serverName': p['sni']?.isNotEmpty == true ? p['sni'] : host,
            'alpn': ['h3'],
          },
          'hysteriaSettings': {'auth': key.password ?? '', 'version': 2},
        },
      };

    default:
      throw ArgumentError('Unsupported protocol: ${key.protocol}');
  }
}

/// Attaches the right `<network>Settings` block. Shared by VLESS/VMess/
/// Trojan/SS -- they all sit on the same Xray transport layer, only the
/// proxy-protocol settings differ. Mirrors vpn_checker.py's
/// _apply_transport_settings, including its normalizations (raw->tcp,
/// kcp->mkcp).
void _applyTransport(Map<String, dynamic> stream, String rawNetwork, Map<String, String> p, String host) {
  var network = rawNetwork.isEmpty ? 'tcp' : rawNetwork;

  if ((network == 'tcp' || network == 'raw') && p['headerType'] == 'http') {
    // HTTP-header obfuscation: the server fronts as a plain HTTP service and
    // expects each connection to open with a real-looking request. Dialing
    // bare TCP against such a server fails as if filtered.
    final request = <String, dynamic>{
      'path': [p['path']?.isNotEmpty == true ? p['path'] : '/'],
    };
    if ((p['host'] ?? '').isNotEmpty) {
      request['headers'] = {'Host': p['host']!.split(',')};
    }
    stream['tcpSettings'] = {
      'header': {'type': 'http', 'request': request},
    };
    network = 'tcp';
  } else if (network == 'ws') {
    final headers = <String, dynamic>{};
    if ((p['host'] ?? '').isNotEmpty) headers['Host'] = p['host'];
    stream['wsSettings'] = {
      'path': p['path']?.isNotEmpty == true ? p['path'] : '/',
      'headers': headers,
    };
  } else if (network == 'grpc') {
    final grpc = <String, dynamic>{
      'serviceName': p['serviceName'] ?? p['path'] ?? '',
      'multiMode': p['mode'] == 'multi',
    };
    if ((p['authority'] ?? '').isNotEmpty) grpc['authority'] = p['authority'];
    stream['grpcSettings'] = grpc;
  } else if (network == 'xhttp') {
    final xhttp = <String, dynamic>{
      'path': p['path']?.isNotEmpty == true ? p['path'] : '/',
      'host': p['host']?.isNotEmpty == true ? p['host'] : host,
      'mode': p['mode']?.isNotEmpty == true ? p['mode'] : 'auto',
    };
    // `extra` is a URL-encoded JSON blob some panels append with XHTTP
    // xmux/padding/session settings. Xray takes it verbatim under the same
    // key. Dropping it completes the TLS handshake but gets the actual
    // traffic rejected -- indistinguishable from a block -- while real
    // clients, which do parse it, work on the same key.
    final extra = p['extra'];
    if (extra != null && extra.isNotEmpty) {
      try {
        xhttp['extra'] = _sanitizeXhttpExtra(jsonDecode(extra));
      } catch (_) {
        // Not valid JSON: ignore it rather than failing the whole config.
      }
    }
    stream['xhttpSettings'] = xhttp;
  } else if (network == 'httpupgrade') {
    stream['httpupgradeSettings'] = {
      'path': p['path']?.isNotEmpty == true ? p['path'] : '/',
      'host': p['host']?.isNotEmpty == true ? p['host'] : host,
    };
  } else if (network == 'kcp' || network == 'mkcp') {
    // mKCP rides UDP. Untestable through the bot's SOCKS5 pool, testable
    // here: xraylib binds UDP sockets to the cellular network too.
    //
    // No kcpSettings block at all: this core removed `header` and `seed`
    // (the only two things a share link ever carries) and rejects a config
    // naming either, so the defaults are all that is left to dial with. A
    // key that does carry them never reaches this point -- the parser
    // refuses it as untestable rather than dialing a channel the server
    // isn't speaking.
    network = 'mkcp';
  }
  // tcp / raw: nothing extra needed. h2/http/quic never reach here -- the
  // parser rejects them as removed from the core.

  stream['network'] = network;
}

/// Some panels emit whole-number XHTTP `extra` fields (e.g. cMaxLifetimeMs)
/// as JSON floats like 0.0. Xray's unmarshaler for those is strict -- it
/// takes a plain integer or a "1-2" range string and rejects 0.0 with
/// "Invalid integer range", failing the ENTIRE config load. Recurses because
/// the offending field can sit nested inside xmux.
dynamic _sanitizeXhttpExtra(dynamic value) {
  if (value is double && value == value.roundToDouble()) return value.toInt();
  if (value is Map) {
    return value.map((k, v) => MapEntry(k, _sanitizeXhttpExtra(v)));
  }
  if (value is List) return value.map(_sanitizeXhttpExtra).toList();
  return value;
}

void _applyTls(Map<String, dynamic> stream, Map<String, String> p, String host) {
  final sni = p['sni']?.isNotEmpty == true
      ? p['sni']
      : (p['host']?.isNotEmpty == true ? p['host'] : host);
  // allowInsecure was REMOVED in this core (replaced by
  // pinnedPeerCertSha256): passing it fails the config load outright with
  // "The feature allowInsecure has been removed", so certificates are
  // validated normally -- same as the server side.
  final tls = <String, dynamic>{'serverName': sni};
  if ((p['alpn'] ?? '').isNotEmpty) tls['alpn'] = p['alpn']!.split(',');
  if ((p['fp'] ?? '').isNotEmpty) tls['fingerprint'] = p['fp'];
  // ECH config blob (v2rayN's "ech" param).
  if ((p['ech'] ?? '').isNotEmpty) tls['echConfigList'] = p['ech'];
  stream['security'] = 'tls';
  stream['tlsSettings'] = tls;
}

/// security=reality. Used by VLESS and Trojan alike -- trojan+reality is a
/// valid client config, and sending it down the plain-TLS path handshakes
/// into a void a REALITY server never answers.
void _applyReality(Map<String, dynamic> stream, Map<String, String> p, String host) {
  final reality = <String, dynamic>{
    'serverName': p['sni']?.isNotEmpty == true ? p['sni'] : host,
    'fingerprint': p['fp']?.isNotEmpty == true ? p['fp'] : 'chrome',
    'publicKey': p['pbk'] ?? '',
    'shortId': p['sid'] ?? '',
    'spiderX': p['spx']?.isNotEmpty == true ? p['spx'] : '/',
  };
  // Post-quantum certificate verify (share-link "pqv"), carried for parity
  // with real clients.
  if ((p['pqv'] ?? '').isNotEmpty) reality['mldsa65Verify'] = p['pqv'];
  stream['security'] = 'reality';
  stream['realitySettings'] = reality;
}

/// Not part of any standard share-link format, but some panels append a
/// `mux=` param -- honor it when present instead of guessing.
void _applyMux(Map<String, dynamic> outbound, Map<String, String> p) {
  if (p['mux'] == '1' || p['mux'] == 'true' || p['mux'] == 'on') {
    outbound['mux'] = {
      'enabled': true,
      'concurrency': int.tryParse(p['muxConcurrency'] ?? '') ?? 8,
    };
  }
}
