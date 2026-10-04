import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/dvk_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('DvkPanel', () {
    Future<FakeMqttService> pump(WidgetTester tester, BusStore store) async {
      final mqtt = FakeMqttService(store);
      await tester.binding.setSurfaceSize(const Size(1200, 800));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const DvkPanel()));
      await tester.pumpAndSettle();
      return mqtt;
    }

    testWidgets('labels the buttons with the radio memory names', (tester) async {
      final store = BusStore();
      store.setRadio();
      store.applyState('muehle/hf/radio', {
        ...store.slots['muehle/hf/radio']!.state!,
        'dvk_memories': [
          {'id': 1, 'name': 'CQ ET', 'duration_ms': 2720},
          {'id': 2, 'name': 'dl9et', 'duration_ms': 2565},
          {'id': 3, 'name': 'Recording 3', 'duration_ms': 7445},
          {'id': 9, 'name': 'Recording 9', 'duration_ms': 0}, // beyond the 4 buttons
        ],
      });
      final mqtt = await pump(tester, store);

      expect(find.text('CQ ET'), findsOneWidget);
      expect(find.text('dl9et'), findsOneWidget);
      expect(find.text('DVK3'), findsNothing); // only memories 1–2 get buttons
      expect(find.text('Recording 3'), findsNothing);
      expect(find.text('Recording 9'), findsNothing);

      await tester.tap(find.widgetWithText(ElevatedButton, 'dl9et'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.topic, 'muehle/hf/radio/cmd');
      expect(mqtt.publishes.single.payload, contains('dvk_play_2'));
    });

    testWidgets('falls back to DVK<n> before the radio reports names', (tester) async {
      final store = BusStore();
      store.setRadio();
      await pump(tester, store);

      expect(find.text('DVK1'), findsOneWidget);
      expect(find.text('DVK2'), findsOneWidget);
      expect(find.text('DVK3'), findsNothing);
    });
  });

  test('memoryNames ignores malformed input', () {
    expect(DvkPanel.memoryNames(null), isEmpty);
    expect(DvkPanel.memoryNames('x'), isEmpty);
    expect(DvkPanel.memoryNames([1, {'id': '1', 'name': 'a'}, {'id': 2}]), isEmpty);
    expect(DvkPanel.memoryNames([{'id': 3, 'name': ' CQ '}, {'id': 4, 'name': '  '}]), {3: 'CQ'});
  });
}
