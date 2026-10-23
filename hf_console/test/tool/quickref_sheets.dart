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
import 'package:hf_console/ui/widgets/pol_ctrl_panel.dart';
import 'package:hf_console/ui/widgets/power_panel.dart';
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

/// The IC-9700 as it is at this station: the bridge has no serial CI-V port,
/// so the monitor is off and no frequency is published.
void _realUhfRadio(BusStore store) =>
    store.applyState('muehle/uhf/radio', {'device_online': false, 'session_state': 'idle', 'monitor': false});

Seeded _running(String page) {
  final s = Seeded(BusStore()..seedScenario(StationScenario.running), _spots(page), VhfcamService());
  _realUhfRadio(s.store);
  return s;
}

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
      // RADIO stays amber here: radio_responding needs the serial monitor.
      leds: {'bridge': 'on', 'session': 'on', 'radio': 'unk', 'audio': 'on'},
      rec: RecStatus(enabled: true, state: 'recording', elapsedS: 754, remainingS: 1046, maxS: 1800, freeBytes: 41200000000),
    ),
  );
  return s;
}

// ---------------------------------------------------------------------------
// The sheets, in reading order. A call-out earns its place only by telling a
// guest operator something the label does not: an unlabeled control, a hidden
// gesture, a side effect or interlock, what an automatic mode does here.
// Facts were checked against the code (2026-10-24); keep them true.

final quickrefSheets = <Sheet>[
  Sheet(
    id: 'overview',
    title: 'Overview',
    purpose: 'Find your way: pages, settings, link and fault status.',
    page: 'Station',
    seed: () {
      final s = Seeded(BusStore()..seedScenario(StationScenario.faults), _spots('Station'), VhfcamService());
      _realUhfRadio(s.store);
      return s;
    },
    after: (tester, s) async {
      // Both halves of a dead link: the store's data gate and the banner's flag.
      s.store.markDisconnected();
      s.mqtt.connected.value = false;
      await tester.pump();
      await tester.tap(find.byTooltip('Settings'));
    },
    callouts: [
      Callout('Settings (sliders icon)', 'Colours, locator, DX spots, camera URL', tooltipAnchor('Settings')),
      Callout('LINK DOWN banner', 'This console lost the broker; bus commands dropped, not queued', typeAnchor(LinkStatusBanner)),
      Callout('Status pill', 'Any one device down or unpowered turns the pill OFFLINE', typeAnchor(StatusPill, inside: PowerPanel)),
      Callout('FAULTS', 'Grey: cleared, or antenna gear unpowered on purpose', typeAnchor(FaultsBar)),
      Callout('Dark / Paper / Aether', 'Applies instantly; not saved, Dark again after restart',
          UnionAnchor([ButtonAnchor('Dark', inside: AlertDialog), ButtonAnchor('Aether', inside: AlertDialog)])),
    ],
  ),
  Sheet(
    id: 'hf',
    title: 'HF page',
    purpose: 'Aim the beam, pick the antenna, drive PA, tuner and FLEX.',
    page: 'HF',
    seed: _hf,
    callouts: [
      Callout('Compass', 'Tap to aim; arrow = boom, cone = radiation lobe', PointAnchor(typeAnchor(CompassPanel), 0.63, 0.59)),
      Callout('Spot filter (SSB ≥ 0dB)', 'Spot SNR gate follows radio mode: SSB 0 dB, CW −15 dB',
          TextAnchor('SSB ≥', contains: true, inflate: 6)),
      Callout('Azimuthal / Mercator', 'Map projection; beam and tap-to-aim on azimuthal only',
          UnionAnchor([tooltipAnchor('Azimuthal'), tooltipAnchor('Mercator')])),
      Callout('Selected station', 'Call keyed in DXLog/Log4OM; tap to aim at it', ButtonAnchor('JA1XYZ ·', contains: true)),
      Callout('AUTO (beam steering)', 'Logger/map aims flip 180° if behind, turn only outside ±30°',
          ButtonAnchor('AUTO', inside: UltrabeamPanel)),
      Callout('Antenna ports', 'Change in RX only; 30-min idle grounds unless MANUAL tapped',
          UnionAnchor([ButtonAnchor('GND', inside: AntennaPanel), ButtonAnchor('FAN DIPOLE', inside: AntennaPanel)])),
      Callout('AUTO / MANUAL', 'Tapped MANUAL: no auto switching, no Ultrabeam/ATU band-follow',
          UnionAnchor([ButtonAnchor('AUTO', inside: AntennaPanel), ButtonAnchor('MANUAL', inside: AntennaPanel)])),
      Callout('NORMAL / STORM / MAINT.', 'STORM/MAINT.: ground, retract, park, lock until NORMAL; MAINT. ±5° nudge',
          UnionAnchor([ButtonAnchor('NORMAL', inside: AntModePanel), ButtonAnchor('MAINT.', inside: AntModePanel)])),
      Callout('BYPASS / TUNE MEM / FULL', 'PA disarmed while tuning; in AUTO, in-line follows band',
          UnionAnchor([ButtonAnchor('BYPASS', inside: TunerPanel), ButtonAnchor('TUNE FULL', inside: TunerPanel)])),
      Callout('Band row', 'Re-tap active band: SSB↔CW; stripe = spot colour on map',
          UnionAnchor([ButtonAnchor('80m', inside: DvkPanel), ButtonAnchor('10m', inside: DvkPanel)])),
      Callout('MIC profiles', 'Hold to rebind; MIC line shows only console-loaded profiles', typeAnchor(MicProfileRow)),
    ],
  ),
  Sheet(
    id: 'station',
    title: 'Station page',
    purpose: 'Power the station up and down; see what runs where.',
    page: 'Station',
    seed: () => _running('Station'),
    callouts: [
      Callout('START / STOP', 'START also wakes STANDBY; STOP leaves PC mains on',
          UnionAnchor([ButtonAnchor('START', inside: PowerPanel), ButtonAnchor('STOP', inside: PowerPanel)])),
      Callout('Phase (ON / STANDBY)', 'Unused station drops to STANDBY; a FLEX VFO change wakes it',
          TextAnchor('ON', inside: PowerPanel, inflate: 4)),
      Callout('PSU 13.8V', 'Feeds FLEX, ATR-1000, antenna switch, PLC #1, IC-9700', ButtonAnchor('PSU 13.8V', inside: PowerPanel)),
      Callout('ANT PWR', 'HF and UHF rotators, Ultrabeam controller; off in standby', ButtonAnchor('ANT PWR', inside: PowerPanel)),
      Callout('TRX / PA', 'FLEX/ACOM remote-on via PLC #1; needs PSU 13.8V on',
          UnionAnchor([ButtonAnchor('TRX', inside: PowerPanel), ButtonAnchor('PA', inside: PowerPanel)])),
      Callout('SHACK PC', 'Status only; standby asks it to sleep; wake sends WOL',
          UnionAnchor([TextAnchor('SHACK PC', inside: PowerPanel), TextAnchor('AWAKE', inside: PowerPanel)],
              grow: const EdgeInsets.all(8))),
      Callout('Bridges n/m', 'Up / expected bridges on that host; tap lists each',
          UnionAnchor([TextAnchor('HOSTS & NETWORK', inside: StationSchematic), TextAnchor('SHARI', inside: StationSchematic)],
              grow: const EdgeInsets.fromLTRB(20, 6, 75, 52))),
      Callout('Schematic block (tap)', 'Tap any block for bridge, host, all values; no controls',
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
      Callout('Map tap', 'Sets azimuth only (great-circle bearing); elevation unchanged',
          PointAnchor(typeAnchor(MercatorMapPanel), 0.30, 0.30)),
      Callout('Beam wedge', 'Rotator beam ±20°, amber while a satellite is tracked',
          PointAnchor(typeAnchor(MercatorMapPanel), 0.40, 0.42)),
      Callout('Satellite (green diamond)', 'OscarWatch sub-satellite point; dimmed while below horizon',
          PointAnchor(typeAnchor(MercatorMapPanel), 0.129, 0.552)),
      Callout('Re-centre (crosshair)', 'Recentres on station; while tracking, frames station and satellite',
          UnionAnchor([
            FinderAnchor(() => find.descendant(of: find.byType(MercatorMapPanel), matching: find.byIcon(Icons.my_location)),
                'crosshair icon in MercatorMapPanel'),
          ], grow: const EdgeInsets.all(6))),
      Callout('RADIO AUDIO', 'Feeds CAM stream; auto-off after 60 s; wfview blocks it',
          ButtonAnchor('RADIO AUDIO:', contains: true, inside: UhfRadioPanel)),
      Callout('MONITOR', 'Read-only CI-V readout; unavailable here, press shows ERR',
          ButtonAnchor('MONITOR:', contains: true, inside: UhfRadioPanel)),
      Callout('− / + / GOTO', '− / + change the typed angle 1°; only GOTO moves',
          UnionAnchor([keyAnchor('sat-az-step-down'), keyAnchor('sat-el-goto')])),
      Callout('PARK 200° / 0°', 'Also retunes IC-9700 (sat mode off), X-Quads to V', keyAnchor('sat-park')),
      Callout('H / V / LHCP / RHCP', 'Manual only (PARK sets V); survives controller reboots',
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
      Callout('Video', 'Runs about 10 s behind live', typeAnchor(AspectRatio, inside: CamFeedPanel)),
      Callout('START / STOP RECORDING', 'Also holds radio audio on; auto-stops at limit or low disk', keyAnchor('rec-toggle')),
      Callout('BRIDGE / SESSION / RADIO / AUDIO', 'RADIO always amber, not a fault; grey SESSION/AUDIO: audio off',
          UnionAnchor([TextAnchor('BRIDGE', inside: CamRadioControls), TextAnchor('AUDIO', inside: CamRadioControls)],
              grow: const EdgeInsets.fromLTRB(18, 4, 4, 4))),
      Callout('RADIO AUDIO ON / OFF', 'Lit only while audio arrives; OFF cannot cut a recording',
          UnionAnchor([ButtonAnchor('RADIO AUDIO ON', inside: CamRadioControls), ButtonAnchor('RADIO AUDIO OFF', inside: CamRadioControls)])),
      Callout('RADIO POWER ON', 'No effect here: the bridge has no serial CI-V port', ButtonAnchor('RADIO POWER ON', inside: CamRadioControls)),
    ],
  ),
];
