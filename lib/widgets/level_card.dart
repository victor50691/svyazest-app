import 'dart:math' as math;

import 'package:flutter/material.dart';

import '../models/profile.dart';
import '../theme.dart';

/// «Уровень» on the home screen: level, share, progress to the next level,
/// the day streak and achievement badges (admin-panel/services/
/// executorLevels.js decides all of it; this only shows).
class LevelCard extends StatelessWidget {
  const LevelCard({super.key, required this.info});
  final LevelInfo info;

  @override
  Widget build(BuildContext context) {
    final next = info.nextMin;
    final frac = next == null
        ? 1.0
        : ((info.points - info.levelMin) / (next - info.levelMin).clamp(1, 1 << 30)).clamp(0.0, 1.0).toDouble();
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const Padding(
          padding: EdgeInsets.fromLTRB(16, 0, 16, 8),
          child: Text('Уровень', style: TextStyle(fontSize: 13, color: AppColors.textSecondary)),
        ),
        Container(
          padding: const EdgeInsets.fromLTRB(16, 14, 16, 14),
          decoration: BoxDecoration(color: AppColors.group, borderRadius: BorderRadius.circular(12)),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Row(
                children: [
                  _LevelBadge(level: info.level),
                  const SizedBox(width: 12),
                  Expanded(
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text('Уровень ${info.level}',
                            style: const TextStyle(fontSize: 17, fontWeight: FontWeight.w600)),
                        const SizedBox(height: 2),
                        Text('Доля ${_pct(info.share)} с каждой проверки',
                            style: const TextStyle(fontSize: 13, color: AppColors.textSecondary)),
                      ],
                    ),
                  ),
                  if (info.streakDays > 0) _Streak(days: info.streakDays, today: info.streakToday),
                ],
              ),
              const SizedBox(height: 14),
              ClipRRect(
                borderRadius: BorderRadius.circular(6),
                child: TweenAnimationBuilder<double>(
                  tween: Tween(begin: 0, end: frac),
                  duration: const Duration(milliseconds: 900),
                  curve: Curves.easeOutCubic,
                  builder: (context, v, _) => LinearProgressIndicator(
                    value: v,
                    minHeight: 8,
                    backgroundColor: AppColors.bg,
                    valueColor: AlwaysStoppedAnimation(next == null ? AppColors.success : AppColors.accent),
                  ),
                ),
              ),
              if (info.achievements.isNotEmpty) ...[
                const SizedBox(height: 16),
                Wrap(
                  spacing: 4,
                  runSpacing: 12,
                  children: [for (final a in info.achievements) _Badge(a: a)],
                ),
              ],
              const SizedBox(height: 12),
              const Text(
                'Уровень прокачивается пока приложение включено (больше — при белых списках) и за каждое выполненное задание.',
                style: TextStyle(fontSize: 12, height: 1.4, color: AppColors.textSecondary),
              ),
            ],
          ),
        ),
      ],
    );
  }

  static String _pct(double v) => '${v == v.roundToDouble() ? v.toInt() : v}%';
}

class _LevelBadge extends StatelessWidget {
  const _LevelBadge({required this.level});
  final int level;

  @override
  Widget build(BuildContext context) {
    return Container(
      width: 44,
      height: 44,
      alignment: Alignment.center,
      decoration: const BoxDecoration(
        shape: BoxShape.circle,
        gradient: LinearGradient(
          colors: [Color(0xFF6FA8FF), Color(0xFF3D73D6)],
          begin: Alignment.topLeft,
          end: Alignment.bottomRight,
        ),
      ),
      child: Text('$level', style: const TextStyle(fontSize: 20, fontWeight: FontWeight.w700, color: Colors.white)),
    );
  }
}

class _Streak extends StatelessWidget {
  const _Streak({required this.days, required this.today});
  final int days;
  final bool today;

  @override
  Widget build(BuildContext context) {
    return Tooltip(
      message: today ? 'Сегодня уже засчитано' : 'Будьте на связи сегодня, чтобы не прервать серию',
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
        decoration: BoxDecoration(
          color: (today ? AppColors.warning : AppColors.textSecondary).withValues(alpha: 0.14),
          borderRadius: BorderRadius.circular(20),
        ),
        child: Text('🔥 $days', style: TextStyle(fontSize: 14, fontWeight: FontWeight.w600,
            color: today ? AppColors.warning : AppColors.textSecondary)),
      ),
    );
  }
}

/// Achievement medallion (1.16.3): a thin line glyph drawn in code
/// (_GlyphPainter) in a ring -- a blue gradient ring when earned, a faint one
/// when not -- with the title under it. Tap = details.
class _Badge extends StatelessWidget {
  const _Badge({required this.a});
  final Achievement a;

  String _date(DateTime d) {
    final l = d.toLocal();
    return '${l.day.toString().padLeft(2, '0')}.${l.month.toString().padLeft(2, '0')}.${l.year}';
  }

  @override
  Widget build(BuildContext context) {
    final got = a.earnedAt != null;
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      // Full screen (1.16.4): the half-height sheet looked cut off.
      onTap: () => Navigator.of(context).push(MaterialPageRoute<void>(
            builder: (_) => _AchievementPage(a: a, date: got ? _date(a.earnedAt!) : null),
          )),
      child: SizedBox(
        width: 70,
        child: Column(
          children: [
            _Medal(code: a.code, earned: got, size: 48),
            const SizedBox(height: 6),
            Text(
              a.title,
              textAlign: TextAlign.center,
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
              style: TextStyle(fontSize: 10.5, height: 1.2,
                  color: got ? AppColors.textPrimary : AppColors.textSecondary.withValues(alpha: 0.75)),
            ),
          ],
        ),
      ),
    );
  }
}

class _AchievementPage extends StatelessWidget {
  const _AchievementPage({required this.a, required this.date});
  final Achievement a;
  final String? date;

  @override
  Widget build(BuildContext context) {
    final got = date != null;
    return Scaffold(
      backgroundColor: AppColors.bg,
      appBar: AppBar(title: const Text('Достижение')),
      body: SafeArea(
        child: Center(
          child: SingleChildScrollView(
            padding: const EdgeInsets.fromLTRB(32, 24, 32, 48),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                _Medal(code: a.code, earned: got, size: 132),
                const SizedBox(height: 28),
                Text(a.title, textAlign: TextAlign.center,
                    style: const TextStyle(fontSize: 24, fontWeight: FontWeight.w600)),
                const SizedBox(height: 10),
                Text(a.desc, textAlign: TextAlign.center,
                    style: const TextStyle(fontSize: 16, height: 1.4, color: AppColors.textSecondary)),
                const SizedBox(height: 24),
                Container(
                  padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
                  decoration: BoxDecoration(
                    color: (got ? AppColors.success : AppColors.textSecondary).withValues(alpha: 0.12),
                    borderRadius: BorderRadius.circular(20),
                  ),
                  child: Text(
                    got ? 'Получено $date' : 'Ещё не получено',
                    style: TextStyle(fontSize: 14, fontWeight: FontWeight.w500,
                        color: got ? AppColors.success : AppColors.textSecondary),
                  ),
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}

class _Medal extends StatelessWidget {
  const _Medal({required this.code, required this.earned, required this.size});
  final String code;
  final bool earned;
  final double size;

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      width: size,
      height: size,
      child: CustomPaint(
        painter: _RingPainter(earned: earned),
        child: Padding(
          padding: EdgeInsets.all(size * 0.27),
          child: CustomPaint(
            painter: _GlyphPainter(
              code: code,
              color: earned ? const Color(0xFFDCE8FF) : AppColors.textSecondary.withValues(alpha: 0.5),
            ),
          ),
        ),
      ),
    );
  }
}

class _RingPainter extends CustomPainter {
  _RingPainter({required this.earned});
  final bool earned;

  @override
  void paint(Canvas canvas, Size size) {
    final c = size.center(Offset.zero);
    final r = size.shortestSide / 2;
    if (earned) {
      canvas.drawCircle(c, r - 1, Paint()
        ..shader = const RadialGradient(colors: [Color(0x403D73D6), Color(0x1A3D73D6)]).createShader(Rect.fromCircle(center: c, radius: r)));
      canvas.drawCircle(c, r - 1.2, Paint()
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1.8
        ..shader = const SweepGradient(colors: [Color(0xFF8DBBFF), Color(0xFF3D73D6), Color(0xFF8DBBFF)])
            .createShader(Rect.fromCircle(center: c, radius: r)));
    } else {
      canvas.drawCircle(c, r - 1, Paint()..color = const Color(0x0DFFFFFF));
      canvas.drawCircle(c, r - 1, Paint()
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1
        ..color = const Color(0x1FFFFFFF));
    }
  }

  @override
  bool shouldRepaint(_RingPainter old) => old.earned != earned;
}

/// Monoline glyphs on a 24x24 grid, one per achievement code.
class _GlyphPainter extends CustomPainter {
  _GlyphPainter({required this.code, required this.color});
  final String code;
  final Color color;

  @override
  void paint(Canvas canvas, Size size) {
    final k = size.shortestSide / 24;
    canvas.save();
    canvas.scale(k, k);
    final p = Paint()
      ..color = color
      ..style = PaintingStyle.stroke
      ..strokeWidth = 1.7
      ..strokeCap = StrokeCap.round
      ..strokeJoin = StrokeJoin.round;
    final fill = Paint()..color = color;
    switch (code) {
      case 'first_job': // target
        canvas.drawCircle(const Offset(12, 12), 8.5, p);
        canvas.drawCircle(const Offset(12, 12), 4.5, p);
        canvas.drawCircle(const Offset(12, 12), 1.3, fill);
        break;
      case 'bs_first': // signal bars
        for (final (x, h) in [(6.0, 5.0), (10.0, 8.5), (14.0, 12.0), (18.0, 15.5)]) {
          canvas.drawLine(Offset(x, 19.5), Offset(x, 19.5 - h), p);
        }
        break;
      case 'night_shift': // crescent + a star
        final moon = Path.combine(
          PathOperation.difference,
          Path()..addOval(Rect.fromCircle(center: const Offset(11, 13), radius: 7.5)),
          Path()..addOval(Rect.fromCircle(center: const Offset(15.2, 9.6), radius: 6.2)),
        );
        canvas.drawPath(moon, p);
        canvas.drawCircle(const Offset(18.6, 4.8), 1.1, fill);
        break;
      case 'streak_7': // flame
        canvas.drawPath(_flame(), p);
        break;
      case 'streak_30': // calendar
        canvas.drawRRect(RRect.fromRectAndRadius(const Rect.fromLTWH(4, 5.5, 16, 14.5), const Radius.circular(3)), p);
        canvas.drawLine(const Offset(4, 10), const Offset(20, 10), p);
        canvas.drawLine(const Offset(8.5, 3.5), const Offset(8.5, 7), p);
        canvas.drawLine(const Offset(15.5, 3.5), const Offset(15.5, 7), p);
        for (final o in const [Offset(8.5, 14), Offset(12, 14), Offset(15.5, 14), Offset(8.5, 17), Offset(12, 17)]) {
          canvas.drawCircle(o, 0.9, fill);
        }
        break;
      case 'online_100h': // clock
        canvas.drawCircle(const Offset(12, 12), 8.5, p);
        canvas.drawLine(const Offset(12, 12), const Offset(12, 7), p);
        canvas.drawLine(const Offset(12, 12), const Offset(15.5, 14), p);
        break;
      case 'jobs_100': // check in a circle
        canvas.drawCircle(const Offset(12, 12), 8.5, p);
        canvas.drawPath(Path()..moveTo(8.3, 12.3)..lineTo(10.9, 14.9)..lineTo(15.9, 9.4), p);
        break;
      case 'jobs_500': // shield with a check
        canvas.drawPath(
          Path()
            ..moveTo(12, 3.5)
            ..lineTo(18.5, 6)
            ..lineTo(18.5, 11.5)
            ..cubicTo(18.5, 15.8, 15.7, 19, 12, 20.5)
            ..cubicTo(8.3, 19, 5.5, 15.8, 5.5, 11.5)
            ..lineTo(5.5, 6)
            ..close(),
          p,
        );
        canvas.drawPath(Path()..moveTo(9, 12)..lineTo(11.2, 14.2)..lineTo(15.2, 9.8), p);
        break;
      default: // star
        final star = Path();
        for (var i = 0; i < 10; i++) {
          final r = i.isEven ? 8.5 : 3.8;
          final a = -math.pi / 2 + i * math.pi / 5;
          final pt = Offset(12 + r * math.cos(a), 12.5 + r * math.sin(a));
          i == 0 ? star.moveTo(pt.dx, pt.dy) : star.lineTo(pt.dx, pt.dy);
        }
        canvas.drawPath(star..close(), p);
    }
    canvas.restore();
  }

  static Path _flame() => Path()
    ..moveTo(12, 3.5)
    ..cubicTo(13.6, 7.6, 18, 9.6, 18, 14.2)
    ..cubicTo(18, 17.7, 15.3, 20.5, 12, 20.5)
    ..cubicTo(8.7, 20.5, 6, 17.7, 6, 14.3)
    ..cubicTo(6, 11.6, 7.5, 9.9, 9, 8.8)
    ..cubicTo(9.2, 10.7, 10, 11.9, 11.2, 12.5)
    ..cubicTo(10.9, 9.3, 11.1, 6.1, 12, 3.5)
    ..close();

  @override
  bool shouldRepaint(_GlyphPainter old) => old.code != code || old.color != color;
}
