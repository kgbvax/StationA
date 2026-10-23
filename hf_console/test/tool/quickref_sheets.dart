// quickref_sheets.dart — the hf_console quick reference (docs/quickref/):
// one sheet per console page, each a seeded tablet render plus call-outs.
// A call-out names its control by what the operator sees (label, tooltip,
// widget type, key); the render test resolves it to the control's rect, so
// re-rendering keeps every leader on its control. quickref_callouts_test.dart
// fails `flutter test` when a call-out no longer finds its control.

import 'dart:convert';
import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/dxspot/world_geometry.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:hf_console/ui/widgets/ant_mode_panel.dart';
import 'package:hf_console/ui/widgets/antenna_panel.dart';
import 'package:hf_console/ui/widgets/cam_feed_panel.dart';
import 'package:hf_console/ui/widgets/cam_radio_controls.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import 'package:hf_console/ui/widgets/dvk_panel.dart';
import 'package:hf_console/ui/widgets/faults_bar.dart';
import 'package:hf_console/ui/widgets/mercator_map_panel.dart';
import 'package:hf_console/ui/widgets/mic_profile_row.dart';
import 'package:hf_console/ui/widgets/pa_arm_row.dart';
import 'package:hf_console/ui/widgets/pa_panel.dart';
import 'package:hf_console/ui/widgets/pol_ctrl_panel.dart';
import 'package:hf_console/ui/widgets/power_panel.dart';
import 'package:hf_console/ui/widgets/sat_rotator_panel.dart';
import 'package:hf_console/ui/widgets/station_schematic.dart';
import 'package:hf_console/ui/widgets/status_pill.dart';
import 'package:hf_console/ui/widgets/tuner_panel.dart';
import 'package:hf_console/ui/widgets/uhf_radio_panel.dart';
import 'package:hf_console/ui/widgets/ultrabeam_panel.dart';
import 'package:hf_console/vhfcam/radio_status.dart';
import 'package:hf_console/vhfcam/rec_status.dart';
import 'package:hf_console/vhfcam/vhfcam_service.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../support/fake_mqtt_service.dart';
import '../support/fixtures.dart';
import '../support/station_fixtures.dart';
import '../support/test_harness.dart';

// ---------------------------------------------------------------------------
// Anchors: how a call-out finds its control.

/// Resolves to the screen rect a call-out points at. A zero-size rect is a
/// point (drawn as a dot): painted content such as the compass disc.
abstract class Anchor {
  const Anchor();
  Rect resolve(WidgetTester tester);
  String get describe;
}

Finder _scoped(Finder f, Type? inside) => inside == null ? f : find.descendant(of: find.byType(inside), matching: f);

Rect _one(WidgetTester tester, Finder f, String what) {
  final hits = f.evaluate().toList();
  if (hits.isEmpty) throw StateError('quickref: no widget for $what');
  return tester.getRect(find.byElementPredicate((e) => identical(e, hits.first)));
}

/// The visible text itself (exact, or containing [text]).
class TextAnchor extends Anchor {
  final String text;
  final bool contains;
  final Type? inside;
  final double inflate;
  const TextAnchor(this.text, {this.contains = false, this.inside, this.inflate = 0});
  Finder get finder => _scoped(contains ? find.textContaining(text) : find.text(text), inside);
  @override
  Rect resolve(WidgetTester t) => _one(t, finder, describe).inflate(inflate);
  @override
  String get describe => '${contains ? 'text containing' : 'text'} "$text"${inside == null ? '' : ' in $inside'}';
}

/// The tappable control (nearest button / ink / gesture ancestor) around a text.
class ButtonAnchor extends Anchor {
  final TextAnchor label;
  ButtonAnchor(String text, {bool contains = false, Type? inside})
      : label = TextAnchor(text, contains: contains, inside: inside);
  @override
  Rect resolve(WidgetTester t) {
    final f = find.ancestor(
      of: label.finder.first,
      matching: find.byWidgetPredicate((w) => w is ButtonStyleButton || w is InkResponse || w is GestureDetector),
    );
    return _one(t, f, describe);
  }

  @override
  String get describe => 'control with ${label.describe}';
}

/// Any widget finder (type, tooltip, key).
class FinderAnchor extends Anchor {
  final Finder Function() finder;
  final String what;
  const FinderAnchor(this.finder, this.what);
  @override
  Rect resolve(WidgetTester t) => _one(t, finder(), what);
  @override
  String get describe => what;
}

FinderAnchor typeAnchor(Type type, {Type? inside}) =>
    FinderAnchor(() => _scoped(find.byType(type), inside), '$type${inside == null ? '' : ' in $inside'}');
FinderAnchor tooltipAnchor(String tip) => FinderAnchor(() => find.byTooltip(tip), 'tooltip "$tip"');
FinderAnchor keyAnchor(String key) => FinderAnchor(() => find.byKey(ValueKey(key)), 'key "$key"');

/// The bounding box of several anchors, optionally grown.
class UnionAnchor extends Anchor {
  final List<Anchor> parts;
  final EdgeInsets grow;
  const UnionAnchor(this.parts, {this.grow = EdgeInsets.zero});
  @override
  Rect resolve(WidgetTester t) {
    final r = parts.map((p) => p.resolve(t)).reduce((a, b) => a.expandToInclude(b));
    return Rect.fromLTRB(r.left - grow.left, r.top - grow.top, r.right + grow.right, r.bottom + grow.bottom);
  }

  @override
  String get describe => parts.map((p) => p.describe).join(' + ');
}

/// A point at fractions ([fx], [fy]) of [base]: for painted content.
class PointAnchor extends Anchor {
  final Anchor base;
  final double fx, fy;
  const PointAnchor(this.base, this.fx, this.fy);
  @override
  Rect resolve(WidgetTester t) {
    final r = base.resolve(t);
    return Rect.fromLTWH(r.left + r.width * fx, r.top + r.height * fy, 0, 0);
  }

  @override
  String get describe => 'point in ${base.describe}';
}

// ---------------------------------------------------------------------------
// Sheets.

class Callout {
  final String title;
  final String text;
  final Anchor anchor;
  const Callout(this.title, this.text, this.anchor);
}

class Seeded {
  final BusStore store;
  final DxSpotService dx;
  final VhfcamService vhfcam;
  final FakeMqttService mqtt;
  Seeded(this.store, this.dx, this.vhfcam) : mqtt = FakeMqttService(store);
}

class Sheet {
  final String id;
  final String title;
  final String purpose;

  /// The console page tab to open ('Station', 'HF', 'UHF', 'CAM').
  final String page;
  final Seeded Function() seed;

  /// Runs after the page is open (drop the link, open a dialog).
  final Future<void> Function(WidgetTester tester, Seeded s)? after;
  final List<Callout> callouts;
  const Sheet({
    required this.id,
    required this.title,
    required this.purpose,
    required this.page,
    required this.seed,
    this.after,
    required this.callouts,
  });
}

bool _geoLoaded = false;

/// Pumps [sheet]'s page into a fresh console under [boundaryKey] (the whole
/// app, so dialogs are inside the capture). Call inside a testWidgets body
/// with the tablet view already set.
Future<Seeded> pumpSheet(WidgetTester tester, Sheet sheet, GlobalKey boundaryKey) async {
  // Once per process: the loader caches a completed future that belongs to
  // the first test's fake-async zone; awaiting it from a later test hangs.
  if (!_geoLoaded) {
    await tester.runAsync(() => WorldGeometry.instance.load());
    _geoLoaded = true;
  }
  // The settings dialog reads these (SharedPreferences on a desktop host).
  SharedPreferences.setMockInitialValues({
    'station_locator': 'JO32WE',
    'station_callsign': '',
    'horstreporter_base_url': 'https://horstreporter.kgbvax.net',
  });
  final s = sheet.seed();
  await tester.pumpWidget(RepaintBoundary(
    key: boundaryKey,
    child: TestHarness(store: s.store, mqtt: s.mqtt, dxSpot: s.dx, vhfcam: s.vhfcam, child: const ConsoleScreen()),
  ));
  await tester.pump(const Duration(milliseconds: 300));
  if (sheet.page != 'HF') {
    await tester.tap(find.widgetWithText(InkWell, sheet.page));
    await tester.pump(const Duration(milliseconds: 300));
  }
  if (sheet.after != null) await sheet.after!(tester, s);
  await tester.pump(const Duration(milliseconds: 300));
  await tester.pump(const Duration(milliseconds: 300));
  return s;
}

// ---------------------------------------------------------------------------
// Seeds.

String _locator4(double lat, double lng) {
  final x = lng + 180, y = lat + 90;
  return '${String.fromCharCode(65 + x ~/ 20)}${String.fromCharCode(65 + y ~/ 10)}${(x % 20) ~/ 2}${(y % 10).floor()}';
}

/// A plausible afternoon of FT8/CW reports for the given page's bands.
DxSpotService _spots(String page) {
  final dx = DxSpotService()
    ..configure(locator: 'JO32WE')
    // The console narrows bands after its first frame; a change purges.
    ..setBands(dxBandsForPage(page.toLowerCase()));
  final rnd = math.Random(7);
  const hubs = [
    (52.0, 10.0, 40), (48.0, 2.0, 25), (41.0, 14.0, 20), (60.0, 18.0, 12),
    (40.0, -75.0, 14), (35.0, 139.0, 6), (-23.0, -46.0, 6), (-33.0, 151.0, 4),
    (19.0, 73.0, 5), (64.0, -21.0, 4),
  ];
  const bands = ['20m', '20m', '40m', '15m', '17m'];
  var n = 0;
  for (final (lat, lng, count) in hubs) {
    for (var i = 0; i < count; i++) {
      final la = lat + (rnd.nextDouble() - .5) * 8, lo = lng + (rnd.nextDouble() - .5) * 12;
      dx.ingest(jsonEncode({
        'lat': la, 'lng': lo, 'snr': rnd.nextInt(30) - 5, 'ageSeconds': rnd.nextInt(600),
        'locator': _locator4(la, lo), 'band': bands[rnd.nextInt(bands.length)], 'sourceType': 'mqtt',
        'sender': 'DL1ABC', 'receiver': 'R${n++}',
      }));
    }
  }
  return dx;
}

Seeded _running(String page) => Seeded(BusStore()..seedScenario(StationScenario.running), _spots(page), VhfcamService());

Seeded _hf() {
  final s = _running('HF');
  final now = DateTime.now().toUtc().toIso8601String();
  s.store.applyState('muehle/hf/spots', {
    'ts': now,
    'device_online': true,
    'selected': {
      'call': 'JA1XYZ', 'name': 'Taro', 'band': '20m', 'mode': 'cw', 'lat': 35.6, 'lng': 139.7,
      'azimuth': 37.0, 'distance_km': 9050.0, 'source': 'dxlog', 'ts': now,
    },
  });
  return s;
}

const _parkAz = {'min': 0.0, 'max': 360.0, 'park': 200.0};
const _parkEl = {'min': 0.0, 'max': 90.0, 'park': 0.0};

Seeded _uhf(String page) {
  final s = _running(page);
  s.store
    ..setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 266, target: 266, limits: _parkAz)
    ..setSatRotator('muehle/uhf/el-rotator', axis: 'el', pos: 18, target: 18, limits: _parkEl)
    ..applyStatus('muehle/uhf/sat-track', 'online')
    ..applyState('muehle/uhf/sat-track', {
      'device_online': true, 'tracking': true, 'sat_name': 'AO-91', 'norad': 43017,
      'sub_lat': 50.8, 'sub_lng': -6.5, 'alt_km': 480.0, 'el': 18.0, 'az': 266.0, 'range_km': 1650.0,
    });
  return s;
}

Seeded _cam() {
  final s = _uhf('CAM');
  s.vhfcam.seedStatus(
    online: true,
    status: const RadioStatus(
      bridgeOnline: true, sessionHeld: true, radioReady: true, audioStream: true, hint: '',
      leds: {'bridge': 'on', 'session': 'on', 'radio': 'on', 'audio': 'on'},
      rec: RecStatus(enabled: true, state: 'recording', elapsedS: 754, remainingS: 1046, maxS: 1800, freeBytes: 41200000000),
    ),
  );
  return s;
}

// ---------------------------------------------------------------------------
// The sheets, in reading order.

final quickrefSheets = <Sheet>[
  Sheet(
    id: 'overview',
    title: 'Overview',
    purpose: 'Find your way: pages, settings, link and fault status.',
    page: 'Station',
    seed: () => Seeded(BusStore()..seedScenario(StationScenario.faults), _spots('Station'), VhfcamService()),
    after: (tester, s) async {
      // Both halves of a dead link: the store's data gate and the banner's flag.
      s.store.markDisconnected();
      s.mqtt.connected.value = false;
      await tester.pump();
      await tester.tap(find.byTooltip('Settings'));
    },
    callouts: [
      Callout('Page tabs', 'Switch page; underline marks current page',
          UnionAnchor([ButtonAnchor('Station', inside: ConsoleScreen), ButtonAnchor('CAM', inside: ConsoleScreen)])),
      Callout('Settings (gear)', 'Colours, locator, DX feed, camera URL', tooltipAnchor('Settings')),
      Callout('LINK DOWN banner', 'Broker lost: data stale, commands not sent', typeAnchor(LinkStatusBanner)),
      Callout('Status pill', 'Green OK · amber attention · red fault/offline', typeAnchor(StatusPill, inside: PowerPanel)),
      Callout('FAULTS', 'Active faults red, cleared ones grey', typeAnchor(FaultsBar)),
      Callout('Dark / Paper / Aether', 'Colour scheme, applied at once',
          UnionAnchor([ButtonAnchor('Dark', inside: AlertDialog), ButtonAnchor('Aether', inside: AlertDialog)])),
      Callout('DX spot callsign', "Map shows only this call's reports",
          FinderAnchor(() => find.ancestor(of: find.text('DX spot callsign'), matching: find.byType(TextField)), 'DX spot callsign field')),
      Callout('SAVE', 'Apply; restarts DX feed and camera', ButtonAnchor('SAVE', inside: AlertDialog)),
    ],
  ),
  Sheet(
    id: 'hf',
    title: 'HF page',
    purpose: 'Aim the beam, pick the antenna, drive PA, tuner and FLEX.',
    page: 'HF',
    seed: _hf,
    callouts: [
      Callout('Compass', 'Beam map; tap to turn the rotator', PointAnchor(typeAnchor(CompassPanel), 0.63, 0.59)),
      Callout('Azimuthal / Mercator', 'Change map projection', UnionAnchor([tooltipAnchor('Azimuthal'), tooltipAnchor('Mercator')])),
      Callout('Selected station', 'From the logger; tap to aim', ButtonAnchor('JA1XYZ ·', contains: true)),
      Callout('FORWARD / 180° / BI-DIR', 'Flip beam electrically, no rotation',
          UnionAnchor([ButtonAnchor('FORWARD', inside: UltrabeamPanel), ButtonAnchor('BI-DIR', inside: UltrabeamPanel)])),
      Callout('AUTO (Ultrabeam)', 'Logger steers: flip or rotate as needed', ButtonAnchor('AUTO', inside: UltrabeamPanel)),
      Callout('RETRACT', 'Pull elements in; always available', ButtonAnchor('RETRACT', inside: UltrabeamPanel)),
      Callout('Antenna ports', 'Pick antenna; locked while transmitting',
          UnionAnchor([ButtonAnchor('GND', inside: AntennaPanel), ButtonAnchor('FAN DIPOLE', inside: AntennaPanel)])),
      Callout('AUTO / MANUAL', 'AUTO follows band; MANUAL holds your pick',
          UnionAnchor([ButtonAnchor('AUTO', inside: AntennaPanel), ButtonAnchor('MANUAL', inside: AntennaPanel)])),
      Callout('NORMAL / STORM / MAINT.', 'Lock antennas for storm or work',
          UnionAnchor([ButtonAnchor('NORMAL', inside: AntModePanel), ButtonAnchor('MAINT.', inside: AntModePanel)])),
      Callout('W FWD', 'Forward power, peak marker',
          UnionAnchor([TextAnchor('W FWD', contains: true, inside: PaPanel), TextAnchor('1200', inside: PaPanel)])),
      Callout('SWR', 'Reflected match, 1.0–4.0',
          UnionAnchor([TextAnchor('SWR', contains: true, inside: PaPanel), TextAnchor('4.0', inside: PaPanel)])),
      Callout('OPERATE / STANDBY', 'Put PA in line or bypass it',
          UnionAnchor([ButtonAnchor('OPERATE', inside: PaPanel), ButtonAnchor('STANDBY', inside: PaPanel)])),
      Callout('ARM PERMIT', 'Allow PA arming; tag says why not', typeAnchor(PaArmRow)),
      Callout('BYPASS / TUNE MEM / FULL', 'Bypass ATU, recall or full tune',
          UnionAnchor([ButtonAnchor('BYPASS', inside: TunerPanel), ButtonAnchor('TUNE FULL', inside: TunerPanel)])),
      Callout('Band row', 'Change band; tap active band = SSB↔CW',
          UnionAnchor([ButtonAnchor('80m', inside: DvkPanel), ButtonAnchor('10m', inside: DvkPanel)])),
      Callout('MIC profiles', 'Tap loads profile; hold to assign', typeAnchor(MicProfileRow)),
      Callout('DVK1 / DVK2 / STOP', 'Play recorded message; STOP aborts',
          UnionAnchor([ButtonAnchor('DVK1', inside: DvkPanel), ButtonAnchor('STOP', inside: DvkPanel)])),
    ],
  ),
  Sheet(
    id: 'station',
    title: 'Station page',
    purpose: 'Power the station up and down; see what runs where.',
    page: 'Station',
    seed: () => _running('Station'),
    callouts: [
      Callout('START / STOP', 'Power the station up or down in order',
          UnionAnchor([ButtonAnchor('START', inside: PowerPanel), ButtonAnchor('STOP', inside: PowerPanel)])),
      Callout('PSU 13.8V', 'Radio power supply relay', ButtonAnchor('PSU 13.8V', inside: PowerPanel)),
      Callout('ANT PWR', 'Rotators and Ultrabeam controller power', ButtonAnchor('ANT PWR', inside: PowerPanel)),
      Callout('TRX / PA', 'Remote-on for FLEX and ACOM',
          UnionAnchor([ButtonAnchor('TRX', inside: PowerPanel), ButtonAnchor('PA', inside: PowerPanel)])),
      Callout('PC PWR', 'Shack PC mains; asks before switching off', ButtonAnchor('PC PWR', inside: PowerPanel)),
      Callout('SHACK PC', 'AWAKE / ASLEEP / NO POWER / OFFLINE',
          UnionAnchor([TextAnchor('SHACK PC', inside: PowerPanel), TextAnchor('AWAKE', inside: PowerPanel)],
              grow: const EdgeInsets.all(8))),
      Callout('Hosts (Bridges n/m)', 'Computers and how many bridges run',
          UnionAnchor([TextAnchor('HOSTS & NETWORK', inside: StationSchematic), TextAnchor('SHARI', inside: StationSchematic)],
              grow: const EdgeInsets.fromLTRB(20, 6, 75, 52))),
      Callout('RF path', 'Cyan = selected path, red = carrier', PointAnchor(typeAnchor(StationSchematic), 0.625, 0.489)),
      Callout('Schematic block (tap)', 'Details: state, problem, host, all values',
          UnionAnchor([TextAnchor('ACOM', inside: StationSchematic), TextAnchor('Temp', inside: StationSchematic)],
              grow: const EdgeInsets.fromLTRB(10, 9, 90, 9))),
    ],
  ),
  Sheet(
    id: 'uhf',
    title: 'UHF page',
    purpose: 'Point the satellite antennas, watch the IC-9700, set polarisation.',
    page: 'UHF',
    seed: () => _uhf('UHF'),
    callouts: [
      Callout('VHF map', 'Tap to aim the UHF rotator', PointAnchor(typeAnchor(MercatorMapPanel), 0.30, 0.30)),
      Callout('Heading chip', 'Azimuth now → target', TextAnchor('AZ ', contains: true, inside: MercatorMapPanel, inflate: 6)),
      Callout('PARK / STOP (map)', 'Park or stop both axes',
          UnionAnchor([ButtonAnchor('PARK', inside: MercatorMapPanel), ButtonAnchor('STOP', inside: MercatorMapPanel)])),
      Callout('Satellite', 'Tracked satellite and coverage', PointAnchor(typeAnchor(MercatorMapPanel), 0.129, 0.552)),
      Callout('RADIO AUDIO', 'IC-9700 receive audio to the stream', ButtonAnchor('RADIO AUDIO:', contains: true, inside: UhfRadioPanel)),
      Callout('MONITOR', 'IC-9700 serial telemetry on/off', ButtonAnchor('MONITOR:', contains: true, inside: UhfRadioPanel)),
      Callout('Radio readout', 'Receive-only telemetry',
          UnionAnchor([TextAnchor('MHz', contains: true, inside: UhfRadioPanel), TextAnchor('FM', inside: UhfRadioPanel)],
              grow: const EdgeInsets.all(4))),
      Callout('AZIMUTH / ELEVATION', 'Position → target, MOVING/ERR',
          UnionAnchor([TextAnchor('AZIMUTH', inside: SatRotatorPanel), TextAnchor('ELEVATION', inside: SatRotatorPanel)],
              grow: const EdgeInsets.fromLTRB(4, 4, 90, 40))),
      Callout('PARK 200° / 0°', 'Park; also sets radio and polarisation', keyAnchor('sat-park')),
      Callout('STOP (rotators)', 'Stop both axes now', ButtonAnchor('STOP', inside: SatRotatorPanel)),
      Callout('H / V / LHCP / RHCP', 'Switch both X-Quads',
          UnionAnchor([ButtonAnchor('H', inside: PolCtrlPanel), ButtonAnchor('RHCP', inside: PolCtrlPanel)])),
    ],
  ),
  Sheet(
    id: 'cam',
    title: 'CAM page',
    purpose: 'Watch the antennas, record the stream, control IC-9700 audio.',
    page: 'CAM',
    seed: _cam,
    callouts: [
      Callout('Video / RETRY', 'Antenna camera; retry when offline', typeAnchor(AspectRatio, inside: CamFeedPanel)),
      Callout('AZ / EL / FREQ', 'Live rotator and radio values',
          UnionAnchor([TextAnchor('AZ ', contains: true, inside: CamFeedPanel), TextAnchor('FREQ', contains: true, inside: CamFeedPanel)],
              grow: const EdgeInsets.all(6))),
      Callout('START / STOP RECORDING', 'Record the preview stream', keyAnchor('rec-toggle')),
      Callout('Progress line', 'Length, time left, disk free', keyAnchor('rec-line')),
      Callout('Download link', 'Fetch the recording', TextAnchor('Download:', contains: true)),
      Callout('● SERVER', 'Camera server reachable', TextAnchor('SERVER', contains: true, inside: CamRadioControls, inflate: 4)),
      Callout('BRIDGE / SESSION / RADIO / AUDIO', 'Audio chain health, left to right',
          UnionAnchor([TextAnchor('BRIDGE', inside: CamRadioControls), TextAnchor('AUDIO', inside: CamRadioControls)],
              grow: const EdgeInsets.fromLTRB(18, 4, 4, 4))),
      Callout('RADIO AUDIO ON / OFF', 'Feed IC-9700 audio into stream',
          UnionAnchor([ButtonAnchor('RADIO AUDIO ON', inside: CamRadioControls), ButtonAnchor('RADIO AUDIO OFF', inside: CamRadioControls)])),
      Callout('RADIO POWER ON', 'Switch the IC-9700 on', ButtonAnchor('RADIO POWER ON', inside: CamRadioControls)),
    ],
  ),
];
