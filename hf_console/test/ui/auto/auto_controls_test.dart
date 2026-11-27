import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/auto_op.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:mqtt_client/mqtt_client.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../../support/auto_op_fixtures.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/render_support.dart';
import '../../support/test_harness.dart';

/// A test key. It is not the key of the station.
final _keyHex = 'ab' * 32;
final _key = parseAutoOpKey(_keyHex)!;

/// A fake link that also records the QoS, and that can fail each publish.
class _Mqtt extends FakeMqttService {
  final List<MqttQos> qos = [];
  bool fail = false;

  _Mqtt(super.store);

  @override
  bool publish(String topic, String payload, {required bool retain, MqttQos qos = MqttQos.atLeastOnce}) {
    if (fail) return false;
    this.qos.add(qos);
    return super.publish(topic, payload, retain: retain, qos: qos);
  }
}

/// Pumps the console on the 75AUTO page, as in `auto_page_test.dart`, with
/// the test key stored unless [key] is false.
Future<(BusStore, _Mqtt)> _pump(
  WidgetTester tester, {
  Map<String, dynamic>? doc,
  String status = 'online',
  String commands = 'operate',
  bool live = true,
  bool key = true,
}) async {
  tester.view.physicalSize = const Size(2400, 1080);
  tester.view.devicePixelRatio = 2.625;
  addTearDown(tester.view.reset);
  SharedPreferences.setMockInitialValues(key ? {'auto_op_key': _keyHex} : {});
  final d = doc ?? busyAutoOpDoc();
  final store = BusStore()..setAutoOp(seq: d['seq'] as int, status: status, commands: commands, changes: d);
  final mqtt = _Mqtt(store);
  await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const ConsoleScreen()));
  await tester.pump(const Duration(milliseconds: 100));
  await tester.tap(find.widgetWithText(InkWell, '75AUTO'));
  await tester.pump(const Duration(milliseconds: 100));
  if (live) _live(store, d);
  await tester.pump(const Duration(milliseconds: 300));
  return (store, mqtt);
}

/// A live /state of [doc] with a new `seq`: the data is fresh.
int _seq = 5000;
void _live(BusStore store, Map<String, dynamic> doc) {
  _seq++;
  store.liveAutoOp({...doc, 'seq': _seq});
  store.applyAutoOpBeat(stateSeq: _seq, nowMs: busyNowMs, slotOffsetMs: 11000);
}

Finder _k(String key) => find.byKey(ValueKey(key));

bool _on(WidgetTester tester, String key) => tester.widget<ElevatedButton>(_k(key)).onPressed != null;

String _line(WidgetTester tester, String key) => tester.widget<Text>(_k(key)).data!;

/// The background of Stop TX, in the disabled state when [disabled].
Color? _stopBackground(WidgetTester tester, {bool disabled = false}) => tester
    .widget<ElevatedButton>(_k('auto-stop-tx'))
    .style!
    .backgroundColor!
    .resolve(disabled ? {WidgetState.disabled} : <WidgetState>{});

/// The frame color of Stop TX.
Color? _stopFrame(WidgetTester tester) =>
    tester.widget<ElevatedButton>(_k('auto-stop-tx')).style!.side!.resolve(<WidgetState>{})!.color;

/// The color of the STOP TX label.
Color? _stopLabel(WidgetTester tester) =>
    tester.widget<Text>(find.descendant(of: _k('auto-stop-tx'), matching: find.text('STOP TX'))).style!.color;

Future<void> _tap(WidgetTester tester, String key) async {
  await tester.tap(_k(key), warnIfMissed: false);
  // The key read and the publish run in microtasks.
  await tester.pump();
  await tester.pump(const Duration(milliseconds: 50));
}

/// Checks the shape and the signature of one published command and returns
/// its JSON.
Map<String, dynamic> _cmd(PublishRecord p) {
  expect(p.topic, 'muehle/hf/auto-op/cmd');
  expect(p.retain, isFalse);
  final m = jsonDecode(p.payload) as Map<String, dynamic>;
  expect(m.keys.toList(), ['action', 'value', 'ts', 'cmd_id', 'sig']);
  final sig = signAutoOpCmd(_key, m['action'] as String, jsonEncode(m['value']), m['ts'] as String, m['cmd_id'] as String);
  expect(m['sig'], sig, reason: 'the signature covers the payload');
  return m;
}

const _operate = ['auto-approve', 'auto-reject', 'auto-follow', 'auto-skip', 'auto-observe', 'auto-confirm', 'auto-auto'];

void main() {
  setUpAll(() async {
    TestWidgetsFlutterBinding.ensureInitialized();
    await loadRenderFonts();
  });

  group('Stop TX', () {
    testWidgets('AE1: with stale data it publishes a signed stop, not retained, at QoS 1', (tester) async {
      final (_, mqtt) = await _pump(tester);
      await tester.pump(const Duration(seconds: 8));
      expect(_line(tester, 'auto-operate-line'), 'DATA STALE');
      expect(_on(tester, 'auto-approve'), isFalse);
      expect(_on(tester, 'auto-stop-tx'), isTrue);

      await _tap(tester, 'auto-stop-tx');
      expect(mqtt.publishes, hasLength(1));
      expect(mqtt.qos.single, MqttQos.atLeastOnce);
      final m = _cmd(mqtt.publishes.single);
      expect(m['action'], 'stop');
      expect(m['value'], isNull);
      expect(_line(tester, 'auto-stop-line'), 'STOP SENT');
    });

    testWidgets('AE13: no latch and no result in 3 s shows STOP NOT CONFIRMED, and a second tap sends again',
        (tester) async {
      final (_, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-stop-tx');
      await tester.pump(const Duration(milliseconds: 2800));
      expect(_line(tester, 'auto-stop-line'), 'STOP SENT');
      await tester.pump(const Duration(milliseconds: 400));
      expect(_line(tester, 'auto-stop-line'), 'STOP NOT CONFIRMED · tap again');
      expect(_on(tester, 'auto-stop-tx'), isTrue);

      await _tap(tester, 'auto-stop-tx');
      expect(mqtt.publishes, hasLength(2));
      final a = _cmd(mqtt.publishes[0]);
      final b = _cmd(mqtt.publishes[1]);
      expect(b['action'], 'stop');
      expect(b['cmd_id'], isNot(a['cmd_id']));
      expect(_line(tester, 'auto-stop-line'), 'STOP SENT');
    });

    testWidgets('it never locks: two quick taps send two stops', (tester) async {
      final (_, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-stop-tx');
      await _tap(tester, 'auto-stop-tx');
      expect(mqtt.publishes, hasLength(2));
    });

    testWidgets('the latch shows LATCHED', (tester) async {
      final (store, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-stop-tx');
      final id = _cmd(mqtt.publishes.single)['cmd_id'];
      _live(store, {
        ...busyAutoOpDoc(),
        'latched': true,
        'stop_reason': 'operator stop',
        'remote': {
          'commands': 'operate',
          'last_cmd': {'cmd_id': id, 'action': 'stop', 'result': 'accepted', 'reason': '', 'at_ms': busyNowMs},
        },
      });
      await tester.pump(const Duration(seconds: 4));
      expect(_line(tester, 'auto-stop-line'), 'LATCHED');
    });

    testWidgets('stop_unconfirmed shows WSJT-X STILL SENDING', (tester) async {
      await _pump(tester, doc: {...busyAutoOpDoc(), 'latched': true, 'stop_unconfirmed': true});
      expect(_line(tester, 'auto-stop-line'), 'WSJT-X STILL SENDING');
    });

    testWidgets('a failed publish shows NOT SENT and keeps the button on', (tester) async {
      final (_, mqtt) = await _pump(tester);
      mqtt.fail = true;
      await _tap(tester, 'auto-stop-tx');
      expect(_line(tester, 'auto-stop-line'), 'NOT SENT · tap again');
      expect(_on(tester, 'auto-stop-tx'), isTrue);
    });

    testWidgets('link down: off with LINK DOWN, and a tap sends nothing', (tester) async {
      final (store, mqtt) = await _pump(tester);
      store.markDisconnected();
      mqtt.connected.value = false;
      await tester.pump(const Duration(milliseconds: 300));
      expect(_on(tester, 'auto-stop-tx'), isFalse);
      expect(_line(tester, 'auto-stop-line'), 'LINK DOWN');
      expect(_line(tester, 'auto-operate-line'), 'LINK DOWN');
      await _tap(tester, 'auto-stop-tx');
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('/status offline: off with 75auto offline', (tester) async {
      final (_, mqtt) = await _pump(tester, status: 'offline', live: false);
      expect(_on(tester, 'auto-stop-tx'), isFalse);
      expect(_line(tester, 'auto-stop-line'), '75auto offline');
      await _tap(tester, 'auto-stop-tx');
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('commands none: off with commands off at bwpc', (tester) async {
      await _pump(tester, commands: 'none');
      expect(_on(tester, 'auto-stop-tx'), isFalse);
      expect(_line(tester, 'auto-stop-line'), 'commands off at bwpc');
    });

    testWidgets('without a key: off with no key', (tester) async {
      final (_, mqtt) = await _pump(tester, key: false);
      expect(_on(tester, 'auto-stop-tx'), isFalse);
      expect(_line(tester, 'auto-stop-line'), 'no key');
      expect(_line(tester, 'auto-operate-line'), 'no key');
      await _tap(tester, 'auto-stop-tx');
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('it has the enabled colors and a red frame only while 75auto transmits', (tester) async {
      final idle = busyAutoOpDoc();
      idle['station'] = {...idle['station'] as Map<String, dynamic>, 'transmitting': false};
      final (store, _) = await _pump(tester, doc: idle);
      // Idle: the disabled colors, but it still takes a tap.
      expect(_on(tester, 'auto-stop-tx'), isTrue);
      expect(_stopBackground(tester), AppTheme.pane);
      expect(_stopFrame(tester), AppTheme.cardLine);
      expect(_stopLabel(tester), AppTheme.txtFaint);

      _live(store, busyAutoOpDoc());
      await tester.pump(const Duration(milliseconds: 300));
      expect(_stopBackground(tester), AppTheme.pane);
      // A red frame, no red fill.
      expect(_stopFrame(tester), AppTheme.red);
      expect(_stopLabel(tester), AppTheme.txt);
    });

    testWidgets('off, it has the disabled colors and no red', (tester) async {
      await _pump(tester, commands: 'none');
      expect(_on(tester, 'auto-stop-tx'), isFalse);
      expect(_stopBackground(tester, disabled: true), AppTheme.pane);
      expect(_stopLabel(tester), AppTheme.txtFaint);
    });
  });

  group('Approve and Reject', () {
    testWidgets('AE3: Approve sends the pending id, locks both until the result, and NO ANSWER after 5 s',
        (tester) async {
      final (_, mqtt) = await _pump(tester);
      expect(_on(tester, 'auto-approve'), isTrue);
      await _tap(tester, 'auto-approve');
      expect(mqtt.publishes, hasLength(1));
      final m = _cmd(mqtt.publishes.single);
      expect(m['action'], 'approve');
      expect(m['value'], 42);
      expect(_line(tester, 'auto-operate-line'), 'APPROVE SENT');

      // A second tap before the result sends nothing.
      expect(_on(tester, 'auto-approve'), isFalse);
      expect(_on(tester, 'auto-reject'), isFalse);
      await _tap(tester, 'auto-approve');
      await _tap(tester, 'auto-reject');
      expect(mqtt.publishes, hasLength(1));

      await tester.pump(const Duration(seconds: 5));
      expect(_line(tester, 'auto-operate-line'), 'APPROVE · NO ANSWER');
      expect(_on(tester, 'auto-approve'), isTrue);
    });

    testWidgets('the result with the same cmd_id unlocks and shows accepted', (tester) async {
      final (store, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-reject');
      final m = _cmd(mqtt.publishes.single);
      expect(m['action'], 'reject');
      expect(m['value'], 42);
      _live(store, {
        ...busyAutoOpDoc(),
        'remote': {
          'commands': 'operate',
          'last_cmd': {'cmd_id': m['cmd_id'], 'action': 'reject', 'result': 'accepted', 'reason': '', 'at_ms': busyNowMs},
        },
      });
      await tester.pump(const Duration(milliseconds: 300));
      expect(_line(tester, 'auto-operate-line'), 'REJECT ACCEPTED');
      expect(_on(tester, 'auto-approve'), isTrue);
    });

    testWidgets('a refused result shows its reason in amber next to the control', (tester) async {
      final (store, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-approve');
      final id = _cmd(mqtt.publishes.single)['cmd_id'];
      // A result of another command does not count.
      _live(store, {
        ...busyAutoOpDoc(),
        'remote': {
          'commands': 'operate',
          'last_cmd': {'cmd_id': 'other', 'action': 'mode', 'result': 'refused', 'reason': 'busy', 'at_ms': 1},
        },
      });
      await tester.pump(const Duration(milliseconds: 300));
      expect(_line(tester, 'auto-operate-line'), 'APPROVE SENT');
      _live(store, {
        ...busyAutoOpDoc(),
        'remote': {
          'commands': 'operate',
          'last_cmd': {
            'cmd_id': id,
            'action': 'approve',
            'result': 'refused',
            'reason': 'no proposal with this id',
            'at_ms': busyNowMs,
          },
        },
      });
      await tester.pump(const Duration(milliseconds: 300));
      expect(_line(tester, 'auto-operate-line'), 'APPROVE REFUSED · no proposal with this id');
      // The amber border marks the control that sent it.
      Color side(String k) => tester.widget<ElevatedButton>(_k(k)).style!.side!.resolve(<WidgetState>{})!.color;
      expect(side('auto-approve'), AppTheme.amber);
      expect(side('auto-reject'), isNot(AppTheme.amber));
    });

    testWidgets('no pending proposal: no Approve and no Reject', (tester) async {
      await _pump(tester, doc: {...busyAutoOpDoc(), 'pending': null});
      expect(_k('auto-approve'), findsNothing);
      expect(_k('auto-reject'), findsNothing);
    });

    testWidgets('AE9: a latched stop shows the latch text and no Resume', (tester) async {
      await _pump(tester, doc: {...busyAutoOpDoc(), 'latched': true, 'stop_reason': 'operator stop'});
      expect(_line(tester, 'auto-next-what'), 'LATCHED · resume at the station');
      expect(_line(tester, 'auto-next-why'), 'operator stop');
      expect(_k('auto-approve'), findsNothing);
      expect(_k('auto-reject'), findsNothing);
      expect(find.textContaining(RegExp('resume', caseSensitive: false)), findsOneWidget);
      expect(find.widgetWithText(ElevatedButton, 'RESUME'), findsNothing);
    });
  });

  group('stop only', () {
    for (final (name, commands, doc) in [
      ('commands stop', 'stop', busyAutoOpDoc()),
      (
        'a replay',
        'operate',
        {
          ...busyAutoOpDoc(),
          'session': {'state': 'running', 'rule_set': 'x', 'level': 'normal', 'mode': 'auto', 'source': 'replay'},
        }
      ),
    ]) {
      testWidgets('$name: only Stop TX is on', (tester) async {
        final (_, mqtt) = await _pump(tester, commands: commands, doc: doc);
        expect(_on(tester, 'auto-stop-tx'), isTrue);
        for (final k in _operate) {
          expect(_on(tester, k), isFalse, reason: k);
        }
        expect(_line(tester, 'auto-operate-line'), 'stop only');
        await _tap(tester, 'auto-presence-chip');
        await _tap(tester, 'auto-confirm');
        expect(mqtt.publishes, isEmpty);
        await _tap(tester, 'auto-stop-tx');
        expect(_cmd(mqtt.publishes.single)['action'], 'stop');
      });
    }
  });

  group('mode, target, presence', () {
    testWidgets('a tap on a band pill sends a band request, and the band shows amber', (tester) async {
      final doc = busyAutoOpDoc();
      doc['bands'] = [
        {'band': '40m', 'dial_hz': 7074000, 'seen': true, 'count': 3, 'period_start_ms': busyNowMs},
        {'band': '20m', 'dial_hz': 14074000, 'seen': true, 'count': 23, 'period_start_ms': busyNowMs},
      ];
      final (_, mqtt) = await _pump(tester, doc: doc);
      // The current band and a band outside the table send nothing.
      await tester.tap(_k('auto-band-20m'), warnIfMissed: false);
      await tester.tap(_k('auto-band-160m'), warnIfMissed: false);
      await tester.pump();
      await tester.pump();
      expect(mqtt.publishes, isEmpty);
      await tester.tap(_k('auto-band-40m'), warnIfMissed: false);
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));
      final m = _cmd(mqtt.publishes.single);
      expect(m['action'], 'band');
      expect(m['value'], '40m');
      await tester.pump(const Duration(milliseconds: 200));
      final box = tester.widget<Container>(_k('auto-band-40m')).decoration as BoxDecoration;
      expect((box.border as Border).top.color, AppTheme.amber);
      expect(_line(tester, 'auto-operate-line'), 'BAND 40m SENT');
    });

    testWidgets('ARM CQ in auto mode sends arm_cq with no value', (tester) async {
      final doc = busyAutoOpDoc();
      doc['arm'] = {'source': '', 'until_ms': 0};
      final (_, mqtt) = await _pump(tester, doc: doc);
      expect(_line(tester, 'auto-arm-cq-label'), 'ARM CQ');
      expect(_on(tester, 'auto-arm-cq'), isTrue);
      await _tap(tester, 'auto-arm-cq');
      final m = _cmd(mqtt.publishes.single);
      expect(m['action'], 'arm_cq');
      expect(m['value'], isNull);
    });

    testWidgets('ARM CQ is off outside auto mode, and confirm mode shows CQ CONFIRM', (tester) async {
      final doc = busyAutoOpDoc();
      (doc['session'] as Map<String, dynamic>)['mode'] = 'observe';
      doc['arm'] = {'source': '', 'until_ms': 0};
      await _pump(tester, doc: doc);
      expect(_on(tester, 'auto-arm-cq'), isFalse);
      expect(_line(tester, 'auto-arm-cq-sub'), 'auto only');

      final c = busyAutoOpDoc();
      (c['session'] as Map<String, dynamic>)['mode'] = 'confirm';
      c['arm'] = {'source': 'confirm', 'until_ms': 0};
      await _pump(tester, doc: c);
      expect(_on(tester, 'auto-arm-cq'), isFalse);
      expect(_line(tester, 'auto-arm-cq-sub'), 'CONFIRM');
    });

    testWidgets('an operator arm shows the time left, and a tap arms again', (tester) async {
      final doc = busyAutoOpDoc();
      doc['arm'] = {'source': 'operator', 'until_ms': busyNowMs + 25 * 60000 + 30000};
      final (_, mqtt) = await _pump(tester, doc: doc);
      expect(_line(tester, 'auto-arm-cq-label'), 'CQ');
      expect(_line(tester, 'auto-arm-cq-sub'), matches(RegExp(r'^25:[23]\d$')));
      await _tap(tester, 'auto-arm-cq');
      expect(_cmd(mqtt.publishes.single)['action'], 'arm_cq');
    });

    testWidgets('a refused arm_cq gives ARM CQ an amber border', (tester) async {
      final (store, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-arm-cq');
      final id = _cmd(mqtt.publishes.single)['cmd_id'];
      final doc = busyAutoOpDoc();
      doc['remote'] = {
        ...(doc['remote'] as Map<String, dynamic>? ?? {}),
        'last_cmd': {'cmd_id': id, 'action': 'arm_cq', 'result': 'refused', 'reason': 'not auto mode', 'at_ms': busyNowMs},
      };
      _live(store, doc);
      await tester.pump(const Duration(milliseconds: 300));
      expect(_line(tester, 'auto-operate-line'), 'ARM CQ REFUSED · not auto mode');
    });

    testWidgets('a tap on the active mode sends nothing, another mode sends mode', (tester) async {
      final (_, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-auto');
      expect(mqtt.publishes, isEmpty);
      await _tap(tester, 'auto-confirm');
      final m = _cmd(mqtt.publishes.single);
      expect(m['action'], 'mode');
      expect(m['value'], 'confirm');
    });

    testWidgets('Follow, then a second tap on the active Follow sends clear', (tester) async {
      final doc = busyAutoOpDoc();
      (doc['station'] as Map<String, dynamic>)['target_choice'] = '';
      final (store, mqtt) = await _pump(tester, doc: doc);
      await _tap(tester, 'auto-follow');
      var m = _cmd(mqtt.publishes.last);
      expect(m['action'], 'target');
      expect(m['value'], {'call': 'VK9XQ', 'choice': 'follow'});
      expect(mqtt.publishes.last.payload, contains('"value":{"call":"VK9XQ","choice":"follow"}'));

      _live(store, busyAutoOpDoc()); // target_choice follow
      await tester.pump(const Duration(milliseconds: 300));
      await _tap(tester, 'auto-follow');
      m = _cmd(mqtt.publishes.last);
      expect(m['value'], {'call': 'VK9XQ', 'choice': 'clear'});

      await _tap(tester, 'auto-skip');
      m = _cmd(mqtt.publishes.last);
      expect(m['value'], {'call': 'VK9XQ', 'choice': 'skip'});
      expect(mqtt.publishes, hasLength(3));
    });

    testWidgets('Follow and Skip are off without a DX call', (tester) async {
      final doc = busyAutoOpDoc();
      (doc['station'] as Map<String, dynamic>)['dx_call'] = '';
      await _pump(tester, doc: doc);
      expect(_on(tester, 'auto-follow'), isFalse);
      expect(_on(tester, 'auto-skip'), isFalse);
      expect(_on(tester, 'auto-confirm'), isTrue);
    });

    testWidgets('a tap on the presence chip sends touch', (tester) async {
      final (_, mqtt) = await _pump(tester);
      await _tap(tester, 'auto-presence-chip');
      final m = _cmd(mqtt.publishes.single);
      expect(m['action'], 'touch');
      expect(m['value'], isNull);
      expect(tester.getSize(_k('auto-presence-chip')).height, greaterThanOrEqualTo(48));
    });

    testWidgets('a refresh or a new view sends nothing', (tester) async {
      final (store, mqtt) = await _pump(tester);
      for (var i = 0; i < 5; i++) {
        _live(store, busyAutoOpDoc());
        await tester.pump(const Duration(seconds: 1));
      }
      store.markDisconnected();
      await tester.pump(const Duration(seconds: 1));
      store.markConnected(scheduleGraceNotify: false);
      await tester.pump(const Duration(seconds: 1));
      expect(mqtt.publishes, isEmpty);
    });
  });

  testWidgets('no control opens a dialog', (tester) async {
    final (_, mqtt) = await _pump(tester);
    for (final k in ['auto-presence-chip', 'auto-follow', 'auto-skip', 'auto-observe', 'auto-approve', 'auto-stop-tx']) {
      await _tap(tester, k);
      expect(find.byType(Dialog), findsNothing, reason: k);
      expect(find.byType(AlertDialog), findsNothing, reason: k);
      expect(find.byType(BottomSheet), findsNothing, reason: k);
    }
    expect(mqtt.publishes, hasLength(6));
  });

  testWidgets('/state.error shows as a red line in place of the action log', (tester) async {
    await _pump(tester, doc: {...busyAutoOpDoc(), 'error': 'rule cycle failed'});
    expect(find.text('75AUTO ERROR · rule cycle failed'), findsOneWidget);
  });
}
