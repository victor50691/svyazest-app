import 'dart:async';
import 'dart:io';

/// Plain IP/domain reachability check straight from this device's active
/// network (whatever ConnectivityManager currently routes through, expected
/// to be mobile data -- the executor loop already gates on that before
/// calling this). No proxy chain, no xray -- this is the whole point of
/// device-mode checks: the phone's own mobile connection *is* the vantage
/// point, so a raw socket from Dart is a completely honest measurement.
class IpCheckResult {
  IpCheckResult({
    required this.accessible,
    this.resolvedIp,
    this.tcpLatencyMs,
    this.port80Open,
    this.port443Open,
    this.tlsOk,
    this.error,
    this.httpCode,
    this.bytes,
    this.finalHost,
    this.finalScheme,
    this.blockReason,
    this.timeMs,
    this.payloadOk,
    this.payloadBytes,
    this.payloadUrl,
    this.payloadFrom,
    this.stallBytes,
    this.antiBot,
    this.redirects,
    this.redirectLimit,
    this.ipFamily,
  });

  final bool accessible;
  final String? resolvedIp;
  final int? tcpLatencyMs;
  final bool? port80Open;
  final bool? port443Open;
  final bool? tlsOk;
  final String? error;
  /// Domain checks: what the real HTTPS request saw.
  final int? httpCode;
  final int? bytes;
  final String? finalHost;
  final String? finalScheme;
  /// dns_poisoned | tls | redirect | timeout | connect -- why it is blocked.
  final String? blockReason;
  final int? timeMs;
  /// Volume test (the ТСПУ ~16 KB cutoff): did a response comfortably
  /// past the cutoff arrive IN FULL from this site? null = could not be
  /// tested (nothing big enough on the site).
  final bool? payloadOk;
  final int? payloadBytes;
  final String? payloadUrl;
  final String? payloadFrom; // page | asset
  final int? stallBytes; // bytes received before the flow was cut
  /// Captcha / DDoS protection that answered instead of the page
  /// (ddos-guard, cloudflare, qrator, stormwall, variti, servicepipe,
  /// yandex-captcha, captcha). The site was reached; null = none seen.
  final String? antiBot;
  final int? redirects;
  /// The site kept redirecting past [_maxRedirects] hops.
  final bool? redirectLimit;
  /// 4 / 6 = every connection of the check went over that IP family only
  /// (job field ip_family, 1.17.2); null = whatever the system picked, as
  /// before.
  final int? ipFamily;

  Map<String, dynamic> toJson() => {
        'accessible': accessible,
        'resolved_ip': resolvedIp,
        'tcp_latency_ms': tcpLatencyMs,
        'port_80_open': port80Open,
        'port_443_open': port443Open,
        'tls_ok': tlsOk,
        'error': error,
        'http_code': httpCode,
        'bytes': bytes,
        'final_host': finalHost,
        'final_scheme': finalScheme,
        'block_reason': blockReason,
        'time_ms': timeMs,
        'payload_ok': payloadOk,
        'payload_bytes': payloadBytes,
        'payload_url': payloadUrl,
        'payload_from': payloadFrom,
        'stall_bytes': stallBytes,
        'anti_bot': antiBot,
        'redirects': redirects,
        'redirect_limit': redirectLimit,
        'ip_family': ipFamily,
      };
}

/// [ipFamily] (job field ip_family, 1.17.2): 6 = resolve the domain's AAAA
/// over the mobile network and connect only over IPv6, 4 = only over IPv4,
/// null = as before (A records for the verdict, the connection itself goes
/// wherever the system sends it). A literal address is its own family.
Future<IpCheckResult> checkIpOrDomain(String target, {int? ipFamily}) async {
  final literal = _asLiteralIp(target);
  if (literal == null) return _checkDomain(target, ipFamily: ipFamily == 6 || ipFamily == 4 ? ipFamily : null);
  return _checkIp(literal);
}

/// The address family a job asks for, as dart:io's type.
InternetAddressType _familyType(int family) =>
    family == 6 ? InternetAddressType.IPv6 : InternetAddressType.IPv4;

/// Addresses of [host] in [family] only, from the network the process is
/// bound to (the cellular one). NAT64 answers (64:ff9b::/96, synthesized by
/// a DNS64 resolver for an IPv4-only site) are not the site's own IPv6 and
/// do not count.
Future<List<InternetAddress>> _lookupFamily(String host, int family) async {
  final literal = InternetAddress.tryParse(host);
  if (literal != null) return literal.type == _familyType(family) ? [literal] : const [];
  final all = await InternetAddress.lookup(host, type: _familyType(family)).timeout(const Duration(seconds: 8));
  return all.where((a) => a.type == _familyType(family) && !(family == 6 && _isNat64(a))).toList();
}

bool _isNat64(InternetAddress a) {
  final b = a.rawAddress;
  return b.length == 16 && b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b &&
      b.sublist(4, 12).every((x) => x == 0);
}

/// Every connection of [client] goes to an address of [family] only --
/// HttpClient itself would take whatever getaddrinfo puts first (IPv6 on a
/// dual-stack operator), which is not what an «IPv4» or «IPv6» check means.
/// https is wrapped in TLS here with the URL's host as SNI and for the
/// certificate check (the factory has to hand back a ready SecureSocket).
void _pinFamily(HttpClient client, int? family) {
  if (family == null) return;
  client.connectionFactory = (Uri uri, String? proxyHost, int? proxyPort) async {
    final addrs = await _lookupFamily(uri.host, family);
    if (addrs.isEmpty) {
      throw SocketException('no IPv$family address for the host', address: null);
    }
    final port = uri.hasPort ? uri.port : (uri.scheme == 'https' ? 443 : 80);
    final task = await Socket.startConnect(addrs.first, port);
    if (uri.scheme != 'https') return task;
    final secured = task.socket.then((s) => SecureSocket.secure(s, host: uri.host));
    return ConnectionTask.fromSocket(secured, task.cancel);
  };
}

// Same numbers as the server-side «Зонд» (services/probe.py): the cutoff
// observed live is ~16 KB, so evidence must be 3x that and arrive whole; a
// transfer that dies under STALL_MAX is the cutoff, above it just a bad line.
const int _minPayloadBytes = 49152;
const int _maxPayloadBytes = 800000;
const int _stallMaxBytes = 40000;
const int _maxAssetCandidates = 6;
// Russian sites chain a lot of hops (www, /ru/, a cookie gate, a region
// picker); a browser follows 20, HttpClient's default is 5.
const int _maxRedirects = 10;

/// GET [start], following redirects by hand and carrying cookies between
/// hops the way a browser does. HttpClient's own follower drops Set-Cookie,
/// so an anti-bot gate that sets a cookie and redirects back to the same
/// URL looks like a loop to it (RedirectException) -- a reachable site
/// recorded as a block. The last response is returned unread.
Future<({HttpClientResponse resp, List<Uri> locations, bool limitHit})> _getFollowing(HttpClient client, Uri start) async {
  final jar = <String, Cookie>{};
  final locations = <Uri>[];
  var at = start;
  for (var hop = 0;; hop++) {
    final req = await client.getUrl(at).timeout(const Duration(seconds: 10));
    req.followRedirects = false;
    final host = at.host.toLowerCase();
    req.cookies.addAll(jar.values.where((c) => _cookieFor(c, host)));
    final resp = await req.close().timeout(const Duration(seconds: 15));
    List<Cookie> set = const [];
    try {
      set = resp.cookies;
    } catch (_) {} // a malformed Set-Cookie is the site's problem, not a block
    for (final c in set) {
      c.domain = (c.domain ?? host).toLowerCase().replaceFirst(RegExp(r'^\.'), '');
      jar['${c.domain}|${c.name}'] = c;
    }
    final loc = resp.headers.value(HttpHeaders.locationHeader);
    Uri? next;
    if (resp.isRedirect && loc != null) {
      try {
        next = at.resolveUri(Uri.parse(loc.trim()));
      } catch (_) {}
    }
    if (next == null || (next.scheme != 'https' && next.scheme != 'http')) {
      return (resp: resp, locations: locations, limitHit: false);
    }
    if (hop >= _maxRedirects) return (resp: resp, locations: locations, limitHit: true);
    await resp.drain<void>().timeout(const Duration(seconds: 10));
    locations.add(Uri.parse(loc!.trim()));
    at = next;
  }
}

bool _cookieFor(Cookie c, String host) {
  final d = c.domain;
  return d == null || host == d || host.endsWith('.$d');
}

/// Which protection answered instead of the site, if any. Only pages that
/// look like a stop -- an error status, a captcha URL or Cloudflare's own
/// challenge header -- count: a normal page of a protected site still
/// carries the vendor's server header and cookies.
String? _detectAntiBot(int? code, HttpHeaders headers, Uri finalUri, List<List<int>> body) {
  final server = (headers.value('server') ?? '').toLowerCase();
  final setCookie = (headers['set-cookie'] ?? const <String>[]).join(';').toLowerCase();
  final path = finalUri.path.toLowerCase();
  final cfChallenge = (headers.value('cf-mitigated') ?? '').toLowerCase() == 'challenge';
  final captchaUrl = path.contains('captcha');
  if (!cfChallenge && !captchaUrl && (code == null || code < 400)) return null;
  final head = <int>[];
  for (final ch in body) {
    head.addAll(ch.length > 65536 - head.length ? ch.sublist(0, 65536 - head.length) : ch);
    if (head.length >= 65536) break;
  }
  final text = String.fromCharCodes(head).toLowerCase();
  bool any(List<String> xs) => xs.any((x) => server.contains(x) || setCookie.contains(x) || text.contains(x));
  if (cfChallenge || (server.contains('cloudflare') && any(['challenge-platform', 'cf-chl', 'just a moment']))) return 'cloudflare';
  if (any(['ddos-guard', '__ddg'])) return 'ddos-guard';
  if (any(['qrator'])) return 'qrator';
  if (any(['stormwall'])) return 'stormwall';
  if (any(['variti'])) return 'variti';
  if (any(['servicepipe'])) return 'servicepipe';
  if (path.contains('showcaptcha') || any(['smartcaptcha', 'captcha.yandex'])) return 'yandex-captcha';
  if (captchaUrl || text.contains('captcha')) return 'captcha';
  return null;
}

final RegExp _assetRe = RegExp(r"(?:src|href)\s*=\s*[\x22\x27]([^\x22\x27#?\s]+\.(?:css|js|png|jpe?g|webp|gif|svg|woff2?|ttf|mp4|json)(?:\?[^\x22\x27\s]*)?)[\x22\x27]", caseSensitive: false);

/// Same-site asset URLs from an HTML body, in document order, deduplicated.
/// Same-site = the checked domain or, after a redirect, the site the page
/// really lives on (twitter.com -> x.com); never a third-party CDN, whose
/// flows say nothing about the checked site.
List<Uri> _assetCandidates(String domain, Uri page, String body) {
  final out = <Uri>[];
  final seen = <String>{};
  for (final m in _assetRe.allMatches(body)) {
    final raw = m.group(1)!;
    Uri? u;
    try {
      u = page.resolve(raw);
    } catch (_) {
      continue;
    }
    if (u.scheme != 'https' && u.scheme != 'http') continue;
    if (_registrable(u.host) != _registrable(domain) && _registrable(u.host) != _registrable(page.host)) continue;
    if (!seen.add(u.toString())) continue;
    out.add(u);
    if (out.length >= _maxAssetCandidates) break;
  }
  return out;
}

/// Fetch one URL fully (capped) and say how it ended. "stalled" = the
/// connection died after some bytes but before STALL_MAX -- the cutoff
/// signature.
Future<({int bytes, bool complete, bool stalled, int? code})> _pull(HttpClient client, Uri uri) async {
  int bytes = 0;
  int? code;
  try {
    final req = await client.getUrl(uri).timeout(const Duration(seconds: 10));
    req.followRedirects = true;
    req.maxRedirects = 5;
    final resp = await req.close().timeout(const Duration(seconds: 15));
    code = resp.statusCode;
    await for (final chunk in resp.timeout(const Duration(seconds: 20))) {
      bytes += chunk.length;
      if (bytes > _maxPayloadBytes) break;
    }
    return (bytes: bytes, complete: true, stalled: false, code: code);
  } catch (_) {
    return (bytes: bytes, complete: false, stalled: bytes > 0 && bytes < _stallMaxBytes, code: code);
  }
}

/// Walks a redirect chain that started at https://[domain]/ hop by hop.
/// A Location that came in a reply over verified TLS was sent by the site
/// itself; one that came over plain http could have been injected by the
/// operator, so a hop to another domain from an http page is untrusted.
({Uri finalUri, bool untrustedHop}) walkRedirects(String domain, List<Uri> locations) {
  var at = Uri.https(domain, '/');
  var untrusted = false;
  for (final loc in locations) {
    final to = at.resolveUri(loc); // may be relative ("/home")
    if (at.scheme != 'https' && _registrable(to.host) != _registrable(domain)) untrusted = true;
    at = to;
  }
  return (finalUri: at, untrustedHop: untrusted);
}

bool _isBogusIp(String ip) {
  final p = ip.split('.').map(int.tryParse).toList();
  if (p.length != 4 || p.any((x) => x == null)) return true;
  final a = p[0]!, b = p[1]!;
  return a == 0 || a == 127 || a == 10 || (a == 172 && b >= 16 && b <= 31) || (a == 192 && b == 168) ||
      (a == 100 && b >= 64 && b <= 127) || (a == 169 && b == 254) || a >= 224;
}

String _registrable(String host) {
  final parts = host.toLowerCase().split('.');
  return parts.length <= 2 ? host.toLowerCase() : parts.sublist(parts.length - 2).join('.');
}

/// A domain is "accessible" only if a real HTTPS request to it succeeds the
/// way a browser would: honest DNS answer, valid certificate for that name
/// (SNI), a response with content, and no redirect off to some other site.
/// That is what a DNS spoof, a TLS reset or an ISP block page all fail.
Future<IpCheckResult> _checkDomain(String domain, {int? ipFamily}) async {
  String? resolvedIp;
  if (ipFamily == 6) {
    // «Через IPv6»: the domain's own AAAA from the mobile network's DNS. None
    // there (or only NAT64 ones) = nothing to reach over IPv6 -- told apart
    // from a DNS failure, it is not a block.
    try {
      final v6 = await _lookupFamily(domain, 6);
      if (v6.isNotEmpty) resolvedIp = v6.first.address;
    } catch (_) {}
    if (resolvedIp == null) {
      return IpCheckResult(accessible: false, error: 'no AAAA', blockReason: 'no_aaaa', ipFamily: 6);
    }
    if (_isBogusIp6(InternetAddress(resolvedIp))) {
      return IpCheckResult(accessible: false, resolvedIp: resolvedIp, error: 'dns poisoned', blockReason: 'dns_poisoned', ipFamily: 6);
    }
  } else {
    try {
      final addresses = await InternetAddress.lookup(domain, type: InternetAddressType.IPv4).timeout(const Duration(seconds: 8));
      if (addresses.isNotEmpty) resolvedIp = addresses.first.address;
    } catch (_) {}
    if (resolvedIp == null) {
      return IpCheckResult(accessible: false, error: 'dns failed', blockReason: 'dns', ipFamily: ipFamily);
    }
    if (_isBogusIp(resolvedIp)) {
      return IpCheckResult(accessible: false, resolvedIp: resolvedIp, error: 'dns poisoned', blockReason: 'dns_poisoned', ipFamily: ipFamily);
    }
  }

  final client = HttpClient();
  _pinFamily(client, ipFamily);
  client.connectionTimeout = const Duration(seconds: 8);
  client.userAgent = 'Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36';
  final sw = Stopwatch()..start();
  int bytes = 0;
  int? code;
  String? finalHost;
  Uri finalUri = Uri.https(domain, '/');
  String? blockReason;
  String? error;
  final bodyChunks = <List<int>>[];
  int bodyKept = 0;
  bool pageComplete = false;
  String? antiBot;
  bool redirectLimit = false;
  int redirects = 0;
  try {
    final got = await _getFollowing(client, Uri.https(domain, '/'));
    final resp = got.resp;
    code = resp.statusCode;
    redirectLimit = got.limitHit;
    redirects = got.locations.length;
    final chain = walkRedirects(domain, got.locations);
    finalUri = chain.finalUri;
    final untrustedHop = chain.untrustedHop;
    finalHost = finalUri.host.isEmpty ? domain : finalUri.host;
    try {
      await for (final chunk in resp.timeout(const Duration(seconds: 15))) {
        bytes += chunk.length;
        if (bodyKept < 512 * 1024) {
          bodyChunks.add(chunk);
          bodyKept += chunk.length;
        }
        if (bytes > _maxPayloadBytes) break;
      }
      pageComplete = true;
    } catch (_) {
      // Body cut mid-way: the page opened, then the flow was killed.
      if (bytes > 0 && bytes < _stallMaxBytes) blockReason = 'volume';
    }
    // A redirect to another domain (twitter.com -> x.com) over the site's
    // own verified TLS is the site's doing, not a block. Blocked: any
    // untrusted hop (above), or ending on another domain over plain http.
    if (blockReason == null &&
        (untrustedHop || (finalUri.scheme != 'https' && _registrable(finalHost) != _registrable(domain)))) {
      blockReason = 'redirect';
    }
    // A captcha / DDoS-protection page arrived over the site's own verified
    // TLS: the network let us through to the site, its protection stopped
    // a robot. Reachable -- whatever the status code or page size.
    if (blockReason == null || blockReason == 'volume') {
      antiBot = _detectAntiBot(code, resp.headers, finalUri, bodyChunks);
      if (antiBot != null && pageComplete) blockReason = null;
    }
  } on HandshakeException catch (e) {
    error = e.toString();
    blockReason = 'tls';
  } on TlsException catch (e) {
    error = e.toString();
    blockReason = 'tls';
  } on TimeoutException {
    error = 'timeout';
    blockReason = 'timeout';
  } on SocketException catch (e) {
    error = e.message;
    blockReason = e.osError?.message.toLowerCase().contains('reset') == true ? 'tls' : 'connect';
  } catch (e) {
    error = e.toString();
    blockReason = 'connect';
  } finally {
    sw.stop();
    client.close(force: true);
  }
  bool ok = blockReason == null && code != null &&
      (antiBot != null || (code < 500 && (bytes > 0 || redirectLimit)));
  final pageMs = sw.elapsedMilliseconds;

  // Volume test. The page itself is evidence when it is big enough and came
  // whole; otherwise hunt for one same-site asset past the threshold. A
  // flow that dies early on any of them is the cutoff -> blocked.
  bool? payloadOk;
  int? payloadBytes;
  String? payloadUrl;
  String? payloadFrom;
  int? stallBytes;
  // A protection page or an endless redirect has no page of its own to
  // measure; the volume verdict stays «not tested».
  if (ok && antiBot == null && !redirectLimit) {
    if (pageComplete && bytes >= _minPayloadBytes) {
      payloadOk = true;
      payloadBytes = bytes;
      payloadUrl = finalUri.toString();
      payloadFrom = 'page';
    } else {
      final body = String.fromCharCodes(bodyChunks.expand((c) => c));
      final vc = HttpClient();
      vc.connectionTimeout = const Duration(seconds: 8);
      vc.userAgent = client.userAgent;
      _pinFamily(vc, ipFamily);
      try {
        for (final u in _assetCandidates(domain, finalUri, body)) {
          final r = await _pull(vc, u);
          if (r.stalled) {
            payloadOk = false;
            stallBytes = r.bytes;
            payloadUrl = u.toString();
            payloadFrom = 'asset';
            blockReason = 'volume';
            ok = false;
            error = 'flow cut after ${r.bytes} bytes';
            break;
          }
          if (r.complete && r.code != null && r.code! < 300 && r.bytes >= _minPayloadBytes) {
            payloadOk = true;
            payloadBytes = r.bytes;
            payloadUrl = u.toString();
            payloadFrom = 'asset';
            break;
          }
        }
      } finally {
        vc.close(force: true);
      }
    }
  } else if (blockReason == 'volume') {
    payloadOk = false;
    stallBytes = bytes;
    payloadUrl = finalUri.toString();
    payloadFrom = 'page';
  }

  return IpCheckResult(
    accessible: ok,
    resolvedIp: resolvedIp,
    tcpLatencyMs: ok ? pageMs : null,
    port443Open: code != null,
    tlsOk: code != null,
    httpCode: code,
    bytes: bytes,
    finalHost: finalHost,
    finalScheme: finalUri.scheme,
    blockReason: ok ? null : (blockReason ?? 'http'),
    timeMs: pageMs,
    error: ok ? null : (error ?? 'HTTP $code'),
    payloadOk: payloadOk,
    payloadBytes: payloadBytes,
    payloadUrl: payloadUrl,
    payloadFrom: payloadFrom,
    stallBytes: stallBytes,
    antiBot: antiBot,
    redirects: redirects,
    redirectLimit: redirectLimit,
    ipFamily: ipFamily,
  );
}

/// A literal address, IPv4 or (1.17.2) IPv6: TCP 443 / TLS / TCP 80 straight
/// to it -- an IPv6 one necessarily over the mobile network's IPv6.
Future<IpCheckResult> _checkIp(InternetAddress target) async {
  final resolvedIp = target.address;
  final host = target;
  final sw = Stopwatch()..start();
  bool port443 = false;
  bool port80 = false;
  bool tlsOk = false;
  String? error;

  try {
    final socket = await Socket.connect(host, 443, timeout: const Duration(seconds: 8));
    port443 = true;
    await socket.close();
  } catch (e) {
    error = e.toString();
  }
  final latency = sw.elapsedMilliseconds;

  try {
    final secure = await SecureSocket.connect(
      host,
      443,
      timeout: const Duration(seconds: 8),
      onBadCertificate: (_) => true, // reachability probe, not a cert-trust decision
    );
    tlsOk = true;
    await secure.close();
  } catch (_) {
    tlsOk = false;
  }

  try {
    final socket = await Socket.connect(host, 80, timeout: const Duration(seconds: 6));
    port80 = true;
    await socket.close();
  } catch (_) {
    port80 = false;
  }

  final accessible = port443 || port80;
  return IpCheckResult(
    accessible: accessible,
    resolvedIp: resolvedIp,
    tcpLatencyMs: accessible ? latency : null,
    port80Open: port80,
    port443Open: port443,
    tlsOk: tlsOk,
    error: accessible ? null : error,
    ipFamily: target.type == InternetAddressType.IPv6 ? 6 : 4,
  );
}

/// A dotted IPv4 (as before) or an IPv6 literal, bare or in brackets.
InternetAddress? _asLiteralIp(String s) {
  final v4 = RegExp(r'^(\d{1,3}\.){3}\d{1,3}$');
  if (v4.hasMatch(s)) return InternetAddress.tryParse(s);
  final bare = s.startsWith('[') && s.endsWith(']') ? s.substring(1, s.length - 1) : s;
  if (!bare.contains(':')) return null;
  final a = InternetAddress.tryParse(bare);
  return a != null && a.type == InternetAddressType.IPv6 ? a : null;
}

/// An AAAA that cannot be the site: unspecified, loopback, link-local,
/// unique-local, multicast, IPv4-mapped -- what a DNS spoof would hand out.
bool _isBogusIp6(InternetAddress a) {
  final b = a.rawAddress;
  if (b.length != 16) return true;
  if (b.every((x) => x == 0)) return true;
  if (a.isLoopback || a.isLinkLocal || a.isMulticast) return true;
  if ((b[0] & 0xfe) == 0xfc) return true; // fc00::/7
  if (b.sublist(0, 10).every((x) => x == 0) && b[10] == 0xff && b[11] == 0xff) return true; // ::ffff:a.b.c.d
  return false;
}
