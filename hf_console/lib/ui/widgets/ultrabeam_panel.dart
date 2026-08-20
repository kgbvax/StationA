import 'package:flutter/material.dart';
import '../theme.dart';
import 'card_container.dart';

class UltrabeamPanel extends StatelessWidget {
  const UltrabeamPanel({super.key});

  @override
  Widget build(BuildContext context) {
    return CardContainer(
      child: Row(
        children: [
          Text('ULTRABEAM'.toUpperCase(), style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.12)),
          const SizedBox(width: 14),
          ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(), child: const Text('FORWARD')),
          const SizedBox(width: 5),
          ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(amber: true), child: const Text('180°')),
          const SizedBox(width: 5),
          ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(), child: const Text('BIDIRECTIONAL')),
          const SizedBox(width: 8),
          ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(danger: true), child: const Text('RETRACT')),
          const Spacer(),
          Text('20M · 14.074.000 HZ', style: AppTheme.mono(10, color: AppTheme.txtFaint)),
        ],
      ),
    );
  }
}
