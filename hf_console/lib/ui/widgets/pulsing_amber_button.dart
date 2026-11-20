import 'package:flutter/material.dart';

import '../theme.dart';
import 'stepped_pulse.dart';

/// Amber action button that pulses slowly while engaged.
///
/// The highlight for irregular-but-deliberate states that are NOT errors —
/// 180° on the Ultrabeam, MANUAL antenna routing, PA STANDBY: noticeable
/// without spending the red reserved for faults. Idle it renders the plain
/// inactive action-button chrome (or [idleStyle] when a panel wants e.g. the
/// dim amber tint).
///
/// Honors reduce-motion: holds solid amber instead of oscillating. A disabled
/// button (no [onPressed]: bridge offline, link down, antenna lock) holds
/// solid amber without pulsing: there is nothing to act on, and an offline PA
/// reads as standby, so it would otherwise pulse through every night-long
/// power-down.
///
/// The pulse runs as long as the state lasts, so it is a [SteppedPulse]
/// (about 11 frames per second, 8 after ten seconds), not a vsync
/// AnimationController (a full frame at the display rate, about 30 % CPU on
/// the idle web console). Widget tests freeze it (TestHarness does) or use plain `pump`s — `pumpAndSettle`
/// would never return.
class PulsingAmberButton extends StatefulWidget {
  const PulsingAmberButton({
    super.key,
    required this.engaged,
    this.idleStyle,
    this.style,
    this.buttonKey,
    this.onPressed,
    required this.child,
  });

  /// The irregular state is present — render amber and pulse.
  final bool engaged;

  /// Style when not engaged; defaults to the plain inactive action button.
  final ButtonStyle? idleStyle;

  /// Layout overrides (size, padding) laid over both states; the pulse colour
  /// still wins.
  final ButtonStyle? style;

  /// Key for the inner [ElevatedButton], so tests can find it.
  final Key? buttonKey;

  final VoidCallback? onPressed;
  final Widget child;

  @override
  State<PulsingAmberButton> createState() => _PulsingAmberButtonState();
}

class _PulsingAmberButtonState extends State<PulsingAmberButton> {
  final SteppedPulse _pulse = SteppedPulse(halfPeriod: const Duration(milliseconds: 1440));

  /// Bottom of the pulse: amber blended mostly toward the pane colour.
  static Color get _amberDim => Color.lerp(AppTheme.amber, AppTheme.pane, 0.55)!;

  @override
  void dispose() {
    _pulse.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final pulsing = widget.engaged && widget.onPressed != null && !MediaQuery.disableAnimationsOf(context);
    if (pulsing) {
      _pulse.start();
    } else {
      _pulse.stop();
    }

    // Own layer: a pulse step repaints this button, not the whole panel.
    return RepaintBoundary(
      child: AnimatedBuilder(
      animation: _pulse,
      builder: (context, _) {
        ButtonStyle style = widget.engaged
            ? AppTheme.actionButton(amberActive: true)
            : (widget.idleStyle ?? AppTheme.actionButton());
        if (widget.style != null) style = widget.style!.merge(style);
        if (widget.engaged) {
          // Overrides the disabled background too: a disabled engaged key
          // (180° while the elements move, under a lock) must still read amber.
          style = style.copyWith(
            backgroundColor: WidgetStatePropertyAll(
              pulsing ? Color.lerp(_amberDim, AppTheme.amber, _pulse.value)! : AppTheme.amber,
            ),
          );
        }
        return ElevatedButton(
          key: widget.buttonKey,
          onPressed: widget.onPressed,
          style: style,
          child: widget.child,
        );
      },
      ),
    );
  }
}
