import 'dart:math';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../theme.dart';

/// Full-screen «Новый уровень!» -- shown once when the app sees a level
/// above the last one it showed (home_screen.dart). A burst of particles,
/// the badge springing in, a vibration.
Future<void> showLevelUp(BuildContext context, {required int level, required double share}) {
  HapticFeedback.heavyImpact();
  return showGeneralDialog<void>(
    context: context,
    barrierDismissible: true,
    barrierLabel: 'level-up',
    barrierColor: Colors.black.withValues(alpha: 0.75),
    transitionDuration: const Duration(milliseconds: 250),
    pageBuilder: (context, _, _) => _LevelUp(level: level, share: share),
    transitionBuilder: (context, anim, _, child) => FadeTransition(opacity: anim, child: child),
  );
}

class _LevelUp extends StatefulWidget {
  const _LevelUp({required this.level, required this.share});
  final int level;
  final double share;

  @override
  State<_LevelUp> createState() => _LevelUpState();
}

class _LevelUpState extends State<_LevelUp> with SingleTickerProviderStateMixin {
  late final AnimationController _c = AnimationController(vsync: this, duration: const Duration(milliseconds: 1800))
    ..forward();
  final _particles = List.generate(48, (i) {
    final r = Random(i * 7919 + 17);
    return (
      angle: r.nextDouble() * 2 * pi,
      speed: 0.55 + r.nextDouble() * 0.55,
      size: 3.0 + r.nextDouble() * 5,
      color: [AppColors.accent, AppColors.warning, AppColors.success, const Color(0xFFFF7AB6)][i % 4],
    );
  });

  @override
  void dispose() {
    _c.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final pct = widget.share == widget.share.roundToDouble() ? widget.share.toInt().toString() : widget.share.toString();
    return Material(
      type: MaterialType.transparency,
      child: AnimatedBuilder(
        animation: _c,
        builder: (context, _) {
          final t = _c.value;
          final pop = Curves.elasticOut.transform((t / 0.6).clamp(0.0, 1.0));
          final fadeText = Curves.easeOut.transform(((t - 0.25) / 0.4).clamp(0.0, 1.0));
          return Stack(
            alignment: Alignment.center,
            children: [
              Positioned.fill(child: CustomPaint(painter: _Burst(t, _particles))),
              Column(
                mainAxisSize: MainAxisSize.min,
                children: [
                  Transform.scale(
                    scale: 0.2 + 0.8 * pop,
                    child: Container(
                      width: 132,
                      height: 132,
                      alignment: Alignment.center,
                      decoration: BoxDecoration(
                        shape: BoxShape.circle,
                        gradient: const LinearGradient(
                          colors: [Color(0xFF7DB2FF), Color(0xFF3D73D6)],
                          begin: Alignment.topLeft,
                          end: Alignment.bottomRight,
                        ),
                        boxShadow: [
                          BoxShadow(color: AppColors.accent.withValues(alpha: 0.55 * pop), blurRadius: 48, spreadRadius: 6),
                        ],
                      ),
                      child: Text('${widget.level}',
                          style: const TextStyle(fontSize: 64, fontWeight: FontWeight.w800, color: Colors.white)),
                    ),
                  ),
                  const SizedBox(height: 28),
                  Opacity(
                    opacity: fadeText,
                    child: Transform.translate(
                      offset: Offset(0, 12 * (1 - fadeText)),
                      child: Column(
                        children: [
                          const Text('Новый уровень!', style: TextStyle(fontSize: 26, fontWeight: FontWeight.w700)),
                          const SizedBox(height: 8),
                          Text('Вы молодец! Теперь ваша доля — $pct% с каждой проверки,\nа задания будут доставаться вам чаще.',
                              textAlign: TextAlign.center,
                              style: const TextStyle(fontSize: 15, height: 1.45, color: AppColors.textSecondary)),
                          const SizedBox(height: 26),
                          ElevatedButton(onPressed: () => Navigator.of(context).pop(), child: const Text('Отлично')),
                        ],
                      ),
                    ),
                  ),
                ],
              ),
            ],
          );
        },
      ),
    );
  }
}

class _Burst extends CustomPainter {
  _Burst(this.t, this.particles);
  final double t;
  final List<({double angle, double speed, double size, Color color})> particles;

  @override
  void paint(Canvas canvas, Size size) {
    final center = Offset(size.width / 2, size.height / 2 - 70);
    final travel = Curves.easeOutCubic.transform(t);
    final fade = (1 - ((t - 0.45) / 0.55).clamp(0.0, 1.0));
    final reach = size.shortestSide * 0.55;
    for (final p in particles) {
      final d = reach * p.speed * travel;
      final pos = center + Offset(cos(p.angle) * d, sin(p.angle) * d + 60 * t * t);
      final paint = Paint()..color = p.color.withValues(alpha: fade);
      canvas.drawCircle(pos, p.size * (1 - 0.4 * t), paint);
    }
  }

  @override
  bool shouldRepaint(_Burst old) => old.t != t;
}
