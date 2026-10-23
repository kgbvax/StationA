// render_support.dart — shared plumbing for the opt-in PNG render tests
// (test/tool/*_render_test.dart): the tablet view, the bundled fonts plus the
// SDK's Material icon font, and a RepaintBoundary → PNG writer.

import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

const _families = {
  'IBMPlexMono': ['Regular', 'Medium', 'SemiBold', 'Bold'],
  'IBMPlexSans': ['Regular', 'Medium', 'SemiBold', 'Bold'],
  'SairaCondensed': ['Regular', 'Medium', 'SemiBold', 'Bold'],
};

ByteData _bytes(String path) => ByteData.view(Uint8List.fromList(File(path).readAsBytesSync()).buffer);

/// Loads the bundled fonts, and the SDK's MaterialIcons when it can be found
/// (tests otherwise draw every icon as a box). Run inside `tester.runAsync`.
Future<void> loadRenderFonts() async {
  for (final MapEntry(key: family, value: weights) in _families.entries) {
    final loader = FontLoader(family);
    for (final w in weights) {
      loader.addFont(Future.value(_bytes('assets/fonts/$family-$w.ttf')));
    }
    await loader.load();
  }
  final icons = _materialIconsPath();
  if (icons != null) {
    await (FontLoader('MaterialIcons')..addFont(Future.value(_bytes(icons)))).load();
  }
}

String? _materialIconsPath() {
  const rel = 'bin/cache/artifacts/material_fonts/MaterialIcons-Regular.otf';
  final roots = [
    Platform.environment['FLUTTER_ROOT'],
    // `flutter test` runs flutter_tester from <root>/bin/cache/artifacts/engine/<platform>/.
    File(Platform.resolvedExecutable).parent.parent.parent.parent.parent.parent.path,
  ];
  for (final r in roots) {
    if (r != null && File('$r/$rel').existsSync()) return '$r/$rel';
  }
  return null;
}

/// Lenovo Tab landscape: logical 1334 x 800 at dpr 1.5.
const tabletLogicalSize = Size(1334, 800);

void setTabletView(WidgetTester tester) {
  tester.view.devicePixelRatio = 1.5;
  tester.view.physicalSize = const Size(2001, 1200);
  addTearDown(tester.view.reset);
}

/// Writes the boundary under [key] to [path] at [pixelRatio] image pixels
/// per logical pixel (1 = the logical size; print renders want 2).
Future<void> shootPng(WidgetTester tester, GlobalKey key, String path, {double pixelRatio = 1}) async {
  final boundary = key.currentContext!.findRenderObject()! as RenderRepaintBoundary;
  await tester.runAsync(() async {
    final ui.Image img = await boundary.toImage(pixelRatio: pixelRatio);
    final png = await img.toByteData(format: ui.ImageByteFormat.png);
    File(path).writeAsBytesSync(png!.buffer.asUint8List());
  });
}
