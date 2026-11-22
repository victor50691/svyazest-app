import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../models/profile.dart';
import '../theme.dart';
import 'api_client.dart';
import 'diag_log.dart';
import 'native_bridge.dart';

/// One-time GPS check support asks for (admin panel → Партнёры → GPS) when
/// the region a partner claimed looks doubtful. Like the log request:
/// nothing happens without the partner's «Разрешить», the notification
/// stays until answered (GpsPlugin.kt / GpsNotifier), and there is no
/// background tracking -- one fix, sent once.
///
/// Anti-spoofing signals travel with the fix (GpsPlugin.fix): the system's
/// mock-location flag, a mock-location app selected in Developer options,
/// Developer options on, satellites actually used, fix age. The server
/// turns them into a verdict for the admin.
class GpsBridge {
  static const _ch = MethodChannel('com.svyazest.svyazest_app/gps');

  static Future<({int id, String action})?> takeLaunchAction() async {
    try {
      final m = await _ch.invokeMethod<Map>('takeLaunchAction');
      if (m == null || m['id'] is! int) return null;
      return (id: m['id'] as int, action: (m['action'] ?? '').toString());
    } catch (_) {
      return null;
    }
  }

  static Future<bool> _bool(String m) async {
    try {
      return await _ch.invokeMethod<bool>(m) ?? false;
    } catch (_) {
      return false;
    }
  }

  static Future<bool> hasPermission() => _bool('hasPermission');
  static Future<bool> requestPermission() => _bool('requestPermission');
  static Future<bool> locationEnabled() => _bool('locationEnabled');
  static Future<bool> openLocationSettings() => _bool('openLocationSettings');
  static Future<bool> openAppSettings() => _bool('openAppSettings');
  static Future<bool> releasePermission() => _bool('releasePermission');

  /// 1.13.0: Google's one-tap «включить точное определение» dialog when
  /// location accuracy (Wi-Fi / towers) is off. state: already | enabled |
  /// declined | unavailable.
  static Future<String> ensureAccuracy() async {
    try {
      final m = await _ch.invokeMethod<Map>('ensureAccuracy');
      return (m?['state'] ?? 'unavailable').toString();
    } catch (_) {
      return 'unavailable';
    }
  }

  /// Can the phone place itself without satellites (Wi-Fi / cell towers)?
  static Future<bool> networkAvailable() async {
    try {
      final m = await _ch.invokeMethod<Map>('sources');
      return m?['network'] == true;
    } catch (_) {
      return false;
    }
  }

  static Future<Map<String, dynamic>> fix({int timeoutMs = 90000}) async {
    try {
      final m = await _ch.invokeMethod<Map>('fix', {'timeoutMs': timeoutMs});
      return (m ?? const {}).map((k, v) => MapEntry(k.toString(), v));
    } catch (e) {
      return {'error': 'native: $e'};
    }
  }
}

class GpsConsent {
  static final _api = ApiClient();

  /// Service and app: deliver a «Отклонить» given on the notification, keep
  /// the notification up only while the request is pending.
  static Future<GpsInfo?> sync(GpsInfo? gps) async {
    final declined = await NativeBridge.gpsPeekDecline();
    if (declined != null) {
      try {
        final st = await _api.gpsDecision(declined, false);
        await NativeBridge.gpsClearDecline(declined);
        if (gps != null && gps.id == declined) gps = GpsInfo(gps.id, st, gps.reason);
      } catch (_) {}
    }
    if (gps != null && gps.status == 'pending') {
      await NativeBridge.gpsShowRequest(gps.id, gps.reason);
    } else {
      await NativeBridge.gpsCancel();
    }
    return gps;
  }

  static Future<void> decline(GpsInfo g) async {
    await _api.gpsDecision(g.id, false);
    await NativeBridge.gpsCancel();
  }

  /// The check itself (UI only: permission needs the screen). Returns a line
  /// for the partner.
  static Future<String> run(BuildContext context, GpsInfo g) async {
    DiagLog.app.log('[gps] проверка #${g.id} начата (статус ${g.status})');
    await NativeBridge.gpsCancel();
    if (g.status == 'pending') await _api.gpsDecision(g.id, true);
    if (!await GpsBridge.hasPermission() && !await GpsBridge.requestPermission()) {
      await _report(g.id, {'error': 'permission_denied'});
      return 'Без доступа к геолокации проверить местоположение нельзя. Разрешите доступ в настройках приложения, '
          'поддержка сможет запросить проверку ещё раз.';
    }
    // Was location switched off before the check? Then it was turned on just
    // for it, and the partner is offered to switch it off again afterwards.
    final locationWasOff = !await GpsBridge.locationEnabled();
    if (locationWasOff) {
      if (!context.mounted) return '';
      final open = await showDialog<bool>(
        context: context,
        barrierDismissible: false,
        builder: (ctx) => AlertDialog(
          title: const Text('Включите геолокацию'),
          content: const Text('Чтобы определить местоположение, включите «Местоположение» в настройках телефона и вернитесь в приложение.'),
          actions: [
            TextButton(onPressed: () => Navigator.pop(ctx, false), child: const Text('Не сейчас')),
            TextButton(onPressed: () => Navigator.pop(ctx, true), child: const Text('Открыть настройки')),
          ],
        ),
      );
      if (open == true) {
        await GpsBridge.openLocationSettings();
        // Wait for the partner to come back with location on (up to 2 min).
        for (var i = 0; i < 60 && !await GpsBridge.locationEnabled(); i++) {
          await Future.delayed(const Duration(seconds: 2));
        }
      }
      if (!await GpsBridge.locationEnabled()) {
        return 'Геолокация выключена — проверка не выполнена. Включите её и откройте приложение снова.';
      }
    }
    // Wi-Fi / towers make indoor fixes work in seconds: let Google offer to
    // switch location accuracy on (one tap); without it only satellites are
    // left, and those are not seen indoors -- say so before the wait.
    final accuracy = await GpsBridge.ensureAccuracy();
    final network = await GpsBridge.networkAvailable();
    DiagLog.app.log('[gps] разрешение есть, геолокация включена; точность: $accuracy; определение по Wi-Fi/вышкам: ${network ? 'есть' : 'НЕТ'}');
    if (!network) {
      if (!context.mounted) return '';
      await showDialog<void>(
        context: context,
        barrierDismissible: false,
        builder: (ctx) => AlertDialog(
          title: const Text('Нужен вид на небо'),
          content: const Text(
            'На этом телефоне местоположение определяется только по спутникам (определение по Wi-Fi и вышкам '
            'выключено или недоступно). В помещении спутники обычно не видны — подойдите к окну или выйдите '
            'на улицу и нажмите «Начать».',
          ),
          actions: [TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Начать'))],
        ),
      );
    }
    if (!context.mounted) return '';
    final fixF = GpsBridge.fix();
    showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (ctx) => const PopScope(
        canPop: false,
        child: AlertDialog(
          content: Row(children: [
            SizedBox(width: 22, height: 22, child: CircularProgressIndicator(strokeWidth: 2.4)),
            SizedBox(width: 16),
            Expanded(child: Text('Определяем местоположение… Это может занять до полутора минут — лучше у окна или на улице.')),
          ]),
        ),
      ),
    );
    final fixAt = DateTime.now();
    final fix = await fixF;
    DiagLog.app.log('[gps] поиск за ${DateTime.now().difference(fixAt).inSeconds} с: '
        '${fix['error'] != null ? 'ошибка ${fix['error']}' : 'точка ${fix['provider']} ±${(fix['accuracy'] as num?)?.round()} м (${fix['source']})'}; '
        'спутников ${fix['satellites_used']}/${fix['satellites_seen']}, приёмник ${fix['gnss_started']}, источники ${fix['providers_enabled']}');
    if (context.mounted) Navigator.of(context, rootNavigator: true).pop();
    final cc = await NativeBridge.networkCountry();
    fix['operator'] = await NativeBridge.operatorName();
    fix['accuracy_setting'] = accuracy;
    fix['network_available'] = network;
    fix['network_country'] = cc.network;
    fix['sim_country'] = cc.sim;
    await _report(g.id, fix);
    // The check is over, whatever came of it (1.15.0): location tracking was
    // already stopped by GpsPlugin.fix; the permission goes back (Android 13+)
    // and, if location was off before, the partner may switch it off again.
    await GpsBridge.releasePermission();
    if (locationWasOff && await GpsBridge.locationEnabled() && context.mounted) {
      final off = await showDialog<bool>(
        context: context,
        builder: (ctx) => AlertDialog(
          title: const Text('Проверка завершена'),
          content: const Text('Геолокация больше не нужна. Вы включали её только для этой проверки — можно выключить её снова.'),
          actions: [
            TextButton(onPressed: () => Navigator.pop(ctx, false), child: const Text('Оставить')),
            TextButton(onPressed: () => Navigator.pop(ctx, true), child: const Text('Выключить')),
          ],
        ),
      );
      if (off == true) await GpsBridge.openLocationSettings();
    }
    if (fix['error'] == 'no_fix' || fix['error'] == 'no_provider') {
      return 'Не удалось определить местоположение. Проверьте, что в настройках геолокации включена «Точность определения» '
          '(или режим «Высокая точность»), и попробуйте у окна или на улице — поддержка может повторить запрос.';
    }
    if (fix['error'] != null) return 'Проверка не выполнена: ${fix['error']}';
    return 'Готово — местоположение отправлено поддержке. Спасибо!';
  }

  static Future<void> _report(int id, Map<String, dynamic> data) async {
    for (var i = 0; i < 3; i++) {
      try {
        await _api.gpsResult(id, data);
        return;
      } catch (_) {
        await Future.delayed(const Duration(seconds: 3));
      }
    }
  }
}

/// The in-app question; returns true = allow, false = decline.
Future<bool?> askGps(BuildContext context, [String reason = '']) => showDialog<bool>(
      context: context,
      barrierDismissible: false,
      builder: (ctx) => PopScope(
        canPop: false,
        child: AlertDialog(
          title: const Text('Подтвердите ваше местоположение'),
          content: Text(
            'Нужно один раз подтвердить по GPS, что вы находитесь в указанном в заявке регионе.\n\n'
            '${reason.isNotEmpty ? 'Причина запроса: $reason\n\n' : ''}'
            'Приложение определит местоположение один раз и отправит его поддержке. Постоянного слежения нет.',
          ),
          actions: [
            TextButton(
              onPressed: () => Navigator.pop(ctx, false),
              style: TextButton.styleFrom(foregroundColor: AppColors.textSecondary),
              child: const Text('Отклонить'),
            ),
            TextButton(onPressed: () => Navigator.pop(ctx, true), child: const Text('Разрешить')),
          ],
        ),
      ),
    );
