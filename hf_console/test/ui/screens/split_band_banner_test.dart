import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

// The cross-band split strip: VFO A (slice A) on one band, the TX slice on
// another. It sits under the LINK DOWN banner on every page, text only — the
// station warns and does not block (docs/known-issues.md "Split cross-band").

void main() {
  const bannerKey = ValueKey('split-band-banner');

  Future<void> pumpWith(WidgetTester tester, BusStore store, {Size size = const Size(3600, 2400)}) async {
    await tester.binding.setSurfaceSize(size);
    await tester.pumpWidget(TestHarness(store: store, mqtt: FakeMqttService(store), child: const ConsoleScreen()));
    await tester.pump(const Duration(milliseconds: 100));
  }

  group('Split cross-band banner', () {
    testWidgets('absent on a plain radio', (tester) async {
      final store = BusStore()..setRadio();
      await pumpWith(tester, store);
      expect(find.byKey(bannerKey), findsNothing);
    });

    testWidgets('absent on a same-band split (split, not cross-band)', (tester) async {
      final store = BusStore()..setRadio(freqHz: 14105000, band: '20m', split: true, rxFreqHz: 14100000, rxBand: '20m');
      await pumpWith(tester, store);
      expect(find.byKey(bannerKey), findsNothing);
    });

    testWidgets('names both sides and the band the next key-up goes out on', (tester) async {
      final store = BusStore()
        ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
      await pumpWith(tester, store);
      expect(find.byKey(bannerKey), findsOneWidget);
      expect(
        find.text('SPLIT CROSS-BAND — RX 17m 18100.0 · TX 20m 14105.0 — TRANSMIT GOES OUT ON 20m'),
        findsOneWidget,
      );
    });

    testWidgets('shows on every page, phone layout included', (tester) async {
      final store = BusStore()
        ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
      await pumpWith(tester, store, size: const Size(1170, 2532)); // dpr 3 → 390×844
      for (final tab in ['Station', 'UHF', 'CAM', 'HF']) {
        await tester.tap(find.widgetWithText(InkWell, tab));
        await tester.pump(const Duration(milliseconds: 100));
        expect(find.byKey(bannerKey), findsOneWidget, reason: tab);
      }
    });

    testWidgets('clears when the split goes away', (tester) async {
      final store = BusStore()
        ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
      await pumpWith(tester, store);
      expect(find.byKey(bannerKey), findsOneWidget);
      store.setRadio(freqHz: 18100000, band: '17m');
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.byKey(bannerKey), findsNothing);
    });

    testWidgets('gone with the radio link: a stale snapshot never warns', (tester) async {
      final store = BusStore()
        ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
      await pumpWith(tester, store);
      store.setDeviceOffline('muehle/hf/radio');
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.byKey(bannerKey), findsNothing);
    });

    testWidgets('gone while the MQTT link is down (LINK DOWN covers that)', (tester) async {
      final store = BusStore()
        ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
      await pumpWith(tester, store);
      store.markDisconnected();
      await tester.pump(const Duration(milliseconds: 100));
      expect(find.byKey(bannerKey), findsNothing);
    });
  });

  group('TRX pill', () {
    testWidgets('says SPLIT while split and not keyed', (tester) async {
      final store = BusStore()..setRadio(split: true, rxFreqHz: 14100000, rxBand: '20m');
      await pumpWith(tester, store);
      expect(find.text('FLEX-8400 · SPLIT'), findsOneWidget);
    });

    testWidgets('TX outranks SPLIT', (tester) async {
      final store = BusStore()..setRadio(tx: 'tx', split: true, rxFreqHz: 14100000, rxBand: '20m');
      await pumpWith(tester, store);
      expect(find.text('FLEX-8400 · TX'), findsOneWidget);
      expect(find.text('FLEX-8400 · SPLIT'), findsNothing);
    });
  });

  group('flash', () {
    final crossBand = BusStore()
      ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');

    const flashKey = ValueKey('split-band-flash');
    double opacity(WidgetTester tester) => tester.widget<FadeTransition>(find.byKey(flashKey)).opacity.value;

    // TestHarness freezes animations; re-allow motion for the banner only.
    Widget motion(bool allowed) => Builder(
          builder: (context) => MediaQuery(
            data: MediaQuery.of(context).copyWith(disableAnimations: !allowed),
            child: const SplitBandBanner(),
          ),
        );

    testWidgets('fades between full and dim, never off', (tester) async {
      await tester.pumpWidget(TestHarness(store: crossBand, child: motion(true)));
      await tester.pump();
      final seen = <double>[];
      for (var i = 0; i < 20; i++) {
        await tester.pump(const Duration(milliseconds: 100));
        seen.add(opacity(tester));
      }
      expect(seen.reduce((a, b) => a < b ? a : b), lessThan(0.5));
      expect(seen.reduce((a, b) => a > b ? a : b), greaterThan(0.9));
      expect(seen.every((o) => o >= 0.25), isTrue);
      // Gradual, not a hard on/off: no step between samples spans the range.
      for (var i = 1; i < seen.length; i++) {
        expect((seen[i] - seen[i - 1]).abs(), lessThan(0.6));
      }
      await tester.pumpWidget(const SizedBox.shrink()); // dispose the repeating ticker
    });

    testWidgets('reduce-motion holds a solid strip', (tester) async {
      await tester.pumpWidget(TestHarness(store: crossBand, child: motion(false)));
      await tester.pump(const Duration(milliseconds: 450));
      expect(find.byKey(bannerKey), findsOneWidget);
      expect(find.byKey(flashKey), findsNothing);
    });
  });

  test('banner text formats kHz with one decimal and tolerates missing parts', () {
    expect(SplitBandBanner.kHz(18100000), '18100.0');
    expect(SplitBandBanner.kHz(14105050), '14105.1');
    expect(SplitBandBanner.kHz(null), '');
    expect(
      SplitBandBanner.text(const SplitState(rxBand: '17m', txBand: '20m')),
      'SPLIT CROSS-BAND — RX 17m · TX 20m — TRANSMIT GOES OUT ON 20m',
    );
  });
}
