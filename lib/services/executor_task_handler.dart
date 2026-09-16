import 'dart:async';
import 'dart:io';

import 'package:flutter_foreground_task/flutter_foreground_task.dart';

import 'api_client.dart';
import 'checkers/ip_checker.dart';
import 'checkers/vpn_checker.dart';
import 'native_bridge.dart';

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
  bool _running = true;
  int _consecutiveErrors = 0;
  String? _hwid;
  // Lease keepalive for the job being checked (POST /jobs/:id/alive every 5 s).
  Timer? _aliveTimer;

  void _startAlive(int jobId) {
    _stopAlive();
    unawaited(_api.jobAlive(jobId).catchError((_) {}));
    _aliveTimer = Timer.periodic(const Duration(seconds: 5), (_) {
      unawaited(_api.jobAlive(jobId).catchError((_) {}));
    });
  }

  void _stopAlive() {
    _aliveTimer?.cancel();
    _aliveTimer = null;
  }
  // When "mobile data off" was last reported to the server (null = online
  // or not reported yet), see _loop.
  DateTime? _offlineReportedAt;
  // Per-isolate id in every trace line: two service instances alive at once
  // would show up as two different ids interleaving.
  final String _instance = DateTime.now().millisecondsSinceEpoch.toRadixString(36);

  /// Server-side breadcrumb (executor_devices.last_error) -- the only log a
  /// phone without adb has. Best effort, never throws.
  Future<void> _trace(String msg) async {
    try {
      await _api.reportError('[$_instance] $msg');
    } catch (_) {}
  }

  @override
  Future<void> onStart(DateTime timestamp, TaskStarter starter) async {
    if (await NativeBridge.isRooted()) {
      _running = false;
      await FlutterForegroundTask.stopService();
      return;
    }
    _running = true;
    await ApiClient.loadAppVersion();
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
        await _api.heartbeat(
          mobileDataEnabled: true, isOnline: true, hwid: _hwid,
          operatorName: await NativeBridge.operatorName(),
          country: cc.network, simCountry: cc.sim,
        );
        _report(status: 'online', title: 'Связь Есть? — онлайн', text: 'Ожидание заданий...');

        final job = await _api.pollJob();
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
        final trafficBefore = await NativeBridge.uidTrafficBytes();

        // Is this operator actually under «белые списки» right now? Measured
        // at every job: the mode is switched on and off per region.
        Map<String, dynamic>? whitelist;
        if (job.whitelistAllowed.isNotEmpty && job.whitelistControl.isNotEmpty) {
          whitelist = await NativeBridge.whitelistProbe(job.whitelistAllowed, job.whitelistControl);
          await _trace('job ${job.id} whitelist=${whitelist['verdict']}');
        }

        // 'completed' = the phone performed the check (whatever it found);
        // 'failed' = it could not (unsupported key, no network). A dead key
        // is a valid, paid result -- the verdict lives inside `result`.
        Map<String, dynamic> result;
        bool success;
        if (job.resourceType == 'vpn_key') {
          final r = await checkVpnKey(job.resource, probeSites: job.probeSites, downloadUrl: job.downloadUrl);
          success = !r.untestable;
          result = r.toJson();
        } else {
          final r = await checkIpOrDomain(job.resource);
          success = true;
          result = r.toJson();
          result['probe'] = await NativeBridge.hostProbe(job.resource);
          await _trace('job ${job.id} probe icmp=${result['probe']['icmp_ms']} tcp=${result['probe']['tcp_ms']}');
        }
        // Honest provenance for the server: whether this process was pinned
        // to the cellular network for the check (false = default route,
        // which may have been a VPN).
        result['bound_cellular'] = pinned;
        result['vpn_active'] = await NativeBridge.hasActiveVpn();
        if (whitelist != null) result['whitelist'] = whitelist;
        final trafficAfter = await NativeBridge.uidTrafficBytes();
        result['traffic_bytes'] = trafficAfter >= trafficBefore ? trafficAfter - trafficBefore : 0;

        await _trace('job ${job.id} checked: success=$success works=${result['works']} connected=${result['connected']} err=${result['error']} bound=${result['sockets_bound']}/${result['sockets_unbound']}');
        _stopAlive();
        await _api.submitResult(job.id, success: success, result: result);
        await _trace('job ${job.id} submitted');
        FlutterForegroundTask.sendDataToMain({'type': 'job_done', 'jobId': job.id, 'success': success});
        _consecutiveErrors = 0;
      } on ApiException catch (e) {
        _stopAlive();
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
      } catch (e) {
        _stopAlive();
        _consecutiveErrors++;
        final msg = '${e.runtimeType}: $e'.replaceAll(RegExp(r'\s+'), ' ');
        final delay = _consecutiveErrors.clamp(1, 6) * 3;
        // Plain network trouble (no internet, server unreachable, timeout)
        // gets a plain sentence; anything else shows class + message so a
        // phone without adb can still tell us what broke. Everything is
        // mirrored to the server for the admin.
        _report(
          status: 'error',
          title: _isNetworkError(e) ? 'Связь Есть? — нет соединения' : 'Связь Есть? — сбой',
          text: _isNetworkError(e)
              ? 'Сервер недоступен, повтор через $delay с'
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

  void _report({required String status, required String title, required String text}) {
    FlutterForegroundTask.updateService(notificationTitle: title, notificationText: text);
    FlutterForegroundTask.sendDataToMain({'type': 'status', 'status': status, 'text': text});
  }

  /// Notification text while checking: the kind of check only. The resource
  /// itself (key / IP / domain) is the customer's data and is never shown.
  String _describeResource(String type) => type == 'vpn_key' ? 'Проверка VPN-ключа' : 'Проверка домена/IP';
}
