import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/mqtt/mqtt_service.dart';
import 'package:hf_console/store/bus_store.dart';

// Ingestion-batch semantics (review T2): the `updates` listener is
// broker-bound, so the batch path is exercised through the ingest test seam.
// The guarantee: one bad payload in a batch must not take out the messages
// after it, and the rejection is counted, never thrown.
void main() {
  test('one bad payload does not abort the rest of its batch', () {
    final store = BusStore();
    final svc = MqttService(store);

    svc.ingest('muehle/hf/pa/state', utf8.encode('{"mode": "operate"}'));
    svc.ingest('muehle/hf/pa/state', utf8.encode('garbage'));
    svc.ingest('muehle/hf/radio/state', utf8.encode('{"freq_hz": 14236000}'));

    expect(store.slots['muehle/hf/pa']?.state?['mode'], 'operate');
    expect(store.slots['muehle/hf/radio']?.state?['freq_hz'], 14236000);
    expect(store.malformedPayloads, 1);
  });

  test('raw non-JSON bytes decode with allowMalformed and are dropped', () {
    final store = BusStore();
    final svc = MqttService(store);

    svc.ingest('muehle/hf/pa/state', [0xff, 0xfe, 0x00]);

    expect(store.slots['muehle/hf/pa']?.state, isNull);
    expect(store.malformedPayloads, 1);
  });
}
