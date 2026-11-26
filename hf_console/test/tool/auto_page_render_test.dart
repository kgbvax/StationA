// auto_page_render_test.dart — renders the 75AUTO page per device and
// scenario to PNGs under $PAGE_OUT (default /tmp/auto-page), bundled fonts.
// Skipped unless PAGE_RENDER=1.
//
//   PAGE_RENDER=1 flutter test test/tool/auto_page_render_test.dart

import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../support/auto_op_fixtures.dart';
import '../support/fake_mqtt_service.dart';
import '../support/fixtures.dart';
import '../support/render_support.dart';
import '../support/station_fixtures.dart';
import '../support/test_harness.dart';

enum _Data { fresh, stale, retained, offline, linkDown }

void main() {
  final enabled = Platform.environment['PAGE_RENDER'] == '1';

  testWidgets('render the 75AUTO page', skip: !enabled, (tester) async {
    await tester.runAsync(loadRenderFonts);
    SharedPreferences.setMockInitialValues({});
    final dir = Directory(Platform.environment['PAGE_OUT'] ?? '/tmp/auto-page')..createSync(recursive: true);

    Future<void> shoot(String name, Size physical, double dpr, _Data data) async {
      tester.view.devicePixelRatio = dpr;
      tester.view.physicalSize = physical;
      addTearDown(tester.view.reset);
      final store = BusStore()..seedScenario(StationScenario.running);
      store.seedAutoOp(busyAutoOpDoc(), status: data == _Data.offline ? 'offline' : 'online');
      final mqtt = FakeMqttService(store);
      final key = GlobalKey();
      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: RepaintBoundary(key: key, child: const ConsoleScreen()),
      ));
      await tester.pump(const Duration(milliseconds: 100));
      await tester.tap(find.widgetWithText(InkWell, '75AUTO'));
      await tester.pump(const Duration(milliseconds: 100));
      if (data != _Data.retained && data != _Data.offline) {
        store.liveAutoOp(busyAutoOpDoc(seq: 2001));
        store.applyAutoOpBeat(stateSeq: 2001, nowMs: busyNowMs + 400, slotOffsetMs: 11400);
      }
      await tester.pump(const Duration(milliseconds: 400));
      if (data == _Data.stale) await tester.pump(const Duration(seconds: 8));
      if (data == _Data.linkDown) {
        store.markDisconnected();
        mqtt.connected.value = false;
        await tester.pump(const Duration(milliseconds: 300));
      }
      await shootPng(tester, key, '${dir.path}/$name.png');
      await tester.pumpWidget(const SizedBox());
    }

    // About 914 x 411 dp: a phone in landscape, as mirrored to the car.
    const phoneL = Size(2400, 1080);
    await shoot('phone-landscape', phoneL, 2.625, _Data.fresh);
    await shoot('phone-landscape-linkdown', phoneL, 2.625, _Data.linkDown);
    await shoot('phone-landscape-stale', phoneL, 2.625, _Data.stale);
    await shoot('phone-landscape-retained', phoneL, 2.625, _Data.retained);
    await shoot('phone-landscape-offline', phoneL, 2.625, _Data.offline);
    await shoot('phone-portrait', const Size(1170, 2532), 3, _Data.fresh);
    await shoot('tablet', const Size(2001, 1200), 1.5, _Data.fresh);
  });
}
