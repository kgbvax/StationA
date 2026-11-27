import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/auto_op.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/auto/decode_list.dart';
import 'package:hf_console/ui/auto/rx_list.dart';

import '../../support/auto_op_fixtures.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/test_harness.dart';

AutoOpView _view(Map<String, dynamic> doc) => AutoOpView.fromState(doc)!;

Future<void> _pumpList(WidgetTester tester, Map<String, dynamic> doc, Widget list) async {
  final store = BusStore()..seedAutoOp(doc);
  await tester.pumpWidget(TestHarness(
    store: store,
    mqtt: FakeMqttService(store),
    child: Center(child: SizedBox(width: 420, height: 400, child: list)),
  ));
  await tester.pump();
}

void main() {
  group('band activity', () {
    testWidgets('AE10: decode text with angle brackets shows as plain text', (tester) async {
      final doc = busyAutoOpDoc()
        ..['decodes'] = [
          {'period_start_ms': busyNowMs - 11000, 'df': 800, 'snr': -3, 'text': '<b>CQ</b> X1ABC', 'call': 'X1ABC', 'cq': true},
        ];
      await _pumpList(tester, doc, const DecodeList());
      expect(find.text('<b>CQ</b> X1ABC'), findsOneWidget);
    });

    test('newest period first, each under a header with its time and parity', () {
      final rows = bandActivityRows(_view(busyAutoOpDoc()));
      final headers = [for (final r in rows) if (r.header != null) r.header];
      expect(headers, ['14:32:30 · EVEN', '14:32:00 · EVEN']);
      expect(rows.first.header, isNotNull);
      // The decodes of the older period are old.
      final older = rows.skipWhile((r) => r.header != '14:32:00 · EVEN').skip(1);
      expect(older.every((r) => r.old), isTrue);
      expect(rows[1].old, isFalse);
    });

    test('an odd period says ODD', () {
      final doc = busyAutoOpDoc()
        ..['decodes'] = [
          {'period_start_ms': busyNowMs - 26000, 'df': 800, 'snr': -3, 'text': 'CQ X1ABC'},
        ];
      expect(bandActivityRows(_view(doc)).first.header, '14:32:15 · ODD');
    });

    testWidgets('markers: TGT on the DX call, NEW on a needed station, no TO ME tag', (tester) async {
      await _pumpList(tester, busyAutoOpDoc(), const DecodeList());
      expect(find.text('TGT'), findsWidgets);
      expect(find.text('NEW'), findsWidgets);
      // A decode to me shows in red. It has no tag.
      expect(find.text('TO ME'), findsNothing);
    });
  });

  group('RX frequency', () {
    test('own lines of this band, decodes within 50 Hz, and decodes to me, oldest first', () {
      final rows = rxRows(_view(busyAutoOpDoc()));
      final texts = [for (final r in rows) r.tx?.text ?? r.decode!.text];
      expect(texts, [
        'VK9XQ DL2XYZ JO31',
        'DL2XYZ VK9XQ -11',
        'VK9XQ DL2XYZ R-09',
        'VK9XQ JA3XYZ R-18',
        'CQ VK9XQ OH29',
      ]);
      // The 40m line stays out on 20m.
      expect(texts, isNot(contains('CQ DL2XYZ JO31')));
    });

    test('a decode to me far from the RX offset is still in', () {
      final doc = busyAutoOpDoc()
        ..['decodes'] = [
          {'period_start_ms': busyNowMs - 11000, 'df': 300, 'snr': -3, 'text': 'DL2XYZ X1ABC -05', 'to_me': true},
          {'period_start_ms': busyNowMs - 11000, 'df': 1600, 'snr': -3, 'text': 'CQ X2ABC'},
        ];
      final texts = [for (final r in rxRows(_view(doc))) if (r.decode != null) r.decode!.text];
      expect(texts, ['DL2XYZ X1ABC -05']);
    });

    testWidgets('the title names the RX offset', (tester) async {
      await _pumpList(tester, busyAutoOpDoc(), const RxList());
      expect(find.text('RX FREQUENCY · 1502 HZ ±50'), findsOneWidget);
    });
  });
}
