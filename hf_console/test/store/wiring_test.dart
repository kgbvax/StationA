// wiring_test.dart — bus-wiring contract tests for the sat-rotator slots
// (U8): the one-shot /cmd retention posture (KTD13) and the value-key
// payload shapes (R3). Every panel consumer null-asserts on cmdRetain, so
// a missing entry is a crash, not a soft failure — pinned here.

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

    test('pol-ctrl stays retained (the Tier-2 polarization control adds it)',
        () {
      // Absent today is fine; it must never read false — polarization is a
      // settable steady state (the ant-switch actuator exception).
      expect(cmdRetain['muehle/uhf/pol-ctrl'] ?? true, isTrue);
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
}