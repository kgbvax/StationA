import 'package:flutter/material.dart';
import '../theme.dart';
import 'card_container.dart';

class ClimatePanel extends StatelessWidget {
  const ClimatePanel({super.key});

  @override
  Widget build(BuildContext context) {
    return CardContainer(
      child: Row(
        mainAxisAlignment: MainAxisAlignment.spaceAround,
        children: [
          Column(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              Row(
                children: [
                  _toggle('HEAT', false),
                  const SizedBox(width: 10),
                  _toggle('COOL', false),
                ],
              ),
              const SizedBox(height: 8),
              Row(
                children: [
                  Text('21.4', style: AppTheme.mono(14)),
                  Text('°C', style: AppTheme.mono(10, color: AppTheme.txtMute)),
                  const SizedBox(width: 10),
                  Text('612', style: AppTheme.mono(14)),
                  Text('ppm', style: AppTheme.mono(10, color: AppTheme.txtMute)),
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
        Text(label, style: AppTheme.mono(10, color: AppTheme.txtMute, weight: FontWeight.w600, letterSpacing: 0.08)),
        const SizedBox(width: 6),
        Container(
          width: 30,
          height: 16,
          decoration: BoxDecoration(
            color: on ? const Color(0x4036B37E) : AppTheme.cardLine,
            borderRadius: BorderRadius.circular(8),
          ),
          child: AnimatedAlign(
            alignment: on ? Alignment.centerRight : Alignment.centerLeft,
            duration: const Duration(milliseconds: 120),
            child: Padding(
              padding: const EdgeInsets.all(2),
              child: Container(
                width: 12,
                height: 12,
                decoration: BoxDecoration(
                  color: on ? AppTheme.green : AppTheme.txtMute,
                  borderRadius: BorderRadius.circular(6),
                ),
              ),
            ),
          ),
        ),
      ],
    );
  }
}
