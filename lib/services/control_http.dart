import 'dart:io' show HttpDate;

import 'package:flutter/services.dart';
import 'package:http/http.dart' as http;

import 'native_bridge.dart';

/// http.Client for the app's own server that goes through the native
/// control channel (ControlChannel.kt, 1.11.0): VPN → Wi-Fi → the VPN
/// client's local proxy → cellular, first that connects. The checks stay on
/// the bare cellular network (the process is bound to it); only the link to
/// the server takes another way -- so it works under «белые списки» while
/// the partner keeps using the internet through a VPN.
class ControlHttpClient extends http.BaseClient {
  static const _ch = MethodChannel('com.svyazest.svyazest_app/native');

  /// Where the last request went, for Настройки and the diagnostics log.
  static String? lastRoute;
  static void Function(String route, String failed)? onRouteChanged;

  /// Server clock minus phone clock (from the Date header of every answer):
  /// signatures carry the server's time, so a phone with a wrong clock
  /// still signs within the server's window.
  static int _clockOffsetMs = 0;

  @override
  Future<http.StreamedResponse> send(http.BaseRequest request) async {
    final body = await request.finalize().toBytes();
    final outHeaders = Map<String, String>.of(request.headers);
    // 1.16.0: job requests are signed with the hardware-bound device key
    // (server: routes/executor/index.js requireSignature).
    if (request.headers.containsKey('Authorization') && request.url.path.contains('/jobs/')) {
      final ts = DateTime.now().millisecondsSinceEpoch + _clockOffsetMs;
      final sig = await NativeBridge.signRequest('${request.method} ${request.url.path} $ts');
      if (sig != null) {
        outHeaders['X-Sig-Ts'] = '$ts';
        outHeaders['X-Sig'] = sig;
      }
    }
    final Map? r;
    try {
      r = await _ch.invokeMethod<Map>('controlRequest', {
        'method': request.method,
        'url': request.url.toString(),
        'headers': outHeaders,
        'body': body.isEmpty ? null : Uint8List.fromList(body),
        'readTimeoutMs': 70000,
      });
    } on PlatformException catch (e) {
      throw http.ClientException('control channel: ${e.message}', request.url);
    }
    final status = (r?['status'] as num?)?.toInt() ?? 0;
    if (status == 0) {
      throw http.ClientException((r?['error'] ?? 'network is unreachable').toString(), request.url);
    }
    final route = r?['route']?.toString();
    if (route != null && route != lastRoute) {
      lastRoute = route;
      onRouteChanged?.call(route, (r?['failed'] ?? '').toString());
    }
    final bytes = (r?['body'] as Uint8List?) ?? Uint8List(0);
    final headers = <String, String>{};
    final h = r?['headers'];
    if (h is Map) h.forEach((k, v) => headers[k.toString()] = v.toString());
    final date = headers['date'];
    if (date != null) {
      try {
        _clockOffsetMs = HttpDate.parse(date).millisecondsSinceEpoch - DateTime.now().millisecondsSinceEpoch;
      } catch (_) {}
    }
    return http.StreamedResponse(
      Stream.value(bytes),
      status,
      contentLength: bytes.length,
      request: request,
      headers: headers,
    );
  }

  static Future<({String route, String? error, int manualPort, String proxyUser})> info() async {
    try {
      final m = await _ch.invokeMethod<Map>('controlInfo');
      return (
        route: (m?['route_short'] ?? m?['route'] ?? '—').toString(),
        error: m?['error']?.toString(),
        manualPort: (m?['manual_port'] as num?)?.toInt() ?? 0,
        proxyUser: (m?['proxy_user'] ?? '').toString(),
      );
    } catch (_) {
      return (route: '—', error: null, manualPort: 0, proxyUser: '');
    }
  }

  /// Настройки → «Прокси VPN-клиента» (1.16.2): port (0 = find it), login
  /// and password ('' = no auth).
  static Future<void> setProxy(int port, String user, String pass) async {
    try {
      await _ch.invokeMethod('controlSetProxy', {'port': port, 'user': user, 'pass': pass});
    } catch (_) {}
  }

  /// Настройки → «Найти прокси VPN-клиента»: what listens on each port.
  static Future<({String? found, List<String> report, String ports})> scanProxy() async {
    try {
      final m = await _ch.invokeMethod<Map>('controlScanProxy');
      return (
        found: m?['found']?.toString(),
        report: ((m?['report'] as List?) ?? const []).map((e) => e.toString()).toList(),
        ports: (m?['ports'] ?? '').toString(),
      );
    } catch (e) {
      return (found: null, report: ['ошибка: $e'], ports: '');
    }
  }

  static Future<void> setProxyPort(int port) async {
    try {
      await _ch.invokeMethod('controlSetProxyPort', {'port': port});
    } catch (_) {}
  }
}
