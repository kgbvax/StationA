import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import '../support/fixtures.dart';

// BusStore.splitCrossBand: flexbridge's cross-band split, surfaced only while
// the radio is live. Null means "nothing to warn about", never "unknown".

void main() {
  BusStore live() => BusStore()..markConnected(scheduleGraceNotify: false);

  test('null on a plain radio and on a same-band split', () {
    final store = live()..setRadio();
    expect(store.splitCrossBand, isNull);
    store.setRadio(freqHz: 14105000, band: '20m', split: true, rxFreqHz: 14100000, rxBand: '20m');
    expect(store.splitCrossBand, isNull);
  });

  test('carries both sides of a cross-band split', () {
    final store = live()
      ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
    final s = store.splitCrossBand;
    expect(s, isNotNull);
    expect(s!.rxBand, '17m');
    expect(s.rxFreqHz, 18100000);
    expect(s.txBand, '20m');
    expect(s.txFreqHz, 14105000);
  });

  test('null while the MQTT link is down, the bridge is down or the radio link is down', () {
    final store = live()
      ..setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
    expect(store.splitCrossBand, isNotNull);

    store.markDisconnected();
    expect(store.splitCrossBand, isNull, reason: 'link down');
    store.markConnected(scheduleGraceNotify: false);
    expect(store.splitCrossBand, isNotNull);

    store.setBridgeOffline('muehle/hf/radio');
    expect(store.splitCrossBand, isNull, reason: 'bridge down');

    store.setRadio(freqHz: 14105000, band: '20m', split: true, crossBand: true, rxFreqHz: 18100000, rxBand: '17m');
    store.applyState('muehle/hf/radio', {'cross_band': true, 'device_online': false});
    expect(store.splitCrossBand, isNull, reason: 'radio link down');
  });

  test('missing rx fields read as empty, not as a crash', () {
    final store = live()..applyStatus('muehle/hf/radio', 'online');
    store.applyState('muehle/hf/radio', {'cross_band': true, 'device_online': true, 'band': '20m'});
    final s = store.splitCrossBand;
    expect(s, isNotNull);
    expect(s!.rxBand, '');
    expect(s.rxFreqHz, isNull);
    expect(s.txBand, '20m');
  });
}
