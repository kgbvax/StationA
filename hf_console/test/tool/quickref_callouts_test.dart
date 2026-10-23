// quickref_callouts_test.dart — drift guard for the quick reference
// (docs/quickref/): every call-out must still find its control on the seeded
// page and land on screen. A failure means a control was renamed, moved or
// removed: fix the call-out in quickref_sheets.dart and rebuild the sheets
// with docs/quickref/build.sh.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import '../support/render_support.dart';
import 'quickref_sheets.dart';

void main() {
  const screen = Rect.fromLTWH(0, 0, 1334, 800);

  for (final sheet in quickrefSheets) {
    testWidgets('quick reference "${sheet.title}": every call-out finds its control', (tester) async {
      setTabletView(tester);
      await pumpSheet(tester, sheet, GlobalKey());
      for (final c in sheet.callouts) {
        final Rect r;
        try {
          r = c.anchor.resolve(tester);
        } on StateError catch (e) {
          fail('call-out "${c.title}": ${e.message}');
        }
        expect(screen.inflate(1).contains(r.topLeft) && screen.inflate(1).contains(r.bottomRight), isTrue,
            reason: 'call-out "${c.title}" (${c.anchor.describe}) is off screen: $r');
      }
    });
  }
}
