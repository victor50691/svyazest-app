import 'dart:async';

import 'package:device_info_plus/device_info_plus.dart';
import 'package:flutter/widgets.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:package_info_plus/package_info_plus.dart';

import '../models/profile.dart';
import 'api_client.dart';
import 'native_bridge.dart';

/// Screens the partner opens / closes (MaterialApp.navigatorObservers).
class DiagNavObserver extends NavigatorObserver {
  String _name(Route<dynamic>? r) {
    if (r == null) return '—';
    final n = r.settings.name;
    if (n != null && n.isNotEmpty) return n;
    if (r is PopupRoute) return 'окно (${r.runtimeType})';
    return r.runtimeType.toString();
  }

  @override
  void didPush(Route<dynamic> route, Route<dynamic>? previousRoute) =>
      DiagLog.app.log('[экран] открыт ${_name(route)} (поверх ${_name(previousRoute)})');

  @override
  void didPop(Route<dynamic> route, Route<dynamic>? previousRoute) =>
      DiagLog.app.log('[экран] закрыт ${_name(route)} → ${_name(previousRoute)}');
}

/// Detailed diagnostics log, collected ONLY while the partner has allowed
/// it: support asks from the admin panel (Партнёры → «Логи»), the phone
/// asks the partner (DiagConsent below), and until the request is stopped
/// -- by the admin, or by the partner in Настройки -- lines go to
/// POST /diag/:id/logs. Without an accepted request [log] does nothing.
///
/// What goes in (1.14.0 «super detailed»): the app's own state (network,
/// VPN, permissions, statuses, server answers with timings, errors with
/// stacks, every step of every job), the native half (NativeLog.kt: every
/// network up/down/change, the route of every request to the server,
/// power / Doze / data saver / screen events, GPS steps, xray), periodic
/// snapshots of the phone's state, crashes and why Android ended the
/// process. Never the resources being checked (customer keys / IPs /
/// domains): job code registers them with [secret] and every line is
/// masked before it is queued.
///
/// One instance per isolate: [service] in the foreground service, [app] in
/// the UI. The accepted request id is shared through flutter_foreground_task
/// storage, so the service picks collection up right after it starts.
class DiagLog {
  DiagLog._(this.source);

  static final service = DiagLog._('service');
  static final app = DiagLog._('app');
  static const _storeKey = 'diag_active_id';

  final String source;
  final _api = ApiClient();
  final _queue = <Map<String, dynamic>>[];
  Timer? _timer;
  bool _flushing = false;
  DateTime? _envAt;

  /// Strings that must never reach the log (the job's resource, its host,
  /// the address it resolved to). Shared by both instances of this isolate.
  static final _secrets = <String>{};
  static void secret(String? s) {
    final v = (s ?? '').trim();
    if (v.length < 4) return;
    _secrets.add(v);
    if (_secrets.length > 300) _secrets.remove(_secrets.first);
  }

  static void clearSecrets() => _secrets.clear();

  static String _mask(String msg) {
    var out = msg;
    for (final x in _secrets) {
      if (out.contains(x)) out = out.replaceAll(x, '‹цель›');
    }
    return out;
  }

  void logError(String where, Object e, [StackTrace? st]) {
    final lines = (st?.toString() ?? '').split('\n').where((l) => l.trim().isNotEmpty).take(25).join('\n');
    log('ОШИБКА [$where]: ${e.runtimeType}: $e${lines.isEmpty ? '' : '\n$lines'}');
  }

  /// Accepted request this isolate is logging for (null = not collecting).
  final ValueNotifier<int?> activeId = ValueNotifier<int?>(null);
  bool get active => activeId.value != null;

  void log(String msg) {
    if (activeId.value == null) return;
    _add(DateTime.now().millisecondsSinceEpoch, source, msg);
  }

  void _add(int t, String src, String msg) {
    _queue.add({'t': t, 's': src, 'm': _mask(msg)});
    // Offline for a long time: keep the newest lines, not unlimited memory.
    if (_queue.length > 20000) _queue.removeRange(0, _queue.length - 20000);
    if (_queue.length >= 100) {
      unawaited(flush());
    } else {
      _timer ??= Timer(const Duration(seconds: 5), () => unawaited(flush()));
    }
  }

  /// Phone state every [every] (service loop), and once when logging starts.
  Future<void> envTick({Duration every = const Duration(minutes: 5)}) async {
    if (!active) return;
    final at = _envAt;
    if (at != null && DateTime.now().difference(at) < every) return;
    _envAt = DateTime.now();
    for (final l in await NativeBridge.diagEnv()) {
      log('[снимок] $l');
    }
  }

  /// Picks up collection that is already running (service start).
  Future<void> restore() async {
    try {
      final id = await FlutterForegroundTask.getData<int>(key: _storeKey);
      if (id != null && id > 0) await setActive(id, persist: false);
    } catch (_) {}
  }

  Future<void> setActive(int? id, {bool persist = true}) async {
    final was = activeId.value;
    if (was == id) return;
    activeId.value = id;
    await NativeBridge.diagNativeEnable(id != null);
    if (persist) {
      try {
        if (id == null) {
          await FlutterForegroundTask.removeData(key: _storeKey);
        } else {
          await FlutterForegroundTask.saveData(key: _storeKey, value: id);
        }
      } catch (_) {}
    }
    if (id == null) {
      _queue.clear();
      _timer?.cancel();
      _timer = null;
      return;
    }
    log('— сбор логов включён (запрос #$id, $source) —');
    await _snapshot();
  }

  Future<void> flush() async {
    _timer?.cancel();
    _timer = null;
    final id = activeId.value;
    if (id == null || _flushing) return;
    // The native half's lines (NativeLog.kt) ride along with this isolate's.
    for (final n in await NativeBridge.diagNativeDrain()) {
      _add(n.t, 'native', n.m);
    }
    _timer?.cancel();
    _timer = null;
    if (_queue.isEmpty) return;
    _flushing = true;
    final batch = List<Map<String, dynamic>>.of(_queue.take(1000));
    var retry = false;
    try {
      final stillActive = await _api.diagLogs(id, batch);
      _queue.removeRange(0, batch.length.clamp(0, _queue.length));
      if (!stillActive) await setActive(null);
    } catch (_) {
      retry = true; // no network / server busy -- the lines wait
    } finally {
      _flushing = false;
    }
    if (activeId.value != null && _queue.isNotEmpty) {
      _timer ??= Timer(Duration(seconds: retry ? 20 : 2), () => unawaited(flush()));
    } else if (activeId.value != null) {
      // Keep pulling the native half even when this isolate is quiet.
      _timer ??= Timer(const Duration(seconds: 15), () => unawaited(flush()));
    }
  }

  Future<void> _snapshot() async {
    try {
      final pkg = await PackageInfo.fromPlatform();
      final a = await DeviceInfoPlugin().androidInfo;
      log('устройство: ${a.manufacturer} ${a.model}, Android ${a.version.release} (SDK ${a.version.sdkInt}); '
          'приложение ${pkg.version} (${pkg.buildNumber}); xray-core ${await NativeBridge.xrayVersion()}');
      final net = await NativeBridge.networkState();
      final cc = await NativeBridge.networkCountry();
      log('сеть: мобильная=${net.mobile} переключатель=${net.mobileSetting} VPN=${net.vpn}; '
          'оператор=${await NativeBridge.operatorName()} страна сети=${cc.network} SIM=${cc.sim}');
      final perms = await NativeBridge.permissions();
      log('разрешения: уведомления=${perms.notifications} батарея=${perms.battery}');
      _envAt = null;
      await envTick();
      for (final l in await NativeBridge.diagPastExits()) {
        log('[прошлый запуск] $l');
      }
    } catch (e, st) {
      logError('снимок состояния', e, st);
    }
  }
}

/// The partner's consent: support's request arrives as `diag` in /heartbeat
/// (service) and /profile (app). Pending -> the notification «Разрешить
/// сбор логов?» (DiagNotifier.kt) stays up, and the app shows a dialog;
/// accepted -> [DiagLog] collects; anything else -> nothing.
class DiagConsent {
  static final _api = ApiClient();

  /// Brings this isolate in line with the server: delivers an answer given
  /// on the notification, shows/hides the notification, starts/stops [log].
  /// Returns the request as it stands now.
  static Future<DiagInfo?> sync(DiagInfo? diag, DiagLog log) async {
    final dec = await NativeBridge.diagPeekDecision();
    if (dec != null) {
      try {
        final status = await _api.diagDecision(dec.id, dec.accept);
        await NativeBridge.diagClearDecision(dec.id);
        if (diag != null && diag.id == dec.id) diag = DiagInfo(dec.id, status, diag.reason);
      } catch (_) {
        // Not delivered: the answer stays stored, the notification stays
        // hidden (DiagNotifier.show skips an answered request).
      }
    }
    if (diag != null && diag.status == 'pending') {
      await NativeBridge.diagShowRequest(diag.id, diag.reason);
    } else {
      await NativeBridge.diagCancel();
    }
    await log.setActive(diag != null && diag.status == 'accepted' ? diag.id : null);
    return diag;
  }

  /// The answer from the in-app dialog.
  static Future<DiagInfo?> decide(DiagInfo d, bool accept, DiagLog log) async {
    final status = await _api.diagDecision(d.id, accept);
    await NativeBridge.diagCancel();
    final now = DiagInfo(d.id, status, d.reason);
    await log.setActive(status == 'accepted' ? d.id : null);
    return now;
  }

  /// Настройки → «Остановить сбор логов».
  static Future<void> stop(int id, DiagLog log) async {
    await log.flush();
    await _api.diagStop(id);
    await log.setActive(null);
  }
}
