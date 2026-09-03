import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('CompassPanel', () {
    testWidgets('shows OFFLINE when rotator bridge is down', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('OFFLINE'), findsOneWidget);
    });

    testWidgets('shows current azimuth and hides target when within 5°', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0, targetAz: 122.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('120°'), findsOneWidget);
      expect(find.textContaining('→'), findsNothing);
    });

    testWidgets('shows target azimuth when more than 5° away', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0, targetAz: 200.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('120°'), findsOneWidget);
      expect(find.textContaining('→ 200°'), findsOneWidget);
    });

    // Preset-button coverage (NA / SA / VK / JA / STOP) lives in
    // `rotator_presets_bar_test.dart` — the rail overlay the compass card
    // carries on its right edge (above the zoom stepper) plus the phone
    // layout's horizontal bar. The compass card itself owns tap-to-aim +
    // the +/- zoom stepper here.
  });
}
