import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/wiring.dart';
import 'package:hf_console/ui/widgets/pulsing_amber_button.dart';
import 'package:hf_console/ui/widgets/rx_antenna_button.dart';
import 'package:hf_console/ui/widgets/trx_panel.dart';
import 'package:shared_preferences/shared_preferences.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  setUp(() => SharedPreferences.setMockInitialValues({}));

  Future<FakeMqttService> pump(WidgetTester tester, BusStore store) async {
    final mqtt = FakeMqttService(store);
    await tester.binding.setSurfaceSize(const Size(1200, 800));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const TrxPanel()));
    await tester.pumpAndSettle();
    return mqtt;
  }

  group('TrxPanel bands', () {
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
  });

  group('RX antenna button', () {
    Finder rxBtn() => find.byKey(const ValueKey('rx-antenna-btn'));
    ElevatedButton button(WidgetTester tester) => tester.widget<ElevatedButton>(rxBtn());

    testWidgets('ANT 1: shows it, tap asks for RX A (not retained)', (tester) async {
      final store = BusStore()..setRadio(rxInput: 'ant1');
      final mqtt = await pump(tester, store);

      expect(find.descendant(of: rxBtn(), matching: find.text('ANT 1')), findsOneWidget);
      await tester.tap(rxBtn());
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.topic, 'muehle/hf/radio/cmd');
      expect(mqtt.publishes.single.payload, radioSelectRxPayload('rx_a'));
      expect(mqtt.publishes.single.retain, isFalse);
      // No optimistic flip: the label waits for the radio.
      expect(find.descendant(of: rxBtn(), matching: find.text('ANT 1')), findsOneWidget);
    });

    testWidgets('RX A: shows it, tap goes back to ANT 1', (tester) async {
      final store = BusStore()..setRadio(rxInput: 'rx_a');
      final mqtt = await pump(tester, store);

      expect(find.descendant(of: rxBtn(), matching: find.text('RX A')), findsOneWidget);
      await tester.tap(rxBtn());
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, radioSelectRxPayload('ant1'));
    });

    testWidgets('follows the radio when SmartSDR changes the antenna', (tester) async {
      final store = BusStore()..setRadio(rxInput: 'ant1');
      final mqtt = await pump(tester, store);

      store.setRadio(rxInput: 'rx_a');
      await tester.pumpAndSettle();
      expect(find.descendant(of: rxBtn(), matching: find.text('RX A')), findsOneWidget);

      // An input the console never sets shows as-is; a tap goes to RX A.
      store.setRadio(rxInput: 'ant2');
      await tester.pumpAndSettle();
      expect(find.descendant(of: rxBtn(), matching: find.text('ANT 2')), findsOneWidget);
      await tester.tap(rxBtn());
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, radioSelectRxPayload('rx_a'));
    });

    testWidgets('disabled without rx_input (older flexbridge)', (tester) async {
      final store = BusStore()..setRadio();
      await pump(tester, store);
      expect(find.descendant(of: rxBtn(), matching: find.text('—')), findsOneWidget);
      expect(button(tester).onPressed, isNull);
    });

    testWidgets('disabled while the radio bridge is offline', (tester) async {
      final store = BusStore()..setRadio(rxInput: 'ant1');
      store.setBridgeOffline('muehle/hf/radio');
      await pump(tester, store);
      expect(button(tester).onPressed, isNull);
    });

    bool pulsing(WidgetTester tester) =>
        tester.widget<PulsingAmberButton>(find.byType(PulsingAmberButton)).engaged;

    testWidgets('RX A pulses above 40m, not on 40m and below', (tester) async {
      final store = BusStore()..setRadio(band: '40m', rxInput: 'rx_a');
      await pump(tester, store);
      expect(pulsing(tester), isFalse);

      store.setRadio(band: '30m', rxInput: 'rx_a');
      await tester.pump();
      expect(pulsing(tester), isTrue);

      store.setRadio(band: '80m', rxInput: 'rx_a');
      await tester.pump();
      expect(pulsing(tester), isFalse);

      // ANT 1 on a high band is the normal state.
      store.setRadio(band: '20m', rxInput: 'ant1');
      await tester.pump();
      expect(pulsing(tester), isFalse);
    });

    testWidgets('in split the listened-on band (rx_band) decides', (tester) async {
      final store = BusStore()
        ..setRadio(band: '20m', split: true, rxBand: '80m', rxInput: 'rx_a');
      await pump(tester, store);
      expect(pulsing(tester), isFalse);
    });

    test('aboveRxABands', () {
      for (final b in ['30m', '20m', '17m', '15m', '12m', '10m', '6m', '2m', '70cm']) {
        expect(RxAntennaButton.aboveRxABands(b), isTrue, reason: b);
      }
      for (final b in ['160m', '80m', '60m', '40m', '', 'gen']) {
        expect(RxAntennaButton.aboveRxABands(b), isFalse, reason: b);
      }
    });
  });
}
