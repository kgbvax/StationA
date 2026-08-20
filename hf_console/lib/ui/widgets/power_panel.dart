import 'package:flutter/material.dart';
import '../theme.dart';
import 'card_container.dart';

class PowerPanel extends StatelessWidget {
  const PowerPanel({super.key});

  @override
  Widget build(BuildContext context) {
    return CardContainer(
      power: true,
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.center,
        children: [
          ElevatedButton(
            onPressed: () {},
            style: AppTheme.actionButton(danger: true).copyWith(
              minimumSize: const WidgetStatePropertyAll(Size(76, 44)),
              padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 10, vertical: 10)),
            ),
            child: const Text('STOP\nSTATION'),
          ),
          const SizedBox(width: 10),
          Expanded(
            child: SingleChildScrollView(
              scrollDirection: Axis.horizontal,
              child: Row(
                children: const [
                  _Relay('MAINS', true),
                  SizedBox(width: 5),
                  _Relay('PSU 13.8V', true),
                  SizedBox(width: 5),
                  _Relay('TRX', true),
                  SizedBox(width: 5),
                  _Relay('PA', true),
                ],
              ),
            ),
          ),
          const SizedBox(width: 8),
          Text('pa-arm-enable', style: AppTheme.mono(9, color: AppTheme.cyan)),
        ],
      ),
    );
  }
}

class _Relay extends StatelessWidget {
  final String name;
  final bool on;

  const _Relay(this.name, this.on);

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 5),
      decoration: BoxDecoration(
        color: const Color(0xFF0E1216),
        border: Border.all(color: AppTheme.cardLine),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(name, style: AppTheme.mono(9, weight: FontWeight.w600, letterSpacing: 0.06)),
          const SizedBox(width: 6),
          Container(
            width: 28,
            height: 15,
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
                  width: 11,
                  height: 11,
                  decoration: BoxDecoration(
                    color: on ? AppTheme.green : AppTheme.txtMute,
                    borderRadius: BorderRadius.circular(6),
                  ),
                ),
              ),
            ),
          ),
          const SizedBox(width: 6),
          Text(on ? 'ON' : 'OFF', style: AppTheme.mono(9, color: on ? AppTheme.green : AppTheme.txtFaint, weight: FontWeight.w700)),
        ],
      ),
    );
  }
}
