import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/auto/auto_controls.dart';
import 'package:hf_console/store/auto_op.dart';
import 'package:hf_console/ui/auto/band_selector.dart';
import 'package:hf_console/ui/auto/auto_page.dart' show autoErrorText, autoMaxScale;
import 'package:hf_console/ui/auto/input_faults.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:hf_console/ui/theme.dart';

import '../../support/auto_op_fixtures.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/render_support.dart';
import '../../support/test_harness.dart';

/// About 914 x 411 dp: a phone in landscape, as mirrored to the car.
const _phoneLandscape = (Size(2400, 1080), 2.625);
const _phonePortrait = (Size(1170, 2532), 3.0);
const _tablet = (Size(2001, 1200), 1.5);

/// Pumps the console on the 75AUTO page. [seed] fills the store before the
/// pump (the retained copy). With [live] a /state with a new `seq` and a
/// /beat follow, so the data is fresh.
Future<(BusStore, FakeMqttService)> _pumpAuto(
  WidgetTester tester, {
  (Size, double) device = _phoneLandscape,
  Map<String, dynamic>? doc,
  String status = 'online',
  bool live = true,
  bool linkDown = false,
}) async {
  tester.view.physicalSize = device.$1;
  tester.view.devicePixelRatio = device.$2;
  addTearDown(tester.view.reset);
  final d = doc ?? busyAutoOpDoc();
  final store = BusStore()..seedAutoOp(d, status: status);
  final mqtt = FakeMqttService(store);
  await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
  await tester.pump(const Duration(milliseconds: 100));
  await tester.tap(find.widgetWithText(InkWell, '75AUTO'));
  await tester.pump(const Duration(milliseconds: 100));
  if (live) {
    store.liveAutoOp({...d, 'seq': (d['seq'] as int) + 1});
    store.applyAutoOpBeat(stateSeq: (d['seq'] as int) + 1, nowMs: busyNowMs, slotOffsetMs: 11000);
  }
  if (linkDown) {
    store.markDisconnected();
    mqtt.connected.value = false;
  }
  await tester.pump(const Duration(milliseconds: 300));
  return (store, mqtt);
}

String _text(WidgetTester tester, String key) => tester.widget<Text>(find.byKey(ValueKey(key))).data!;

void _expectStopInView(WidgetTester tester, Size logical) {
  final stop = tester.getRect(find.byKey(const ValueKey('auto-stop-tx')));
  // On a larger screen the area under the top bar scales up as a whole,
  // at most 2x. A phone does not scale.
  if (logical.width < 960) {
    expect(stop.height, moreOrLessEquals(autoStopHeight));
  } else {
    expect(stop.height, inInclusiveRange(autoStopHeight, autoStopHeight * autoMaxScale));
  }
  expect(stop.top, greaterThanOrEqualTo(0));
  expect(stop.bottom, lessThanOrEqualTo(logical.height));
  expect(stop.right, lessThanOrEqualTo(logical.width));
}

void main() {
  // The layout checks need the real fonts: the test font draws every glyph
  // as a square, much wider than the bundled faces.
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    await loadRenderFonts();
  });

  group('layout', () {
    for (final (name, device) in [('phone', _phoneLandscape), ('tablet', _tablet), ('ID.7', (const Size(3200, 1520), 2.0))]) {
      testWidgets('$name: the page tabs stay in place between HF and 75AUTO', (tester) async {
        tester.view.physicalSize = device.$1;
        tester.view.devicePixelRatio = device.$2;
        addTearDown(tester.view.reset);
        final store = BusStore()..seedAutoOp(busyAutoOpDoc());
        await tester.pumpWidget(TestHarness(store: store, mqtt: FakeMqttService(store), child: const ConsoleScreen()));
        await tester.pump(const Duration(milliseconds: 100));
        // The left and top edge of each tab. The active tab has a bolder
        // label, so a width can change by a fraction of a pixel.
        List<double> tabs() => [
              for (final t in ['Station', 'HF', 'UHF', 'CAM', '75AUTO'])
                ...() {
                  final r = tester.getRect(find.widgetWithText(InkWell, t));
                  return [r.left, r.top];
                }(),
            ];
        await tester.tap(find.widgetWithText(InkWell, 'HF'));
        await tester.pump(const Duration(milliseconds: 300));
        final onHf = tabs();
        await tester.tap(find.widgetWithText(InkWell, '75AUTO'));
        await tester.pump(const Duration(milliseconds: 300));
        final onAuto = tabs();
        for (var i = 0; i < onHf.length; i++) {
          expect(onAuto[i], moreOrLessEquals(onHf[i], epsilon: 1.0));
        }
      });
    }

    testWidgets('landscape phone: no overflow, no page scroll, Stop TX in view', (tester) async {
      await _pumpAuto(tester);
      expect(find.byKey(const ValueKey('auto-portrait-scroll')), findsNothing);
      _expectStopInView(tester, tester.view.physicalSize / tester.view.devicePixelRatio);
    });

    testWidgets('landscape phone under LINK DOWN: no overflow, Stop TX in view', (tester) async {
      await _pumpAuto(tester, linkDown: true);
      expect(find.text('LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED'), findsOneWidget);
      _expectStopInView(tester, tester.view.physicalSize / tester.view.devicePixelRatio);
      // Approve and Reject keep their touch target.
      expect(tester.getSize(find.widgetWithText(ElevatedButton, 'APPROVE')).height, greaterThanOrEqualTo(48));
    });

    testWidgets('portrait phone: no overflow, the lists scroll below the controls', (tester) async {
      await _pumpAuto(tester, device: _phonePortrait);
      expect(find.byKey(const ValueKey('auto-portrait-scroll')), findsOneWidget);
      final stop = tester.getRect(find.byKey(const ValueKey('auto-stop-tx')));
      final scroll = tester.getRect(find.byKey(const ValueKey('auto-portrait-scroll')));
      expect(stop.bottom, lessThanOrEqualTo(scroll.top));
    });

    testWidgets('tablet: no overflow', (tester) async {
      await _pumpAuto(tester, device: _tablet);
      _expectStopInView(tester, tester.view.physicalSize / tester.view.devicePixelRatio);
    });

    testWidgets('every touch target is at least 48 x 48 dp', (tester) async {
      await _pumpAuto(tester);
      for (final e in find.byType(ElevatedButton).evaluate()) {
        final size = tester.getSize(find.byWidget(e.widget));
        expect(size.width, greaterThanOrEqualTo(48), reason: '${e.widget}');
        expect(size.height, greaterThanOrEqualTo(48), reason: '${e.widget}');
      }
    });
  });

  group('top bar', () {
    testWidgets('a missing mode shows ?, never AUTO', (tester) async {
      final doc = busyAutoOpDoc();
      doc['session'] = {'state': 'running', 'rule_set': 'x', 'level': 'normal', 'source': 'live'};
      await _pumpAuto(tester, doc: doc);
      expect(_text(tester, 'auto-mode'), 'MODE ?');
    });

    testWidgets('auto mode shows the presence time', (tester) async {
      await _pumpAuto(tester);
      expect(_text(tester, 'auto-mode'), 'AUTO · PRESENCE 08:12');
    });

    testWidgets('a replay shows the REPLAY pill', (tester) async {
      final doc = busyAutoOpDoc();
      doc['session'] = {...doc['session'] as Map<String, dynamic>, 'source': 'replay'};
      await _pumpAuto(tester, doc: doc);
      expect(find.byKey(const ValueKey('auto-replay')), findsOneWidget);
    });

    testWidgets('a live session has no REPLAY pill', (tester) async {
      await _pumpAuto(tester);
      expect(find.byKey(const ValueKey('auto-replay')), findsNothing);
    });

    testWidgets('a retained document before any new seq shows RETAINED with its ts', (tester) async {
      await _pumpAuto(tester, live: false);
      expect(_text(tester, 'auto-age'), 'RETAINED 14:32:41');
    });

    testWidgets('fresh data shows its age', (tester) async {
      await _pumpAuto(tester);
      expect(_text(tester, 'auto-age'), contains('0.'));
      expect(_text(tester, 'auto-age'), isNot(contains('STALE')));
    });

    testWidgets('AE6: no new seq for 6 s in a running session shows DATA STALE', (tester) async {
      await _pumpAuto(tester);
      await tester.pump(const Duration(milliseconds: 5000));
      expect(_text(tester, 'auto-age'), isNot(contains('STALE')));
      await tester.pump(const Duration(milliseconds: 1000));
      expect(_text(tester, 'auto-age'), startsWith('DATA STALE · 6'));
    });

    testWidgets('a beat keeps the data fresh', (tester) async {
      final (store, _) = await _pumpAuto(tester);
      await tester.pump(const Duration(seconds: 4));
      store.applyAutoOpBeat(stateSeq: 2001);
      await tester.pump(const Duration(seconds: 4));
      expect(_text(tester, 'auto-age'), isNot(contains('STALE')));
    });

    testWidgets('without a session the limit is 25 s', (tester) async {
      final doc = busyAutoOpDoc();
      doc['session'] = {...doc['session'] as Map<String, dynamic>, 'state': 'ended'};
      await _pumpAuto(tester, doc: doc);
      await tester.pump(const Duration(seconds: 20));
      expect(_text(tester, 'auto-age'), isNot(contains('STALE')));
      await tester.pump(const Duration(seconds: 6));
      expect(_text(tester, 'auto-age'), startsWith('DATA STALE'));
    });

    testWidgets('LINK DOWN makes the data stale at once', (tester) async {
      await _pumpAuto(tester, linkDown: true);
      expect(_text(tester, 'auto-age'), startsWith('DATA STALE'));
    });

    testWidgets('a /status offline shows 75auto offline with the time it went dark', (tester) async {
      await _pumpAuto(tester, status: 'offline', live: false);
      expect(_text(tester, 'auto-session'), matches(RegExp(r'^75AUTO OFFLINE since \d\d:\d\d:\d\d$')));
    });
  });

  group('readouts', () {
    testWidgets('missing station values show ?', (tester) async {
      final doc = busyAutoOpDoc();
      doc['station'] = <String, dynamic>{};
      await _pumpAuto(tester, doc: doc);
      expect(find.text('?'), findsWidgets);
      expect(_text(tester, 'auto-tx'), 'TX ?');
      // No stage chip is lit without a stage.
      expect(find.byKey(const ValueKey('auto-stage-now')), findsNothing);
    });

    testWidgets('the QSO stage lights its chip', (tester) async {
      await _pumpAuto(tester);
      expect(
        find.descendant(of: find.byKey(const ValueKey('auto-stage-now')), matching: find.text('R+RPT')),
        findsOneWidget,
      );
    });
  });

  group('stage chips', () {
    Color? border(WidgetTester tester, String label) {
      final box = tester.widget<Container>(
          find.ancestor(of: find.text(label), matching: find.byType(Container)).first);
      return ((box.decoration as BoxDecoration).border as Border).top.color;
    }

    testWidgets('a stage the QSO skips stays dark, the stages seen are green', (tester) async {
      await _pumpAuto(tester);
      // R+RPT is the stage now; CALL and RPT were not seen on this page.
      expect(border(tester, 'CALL'), AppTheme.cardLineHi);
      expect(border(tester, 'RPT'), AppTheme.cardLineHi);
    });

    testWidgets('the stage LOG shows green, not lit', (tester) async {
      final doc = busyAutoOpDoc();
      (doc['station'] as Map<String, dynamic>)['qso_stage'] = 'LOG';
      await _pumpAuto(tester, doc: doc);
      expect(find.byKey(const ValueKey('auto-stage-now')), findsNothing);
      expect(border(tester, 'LOG'), AppTheme.green);
    });
  });

  group('band selector', () {
    testWidgets('values from the survey, no counts, band colors, band order, and nothing off the table', (tester) async {
      final doc = busyAutoOpDoc();
      // Out of order on purpose: the selector sorts by dial.
      doc['bands'] = [
        {'band': '20m', 'dial_hz': 14074000, 'seen': true, 'count': 23, 'period_start_ms': busyNowMs - 10000, 'value': 42},
        {'band': '10m', 'dial_hz': 28074000, 'seen': false, 'count': 0, 'period_start_ms': 0},
        {'band': '40m', 'dial_hz': 7074000, 'seen': true, 'count': 12, 'period_start_ms': busyNowMs - 660000, 'value': 7},
      ];
      await _pumpAuto(tester, doc: doc);
      expect(_text(tester, 'auto-band-value-40m'), '7');
      expect(_text(tester, 'auto-band-value-20m'), '42');
      expect(_text(tester, 'auto-band-value-10m'), '–');
      // No QRG in the pills.
      expect(find.text('7.074'), findsNothing);
      // 160 m is not in the band table: no value. No pill shows a count.
      expect(find.byKey(const ValueKey('auto-band-value-160m')), findsNothing);
      expect(find.byKey(const ValueKey('auto-band-160m')), findsOneWidget);
      expect(find.text('23'), findsNothing); // no counts, and no Decodes readout
      final y40 = tester.getCenter(find.byKey(const ValueKey('auto-band-40m'))).dy;
      final y20 = tester.getCenter(find.byKey(const ValueKey('auto-band-20m'))).dy;
      final y10 = tester.getCenter(find.byKey(const ValueKey('auto-band-10m'))).dy;
      expect(y40, lessThan(y20));
      expect(y20, lessThan(y10));
      final color = tester.widget<Container>(find.byKey(const ValueKey('auto-band-color-40m'))).color;
      expect(color, AppTheme.bandColor('40m'));
      // No age of a count either.
      expect(find.text('11m'), findsNothing);
    });

    testWidgets('a band without a batch in this session shows its survey value undimmed', (tester) async {
      final doc = busyAutoOpDoc();
      doc['bands'] = [
        {'band': '80m', 'dial_hz': 3573000, 'seen': false, 'count': 0, 'period_start_ms': 0, 'value': 23},
        {'band': '20m', 'dial_hz': 14074000, 'seen': true, 'count': 23, 'period_start_ms': busyNowMs, 'value': 42},
        {'band': '10m', 'dial_hz': 28074000, 'seen': false, 'count': 0, 'period_start_ms': 0},
      ];
      await _pumpAuto(tester, doc: doc);
      Color? color(String key) => tester.widget<Text>(find.byKey(ValueKey(key))).style?.color;
      expect(_text(tester, 'auto-band-value-80m'), '23');
      expect(color('auto-band-value-80m'), AppTheme.txt);
      expect(color('auto-band-value-20m'), AppTheme.txt);
      // A band without a value shows a dimmed dash.
      expect(color('auto-band-value-10m'), AppTheme.txtFaint);
    });

    testWidgets('a band more than 10 % above the current band shows green', (tester) async {
      final doc = busyAutoOpDoc(); // current band 20m
      doc['bands'] = [
        {'band': '40m', 'dial_hz': 7074000, 'seen': false, 'count': 0, 'period_start_ms': 0, 'value': 111},
        {'band': '30m', 'dial_hz': 10136000, 'seen': false, 'count': 0, 'period_start_ms': 0, 'value': 110},
        {'band': '20m', 'dial_hz': 14074000, 'seen': true, 'count': 23, 'period_start_ms': busyNowMs, 'value': 100},
        {'band': '15m', 'dial_hz': 21074000, 'seen': false, 'count': 0, 'period_start_ms': 0, 'value': 50},
      ];
      await _pumpAuto(tester, doc: doc);
      Color? value(String b) => tester.widget<Text>(find.byKey(ValueKey('auto-band-value-$b'))).style?.color;
      Color border(String b) =>
          ((tester.widget<Container>(find.byKey(ValueKey('auto-band-$b'))).decoration as BoxDecoration).border as Border)
              .top
              .color;
      expect(value('40m'), AppTheme.green);
      expect(border('40m'), AppTheme.green);
      // Exactly 10 % above is not more than 10 %.
      expect(value('30m'), AppTheme.txt);
      expect(border('30m'), isNot(AppTheme.green));
      expect(value('15m'), AppTheme.txt);
      expect(border('20m'), AppTheme.accent);
    });

    test('isBetterBand needs both values and more than 10 %', () {
      expect(BandSelector.isBetterBand(111, 100), isTrue);
      expect(BandSelector.isBetterBand(110, 100), isFalse);
      expect(BandSelector.isBetterBand(1, 0), isTrue);
      expect(BandSelector.isBetterBand(null, 100), isFalse);
      expect(BandSelector.isBetterBand(100, null), isFalse);
    });
  });

  group('band order', () {
    testWidgets('the pills keep the band order when a dial is missing or moves', (tester) async {
      final doc = busyAutoOpDoc();
      doc['bands'] = [
        {'band': '20m', 'dial_hz': 0, 'seen': true, 'count': 23, 'period_start_ms': busyNowMs},
        {'band': '40m', 'dial_hz': 7047500, 'seen': true, 'count': 3, 'period_start_ms': busyNowMs},
        {'band': '80m', 'dial_hz': 3573000, 'seen': false, 'count': 0, 'period_start_ms': 0},
      ];
      await _pumpAuto(tester, doc: doc);
      double y(String b) => tester.getCenter(find.byKey(ValueKey('auto-band-$b'))).dy;
      final order = ['160m', '80m', '40m', '30m', '20m', '17m', '15m', '12m', '10m'];
      for (var i = 1; i < order.length; i++) {
        expect(y(order[i - 1]), lessThan(y(order[i])), reason: '${order[i - 1]} above ${order[i]}');
      }
    });
  });

  group('next action', () {
    testWidgets('a pending proposal shows with its countdown', (tester) async {
      await _pumpAuto(tester);
      expect(_text(tester, 'auto-next-what'), 'QSY 17m');
      expect(_text(tester, 'auto-confirm-countdown'), 'CONFIRM 12 s');
      await tester.pump(const Duration(seconds: 2));
      expect(_text(tester, 'auto-confirm-countdown'), 'CONFIRM 10 s');
    });

    testWidgets('the countdown stops while the data is stale', (tester) async {
      await _pumpAuto(tester);
      await tester.pump(const Duration(seconds: 7));
      final a = _text(tester, 'auto-confirm-countdown');
      await tester.pump(const Duration(seconds: 2));
      expect(_text(tester, 'auto-confirm-countdown'), a);
    });

    testWidgets('with a DX call the card shows the station: call, name and distance', (tester) async {
      final doc = busyAutoOpDoc();
      doc['pending'] = null;
      final st = doc['station'] as Map<String, dynamic>;
      st['dx_name'] = 'Ana Example';
      st['dx_image'] = '';
      st['dx_km'] = 11234;
      await _pumpAuto(tester, doc: doc);
      expect(_text(tester, 'auto-dx-call'), 'VK9XQ');
      expect(_text(tester, 'auto-dx-name'), 'Ana Example');
      expect(_text(tester, 'auto-dx-km'), '11234km');
      // No proposal strip without a proposal, and no picture without a URL.
      expect(find.byKey(const ValueKey('auto-proposal')), findsNothing);
      expect(find.byKey(const ValueKey('auto-dx-image')), findsNothing);
      expect(find.byKey(const ValueKey('auto-confirm-countdown')), findsNothing);
    });

    testWidgets('the QRZ picture spans the card, with the name under it', (tester) async {
      final doc = busyAutoOpDoc();
      doc['pending'] = null;
      final st = doc['station'] as Map<String, dynamic>;
      st['dx_name'] = 'Ana Example';
      st['dx_image'] = 'https://example.org/vk9xq.jpg';
      await _pumpAuto(tester, doc: doc, device: _tablet);
      final image = tester.getRect(find.byKey(const ValueKey('auto-dx-image')));
      final name = tester.getRect(find.byKey(const ValueKey('auto-dx-name')));
      final call = tester.getRect(find.byKey(const ValueKey('auto-dx-card')));
      expect(name.top, greaterThanOrEqualTo(image.bottom));
      // The width of the card less its padding (scaled with the page).
      expect(image.width, greaterThan(call.width * 0.9));
      expect(image.height, greaterThanOrEqualTo(32));
    });

    testWidgets('a pending proposal shows as a strip under the station', (tester) async {
      await _pumpAuto(tester);
      expect(_text(tester, 'auto-dx-call'), 'VK9XQ');
      expect(find.byKey(const ValueKey('auto-proposal')), findsOneWidget);
      expect(find.descendant(of: find.byKey(const ValueKey('auto-proposal')), matching: find.text('QSY 17m')),
          findsOneWidget);
    });

    testWidgets('a finished call does not stay as the current action', (tester) async {
      final doc = busyAutoOpDoc();
      doc['pending'] = null;
      // The QSO with EP2AES is logged: no DX call, but 75auto still sends
      // the call as its newest action line.
      (doc['station'] as Map<String, dynamic>)['dx_call'] = '';
      doc['current'] = {
        'id': 52,
        'time_ms': busyNowMs - 20000,
        'rule': 'call-needed',
        'action': 'call-station',
        'call': 'EP2AES',
        'band': '17m',
        'result': 'took-effect',
        'cause': '',
        'gate_reason': '',
        'reason': 'dxcc EP',
      };
      doc['idle'] = {
        'proposed': 0,
        'blocks': [
          {'call': 'DL1ABC', 'why': 'worked before'},
        ],
        'more_blocks': 0,
      };
      await _pumpAuto(tester, doc: doc, device: _tablet);
      expect(_text(tester, 'auto-next-what'), 'no proposal');
      expect(_text(tester, 'auto-next-why'), 'DL1ABC: worked before');
    });

    testWidgets('without pending or current it says why the rules propose nothing', (tester) async {
      final doc = busyAutoOpDoc();
      doc['pending'] = null;
      doc['current'] = null;
      (doc['station'] as Map<String, dynamic>)['dx_call'] = '';
      doc['idle'] = {
        'proposed': 0,
        'blocks': [
          {'call': 'DL1ABC', 'why': 'worked before'},
        ],
        'more_blocks': 2,
      };
      await _pumpAuto(tester, doc: doc, device: _tablet);
      expect(_text(tester, 'auto-next-what'), 'no proposal');
      expect(_text(tester, 'auto-next-why'), 'DL1ABC: worked before (+2 more)');
    });

    testWidgets('a latched stop shows the latch and its reason', (tester) async {
      final doc = busyAutoOpDoc()
        ..['latched'] = true
        ..['stop_reason'] = 'operator stop';
      await _pumpAuto(tester, doc: doc, device: _tablet);
      expect(_text(tester, 'auto-next-what'), 'LATCHED · resume at the station');
      expect(_text(tester, 'auto-next-why'), 'operator stop');
      expect(find.textContaining('RESUME'), findsNothing);
    });
  });

  group('input faults', () {
    testWidgets('healthy inputs show no lights and no fault line', (tester) async {
      await _pumpAuto(tester, doc: busyAutoOpDoc());
      for (final name in ['WSJT-X', 'RIG', 'NEEDED', 'SPOTS', 'CLOCK']) {
        expect(find.text(name), findsNothing);
      }
      expect(find.byKey(const ValueKey('auto-error')), findsNothing);
    });

    testWidgets('a stale input, a bad rig and a failing clock show as one fault line', (tester) async {
      final doc = busyAutoOpDoc()
        ..['health'] = [
          {'interface': 'digimode', 'name': 'WSJT-X', 'fresh': false, 'reason': ''},
          {'interface': 'spots', 'name': 'SPOTS', 'fresh': false, 'reason': '4 min old'},
          {'interface': 'needed', 'name': 'NEEDED', 'fresh': null},
        ]
        ..['rig'] = {'present': true, 'ok': false, 'reason': 'no reply'}
        ..['clock_failing'] = true;
      await _pumpAuto(tester, doc: doc);
      expect(find.text('INPUT FAULT · WSJT-X not reporting · RIG no reply · SPOTS 4 min old · CLOCK failing'),
          findsOneWidget);
    });

    test('unknown inputs and a missing rig are no faults', () {
      final doc = busyAutoOpDoc()
        ..['health'] = [
          {'interface': 'digimode', 'name': 'WSJT-X'},
        ]
        ..['rig'] = {'present': false}
        ..remove('clock_failing');
      expect(autoInputFaults(AutoOpView.fromState(doc)), isEmpty);
    });

    test('the error comes first, then the faults', () {
      expect(autoErrorText('rule cycle failed', ['CLOCK failing']),
          '75AUTO ERROR · rule cycle failed · INPUT FAULT · CLOCK failing');
      expect(autoErrorText('', const []), '');
    });
  });

  testWidgets('the other pages keep the HF band set of the DX feed', (tester) async {
    expect(dxBandsForPage('75auto'), dxBandsForPage('hf'));
    expect(dxModeRadioForPage('75auto'), dxModeRadioForPage('hf'));
  });
}
