import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/faults_bar.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('FaultsBar', () {
    testWidgets('shows all-ok placeholder when nothing is wrong', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
      await tester.pumpAndSettle();

      expect(find.text('ALL OK'), findsOneWidget);
      expect(find.text('No faults or offline devices'), findsOneWidget);
    });

    testWidgets('renders an active PA fault in red', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaFault(error: 'HOT SWITCHING ATTEMPT');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
      await tester.pumpAndSettle();

      expect(find.text('1 ACTIVE'), findsOneWidget);
      expect(find.textContaining('HOT SWITCHING ATTEMPT'), findsOneWidget);
    });

    testWidgets('renders offline warnings', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setBridgeOffline('muehle/hf/radio');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
      await tester.pumpAndSettle();

      expect(find.text('1 ACTIVE'), findsOneWidget);
      expect(find.textContaining('muehle/hf/radio: bridge down'), findsOneWidget);
    });

    testWidgets('active fault suppresses generic offline line for same address', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaFault();
      // PA bridge reports online, so offlineList has no PA entry anyway, but
      // force a bridge-down status on the same address to verify suppression.
      store.applyStatus('muehle/hf/pa', '');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
      await tester.pumpAndSettle();

      expect(find.textContaining('HOT SWITCHING ATTEMPT'), findsOneWidget);
      expect(find.textContaining('muehle/hf/pa: bridge down'), findsNothing);
    });

    testWidgets('caps visible list at 4 entries', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      for (var i = 0; i < 10; i++) {
        store.applyState('muehle/hf/pa', {
          'fault': 'other',
          'error': 'fault $i',
          'device_online': true,
          'ts': '2026-08-20T${14 + i ~/ 60}:${(i % 60).toString().padLeft(2, '0')}:00.000000',
        });
      }

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
      await tester.pumpAndSettle();

      expect(find.textContaining('FAULT '), findsNWidgets(4));
    });

    group('PSU root-cause line', () {
      testWidgets('names the PSU when it is confirmed off and the HF chain is dead', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setPower(psu: false, master: true);
        store.setDeviceOffline('muehle/hf/tuner'); // the dead HF downstream

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
        await tester.pumpAndSettle();

        expect(find.textContaining('muehle/power/psu-13v8: PSU OFF'), findsOneWidget);
      });

      testWidgets('does not name the PSU while it is on', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setPower(psu: true);
        store.setDeviceOffline('muehle/hf/tuner');

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
        await tester.pumpAndSettle();

        expect(find.textContaining('PSU OFF'), findsNothing);
      });

      testWidgets('does not manufacture the line from a missing power key', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        // PSU bridge online but its state carries no 'power' key: unknown,
        // not off — inferring otherwise fabricates a root cause.
        store.setOnline('muehle/power/psu-13v8');
        store.applyState('muehle/power/psu-13v8', {'device_online': true});
        store.setDeviceOffline('muehle/hf/tuner');

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
        await tester.pumpAndSettle();

        expect(find.textContaining('PSU OFF'), findsNothing);
      });

      testWidgets('does not name the PSU when the bridge itself is dead', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        // Stale retained 'off' + dead bridge: the console cannot know the
        // PSU state, so it must not assert the cause.
        store.applyStatus('muehle/power/psu-13v8', '');
        store.applyState('muehle/power/psu-13v8', {'power': 'off', 'device_online': true});
        store.setDeviceOffline('muehle/hf/tuner');

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const FaultsBar()));
        await tester.pumpAndSettle();

        expect(find.textContaining('PSU OFF'), findsNothing);
      });
    });
  });
}
