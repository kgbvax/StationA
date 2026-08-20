import 'package:flutter/material.dart';
import '../theme.dart';
import 'card_container.dart';

class TunerPanel extends StatelessWidget {
  const TunerPanel({super.key});

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
                title: 'Tuner · ATR-1000',
                trailing: _tag('IN LINE', const Color(0x1036B37E), AppTheme.green),
              ),
              SizedBox(height: isWide ? 8 : 4),
              _datablock([
                _Field('FWD W', '90', AppTheme.green),
                _Field('SWR', '1.3', AppTheme.green),
                _Field('L µH', '0.42', AppTheme.txt),
                _Field('C pF', '128', AppTheme.txt),
              ], labelSize, valueSize, isWide),
              SizedBox(height: isWide ? 12 : 8),
              Row(
                children: [
                  Expanded(child: ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(), child: const Text('TUNE MEM'))),
                  const SizedBox(width: 5),
                  Expanded(child: ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(), child: const Text('TUNE FULL'))),
                ],
              ),
              SizedBox(height: isWide ? 8 : 5),
              SizedBox(
                width: double.infinity,
                child: ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(active: true), child: const Text('IN LINE')),
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
