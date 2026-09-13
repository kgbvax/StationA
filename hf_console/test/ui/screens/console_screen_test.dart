import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('ConsoleScreen layout', () {
    testWidgets('HF page renders all main modules at 1920x1200', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();
      store.setPaHealthy();
      store.setTuner();
      store.setRotator(az: 120.0);
      store.setUltrabeam();
      store.setAntenna();
      store.setRadio();

      await tester.binding.setSurfaceSize(const Size(1920, 1200));
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
      // The antenna panel has a continuously repeating pending-dot animation,
      // so pumpAndSettle would time out. Pump a fixed frame instead.
      await tester.pump(const Duration(milliseconds: 100));

      // Module titles visible in the HF layout (CardHeader renders uppercase).
      expect(find.textContaining('PA · ACOM 1200S'), findsWidgets);
      expect(find.textContaining('TUNER · ATR-1000'), findsWidgets);
      expect(find.textContaining('TRX · FLEX-8400'), findsWidgets);
      expect(find.textContaining('ULTRABEAM'), findsWidgets);
      expect(find.textContaining('ROUTING'), findsWidgets);
      // Faults bar is shown on the HF page.
      expect(find.text('FAULTS'), findsOneWidget);
    });

    testWidgets('station page renders power and climate modules', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();

      await tester.binding.setSurfaceSize(const Size(1920, 1200));
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
      await tester.pump(const Duration(milliseconds: 100));

      // Open station page via the top bar.
      await tester.tap(find.text('Station'));
      await tester.pump(const Duration(milliseconds: 100));

      expect(find.text('STOP\nSTATION'), findsOneWidget);
      expect(find.text('MAINS'), findsOneWidget);
    });

    testWidgets('UHF page renders the sat rotator panel (tablet layout)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 45, target: 90);
      store.setSatRotator('muehle/uhf/el-rotator', axis: 'el', pos: 10);

      // setSurfaceSize is in physical pixels (test dpr 3.0): 3600x2400 →
      // logical 1200x800, shortestSide 800 → the tablet branch.
      await tester.binding.setSurfaceSize(const Size(3600, 2400));
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
      await tester.pump(const Duration(milliseconds: 100));

      await tester.tap(find.text('UHF'));
      await tester.pump(const Duration(milliseconds: 100));

      expect(find.textContaining('SAT ROTATORS'), findsOneWidget);
      expect(find.text('AZIMUTH'), findsOneWidget);
      expect(find.text('ELEVATION'), findsOneWidget);
      expect(find.byKey(const ValueKey('sat-stop')), findsOneWidget);
      // The placeholder is gone.
      expect(find.text('UHF controls are not yet wired.'), findsNothing);
    });

    testWidgets('UHF page renders the sat rotator panel (phone layout)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 45);
      store.setSatRotator('muehle/uhf/el-rotator', axis: 'el', pos: 10);

      // 1650x2640 physical → logical 550x880, shortestSide 550 < 600 → the
      // phone reflow branch. (Real-iPhone widths are not usable here: below
      // roughly 450 logical the HF page — pumped before the UHF tab can be
      // opened — trips a pre-existing DVK-panel overflow unrelated to U8.)
      await tester.binding.setSurfaceSize(const Size(1650, 2640));
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
      await tester.pump(const Duration(milliseconds: 100));

      await tester.tap(find.text('UHF'));
      await tester.pump(const Duration(milliseconds: 100));

      expect(find.textContaining('SAT ROTATORS'), findsOneWidget);
      expect(find.byKey(const ValueKey('sat-stop')), findsOneWidget);
    });
  });
}
