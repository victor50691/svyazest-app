import 'dart:convert';

/// Parses the common VLESS/VMESS/TROJAN/SHADOWSOCKS/HYSTERIA2 share-link
/// formats into a normalized shape that build_xray_config.dart turns into
/// an Xray outbound. Deliberately covers the mainstream cases (TCP/WS/gRPC,
/// TLS/REALITY/none) rather than every exotic transport combination that
/// exists in the wild -- an unrecognized combination surfaces as
/// `unsupported`, not a crash, matching this project's server-side
/// vpn_checker.py's own "reject what we can't honestly test" stance.
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
    this.network,
    this.security,
    this.sni,
    this.fingerprint,
    this.publicKey,
    this.shortId,
    this.flow,
    this.path,
    this.host,
    this.serviceName,
    this.alterId,
    this.obfs,
    this.obfsPassword,
    this.allowInsecure = false,
  });

  final String protocol; // vless | vmess | trojan | shadowsocks | hysteria2
  final String raw;
  final String? unsupportedReason;

  final String? address;
  final int? port;
  final String? uuid; // vless
  final String? password; // trojan / shadowsocks / hysteria2
  final String? method; // shadowsocks cipher
  final String? network; // tcp | ws | grpc | xhttp
  final String? security; // tls | reality | none
  final String? sni;
  final String? fingerprint;
  final String? publicKey; // reality pbk
  final String? shortId; // reality sid
  final String? flow;
  final String? path; // ws/xhttp path
  final String? host; // ws Host header
  final String? serviceName; // grpc
  final int? alterId; // vmess legacy
  final String? obfs; // hysteria2
  final String? obfsPassword;
  final bool allowInsecure;

  bool get isSupported => unsupportedReason == null && address != null && port != null;
}

ParsedVpnKey? parseVpnUri(String uri) {
  final schemeMatch = RegExp(r'^([a-zA-Z0-9+.-]+)://').firstMatch(uri);
  if (schemeMatch == null) return null;
  final scheme = schemeMatch.group(1)!.toLowerCase();

  try {
    switch (scheme) {
      case 'vless':
        return _parseVless(uri);
      case 'trojan':
        return _parseTrojan(uri);
      case 'vmess':
        return _parseVmess(uri);
      case 'ss':
      case 'shadowsocks':
        return _parseShadowsocks(uri);
      case 'hysteria2':
      case 'hy2':
        return _parseHysteria2(uri);
      default:
        return null;
    }
  } catch (e) {
    return ParsedVpnKey(protocol: scheme, raw: uri, unsupportedReason: 'parse_error: $e');
  }
}

Map<String, String> _queryParams(Uri u) => u.queryParameters;

ParsedVpnKey _parseVless(String raw) {
  final u = Uri.parse(raw);
  final q = _queryParams(u);
  return ParsedVpnKey(
    protocol: 'vless',
    raw: raw,
    uuid: u.userInfo,
    address: u.host,
    port: u.hasPort ? u.port : 443,
    network: q['type'] ?? 'tcp',
    security: q['security'] ?? 'none',
    sni: q['sni'],
    fingerprint: q['fp'],
    publicKey: q['pbk'],
    shortId: q['sid'],
    flow: q['flow'],
    path: q['path'],
    host: q['host'],
    serviceName: q['serviceName'],
    allowInsecure: q['allowInsecure'] == '1' || q['allowInsecure'] == 'true',
  );
}

ParsedVpnKey _parseTrojan(String raw) {
  final u = Uri.parse(raw);
  final q = _queryParams(u);
  return ParsedVpnKey(
    protocol: 'trojan',
    raw: raw,
    password: u.userInfo,
    address: u.host,
    port: u.hasPort ? u.port : 443,
    network: q['type'] ?? 'tcp',
    security: q['security'] ?? 'tls',
    sni: q['sni'],
    fingerprint: q['fp'],
    path: q['path'],
    host: q['host'],
    serviceName: q['serviceName'],
    allowInsecure: q['allowInsecure'] == '1' || q['allowInsecure'] == 'true',
  );
}

ParsedVpnKey _parseVmess(String raw) {
  final b64 = raw.substring('vmess://'.length);
  final jsonStr = utf8.decode(base64.decode(base64.normalize(b64)));
  final j = jsonDecode(jsonStr) as Map<String, dynamic>;
  final net = (j['net'] as String?) ?? 'tcp';
  final tls = (j['tls'] as String?) ?? '';
  return ParsedVpnKey(
    protocol: 'vmess',
    raw: raw,
    uuid: j['id'] as String?,
    address: j['add'] as String?,
    port: int.tryParse(j['port'].toString()),
    network: net,
    security: tls == 'tls' ? 'tls' : 'none',
    sni: (j['sni'] as String?) ?? (j['host'] as String?),
    path: j['path'] as String?,
    host: j['host'] as String?,
    serviceName: net == 'grpc' ? (j['path'] as String?) : null,
    alterId: int.tryParse((j['aid'] ?? '0').toString()) ?? 0,
  );
}

ParsedVpnKey _parseShadowsocks(String raw) {
  final body = raw.substring(raw.indexOf('://') + 3);
  final hashIdx = body.indexOf('#');
  final withoutTag = hashIdx >= 0 ? body.substring(0, hashIdx) : body;

  String userInfo;
  String hostPort;
  if (withoutTag.contains('@')) {
    final at = withoutTag.lastIndexOf('@');
    userInfo = withoutTag.substring(0, at);
    hostPort = withoutTag.substring(at + 1);
    if (!_looksBase64Decodable(userInfo)) {
      // some generators leave method:password in plaintext before '@'
    } else {
      try {
        userInfo = utf8.decode(base64.decode(base64.normalize(userInfo)));
      } catch (_) {
        // fall through with the raw (already plaintext) userInfo
      }
    }
  } else {
    // Legacy form: ss://base64(method:password@host:port)
    final decoded = utf8.decode(base64.decode(base64.normalize(withoutTag)));
    final at = decoded.lastIndexOf('@');
    userInfo = decoded.substring(0, at);
    hostPort = decoded.substring(at + 1);
  }

  final colon = userInfo.indexOf(':');
  final method = userInfo.substring(0, colon);
  final password = userInfo.substring(colon + 1);
  final portColon = hostPort.lastIndexOf(':');
  final host = hostPort.substring(0, portColon);
  final port = int.tryParse(hostPort.substring(portColon + 1));

  return ParsedVpnKey(
    protocol: 'shadowsocks',
    raw: raw,
    method: method,
    password: password,
    address: host,
    port: port,
    network: 'tcp',
    security: 'none',
  );
}

bool _looksBase64Decodable(String s) {
  try {
    base64.decode(base64.normalize(s));
    return true;
  } catch (_) {
    return false;
  }
}

ParsedVpnKey _parseHysteria2(String raw) {
  final u = Uri.parse(raw.replaceFirst('hy2://', 'hysteria2://'));
  final q = _queryParams(u);
  return ParsedVpnKey(
    protocol: 'hysteria2',
    raw: raw,
    password: u.userInfo,
    address: u.host,
    port: u.hasPort ? u.port : 443,
    sni: q['sni'],
    obfs: q['obfs'],
    obfsPassword: q['obfs-password'],
    allowInsecure: q['insecure'] == '1' || q['insecure'] == 'true',
  );
}
