import 'dart:convert';

/// Parses VLESS/VMESS/TROJAN/SHADOWSOCKS/HYSTERIA2 share links into a
/// normalized shape that build_xray_config.dart turns into an Xray outbound.
///
/// Deliberately mirrors the server side's services/vpn_checker.py
/// (parse_vpn_uri / _parse_vmess_uri): the embedded core here is the exact
/// same Xray build the bot runs (v26.3.27, commit d2758a0), so a key must
/// produce the same outbound on the phone as it does on a PoP -- otherwise
/// the same key gets two different verdicts depending on which vantage
/// point happened to test it. Like that module, everything the link carries
/// is kept in [params] verbatim rather than cherry-picking a few fields:
/// dropping `flow`, `encryption`, `fp` or XHTTP's `extra` still completes a
/// handshake, which is exactly what makes those bugs look like a DPI block
/// instead of a client-side omission.
///
/// What a phone can dial that a PoP cannot: this app's checks run over the
/// cellular link with every socket bound by NativePlugin.kt (including UDP
/// ones -- xraylib registers an internet.RegisterDialerController), so
/// UDP/QUIC keys (Hysteria2) and UDP transports (mKCP) are fully testable
/// here. The bot rejects those only because its residential SOCKS5 pool has
/// no UDP ASSOCIATE, which is not a limitation of this vantage point.
class ParsedVpnKey {
  ParsedVpnKey({
    required this.protocol,
    required this.raw,
    this.unsupportedReason,
    this.address,
    this.port,
    this.uuid,
    this.password,
    this.method,
    this.alterId = 0,
    this.vmessSecurity = 'auto',
    this.name = '',
    Map<String, String>? params,
  }) : params = params ?? const {};

  final String protocol; // vless | vmess | trojan | shadowsocks | hysteria2
  final String raw;
  final String? unsupportedReason;

  final String? address;
  final int? port;
  final String? uuid; // vless / vmess
  final String? password; // trojan / shadowsocks / hysteria2 auth
  final String? method; // shadowsocks cipher
  final int alterId; // vmess legacy
  final String vmessSecurity; // vmess "scy"
  final String name; // link fragment / vmess "ps", for logs

  /// Everything the share link carried, decoded but otherwise untouched:
  /// type, security, sni, fp, alpn, pbk, sid, spx, pqv, ech, flow,
  /// encryption, path, host, serviceName, mode, authority, seed,
  /// headerType, extra, mux... VMess links (a base64 JSON blob) are
  /// normalized into these same keys.
  final Map<String, String> params;

  String get network => params['type']?.toLowerCase() ?? 'tcp';
  String? get security => params['security'];

  bool get isSupported => unsupportedReason == null && address != null && port != null;
}

/// Schemes Xray-core cannot dial as a client at all. Reported honestly
/// instead of being attempted and failing as something that reads like a
/// block. (WireGuard has an Xray outbound but no share-link format to build
/// it from -- a `wireguard://` link is not something the core can consume.)
const _unsupportedSchemes = {
  'tuic', 'juicity',
  'hysteria', // v1: gone from the core, v2 is a separate scheme we do dial
  'wireguard', 'wg', 'ssr', 'snell',
};

/// Untestable reasons travel to the bot in the job result and end up in the
/// user's report, so they are machine codes in `code:detail` form that
/// telegram-bot/main.py translates (see _device_check_line) -- not English
/// prose a Russian-, Chinese- or Farsi-speaking user would be shown raw.
const _reasonSchemeUnsupported = 'scheme_unsupported_by_core';
const _reasonTransportRemoved = 'transport_removed_from_core';
const _reasonMkcpRemoved = 'mkcp_header_seed_removed_from_core';

/// Transports Xray-core REMOVED (PrintRemovedFeatureError in
/// infra/conf/transport_internet.go, confirmed on the 26.3.27 build shipped
/// in xraylib.aar): naming one fails the whole config load, so the key is
/// untestable by any current core, not blocked. Same list as the server
/// side's REMOVED_TRANSPORTS.
const _removedTransports = {'h2', 'http', 'quic'};

/// mKCP's obfuscation parameters went the same way in this core: KCPConfig
/// .Build() errors out with "The feature mkcp header & seed has been removed
/// and migrated to finalmask/udp header-* & mkcp-original & mkcp-aes128gcm"
/// as soon as `header` or `seed` is present (verified against the 26.3.27
/// binary). A key carrying them describes a channel this core can no longer
/// speak, and dialing it without them would hand the user a confident
/// "doesn't work" for a key a v2rayN build still dials fine -- so it is
/// reported as untestable instead. Plain `type=kcp` with no obfuscation is
/// still built and dialed normally.
bool _usesRemovedMkcpFeatures(Map<String, String> p) {
  final header = (p['headerType'] ?? '').toLowerCase();
  return (p['seed'] ?? '').isNotEmpty || (header.isNotEmpty && header != 'none');
}

ParsedVpnKey? parseVpnUri(String uri) {
  final schemeMatch = RegExp(r'^([a-zA-Z0-9+.-]+)://').firstMatch(uri);
  if (schemeMatch == null) return null;
  final scheme = schemeMatch.group(1)!.toLowerCase();

  if (_unsupportedSchemes.contains(scheme)) {
    return ParsedVpnKey(
      protocol: scheme,
      raw: uri,
      unsupportedReason: '$_reasonSchemeUnsupported:$scheme',
    );
  }

  try {
    final ParsedVpnKey key;
    switch (scheme) {
      case 'vless':
        key = _parseVless(uri);
        break;
      case 'trojan':
        key = _parseTrojan(uri);
        break;
      case 'vmess':
        key = _parseVmess(uri);
        break;
      case 'ss':
      case 'shadowsocks':
        key = _parseShadowsocks(uri);
        break;
      case 'hysteria2':
      case 'hy2':
        key = _parseHysteria2(uri);
        break;
      default:
        return null;
    }
    final transport = key.network;
    if (_removedTransports.contains(transport)) {
      return ParsedVpnKey(
        protocol: key.protocol,
        raw: uri,
        unsupportedReason: '$_reasonTransportRemoved:$transport',
      );
    }
    if ((transport == 'kcp' || transport == 'mkcp') && _usesRemovedMkcpFeatures(key.params)) {
      return ParsedVpnKey(
        protocol: key.protocol,
        raw: uri,
        unsupportedReason: '$_reasonMkcpRemoved:$transport',
      );
    }
    return key;
  } catch (e) {
    return ParsedVpnKey(protocol: scheme, raw: uri, unsupportedReason: 'parse_error:$e');
  }
}

/// A link's query string, decoded. Kept whole so build_xray_config.dart can
/// honor fields this parser has no named slot for.
Map<String, String> _queryParams(Uri u) => Map<String, String>.from(u.queryParameters);

/// The `#name` tag. A fragment truncated mid-escape (Telegram's 256-char
/// inline cap does this) leaves a literal '%' behind and decodes to
/// garbage -- fall back to the host, exactly like parse_vpn_uri does.
String _linkName(Uri u, String host) {
  if (u.fragment.isEmpty) return host;
  try {
    final decoded = Uri.decodeComponent(u.fragment);
    return decoded.contains('%') ? host : decoded;
  } catch (_) {
    return host;
  }
}

ParsedVpnKey _parseVless(String raw) {
  final u = Uri.parse(raw);
  return ParsedVpnKey(
    protocol: 'vless',
    raw: raw,
    uuid: Uri.decodeComponent(u.userInfo),
    address: u.host,
    port: u.hasPort ? u.port : 443,
    name: _linkName(u, u.host),
    params: _queryParams(u),
  );
}

ParsedVpnKey _parseTrojan(String raw) {
  final u = Uri.parse(raw);
  return ParsedVpnKey(
    protocol: 'trojan',
    raw: raw,
    password: Uri.decodeComponent(u.userInfo),
    address: u.host,
    port: u.hasPort ? u.port : 443,
    name: _linkName(u, u.host),
    params: _queryParams(u),
  );
}

/// vmess://BASE64({v,ps,add,port,id,aid,scy,net,type,host,path,tls,sni,alpn,fp}),
/// the v2rayN convention. Flattened into the same param names the
/// query-string protocols use so the config builder has one code path.
ParsedVpnKey _parseVmess(String raw) {
  final j = jsonDecode(utf8.decode(_b64Bytes(raw.substring('vmess://'.length))))
      as Map<String, dynamic>;
  String s(Object? v) => (v ?? '').toString();

  final host = s(j['add']);
  // v2rayN writes "tls" for TLS and "" for plaintext, but some exporters
  // write the literal "none" -- a truthiness check reads that as TLS and
  // wraps a plaintext key in a handshake its server never answers.
  final tlsField = s(j['tls']).toLowerCase();
  final params = <String, String>{
    'type': s(j['net']).isEmpty ? 'tcp' : s(j['net']),
    'headerType': s(j['type']).isEmpty ? 'none' : s(j['type']),
    'path': s(j['path']),
    'host': s(j['host']),
    'sni': s(j['sni']).isNotEmpty ? s(j['sni']) : s(j['host']),
    'security': (tlsField == 'tls' || tlsField == 'true' || tlsField == '1') ? 'tls' : 'none',
    'alpn': s(j['alpn']),
    // uTLS fingerprint: every real client carries it, and dropping it tests
    // the key with Go's default ClientHello -- a fingerprint DPI singles out.
    'fp': s(j['fp']),
    // vmess grpc links reuse "path" for the gRPC serviceName.
    'serviceName': s(j['path']),
  }..removeWhere((_, v) => v.isEmpty);

  return ParsedVpnKey(
    protocol: 'vmess',
    raw: raw,
    uuid: s(j['id']),
    address: host,
    port: int.tryParse(s(j['port'])) ?? 443,
    alterId: int.tryParse(s(j['aid'])) ?? 0,
    vmessSecurity: s(j['scy']).isEmpty ? 'auto' : s(j['scy']),
    name: s(j['ps']).isEmpty ? host : s(j['ps']),
    params: params,
  );
}

/// ss://BASE64(method:password)@host:port?params#tag and the legacy
/// ss://BASE64(method:password@host:port) form. SS rides the shared
/// transport layer too (ss over ws/tls exists in the wild), so query params
/// are carried through rather than assuming bare TCP.
ParsedVpnKey _parseShadowsocks(String raw) {
  final body = raw.substring(raw.indexOf('://') + 3);
  final hashIdx = body.indexOf('#');
  final tag = hashIdx >= 0 ? body.substring(hashIdx + 1) : '';
  var withoutTag = hashIdx >= 0 ? body.substring(0, hashIdx) : body;

  String query = '';
  final qIdx = withoutTag.indexOf('?');
  if (qIdx >= 0) {
    query = withoutTag.substring(qIdx + 1);
    withoutTag = withoutTag.substring(0, qIdx);
  }

  String userInfo;
  String hostPort;
  if (withoutTag.contains('@')) {
    final at = withoutTag.lastIndexOf('@');
    userInfo = Uri.decodeComponent(withoutTag.substring(0, at));
    hostPort = withoutTag.substring(at + 1);
    if (!userInfo.contains(':')) {
      // SIP002: the userinfo is base64(method:password).
      userInfo = utf8.decode(_b64Bytes(userInfo));
    }
  } else {
    final decoded = utf8.decode(_b64Bytes(withoutTag));
    final at = decoded.lastIndexOf('@');
    userInfo = decoded.substring(0, at);
    hostPort = decoded.substring(at + 1);
  }

  final colon = userInfo.indexOf(':');
  final method = colon >= 0 ? userInfo.substring(0, colon) : 'aes-256-gcm';
  final password = colon >= 0 ? userInfo.substring(colon + 1) : userInfo;

  // IPv6 literals arrive as [::1]:443.
  final portColon = hostPort.lastIndexOf(':');
  final host = hostPort.substring(0, portColon).replaceAll(RegExp(r'^\[|\]$'), '');
  final port = int.tryParse(hostPort.substring(portColon + 1));

  final params = query.isEmpty ? <String, String>{} : Uri.splitQueryString(query);
  return ParsedVpnKey(
    protocol: 'shadowsocks',
    raw: raw,
    method: method,
    password: password,
    address: host,
    port: port,
    name: tag.isEmpty ? host : Uri.decodeComponent(tag),
    params: Map<String, String>.from(params),
  );
}

ParsedVpnKey _parseHysteria2(String raw) {
  final u = Uri.parse(raw.replaceFirst(RegExp(r'^hy2://', caseSensitive: false), 'hysteria2://'));
  return ParsedVpnKey(
    protocol: 'hysteria2',
    raw: raw,
    // The whole userinfo is the auth string: Hysteria2 allows "user:pass"
    // there and it is passed to the server verbatim, so splitting on ':'
    // would send half a credential.
    password: Uri.decodeComponent(u.userInfo),
    address: u.host,
    port: u.hasPort ? u.port : 443,
    name: _linkName(u, u.host),
    params: _queryParams(u),
  );
}

/// Decodes either base64 alphabet, padded or not. Share links use both: the
/// URL-safe one (-_ instead of +/) is what most subscription exporters emit
/// and plain base64.decode rejects it outright.
List<int> _b64Bytes(String blob) {
  final normalized = blob.replaceAll('-', '+').replaceAll('_', '/').trim();
  return base64.decode(base64.normalize(normalized));
}
