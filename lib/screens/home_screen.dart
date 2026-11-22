import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:url_launcher/url_launcher.dart';

import '../models/profile.dart';
import '../services/api_client.dart';
import '../services/app_updater.dart';
import '../services/executor_task_handler.dart';
import '../services/diag_log.dart';
import '../services/gps_check.dart';
import '../services/native_bridge.dart';
import '../services/session_guard.dart';
import '../theme.dart';
import '../widgets/diag_stop.dart';
import '../widgets/level_card.dart';
import '../widgets/level_up.dart';
import '../widgets/grouped_list.dart';
import 'vpn_bypass_guide_screen.dart';

class HomeTab extends StatefulWidget {
  const HomeTab({super.key});

  @override
  State<HomeTab> createState() => _HomeTabState();
}

class _HomeTabState extends State<HomeTab> with WidgetsBindingObserver, AutomaticKeepAliveClientMixin {
  final _api = ApiClient();
  bool _isRunning = false;
  bool _hasMobileData = false;
  bool? _mobileSetting; // the phone's mobile-data switch, see NetworkState
  bool _hasVpn = false;
  String _statusText = 'Ожидание запуска...';
  String _serviceStatus = ''; // last status code from the service (online, no_mobile_data, ...)
  ExecutorProfile? _profile;
  Timer? _refreshTimer;
  StreamSubscription<NetworkState>? _netSub;
  bool _notifOk = true;
  bool _batteryOk = true;
  bool _vpnLockdown = false;
  String _lockdownDetail = '';
  bool _countryBlocked = false;
  String? _blockedCountry;
  VoidCallback? _permSheetRefresh; // set while the permissions sheet is open
  bool _permSheetOpen = false;
  // A toggle in flight: the switch already shows the target state, and a
  // concurrent _refresh must not flip it back until the service reports in.
  bool _busy = false;
  // Network report (ApiClient.reportNetwork): sent once per app session
  // over cellular; retried when mobile data comes up if it could not be.
  bool _cellularReported = false;
  DateTime? _lastReportAt;
  // In-app update (1.19.0): a newer version from the server, and what the
  // «Обновить» button is doing right now (null = idle).
  AppUpdate? _update;
  String? _updateBusy;
  String? _updateError;
  DateTime? _updateCheckedAt;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    FlutterForegroundTask.addTaskDataCallback(_onTaskData);
    // With Wi-Fi connected Android tends to drop the cellular link entirely
    // while nothing asks for it; keep it requested while this screen is
    // open so the "Мобильный интернет" row reflects the setting, not the
    // system's power saving. (The service makes its own request while
    // running -- separate engine, separate callback.)
    NativeBridge.cellularRequest(true);
    _refresh();
    unawaited(_reportNetwork());
    unawaited(_checkUpdate());
    _refreshTimer = Timer.periodic(const Duration(seconds: 15), (_) => _refresh());
    // Network rows update the moment mobile data / a VPN goes up or down.
    _netSub = NativeBridge.networkChanges().listen((st) {
      if (!mounted) return;
      setState(() {
        _hasMobileData = st.mobile;
        _hasVpn = st.vpn;
        _mobileSetting = st.mobileSetting;
      });
      _checkLockdown();
      if (st.mobile) unawaited(_reportNetwork());
    }, onError: (_) {});
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    FlutterForegroundTask.removeTaskDataCallback(_onTaskData);
    _refreshTimer?.cancel();
    _netSub?.cancel();
    NativeBridge.cellularRequest(false);
    super.dispose();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    // Back from the system settings page -- see whether the user granted
    // (or revoked) something while we were away.
    if (state == AppLifecycleState.resumed) {
      _refresh().then((_) => _permSheetRefresh?.call());
      unawaited(_checkUpdate());
    }
  }

  void _onTaskData(Object data) {
    if (data is Map) {
      if (data['type'] == 'status' && mounted) {
        final status = data['status'] as String? ?? '';
        setState(() {
          _statusText = data['text'] as String? ?? _statusText;
          _serviceStatus = status;
          // The service stops itself on these (min version raised in the
          // admin panel, country not allowed, signed out): drop «Работает»
          // at once instead of waiting for the partner to toggle the switch.
          if (_stopStatuses.contains(status)) _isRunning = false;
        });
        if (_stopStatuses.contains(status)) {
          // Re-read the profile/state once the service is really down, so
          // the reason («Обновитесь», country, sign-out) shows by itself.
          Future.delayed(const Duration(milliseconds: 1500), () {
            if (mounted) _refresh();
          });
        }
      } else if (data['type'] == 'job_done') {
        _loadProfile();
      }
    }
  }

  static const _stopStatuses = {'app_outdated', 'country_blocked', 'signed_out'};

  Future<void> _refresh() async {
    // Cheap, local facts first -- the screen updates at once. The slow parts
    // (cellular probe up to 4 s behind a VPN, profile over the network) run
    // after that and never hold the switch.
    final running = await FlutterForegroundTask.isRunningService;
    final net = await NativeBridge.networkState();
    await _checkPermissions();
    if (!mounted) return;
    setState(() {
      if (!_busy) _isRunning = running;
      _hasMobileData = net.mobile;
      _hasVpn = net.vpn;
      _mobileSetting = net.mobileSetting;
    });
    await _checkLockdown();
    await _loadProfile();
    await _checkCountry();
    // Permissions are mandatory: if one was revoked while the service was
    // running, the service goes down with it and the reason is shown. Same
    // for a VPN that forbids bypass, and for a mobile network from a
    // country that is not allowed.
    _diagScreen('экран: служба=${running ? 'работает' : 'выключена'} статус=$_serviceStatus; '
        'мобильная=$_hasMobileData переключатель=$_mobileSetting VPN=$_hasVpn блок VPN=$_vpnLockdown; '
        'разрешения: уведомления=$_notifOk батарея=$_batteryOk; страна заблокирована=$_countryBlocked; '
        'модерация=${_profile?.moderationStatus} устарела=${_profile?.appOutdated}');
    if (running && !_busy && (!_permsOk || _vpnLockdown || _countryBlocked || _switchLocked)) {
      DiagLog.app.log('экран останавливает службу: разрешения/VPN/страна/блокировка');
      await FlutterForegroundTask.stopService();
      if (mounted) setState(() => _isRunning = false);
    }
  }

  bool get _permsOk => _notifOk && _batteryOk;

  // Diagnostics log (only while the partner allows it): the screen state,
  // written when it changes rather than every 15 s refresh.
  String? _diagLastScreen;
  void _diagScreen(String line) {
    if (line == _diagLastScreen) return;
    _diagLastScreen = line;
    DiagLog.app.log(line);
  }

  // Support's GPS check: «Разрешить» on the notification opened the app
  // (launch action) -> run it; pending -> ask here; accepted but not sent
  // (the partner left midway) -> run it again.
  bool _gpsBusy = false;
  Future<void> _handleGps(GpsInfo? info) async {
    if (_gpsBusy || !mounted) return;
    _gpsBusy = true;
    try {
      final launch = await GpsBridge.takeLaunchAction();
      final g = await GpsConsent.sync(info);
      if (g == null || !mounted) return;
      bool? go;
      if (g.status == 'accepted' || (launch != null && launch.id == g.id && launch.action == 'accept')) {
        go = true;
      } else if (g.status == 'pending') {
        go = await askGps(context, g.reason);
      }
      if (go == null || !mounted) return;
      if (!go) {
        await GpsConsent.decline(g);
        if (mounted) ScaffoldMessenger.of(context).showSnackBar(const SnackBar(content: Text('Проверка местоположения отклонена')));
        return;
      }
      final msg = await GpsConsent.run(context, g);
      if (mounted && msg.isNotEmpty) ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(msg)));
    } catch (_) {
    } finally {
      _gpsBusy = false;
    }
  }

  // Level up / new achievement: shown once, against what this phone showed
  // last (flutter_foreground_task storage). First run of this version only
  // remembers the current state -- no fireworks for levels earned before.
  bool _celebrating = false;
  Future<void> _celebrate(LevelInfo? info) async {
    if (info == null || !mounted || _celebrating) return;
    _celebrating = true;
    try {
      final seen = await FlutterForegroundTask.getData<int>(key: 'seen_level');
      final got = info.achievements.where((a) => a.earnedAt != null).map((a) => a.code).toList()..sort();
      final seenAch = await FlutterForegroundTask.getData<String>(key: 'seen_achievements');
      await FlutterForegroundTask.saveData(key: 'seen_level', value: info.level);
      await FlutterForegroundTask.saveData(key: 'seen_achievements', value: got.join(','));
      if (!mounted) return;
      if (seen != null && info.level > seen) {
        await showLevelUp(context, level: info.level, share: info.share);
      }
      if (seenAch != null && mounted) {
        final before = seenAch.split(',').toSet();
        for (final a in info.achievements.where((a) => a.earnedAt != null && !before.contains(a.code))) {
          ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text('Новое достижение: «${a.title}»')));
        }
      }
    } catch (_) {
    } finally {
      _celebrating = false;
    }
  }

  // «Разрешить сбор логов?» in the app itself: can't be dismissed without
  // an answer. One dialog per request.
  int? _diagAsking;
  Future<void> _askDiag(DiagInfo d) async {
    if (!mounted || _diagAsking == d.id) return;
    _diagAsking = d.id;
    final accept = await showDialog<bool>(
      context: context,
      barrierDismissible: false,
      builder: (ctx) => PopScope(
        canPop: false,
        child: AlertDialog(
          title: const Text('Разрешите диагностический сбор логов'),
          content: Text(
            'Чтобы разобраться, почему приложение «Связь Есть?» работает не так, как задумано, '
            'нам нужны технические логи с этого устройства.\n\n'
            '${d.reason.isNotEmpty ? 'Причина запроса: ${d.reason}\n\n' : ''}'
            'В логах — подробная работа приложения: мобильная сеть, Wi-Fi и VPN (подключения и отключения), '
            'батарея и энергосбережение, разрешения, связь с сервером, шаги заданий, ошибки и сбои приложения. '
            'Ваши личные данные, содержимое других приложений, ваше местоположение и проверяемые адреса не собираются.\n\n'
            'Сбор отключит поддержка, когда закончит диагностику. Остановить его самому можно в любой момент — '
            'кнопкой на главном экране или в Настройках.',
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
    if (accept == null) return;
    try {
      final now = await DiagConsent.decide(d, accept, DiagLog.app);
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(SnackBar(
        content: Text(now?.status == 'accepted'
            ? 'Сбор логов разрешён. Остановить — в Настройках'
            : 'Сбор логов отклонён'),
      ));
    } catch (_) {
      // Not delivered: ask again on the next refresh.
      _diagAsking = null;
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(content: Text('Не удалось отправить ответ, попробуем ещё раз')),
        );
      }
    }
  }

  /// Partner application not approved yet (or rejected) in the Telegram
  /// bot: signed in, but the work switch stays locked.
  bool get _moderationLocked => _profile != null && !_profile!.isApproved;

  /// This app is below the minimum supported version (admin panel).
  bool get _updateLocked => _profile?.appOutdated == true;

  /// Anything that keeps the work switch greyed out.
  bool get _switchLocked => _updateLocked || _moderationLocked;

  Future<void> _openUpdate() async {
    // In-app update when the server has the file; the download link otherwise.
    if (_update != null) return _runUpdate();
    final url = _profile?.updateUrl;
    if (url == null || url.isEmpty) return;
    await launchUrl(Uri.parse(url), mode: LaunchMode.externalApplication);
  }

  /// Asks the server for a newer version: on start, on return to the app,
  /// at most every 30 minutes.
  Future<void> _checkUpdate({bool force = false}) async {
    final now = DateTime.now();
    if (!force && _updateCheckedAt != null && now.difference(_updateCheckedAt!) < const Duration(minutes: 30)) return;
    _updateCheckedAt = now;
    try {
      final u = await AppUpdater.check();
      if (mounted) setState(() => _update = u);
    } catch (_) {}
  }

  Future<void> _runUpdate() async {
    final u = _update;
    if (u == null || _updateBusy != null) return;
    setState(() {
      _updateBusy = 'Скачиваю обновление (${u.sizeLabel})…';
      _updateError = null;
    });
    try {
      final path = await AppUpdater.download(u);
      if (!mounted) return;
      setState(() => _updateBusy = 'Открываю установку…');
      final started = await AppUpdater.install(path);
      if (!mounted) return;
      if (!started) {
        setState(() => _updateError =
            'Разрешите установку обновлений: в открывшихся настройках включите «Разрешить из этого источника», '
            'вернитесь и нажмите «Обновить» ещё раз.');
      }
    } catch (e) {
      if (mounted) setState(() => _updateError = e.toString().replaceFirst('Exception: ', ''));
    } finally {
      if (mounted) setState(() => _updateBusy = null);
    }
  }

  Widget _updateCard() {
    final u = _update!;
    return Container(
      margin: const EdgeInsets.fromLTRB(0, 8, 0, 12),
      padding: const EdgeInsets.fromLTRB(16, 14, 16, 14),
      decoration: BoxDecoration(color: AppColors.group, borderRadius: BorderRadius.circular(12)),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('Доступно обновление ${u.version}',
              style: const TextStyle(fontSize: 16, fontWeight: FontWeight.w600)),
          const SizedBox(height: 4),
          Text(
            _updateBusy ?? 'Установленная версия ${ApiClient.appVersion ?? '—'}. Обновление скачается в приложении, '
                'Android попросит подтвердить установку.',
            style: const TextStyle(fontSize: 13, height: 1.4, color: AppColors.textSecondary),
          ),
          if (_updateError != null) ...[
            const SizedBox(height: 6),
            Text(_updateError!, style: const TextStyle(fontSize: 13, height: 1.4, color: AppColors.warning)),
          ],
          const SizedBox(height: 10),
          SizedBox(
            width: double.infinity,
            child: ElevatedButton(
              onPressed: _updateBusy == null ? _runUpdate : null,
              child: _updateBusy == null
                  ? const Text('Обновить')
                  : const SizedBox(width: 18, height: 18, child: CircularProgressIndicator(strokeWidth: 2)),
            ),
          ),
        ],
      ),
    );
  }

  Future<void> _reportNetwork() async {
    if (_cellularReported) return;
    final now = DateTime.now();
    // The server keeps one report a minute per device.
    if (_lastReportAt != null && now.difference(_lastReportAt!) < const Duration(seconds: 70)) return;
    _lastReportAt = now;
    try {
      _cellularReported = await _api.reportNetwork();
    } catch (_) {}
  }

  void _explainModeration() {
    final p = _profile;
    if (p == null || !mounted) return;
    ScaffoldMessenger.of(context).hideCurrentSnackBar();
    ScaffoldMessenger.of(context).showSnackBar(SnackBar(
      content: Text(p.appOutdated
          ? 'Эта версия приложения больше не поддерживается — установите обновление'
          : p.moderationStatus == 'rejected'
          ? 'Заявка партнёра отклонена — подайте её заново в Telegram-боте «Связь Есть?»'
          : 'Ваш аккаунт на модерации — включить приём заданий можно после одобрения заявки'),
    ));
  }

  /// Country allowlist comes from the server (profile); the country itself
  /// from the phone's mobile network, so the verdict is instant even before
  /// the first heartbeat. Empty allowlist = no restriction.
  Future<void> _checkCountry() async {
    final allowed = _profile?.allowedCountries ?? const [];
    if (allowed.isEmpty) {
      if (_countryBlocked && mounted) setState(() => _countryBlocked = false);
      return;
    }
    final cc = await NativeBridge.networkCountry();
    final code = cc.network ?? cc.sim;
    final blocked = code == null || !allowed.contains(code);
    if (!mounted) return;
    if (blocked != _countryBlocked || code != _blockedCountry) {
      setState(() {
        _countryBlocked = blocked;
        _blockedCountry = code;
      });
    }
  }

  Future<void> _checkLockdown() async {
    final r = _hasVpn ? await NativeBridge.vpnLockdown() : (lockdown: false, vpn: false, detail: '');
    if (!mounted) return;
    if (r.lockdown != _vpnLockdown || r.detail != _lockdownDetail) {
      setState(() {
        _vpnLockdown = r.lockdown;
        _lockdownDetail = r.detail;
      });
      // Diagnostics for the admin: what the cellular probe saw.
      if (_hasVpn) _api.reportError('vpnLockdown=${r.lockdown} ${r.detail}').catchError((_) {});
    }
  }

  Future<void> _checkPermissions() async {
    final perms = await NativeBridge.permissions();
    if (!mounted) return;
    setState(() {
      _notifOk = perms.notifications;
      _batteryOk = perms.battery;
    });
  }

  Future<void> _loadProfile() async {
    try {
      final p = await _api.profile();
      if (mounted) setState(() => _profile = p);
      // Support's log request: deliver a notification answer, keep the
      // notification in sync, ask here too while it is pending.
      final diag = await DiagConsent.sync(p.diag, DiagLog.app);
      if (diag != null && diag.status == 'pending') unawaited(_askDiag(diag));
      unawaited(_celebrate(p.level));
      unawaited(_handleGps(p.gps));
    } catch (e) {
      DiagLog.app.log('профиль не загрузился: $e');
      // Signed out elsewhere / banned -> leave the shell; anything else is
      // offline or not yet reachable -- keep showing the last known values.
      if (mounted) await handleAuthFailure(context, e);
    }
  }

  Future<void> _openNotificationSettings() async {
    // The runtime prompt shows only once; after a refusal the app must send
    // the user to the system page instead.
    final p = await FlutterForegroundTask.requestNotificationPermission();
    if (p != NotificationPermission.granted) await NativeBridge.openNotificationSettings();
  }

  Future<void> _openBatterySettings() async {
    final ok = await FlutterForegroundTask.requestIgnoreBatteryOptimization();
    if (!ok) await FlutterForegroundTask.openIgnoreBatteryOptimizationSettings();
  }

  Future<void> _showPermissionsSheet() async {
    if (!mounted || _permSheetOpen) return;
    _permSheetOpen = true;
    await showModalBottomSheet<void>(
      context: context,
      backgroundColor: AppColors.group,
      isScrollControlled: true,
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(16))),
      builder: (ctx) => StatefulBuilder(
        builder: (ctx, setSheet) {
          _permSheetRefresh = () {
            if (!ctx.mounted) return;
            setSheet(() {});
            if (_permsOk) {
              Navigator.of(ctx).pop();
              if (mounted) setState(() { _isRunning = true; _statusText = 'Запуск...'; });
              _startService();
            }
          };
          Widget row(String title, String subtitle, bool ok, Future<void> Function() onGrant) => Padding(
                padding: const EdgeInsets.symmetric(vertical: 10),
                child: Row(
                  children: [
                    Expanded(
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Text(title, style: const TextStyle(fontSize: 16)),
                          const SizedBox(height: 3),
                          Text(subtitle, style: const TextStyle(fontSize: 13, height: 1.35, color: AppColors.textSecondary)),
                        ],
                      ),
                    ),
                    const SizedBox(width: 12),
                    ok
                        ? const Text('Разрешено', style: TextStyle(fontSize: 15, color: AppColors.success))
                        : TextButton(
                            onPressed: () async {
                              await onGrant();
                              await _checkPermissions();
                              _permSheetRefresh?.call();
                            },
                            child: const Text('Разрешить'),
                          ),
                  ],
                ),
              );
          return SafeArea(
            child: Padding(
              padding: const EdgeInsets.fromLTRB(20, 16, 20, 12),
              child: Column(
                mainAxisSize: MainAxisSize.min,
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  const Text('Нужны два разрешения', style: TextStyle(fontSize: 20, fontWeight: FontWeight.w600)),
                  const SizedBox(height: 4),
                  const Text(
                    'Без них приложение не сможет ждать задания в фоне.',
                    style: TextStyle(fontSize: 14, color: AppColors.textSecondary),
                  ),
                  const SizedBox(height: 12),
                  row('Уведомления', 'Постоянное уведомление держит службу живой и показывает статус.', _notifOk,
                      _openNotificationSettings),
                  const Divider(height: 1, color: AppColors.separator),
                  row('Работа в фоне', 'Исключение из оптимизации батареи, иначе телефон заморозит приложение во сне.',
                      _batteryOk, _openBatterySettings),
                  const SizedBox(height: 12),
                ],
              ),
            ),
          );
        },
      ),
    );
    _permSheetRefresh = null;
    _permSheetOpen = false;
  }

  Future<void> _startService() async {
    final result = await FlutterForegroundTask.isRunningService
        ? await FlutterForegroundTask.restartService()
        : await FlutterForegroundTask.startService(
            serviceId: 256,
            notificationTitle: 'Связь Есть?',
            notificationText: 'Запуск...',
            callback: executorTaskHandlerCallback,
          );
    if (result is ServiceRequestFailure) {
      DiagLog.app.log('служба не запустилась: ${result.error}');
      if (mounted) {
        setState(() => _isRunning = false);
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text('Не удалось запустить: ${result.error}')),
        );
      }
      return;
    }
    final running = await FlutterForegroundTask.isRunningService;
    if (mounted) setState(() => _isRunning = running);
  }

  Future<void> _toggle(bool value) async {
    // One toggle at a time: a second tap while the first is still checking
    // permissions (or has the permissions sheet open) is ignored, so the
    // sheet cannot stack up.
    if (_busy) return;
    _busy = true;
    try {
      DiagLog.app.log('переключатель «Работать»: ${value ? 'включить' : 'выключить'}');
      if (!value) {
        // Flip first, stop second -- the word changes the moment it is tapped.
        if (mounted) {
          setState(() {
            _isRunning = false;
            _serviceStatus = '';
          });
        }
        await FlutterForegroundTask.stopService();
        return;
      }
      if (_profile == null) await _loadProfile();
      if (_switchLocked) {
        _explainModeration();
        return;
      }
      // Optimistic: show «Работает / Запуск...» right away; any failed
      // check below flips it back and explains why in the subtitle.
      if (mounted) {
        setState(() {
          _isRunning = true;
          _statusText = 'Запуск...';
          _serviceStatus = '';
        });
      }
      await _checkCountry();
      if (_countryBlocked) {
        if (mounted) setState(() => _isRunning = false);
        return;
      }
      await _checkLockdown();
      if (_vpnLockdown) {
        if (mounted) setState(() => _isRunning = false);
        return;
      }
      await _checkPermissions();
      if (!_permsOk) {
        // Ask in one place: a sheet with both permissions; it starts the
        // service itself once both are granted.
        if (mounted) setState(() => _isRunning = false);
        await _showPermissionsSheet();
        return;
      }
      await _startService();
    } finally {
      _busy = false;
      // Background facts (profile, lockdown) catch up without touching the
      // switch, which the service state has just confirmed.
      unawaited(_refresh());
    }
  }

  @override
  bool get wantKeepAlive => true;

  @override
  Widget build(BuildContext context) {
    super.build(context);
    final mobileOn = _hasMobileData;
    return RefreshIndicator(
      onRefresh: _refresh,
      child: ListView(
        padding: const EdgeInsets.fromLTRB(16, 4, 16, 16),
        children: [
          if (_update != null) _updateCard(),
          // The whole status row is the switch: tapping the word toggles
          // too, so nobody has to aim for the small control on the right.
          InkWell(
            onTap: () => _switchLocked && !_isRunning ? _explainModeration() : _toggle(!_isRunning),
            borderRadius: BorderRadius.circular(12),
            splashColor: Colors.transparent,
            highlightColor: Colors.transparent,
            splashFactory: NoSplash.splashFactory,
            child: Padding(
            padding: const EdgeInsets.fromLTRB(16, 12, 16, 24),
            child: Row(
              children: [
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      // Crossfade only, in a fixed-height slot: no sliding,
                      // nothing else on the screen moves when the word changes.
                      SizedBox(
                        height: 36,
                        child: AnimatedSwitcher(
                          duration: const Duration(milliseconds: 400),
                          switchInCurve: Curves.easeInOut,
                          switchOutCurve: Curves.easeInOut,
                          transitionBuilder: (child, anim) => FadeTransition(opacity: anim, child: child),
                          layoutBuilder: (current, previous) => Stack(
                            alignment: Alignment.centerLeft,
                            children: [...previous, ?current],
                          ),
                          child: Text(
                            _headline(),
                            key: ValueKey(_headline()),
                            style: TextStyle(
                              fontSize: 28,
                              fontWeight: FontWeight.w600,
                              letterSpacing: -0.3,
                              height: 1.1,
                              color: _headline() == 'Работает'
                                  ? AppColors.success
                                  : (_headline() == 'На модерации' || _headline() == 'Обновитесь' || _headline() == 'Нет БС'
                                      ? AppColors.warning
                                      : AppColors.danger),
                            ),
                          ),
                        ),
                      ),
                      AnimatedSwitcher(
                        duration: const Duration(milliseconds: 300),
                        switchInCurve: Curves.easeInOut,
                        switchOutCurve: Curves.easeInOut,
                        transitionBuilder: (child, anim) => FadeTransition(opacity: anim, child: child),
                        layoutBuilder: (current, previous) => Stack(
                          alignment: Alignment.centerLeft,
                          children: [...previous, ?current],
                        ),
                        child: Text(
                          _subtitle(),
                          key: ValueKey('$_isRunning|$_vpnLockdown|$_countryBlocked|$_switchLocked|$_statusText'),
                          style: const TextStyle(fontSize: 15, color: AppColors.textSecondary),
                        ),
                      ),
                    ],
                  ),
                ),
                const SizedBox(width: 16),
                // Locked (greyed out) while the partner application is on moderation.
                Switch(value: _isRunning, onChanged: _switchLocked && !_isRunning ? null : _toggle),
              ],
            ),
          ),
          ),
          if (_updateLocked && !_isRunning) ...[
            Padding(
              padding: const EdgeInsets.fromLTRB(16, 0, 16, 0),
              child: Text(
                'Установленная версия приложения больше не поддерживается'
                '${(_profile!.minAppVersion ?? '').isNotEmpty ? ' (нужна ${_profile!.minAppVersion} или новее)' : ''}. '
                'Пока вы не обновите приложение, задания назначаться не будут.',
                style: const TextStyle(fontSize: 13, height: 1.45, color: AppColors.textSecondary),
              ),
            ),
            if ((_profile!.updateUrl ?? '').isNotEmpty && _update == null) ...[
              const SizedBox(height: 12),
              Padding(
                padding: const EdgeInsets.symmetric(horizontal: 16),
                child: SizedBox(
                  width: double.infinity,
                  child: ElevatedButton(onPressed: _openUpdate, child: const Text('Скачать обновление')),
                ),
              ),
            ],
            const SizedBox(height: 20),
          ] else if (_moderationLocked && !_isRunning) ...[
            Padding(
              padding: const EdgeInsets.fromLTRB(16, 0, 16, 0),
              child: Text(
                _profile!.moderationStatus == 'rejected'
                    ? 'Заявка партнёра отклонена${(_profile!.moderationReason ?? '').isNotEmpty ? ': ${_profile!.moderationReason}' : ''}. '
                        'Исправьте данные и подайте заявку заново в Telegram-боте «Связь Есть?».'
                    : 'Ваш аккаунт на модерации: мы проверяем заявку партнёра (страна, регион и оператор). '
                        'Пока её не одобрят, приём заданий включить нельзя. Результат придёт в Telegram-бот «Связь Есть?».',
                style: const TextStyle(fontSize: 13, height: 1.45, color: AppColors.textSecondary),
              ),
            ),
            const SizedBox(height: 20),
          ],
          if (_vpnLockdown) ...[
            const Padding(
              padding: EdgeInsets.fromLTRB(16, 0, 16, 0),
              child: Text(
                'Ваш VPN-клиент не разрешает приложениям выходить в сеть мимо туннеля, а проверки должны идти через '
                'мобильную сеть напрямую. Добавьте «Связь Есть?» в исключения VPN (раздельное туннелирование, «обход для '
                'приложений», split tunneling) или на время выключите VPN.',
                style: TextStyle(fontSize: 13, height: 1.45, color: AppColors.textSecondary),
              ),
            ),
            const SizedBox(height: 20),
            const VpnBypassClientsSection(header: 'Инструкция для вашего VPN'),
            const SizedBox(height: 20),
          ],
          ..._partnerSection(),
          GroupedSection(
            header: 'Сеть',
            children: [
              // Three states: the network is up; the switch is on but the
              // phone has not brought the link up yet (Wi-Fi); switched off.
              GroupedRow(
                label: 'Мобильный интернет',
                value: mobileOn
                    ? 'Включён'
                    : (_mobileSetting == true ? 'Включён, нет сети' : 'Выключен'),
                valueColor: mobileOn
                    ? AppColors.success
                    : (_mobileSetting == true ? AppColors.warning : AppColors.danger),
              ),
              GroupedRow(
                label: 'Сторонний VPN',
                value: _hasVpn ? 'Обнаружен' : 'Не обнаружен',
                valueColor: _hasVpn ? AppColors.warning : null,
              ),
            ],
          ),
          if (_profile?.level != null) ...[
            const SizedBox(height: 20),
            LevelCard(info: _profile!.level!),
          ],
          // Logs for support are being collected: say so where it is seen,
          // with a way to stop (warned first, see widgets/diag_stop.dart).
          ValueListenableBuilder<int?>(
            valueListenable: DiagLog.app.activeId,
            builder: (context, id, _) => id == null
                ? const SizedBox.shrink()
                : Padding(
                    padding: const EdgeInsets.only(top: 20),
                    child: GroupedSection(
                      header: 'Диагностика',
                      children: [
                        const GroupedRow(label: 'Идёт сбор логов для поддержки', value: 'включён', valueColor: AppColors.warning),
                        GroupedRow(
                          label: 'Остановить сбор логов',
                          labelColor: AppColors.danger,
                          onTap: () => confirmStopDiag(context, id),
                        ),
                      ],
                    ),
                  ),
          ),
          const SizedBox(height: 20),
          GroupedSection(
            header: 'Баланс',
            children: [
              GroupedRow(
                label: 'Доступно',
                value: _money(_profile?.availableBalance ?? _profile?.balance),
                valueColor: AppColors.textPrimary,
              ),
              // Hold on withdrawals (admin panel): rewards of the last N
              // days are on the balance but can't be withdrawn yet.
              if ((_profile?.heldBalance ?? 0) > 0)
                GroupedRow(label: 'В холде (${_profile!.holdDays} дн.)', value: _money(_profile!.heldBalance)),
              GroupedRow(label: 'Заработано всего', value: _money(_profile?.totalEarned)),
              GroupedRow(label: 'Заданий выполнено', value: '${_profile?.totalJobsCompleted ?? '—'}'),
              GroupedRow(label: 'Трафика использовано', value: _traffic(_profile?.trafficBytesTotal)),
            ],
          ),
          if (_hasVpn && !_vpnLockdown) ...[
            const SizedBox(height: 12),
            const Padding(
              padding: EdgeInsets.symmetric(horizontal: 16),
              child: Text(
                'Сторонний VPN обнаружен. Проверки всё равно идут напрямую через мобильный интернет, минуя VPN.',
                style: TextStyle(fontSize: 13, color: AppColors.textSecondary),
              ),
            ),
          ],
        ],
      ),
    );
  }

  /// Which operator and region this partner is signed up as -- the thing
  /// that decides which jobs reach this phone at all. While the application
  /// is on moderation the values come from the application itself (nothing
  /// is approved yet); once approved, the approved ones are shown, and a
  /// pending change is noted underneath instead of replacing them, because
  /// the work still runs on the old data until it is reviewed.
  List<Widget> _partnerSection() {
    final p = _profile;
    if (p == null || !p.hasPartnerInfo) return const [];
    final pending = p.pendingApplication;
    final showApplication = pending != null && !p.isApproved;
    final source = showApplication ? pending : null;

    final operator = source?.operatorLabel ?? p.operatorLabel;
    final region = source?.region ?? p.region;
    final district = source?.federalDistrictLabel ?? p.federalDistrictLabel;
    if ((operator ?? '').isEmpty && (region ?? '').isEmpty) return const [];

    final rows = <Widget>[
      GroupedRow(label: 'Оператор', value: (operator ?? '').isEmpty ? '—' : operator),
      GroupedRow(label: 'Регион', value: (region ?? '').isEmpty ? '—' : region),
      if ((district ?? '').isNotEmpty) GroupedRow(label: 'Федеральный округ', value: district),
    ];

    String? note;
    if (showApplication) {
      note = 'Эти данные вы указали в заявке — мы их проверяем. '
          'Задания начнут приходить после одобрения.';
    } else if (pending != null) {
      final changed = [pending.operatorLabel, pending.region]
          .where((v) => (v ?? '').isNotEmpty)
          .join(', ');
      note = 'На модерации новые данные${changed.isEmpty ? '' : ': $changed'}. '
          'Пока заявку не одобрят, задания приходят по текущим.';
    }

    return [
      GroupedSection(
        header: showApplication ? 'Заявка на модерации' : 'Партнёр',
        children: rows,
      ),
      if (note != null)
        Padding(
          padding: const EdgeInsets.fromLTRB(16, 8, 16, 0),
          child: Text(
            note,
            style: const TextStyle(fontSize: 13, height: 1.45, color: AppColors.textSecondary),
          ),
        ),
      const SizedBox(height: 20),
    ];
  }

  /// The big word. The switch can be on while the service is only waiting
  /// for mobile data -- that is not "working", so it says «Отключено» and
  /// the subtitle tells what to do.
  String _headline() {
    if (!_isRunning && _updateLocked) return 'Обновитесь';
    if (!_isRunning && _moderationLocked) {
      return _profile!.moderationStatus == 'rejected' ? 'Заявка отклонена' : 'На модерации';
    }
    if (!_isRunning) return 'Остановлено';
    if (_serviceStatus == 'no_mobile_data') return 'Отключено';
    // «Задания только при БС» is on and the operator has no whitelist mode
    // right now: still on, no jobs; flips back to «Работает» by itself.
    if (_serviceStatus == 'no_whitelist') return 'Нет БС';
    return 'Работает';
  }

  String _subtitle() {
    if (_isRunning) return _statusText;
    if (_updateLocked) return 'Эта версия больше не поддерживается';
    if (_moderationLocked) {
      return _profile!.moderationStatus == 'rejected'
          ? 'Подайте заявку заново в Telegram-боте'
          : 'Приём заданий — после одобрения заявки';
    }
    if (_countryBlocked) return 'Мобильная сеть этой страны (${(_blockedCountry ?? '—').toUpperCase()}) не поддерживается';
    if (_vpnLockdown) return 'Добавьте приложение в исключения VPN';
    return 'Включите, чтобы принимать задания';
  }

  String _money(double? v) => v == null ? '—' : '${v.toStringAsFixed(4)} USD';

  String _traffic(int? bytes) {
    if (bytes == null) return '—';
    if (bytes < 1024 * 1024) return '${(bytes / 1024).toStringAsFixed(0)} КБ';
    if (bytes < 1024 * 1024 * 1024) return '${(bytes / (1024 * 1024)).toStringAsFixed(1)} МБ';
    return '${(bytes / (1024 * 1024 * 1024)).toStringAsFixed(2)} ГБ';
  }
}
