import 'package:flutter/material.dart';

import '../theme.dart';

/// Amber action button that pulses slowly while engaged.
///
/// The highlight for irregular-but-deliberate states that are NOT errors —
/// 180° on the Ultrabeam, MANUAL antenna routing, PA STANDBY: noticeable
/// without spending the red reserved for faults. Idle it renders the plain
/// inactive action-button chrome (or [idleStyle] when a panel wants e.g. the
/// dim amber tint).
///
/// Honors reduce-motion: holds solid amber instead of oscillating. The pulse
/// runs forever by design, so widget tests must freeze it (TestHarness does)
/// or use plain `pump`s — `pumpAndSettle` would never return.
class PulsingAmberButton extends StatefulWidget {
  const PulsingAmberButton({
    super.key,
    required this.engaged,
    this.idleStyle,
    this.onPressed,
    required this.child,
  });

  /// The irregular state is present — render amber and pulse.
  final bool engaged;

  /// Style when not engaged; defaults to the plain inactive action button.
  final ButtonStyle? idleStyle;

  final VoidCallback? onPressed;
  final Widget child;

  @override
  State<PulsingAmberButton> createState() => _PulsingAmberButtonState();
}

class _PulsingAmberButtonState extends State<PulsingAmberButton> with SingleTickerProviderStateMixin {
  late final AnimationController _pulse = AnimationController(
    vsync: this,
    duration: const Duration(milliseconds: 1440),
  );

  /// Bottom of the pulse: amber blended mostly toward the pane colour.
  static Color get _amberDim => Color.lerp(AppTheme.amber, AppTheme.pane, 0.55)!;

  @override
  void dispose() {
    _pulse.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final pulsing = widget.engaged && !MediaQuery.disableAnimationsOf(context);
    if (pulsing) {
      if (!_pulse.isAnimating) _pulse.repeat(reverse: true);
    } else {
      _pulse.stop();
      _pulse.value = 1.0;
    }

    return AnimatedBuilder(
      animation: _pulse,
      builder: (context, _) {
        ButtonStyle style = widget.engaged
            ? AppTheme.actionButton(amberActive: true)
            : (widget.idleStyle ?? AppTheme.actionButton());
        if (pulsing) {
          final t = Curves.easeInOut.transform(_pulse.value);
          style = style.copyWith(
            backgroundColor: WidgetStatePropertyAll(
              Color.lerp(_amberDim, AppTheme.amber, t)!,
            ),
          );
        }
        return ElevatedButton(
          onPressed: widget.onPressed,
          style: style,
          child: widget.child,
        );
      },
    );
  }
}
