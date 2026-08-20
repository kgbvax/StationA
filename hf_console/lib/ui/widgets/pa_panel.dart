import 'package:flutter/material.dart';
import '../theme.dart';
import 'card_container.dart';

class PaPanel extends StatelessWidget {
  const PaPanel({super.key});

  @override
  Widget build(BuildContext context) {
    return CardContainer(
      padding: const EdgeInsets.all(8),
      child: LayoutBuilder(
        builder: (context, constraints) {
          final isWide = constraints.maxWidth > 220;
          final labelSize = isWide ? 9.0 : 7.5;
          final valueSize = isWide ? 14.0 : 11.0;
          return Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            mainAxisSize: MainAxisSize.min,
            children: [
              CardHeader(
                title: 'PA · ACOM 1200S',
                trailing: _tag('● TX', const Color(0x10D9533A), AppTheme.red),
              ),
              SizedBox(height: isWide ? 8 : 4),
              _datablock([
                _Field('FWD W', '820', AppTheme.green),
                _Field('REFL W', '18', AppTheme.amber),
                _Field('SWR', '1.9', AppTheme.amber),
                _Field('TEMP °C', '54', AppTheme.txt),
              ], labelSize, valueSize, isWide),
              SizedBox(height: isWide ? 8 : 4),
              _meter(820 / 1200, isWide),
              SizedBox(height: isWide ? 12 : 8),
              Row(
                children: [
                  Expanded(child: ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(active: true), child: const Text('OPERATE'))),
                  const SizedBox(width: 5),
                  Expanded(child: ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(amber: true), child: const Text('STANDBY'))),
                ],
              ),
            ],
          );
        },
      ),
    );
  }

  Widget _datablock(List<_Field> fields, double labelSize, double valueSize, bool isWide) {
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border.all(color: AppTheme.cardLine),
        borderRadius: BorderRadius.circular(3),
      ),
      child: GridView.count(
        crossAxisCount: 2,
        shrinkWrap: true,
        childAspectRatio: isWide ? 2.6 : 2.2,
        physics: const NeverScrollableScrollPhysics(),
        children: fields.map((f) {
          return Container(
            padding: EdgeInsets.symmetric(horizontal: isWide ? 10 : 6, vertical: isWide ? 4 : 2),
            decoration: BoxDecoration(border: Border(right: BorderSide(color: AppTheme.cardLine), bottom: BorderSide(color: AppTheme.cardLine))),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              mainAxisAlignment: MainAxisAlignment.center,
              children: [
                Text(f.label, style: AppTheme.mono(labelSize, color: AppTheme.txtMute, weight: FontWeight.w500)),
                Text(f.value, style: AppTheme.mono(valueSize, color: f.color, weight: FontWeight.w600)),
              ],
            ),
          );
        }).toList(),
      ),
    );
  }

  Widget _meter(double fraction, bool isWide) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        SizedBox(
          height: isWide ? 16 : 10,
          child: Stack(
            children: [
              Container(decoration: BoxDecoration(color: AppTheme.cardLine, borderRadius: BorderRadius.circular(2))),
              FractionallySizedBox(
                alignment: Alignment.centerLeft,
                widthFactor: fraction,
                child: Container(
                  decoration: BoxDecoration(
                    gradient: const LinearGradient(colors: [AppTheme.green, Color(0xFF8BD445), AppTheme.amber]),
                    borderRadius: BorderRadius.circular(2),
                  ),
                ),
              ),
              CustomPaint(size: const Size(double.infinity, double.infinity), painter: const _TickPainter()),
            ],
          ),
        ),
        SizedBox(height: isWide ? 4 : 2),
        Row(
          mainAxisAlignment: MainAxisAlignment.spaceBetween,
          children: [
            Text('0', style: AppTheme.mono(isWide ? 10 : 8, color: AppTheme.txtMute)),
            Text('500', style: AppTheme.mono(isWide ? 10 : 8, color: AppTheme.txtMute)),
            Text('1000', style: AppTheme.mono(isWide ? 10 : 8, color: AppTheme.txtMute)),
            Text('1200', style: AppTheme.mono(isWide ? 10 : 8, color: AppTheme.txtMute)),
          ],
        ),
      ],
    );
  }

  Widget _tag(String text, Color bg, Color fg) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(3)),
      child: Text(text, style: AppTheme.mono(9, color: fg, weight: FontWeight.w600)),
    );
  }
}

class _Field {
  final String label;
  final String value;
  final Color color;
  _Field(this.label, this.value, this.color);
}

class _TickPainter extends CustomPainter {
  const _TickPainter();

  @override
  void paint(Canvas canvas, Size size) {
    final paint = Paint()
      ..color = const Color(0x20FFFFFF)
      ..strokeWidth = 1;
    for (final x in [size.width * 0.4166, size.width * 0.8333]) {
      canvas.drawLine(Offset(x, 0), Offset(x, size.height), paint);
    }
  }

  @override
  bool shouldRepaint(covariant CustomPainter oldDelegate) => false;
}
