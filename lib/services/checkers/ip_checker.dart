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
      };
}

Future<IpCheckResult> checkIpOrDomain(String target) async {
  if (_asLiteralIp(target) == null) return _checkDomain(target);
  return _checkIp(target);
}

// Same numbers as the server-side «Зонд» (services/probe.py): the cutoff
// observed live is ~16 KB, so evidence must be 3x that and arrive whole; a
// transfer that dies under STALL_MAX is the cutoff, above it just a bad line.
const int _minPayloadBytes = 49152;
const int _maxPayloadBytes = 800000;
const int _stallMaxBytes = 40000;
const int _maxAssetCandidates = 6;

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
Future<IpCheckResult> _checkDomain(String domain) async {
  String? resolvedIp;
  try {
    final addresses = await InternetAddress.lookup(domain, type: InternetAddressType.IPv4).timeout(const Duration(seconds: 8));
    if (addresses.isNotEmpty) resolvedIp = addresses.first.address;
  } catch (_) {}
  if (resolvedIp == null) {
    return IpCheckResult(accessible: false, error: 'dns failed', blockReason: 'dns');
  }
  if (_isBogusIp(resolvedIp)) {
    return IpCheckResult(accessible: false, resolvedIp: resolvedIp, error: 'dns poisoned', blockReason: 'dns_poisoned');
  }

  final client = HttpClient();
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
  try {
    final req = await client.getUrl(Uri.https(domain, '/')).timeout(const Duration(seconds: 10));
    req.followRedirects = true;
    req.maxRedirects = 5;
    final resp = await req.close().timeout(const Duration(seconds: 15));
    code = resp.statusCode;
    final chain = walkRedirects(domain, resp.redirects.map((r) => r.location).toList());
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
  bool ok = blockReason == null && code != null && code < 500 && bytes > 0;
  final pageMs = sw.elapsedMilliseconds;

  // Volume test. The page itself is evidence when it is big enough and came
  // whole; otherwise hunt for one same-site asset past the threshold. A
  // flow that dies early on any of them is the cutoff -> blocked.
  bool? payloadOk;
  int? payloadBytes;
  String? payloadUrl;
  String? payloadFrom;
  int? stallBytes;
  if (ok) {
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
  );
}

Future<IpCheckResult> _checkIp(String target) async {
  String? resolvedIp;
  try {
    final addresses = await InternetAddress.lookup(target, type: InternetAddressType.IPv4).timeout(const Duration(seconds: 8));
    if (addresses.isNotEmpty) resolvedIp = addresses.first.address;
  } catch (_) {
    // target may already be a literal IP, or DNS itself may be blocked --
    // both are meaningful outcomes, not fatal to the rest of the check.
    resolvedIp ??= _asLiteralIp(target);
  }

  final host = resolvedIp ?? target;
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
  );
}

String? _asLiteralIp(String s) {
  final v4 = RegExp(r'^(\d{1,3}\.){3}\d{1,3}$');
  return v4.hasMatch(s) ? s : null;
}
