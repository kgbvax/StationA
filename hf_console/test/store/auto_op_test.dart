import 'dart:convert';

import 'package:clock/clock.dart';
import 'package:fake_async/fake_async.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/auto_op.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/credential_store.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../support/fixtures.dart';

/// The key of the shared test vectors: the 32 bytes 0x00 to 0x1f. The Go side
/// tests the same vectors in internal/remote/command_test.go
/// (TestSignMatchesTheSharedTestVectors).
final _key = List<int>.generate(32, (i) => i);

void main() {
  group('AutoOpView.fromState', () {
    test('a full document parses into every field', () {
      final v = AutoOpView.fromState(autoOpStateDoc())!;
      expect(v.ts, '2026-11-26T14:32:41Z');
      expect(v.seq, 1234);
      expect(v.nowMs, 1795703561000);

      final se = v.session!;
      expect([se.state, se.ruleSet, se.level, se.mode, se.source],
          ['running', 'everyday.gess', 'normal', 'auto', 'live']);

      final st = v.station!;
      expect(st.band, '20m');
      expect(st.dialHz, 14074000);
      expect(st.mode, 'FT8');
      expect(st.dxCall, 'VK9XQ');
      expect(st.dxEntity, 'Christmas Island');
      expect(st.txDf, 1487);
      expect(st.rxDf, 1502);
      expect(st.txEnabled, isTrue);
      expect(st.transmitting, isFalse);
      expect(st.periodMs, 15000);
      expect(st.slotOffsetMs, 6100);
      expect(st.bandChange!.target, '17m');
      expect(st.bandChange!.result, 'pending');
      expect(st.decodeCount, 23);
      expect(st.lastDecodeEndMs, 1795703550000);
      expect(st.qsoStage, 'R+RPT');
      expect(st.tryCount, 0);
      expect(st.repeatCount, 1);
      expect(st.targetChoice, 'follow');

      expect(v.presenceEndMs, 1795707161000);

      final p = v.pending!;
      expect([p.id, p.rule, p.action, p.call, p.band, p.reason, p.priority, p.deadlineMs, p.expired],
          [42, 'call-needed', 'call-station', 'VK9XQ', '20m', 'dxcc VK9', 50, 1795703570000, false]);

      final c = v.current!;
      expect([c.id, c.timeMs, c.rule, c.action, c.call, c.band, c.result, c.cause, c.gateReason, c.reason],
          [41, 1795703535000, 'call-needed', 'call-station', 'VK9XQ', '20m', 'took-effect', '', '', 'dxcc VK9']);

      final idle = v.idle!;
      expect(idle.proposed, 0);
      expect(idle.blocks!.single.call, 'DL1ABC');
      expect(idle.blocks!.single.why, 'worked before');
      expect(idle.moreBlocks, 2);

      expect(v.hint, 'waiting for the answer');
      expect(v.arm!.source, 'operator');
      expect(v.arm!.untilMs, 1795703900000);
      expect(v.latched, isFalse);
      expect(v.stopUnconfirmed, isFalse);
      expect(v.stopReason, '');
      expect(v.cycleError, '');

      expect(v.decodes, hasLength(2));
      final d0 = v.decodes![0];
      expect([d0.periodStartMs, d0.df, d0.snr, d0.text, d0.call], [1795703535000, 1502, -14, 'DL2XYZ VK9XQ -11', 'VK9XQ']);
      expect([d0.needed, d0.cq, d0.toMe, d0.latest], [true, false, true, true]);
      final d1 = v.decodes![1];
      // The booleans left out of the document read as false (contract §5.3).
      expect([d1.needed, d1.cq, d1.toMe, d1.latest], [false, true, false, false]);

      final tx = v.txLines!.single;
      expect([tx.periodStartMs, tx.band, tx.df, tx.text], [1795703520000, '20m', 1487, 'VK9XQ DL2XYZ R-09']);

      expect(v.bands, hasLength(2));
      final b0 = v.bands![0];
      expect([b0.band, b0.dialHz, b0.seen, b0.count, b0.periodStartMs], ['40m', 7074000, true, 12, 1795702000000]);
      expect(v.bands![1].seen, isFalse);

      final l = v.log!.single;
      expect([l.id, l.result, l.cause, l.gateReason], [40, 'refused', 'gate', 'tx limit']);

      final h = v.health!.single;
      expect([h.interface, h.name, h.fresh, h.reason], ['digimode', 'WSJT-X', true, '']);

      final r = v.rig!;
      expect([r.present, r.ok, r.name, r.reason], [true, true, 'rigctld', '']);
      expect(v.clockFailing, isFalse);

      expect(v.remote!.commands, 'operate');
      final lc = v.remote!.lastCmd!;
      expect([lc.cmdId, lc.action, lc.result, lc.reason, lc.atMs],
          ['c1f0e7a2c1f0e7a2', 'target', 'accepted', '', 1795703561500]);
      expect(v.error, '');
    });

    test('a document without session.mode gives a null mode', () {
      final doc = autoOpStateDoc();
      (doc['session'] as Map).remove('mode');
      final v = AutoOpView.fromState(doc)!;
      expect(v.session!.mode, isNull);
      expect(v.session!.state, 'running');
    });

    test('missing and wrong values give null, never a default', () {
      final v = AutoOpView.fromState({
        'seq': 'x',
        'latched': 'yes',
        'station': {'tx_enabled': 1, 'dial_hz': 14074000.5, 'band': 20},
      })!;
      expect(v.seq, isNull);
      expect(v.latched, isNull);
      expect(v.stopUnconfirmed, isNull);
      expect(v.session, isNull);
      expect(v.pending, isNull);
      expect(v.decodes, isNull);
      expect(v.station!.txEnabled, isNull);
      expect(v.station!.dialHz, isNull);
      expect(v.station!.band, isNull);
    });

    test('null objects stay null', () {
      final v = AutoOpView.fromState({...autoOpStateDoc(), 'pending': null, 'current': null, 'idle': null})!;
      expect(v.pending, isNull);
      expect(v.current, isNull);
      expect(v.idle, isNull);
    });

    test('start carries the rule sets and the last choice', () {
      final v = AutoOpView.fromState(autoOpStateDoc())!;
      expect(v.start!.ruleSets, ['everyday.gess', 'hunt.gess']);
      expect(v.start!.last!.ruleSet, 'everyday.gess');
      expect(v.start!.last!.mode, 'confirm');
    });

    test('start.last is null before the first live start, and a missing start is null', () {
      final v = AutoOpView.fromState({
        ...autoOpStateDoc(),
        'start': {'rule_sets': ['everyday.gess'], 'last': null},
      })!;
      expect(v.start!.ruleSets, ['everyday.gess']);
      expect(v.start!.last, isNull);
      expect(AutoOpView.fromState({...autoOpStateDoc()}..remove('start'))!.start, isNull);
    });

    for (final (name, change) in [
      ('a decode list of numbers', <String, dynamic>{'decodes': [1, 2, 3]}),
      ('decodes as an object', <String, dynamic>{'decodes': <String, dynamic>{'df': 1}}),
      ('session as a string', <String, dynamic>{'session': 'running'}),
      ('pending as a number', <String, dynamic>{'pending': 42}),
      ('start as a string', <String, dynamic>{'start': 'everyday.gess'}),
      ('rule sets as a string', <String, dynamic>{'start': <String, dynamic>{'rule_sets': 'everyday.gess'}}),
      ('a rule set that is a number', <String, dynamic>{'start': <String, dynamic>{'rule_sets': [1]}}),
    ]) {
      test('$name is malformed and does not throw', () {
        expect(AutoOpView.fromState({...autoOpStateDoc(), ...change}), isNull);
      });
    }
  });

  group('AutoOpMeta.fromMeta', () {
    test('reads the capabilities', () {
      final m = AutoOpMeta.fromMeta({
        'host': 'bwpc',
        'capabilities': {
          'bands': ['40m', '20m'],
          'commands': 'stop',
          'signed': true,
        },
      });
      expect(m.host, 'bwpc');
      expect(m.bands, ['40m', '20m']);
      expect(m.commands, 'stop');
      expect(m.signed, isTrue);
    });

    test('an unknown command level and a missing band list give null', () {
      final m = AutoOpMeta.fromMeta({
        'capabilities': {'commands': 'everything'},
      });
      expect(m.commands, isNull);
      expect(m.bands, isNull);
      expect(m.signed, isNull);
    });
  });

  group('autoOpCmd', () {
    test('the signature matches the shared test vectors', () {
      expect(signAutoOpCmd(_key, 'stop', 'null', '2026-11-26T14:32:41Z', '0123456789abcdef'),
          '82dc9832b2e636f3f3dbf08d09aa98bd20b7972c1bf2d6cd00cfa75fb3021f6f');
      expect(
          signAutoOpCmd(
              _key, 'target', jsonEncode({'call': 'VK9XQ', 'choice': 'follow'}), '2026-11-26T14:32:41Z', 'c1f0e7a2c1f0e7a2'),
          'e0753b12b701eecfff948a772992e4f0b51e887bc0ffef0355f4e5e9010acfe8');
    });

    test('the signed text has the keys in order and no spaces', () {
      expect(autoOpSignedText('target', jsonEncode({'call': 'VK9XQ', 'choice': 'follow'}), '2026-11-26T14:32:41Z', 'c1f0e7a2'),
          '{"action":"target","value":{"call":"VK9XQ","choice":"follow"},"ts":"2026-11-26T14:32:41Z","cmd_id":"c1f0e7a2"}');
    });

    test("autoOpCmd('stop', null) has a UTC whole-second ts, a 16-character cmd_id, and the vector sig", () {
      final m = jsonDecode(autoOpCmd('stop', null,
          key: _key, now: DateTime.utc(2026, 11, 26, 14, 32, 41, 678), cmdId: '0123456789abcdef')) as Map;
      expect(m.keys, ['action', 'value', 'ts', 'cmd_id', 'sig']);
      expect(m['action'], 'stop');
      expect(m['value'], isNull);
      expect(m['ts'], '2026-11-26T14:32:41Z');
      expect(m['cmd_id'], '0123456789abcdef');
      expect(m['sig'], '82dc9832b2e636f3f3dbf08d09aa98bd20b7972c1bf2d6cd00cfa75fb3021f6f');
    });

    test('a Follow matches its vector', () {
      final m = jsonDecode(autoOpCmd('target', {'call': 'VK9XQ', 'choice': 'follow'},
          key: _key, now: DateTime.utc(2026, 11, 26, 14, 32, 41), cmdId: 'c1f0e7a2c1f0e7a2')) as Map;
      expect(m['value'], {'call': 'VK9XQ', 'choice': 'follow'});
      expect(m['sig'], 'e0753b12b701eecfff948a772992e4f0b51e887bc0ffef0355f4e5e9010acfe8');
    });

    test('without an injected id and time: a fresh 16-hex cmd_id and a UTC Z ts', () {
      final a = jsonDecode(autoOpCmd('stop', null, key: _key)) as Map;
      final b = jsonDecode(autoOpCmd('stop', null, key: _key)) as Map;
      expect(a['cmd_id'], matches(RegExp(r'^[0-9a-f]{16}$')));
      expect(a['cmd_id'], isNot(b['cmd_id']));
      expect(a['ts'], matches(RegExp(r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$')));
      expect(a['sig'], signAutoOpCmd(_key, 'stop', 'null', a['ts'] as String, a['cmd_id'] as String));
    });

    test('start_session signs its value with rule_set before mode', () {
      final m = jsonDecode(autoOpCmd('start_session', autoStartSessionValue('everyday.gess', 'auto'),
          key: _key, now: DateTime.utc(2026, 11, 26, 14, 32, 41), cmdId: '0123456789abcdef')) as Map;
      expect(m['action'], 'start_session');
      expect(m['value'], {'rule_set': 'everyday.gess', 'mode': 'auto'});
      expect(m['sig'],
          signAutoOpCmd(_key, 'start_session', '{"rule_set":"everyday.gess","mode":"auto"}', m['ts'] as String, '0123456789abcdef'));
    });

    test('a local time is sent as UTC', () {
      final local = DateTime.utc(2026, 11, 26, 14, 32, 41).toLocal();
      final m = jsonDecode(autoOpCmd('touch', null, key: _key, now: local, cmdId: '0000000000000000')) as Map;
      expect(m['ts'], '2026-11-26T14:32:41Z');
    });
  });

  group('parseAutoOpKey', () {
    test('takes 64 hex digits in either case', () {
      final hex = List.generate(32, (i) => i.toRadixString(16).padLeft(2, '0')).join();
      expect(parseAutoOpKey(hex), _key);
      expect(parseAutoOpKey(' ${hex.toUpperCase()} '), _key);
    });

    test('refuses any other text', () {
      expect(parseAutoOpKey(''), isNull);
      expect(parseAutoOpKey('00' * 31), isNull);
      expect(parseAutoOpKey('00' * 33), isNull);
      expect(parseAutoOpKey('zz' * 32), isNull);
    });
  });

  group('CredentialStore command key (SharedPreferences path)', () {
    TestWidgetsFlutterBinding.ensureInitialized();
    setUp(() => SharedPreferences.setMockInitialValues({}));

    test('stores, reads, and clears the key, and readAll leaves it out', () async {
      final store = CredentialStore();
      expect(await store.readAutoOpKey(), isNull);
      expect(await store.writeAutoOpKey('nope'), isFalse);
      expect(await store.readAutoOpKey(), isNull);
      final hex = List.generate(32, (i) => i.toRadixString(16).padLeft(2, '0')).join().toUpperCase();
      expect(await store.writeAutoOpKey(hex), isTrue);
      expect(await store.readAutoOpKey(), _key);
      expect((await store.readAll()).containsKey(autoOpKeyStorageKey), isFalse);
      await store.clearAutoOpKey();
      expect(await store.readAutoOpKey(), isNull);
    });
  });

  group('BusStore and the auto-op slot', () {
    test('holds the parsed view, meta, and beat', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.setAutoOp(commands: 'stop');
      expect(store.autoOp!.station!.dxCall, 'VK9XQ');
      expect(store.autoOpMeta!.commands, 'stop');
      expect(store.autoOpMeta!.bands, hasLength(8));
      store.applyAutoOpBeat(stateSeq: 1235, slotOffsetMs: 9000);
      expect(store.autoOpBeat!.stateSeq, 1235);
      expect(store.autoOpBeat!.slotOffsetMs, 9000);
      expect(store.slots[autoOpSlot]!.isOnline, isTrue);
    });

    test('a malformed document counts and keeps the last good view', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.setAutoOp();
      final before = store.malformedPayloads;
      expect(
          () => store.apply('$autoOpSlot/state', jsonEncode({...autoOpStateDoc(seq: 1235), 'decodes': [1, 2]}), false),
          returnsNormally);
      expect(store.malformedPayloads, before + 1);
      expect(store.autoOp!.seq, 1234);
      // A malformed document proves no fresh view.
      expect(store.stateFresh(autoOpSlot), isFalse);
    });

    test('an empty retained /state clears the view', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.setAutoOp();
      store.apply('$autoOpSlot/state', '', true);
      expect(store.autoOp, isNull);
    });

    test('a malformed beat counts and is not fresh', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.apply('$autoOpSlot/beat', '[1]', false);
      expect(store.malformedPayloads, 1);
      expect(store.stateFresh(autoOpSlot), isFalse);
    });

    test('a beat alone creates no slot (no bridge-down row)', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.applyAutoOpBeat();
      expect(store.slots[autoOpSlot], isNull);
      expect(store.offlineList.where((e) => e.startsWith(autoOpSlot)), isEmpty);
    });
  });

  group('freshness (contract §6)', () {
    test('a retained /state at connect is not fresh, a new seq is', () {
      fakeAsync((fa) {
        withClock(fa.getClock(DateTime.utc(2026, 11, 26, 14)), () {
          final store = BusStore()..markConnected(scheduleGraceNotify: false);
          store.setAutoOp(seq: 10);
          expect(store.stateFresh(autoOpSlot), isFalse);
          expect(store.stateAge(autoOpSlot), isNull);

          // The same seq again (a second retained copy) stays not fresh.
          store.setAutoOp(seq: 10, retained: false);
          expect(store.stateFresh(autoOpSlot), isFalse);

          store.setAutoOp(seq: 11, retained: false);
          expect(store.stateFresh(autoOpSlot), isTrue);
          expect(store.stateAge(autoOpSlot), Duration.zero);
        });
      });
    });

    test('a retained /state with a new seq is still not fresh', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.setAutoOp(seq: 10);
      store.setAutoOp(seq: 11);
      expect(store.stateFresh(autoOpSlot), isFalse);
    });

    test('a /beat makes the data fresh', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.setAutoOp(seq: 10);
      store.applyAutoOpBeat(stateSeq: 10);
      expect(store.stateFresh(autoOpSlot), isTrue);
    });

    test('stateAge grows with the clock and resets on a new seq or a beat', () {
      fakeAsync((fa) {
        withClock(fa.getClock(DateTime.utc(2026, 11, 26, 14)), () {
          final store = BusStore()..markConnected(scheduleGraceNotify: false);
          store.setAutoOp(seq: 10);
          store.applyAutoOpBeat(stateSeq: 10);
          fa.elapse(const Duration(seconds: 3));
          expect(store.stateAge(autoOpSlot), const Duration(seconds: 3));
          store.setAutoOp(seq: 11, retained: false);
          expect(store.stateAge(autoOpSlot), Duration.zero);
          fa.elapse(const Duration(seconds: 7));
          expect(store.stateAge(autoOpSlot), const Duration(seconds: 7));
          store.applyAutoOpBeat(stateSeq: 11);
          expect(store.stateAge(autoOpSlot), Duration.zero);
        });
      });
    });

    test('a reconnect resets the freshness and drops the old beat', () {
      final store = BusStore()..markConnected(scheduleGraceNotify: false);
      store.setAutoOp(seq: 10);
      store.applyAutoOpBeat(stateSeq: 10);
      expect(store.stateFresh(autoOpSlot), isTrue);
      store.markDisconnected();
      expect(store.stateFresh(autoOpSlot), isFalse, reason: 'not fresh while the link is down');
      store.markConnected(scheduleGraceNotify: false);
      expect(store.stateFresh(autoOpSlot), isFalse);
      expect(store.stateAge(autoOpSlot), isNull);
      expect(store.autoOpBeat, isNull);
      // The retained copy that the broker sends again is not fresh.
      store.setAutoOp(seq: 12);
      expect(store.stateFresh(autoOpSlot), isFalse);
      expect(store.autoOp!.seq, 12, reason: 'the view itself is kept and shown as retained');
    });
  });
}
