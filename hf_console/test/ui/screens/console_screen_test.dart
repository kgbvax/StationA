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
  });
}
