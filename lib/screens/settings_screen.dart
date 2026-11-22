import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:package_info_plus/package_info_plus.dart';
import 'package:url_launcher/url_launcher.dart';

import '../services/api_client.dart';
import '../services/attestation.dart';
import '../services/control_http.dart';
import '../services/diag_log.dart';
import '../services/native_bridge.dart';
import '../services/secure_store.dart';
import '../theme.dart';
import '../widgets/diag_stop.dart';
import '../widgets/grouped_list.dart';
import 'login_screen.dart';

const String githubUrl = 'https://github.com/victor50691/svyazest-app'; // = executor_app_repo_url in the bot

class SettingsTab extends StatefulWidget {
  const SettingsTab({super.key});

  @override
  State<SettingsTab> createState() => _SettingsTabState();
}

class _SettingsTabState extends State<SettingsTab> with AutomaticKeepAliveClientMixin {
  String _version = '';
  String _xray = ''; // embedded Xray-core (xraylib.aar), e.g. 26.3.27
  String? _executorId;
  String? _attestation;
  final _api = ApiClient();
  // «Связь с сервером» (1.11.0): the route the last request took, and the
  // VPN client's proxy port if the partner set one (0 = found automatically).
  String _route = '—';
  int _proxyPort = 0;
  String _proxyUser = '';

  @override
  void initState() {
    super.initState();
    PackageInfo.fromPlatform().then((v) {
      if (mounted) setState(() => _version = '${v.version} (${v.buildNumber})');
    });
    NativeBridge.xrayVersion().then((v) {
      if (mounted) setState(() => _xray = v);
    });
    SecureStore.getExecutorId().then((v) {
      if (mounted) setState(() => _executorId = v);
    });
    _loadAttestation();
    _loadControl();
    // The route changes as networks come and go -- keep the row current
    // (the tab stays alive, so a value read once at start would go stale).
    _controlTimer = Timer.periodic(const Duration(seconds: 3), (_) => _loadControl());
  }

  Timer? _controlTimer;

  @override
  void dispose() {
    _controlTimer?.cancel();
    super.dispose();
  }

  Future<void> _loadControl() async {
    final i = await ControlHttpClient.info();
    if (!mounted || (i.route == _route && i.manualPort == _proxyPort && i.proxyUser == _proxyUser)) return;
    setState(() { _route = i.route; _proxyPort = i.manualPort; _proxyUser = i.proxyUser; });
  }

  Future<void> _editProxy() async {
    final portCtl = TextEditingController(text: _proxyPort > 0 ? '$_proxyPort' : '');
    final userCtl = TextEditingController(text: _proxyUser);
    final passCtl = TextEditingController();
    final saved = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Прокси VPN-клиента'),
        content: SingleChildScrollView(
          child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.start, children: [
            const Text('Обычно ничего вводить не нужно: приложение находит прокси само. Заполните, только если в «Сейчас через» '
                '«нет связи», а прокси в VPN-клиенте на необычном порту или с логином и паролем.',
                style: TextStyle(fontSize: 14, height: 1.4, color: AppColors.textSecondary)),
            const SizedBox(height: 12),
            TextField(controller: portCtl, keyboardType: TextInputType.number,
                decoration: const InputDecoration(labelText: 'Порт', hintText: 'пусто — искать автоматически')),
            const SizedBox(height: 8),
            TextField(controller: userCtl, autocorrect: false, enableSuggestions: false,
                decoration: const InputDecoration(labelText: 'Логин', hintText: 'пусто — без авторизации')),
            const SizedBox(height: 8),
            TextField(controller: passCtl, obscureText: true, autocorrect: false, enableSuggestions: false,
                decoration: InputDecoration(labelText: 'Пароль',
                    hintText: _proxyUser.isNotEmpty ? 'пусто — оставить прежний' : null)),
          ]),
        ),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx, true), child: const Text('Сохранить')),
        ],
      ),
    );
    if (saved != true) return;
    final port = int.tryParse(portCtl.text.trim()) ?? 0;
    final user = userCtl.text.trim();
    var pass = passCtl.text;
    // Same login and an empty password field = keep the stored password.
    if (user.isNotEmpty && user == _proxyUser && pass.isEmpty) pass = '\u0000keep';
    await ControlHttpClient.setProxy(port >= 1 && port <= 65535 ? port : 0, user, pass);
    await _loadControl();
  }

  Future<void> _loadAttestation() async {
    try {
      final p = await _api.profile();
      if (mounted) setState(() => _attestation = p.attestationStatus);
    } catch (_) {}
  }

  Color? _attestationColor() {
    switch (_attestation) {
      case 'verified':
        return AppColors.success;
      case 'limited':
        return AppColors.warning;
      case 'failed':
        return AppColors.danger;
      default:
        return null;
    }
  }

  Future<void> _logout() async {
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Выйти из приложения?'),
        content: const Text('Задания перестанут приходить на этот телефон, пока вы не войдёте снова.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(context, false), child: const Text('Отмена')),
          TextButton(
            onPressed: () => Navigator.pop(context, true),
            style: TextButton.styleFrom(foregroundColor: AppColors.danger),
            child: const Text('Выйти'),
          ),
        ],
      ),
    );
    if (confirmed != true) return;
    await FlutterForegroundTask.stopService();
    await SecureStore.clear();
    if (!mounted) return;
    Navigator.of(context).pushAndRemoveUntil(
      MaterialPageRoute(builder: (_) => const LoginScreen()),
      (route) => false,
    );
  }

  @override
  bool get wantKeepAlive => true;

  @override
  Widget build(BuildContext context) {
    super.build(context);
    return ListView(
      padding: const EdgeInsets.fromLTRB(16, 4, 16, 16),
      children: [
        GroupedSection(
          children: [
            GroupedRow(label: 'ID партнёра', value: _executorId ?? '—'),
            GroupedRow(label: 'Версия', value: _version.isEmpty ? '—' : _version),
            GroupedRow(label: 'Xray-core', value: _xray.isEmpty ? '—' : _xray),
            GroupedRow(
              label: 'Подлинность',
              value: Attestation.label(_attestation),
              valueColor: _attestationColor(),
            ),
          ],
        ),
        ValueListenableBuilder<int?>(
          valueListenable: DiagLog.app.activeId,
          builder: (context, id, _) => id == null
              ? const SizedBox.shrink()
              : Padding(
                  padding: const EdgeInsets.only(top: 12),
                  child: GroupedSection(
                    header: 'Диагностика',
                    children: [
                      const GroupedRow(label: 'Сбор логов', value: 'включён', valueColor: AppColors.warning),
                      GroupedRow(
                        label: 'Остановить сбор логов',
                        labelColor: AppColors.danger,
                        onTap: () => confirmStopDiag(context, id),
                      ),
                    ],
                  ),
                ),
        ),
        const SizedBox(height: 12),
        GroupedSection(
          header: 'Связь с сервером',
          children: [
            GroupedRow(
              label: 'Сейчас через',
              value: _route == '—' ? 'ещё не было запросов' : _route,
              valueColor: _route == 'нет связи' ? AppColors.danger : null,
              onTap: _loadControl,
            ),
            GroupedRow(
              label: 'Прокси VPN-клиента',
              value: (_proxyPort > 0 ? 'порт $_proxyPort' : 'авто') + (_proxyUser.isNotEmpty ? ' · с паролем' : ''),
              chevron: true,
              onTap: _editProxy,
            ),
          ],
        ),
        const SizedBox(height: 12),
        GroupedSection(
          children: [
            GroupedRow(
              label: 'Бот «Связь Есть?»',
              chevron: true,
              onTap: () => launchUrl(Uri.parse('https://t.me/svyaz_estt_bot'), mode: LaunchMode.externalApplication),
            ),
            GroupedRow(
              label: 'Исходный код',
              chevron: true,
              onTap: () => launchUrl(Uri.parse(githubUrl), mode: LaunchMode.externalApplication),
            ),
          ],
        ),
        const SizedBox(height: 12),
        GroupedSection(
          children: [
            GroupedRow(label: 'Выйти', labelColor: AppColors.danger, onTap: _logout),
          ],
        ),
      ],
    );
  }
}
