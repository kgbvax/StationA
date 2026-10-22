import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/wiring.dart';
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

    testWidgets('another band sends set_band', (tester) async {
      final store = BusStore();
      store.setRadio(freqHz: 3700000, band: '80m', mode: 'lsb');
      final mqtt = await pump(tester, store);

      await tester.tap(find.widgetWithText(ElevatedButton, '40m'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, radioSetBandPayload('40m'));
    });

    testWidgets('the active band toggles SSB <-> CW', (tester) async {
      final store = BusStore();
      store.setRadio(freqHz: 3700000, band: '80m', mode: 'lsb');
      final mqtt = await pump(tester, store);

      await tester.tap(find.widgetWithText(ElevatedButton, '80m'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.topic, 'muehle/hf/radio/cmd');
      expect(mqtt.publishes.single.payload, radioSetModePayload('cw'));
      expect(mqtt.publishes.single.retain, isFalse);

      store.setRadio(freqHz: 3550000, band: '80m', mode: 'cw');
      await tester.pumpAndSettle();
      mqtt.resetPublishes();
      await tester.tap(find.widgetWithText(ElevatedButton, '80m'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, radioSetModePayload('lsb'));
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

  test('toggledSsbCwMode picks the sideband by frequency', () {
    expect(toggledSsbCwMode('cw', 3550000), 'lsb'); // 80m
    expect(toggledSsbCwMode('cw', 7030000), 'lsb'); // 40m
    expect(toggledSsbCwMode('cw', 10000000), 'usb'); // 10 MHz and up: USB
    expect(toggledSsbCwMode('cw', 14025000), 'usb'); // 20m
    expect(toggledSsbCwMode('cw', null), 'usb');
    expect(toggledSsbCwMode('lsb', 3700000), 'cw');
    expect(toggledSsbCwMode('usb', 14200000), 'cw');
    expect(toggledSsbCwMode('data', 14074000), 'cw');
    expect(toggledSsbCwMode(null, null), 'cw');
  });

  test('memoryNames ignores malformed input', () {
    expect(DvkPanel.memoryNames(null), isEmpty);
    expect(DvkPanel.memoryNames('x'), isEmpty);
    expect(DvkPanel.memoryNames([1, {'id': '1', 'name': 'a'}, {'id': 2}]), isEmpty);
    expect(DvkPanel.memoryNames([{'id': 3, 'name': ' CQ '}, {'id': 4, 'name': '  '}]), {3: 'CQ'});
  });
}
