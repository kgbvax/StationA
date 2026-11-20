// stepped_pulse_test.dart — the timer-sampled amber swing: fast sampling
// while the state is new, 8 fps once it has settled, phase continuous.

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/ui/widgets/stepped_pulse.dart';

void main() {
  testWidgets('samples every 90 ms, then every 125 ms after 10 s', (tester) async {
    final pulse = SteppedPulse(halfPeriod: const Duration(milliseconds: 1440));
    var notifies = 0;
    pulse.addListener(() => notifies++);
    pulse.start();
    expect(pulse.currentStep, const Duration(milliseconds: 90));

    await tester.pump(const Duration(seconds: 1));
    expect(notifies, 11); // 1000 ms / 90 ms

    await tester.pump(const Duration(seconds: 9, milliseconds: 90));
    expect(pulse.currentStep, const Duration(milliseconds: 125));

    notifies = 0;
    await tester.pump(const Duration(seconds: 2));
    expect(notifies, 16); // 8 per second

    pulse.dispose();
  });

  testWidgets('phase carries over the rate change', (tester) async {
    final pulse = SteppedPulse(halfPeriod: const Duration(milliseconds: 1440));
    pulse.start();
    final seen = <double>[];
    pulse.addListener(() => seen.add(pulse.value));
    await tester.pump(const Duration(seconds: 14));
    // Neighbouring samples stay close: no jump at the switch near 10 s.
    for (var i = 1; i < seen.length; i++) {
      expect((seen[i] - seen[i - 1]).abs(), lessThan(0.2));
    }
    expect(seen.reduce((a, b) => a < b ? a : b), lessThan(0.05));
    expect(seen.reduce((a, b) => a > b ? a : b), greaterThan(0.95));
    pulse.dispose();
  });

  testWidgets('stop holds the given value and restarts fast from the dim end', (tester) async {
    final pulse = SteppedPulse(halfPeriod: const Duration(milliseconds: 1440));
    pulse.start();
    await tester.pump(const Duration(seconds: 11));
    expect(pulse.currentStep, const Duration(milliseconds: 125));
    pulse.stop();
    expect(pulse.value, 1.0);
    expect(pulse.isRunning, isFalse);
    pulse.start();
    expect(pulse.value, 0.0);
    expect(pulse.currentStep, const Duration(milliseconds: 90));
    pulse.dispose();
  });
}
