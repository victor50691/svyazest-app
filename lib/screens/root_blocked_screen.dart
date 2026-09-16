import 'package:flutter/material.dart';
import '../theme.dart';

/// Shown instead of the app on a rooted device: a root can fake network
/// state and check results, so such phones are not accepted as executors.
class RootBlockedScreen extends StatelessWidget {
  const RootBlockedScreen({super.key});

  @override
  Widget build(BuildContext context) {
    return const Scaffold(
      body: SafeArea(
        child: Padding(
          padding: EdgeInsets.fromLTRB(24, 120, 24, 24),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                'Устройство с root-правами',
                style: TextStyle(fontSize: 28, fontWeight: FontWeight.w600, letterSpacing: -0.3),
              ),
              SizedBox(height: 8),
              Text(
                'На этом телефоне обнаружены root-права. Приложение «Связь Есть?» на таких устройствах не работает: '
                'результаты проверок с них нельзя считать достоверными.',
                style: TextStyle(fontSize: 15, height: 1.4, color: AppColors.textSecondary),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
