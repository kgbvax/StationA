import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/power_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('PowerPanel', () {
    testWidgets('no SEQUENCE label; an idle sequencer shows no phase word', (tester) async {
      final store = BusStore();
      store.setPower();
      store.applyState('muehle/hf/power-seq', {'phase': 'idle', 'fault': '', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: FakeMqttService(store), child: const PowerPanel()));
      await tester.pumpAndSettle();

      expect(find.text('SEQUENCE'), findsNothing);
      expect(find.text('IDLE'), findsNothing);
      expect(find.widgetWithText(ElevatedButton, 'START'), findsOneWidget);
      expect(find.widgetWithText(ElevatedButton, 'STOP'), findsOneWidget);

      store.applyState('muehle/hf/power-seq', {'phase': 'running', 'fault': '', 'device_online': true});
      await tester.pumpAndSettle();
      expect(find.text('ON'), findsOneWidget); // the phase word, not a relay chip
    });

    testWidgets('relay chips carry no ON/OFF words, only OFFLINE when out of reach', (tester) async {
      final store = BusStore();
      store.setPower(psu: true, trx: false, pa: true);
      store.applyState('muehle/hf/power-seq', {'phase': 'idle', 'fault': '', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: FakeMqttService(store), child: const PowerPanel()));
      await tester.pumpAndSettle();

      expect(find.text('ON'), findsNothing);
      expect(find.text('OFF'), findsNothing);
      // ANT PWR and PC PWR plugs were never heard: their chips read OFFLINE,
      // as does the shack-PC agent's indicator.
      expect(find.text('OFFLINE'), findsNWidgets(3));
    });

    testWidgets('shows relay states and publishes toggle commands', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower(psu: false, trx: true, pa: false);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      expect(find.text('MAINS'), findsNothing); // site mains is out of scope
      expect(find.text('PSU 13.8V'), findsOneWidget);
      expect(find.text('TRX'), findsOneWidget);
      expect(find.text('PA'), findsOneWidget);

      await tester.tap(find.widgetWithText(ElevatedButton, 'STOP'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/power-seq/cmd');
      expect(mqtt.publishes.first.payload, contains('stop'));
    });

    testWidgets('START STATION is disabled while the sequence runs', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower(); // seeds power-seq phase 'running'

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      final start = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'START'));
      expect(start.onPressed, isNull);
      await tester.tap(find.widgetWithText(ElevatedButton, 'START'), warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('START STATION publishes the start cmd when idle', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();
      store.applyState('muehle/hf/power-seq', {'phase': 'idle', 'fault': '', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      final start = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'START'));
      expect(start.onPressed, isNotNull);
      await tester.tap(find.widgetWithText(ElevatedButton, 'START'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/power-seq/cmd');
      expect(mqtt.publishes.first.payload, contains('start'));
    });

    testWidgets('shows the current step while the sequence starts', (tester) async {
      final store = BusStore();
      store.setPower();
      store.applyState('muehle/hf/power-seq', {'phase': 'starting', 'step': 'psu-on', 'device_online': true});
      await tester.pumpWidget(TestHarness(store: store, child: const PowerPanel()));
      await tester.pump(const Duration(milliseconds: 100));

      expect(find.text('STARTING'), findsOneWidget);
      expect(find.byKey(const ValueKey('seq-step')), findsOneWidget);
      expect(find.text('psu-on'), findsOneWidget);
    });

    testWidgets('ANT PWR toggle publishes a retained set_power', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();
      store.setOnline('muehle/power/antennas');
      store.applyState('muehle/power/antennas', {'power': 'on', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.text('ANT PWR'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/power/antennas/cmd');
      expect(mqtt.publishes.first.payload, contains('"value":"off"'));
      expect(mqtt.publishes.first.retain, isTrue);
    });

    testWidgets('ANT PWR is inert while its plug bridge is offline', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower(); // antennas slot never seen

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.text('ANT PWR'), warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('SHACK PC reads AWAKE / ASLEEP from the agent status', (tester) async {
      final store = BusStore();
      store.setPower();
      store.setOnline('muehle/power/shack-pc');
      store.applyState('muehle/power/shack-pc', {'power': 'awake', 'user_idle_s': 12, 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, child: const PowerPanel()));
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.text('SHACK PC'), findsOneWidget);
      expect(find.text('AWAKE'), findsOneWidget);

      store.applyStatus('muehle/power/shack-pc', 'offline');
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.text('ASLEEP'), findsOneWidget);
    });

    testWidgets('PC PWR on publishes a retained set_power at once', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();
      store.setOnline('muehle/power/shack-pc-mains');
      store.applyState('muehle/power/shack-pc-mains', {'power': 'off', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();
      expect(find.text('NO POWER'), findsOneWidget);

      await tester.tap(find.text('PC PWR'));
      await tester.pumpAndSettle();

      expect(find.byType(AlertDialog), findsNothing);
      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/power/shack-pc-mains/cmd');
      expect(mqtt.publishes.first.payload, contains('"value":"on"'));
      expect(mqtt.publishes.first.retain, isTrue);
    });

    testWidgets('PC PWR off asks first; cancel publishes nothing', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();
      store.setOnline('muehle/power/shack-pc-mains');
      store.applyState('muehle/power/shack-pc-mains', {'power': 'on', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.text('PC PWR'));
      await tester.pumpAndSettle();
      expect(find.byType(AlertDialog), findsOneWidget);
      await tester.tap(find.text('Cancel'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);

      await tester.tap(find.text('PC PWR'));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('pc-mains-off-confirm')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/power/shack-pc-mains/cmd');
      expect(mqtt.publishes.first.payload, contains('"value":"off"'));
    });

    testWidgets('SHACK PC shows the agent error text', (tester) async {
      final store = BusStore();
      store.setPower();
      store.setOnline('muehle/power/shack-pc');
      store.applyState('muehle/power/shack-pc',
          {'power': 'awake', 'user_idle_s': 0, 'device_online': true, 'error': 'sleep blocked'});

      await tester.pumpWidget(TestHarness(store: store, child: const PowerPanel()));
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.byKey(const ValueKey('shack-pc-error')), findsOneWidget);
      expect(find.text('sleep blocked'), findsOneWidget);
    });

    testWidgets('standby: amber STANDBY, START enabled (wake)', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();
      store.applyState('muehle/hf/power-seq', {'phase': 'standby', 'step': '', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      final label = tester.widget<Text>(find.text('STANDBY'));
      expect(label.style?.color, AppTheme.amber);
      expect(find.byKey(const ValueKey('seq-step')), findsNothing);

      await tester.tap(find.widgetWithText(ElevatedButton, 'START'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, contains('start'));
    });

    for (final (phase, label) in [('entering_standby', 'ENTER STANDBY'), ('waking', 'WAKING')]) {
      testWidgets('$phase: amber $label with step, START disabled, STOP enabled', (tester) async {
        final store = BusStore();
        store.setPower();
        store.applyState('muehle/hf/power-seq', {'phase': phase, 'step': 'ant-plug', 'device_online': true});

        await tester.pumpWidget(TestHarness(store: store, child: const PowerPanel()));
        await tester.pump(const Duration(milliseconds: 100));

        expect(tester.widget<Text>(find.text(label)).style?.color, AppTheme.amber);
        expect(find.text('ant-plug'), findsOneWidget);
        final start = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'START'));
        expect(start.onPressed, isNull);
        final stop = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'STOP'));
        expect(stop.onPressed, isNotNull);
      });
    }

    testWidgets('no step line once running', (tester) async {
      final store = BusStore();
      store.setPower();
      store.applyState('muehle/hf/power-seq', {'phase': 'running', 'step': '', 'device_online': true});
      await tester.pumpWidget(TestHarness(store: store, child: const PowerPanel()));
      await tester.pump(const Duration(milliseconds: 100));

      expect(find.byKey(const ValueKey('seq-step')), findsNothing);
    });
  });
}
