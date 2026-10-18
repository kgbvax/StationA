import 'package:flutter/material.dart';
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

    testWidgets('caps visible list at 3 entries', (tester) async {
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

      expect(find.textContaining('FAULT '), findsNWidgets(3));
    });

    group('PSU root-cause line', () {
      testWidgets('names the PSU when it is confirmed off and the HF chain is dead', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setPower(psu: false);
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

    group('antenna power / standby', () {
      void seedDeadAntennaSide(BusStore store) {
        store.setDeviceOffline('muehle/hf/ant-ctrl');
        store.setBridgeOffline('muehle/hf/rotator');
        store.setBridgeOffline('muehle/uhf/az-rotator');
        store.setDeviceOffline('muehle/uhf/el-rotator');
      }

      testWidgets('antennas plug confirmed off: one info line, no rotator faults', (tester) async {
        final store = BusStore();
        store.setPower();
        store.setOnline('muehle/power/antennas');
        store.applyState('muehle/power/antennas', {'power': 'off', 'device_online': true});
        seedDeadAntennaSide(store);

        await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar(expanded: true)));
        await tester.pumpAndSettle();

        expect(find.text('ALL OK'), findsOneWidget);
        expect(find.textContaining('ANT PWR OFF — rotators/Ultrabeam unpowered'), findsOneWidget);
        expect(find.textContaining('muehle/hf/ant-ctrl'), findsNothing);
        expect(find.textContaining('muehle/hf/rotator'), findsNothing);
        expect(find.textContaining('muehle/uhf/az-rotator'), findsNothing);
        expect(find.textContaining('muehle/uhf/el-rotator'), findsNothing);
      });

      testWidgets('power-seq standby suppresses them even without the plug state', (tester) async {
        final store = BusStore();
        store.setPower();
        store.applyState('muehle/hf/power-seq', {'phase': 'standby', 'step': '', 'device_online': true});
        seedDeadAntennaSide(store);

        await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar(expanded: true)));
        await tester.pumpAndSettle();

        expect(find.text('ALL OK'), findsOneWidget);
        expect(find.textContaining('STANDBY — rotators/Ultrabeam unpowered'), findsOneWidget);
      });

      testWidgets('an unpowered Ultrabeam error is greyed, not active', (tester) async {
        final store = BusStore();
        store.setPower();
        store.applyState('muehle/hf/power-seq', {'phase': 'standby', 'step': '', 'device_online': true});
        store.applyStatus('muehle/hf/ant-ctrl', 'online');
        store.applyState('muehle/hf/ant-ctrl', {'device_online': false, 'error': 'no reply'});

        await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar(expanded: true)));
        await tester.pumpAndSettle();

        expect(find.text('ALL OK'), findsOneWidget);
        expect(find.textContaining('NO REPLY'), findsOneWidget);
      });

      testWidgets('antennas plug on: rotator outages are real faults', (tester) async {
        final store = BusStore();
        store.setPower();
        store.setOnline('muehle/power/antennas');
        store.applyState('muehle/power/antennas', {'power': 'on', 'device_online': true});
        store.setBridgeOffline('muehle/hf/rotator');

        await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar()));
        await tester.pumpAndSettle();

        expect(find.text('1 ACTIVE'), findsOneWidget);
        expect(find.textContaining('muehle/hf/rotator: bridge down'), findsOneWidget);
        expect(find.textContaining('unpowered'), findsNothing);
      });

      testWidgets('a dead plug bridge with a stale off is unknown, not off', (tester) async {
        final store = BusStore();
        store.setPower();
        store.applyStatus('muehle/power/antennas', '');
        store.applyState('muehle/power/antennas', {'power': 'off', 'device_online': true});
        store.setBridgeOffline('muehle/hf/rotator');

        await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar(expanded: true)));
        await tester.pumpAndSettle();

        expect(find.textContaining('muehle/hf/rotator: bridge down'), findsOneWidget);
        expect(find.textContaining('rotators/Ultrabeam unpowered'), findsNothing);
      });
    });

    testWidgets('a sleeping shack PC is not a fault', (tester) async {
      final store = BusStore();
      store.applyStatus('muehle/power/shack-pc', 'offline');
      store.applyState('muehle/power/shack-pc', {'power': 'awake', 'user_idle_s': 900, 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar()));
      await tester.pumpAndSettle();

      expect(find.text('ALL OK'), findsOneWidget);
      expect(find.textContaining('shack-pc'), findsNothing);
    });

    testWidgets('strip shows 3 rows; expanded shows every row', (tester) async {
      final store = BusStore();
      for (final a in ['muehle/hf/radio', 'muehle/hf/pa', 'muehle/hf/tuner', 'muehle/hf/rotator']) {
        store.setBridgeOffline(a);
      }
      await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar()));
      await tester.pump();
      expect(find.textContaining('bridge down'), findsNWidgets(3));

      await tester.pumpWidget(TestHarness(
        store: store,
        child: const SizedBox(height: 400, child: FaultsBar(expanded: true)),
      ));
      await tester.pump();
      expect(find.textContaining('bridge down'), findsNWidgets(4));
    });
  });
}
