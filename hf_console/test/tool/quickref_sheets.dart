// quickref_sheets.dart — the hf_console quick reference (docs/quickref/):
// one sheet per console page, each a seeded tablet render plus call-outs.
// A call-out names its control by what the operator sees (label, tooltip,
// widget type, key); the render test resolves it to the control's rect, so
// re-rendering keeps every leader on its control. quickref_callouts_test.dart
// fails `flutter test` when a call-out no longer finds its control.

import 'dart:convert';
import 'dart:io';
import 'dart:math' as math;
import 'dart:typed_data';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/dxspot/world_geometry.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/screens/console_screen.dart';
import 'package:hf_console/ui/widgets/ant_mode_panel.dart';
import 'package:hf_console/ui/widgets/antenna_panel.dart';
import 'package:hf_console/ui/widgets/cam_feed_panel.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import 'package:hf_console/ui/widgets/dvk_panel.dart';
import 'package:hf_console/ui/widgets/faults_bar.dart';
import 'package:hf_console/ui/widgets/mercator_map_panel.dart';
import 'package:hf_console/ui/widgets/mic_profile_row.dart';
import 'package:hf_console/ui/widgets/pol_ctrl_panel.dart';
import 'package:hf_console/ui/widgets/power_panel.dart';
import 'package:hf_console/ui/widgets/station_schematic.dart';
import 'package:hf_console/ui/widgets/status_pill.dart';
import 'package:hf_console/ui/widgets/trx_panel.dart';
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

  /// Preferred place for the numbered marker ('left', 'right', 'above',
  /// 'below', or 'inside' the box's left end) when the default (left of the
  /// box) would sit closer to a neighbour than to its own control.
  final String? marker;
  const Callout(this.title, this.text, this.anchor, {this.marker});
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

  /// The console page tab to open ('Station', 'HF', 'UHF', 'CAM').
  final String page;
  final Seeded Function() seed;

  /// Runs after the page is open (drop the link, open a dialog).
  final Future<void> Function(WidgetTester tester, Seeded s)? after;
  final List<Callout> callouts;
  const Sheet({
    required this.id,
    required this.title,
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
  // The voice keyer buttons carry the memory names set in SmartSDR.
  s.store.applyState('muehle/hf/radio', {
    ...?s.store.slots['muehle/hf/radio']?.state,
    'dvk_memories': [
      {'id': 1, 'name': 'CQ contest', 'duration_ms': 4200},
      {'id': 2, 'name': 'QRZ?', 'duration_ms': 1300},
    ],
  });
  // In-line tuner at a healthy SWR: plain TGXL pill, no BYPASS/TUNING suffix.
  s.store.applyState('muehle/hf/tuner', {
    ...?s.store.slots['muehle/hf/tuner']?.state,
    'inline': true,
    'settling': false,
    'swr': 1.2,
  });
  final now = DateTime.now().toUtc().toIso8601String();
  s.store.apply(BusStore.workingTopic, {
    'ts': now,
    'device_online': true,
    'selected': {
      'call': 'JA1XYZ', 'name': 'Taro', 'band': '20m', 'mode': 'cw', 'lat': 35.6, 'lng': 139.7,
      'azimuth': 37.0, 'distance_km': 9050.0, 'source': 'dxlog', 'ts': now,
    },
  }, true);
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

/// A real frame of the antenna cam, grabbed from the live HLS stream by
/// docs/quickref/build.sh ($QUICKREF_CAM_FRAME). Without it the sheet shows
/// the CAMERA STREAM STOPPED state instead of a picture.
Uint8List? _camFrame() {
  final path = Platform.environment['QUICKREF_CAM_FRAME'];
  if (path == null || path.isEmpty) return null;
  final f = File(path);
  return f.existsSync() && f.lengthSync() > 0 ? f.readAsBytesSync() : null;
}

Seeded _cam() {
  final s = _uhf('CAM');
  s.vhfcam.seedStatus(
    online: true,
    still: _camFrame(),
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
// The sheets, in reading order. A call-out says what the control does or the
// gesture that the label does not show (tap, hold, double-tap), in one or two
// short, complete phrases. No background detail, no shorthand ("=", ";").
// Facts were checked against the code (2026-10-24); keep them true.

final quickrefSheets = <Sheet>[
  Sheet(
    id: 'overview',
    title: 'Overview',
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
    },
    callouts: [
      Callout('Station · HF · UHF · CAM · 75AUTO', 'Tap a name to open that page',
          UnionAnchor([ButtonAnchor('Station', inside: ConsoleScreen), ButtonAnchor('75AUTO', inside: ConsoleScreen)])),
      Callout('LINK DOWN banner', 'The console has no link to the station. Commands are lost.', typeAnchor(LinkStatusBanner),
          marker: 'inside'),
      Callout('Status pill', 'OFFLINE when a device is down or has no power', typeAnchor(StatusPill, inside: PowerPanel)),
      Callout('FAULTS', 'Lists the active faults. Grey means cleared.', TextAnchor('FAULTS', inside: FaultsBar),
          marker: 'right'),
    ],
  ),
  Sheet(
    id: 'hf',
    title: 'HF page',
    page: 'HF',
    seed: _hf,
    callouts: [
      Callout('Compass', 'Tap to aim', PointAnchor(typeAnchor(CompassPanel), 0.63, 0.59)),
      Callout('Spot filter (SSB ≥ 0dB)', 'Hides weaker spots. The limit follows the radio mode.',
          TextAnchor('SSB ≥', contains: true, inflate: 6)),
      Callout('Azimuthal / Mercator', 'Switch the map projection',
          UnionAnchor([tooltipAnchor('Azimuthal'), tooltipAnchor('Mercator')])),
      Callout('Selected station', 'The call typed into the logger. Tap to aim at it.', ButtonAnchor('JA1XYZ ·', contains: true)),
      Callout('AUTO (beam steering)', 'Reverses the beam instead of a rotator turn when the target is behind',
          ButtonAnchor('AUTO', inside: UltrabeamPanel), marker: 'right'),
      Callout('Antenna ports', 'Tap to select an antenna. AUTO follows the band, MANUAL keeps your choice.',
          UnionAnchor([ButtonAnchor('GND', inside: AntennaPanel), ButtonAnchor('FAN DIPOLE', inside: AntennaPanel)]),
          marker: 'above'),
      Callout('Band selection', 'Tap to change band. Tap the active band to switch SSB and CW.',
          UnionAnchor([ButtonAnchor('80m', inside: TrxPanel), ButtonAnchor('10m', inside: TrxPanel)])),
      Callout('Voice keyer (DVK)', 'Tap to play. Hold to auto-repeat. Double-tap to set memory and repeat time.',
          UnionAnchor([ButtonAnchor('CQ contest', inside: DvkPanel), ButtonAnchor('STOP', inside: DvkPanel)])),
      Callout('TUNE', 'Starts a tune cycle and keys the radio. Put the PA in standby first.',
          ButtonAnchor('TUNE', inside: TunerPanel)),
      Callout('RX ANT', 'Tap to switch the receive antenna between ANT 1 and RX A',
          ButtonAnchor('ANT 1', inside: TrxPanel)),
      Callout('MIC profiles', 'Hold to rebind', typeAnchor(MicProfileRow)),
    ],
  ),
  Sheet(
    id: 'station',
    title: 'Station page',
    page: 'Station',
    seed: () => _running('Station'),
    callouts: [
      Callout('START / STOP', 'Power the station up or down. START also wakes it from STANDBY.',
          UnionAnchor([ButtonAnchor('START', inside: PowerPanel), ButtonAnchor('STOP', inside: PowerPanel)])),
      Callout('Phase (ON / STANDBY)', 'An unused station goes to STANDBY. A VFO change wakes it.',
          TextAnchor('ON', inside: PowerPanel, inflate: 4)),
      Callout('NORMAL / STORM / MAINT.', 'STORM and MAINT. ground, retract and park the antenna. It stays locked until NORMAL.',
          UnionAnchor([ButtonAnchor('NORMAL', inside: AntModePanel), ButtonAnchor('MAINT.', inside: AntModePanel)])),
      Callout('Bridges n/m', 'Bridges up / expected on the host. Tap for the list.',
          UnionAnchor([TextAnchor('HOSTS & NETWORK', inside: StationSchematic), TextAnchor('SHARI', inside: StationSchematic)],
              grow: const EdgeInsets.fromLTRB(20, 6, 75, 52))),
      Callout('Schematic block', 'Tap a block to see all its values',
          UnionAnchor([TextAnchor('ACOM', inside: StationSchematic), TextAnchor('Temp', inside: StationSchematic)],
              grow: const EdgeInsets.fromLTRB(10, 9, 90, 9)),
          marker: 'above'),
    ],
  ),
  Sheet(
    id: 'uhf',
    title: 'UHF page',
    page: 'UHF',
    seed: () => _uhf('UHF'),
    callouts: [
      Callout('Map tap', 'Tap to set the azimuth. The elevation does not change.',
          PointAnchor(typeAnchor(MercatorMapPanel), 0.30, 0.30)),
      Callout('Beam wedge', 'Shows where the antennas point',
          PointAnchor(typeAnchor(MercatorMapPanel), 0.40, 0.42)),
      Callout('Satellite', 'Position of the tracked satellite',
          PointAnchor(typeAnchor(MercatorMapPanel), 0.129, 0.552)),
      Callout('RADIO AUDIO', 'Puts the radio audio on the CAM stream',
          ButtonAnchor('RADIO AUDIO:', contains: true, inside: UhfRadioPanel)),
      Callout('MONITOR', 'Frequency readout from the radio. Not available at this station.',
          ButtonAnchor('MONITOR:', contains: true, inside: UhfRadioPanel)),
      Callout('PARK 200° / 0°', 'Parks both rotators. Also retunes the radio and sets vertical polarization.', keyAnchor('sat-park')),
      Callout('H / V / LHCP / RHCP', 'Select the antenna polarization',
          UnionAnchor([ButtonAnchor('H', inside: PolCtrlPanel), ButtonAnchor('RHCP', inside: PolCtrlPanel)])),
    ],
  ),
  // Everything on the CAM page does what its label says: screenshot only.
  Sheet(
    id: 'cam',
    title: 'CAM page',
    page: 'CAM',
    seed: _cam,
    // Decode the cam frame outside fake async, or the capture shows an empty feed.
    after: (tester, s) async {
      final frame = s.vhfcam.stillFrame;
      if (frame == null) return;
      final ctx = tester.element(find.byType(CamFeedPanel));
      await tester.runAsync(() => precacheImage(MemoryImage(frame), ctx));
    },
    callouts: [],
  ),
];
