import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/faults_bar.dart';
import 'package:hf_console/ui/widgets/pa_arm_row.dart';
import 'package:hf_console/ui/widgets/pa_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('paArmAnnunciator', () {
    (String, Color) tag({bool online = true, bool? enabled = true, bool? armed = false, String block = ''}) =>
        paArmAnnunciator(online: online, enabled: enabled, armed: armed, block: block);

    test('offline wins over any retained state', () {
      expect(tag(online: false, armed: true), ('OFFLINE', AppTheme.txtMute));
    });

    test('armed and clear is green', () {
      expect(tag(armed: true), ('ARMED', AppTheme.green));
    });

    test('armed with a pending readiness gate is amber', () {
      expect(tag(armed: true, block: 'pa offline'), ('ARMED · PA OFFLINE', AppTheme.amber));
    });

    test('permit off is grey even when a gate also blocks', () {
      expect(tag(enabled: false, block: 'ultrabeam moving'), ('PERMIT OFF', AppTheme.txtMute));
    });

    test('permitted but blocked names the gate in amber', () {
      expect(tag(block: 'band change settling'), ('BLOCKED: BAND CHANGE SETTLING', AppTheme.amber));
    });

    test('missing keys stay unknown (R4.4)', () {
      expect(tag(enabled: null, armed: null), ('ARM ?', AppTheme.amber));
      expect(tag(armed: null), ('ARM ?', AppTheme.amber));
    });
  });

  group('PaArmRow', () {
    Future<(BusStore, FakeMqttService)> pump(WidgetTester tester, void Function(BusStore) seed) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      seed(store);
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaArmRow()));
      await tester.pumpAndSettle();
      return (store, mqtt);
    }

    testWidgets('sits on the PA card', (tester) async {
      final store = BusStore();
      store.setPaHealthy();
      store.setPaArm(enabled: true, armed: true);
      await tester.pumpWidget(TestHarness(store: store, mqtt: FakeMqttService(store), child: const PaPanel()));
      await tester.pumpAndSettle();

      expect(find.byType(PaArmRow), findsOneWidget);
      expect(find.text('ARMED'), findsOneWidget);
    });

    testWidgets('shows OFFLINE and disables the permit while the slot is dark', (tester) async {
      final (_, mqtt) = await pump(tester, (s) {});

      expect(find.text('OFFLINE'), findsOneWidget);
      await tester.tap(find.text('ARM PERMIT'));
      await tester.pumpAndSettle();
      expect(find.byType(AlertDialog), findsNothing);
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('a dead bridge hides retained armed state', (tester) async {
      await pump(tester, (s) {
        s.setPaArm(enabled: true, armed: true);
        s.setBridgeOffline('muehle/hf/pa-arm');
      });

      expect(find.text('OFFLINE'), findsOneWidget);
      expect(find.text('ARMED'), findsNothing);
    });

    testWidgets('block shows on the card but never in FAULTS', (tester) async {
      final store = BusStore();
      store.setPaArm(enabled: true, armed: false, block: 'ultrabeam moving');
      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: FakeMqttService(store),
        child: const Column(children: [PaArmRow(), FaultsBar()]),
      ));
      await tester.pumpAndSettle();

      expect(find.text('BLOCKED: ULTRABEAM MOVING'), findsOneWidget);
      expect(find.textContaining('ultrabeam moving', findRichText: true), findsNothing);
      expect(store.faultHistory, isEmpty);
    });

    testWidgets('granting the permit asks first, then publishes retained', (tester) async {
      final (_, mqtt) = await pump(tester, (s) => s.setPaArm(enabled: false, armed: false, block: 'permit off'));
      expect(find.text('PERMIT OFF'), findsOneWidget);

      await tester.tap(find.text('ARM PERMIT'));
      await tester.pumpAndSettle();
      expect(find.text('Grant arm permit?'), findsOneWidget);
      expect(mqtt.publishes, isEmpty);

      await tester.tap(find.text('GRANT'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.single.topic, 'muehle/hf/pa-arm/cmd');
      expect(mqtt.publishes.single.payload, contains('"set_enabled"'));
      expect(mqtt.publishes.single.payload, contains('"true"'));
      expect(mqtt.publishes.single.retain, isTrue);
    });

    testWidgets('cancel publishes nothing', (tester) async {
      final (_, mqtt) = await pump(tester, (s) => s.setPaArm(enabled: true, armed: true));

      await tester.tap(find.text('ARM PERMIT'));
      await tester.pumpAndSettle();
      expect(find.text('Revoke arm permit?'), findsOneWidget);
      await tester.tap(find.text('Cancel'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('revoke publishes set_enabled false', (tester) async {
      final (_, mqtt) = await pump(tester, (s) => s.setPaArm(enabled: true, armed: true));

      await tester.tap(find.text('ARM PERMIT'));
      await tester.pumpAndSettle();
      await tester.tap(find.text('REVOKE'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.single.payload, contains('"false"'));
      expect(mqtt.publishes.single.retain, isTrue);
    });

    testWidgets('no publish if the slot drops while the dialog is open', (tester) async {
      final (store, mqtt) = await pump(tester, (s) => s.setPaArm(enabled: false, armed: false, block: 'permit off'));

      await tester.tap(find.text('ARM PERMIT'));
      await tester.pumpAndSettle();
      store.setBridgeOffline('muehle/hf/pa-arm');
      await tester.tap(find.text('GRANT'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('unknown permit disables the toggle', (tester) async {
      final (_, mqtt) = await pump(tester, (s) => s.setPaArm(enabled: null, armed: null));

      expect(find.text('ARM ?'), findsOneWidget);
      await tester.tap(find.text('ARM PERMIT'));
      await tester.pumpAndSettle();
      expect(find.byType(AlertDialog), findsNothing);
      expect(mqtt.publishes, isEmpty);
    });
  });
}
