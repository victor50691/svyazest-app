import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:package_info_plus/package_info_plus.dart';
import 'package:url_launcher/url_launcher.dart';

import '../services/api_client.dart';
import '../services/attestation.dart';
import '../services/secure_store.dart';
import '../theme.dart';
import '../widgets/grouped_list.dart';
import 'login_screen.dart';

const String githubUrl = 'https://github.com/svyazest/svyazest-android';

class SettingsTab extends StatefulWidget {
  const SettingsTab({super.key});

  @override
  State<SettingsTab> createState() => _SettingsTabState();
}

class _SettingsTabState extends State<SettingsTab> with AutomaticKeepAliveClientMixin {
  String _version = '';
  String? _executorId;
  String? _attestation;
  final _api = ApiClient();

  @override
  void initState() {
    super.initState();
    PackageInfo.fromPlatform().then((v) {
      if (mounted) setState(() => _version = '${v.version} (${v.buildNumber})');
    });
    SecureStore.getExecutorId().then((v) {
      if (mounted) setState(() => _executorId = v);
    });
    _loadAttestation();
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
            GroupedRow(label: 'ID исполнителя', value: _executorId ?? '—'),
            GroupedRow(label: 'Версия', value: _version.isEmpty ? '—' : _version),
            GroupedRow(
              label: 'Подлинность',
              value: Attestation.label(_attestation),
              valueColor: _attestationColor(),
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
