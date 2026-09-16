import 'package:flutter/material.dart';
import '../theme.dart';

/// A rounded list group with an optional small header above it -- the one
/// building block every screen is made of (see the approved design).
class GroupedSection extends StatelessWidget {
  const GroupedSection({super.key, this.header, required this.children});

  final String? header;
  final List<Widget> children;

  @override
  Widget build(BuildContext context) {
    final rows = <Widget>[];
    for (var i = 0; i < children.length; i++) {
      if (i > 0) {
        rows.add(const Padding(
          padding: EdgeInsets.only(left: 16),
          child: Divider(height: 1, thickness: 1, color: AppColors.separator),
        ));
      }
      rows.add(children[i]);
    }
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (header != null)
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 0, 16, 8),
            child: Text(header!, style: const TextStyle(fontSize: 13, color: AppColors.textSecondary)),
          ),
        // Material is what InkWell paints its ripple on -- putting one here
        // (clipped to the radius) keeps the tap highlight inside the group
        // instead of a square on the page behind it.
        Material(
          color: AppColors.group,
          borderRadius: BorderRadius.circular(12),
          clipBehavior: Clip.antiAlias,
          child: Column(children: rows),
        ),
      ],
    );
  }
}

/// "Label ........ value" row. `value` is secondary grey unless a colour is
/// given; `chevron` marks a row that opens something.
class GroupedRow extends StatelessWidget {
  const GroupedRow({
    super.key,
    required this.label,
    this.value,
    this.valueColor,
    this.labelColor,
    this.chevron = false,
    this.onTap,
    this.leading,
  });

  final String label;
  final String? value;
  final Color? valueColor;
  final Color? labelColor;
  final bool chevron;
  final VoidCallback? onTap;
  /// Small icon / logo shown before the label.
  final Widget? leading;

  @override
  Widget build(BuildContext context) {
    final body = ConstrainedBox(
      constraints: const BoxConstraints(minHeight: 52),
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 16),
        child: Row(
          children: [
            if (leading != null) ...[
              Padding(padding: const EdgeInsets.symmetric(vertical: 10), child: leading),
              const SizedBox(width: 12),
            ],
            Expanded(
              child: Text(label, style: TextStyle(fontSize: 16, color: labelColor ?? AppColors.textPrimary)),
            ),
            if (value != null)
              Text(value!, style: TextStyle(fontSize: 16, color: valueColor ?? AppColors.textSecondary)),
            if (chevron) ...[
              const SizedBox(width: 6),
              const Icon(Icons.chevron_right, size: 20, color: AppColors.chevron),
            ],
          ],
        ),
      ),
    );
    if (onTap == null) return body;
    // No ripple: a plain tap, nothing sprays out from under the finger.
    return InkWell(
      onTap: onTap,
      splashColor: Colors.transparent,
      highlightColor: Colors.transparent,
      splashFactory: NoSplash.splashFactory,
      child: body,
    );
  }
}

/// Two-line row for the history list: title + subtitle on the left, a
/// coloured trailing value (reward or status word) on the right.
class GroupedTwoLineRow extends StatelessWidget {
  const GroupedTwoLineRow({
    super.key,
    required this.title,
    required this.subtitle,
    required this.trailing,
    required this.trailingColor,
  });

  final String title;
  final String subtitle;
  final String trailing;
  final Color trailingColor;

  @override
  Widget build(BuildContext context) {
    return ConstrainedBox(
      constraints: const BoxConstraints(minHeight: 60),
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 10),
        child: Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(title, maxLines: 1, overflow: TextOverflow.ellipsis, style: const TextStyle(fontSize: 16)),
                  const SizedBox(height: 3),
                  Text(subtitle, style: const TextStyle(fontSize: 13, color: AppColors.textSecondary)),
                ],
              ),
            ),
            const SizedBox(width: 12),
            Text(trailing, style: TextStyle(fontSize: 15, color: trailingColor)),
          ],
        ),
      ),
    );
  }
}
