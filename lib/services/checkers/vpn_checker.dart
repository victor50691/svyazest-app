import 'dart:convert';
import 'dart:io';
import 'dart:math';

import '../build_xray_config.dart';
import '../native_bridge.dart';
import '../vpn_uri_parser.dart';

class VpnCheckResult {
  VpnCheckResult({
    required this.connected,
    this.latencyMs,
    this.error,
    this.untestable = false,
    this.untestableReason,
    this.socketsBound = 0,
    this.socketsUnbound = 0,
    this.sites = const [],
    this.download,
    this.bindError,
  });

  final bool connected;
  final int? latencyMs;
  final String? error;
  final bool untestable;
  final String? untestableReason;
  /// Xray sockets pinned to the cellular network vs. not during this check.
  final int socketsBound;
  final int socketsUnbound;
  /// Per-site results through the tunnel and the fixed-file download --
  /// a key can complete its handshake and still be dead after ~16-20 KB.
  final List<Map<String, dynamic>> sites;
  final Map<String, dynamic>? download;
  final String? bindError;

  /// Handshake + at least one site + the whole file: what "the key works"
  /// actually means to a person.
  bool get works => connected && (sites.isEmpty || sites.any((s) => s['ok'] == true)) && (download == null || download!['ok'] == true);

  Map<String, dynamic> toJson() => {
        'connected': connected,
        'latency_ms': latencyMs,
        'error': error,
        'untestable': untestable,
        'untestable_reason': untestableReason,
        'sockets_bound': socketsBound,
        'sockets_unbound': socketsUnbound,
        'works': works,
        'sites': sites,
        'download': ?download,
        'bind_error': ?bindError,
      };
}

/// Same four-way outcome shape (connected / error / proxy_dead-analog /
/// untestable) the server side's services/vpn_checker.py returns, so admin
/// tooling and the requesting user's report read the same regardless of
/// which vantage point produced the result.
Future<VpnCheckResult> checkVpnKey(String uri, {List<String> probeSites = const [], String? downloadUrl}) async {
  final parsed = parseVpnUri(uri);
  if (parsed == null) {
    return VpnCheckResult(connected: false, untestable: true, untestableReason: 'unrecognized_scheme');
  }
  if (parsed.unsupportedReason != null || !parsed.isSupported) {
    return VpnCheckResult(
      connected: false,
      untestable: true,
      untestableReason: parsed.unsupportedReason ?? 'missing_fields',
    );
  }

  final localPort = 20000 + Random().nextInt(9000);
  final config = buildXrayConfig(parsed, localPort);

  // Xray-core runs inside this process (xraylib.aar) and every socket it
  // opens is bound to the cellular network by NativePlugin.kt, so the
  // result reflects the phone's mobile connection even with a third-party
  // VPN switched on. The loopback hop to the local HTTP inbound is
  // unaffected by network binding.
  VpnCheckResult outcome;
  try {
    final startError = await NativeBridge.xrayStart(jsonEncode(config));
    if (startError != null) {
      outcome = VpnCheckResult(connected: false, error: 'xray: $startError');
    } else if (!await _waitForPort(localPort)) {
      outcome = VpnCheckResult(connected: false, error: 'local proxy did not come up in time');
    } else {
      final sw = Stopwatch()..start();
      final probe = await _probeThroughProxy(localPort);
      sw.stop();
      if (!probe) {
        outcome = VpnCheckResult(connected: false, error: 'target unreachable through this key');
      } else {
        // Handshake works -- now the part that matters: real pages and a
        // sustained transfer. Sequential, one site at a time, so a stalled
        // tunnel shows up as a clean per-site failure rather than noise.
        final sites = <Map<String, dynamic>>[];
        for (final host in probeSites) {
          sites.add(await _fetchThroughProxy(localPort, Uri.parse('https://$host/'), host: host));
        }
        Map<String, dynamic>? download;
        if (downloadUrl != null && downloadUrl.isNotEmpty) {
          download = await _downloadThroughProxy(localPort, Uri.parse(downloadUrl));
        }
        outcome = VpnCheckResult(connected: true, latencyMs: sw.elapsedMilliseconds, sites: sites, download: download);
      }
    }
  } catch (e) {
    outcome = VpnCheckResult(connected: false, error: e.toString());
  }
  final stats = await NativeBridge.xrayStop();
  return VpnCheckResult(
    connected: outcome.connected,
    latencyMs: outcome.latencyMs,
    error: outcome.error,
    untestable: outcome.untestable,
    untestableReason: outcome.untestableReason,
    socketsBound: stats.bound,
    socketsUnbound: stats.unbound,
    sites: outcome.sites,
    download: outcome.download,
    bindError: stats.error,
  );
}

HttpClient _proxyClient(int localPort, Duration connectTimeout) {
  final client = HttpClient();
  client.connectionTimeout = connectTimeout;
  client.findProxy = (uri) => 'PROXY 127.0.0.1:$localPort';
  client.badCertificateCallback = (cert, host, port) => true;
  client.userAgent = 'Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Mobile Safari/537.36';
  return client;
}

/// GET a site through the tunnel, following redirects, reading the WHOLE
/// body -- ok only if real content came back (a bare 3xx with an empty body
/// moves no data and proves nothing; the server-side checker has the same
/// rule).
Future<Map<String, dynamic>> _fetchThroughProxy(int localPort, Uri uri, {required String host}) async {
  final client = _proxyClient(localPort, const Duration(seconds: 10));
  final sw = Stopwatch()..start();
  int bytes = 0;
  int? code;
  String? error;
  try {
    final req = await client.getUrl(uri).timeout(const Duration(seconds: 12));
    req.followRedirects = true;
    req.maxRedirects = 5;
    final resp = await req.close().timeout(const Duration(seconds: 15));
    code = resp.statusCode;
    await for (final chunk in resp.timeout(const Duration(seconds: 15))) {
      bytes += chunk.length;
      if (bytes > 2 * 1024 * 1024) break; // enough to prove the tunnel carries data
    }
  } catch (e) {
    error = e.runtimeType.toString();
  } finally {
    sw.stop();
    client.close(force: true);
  }
  return {
    'host': host,
    'ok': code != null && code < 400 && bytes > 0,
    'http_code': code,
    'time_ms': sw.elapsedMilliseconds,
    'bytes': bytes,
    'error': ?error,
  };
}

/// Pull the fixed test file through the tunnel and count what arrived:
/// complete = ok; a transfer that stops after the first tens of KB is the
/// classic "handshake passes, then DPI cuts the flow" signature.
Future<Map<String, dynamic>> _downloadThroughProxy(int localPort, Uri uri) async {
  final client = _proxyClient(localPort, const Duration(seconds: 10));
  final sw = Stopwatch()..start();
  int bytes = 0;
  int? expected;
  int? code;
  String? error;
  try {
    final req = await client.getUrl(uri).timeout(const Duration(seconds: 12));
    req.followRedirects = true;
    final resp = await req.close().timeout(const Duration(seconds: 15));
    code = resp.statusCode;
    expected = resp.contentLength > 0 ? resp.contentLength : null;
    // Whole-transfer budget: 1 MB in 40 s is 200 kbit/s -- anything slower
    // is not a usable tunnel anyway.
    await for (final chunk in resp.timeout(const Duration(seconds: 20))) {
      bytes += chunk.length;
      if (sw.elapsed > const Duration(seconds: 40)) break;
    }
  } catch (e) {
    error = e.runtimeType.toString();
  } finally {
    sw.stop();
    client.close(force: true);
  }
  final nominal = expected ?? 1000000;
  final ok = code != null && code < 400 && bytes >= (nominal * 0.9).floor();
  final seconds = sw.elapsedMilliseconds / 1000.0;
  return {
    'ok': ok,
    'http_code': code,
    'bytes': bytes,
    'expected_bytes': expected,
    'time_ms': sw.elapsedMilliseconds,
    'speed_mbps': seconds > 0 ? double.parse((bytes * 8 / seconds / 1e6).toStringAsFixed(2)) : 0,
    'error': ?error,
  };
}

Future<bool> _waitForPort(int port, {Duration timeout = const Duration(seconds: 3)}) async {
  final deadline = DateTime.now().add(timeout);
  while (DateTime.now().isBefore(deadline)) {
    try {
      final s = await Socket.connect('127.0.0.1', port, timeout: const Duration(milliseconds: 300));
      await s.close();
      return true;
    } catch (_) {
      await Future.delayed(const Duration(milliseconds: 150));
    }
  }
  return false;
}

Future<bool> _probeThroughProxy(int localPort) async {
  final client = HttpClient();
  client.connectionTimeout = const Duration(seconds: 12);
  client.findProxy = (uri) => 'PROXY 127.0.0.1:$localPort';
  client.badCertificateCallback = (cert, host, port) => true;
  try {
    final req = await client.getUrl(Uri.parse('https://www.gstatic.com/generate_204')).timeout(const Duration(seconds: 12));
    final resp = await req.close().timeout(const Duration(seconds: 12));
    await resp.drain<void>();
    return resp.statusCode < 500;
  } catch (_) {
    return false;
  } finally {
    client.close(force: true);
  }
}
