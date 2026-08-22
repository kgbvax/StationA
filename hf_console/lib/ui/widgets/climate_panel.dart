import 'package:flutter/material.dart';
import '../theme.dart';

class ClimatePanel extends StatelessWidget {
  const ClimatePanel({super.key});

  @override
  Widget build(BuildContext context) {
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border.all(color: AppTheme.cardLine),
        borderRadius: BorderRadius.circular(6),
      ),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      child: Row(
        mainAxisAlignment: MainAxisAlignment.spaceAround,
        children: [
          Column(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              Row(
                children: [
                  _toggle('HEAT', true),
                  const SizedBox(width: 12),
                  _toggle('COOL', false),
                ],
              ),
              const SizedBox(height: 10),
              Row(
                children: [
                  Text('21.4', style: AppTheme.mono(18, weight: FontWeight.w700)),
                  Text('°C', style: AppTheme.mono(11, color: AppTheme.txtFaint)),
                  const SizedBox(width: 14),
                  Text('612', style: AppTheme.mono(18, weight: FontWeight.w700)),
                  Text('ppm', style: AppTheme.mono(11, color: AppTheme.txtFaint)),
                ],
              ),
            ],
          ),
        ],
      ),
    );
  }

  Widget _toggle(String label, bool on) {
    return Row(
      children: [
        Text(label, style: AppTheme.mono(11, color: AppTheme.txtMute, weight: FontWeight.w700, letterSpacing: 0.08)),
        const SizedBox(width: 6),
        Container(
          width: 34,
          height: 18,
          decoration: BoxDecoration(
            color: on ? AppTheme.blend(AppTheme.green, 0.20) : AppTheme.cardLine,
            borderRadius: BorderRadius.circular(9),
          ),
          child: AnimatedAlign(
            alignment: on ? Alignment.centerRight : Alignment.centerLeft,
            duration: const Duration(milliseconds: 120),
            child: Padding(
              padding: const EdgeInsets.all(2),
              child: Container(
                width: 14,
                height: 14,
                decoration: BoxDecoration(
                  color: on ? AppTheme.green : AppTheme.txtMute,
                  borderRadius: BorderRadius.circular(7),
                ),
              ),
            ),
          ),
        ),
      ],
    );
  }
}
