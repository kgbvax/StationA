import 'package:flutter/material.dart';
import '../theme.dart';

/// Small bordered status badge (`ERR`, `MOVING`, `OFFLINE`, …) used across
/// the panel headers and readout rows. [color] drives both the border and
/// the label; the fill is the same color blended low against the card.
class StatusTag extends StatelessWidget {
  final String label;
  final Color color;

  const StatusTag({super.key, required this.label, required this.color});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 2),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(label,
          style: AppTheme.mono(10, color: color, weight: FontWeight.w700)),
    );
  }
}