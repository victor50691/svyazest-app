import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:url_launcher/url_launcher.dart';

import '../models/profile.dart';
import '../services/api_client.dart';
import '../services/executor_task_handler.dart';
import '../services/native_bridge.dart';
import '../services/session_guard.dart';
import '../theme.dart';
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
    }
  }

  void _onTaskData(Object data) {
    if (data is Map) {
      if (data['type'] == 'status' && mounted) {
        setState(() {
          _statusText = data['text'] as String? ?? _statusText;
          _serviceStatus = data['status'] as String? ?? '';
        });
      } else if (data['type'] == 'job_done') {
        _loadProfile();
      }
    }
  }

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
    if (running && !_busy && (!_permsOk || _vpnLockdown || _countryBlocked || _switchLocked)) {
      await FlutterForegroundTask.stopService();
      if (mounted) setState(() => _isRunning = false);
    }
  }

  bool get _permsOk => _notifOk && _batteryOk;

  /// Partner application not approved yet (or rejected) in the Telegram
  /// bot: signed in, but the work switch stays locked.
  bool get _moderationLocked => _profile != null && !_profile!.isApproved;

  /// This app is below the minimum supported version (admin panel).
  bool get _updateLocked => _profile?.appOutdated == true;

  /// Anything that keeps the work switch greyed out.
  bool get _switchLocked => _updateLocked || _moderationLocked;

  Future<void> _openUpdate() async {
    final url = _profile?.updateUrl;
    if (url == null || url.isEmpty) return;
    await launchUrl(Uri.parse(url), mode: LaunchMode.externalApplication);
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
    } catch (e) {
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
                                  : (_headline() == 'На модерации' || _headline() == 'Нужно обновление'
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
            if ((_profile!.updateUrl ?? '').isNotEmpty) ...[
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
            const VpnBypassClientsSection(header: 'Инструкция для вашего VPN-клиента'),
            const SizedBox(height: 20),
          ],
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
          const SizedBox(height: 20),
          GroupedSection(
            header: 'Баланс',
            children: [
              GroupedRow(label: 'Доступно', value: _money(_profile?.balance), valueColor: AppColors.textPrimary),
              GroupedRow(label: 'Заработано всего', value: _money(_profile?.totalEarned)),
              GroupedRow(label: 'Заданий выполнено', value: '${_profile?.totalJobsCompleted ?? '—'}'),
              GroupedRow(label: 'Трафик на проверки', value: _traffic(_profile?.trafficBytesTotal)),
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

  /// The big word. The switch can be on while the service is only waiting
  /// for mobile data -- that is not "working", so it says «Отключено» and
  /// the subtitle tells what to do.
  String _headline() {
    if (!_isRunning && _updateLocked) return 'Нужно обновление';
    if (!_isRunning && _moderationLocked) {
      return _profile!.moderationStatus == 'rejected' ? 'Заявка отклонена' : 'На модерации';
    }
    if (!_isRunning) return 'Остановлено';
    if (_serviceStatus == 'no_mobile_data') return 'Отключено';
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
