import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../theme.dart';

class TopBar extends StatelessWidget {
  const TopBar({super.key});

  @override
  Widget build(BuildContext context) {
    context.watch<BusStore>();
    const bands = ['160', '80', '40', '20', '17', '15', '12', '10', '6'];
    const activeBand = '20';
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
            color: const Color(0x1E36B37E),
            border: Border.all(color: const Color(0x4D36B37E)),
            borderRadius: BorderRadius.circular(4),
          ),
          child: Text('● all online', style: AppTheme.mono(isWide ? 12 : 10, color: AppTheme.green)),
        ),
        const SizedBox(width: 10),
        Expanded(
          child: SingleChildScrollView(
            scrollDirection: Axis.horizontal,
            child: Row(
              children: bands.map((b) {
                final active = b == activeBand;
                return Padding(
                  padding: const EdgeInsets.only(right: 3),
                  child: ElevatedButton(
                    onPressed: () {},
                    style: AppTheme.actionButton(active: active).copyWith(
                      minimumSize: const WidgetStatePropertyAll(Size(34, 28)),
                      padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 4)),
                    ),
                    child: Text(b),
                  ),
                );
              }).toList(),
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
            _tag('● TX', const Color(0x10D9533A), AppTheme.red, isWide),
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
          decoration: BoxDecoration(color: AppTheme.cardLine, borderRadius: BorderRadius.circular(2)),
          child: FractionallySizedBox(
            alignment: Alignment.centerLeft,
            widthFactor: value,
            child: Container(decoration: BoxDecoration(color: AppTheme.blue, borderRadius: BorderRadius.circular(2))),
          ),
        ),
        const SizedBox(width: 4),
        Text('DRIVE ${(value * 100).toInt()}%', style: AppTheme.mono(isWide ? 13 : 11, color: AppTheme.txt, weight: FontWeight.w600)),
      ],
    );
  }

  Widget _tag(String text, Color bg, Color fg, bool isWide) {
    return Container(
      padding: EdgeInsets.symmetric(horizontal: isWide ? 8 : 6, vertical: isWide ? 3 : 2),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(3)),
      child: Text(text, style: AppTheme.mono(isWide ? 11 : 9, color: fg, weight: FontWeight.w600)),
    );
  }
}
