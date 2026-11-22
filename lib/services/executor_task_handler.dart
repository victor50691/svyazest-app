import 'dart:async';
import 'dart:io';

import 'dart:convert';
import 'dart:ui' show PlatformDispatcher;

import 'package:flutter_foreground_task/flutter_foreground_task.dart';

import 'api_client.dart';
import 'attestation.dart';
import 'cellular_ip.dart';
import 'secure_store.dart';
import 'control_http.dart';
import 'checkers/ip_checker.dart';
import 'checkers/subnet_checker.dart';
import 'checkers/vpn_checker.dart';
import 'diag_log.dart';
import 'gps_check.dart';
import 'native_bridge.dart';
import 'vpn_uri_parser.dart';

/// Runs entirely in the foreground service's background isolate (a
/// separate Dart isolate from the UI -- see main.dart's startCallback).
/// Deliberately a single owned while-loop rather than flutter_foreground_task's
/// periodic onRepeatEvent: each poll cycle blocks on a ~25-40s long-poll
/// HTTP request, which doesn't fit a fixed-interval tick model cleanly.
@pragma('vm:entry-point')
void executorTaskHandlerCallback() {
  FlutterForegroundTask.setTaskHandler(ExecutorTaskHandler());
}

class ExecutorTaskHandler extends TaskHandler {
  final _api = ApiClient();
  /// Root on this device, checked once per service start (1.18.0).
  bool? _rooted;
  bool _running = true;
  int _consecutiveErrors = 0;
  String? _hwid;
  // Lease keepalive for the job being checked (POST /jobs/:id/alive every 5 s).
  Timer? _aliveTimer;

  void _startAlive(int jobId) {
    _stopAlive();
    var n = 0;
    Future<void> ping() async {
      final i = ++n;
      final t = DateTime.now();
      try {
        await _api.jobAlive(jobId);
        if (i == 1 || i % 6 == 0) DiagLog.service.log('[задание $jobId] продление аренды #$i ок за ${_ms(t)}');
      } catch (e) {
        DiagLog.service.log('[задание $jobId] продление аренды #$i НЕ УДАЛОСЬ за ${_ms(t)}: $e — через 15 с без продления задание отдадут другому');
      }
    }
    unawaited(ping());
    _aliveTimer = Timer.periodic(const Duration(seconds: 5), (_) => unawaited(ping()));
  }

  static String _ms(DateTime from) => '${DateTime.now().difference(from).inMilliseconds} мс';

  DateTime? _reattestAt;
  Future<void> _reattest(String why) async {
    final at = _reattestAt;
    if (at != null && DateTime.now().difference(at) < const Duration(minutes: 10)) return;
    _reattestAt = DateTime.now();
    try {
      final status = await Attestation.run();
      DiagLog.service.log('[подлинность] перепроверка ($why): $status');
    } catch (e) {
      DiagLog.service.log('[подлинность] перепроверка ($why) не удалась: $e');
    }
  }

  /// What the job result says, without the customer's resource: only
  /// verdict fields (and control-site names, which are ours).
  static String _summary(String type, Map<String, dynamic> r) {
    String v(String k) => r[k] == null ? '' : '$k=${r[k]} ';
    if (type == 'vpn_key') {
      final sites = (r['sites'] as List?) ?? const [];
      final ok = sites.where((x) => x is Map && x['ok'] == true).length;
      final dl = r['download'] is Map ? r['download'] as Map : null;
      return '${v('works')}${v('connected')}${v('latency_ms')}сайты $ok/${sites.length} '
          '${sites.whereType<Map>().map((x) => '${x['host']}:${x['ok'] == true ? 'ок' : 'нет'}${x['http_code'] != null ? '/${x['http_code']}' : ''}').join(',')} '
          '${dl != null ? 'файл=${dl['ok']} ${dl['speed_mbps'] ?? ''}Мбит/с ' : ''}${v('sockets_bound')}${v('sockets_unbound')}${v('untestable_reason')}${v('error')}';
    }
    if (type == 'subnet') return '${v('alive')}${v('probed')}${v('verdict')}${v('untestable_reason')}${v('error')}';
    final p = r['probe'] is Map ? r['probe'] as Map : null;
    return '${v('accessible')}${v('tls_ok')}${v('http_code')}${v('time_ms')}${v('tcp_latency_ms')}${v('bytes')}${v('block_reason')}'
        '${v('anti_bot')}${v('redirects')}${v('payload_ok')}${v('payload_bytes')}${v('stall_bytes')}'
        '${p != null ? 'пинг=${p['icmp_ms']}мс tcp=${p['tcp_ms']}мс ' : ''}${v('error')}';
  }

  static String _wlSummary(Map<String, dynamic>? w) {
    if (w == null) return 'не замерялись';
    String side(Object? a) => ((a as List?) ?? const []).whereType<Map>()
        .map((x) => '${x['host']}:${x['ok'] == true ? 'ок' : 'нет'}${x['tcp_ms'] != null ? '/${x['tcp_ms']}мс' : ''}').join(',');
    final dns = w['dns'] is Map ? w['dns'] as Map : const {};
    return 'вердикт=${w['verdict']}; из БС: ${side(w['allowed'])}; контрольные: ${side(w['control'])}; '
        'DNS ${dns['verdict']} (системный ${dns['system']}/${dns['total']}, запасной ${dns['fallback']}, частный DNS ${dns['private_dns']})';
  }

  /// The job's own resource never reaches the log: it, its host and any
  /// address in a JSON config are masked (DiagLog.secret).
  static void _hideResource(String resource) {
    DiagLog.clearSecrets();
    DiagLog.secret(resource);
    final k = parseVpnUri(resource);
    DiagLog.secret(k?.address);
    for (final m in RegExp(r'"(?:address|server|host|serverName|sni)"\s*:\s*"([^"]+)"').allMatches(resource)) {
      DiagLog.secret(m.group(1));
    }
    try {
      final u = Uri.parse(resource.contains('://') ? resource : 'x://$resource');
      DiagLog.secret(u.host);
      for (final q in u.queryParameters.values) {
        if (q.length >= 6) DiagLog.secret(q);
      }
    } catch (_) {}
  }

  void _stopAlive() {
    _aliveTimer?.cancel();
    _aliveTimer = null;
  }
  // When "mobile data off" was last reported to the server (null = online
  // or not reported yet), see _loop.
  DateTime? _offlineReportedAt;
  // «Белые списки» state: when the mode was last measured, the verdict
  // waiting to be reported, and the last one the server knows about (what
  // the notification text is based on).
  DateTime? _whitelistProbedAt;
  String? _pendingWhitelistVerdict;
  String? _lastWhitelistVerdict;
  // Whether a «Частный DNS» was on at that measurement (the no_dns hint).
  bool _lastPrivateDns = false;
  // IPv6 of the mobile network (1.17.2): measured every [_ipv6Interval] and
  // sent with every heartbeat -- the server gives IPv6 checks (an IPv6
  // address, a domain «через IPv6») only to phones that say true.
  bool? _ipv6Ok;
  DateTime? _ipv6At;
  static const _ipv6Interval = Duration(minutes: 10);

  /// IPv6 works over the cellular network = it has a global IPv6 address and
  /// an IPv6 default route (LinkProperties, no traffic), and Yandex's
  /// IPv6-only echo answers over it (Yandex is in every whitelist, so this
  /// also holds under «белые списки»).
  Future<void> _measureIpv6() async {
    _ipv6At = DateTime.now();
    final lp = await NativeBridge.cellularIpv6();
    bool ok = false;
    String why;
    if (!lp.global || !lp.route) {
      why = 'нет IPv6 в мобильной сети (адрес ${lp.global ? 'есть' : 'нет'}, маршрут ${lp.route ? 'есть' : 'нет'}${lp.error != null ? ', ${lp.error}' : ''})';
    } else {
      final echo = await ApiClient.cellularEcho(6);
      ok = echo.ip != null;
      why = ok ? 'IPv6 работает' : 'IPv6-адрес есть, но эхо не ответило: ${echo.error}';
    }
    if (_ipv6Ok != ok) DiagLog.service.log('[IPv6] $why');
    _ipv6Ok = ok;
  }
  // Per-isolate id in every trace line: two service instances alive at once
  // would show up as two different ids interleaving.
  final String _instance = DateTime.now().millisecondsSinceEpoch.toRadixString(36);

  /// Server-side breadcrumb (executor_devices.last_error) -- the only log a
  /// phone without adb has. Best effort, never throws.
  Future<void> _trace(String msg) async {
    DiagLog.service.log(msg);
    try {
      await _api.reportError('[$_instance] $msg');
    } catch (_) {}
  }

  @override
  Future<void> onStart(DateTime timestamp, TaskStarter starter) async {
    if (await NativeBridge.isRooted()) {
      // 1.18.1: a rooted phone runs only with the admin's exception for the
      // partner (profile.integrity_exempt) -- asked here too, not only on
      // the start screen (1.18.0 stopped the service right away anyway).
      bool exempt;
      try {
        exempt = (await _api.profile()).integrityExempt;
        await SecureStore.setRootExempt(exempt);
      } catch (_) {
        exempt = await SecureStore.getRootExempt();
      }
      if (!exempt) {
        _running = false;
        await FlutterForegroundTask.stopService();
        return;
      }
    }
    _running = true;
    await ApiClient.loadAppVersion();
    // Logging allowed by the partner earlier keeps going in this isolate.
    await DiagLog.service.restore();
    // Anything that escapes the loop's own try/catch (timers, callbacks).
    PlatformDispatcher.instance.onError = (e, st) {
      DiagLog.service.logError('служба: необработанная ошибка', e, st);
      return true;
    };
    // Did the last run end without onDestroy (killed by Android / crashed)?
    try {
      final wasRunning = await FlutterForegroundTask.getData<bool>(key: 'svc_running');
      final aliveAt = await FlutterForegroundTask.getData<int>(key: 'svc_alive');
      if (wasRunning == true) {
        final when = aliveAt != null ? DateTime.fromMillisecondsSinceEpoch(aliveAt) : null;
        DiagLog.service.log('ПРОШЛЫЙ ЗАПУСК СЛУЖБЫ ОБОРВАЛСЯ без остановки (Android убил процесс или сбой); '
            'последний признак жизни: ${when ?? 'неизвестно'}${when != null ? ' (${DateTime.now().difference(when).inMinutes} мин назад)' : ''}');
      }
      await FlutterForegroundTask.saveData(key: 'svc_running', value: true);
    } catch (_) {}
    for (final l in await NativeBridge.diagPastExits()) {
      DiagLog.service.log('[прошлый запуск] $l');
    }
    DiagLog.service.log('сервис запущен (${starter.name})');
    ControlHttpClient.onRouteChanged = (route, failed) =>
        DiagLog.service.log('связь с сервером: $route${failed.isEmpty ? '' : ' (не вышло: $failed)'}');
    // Keep mobile data up next to Wi-Fi and route this process's own
    // sockets (heartbeat, poll, IP/domain checks) through it.
    await NativeBridge.cellularRequest(true);
    unawaited(_loop());
  }

  @override
  void onRepeatEvent(DateTime timestamp) {
    // Unused -- see class doc comment. flutter_foreground_task still
    // requires an eventAction to be configured; onStart's own loop does
    // all the real work.
  }

  @override
  Future<void> onDestroy(DateTime timestamp, bool isTimeout) async {
    _running = false;
    _stopAlive();
    DiagLog.service.log('сервис остановлен${isTimeout ? ' (таймаут системы)' : ''}');
    try {
      await FlutterForegroundTask.saveData(key: 'svc_running', value: false);
    } catch (_) {}
    try {
      await DiagLog.service.flush().timeout(const Duration(seconds: 5));
    } catch (_) {}
    // Tell the server right away, so the admin panel and the DPI//Checker
    // bot stop counting this phone as online (otherwise it looks online
    // for up to two minutes after the switch is turned off).
    try {
      await _api.heartbeat(mobileDataEnabled: false, isOnline: false, hwid: _hwid).timeout(const Duration(seconds: 5));
    } catch (_) {}
    await NativeBridge.xrayStop();
    await NativeBridge.bindProcessToCellular(false);
    await NativeBridge.cellularRequest(false);
  }

  @override
  void onReceiveData(Object data) {}

  Future<void> _loop() async {
    while (_running) {
      try {
        // Both permissions are mandatory (see home_screen.dart). If the user
        // revokes one while we run, stop rather than limp along as a service
        // the system will kill or freeze anyway.
        final perms = await NativeBridge.permissions();
        if (!perms.notifications || !perms.battery) {
          _report(status: 'permissions', title: 'Связь Есть? — остановлено', text: 'Нужны разрешения, откройте приложение');
          _running = false;
          await FlutterForegroundTask.stopService();
          break;
        }
        bool hasMobileData;
        try {
          hasMobileData = await NativeBridge.hasMobileData();
        } catch (e) {
          throw StateError('native hasMobileData: $e');
        }
        if (await NativeBridge.hasActiveVpn()) {
          final ld = await NativeBridge.vpnLockdown();
          try {
            await _api.reportError('service vpnLockdown=${ld.lockdown} ${ld.detail}');
          } catch (_) {}
          if (ld.lockdown) {
            _report(
              status: 'vpn_lockdown',
              title: 'Связь Есть? — остановлено',
              text: 'VPN не разрешает обход — добавьте приложение в исключения VPN или выключите VPN',
            );
            _running = false;
            await FlutterForegroundTask.stopService();
            break;
          }
        }
        if (!hasMobileData) {
          // Tell the server right away, or the DPI//Checker bot keeps
          // counting this phone as a free executor until its heartbeat goes
          // stale and customers pay for checks nobody can run. The pinned
          // cellular network is gone, so unpin first: the report goes out
          // over Wi-Fi if there is one (no network at all -> the server's
          // 60 s staleness window covers it). Once per outage, then every
          // 30 s so a later Wi-Fi connection still gets it through.
          final now = DateTime.now();
          if (_offlineReportedAt == null || now.difference(_offlineReportedAt!) > const Duration(seconds: 30)) {
            _offlineReportedAt = now;
            await NativeBridge.bindProcessToCellular(false);
            try {
              _hwid ??= await NativeBridge.hwid();
              await _api
                  .heartbeat(mobileDataEnabled: false, isOnline: true, hwid: _hwid)
                  .timeout(const Duration(seconds: 5));
            } catch (_) {
              _offlineReportedAt = null; // not delivered -- retry next cycle
            }
          }
          // Switch on but the link is down (Wi-Fi phones): the system is
          // being asked to bring it up -- say so instead of "no data".
          final st = await NativeBridge.networkState();
          _report(
            status: 'no_mobile_data',
            title: 'Связь Есть? — офлайн',
            text: st.mobileSetting == true ? 'Ожидание мобильной сети...' : 'Включите мобильный интернет',
          );
          await Future.delayed(const Duration(seconds: 5));
          continue;
        }

        _offlineReportedAt = null;

        // Re-pin every cycle: the cellular Network object changes on every
        // reconnect, and a VPN toggled on in between must not capture us.
        await NativeBridge.bindProcessToCellular(true, waitMs: 3000);

        _hwid ??= await NativeBridge.hwid();
        final cc = await NativeBridge.networkCountry();
        // Liveness for "did the last run end abruptly" (see onStart), every cycle.
        unawaited(FlutterForegroundTask.saveData(key: 'svc_alive', value: DateTime.now().millisecondsSinceEpoch).catchError((_) => false));
        unawaited(DiagLog.service.envTick());
        if (_ipv6At == null || DateTime.now().difference(_ipv6At!) >= _ipv6Interval) {
          try {
            await _measureIpv6();
          } catch (e) {
            DiagLog.service.log('[IPv6] замер не удался: $e');
          }
        }
        final hbAt = DateTime.now();
        final hb = await _api.heartbeat(
          mobileDataEnabled: true, isOnline: true, hwid: _hwid,
          operatorName: await NativeBridge.operatorName(),
          country: cc.network, simCountry: cc.sim,
          // The verdict measured since the last heartbeat, if any.
          whitelistVerdict: _pendingWhitelistVerdict,
          ipv6: _ipv6Ok,
          rooted: _rooted ??= await NativeBridge.isRooted(),
        );
        if (_pendingWhitelistVerdict != null) {
          _lastWhitelistVerdict = _pendingWhitelistVerdict;
          _pendingWhitelistVerdict = null;
        }
        // Support's log request: notification / answer / collection.
        await DiagConsent.sync(hb.diag, DiagLog.service);
        // Support's GPS check request: keep its notification up while pending.
        await GpsConsent.sync(hb.gps);
        // Daily re-attestation of the hardware-bound key (1.16.0), by itself.
        if (hb.attestDue) await _reattest('сервер просит ежедневную перепроверку');
        _diagChanged('heartbeat', 'heartbeat ок: БС обязательны=${hb.whitelistRequired}, замер БС=${hb.canProbe}, '
            'вердикт=$_lastWhitelistVerdict; сеть=${cc.network} SIM=${cc.sim}');
        DiagLog.service.log('heartbeat за ${_ms(hbAt)}: связь «${ControlHttpClient.lastRoute ?? '?'}»; БС обязательны=${hb.whitelistRequired}, '
            'интервал замера ${hb.whitelistInterval.inMinutes} мин; логи=${hb.diag?.status ?? '—'}, GPS=${hb.gps?.status ?? '—'}');

        // «Белые списки»: re-measure on the server's schedule, not only when
        // a job arrives -- the server decides whether this phone gets work
        // at all from this verdict, so it has to exist beforehand.
        final now = DateTime.now();
        if (hb.canProbe &&
            (_whitelistProbedAt == null || now.difference(_whitelistProbedAt!) >= hb.whitelistInterval)) {
          _whitelistProbedAt = now;
          final wlAt = DateTime.now();
          final wl = await NativeBridge.whitelistProbe(hb.whitelistAllowed, hb.whitelistControl);
          DiagLog.service.log('замер белых списков за ${_ms(wlAt)}: ${_wlSummary(wl)}');
          final verdict = wl['verdict'] as String?;
          if (verdict != null && verdict != 'unknown') {
            _pendingWhitelistVerdict = verdict;
            _lastWhitelistVerdict = verdict;
            _lastPrivateDns = (wl['dns'] as Map?)?['private_dns'] == true;
            await _trace('whitelist verdict=$verdict dns=${wl['dns']}');
          }
        }

        // The phone's resolver is dead: every check would come out wrong,
        // so the server hands out nothing until it answers again. A
        // «Частный DNS» is the usual culprit and the one thing the person
        // can fix themselves.
        if (_lastWhitelistVerdict == 'no_dns') {
          _report(
            status: 'no_dns',
            title: 'Связь Есть? — не работает DNS',
            text: _lastPrivateDns
                ? 'Отключите «Частный DNS» в настройках телефона — заданий нет'
                : 'DNS мобильной сети не отвечает — заданий нет',
          );
        } else if (hb.whitelistRequired && _lastWhitelistVerdict != null && _lastWhitelistVerdict != 'active') {
          // Nothing to measure for a БС check while the operator is not in
          // whitelist mode: say so plainly instead of "waiting for jobs",
          // which would look like the network is idle.
          _report(
            status: 'no_whitelist',
            title: 'Связь Есть? — онлайн',
            text: 'Белые списки у оператора не включены — заданий нет',
          );
        } else {
          _report(status: 'online', title: 'Связь Есть? — онлайн', text: 'Ожидание заданий...');
        }

        final pollAt = DateTime.now();
        final job = await _api.pollJob();
        DiagLog.service.log(job == null
            ? 'опрос заданий за ${_ms(pollAt)}: пусто'
            : 'опрос заданий за ${_ms(pollAt)}: задание ${job.id}, тип ${job.resourceType}${job.protocol != null ? ', протокол ${job.protocol}' : ''}');
        _diagChanged('poll', job == null ? 'опрос: заданий нет' : 'опрос: задание ${job.id} (${job.resourceType})');
        if (!_running) {
          // Switched off while the poll was in flight: give the job back
          // instead of letting it expire on a phone that will not run it.
          if (job != null) {
            try {
              await _api.releaseJob(job.id);
              await _trace('job ${job.id} released (stopping)');
            } catch (_) {}
          }
          break;
        }
        if (job == null) {
          _consecutiveErrors = 0;
          continue; // immediately re-poll; server already long-polled ~25s
        }
        // Keep the lease alive for as long as the check runs; stopped before
        // the result goes out (and on any error below).
        _hideResource(job.resource);
        final jobAt = DateTime.now();
        _startAlive(job.id);
        await _trace('job ${job.id} received (${job.resourceType})');

        _report(
          status: 'running',
          title: 'Связь Есть? — выполняется проверка',
          text: _describeResource(job.resourceType),
        );

        // Pin again right before the check (the cellular Network may have
        // been replaced during the long poll) and record the outcome
        // alongside the result -- this is what "bound_cellular" means.
        final bind = await NativeBridge.bindCellular(true, waitMs: 5000);
        final pinned = bind.ok;
        await _trace('job ${job.id} bind ok=${bind.ok} cellular=${bind.cellular} err=${bind.error}');
        if (!pinned) {
          // Cannot leave the phone over the cellular network -> the check
          // would silently go through the user's VPN. Refuse honestly: the
          // job comes back 'failed' with the reason, the customer is
          // refunded, and the person sees what to fix on the home screen.
          final vpnOn = await NativeBridge.hasActiveVpn();
          _report(
            status: 'cellular_blocked',
            title: 'Связь Есть? — нет выхода через мобильную сеть',
            text: vpnOn
                ? 'VPN не разрешает обход — добавьте приложение в исключения VPN или выключите VPN'
                : 'Мобильная сеть недоступна для приложения (${bind.error ?? 'unknown'})',
          );
          _stopAlive();
          try {
            await _api.submitResult(job.id, success: false, result: {
              'untestable': true,
              'untestable_reason': 'cellular_unavailable',
              'error': bind.error,
              'vpn_active': vpnOn,
              'bound_cellular': false,
            });
            await _trace('job ${job.id} submitted as failed (cellular_unavailable)');
          } catch (e) {
            await _trace('job ${job.id} submit FAILED: $e');
          }
          FlutterForegroundTask.sendDataToMain({'type': 'job_done', 'jobId': job.id, 'success': false});
          await Future.delayed(const Duration(seconds: 10));
          continue;
        }
        // The operator's public IP, over this same cellular link (1.12.0):
        // the result may travel to the server another way (VPN / Wi-Fi).
        final ipAt = DateTime.now();
        final cellularIp = await CellularIp.measure(token: await SecureStore.getDeviceToken());
        DiagLog.service.log('[задание ${job.id}] IP мобильной сети узнан за ${_ms(ipAt)} через ${cellularIp['via'] ?? '—'}${cellularIp['error'] != null ? ' (не вышло: ${cellularIp['error']})' : ''}');
        await _trace('job ${job.id} cellular ip=${cellularIp['ip']} via=${cellularIp['via']} err=${cellularIp['error']}');
        final trafficBefore = await NativeBridge.uidTrafficBytes();

        // Is this operator actually under «белые списки» right now? Measured
        // at every job: the mode is switched on and off per region.
        Map<String, dynamic>? whitelist;
        if (job.whitelistAllowed.isNotEmpty && job.whitelistControl.isNotEmpty) {
          final wAt = DateTime.now();
          whitelist = await NativeBridge.whitelistProbe(job.whitelistAllowed, job.whitelistControl);
          DiagLog.service.log('[задание ${job.id}] белые списки за ${_ms(wAt)}: ${_wlSummary(whitelist)}');
          await _trace('job ${job.id} whitelist=${whitelist['verdict']} dns=${whitelist['dns']}');
        }
        if (whitelist?['verdict'] == 'no_dns') {
          // Resolver died since the last heartbeat: nothing measured now
          // would be about the resource. Hand it back as untestable (the
          // customer is refunded) and let the server know at once.
          _lastWhitelistVerdict = 'no_dns';
          _pendingWhitelistVerdict = 'no_dns';
          _lastPrivateDns = (whitelist!['dns'] as Map?)?['private_dns'] == true;
          _stopAlive();
          try {
            await _api.submitResult(job.id, success: false, result: {
              'untestable': true,
              'untestable_reason': 'dns_unavailable',
              'whitelist': whitelist,
              'bound_cellular': pinned,
            });
            await _trace('job ${job.id} submitted as failed (dns_unavailable)');
          } catch (e) {
            await _trace('job ${job.id} submit FAILED: $e');
          }
          FlutterForegroundTask.sendDataToMain({'type': 'job_done', 'jobId': job.id, 'success': false});
          continue;
        }

        // 'completed' = the phone performed the check (whatever it found);
        // 'failed' = it could not (unsupported key, no network). A dead key
        // is a valid, paid result -- the verdict lives inside `result`.
        Map<String, dynamic> result;
        bool success;
        final checkAt = DateTime.now();
        DiagLog.service.log('[задание ${job.id}] проверка начата (${job.resourceType})');
        if (job.resourceType == 'vpn_key') {
          final r = await checkVpnKey(job.resource, probeSites: job.probeSites, downloadUrl: job.downloadUrl, core: job.needsCore);
          success = !r.untestable;
          result = r.toJson();
        } else if (job.resourceType == 'subnet') {
          // All 256 addresses of a /24 over the cellular link -- "is the
          // whole range dark here, or only some hosts in it".
          final r = await checkSubnet(job.resource);
          success = !r.untestable;
          result = r.toJson();
          await _trace('job ${job.id} subnet=${r.cidr} alive=${r.alive}/${r.probed} verdict=${r.verdict}');
        } else if (job.ipFamily == 6 && !(await NativeBridge.cellularIpv6()).global) {
          // An IPv6 check on a phone whose mobile network lost IPv6 since
          // the last measurement (switched to 3G, another APN): every
          // connection would fail and read as a block. Not testable here --
          // the customer is refunded, and the server learns at once.
          _ipv6Ok = false;
          _ipv6At = DateTime.now();
          success = false;
          result = {'untestable': true, 'untestable_reason': 'ipv6_unavailable', 'ip_family': 6};
          await _trace('job ${job.id} ipv6 job, but no IPv6 on cellular');
        } else {
          final r = await checkIpOrDomain(job.resource, ipFamily: job.ipFamily);
          success = true;
          result = r.toJson();
          result['probe'] = await NativeBridge.hostProbe(job.resource, family: r.ipFamily);
          await _trace('job ${job.id} probe icmp=${result['probe']['icmp_ms']} tcp=${result['probe']['tcp_ms']} ipv${r.ipFamily ?? '-'}');
        }
        // Honest provenance for the server: whether this process was pinned
        // to the cellular network for the check (false = default route,
        // which may have been a VPN).
        result['bound_cellular'] = pinned;
        result['vpn_active'] = await NativeBridge.hasActiveVpn();
        if (whitelist != null) result['whitelist'] = whitelist;
        result['cellular_ip'] = cellularIp;
        final trafficAfter = await NativeBridge.uidTrafficBytes();
        result['traffic_bytes'] = trafficAfter >= trafficBefore ? trafficAfter - trafficBefore : 0;

        DiagLog.secret(result['resolved_ip']?.toString());
        DiagLog.secret(result['final_host']?.toString());
        DiagLog.secret(result['payload_url']?.toString());
        DiagLog.service.log('[задание ${job.id}] проверка за ${_ms(checkAt)}: успех=$success; ${_summary(job.resourceType, result)}; '
            'через мобильную=${result['bound_cellular']}, VPN на телефоне=${result['vpn_active']}, трафик ${result['traffic_bytes']} Б');
        await _trace('job ${job.id} checked: success=$success works=${result['works']} connected=${result['connected']} err=${result['error']} bound=${result['sockets_bound']}/${result['sockets_unbound']}');
        _stopAlive();
        final subAt = DateTime.now();
        await _api.submitResult(job.id, success: success, result: result);
        DiagLog.service.log('[задание ${job.id}] результат отправлен за ${_ms(subAt)} через «${ControlHttpClient.lastRoute ?? '?'}»; всё задание ${_ms(jobAt)} '
            '(${jsonEncode(result).length} Б JSON)');
        await _trace('job ${job.id} submitted');
        FlutterForegroundTask.sendDataToMain({'type': 'job_done', 'jobId': job.id, 'success': success});
        _consecutiveErrors = 0;
      } on ApiException catch (e) {
        if (e.code == 'signature_required') {
          // The server has another key for this phone (or none signed yet):
          // re-attest and go on with the next cycle.
          _stopAlive();
          await _reattest('сервер не принял подпись запроса');
          await Future.delayed(const Duration(seconds: 3));
          continue;
        }
        _stopAlive();
        DiagLog.service.log('ошибка API: HTTP ${e.statusCode} code=${e.code} ${e.message}');
        if (e.isCountryBlocked) {
          final cc = await NativeBridge.networkCountry();
          _report(
            status: 'country_blocked',
            title: 'Связь Есть? — остановлено',
            text: 'Мобильная сеть этой страны (${(cc.network ?? cc.sim ?? '—').toUpperCase()}) не поддерживается',
          );
          _running = false;
          await FlutterForegroundTask.stopService();
          break;
        }
        if (e.isAppOutdated) {
          // Stays off until the partner updates; the home screen explains
          // it and offers the download link.
          _report(status: 'app_outdated', title: 'Связь Есть? — нужно обновление', text: 'Эта версия приложения больше не поддерживается');
          _running = false;
          await FlutterForegroundTask.stopService();
          break;
        }
        if (e.isUnauthorized || e.isDeviceBanned || e.isAccountBanned) {
          // Token gone or device banned: nothing to retry. The UI shows
          // the reason the next time it asks for the profile.
          _report(status: 'signed_out', title: 'Связь Есть? — выход', text: 'Откройте приложение');
          _running = false;
          await FlutterForegroundTask.stopService();
          break;
        }
        _consecutiveErrors++;
        _report(status: 'error', title: 'Связь Есть? — сбой соединения', text: 'Повтор через несколько секунд...');
        await Future.delayed(Duration(seconds: _consecutiveErrors.clamp(1, 6) * 3));
      } catch (e, st) {
        _stopAlive();
        _consecutiveErrors++;
        final msg = '${e.runtimeType}: $e'.replaceAll(RegExp(r'\s+'), ' ');
        DiagLog.service.log('ошибка цикла #$_consecutiveErrors: $msg\n${st.toString().split('\n').take(25).join('\n')}');
        final delay = _consecutiveErrors.clamp(1, 6) * 3;
        // Plain network trouble (no internet, server unreachable, timeout)
        // gets a plain sentence; anything else shows class + message so a
        // phone without adb can still tell us what broke. Everything is
        // mirrored to the server for the admin.
        _report(
          status: 'error',
          title: _isNetworkError(e) ? 'Связь Есть? — нет соединения' : 'Связь Есть? — сбой',
          text: _isNetworkError(e)
              ? (_lastWhitelistVerdict == 'active'
                  ? 'Белые списки: нет связи с сервером — нужен Wi-Fi или прокси VPN-клиента (Настройки → Связь с сервером)'
                  : 'Сервер недоступен, повтор через $delay с')
              : (msg.length > 120 ? '${msg.substring(0, 120)}…' : msg),
        );
        if (_consecutiveErrors <= 3 || _consecutiveErrors % 10 == 0) {
          await _trace('loop error #$_consecutiveErrors: ${msg.length > 800 ? msg.substring(0, 800) : msg}');
        }
        // Back off a little harder after repeated failures so a broken
        // network doesn't turn into a tight retry loop draining battery.
        await Future.delayed(Duration(seconds: _consecutiveErrors.clamp(1, 6) * 3));
      }
    }
  }

  bool _isNetworkError(Object e) {
    if (e is SocketException || e is TimeoutException || e is HandshakeException || e is HttpException) return true;
    final t = e.toString();
    return t.contains('ClientException') || t.contains('SocketException') || t.contains('Connection') ||
        t.contains('network is unreachable') || t.contains('Failed host lookup') || t.contains('timed out');
  }

  // Last line written per kind (diagnostics log): repeated states are
  // written once, not every 25 s cycle.
  final Map<String, String> _diagLast = {};
  void _diagChanged(String kind, String line) {
    if (_diagLast[kind] == line) return;
    _diagLast[kind] = line;
    DiagLog.service.log(line);
  }

  void _report({required String status, required String title, required String text}) {
    _diagChanged('status', 'статус: $status — $text');
    FlutterForegroundTask.updateService(notificationTitle: title, notificationText: text);
    FlutterForegroundTask.sendDataToMain({'type': 'status', 'status': status, 'text': text});
  }

  /// Notification text while checking: the kind of check only. The resource
  /// itself (key / IP / domain) is the customer's data and is never shown.
  String _describeResource(String type) => type == 'vpn_key' ? 'Проверка VPN-ключа' : 'Проверка домена/IP';
}
