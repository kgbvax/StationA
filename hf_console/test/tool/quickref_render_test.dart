// quickref_render_test.dart — renders every quick-reference sheet
// (quickref_sheets.dart) on the tablet console, resolves each call-out to its
// control's rect, and writes the self-contained docs/quickref/
// hf-console-quickref.html from docs/quickref/template.html (screenshots
// inlined). The PNGs and the call-out JSON also go to $QUICKREF_OUT
// (default /tmp/quickref) for eyeballing. Skipped unless QUICKREF_RENDER=1;
// docs/quickref/build.sh runs it and prints the PDF.

import 'dart:convert';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import '../support/render_support.dart';
import 'quickref_sheets.dart';

void main() {
  final enabled = Platform.environment['QUICKREF_RENDER'] == '1';

  testWidgets('render the quick-reference sheets', skip: !enabled, (tester) async {
    await tester.runAsync(loadRenderFonts);
    setTabletView(tester);
    final out = Directory(Platform.environment['QUICKREF_OUT'] ?? '/tmp/quickref')..createSync(recursive: true);

    final sheets = <Map<String, dynamic>>[];
    for (final sheet in quickrefSheets) {
      final key = GlobalKey();
      await pumpSheet(tester, sheet, key);
      final png = '${out.path}/${sheet.id}.png';
      await shootPng(tester, key, png);
      sheets.add({
        'id': sheet.id,
        'title': sheet.title,
        'purpose': sheet.purpose,
        'width': tabletLogicalSize.width,
        'height': tabletLogicalSize.height,
        'image': 'data:image/png;base64,${base64Encode(File(png).readAsBytesSync())}',
        'callouts': [
          for (final c in sheet.callouts)
            {
              'title': c.title,
              'text': c.text,
              'rect': switch (c.anchor.resolve(tester)) {
                final r => [r.left, r.top, r.width, r.height].map((v) => double.parse(v.toStringAsFixed(1))).toList(),
              },
            },
        ],
      });
    }

    final json = const JsonEncoder.withIndent(' ').convert({'sheets': sheets});
    File('${out.path}/quickref.json').writeAsStringSync(json.replaceAll(RegExp(r'"data:image/png;base64,[^"]*"'), '"…"'));
    final template = File('docs/quickref/template.html').readAsStringSync();
    const slot = '/*QUICKREF_DATA*/null';
    expect(template.contains(slot), isTrue, reason: 'template.html lost its data slot');
    File('docs/quickref/hf-console-quickref.html').writeAsStringSync(template.replaceFirst(slot, jsonEncode({'sheets': sheets})));
  });
}
