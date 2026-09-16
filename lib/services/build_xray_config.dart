import 'vpn_uri_parser.dart';

/// Builds a minimal Xray JSON config: one local HTTP inbound (so the Dart
/// side can drive the probe through plain HttpClient.findProxy -- Dart has
/// no built-in SOCKS5 client) and one outbound for the parsed key. No
/// `dialerProxy`/upstream chaining like the server side's
/// services/vpn_checker.py uses: there is no residential-proxy hop here,
/// this device's own mobile connection is the vantage point.
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
  switch (key.protocol) {
    case 'vless':
      return {
        'protocol': 'vless',
        'settings': {
          'vnext': [
            {
              'address': key.address,
              'port': key.port,
              'users': [
                {'id': key.uuid, 'encryption': 'none', if (key.flow != null && key.flow!.isNotEmpty) 'flow': key.flow},
              ],
            },
          ],
        },
        'streamSettings': _streamSettings(key),
      };
    case 'trojan':
      return {
        'protocol': 'trojan',
        'settings': {
          'servers': [
            {'address': key.address, 'port': key.port, 'password': key.password},
          ],
        },
        'streamSettings': _streamSettings(key),
      };
    case 'vmess':
      return {
        'protocol': 'vmess',
        'settings': {
          'vnext': [
            {
              'address': key.address,
              'port': key.port,
              'users': [
                {'id': key.uuid, 'alterId': key.alterId ?? 0, 'security': 'auto'},
              ],
            },
          ],
        },
        'streamSettings': _streamSettings(key),
      };
    case 'shadowsocks':
      return {
        'protocol': 'shadowsocks',
        'settings': {
          'servers': [
            {'address': key.address, 'port': key.port, 'method': key.method, 'password': key.password},
          ],
        },
      };
    case 'hysteria2':
      // Best-effort: Xray-core added native Hysteria2 outbound support in a
      // recent release, schema below matches its documented shape at the
      // time this was written. If a future/older xray build rejects it, the
      // process exits non-zero and vpn_checker.dart reports that as a clean
      // "untestable" result rather than crashing -- see its _spawnXray().
      return {
        'protocol': 'hysteria2',
        'settings': {
          'servers': [
            {'address': key.address, 'port': key.port, 'password': key.password},
          ],
        },
        'streamSettings': {
          'security': 'tls',
          'tlsSettings': {
            'serverName': key.sni ?? key.address,
            'allowInsecure': key.allowInsecure,
          },
        },
      };
    default:
      throw ArgumentError('Unsupported protocol: ${key.protocol}');
  }
}

Map<String, dynamic> _streamSettings(ParsedVpnKey key) {
  final network = key.network ?? 'tcp';
  final settings = <String, dynamic>{'network': network, 'security': key.security ?? 'none'};

  if (key.security == 'tls') {
    settings['tlsSettings'] = {
      'serverName': key.sni ?? key.address,
      if (key.fingerprint != null) 'fingerprint': key.fingerprint,
      'allowInsecure': key.allowInsecure,
    };
  } else if (key.security == 'reality') {
    settings['realitySettings'] = {
      'serverName': key.sni ?? '',
      'fingerprint': key.fingerprint ?? 'chrome',
      'publicKey': key.publicKey ?? '',
      'shortId': key.shortId ?? '',
    };
  }

  if (network == 'ws') {
    settings['wsSettings'] = {
      'path': key.path ?? '/',
      if (key.host != null) 'headers': {'Host': key.host},
    };
  } else if (network == 'grpc') {
    settings['grpcSettings'] = {'serviceName': key.serviceName ?? ''};
  } else if (network == 'xhttp') {
    settings['xhttpSettings'] = {
      'path': key.path ?? '/',
      if (key.host != null) 'host': key.host,
    };
  }

  return settings;
}
