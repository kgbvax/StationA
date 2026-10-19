// station_schematic_render_test.dart — renders the Station page schematic
// for every scenario × colour scheme to PNGs under $SCHEMATIC_OUT (default
// /tmp/schematic) with the bundled fonts, for eyeballing. Skipped unless
// SCHEMATIC_RENDER=1.

import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/station_schematic.dart';

import '../support/station_fixtures.dart';
import '../support/test_harness.dart';

const _families = {
  'IBMPlexMono': ['Regular', 'Medium', 'SemiBold', 'Bold'],
  'IBMPlexSans': ['Regular', 'Medium', 'SemiBold', 'Bold'],
  'SairaCondensed': ['Regular', 'Medium', 'SemiBold', 'Bold'],
};

Future<void> _loadFonts() async {
  for (final MapEntry(key: family, value: weights) in _families.entries) {
    final loader = FontLoader(family);
    for (final w in weights) {
      final bytes = File('assets/fonts/$family-$w.ttf').readAsBytesSync();
      loader.addFont(Future.value(ByteData.view(Uint8List.fromList(bytes).buffer)));
    }
    await loader.load();
  }
}

void main() {
  final enabled = Platform.environment['SCHEMATIC_RENDER'] == '1';
  final size = Size(
    double.tryParse(Platform.environment['SCHEMATIC_W'] ?? '') ?? 747,
    double.tryParse(Platform.environment['SCHEMATIC_H'] ?? '') ?? 770,
  );

  testWidgets('render station schematic grid', skip: !enabled, (tester) async {
    await tester.runAsync(_loadFonts);
    final dir = Directory(Platform.environment['SCHEMATIC_OUT'] ?? '/tmp/schematic')..createSync(recursive: true);
    await tester.binding.setSurfaceSize(Size(size.width + 40, size.height + 40));
    addTearDown(() => AppTheme.setScheme(AppColorScheme.dc));

    for (final scheme in AppColorScheme.values) {
      AppTheme.setScheme(scheme);
      for (final scenario in StationScenario.values) {
        final store = BusStore()..seedScenario(scenario);
        final key = GlobalKey();
        await tester.pumpWidget(TestHarness(
          store: store,
          child: Center(
            child: RepaintBoundary(
              key: key,
              child: Container(
                width: size.width,
                height: size.height,
                color: AppTheme.pane,
                child: const StationSchematic(),
              ),
            ),
          ),
        ));
        if (scenario == StationScenario.linkDown) store.markDisconnected();
        await tester.pump();
        if (Platform.environment['SCHEMATIC_TAP'] case final id? when scenario == StationScenario.running) {
          await tester.tap(find.byKey(ValueKey('schematic-hit-$id')));
          await tester.pump();
        }
        final boundary = key.currentContext!.findRenderObject()! as RenderRepaintBoundary;
        await tester.runAsync(() async {
          final ui.Image img = await boundary.toImage(pixelRatio: 2);
          final png = await img.toByteData(format: ui.ImageByteFormat.png);
          File('${dir.path}/${scheme.name}-${scenario.name}.png').writeAsBytesSync(png!.buffer.asUint8List());
        });
      }
    }
  });
}
