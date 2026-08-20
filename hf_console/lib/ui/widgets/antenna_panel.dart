import 'package:flutter/material.dart';
import '../theme.dart';
import 'card_container.dart';

class AntennaPanel extends StatelessWidget {
  const AntennaPanel({super.key});

  @override
  Widget build(BuildContext context) {
    const ports = ['OFF', 'DUMMY', 'P2', 'P3', 'ULTRABEAM', 'P5', 'FAN-DIPOLE'];
    const active = 'ULTRABEAM';

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              Text('ANTENNA SELECT'.toUpperCase(), style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.12)),
              Row(
                children: [
                  Text('selected: ', style: AppTheme.body(12, color: AppTheme.txtMute)),
                  Text('Ultrabeam (P4)', style: AppTheme.body(12, weight: FontWeight.w700)),
                ],
              ),
            ],
          ),
          const SizedBox(height: 8),
          Row(
            children: [
              Wrap(
                spacing: 4,
                runSpacing: 4,
                children: ports.map((p) {
                  final isActive = p == active;
                  return ElevatedButton(
                    onPressed: () {},
                    style: AppTheme.actionButton(active: isActive),
                    child: Text(p),
                  );
                }).toList(),
              ),
              const SizedBox(width: 28),
              Row(
                children: [
                  ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(active: true), child: const Text('AUTO')),
                  const SizedBox(width: 5),
                  ElevatedButton(onPressed: () {}, style: AppTheme.actionButton(), child: const Text('MANUAL')),
                ],
              ),
            ],
          ),
        ],
      ),
    );
  }
}
