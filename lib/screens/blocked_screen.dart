import 'package:flutter/material.dart';
import '../theme.dart';

/// Terminal screen for a device banned by HWID or a banned account: nothing
/// to do here but read the reason.
class BlockedScreen extends StatelessWidget {
  const BlockedScreen({super.key, required this.title, required this.body});

  final String title;
  final String body;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.fromLTRB(24, 120, 24, 24),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(title, style: const TextStyle(fontSize: 28, fontWeight: FontWeight.w600, letterSpacing: -0.3)),
              const SizedBox(height: 8),
              Text(body, style: const TextStyle(fontSize: 15, height: 1.4, color: AppColors.textSecondary)),
            ],
          ),
        ),
      ),
    );
  }
}
