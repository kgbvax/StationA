// wiring_test.dart — bus-wiring contract tests for the sat-rotator slots
// (U8) and the pol-ctrl slot (U11): the /cmd retention postures (KTD13 and
// its retained-steady-state contrast) and the value-key payload shapes
// (R3). Every panel consumer null-asserts on cmdRetain, so a missing entry
// is a crash, not a soft failure — pinned here.

import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/wiring.dart';

void main() {
  group('cmdRetain — sat rotator slots', () {
    test('both rotator slots are one-shot (non-retained)', () {
      expect(cmdRetain['muehle/uhf/az-rotator'], isFalse,
          reason: 'KTD13: a stale retained goto must never replay');
      expect(cmdRetain['muehle/uhf/el-rotator'], isFalse);
    });

    test('pol-ctrl is retained steady state (the Tier-2 polarization control)',
        () {
      // Polarization is a settable steady state — the ant-switch actuator
      // exception, the deliberate contrast with the one-shot rotators
      // (KTD13): a retained set_pol re-applies the last intent after a
      // controller reboot or broker reconnect.
      expect(cmdRetain['muehle/uhf/pol-ctrl'], isTrue);
    });

    test('hf slots keep their existing retention posture', () {
      expect(cmdRetain['muehle/hf/rotator'], isFalse);
      expect(cmdRetain['muehle/hf/ant-ctrl'], isTrue);
      expect(cmdRetain['muehle/power/master'], isTrue);
    });
  });

  group('expectedSlots — sat rotator slots', () {
    test('both rotator slots are monitored (silent reporting)', () {
      expect(expectedSlots, contains('muehle/uhf/az-rotator'));
      expect(expectedSlots, contains('muehle/uhf/el-rotator'));
      // The existing PTS pan/tilt slot stays monitored.
      expect(expectedSlots, contains('muehle/uhf/rotator'));
    });
  });

  group('sat rotator payload builders', () {
    test('goto carries degrees under the value key', () {
      final payload = jsonDecode(satRotatorGotoPayload(45.0)) as Map<String, dynamic>;
      expect(payload, {'action': 'goto', 'value': '45.0'});
    });

    test('goto fractional degrees survive round-trip', () {
      final payload = jsonDecode(satRotatorGotoPayload(12.5)) as Map<String, dynamic>;
      expect(payload['value'], '12.5');
    });

    test('stop has no argument', () {
      expect(jsonDecode(satRotatorStopPayload()), {'action': 'stop'});
    });

    test('cmdTopic addresses the uhf slots', () {
      expect(cmdTopic('uhf/az-rotator'), 'muehle/uhf/az-rotator/cmd');
      expect(cmdTopic('uhf/el-rotator'), 'muehle/uhf/el-rotator/cmd');
    });
  });

  group('pol-ctrl payload builders', () {
    test('set_pol carries the phase under the value key', () {
      final payload = jsonDecode(setPolPayload('cl')) as Map<String, dynamic>;
      expect(payload, {'action': 'set_pol', 'value': 'cl'});
    });

    test('set_pol passes each vocabulary value through verbatim', () {
      for (final pol in ['h', 'v', 'cl', 'cr']) {
        expect(jsonDecode(setPolPayload(pol)), {'action': 'set_pol', 'value': pol});
      }
    });

    test('cmdTopic addresses the pol-ctrl slot', () {
      expect(cmdTopic('uhf/pol-ctrl'), 'muehle/uhf/pol-ctrl/cmd');
    });
  });

  group('cmdRetain — UHF radio slot (receive-only)', () {
    test('muehle/uhf/radio is one-shot (non-retained)', () {
      expect(cmdRetain['muehle/uhf/radio'], isFalse,
          reason: 'KTD6: a stale queued audio/monitor demand must never '
              're-fire after a bridge restart');
    });

    test('the radio slot is expected (monitored when silent)', () {
      expect(expectedSlots, contains('muehle/uhf/radio'));
    });
  });

  group('UHF radio payload builders (receive-only action set)', () {
    test('the five actions carry no value — the action name is the intent',
        () {
      expect(jsonDecode(uhfRadioAudioOnPayload()), {'action': 'audio_on'});
      expect(jsonDecode(uhfRadioAudioOffPayload()), {'action': 'audio_off'});
      expect(jsonDecode(uhfRadioPowerOnPayload()), {'action': 'power_on'});
      expect(jsonDecode(uhfRadioMonitorOnPayload()), {'action': 'monitor_on'});
      expect(jsonDecode(uhfRadioMonitorOffPayload()),
          {'action': 'monitor_off'});
    });

    // The control builders (set_freq/set_mode/arm/disarm/ptt) are deleted,
    // not stubbed: any attempt to re-add a caller is a compile error, and
    // the analyzer is the guard here — the bridge rejects those actions
    // regardless (TestControlActionsRejected Go-side).

    test('cmdTopic addresses the radio slot', () {
      expect(cmdTopic('uhf/radio'), 'muehle/uhf/radio/cmd');
    });
  });
}