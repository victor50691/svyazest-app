import 'package:flutter/material.dart';

import '../services/diag_log.dart';
import '../theme.dart';

/// «Остановить сбор логов» -- on the home screen (while collecting) and in
/// Настройки. The partner may stop at any time, but is warned first: support
/// turns collection off itself once the diagnosis is done, and stopping
/// early can leave the problem unexplained.
Future<void> confirmStopDiag(BuildContext context, int id) async {
  final ok = await showDialog<bool>(
    context: context,
    builder: (context) => AlertDialog(
      title: const Text('Остановить сбор логов?'),
      content: const Text(
        'Сбор логов отключает поддержка, когда закончит диагностику. '
        'Если остановить его раньше, поддержке может не хватить данных, чтобы найти причину проблемы.\n\n'
        'Остановить сбор сейчас?',
      ),
      actions: [
        TextButton(onPressed: () => Navigator.pop(context, false), child: const Text('Не останавливать')),
        TextButton(
          onPressed: () => Navigator.pop(context, true),
          style: TextButton.styleFrom(foregroundColor: AppColors.danger),
          child: const Text('Остановить'),
        ),
      ],
    ),
  );
  if (ok != true || !context.mounted) return;
  try {
    await DiagConsent.stop(id, DiagLog.app);
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(const SnackBar(content: Text('Сбор логов остановлен')));
    }
  } catch (_) {
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(content: Text('Не удалось остановить — проверьте интернет и попробуйте ещё раз')),
      );
    }
  }
}
