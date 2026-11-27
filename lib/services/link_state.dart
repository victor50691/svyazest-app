import 'dart:convert';

import 'package:flutter_foreground_task/flutter_foreground_task.dart';

/// «Связь с сервером» and «Белые списки» on the home screen (1.21.0).
///
/// The service (its own isolate) is the one talking to the server and
/// measuring the whitelist mode while it runs; it hands each result to the
/// screen ({'type': 'link'} via sendDataToMain) and keeps the last one in
/// flutter_foreground_task storage, so a screen opened later still shows it.
/// With the service off the screen asks the server itself (profile) and
/// measures the whitelist mode itself with the host lists the service kept
/// from its last heartbeat.
class LinkStore {
  static const _stateKey = 'link_state';
  static const _listsKey = 'wl_lists';

  static Future<void> save(Map<String, Object?> m) async {
    try {
      await FlutterForegroundTask.saveData(key: _stateKey, value: jsonEncode(m));
    } catch (_) {}
  }

  static Future<Map<String, dynamic>?> load() async {
    try {
      final s = await FlutterForegroundTask.getData<String>(key: _stateKey);
      if (s == null || s.isEmpty) return null;
      return Map<String, dynamic>.from(jsonDecode(s) as Map);
    } catch (_) {
      return null;
    }
  }

  /// Hosts for the whitelist probe, as the server gave them last.
  static Future<void> saveLists(List<String> allowed, List<String> control, bool required) async {
    try {
      await FlutterForegroundTask.saveData(
          key: _listsKey, value: jsonEncode({'allowed': allowed, 'control': control, 'required': required}));
    } catch (_) {}
  }

  static Future<({List<String> allowed, List<String> control, bool required})?> lists() async {
    try {
      final s = await FlutterForegroundTask.getData<String>(key: _listsKey);
      if (s == null || s.isEmpty) return null;
      final m = jsonDecode(s) as Map;
      final a = ((m['allowed'] as List?) ?? const []).map((e) => e.toString()).toList();
      final c = ((m['control'] as List?) ?? const []).map((e) => e.toString()).toList();
      if (a.isEmpty || c.isEmpty) return null;
      return (allowed: a, control: c, required: m['required'] != false);
    } catch (_) {
      return null;
    }
  }
}

/// Short words for the whitelist verdicts of NativePlugin.whitelistProbe.
String whitelistWord(String? verdict) {
  switch (verdict) {
    case 'active':
      return 'Включены';
    case 'inactive':
      return 'Не включены';
    case 'partial':
      return 'Частично';
    case 'no_dns':
      return 'Не работает DNS';
    case 'no_internet':
      return 'Нет интернета';
    case 'no_cellular':
      return 'Нет сети для проверок';
    default:
      return 'Нет данных';
  }
}

/// «10 с назад», «3 мин назад», «2 ч назад».
String agoText(DateTime at) {
  final d = DateTime.now().difference(at);
  if (d.inSeconds < 5) return 'только что';
  if (d.inSeconds < 60) return '${d.inSeconds} с назад';
  if (d.inMinutes < 60) return '${d.inMinutes} мин назад';
  if (d.inHours < 24) return '${d.inHours} ч назад';
  return '${d.inDays} дн. назад';
}
