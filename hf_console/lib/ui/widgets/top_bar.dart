import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../theme.dart';

class TopBar extends StatelessWidget {
  const TopBar({super.key});

  @override
  Widget build(BuildContext context) {
    context.watch<BusStore>();
    final width = MediaQuery.of(context).size.width;
    final isWide = width > 1200;
    final brandSize = isWide ? 20.0 : 16.0;

    return Row(
      children: [
        Text('MÜHLE', style: AppTheme.display(brandSize, weight: FontWeight.w700)),
        const SizedBox(width: 4),
        Text('· HF', style: AppTheme.display(brandSize, color: AppTheme.txtMute, weight: FontWeight.w500)),
        const SizedBox(width: 10),
        Container(
          padding: EdgeInsets.symmetric(horizontal: isWide ? 12 : 8, vertical: isWide ? 5 : 3),
          decoration: BoxDecoration(
            color: AppTheme.blend(AppTheme.green, 0.12),
            border: Border.all(color: AppTheme.blend(AppTheme.green, 0.45)),
            borderRadius: BorderRadius.circular(4),
          ),
          child: Text('● all online', style: AppTheme.mono(isWide ? 12 : 10, color: AppTheme.green)),
        ),
        const SizedBox(width: 10),
        Expanded(
          child: SingleChildScrollView(
            scrollDirection: Axis.horizontal,
            child: Row(
              children: const [],
            ),
          ),
        ),
        const SizedBox(width: 10),
        Row(
          children: [
            Text('USB', style: AppTheme.mono(isWide ? 16 : 14, color: AppTheme.txt, weight: FontWeight.w700)),
            SizedBox(width: isWide ? 14 : 10),
            Text('14.074.000 HZ', style: AppTheme.mono(isWide ? 16 : 14, color: AppTheme.txt, weight: FontWeight.w600)),
            SizedBox(width: isWide ? 14 : 10),
            _driveBar(0.40, isWide),
            SizedBox(width: isWide ? 14 : 10),
            _tag('● TX', AppTheme.red, isWide),
          ],
        ),
      ],
    );
  }

  Widget _driveBar(double value, bool isWide) {
    return Row(
      children: [
        Container(
          width: isWide ? 72 : 48,
          height: isWide ? 6 : 4,
          decoration: BoxDecoration(
            color: AppTheme.pane,
            borderRadius: BorderRadius.circular(2),
            border: Border.all(color: AppTheme.cardLine),
          ),
          child: FractionallySizedBox(
            alignment: Alignment.centerLeft,
            widthFactor: value,
            child: Container(decoration: BoxDecoration(color: AppTheme.accent, borderRadius: BorderRadius.circular(2))),
          ),
        ),
        const SizedBox(width: 4),
        Text('DRIVE ${(value * 100).toInt()}%', style: AppTheme.mono(isWide ? 13 : 11, color: AppTheme.txt, weight: FontWeight.w600)),
      ],
    );
  }

  Widget _tag(String text, Color fg, bool isWide) {
    return Container(
      padding: EdgeInsets.symmetric(horizontal: isWide ? 8 : 6, vertical: isWide ? 3 : 2),
      decoration: BoxDecoration(
        color: AppTheme.blend(fg, 0.12),
        border: Border.all(color: fg),
        borderRadius: BorderRadius.circular(3),
      ),
      child: Text(text, style: AppTheme.mono(isWide ? 11 : 9, color: fg, weight: FontWeight.w700)),
    );
  }
}
