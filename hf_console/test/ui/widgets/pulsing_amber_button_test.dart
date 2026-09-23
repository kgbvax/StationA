// pulsing_amber_button_test.dart — the pulse itself. TestHarness freezes
// animations globally so panel tests can pumpAndSettle; these tests opt
// back into motion with an explicit disableAnimations:false override to
// prove the engaged button actually oscillates.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/pulsing_amber_button.dart';
import '../../support/test_harness.dart';

void main() {
  // TestHarness freezes animations; this Builder re-allows them so the
  // pulse's AnimationController actually runs.
  Widget motionAllowed(Widget child) => Builder(
        builder: (context) => MediaQuery(
          data: MediaQuery.of(context).copyWith(disableAnimations: false),
          child: child,
        ),
      );

  Color? bgOf(WidgetTester tester, String label) =>
      tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, label))
          .style!.backgroundColor!.resolve({});

  testWidgets('engaged pulses amber when motion is allowed', (tester) async {
    await tester.pumpWidget(TestHarness(
      child: motionAllowed(
        PulsingAmberButton(engaged: true, onPressed: () {}, child: const Text('PULSE')),
      ),
    ));
    // 450 ms into the 1800 ms cycle → controller at t=0.25, clearly off
    // the frozen solid-amber value.
    await tester.pump(const Duration(milliseconds: 450));

    final dim = Color.lerp(AppTheme.amber, AppTheme.pane, 0.45)!;
    final expected = Color.lerp(dim, AppTheme.amber, Curves.easeInOut.transform(0.25))!;
    expect(bgOf(tester, 'PULSE'), expected);
  });

  testWidgets('engaged holds solid amber under reduce-motion', (tester) async {
    await tester.pumpWidget(TestHarness(
      child: PulsingAmberButton(engaged: true, onPressed: () {}, child: const Text('PULSE')),
    ));
    await tester.pumpAndSettle();

    expect(bgOf(tester, 'PULSE'), AppTheme.amber);
  });

  testWidgets('not engaged renders the idle style', (tester) async {
    await tester.pumpWidget(TestHarness(
      child: PulsingAmberButton(
        engaged: false,
        idleStyle: AppTheme.actionButton(amber: true),
        onPressed: () {},
        child: const Text('PULSE'),
      ),
    ));
    await tester.pumpAndSettle();

    expect(bgOf(tester, 'PULSE'), AppTheme.blend(AppTheme.amber, 0.12));
  });
}
