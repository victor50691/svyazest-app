import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'api_client.dart';

/// The operator's public IP, asked over the cellular network right before a
/// job (1.12.0, 2026-11-21) -- the admin sees it in Задания →
/// partner_network.ip. Since 1.11.0 the result itself may reach the server
/// through a VPN or Wi-Fi, so the request's own address no longer tells
/// which network the check ran from.
///
/// dart:io sockets follow the process binding, and the service binds the
/// process to the cellular network for jobs (NativeBridge.bindCellular), so
/// both requests below leave over mobile data. First our own server; under
/// «белые списки» it is usually unreachable that way, then Yandex's echo
/// (answers the real address only to browser-like requests).
class CellularIp {
  static const _yandex = 'https://ipv4-internet.yandex.net/api/v0/ip';
  static DateTime? _at;
  static Map<String, dynamic>? _last;

  static Future<Map<String, dynamic>> measure({String? token}) async {
    final at = _at;
    if (at != null && _last != null && _last!['ip'] != null && DateTime.now().difference(at) < const Duration(minutes: 2)) {
      return _last!;
    }
    final errors = <String>[];
    String? ip;
    String? via;
    try {
      final body = await _get('$apiBaseUrl/ip', {
        if (token != null) 'Authorization': 'Bearer $token',
        if (ApiClient.appVersion != null) 'X-App-Version': ApiClient.appVersion!,
      });
      final v = (jsonDecode(body) as Map)['ip'];
      if (v is String && v.isNotEmpty) { ip = v; via = 'dpichecker'; }
    } catch (e) {
      errors.add('сервер: ${_short(e)}');
    }
    // Yandex's echo now and then answers 504 between two good jobs
    // (2026-11-22, Теле2 under «белые списки») -- one more try a second later.
    for (var attempt = 0; ip == null && attempt < 2; attempt++) {
      if (attempt > 0) await Future.delayed(const Duration(seconds: 1));
      final who = attempt == 0 ? 'яндекс' : 'яндекс, повтор';
      try {
        final body = await _get(_yandex, {
          'User-Agent': 'Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36',
          'Origin': 'https://yandex.ru',
          'Referer': 'https://yandex.ru/internet/',
        });
        final v = jsonDecode(body);
        if (v is String && v.isNotEmpty && v != '127.0.0.1') { ip = v; via = 'yandex'; } else { errors.add('$who: ответ $body'); }
      } catch (e) {
        errors.add('$who: ${_short(e)}');
      }
    }
    final out = <String, dynamic>{'ip': ip, 'via': via, if (ip == null) 'error': errors.join('; ')};
    _at = DateTime.now();
    _last = out;
    return out;
  }

  static Future<String> _get(String url, Map<String, String> headers) async {
    final c = HttpClient()..connectionTimeout = const Duration(seconds: 5);
    try {
      final req = await c.getUrl(Uri.parse(url)).timeout(const Duration(seconds: 6));
      headers.forEach(req.headers.set);
      final resp = await req.close().timeout(const Duration(seconds: 6));
      final body = await resp.transform(utf8.decoder).join().timeout(const Duration(seconds: 6));
      if (resp.statusCode != 200) throw HttpException('HTTP ${resp.statusCode}');
      return body.trim();
    } finally {
      c.close(force: true);
    }
  }

  static String _short(Object e) {
    final s = e.toString().replaceAll(RegExp(r'\s+'), ' ');
    return s.length > 80 ? s.substring(0, 80) : s;
  }
}
