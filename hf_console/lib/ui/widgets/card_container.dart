import 'package:flutter/material.dart';
import '../theme.dart';

class CardContainer extends StatelessWidget {
  final Widget child;
  final bool power;
  final EdgeInsets padding;

  const CardContainer({
    super.key,
    required this.child,
    this.power = false,
    this.padding = const EdgeInsets.all(10),
  });

  @override
  Widget build(BuildContext context) {
    return Container(
      decoration: AppTheme.cardDecoration(power: power),
      padding: padding,
      child: child,
    );
  }
}

class CardHeader extends StatelessWidget {
  final String title;
  final Widget? trailing;

  const CardHeader({super.key, required this.title, this.trailing});

  @override
  Widget build(BuildContext context) {
    return Row(
      mainAxisAlignment: MainAxisAlignment.spaceBetween,
      children: [
        Text(title.toUpperCase(), style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.12)),
        if (trailing != null) trailing!,
      ],
    );
  }
}
