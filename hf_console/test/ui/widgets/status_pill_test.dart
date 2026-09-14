import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/status_pill.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

Color pillColor(WidgetTester tester, String text) {
  final container = tester.widget<Container>(
    find.ancestor(of: find.textContaining(text), matching: find.byType(Container)).first,
  );
  final deco = container.decoration as BoxDecoration;
  return (deco.border as Border).top.color;
}

void main() {
  group('StatusPill', () {
    testWidgets('green device name from /meta when online', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline('muehle/hf/pa');
      store.applyMeta('muehle/hf/pa', {
        'device': {'model': 'ACOM 1200S'},
        'expose': {
          'device': {'name': 'ACOM 1200S'},
        },
      });

      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: const StatusPill(slots: ['muehle/hf/pa'], label: 'fallback'),
      ));
      await tester.pump();

      expect(find.text('ACOM 1200S'), findsOneWidget);
      expect(pillColor(tester, 'ACOM'), AppTheme.green);
    });

    testWidgets('falls back to the label without /meta', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline('muehle/hf/pa');

      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: const StatusPill(slots: ['muehle/hf/pa'], label: 'ACOM 1200S'),
      ));
      await tester.pump();

      expect(find.text('ACOM 1200S'), findsOneWidget);
    });

    testWidgets('red name + OFFLINE when the slot is down', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline('muehle/hf/pa');
      store.setBridgeOffline('muehle/hf/pa');

      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: const StatusPill(slots: ['muehle/hf/pa'], label: 'ACOM 1200S'),
      ));
      await tester.pump();

      expect(find.text('ACOM 1200S · OFFLINE'), findsOneWidget);
      expect(pillColor(tester, 'OFFLINE'), AppTheme.red);
    });

    testWidgets('worst case across slots: one down turns the pill red', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline('muehle/hf/ant-switch');
      store.setOnline('muehle/hf/antenna-select');
      store.setBridgeOffline('muehle/hf/antenna-select');

      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: const StatusPill(
          slots: ['muehle/hf/ant-switch', 'muehle/hf/antenna-select'],
          label: 'Ant switch',
          useMetaName: false,
        ),
      ));
      await tester.pump();

      expect(find.text('Ant switch · OFFLINE'), findsOneWidget);
      expect(pillColor(tester, 'OFFLINE'), AppTheme.red);
    });

    testWidgets('irregular state renders name + suffix in its severity colour', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline('muehle/hf/pa');

      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: StatusPill(
          slots: const ['muehle/hf/pa'],
          label: 'ACOM 1200S',
          suffix: 'TX',
          suffixColor: AppTheme.red,
        ),
      ));
      await tester.pump();

      expect(find.text('ACOM 1200S · TX'), findsOneWidget);
      expect(pillColor(tester, 'TX'), AppTheme.red);
    });

    testWidgets('every tracked slot must be present — missing counts as offline', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // No state at all for either slot (pre-first-message silence).

      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: const StatusPill(slots: ['muehle/hf/pa'], label: 'ACOM 1200S'),
      ));
      await tester.pump();

      expect(find.textContaining('OFFLINE'), findsOneWidget);
    });
  });
}
