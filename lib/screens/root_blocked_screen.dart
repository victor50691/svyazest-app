import 'package:flutter/material.dart';
import '../main.dart' show StartupGate;
import '../services/secure_store.dart';
import '../theme.dart';

/// Shown instead of the app on a rooted device: a root can fake network
/// state and check results, so such phones are not accepted as executors --
/// unless the admin made an exception for this partner (1.18.0, admin panel
/// → Партнёры → «Исключение: root и проверка подлинности»). The partner signs
/// in first, so the server knows who asks; «Проверить снова» asks again.
class RootBlockedScreen extends StatelessWidget {
  const RootBlockedScreen({super.key});

  void _restart(BuildContext context) {
    Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => const StartupGate()));
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(24, 120, 24, 24),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              const Text(
                'Устройство с root-правами',
                style: TextStyle(fontSize: 28, fontWeight: FontWeight.w600, letterSpacing: -0.3),
              ),
              const SizedBox(height: 8),
              const Text(
                'На этом телефоне обнаружены root-права. Приложение «Связь Есть?» на таких устройствах не работает: '
                'результаты проверок с них нельзя считать достоверными.\n\n'
                'Если поддержка сделала для вас исключение, нажмите «Проверить снова».',
                style: TextStyle(fontSize: 15, height: 1.4, color: AppColors.textSecondary),
              ),
              const SizedBox(height: 24),
              FilledButton(
                onPressed: () => _restart(context),
                child: const Text('Проверить снова'),
              ),
              const SizedBox(height: 8),
              TextButton(
                onPressed: () async {
                  await SecureStore.clear();
                  if (context.mounted) _restart(context);
                },
                child: const Text('Выйти из аккаунта'),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
