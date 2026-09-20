import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:svyazest_app/services/build_xray_config.dart';
import 'package:svyazest_app/services/vpn_uri_parser.dart';

/// Guards the parity between this app's outbound and the one the bot builds
/// in telegram-bot/services/vpn_checker.py. Both feed the same Xray build
/// (v26.3.27 / d2758a0), so a field silently dropped here means the same key
/// gets two different verdicts depending on which vantage point ran it --
/// and a dropped field never looks like a bug, it looks like a block.
///
/// Every config below was additionally fed to the real xray binary with
/// `xray run -test` while this was written; what a unit test can still catch
/// on its own is the field going missing again.
void main() {
  const uuid = '3f8a1b2c-4d5e-6f70-8192-a3b4c5d6e7f8';

  Map<String, dynamic> outbound(String uri) {
    final parsed = parseVpnUri(uri);
    expect(parsed, isNotNull, reason: 'parser returned null for $uri');
    expect(parsed!.isSupported, isTrue, reason: parsed.unsupportedReason ?? '');
    return buildXrayConfig(parsed, 20080)['outbounds'][0] as Map<String, dynamic>;
  }

  Map<String, dynamic> stream(String uri) =>
      outbound(uri)['streamSettings'] as Map<String, dynamic>;

  group('VLESS', () {
    test('reality carries flow, pbk/sid/spx and the uTLS fingerprint', () {
      final o = outbound(
        'vless://$uuid@1.2.3.4:443?type=tcp&security=reality&pbk=PBK&sid=ab&spx=%2Fs&fp=firefox&flow=xtls-rprx-vision&sni=www.microsoft.com#RU',
      );
      final user = o['settings']['vnext'][0]['users'][0];
      expect(user['flow'], 'xtls-rprx-vision');
      final reality = o['streamSettings']['realitySettings'];
      expect(reality['publicKey'], 'PBK');
      expect(reality['shortId'], 'ab');
      expect(reality['spiderX'], '/s');
      expect(reality['fingerprint'], 'firefox');
      expect(reality['serverName'], 'www.microsoft.com');
      expect(o['streamSettings']['security'], 'reality');
    });

    test('VLESS Encryption is passed through, not overwritten with none', () {
      final o = outbound(
        'vless://$uuid@1.2.3.4:443?security=none&encryption=mlkem768x25519plus.native.0rtt.KEY',
      );
      expect(o['settings']['vnext'][0]['users'][0]['encryption'],
          'mlkem768x25519plus.native.0rtt.KEY');
    });

    test('ws keeps the percent-decoded path, Host header and alpn list', () {
      final s = stream(
        'vless://$uuid@example.com:443?type=ws&security=tls&path=%2Fws%3Fed%3D2048&host=cdn.example.com&sni=example.com&alpn=h2%2Chttp%2F1.1&fp=chrome',
      );
      expect(s['wsSettings']['path'], '/ws?ed=2048');
      expect(s['wsSettings']['headers']['Host'], 'cdn.example.com');
      expect(s['tlsSettings']['alpn'], ['h2', 'http/1.1']);
      expect(s['tlsSettings']['fingerprint'], 'chrome');
      // Removed from this core: its presence fails the whole config load.
      expect(s['tlsSettings'].containsKey('allowInsecure'), isFalse);
    });

    test('mux is honored only when the link asks for it', () {
      expect(outbound('vless://$uuid@1.2.3.4:443').containsKey('mux'), isFalse);
      final o = outbound('vless://$uuid@1.2.3.4:443?mux=1&muxConcurrency=4');
      expect(o['mux'], {'enabled': true, 'concurrency': 4});
    });

    test('grpc multiMode and authority', () {
      final s = stream(
        'vless://$uuid@example.com:443?type=grpc&security=tls&serviceName=svc&mode=multi&authority=a.example.com&sni=example.com',
      );
      expect(s['grpcSettings'], {
        'serviceName': 'svc',
        'multiMode': true,
        'authority': 'a.example.com',
      });
    });

    test('xhttp extra survives and its float integers are normalized', () {
      const extra = '{"xmux":{"cMaxLifetimeMs":0.0,"maxConcurrency":"16-32"}}';
      final s = stream(
        'vless://$uuid@example.com:443?type=xhttp&security=tls&path=%2Fx&host=example.com&mode=packet-up&sni=example.com&extra=${Uri.encodeComponent(extra)}',
      );
      // 0.0 would make the core reject the entire config ("Invalid integer range").
      expect(jsonEncode(s['xhttpSettings']['extra']),
          '{"xmux":{"cMaxLifetimeMs":0,"maxConcurrency":"16-32"}}');
      expect(s['xhttpSettings']['mode'], 'packet-up');
    });

    test('tcp with http header obfuscation builds the request block', () {
      final s = stream(
        'vless://$uuid@1.2.3.4:80?type=tcp&headerType=http&path=%2Fdl&host=a.example.com%2Cb.example.com',
      );
      expect(s['tcpSettings']['header']['type'], 'http');
      expect(s['tcpSettings']['header']['request']['path'], ['/dl']);
      expect(s['tcpSettings']['header']['request']['headers']['Host'],
          ['a.example.com', 'b.example.com']);
    });
  });

  group('VMess', () {
    String link(Map<String, dynamic> j) => 'vmess://${base64.encode(utf8.encode(jsonEncode(j)))}';

    test('base64 blob maps onto the same fields as a query-string link', () {
      final o = outbound(link({
        'v': '2', 'ps': 'n', 'add': 'example.com', 'port': '443', 'id': uuid,
        'aid': '0', 'scy': 'zero', 'net': 'ws', 'host': 'example.com',
        'path': '/vm', 'tls': 'tls', 'sni': 'example.com', 'fp': 'chrome',
      }));
      final user = o['settings']['vnext'][0]['users'][0];
      expect(user['security'], 'zero');
      expect(user['alterId'], 0);
      expect(o['streamSettings']['wsSettings']['path'], '/vm');
      expect(o['streamSettings']['tlsSettings']['fingerprint'], 'chrome');
    });

    test('tls:"none" means plaintext, not TLS', () {
      final s = stream(link({
        'add': '1.2.3.4', 'port': 8080, 'id': uuid, 'aid': 0, 'net': 'tcp', 'tls': 'none',
      }));
      expect(s.containsKey('tlsSettings'), isFalse);
    });
  });

  group('Trojan', () {
    test('defaults to TLS when the link says nothing', () {
      expect(stream('trojan://pw@example.com:443')['security'], 'tls');
    });

    test('security=reality gets REALITY, not a plain-TLS handshake', () {
      final s = stream('trojan://pw@1.2.3.4:443?security=reality&pbk=PBK&sid=ef&sni=www.apple.com');
      expect(s['security'], 'reality');
      expect(s['realitySettings']['publicKey'], 'PBK');
    });

    test('security=none stays plaintext', () {
      expect(stream('trojan://pw@1.2.3.4:443?security=none')['security'], 'none');
    });

    test('percent-encoded password is decoded', () {
      final o = outbound('trojan://pass%40word@example.com:443');
      expect(o['settings']['servers'][0]['password'], 'pass@word');
    });
  });

  group('Shadowsocks', () {
    test('SIP002 with url-safe base64 userinfo', () {
      final ui = base64Url.encode(utf8.encode('aes-256-gcm:secret')).replaceAll('=', '');
      final o = outbound('ss://$ui@1.2.3.4:8388#tag');
      expect(o['settings']['servers'][0]['method'], 'aes-256-gcm');
      expect(o['settings']['servers'][0]['password'], 'secret');
    });

    test('plaintext userinfo form', () {
      final o = outbound('ss://chacha20-ietf-poly1305:secret@1.2.3.4:8388');
      expect(o['settings']['servers'][0]['method'], 'chacha20-ietf-poly1305');
    });

    test('legacy fully-encoded form', () {
      final blob = base64.encode(utf8.encode('aes-128-gcm:pw@1.2.3.4:8388'));
      final o = outbound('ss://$blob');
      expect(o['settings']['servers'][0]['address'], '1.2.3.4');
      expect(o['settings']['servers'][0]['port'], 8388);
    });

    test('ss over ws+tls honors the transport params', () {
      final ui = base64Url.encode(utf8.encode('aes-256-gcm:secret')).replaceAll('=', '');
      final s = stream('ss://$ui@example.com:443?type=ws&security=tls&path=%2Fss&host=example.com');
      expect(s['network'], 'ws');
      expect(s['tlsSettings']['serverName'], 'example.com');
    });
  });

  group('Hysteria2', () {
    test('uses the core protocol name and shape, not the scheme name', () {
      final o = outbound('hy2://auth%40pass@1.2.3.4:443?sni=example.com#hy');
      // "hysteria2" is not a protocol the core knows, and a servers[] array
      // in settings panics its config builder.
      expect(o['protocol'], 'hysteria');
      expect(o['settings'], {'address': '1.2.3.4', 'port': 443, 'version': 2});
      final s = o['streamSettings'];
      expect(s['network'], 'hysteria');
      expect(s['hysteriaSettings'], {'auth': 'auth@pass', 'version': 2});
      // Hysteria2 rides HTTP/3: the server forces this ALPN itself.
      expect(s['tlsSettings']['alpn'], ['h3']);
      expect(s['tlsSettings']['serverName'], 'example.com');
    });

    test('a "user:pass" auth string is kept whole', () {
      final o = outbound('hysteria2://user:pass@1.2.3.4:8443?sni=example.com');
      expect(o['streamSettings']['hysteriaSettings']['auth'], 'user:pass');
    });
  });

  group('honest refusals', () {
    void refused(String uri, String code) {
      final parsed = parseVpnUri(uri);
      expect(parsed?.isSupported ?? false, isFalse, reason: 'should not be dialed: $uri');
      expect(parsed!.unsupportedReason, startsWith(code));
    }

    test('transports this core removed', () {
      refused('vless://$uuid@example.com:443?type=http&security=tls', 'transport_removed_from_core');
      refused('vless://$uuid@example.com:443?type=quic&security=tls', 'transport_removed_from_core');
    });

    test('mKCP obfuscation the core no longer speaks', () {
      refused('vless://$uuid@1.2.3.4:2096?type=kcp&seed=s', 'mkcp_header_seed_removed_from_core');
      refused('vless://$uuid@1.2.3.4:2096?type=kcp&headerType=srtp',
          'mkcp_header_seed_removed_from_core');
    });

    test('plain mKCP is still dialed', () {
      expect(stream('vless://$uuid@1.2.3.4:2096?type=kcp')['network'], 'mkcp');
    });

    test('schemes the core cannot dial at all', () {
      refused('tuic://$uuid:pw@example.com:443', 'scheme_unsupported_by_core');
      refused('wireguard://key@1.2.3.4:51820', 'scheme_unsupported_by_core');
      refused('hysteria://pw@1.2.3.4:443', 'scheme_unsupported_by_core');
    });

    test('garbage is a null parse, not a crash', () {
      expect(parseVpnUri('not a link'), isNull);
      expect(parseVpnUri('vmess://!!!not-base64!!!')!.unsupportedReason, startsWith('parse_error'));
    });
  });
}
