import 'dart:convert';
import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:provider/provider.dart';

import '../../store/ant_mode.dart' show antModeSlot;
import '../../store/bus_store.dart';
import '../../store/sat_track.dart';
import '../../store/station_hosts.dart';
import '../../store/station_schematic.dart';
import '../../store/wiring.dart' show powerSeqLabel, powerSeqStandbyPhases;
import '../theme.dart';

/// Drawing size of the sheet. The schematic is laid out once in these units
/// and scaled uniformly to whatever pane it gets, like a printed sheet.
const double kSchematicWidth = 760;
const double kSchematicHeight = 790;

/// Rendered size every tap target is grown to, whatever the scale.
const double _minTarget = 48;

/// Station schematic for the Station page's left pane: hosts and network,
/// the power path (mains → Shelly plugs → 13.8 V bus / antenna bus), the HF
/// RF chain into the antenna switch and the UHF chain, each block carrying
/// its live readings. Read-only: tapping a block opens its details (bridge,
/// host, liveness, every /state field); all commands stay in the panels.
///
/// Drawn in the console's own visual language — cards, status dots and
/// relay chips. Powered lines are green, the selected RF path cyan, and a
/// carrier turns it red like the TX annunciator. Antennas are passive
/// resources (no slot) and are drawn as symbols only.
class StationSchematic extends StatefulWidget {
  const StationSchematic({super.key});

  @override
  State<StationSchematic> createState() => _StationSchematicState();
}

class _StationSchematicState extends State<StationSchematic> {
  String? _open;
  Rect _anchor = Rect.zero;
  final _focus = FocusNode(debugLabel: 'station-schematic', skipTraversal: true);

  @override
  void dispose() {
    _focus.dispose();
    super.dispose();
  }

  void _toggle(String id, Rect sheetRect) {
    setState(() {
      if (_open == id) {
        _open = null;
      } else {
        _open = id;
        _anchor = sheetRect;
        // Escape must reach the shortcut even when the details were opened
        // by touch (nothing focused) rather than from a focused target.
        if (!_focus.hasFocus) _focus.requestFocus();
      }
    });
  }

  void _close() {
    if (_open != null) setState(() => _open = null);
  }

  @override
  Widget build(BuildContext context) {
    final model = StationSchematicModel(context.watch<BusStore>());
    return LayoutBuilder(
      builder: (context, c) {
        final scale = math.min(c.maxWidth / kSchematicWidth, c.maxHeight / kSchematicHeight);
        if (!scale.isFinite || scale <= 0) return const SizedBox.shrink();
        final w = kSchematicWidth * scale, h = kSchematicHeight * scale;
        final dx = (c.maxWidth - w) / 2, dy = (c.maxHeight - h) / 2;
        final open = _open;
        // Escape closes the details from wherever focus is (a hit target
        // opened them from the keyboard, or the popover's close button).
        return CallbackShortcuts(
          bindings: {const SingleActivator(LogicalKeyboardKey.escape): _close},
          child: Focus(
            focusNode: _focus,
            child: GestureDetector(
              behavior: HitTestBehavior.opaque,
              onTap: _close,
              child: Stack(
                children: [
                  Positioned(
                    left: dx,
                    top: dy,
                    width: w,
                    height: h,
                    child: FittedBox(
                      fit: BoxFit.fill,
                      // Fixed geometry: the system text scale must not grow labels
                      // out of their slots — the whole sheet scales instead.
                      child: MediaQuery.withNoTextScaling(
                        child: SizedBox(
                          width: kSchematicWidth,
                          height: kSchematicHeight,
                          child: _Sheet(model: model, scale: scale, open: open, onTap: _toggle),
                        ),
                      ),
                    ),
                  ),
                  if (open != null)
                    Positioned.fill(
                      child: CustomSingleChildLayout(
                        delegate: _PopoverLayout(
                          Rect.fromLTWH(
                            dx + _anchor.left * scale,
                            dy + _anchor.top * scale,
                            _anchor.width * scale,
                            _anchor.height * scale,
                          ),
                        ),
                        child: _Popover(
                          key: const ValueKey('schematic-popover'),
                          details: _details(model, open),
                          onClose: _close,
                        ),
                      ),
                    ),
                ],
              ),
            ),
          ),
        );
      },
    );
  }
}

// ── Sheet ─────────────────────────────────────────────────────────────────

/// Antenna-switch ports in drawing order: map key, label, wiper angle (°,
/// 0 = right, clockwise), row of the antenna it feeds. Keys follow
/// `antennaMap` in wiring.dart (a test holds the two together). Which port
/// carries the Ultrabeam is still an open question — see the port-3/4 entry
/// in docs/known-issues.md.
@visibleForTesting
const schematicPorts = [
  (port: 'port4', name: 'Ultrabeam', ang: -62.0, row: 358.0),
  (port: 'port6', name: 'Fan dipole', ang: -24.0, row: 398.0),
  (port: 'port5', name: 'Port 5', ang: 10.0, row: 426.0),
  (port: 'port1', name: 'Dummy load', ang: 42.0, row: 452.0),
  (port: 'off', name: 'Ground', ang: 78.0, row: 480.0),
];
const _sel = Offset(505, 388);
const double _selR = 20, _selTerm = 31;
Offset _terminal(double ang) => _sel + Offset(math.cos(ang * math.pi / 180), math.sin(ang * math.pi / 180)) * _selTerm;

const _hostBoxes = [
  (id: 'bwpc', x: 16.0, w: 130.0),
  (id: 'scmino', x: 162.0, w: 174.0),
  (id: 'hassio', x: 352.0, w: 118.0),
  (id: 'shari', x: 486.0, w: 118.0),
];

class _Hit {
  final String id;
  final String label;
  final Rect rect;
  const _Hit(this.id, this.label, this.rect);
}

class _Sheet extends StatelessWidget {
  final StationSchematicModel model;
  final double scale;
  final String? open;
  final void Function(String id, Rect sheetRect) onTap;

  const _Sheet({required this.model, required this.scale, required this.open, required this.onTap});

  @override
  Widget build(BuildContext context) {
    final m = model;
    final hits = <_Hit>[];
    final kids = <Widget>[];

    final qSwitch = m.quality(antSwitchSlot);

    kids.add(
      CustomPaint(size: const Size(kSchematicWidth, kSchematicHeight), painter: _SchematicPainter(schematicWires(m))),
    );

    // Zone labels.
    for (final (y, label) in [(0.0, 'HOSTS & NETWORK'), (112.0, 'POWER'), (292.0, 'HF'), (572.0, 'UHF')]) {
      kids.add(_text(34, y + 18, label, _mono(10.5, color: AppTheme.txtFaint, weight: FontWeight.w600, ls: 1.2)));
    }

    // ── Hosts ────────────────────────────────────────────────────────────
    for (final hb in _hostBoxes) {
      final r = Rect.fromLTWH(hb.x, 30, hb.w, 56);
      kids.add(_hostCard(m, hb.id, r));
      hits.add(_Hit('host:${hb.id}', stationHosts[hb.id]!.name, r));
    }

    // ── Power ────────────────────────────────────────────────────────────
    kids.add(_text(14, 208, 'House mains', _mono(10.5, color: AppTheme.txtFaint)));
    kids.addAll(_plug(m, 'muehle/power/shack-pc-mains', const Offset(118, 146), 'PC mains', hits));
    kids.addAll(_plug(m, psuSlot, const Offset(250, 206), 'PSU mains', hits, labelLeft: true));
    kids.addAll(_plug(m, antennaPlugSlot, const Offset(630, 206), 'Antenna mains', hits));
    final qPsu = m.quality(psuSlot);
    final psu = m.psuPower;
    const psuRect = Rect.fromLTWH(206, 224, 88, 34);
    kids.add(
      _card(
        psuRect,
        q: qPsu,
        sev: Severity.none,
        off: psu != true,
        rows: (w) => [
          _title(w, 15, 'PSU'),
          _pair(w, 28, 'Out', switch (psu) {
            true => '13.8 V',
            false => '0 V',
            null => '—',
          }, q: qPsu),
        ],
      ),
    );
    hits.add(const _Hit(psuCardId, '13.8 V TRX PSU', psuRect));

    final seqPhase = m.value<String>(powerSeqSlot, 'phase');
    final seqWord = seqPhase == null ? '—' : powerSeqLabel(seqPhase, m.value<String>(powerSeqSlot, 'fault') ?? '');
    kids.add(
      _pill(
        const Offset(420, 206),
        124,
        40,
        'Sequencer',
        seqWord,
        q: m.quality(powerSeqSlot),
        sev: m.severity(powerSeqSlot),
        attention: powerSeqStandbyPhases.contains(seqPhase),
        off: !const {'running', 'starting'}.contains(seqPhase),
      ),
    );
    hits.add(_Hit(powerSeqSlot, 'Sequencer', Rect.fromCenter(center: const Offset(420, 206), width: 124, height: 40)));

    // ── HF ───────────────────────────────────────────────────────────────
    kids.add(_flex(m, hits));
    kids.add(_acom(m, hits));
    kids.add(_atr(m, hits));
    kids.add(_text(499, 328, 'Switch', _mono(11, color: AppTheme.txtMute), align: TextAlign.right));
    kids.add(_dot(const Offset(516, 324), _dotColor(qSwitch, m.severity(antSwitchSlot))));
    hits.add(_Hit(antSwitchSlot, 'Antenna switch', Rect.fromCircle(center: _sel, radius: _selTerm + 6)));
    kids.addAll(_antennaLabels(m));
    kids.addAll(_hfRotatorAndController(m, hits));
    kids.add(_plc1(m, hits));

    final mode = m.value<String>('muehle/hf/antenna-select', 'mode');
    final idle = m.value<String>('muehle/hf/antenna-select', 'activity') == 'inactive';
    kids.add(
      _pill(
        const Offset(420, 512),
        128,
        40,
        'Antenna select',
        mode == null
            ? '—'
            : mode == 'manual'
            ? 'MANUAL HOLD'
            : idle
            ? 'AUTO, IDLE'
            : mode.toUpperCase(),
        q: m.quality('muehle/hf/antenna-select'),
        sev: m.severity('muehle/hf/antenna-select'),
      ),
    );
    hits.add(
      _Hit(
        'muehle/hf/antenna-select',
        'Antenna select',
        Rect.fromCenter(center: const Offset(420, 512), width: 128, height: 40),
      ),
    );
    // Beam steer and the antenna mode share the row right of antenna
    // select: both are logic slots acting on the rotator + Ultrabeam pair
    // above them.
    final steer = m.value<bool>('muehle/hf/beam-steer', 'enabled');
    kids.add(
      _pill(
        const Offset(560, 530),
        96,
        36,
        'Beam steer',
        steer == null
            ? '—'
            : steer
            ? 'ON'
            : 'OFF',
        q: m.quality('muehle/hf/beam-steer'),
        sev: m.severity('muehle/hf/beam-steer'),
        off: steer != true,
      ),
    );
    hits.add(
      _Hit(
        'muehle/hf/beam-steer',
        'Beam steer',
        Rect.fromCenter(center: const Offset(560, 530), width: 96, height: 36),
      ),
    );
    kids.add(
      _pill(
        const Offset(680, 530),
        128,
        36,
        'Ant mode',
        _antModeWord(m),
        q: m.quality(antModeSlot),
        sev: m.severity(antModeSlot),
      ),
    );
    hits.add(
      _Hit(antModeSlot, 'Antenna mode', Rect.fromCenter(center: const Offset(680, 530), width: 128, height: 36)),
    );

    // ── UHF ──────────────────────────────────────────────────────────────
    kids.add(_ic9700(m, hits));
    final sat = SatTrack.fromStore(m.store);
    final el = sat?.el;
    kids.add(
      _pill(
        const Offset(300, 612),
        168,
        40,
        'Sat track',
        sat == null ? 'NO PASS' : sat.name,
        valueRight: sat == null ? null : 'EL ${el == null ? '—' : '${el.round()}°'}',
        q: m.quality('muehle/uhf/sat-track'),
        sev: m.severity('muehle/uhf/sat-track'),
        off: sat == null,
      ),
    );
    hits.add(
      _Hit(
        'muehle/uhf/sat-track',
        'Sat track',
        Rect.fromCenter(center: const Offset(300, 612), width: 168, height: 40),
      ),
    );
    kids.addAll(_uhfAntenna(m, hits));

    // ── Legend ───────────────────────────────────────────────────────────
    for (final (x, label) in [
      (18.0, 'Powered'),
      (130.0, 'Off'),
      (214.0, 'RF path'),
      (324.0, 'Carrier'),
      (430.0, 'Control'),
      (538.0, 'Data link'),
    ]) {
      kids.add(_text(x + 34, 760, label, _mono(11, color: AppTheme.txtMute)));
    }

    // Tap targets last, so they sit above every visual, each grown to the
    // minimum rendered size.
    final minSheet = _minTarget / scale;
    final seen = <String, int>{};
    for (final hit in hits) {
      final n = seen[hit.id] = (seen[hit.id] ?? 0) + 1;
      final r = hit.rect;
      final grown = Rect.fromCenter(
        center: r.center,
        width: math.max(r.width, minSheet),
        height: math.max(r.height, minSheet),
      );
      kids.add(
        Positioned.fromRect(
          rect: grown,
          child: _HitTarget(
            key: ValueKey(n == 1 ? 'schematic-hit-${hit.id}' : 'schematic-hit-${hit.id}#$n'),
            label: '${hit.label} details',
            selected: open == hit.id,
            onTap: () => onTap(hit.id, r),
          ),
        ),
      );
    }

    return Stack(clipBehavior: Clip.none, children: kids);
  }
}

const antSwitchSlot = 'muehle/hf/ant-switch';

/// The antenna-mode pill's word: the mode (a locked snapshot holds even with
/// antmode down), or the pending storm suggestion; '—' before any state.
String _antModeWord(StationSchematicModel m) {
  if (m.store.slots[antModeSlot]?.state == null) return '—';
  final a = m.store.antMode;
  if (a.locked) return a.label;
  return a.stormSuggested ? 'SUGGEST STORM' : 'NORMAL';
}

/// Tap id of the PSU block (the plug chip above it carries [psuSlot]).
const psuCardId = 'psu:13v8';
const antCtrlSlot = 'muehle/hf/ant-ctrl';

/// What the painter draws from: power and RF liveness, the carrier, the
/// switch position and state. A record, so repaint is a value compare.
typedef SchematicWires = ({
  AppColorScheme scheme,
  bool link,
  bool psu,
  bool ant,
  bool pc,
  bool rf,
  bool carrier,
  String? port,
  bool uhfRf,
  String? direction,
  bool switchAlarm,
  bool switchDead,
});

/// Wires live only on a plug confirmed on (an unknown plug draws grey) and
/// only while our link is up; the wiper only while the switch is trusted.
@visibleForTesting
SchematicWires schematicWires(StationSchematicModel m) {
  final link = m.linkUp;
  final qSwitch = m.quality(antSwitchSlot);
  final qUb = m.quality(antCtrlSlot);
  final rf = link && m.psuPower == true;
  return (
    scheme: AppTheme.selected,
    link: link,
    psu: rf,
    ant: link && m.antennaPower == true,
    pc: link && m.pcMainsPower == true,
    rf: rf,
    carrier: link && m.carrier,
    port: _bad(qSwitch) || qSwitch == SlotQuality.unpowered ? null : m.value<String>(antSwitchSlot, 'selected'),
    uhfRf: rf && m.quality(uhfRadioSlot) == SlotQuality.ok,
    direction: qUb == SlotQuality.ok || qUb == SlotQuality.stale ? m.value<String>(antCtrlSlot, 'direction') : null,
    switchAlarm: m.severity(antSwitchSlot) == Severity.alarm,
    switchDead: _bad(qSwitch) || qSwitch == SlotQuality.unpowered,
  );
}

bool _bad(SlotQuality q) =>
    q == SlotQuality.down || q == SlotQuality.lost || q == SlotQuality.stale || q == SlotQuality.noData;

// ── Blocks ────────────────────────────────────────────────────────────────

Widget _hostCard(StationSchematicModel m, String id, Rect r) {
  // No card for this console: a healthy link is not news, and a dead one is
  // the red LINK DOWN banner across the whole screen.
  final link = m.linkUp;
  final h = stationHosts[id]!;
  final ip = h.address.replaceFirst('192.168.1', '');
  final q = link ? SlotQuality.ok : SlotQuality.stale;
  if (id == 'bwpc') {
    final pcQ = m.quality(shackPcSlot);
    final unpowered = m.pcMainsPower == false;
    final status = m.store.slots[shackPcSlot]?.status;
    final word = !link
        ? 'UNKNOWN'
        : unpowered
        ? 'NO POWER'
        : status == 'online'
        ? 'AWAKE'
        : status == 'offline'
        ? 'ASLEEP'
        : 'OFFLINE';
    final awake = word == 'AWAKE';
    return _card(
      r,
      q: q,
      sev: m.problemText(shackPcSlot) != null && pcQ == SlotQuality.ok ? Severity.alarm : Severity.none,
      off: !awake,
      rows: (w) => [
        _title(w, 17, h.name),
        // The PC's state is the point of this card: its own row, never a
        // title tag that may be dropped for width.
        _pair(w, 34, ip, word, color: awake ? AppTheme.green : AppTheme.txtMute),
      ],
    );
  }
  if (id == 'hassio') {
    return _card(r, q: q, sev: Severity.none, rows: (w) => [_title(w, 17, h.name), _pair(w, 34, ip, '')]);
  }
  final hh = m.hostHealth(id);
  return _card(
    r,
    q: q,
    sev: link && hh.up < hh.total ? Severity.warn : Severity.none,
    rows: (w) => [
      _title(w, 17, h.name),
      _pair(w, 34, ip, ''),
      _pair(w, 50, 'Bridges', '${hh.up}/${hh.total}', q: q, color: link && hh.up < hh.total ? AppTheme.amber : null),
    ],
  );
}

/// Shelly plug: relay chip on the feed, name and reading beside it
/// ([labelLeft] puts them left of the chip). The reading is the live draw
/// once shelly-power-bridge publishes `apower` in /state (an open item in
/// docs/known-issues.md); until then it is the relay position.
List<Widget> _plug(
  StationSchematicModel m,
  String slot,
  Offset at,
  String label,
  List<_Hit> hits, {
  bool labelLeft = false,
}) {
  final q = m.quality(slot);
  final watts = m.value<num>(slot, 'apower');
  final on = m.value<String>(slot, 'power') == 'on';
  hits.add(
    _Hit(
      slot,
      label,
      labelLeft ? Rect.fromLTWH(at.dx - 106, at.dy - 14, 120, 30) : Rect.fromLTWH(at.dx - 14, at.dy - 14, 120, 30),
    ),
  );
  final x = labelLeft ? at.dx - 16 : at.dx + 16;
  final align = labelLeft ? TextAlign.right : TextAlign.left;
  return [
    _chip(at, on: on, q: q, sev: m.severity(slot)),
    _text(
      x,
      at.dy - 3,
      label,
      _mono(11.5, color: AppTheme.txtMute),
      align: align,
      width: labelLeft ? 110 : null,
    ),
    _text(
      x,
      at.dy + 12,
      q == SlotQuality.down || q == SlotQuality.lost
          ? '—'
          : watts != null
          ? '${watts.round()} W'
          : switch (m.plugPower(slot)) {
              true => 'ON',
              false => 'OFF',
              null => '—',
            },
      _mono(12.5, color: _bad(q) ? AppTheme.txtFaint : AppTheme.txt, weight: FontWeight.w600),
      align: align,
      width: labelLeft ? 110 : null,
    ),
  ];
}

Widget _flex(StationSchematicModel m, List<_Hit> hits) {
  const a = 'muehle/hf/radio';
  const r = Rect.fromLTWH(40, 336, 120, 100);
  hits.add(const _Hit(a, 'FLEX-8400', r));
  final q = m.quality(a);
  final rf = m.store.rfState;
  final drive = m.value<num>(a, 'drive');
  return _card(
    r,
    q: q,
    sev: m.severity(a),
    rows: (w) => [
      _title(w, 17, 'FLEX-8400'),
      _big(w, 43, _freq(m.value<num>(a, 'freq_hz')), q: q),
      _pair(w, 63, _band(m.value<String>(a, 'band')), (m.value<String>(a, 'mode') ?? '—').toUpperCase(), q: q),
      _badge(
        w,
        88,
        switch (rf) {
          RfState.tx => 'TX',
          RfState.tune => 'TUNE',
          RfState.rx => 'RX',
        },
        lit: rf != RfState.rx && m.linkUp,
        right: drive == null ? null : '${drive.round()} %',
        q: q,
      ),
    ],
  );
}

Widget _acom(StationSchematicModel m, List<_Hit> hits) {
  const a = 'muehle/hf/pa';
  const r = Rect.fromLTWH(186, 336, 130, 100);
  hits.add(const _Hit(a, 'ACOM 1200S', r));
  final q = m.quality(a);
  final mode = m.value<String>(a, 'mode');
  final keyed = m.value<String>(a, 'keyed') == 'tx';
  final fwd = m.value<num>(a, 'fwd_power_w');
  final swr = m.value<num>(a, 'swr')?.toDouble();
  final temp = m.value<num>(a, 'temp_c');
  final problem = m.problemText(a);
  final relayOff = m.value<String>(hfSwitchSlot, 'pa') == 'off';
  return _card(
    r,
    q: q,
    sev: m.severity(a),
    off: relayOff,
    rows: (w) => [
      _title(
        w,
        17,
        'ACOM',
        right: relayOff
            ? 'RELAY OFF'
            : mode == 'operate'
            ? 'OPER'
            : mode == 'standby'
            ? 'STBY'
            : null,
        rightColor: relayOff ? AppTheme.amber : null,
      ),
      if (problem != null && q == SlotQuality.ok)
        _note(w, 41, '${m.hasFault(a) ? 'FAULT' : 'ERR'} $problem', AppTheme.red)
      else
        _big(w, 41, fwd == null ? '—' : '${fwd.round()}', unit: 'W FWD', q: q),
      _pair(
        w,
        63,
        'SWR',
        (keyed || problem != null) && swr != null ? swr.toStringAsFixed(2) : '—',
        q: q,
        color: keyed || problem != null ? _swrColor(swr) : null,
      ),
      _pair(w, 85, 'Temp', temp == null ? '—' : '${temp.round()}°', q: q),
    ],
  );
}

Widget _atr(StationSchematicModel m, List<_Hit> hits) {
  const a = 'muehle/hf/tuner';
  const r = Rect.fromLTWH(342, 336, 116, 100);
  hits.add(const _Hit(a, 'ATR-1000', r));
  final q = m.quality(a);
  final inline = m.value<bool>(a, 'inline');
  final settling = m.value<bool>(a, 'settling') == true;
  final swr = m.value<num>(a, 'swr')?.toDouble();
  final fwd = m.value<num>(a, 'fwd');
  return _card(
    r,
    q: q,
    sev: m.severity(a),
    attention: settling,
    rows: (w) => [
      _title(w, 17, 'ATR-1000'),
      _big(
        w,
        41,
        settling
            ? 'TUNING'
            : inline == null
            ? '—'
            : inline
            ? 'INLINE'
            : 'BYPASS',
        q: q,
      ),
      _pair(w, 63, 'SWR', swr == null ? '—' : swr.toStringAsFixed(1), q: q, color: _swrColor(swr)),
      _pair(w, 85, 'Fwd', fwd == null ? '—' : '${fwd.round()} W', q: q),
    ],
  );
}

List<Widget> _antennaLabels(StationSchematicModel m) {
  final qU = m.quality('muehle/hf/ant-ctrl');
  final band = _band(m.value<String>('muehle/hf/ant-ctrl', 'band'));
  final dir =
      const {'forward': 'Forward', 'reverse': 'Reverse', 'bidirectional': 'Bi-dir'}[m.value<String>(
        'muehle/hf/ant-ctrl',
        'direction',
      )] ??
      '—';
  final sub = switch (qU) {
    SlotQuality.unpowered => 'unpowered',
    SlotQuality.down || SlotQuality.lost || SlotQuality.noData => 'no data',
    _ => '$band $dir',
  };
  final name = _mono(12, color: AppTheme.txt, weight: FontWeight.w600);
  return [
    _text(658, 355, 'Ultrabeam', name),
    _text(658, 371, sub, _mono(11.5, color: qU == SlotQuality.ok ? AppTheme.txtMute : AppTheme.txtFaint), width: 90),
    _text(658, 402, 'Fan dipole', name),
    _text(658, 430, 'Port 5', _mono(12, color: AppTheme.txtFaint)),
    _text(658, 456, 'Dummy load', name),
    _text(658, 484, 'Ground', name),
  ];
}

List<Widget> _hfRotatorAndController(StationSchematicModel m, List<_Hit> hits) {
  const rot = 'muehle/hf/rotator', rcu = 'muehle/hf/ant-ctrl';
  const rRot = Rect.fromLTWH(578, 296, 80, 34), rRcu = Rect.fromLTWH(664, 296, 80, 34);
  hits
    ..add(const _Hit(rot, 'G-450 rotator', rRot))
    ..add(const _Hit(rcu, 'Ultrabeam RCU-06', rRcu));
  final qRot = m.quality(rot), qRcu = m.quality(rcu);
  final az = m.value<num>(rot, 'az');
  return [
    _card(
      rRot,
      q: qRot,
      sev: m.severity(rot),
      attention: m.value<bool>(rot, 'moving') == true,
      rows: (w) => [
        _title(w, 14, 'G-450'),
        _pair(w, 28, 'Az', _small(qRot, az == null ? '—' : '${az.round()}°'), q: qRot),
      ],
    ),
    _card(
      rRcu,
      q: qRcu,
      sev: m.severity(rcu),
      attention: m.value<bool>(rcu, 'moving') == true,
      rows: (w) => [
        _title(w, 14, 'RCU-06'),
        _pair(w, 28, 'Band', _small(qRcu, _band(m.value<String>(rcu, 'band'))), q: qRcu),
      ],
    ),
  ];
}

Widget _plc1(StationSchematicModel m, List<_Hit> hits) {
  const a = 'muehle/hf/switch', arm = 'muehle/hf/pa-arm';
  const r = Rect.fromLTWH(40, 482, 268, 62);
  hits.add(const _Hit(a, 'PLC #1 relays', r));
  final q = m.quality(a), qArm = m.quality(arm);
  final trxWord = m.value<String>(a, 'trx'), paWord = m.value<String>(a, 'pa');
  final trx = trxWord == 'on', pa = paWord == 'on';
  String relay(String name, String? word) =>
      '$name ${word == null || q == SlotQuality.unpowered ? '?' : word.toUpperCase()}';
  hits.add(_Hit(arm, 'PA arm', Rect.fromCenter(center: r.topLeft + const Offset(244, 42), width: 56, height: 40)));
  final armed = m.value<bool>(arm, 'armed');
  final sev = m.severity(a) == Severity.alarm || m.severity(arm) == Severity.alarm ? Severity.alarm : Severity.none;
  final label = _mono(11, color: AppTheme.txtMute);
  return _card(
    r,
    q: q,
    sev: sev,
    rows: (w) => [
      _title(w, 17, 'PLC #1'),
      _chip(const Offset(60, 34), on: trx, q: q),
      _chip(const Offset(182, 34), on: pa, q: q),
      _chip(const Offset(244, 34), on: armed == true, q: qArm),
      _text(60, 55, relay('TRX', trxWord), label, align: TextAlign.center, width: 80),
      _text(182, 55, relay('PA', paWord), label, align: TextAlign.center, width: 80),
      _text(
        244,
        55,
        armed == null || qArm == SlotQuality.unpowered
            ? 'ARM ?'
            : armed
            ? 'ARMED'
            : 'SAFE',
        label,
        align: TextAlign.center,
        width: 60,
      ),
    ],
  );
}

Widget _ic9700(StationSchematicModel m, List<_Hit> hits) {
  const a = 'muehle/uhf/radio';
  const r = Rect.fromLTWH(40, 598, 124, 84);
  hits.add(const _Hit(a, 'IC-9700', r));
  final q = m.quality(a);
  final session = m.value<String>(a, 'session_state');
  final deaf = m.uhfRadioDeaf;
  final word = deaf
      ? 'STANDBY'
      : switch (session) {
          'live' => 'LIVE',
          'connecting' => 'CONN',
          'error' => 'ERROR',
          'idle' => 'IDLE',
          _ => '—',
        };
  return _card(
    r,
    q: q,
    sev: m.severity(a),
    attention: deaf,
    rows: (w) => [
      _title(w, 17, 'IC-9700'),
      _big(w, 43, _freq(m.value<num>(a, 'freq_hz')), q: q),
      _pair(w, 62, (m.value<String>(a, 'mode') ?? '—').toUpperCase(), '', q: q),
      _pair(
        w,
        77,
        'Session',
        word,
        q: q,
        color: session == 'error'
            ? AppTheme.red
            : deaf || session == 'connecting'
            ? AppTheme.amber
            : null,
      ),
    ],
  );
}

List<Widget> _uhfAntenna(StationSchematicModel m, List<_Hit> hits) {
  const pol = 'muehle/uhf/pol-ctrl', az = 'muehle/uhf/az-rotator', el = 'muehle/uhf/el-rotator';
  const rPol = Rect.fromLTWH(420, 596, 110, 34);
  const rAz = Rect.fromLTWH(578, 664, 80, 34), rEl = Rect.fromLTWH(664, 664, 80, 34);
  hits
    ..add(const _Hit(pol, 'PLC #2 polarization', rPol))
    ..add(const _Hit(az, 'SPID azimuth rotator', rAz))
    ..add(const _Hit(el, 'GS-500 elevation rotator', rEl));
  final qPol = m.quality(pol), qAz = m.quality(az), qEl = m.quality(el);
  final p = _polShort[m.value<String>(pol, 'pol')] ?? '—';
  final azv = m.value<num>(az, 'az'), elv = m.value<num>(el, 'el');
  return [
    _card(
      rPol,
      q: qPol,
      sev: m.severity(pol),
      rows: (w) => [_title(w, 14, 'PLC #2'), _pair(w, 27, 'Pol', _small(qPol, p), q: qPol)],
    ),
    _text(658, 636, 'X-Quad', _mono(12, color: AppTheme.txt, weight: FontWeight.w600)),
    _text(658, 652, _small(qPol, p), _mono(11.5, color: _bad(qPol) ? AppTheme.txtFaint : AppTheme.txtMute)),
    _card(
      rAz,
      q: qAz,
      sev: m.severity(az),
      attention: m.value<bool>(az, 'moving') == true,
      rows: (w) => [
        _title(w, 14, 'SPID'),
        _pair(w, 28, 'Az', _small(qAz, azv == null ? '—' : '${azv.round()}°'), q: qAz),
      ],
    ),
    _card(
      rEl,
      q: qEl,
      sev: m.severity(el),
      attention: m.value<bool>(el, 'moving') == true,
      rows: (w) => [
        _title(w, 14, 'GS-500'),
        _pair(w, 28, 'El', _small(qEl, elv == null ? '—' : '${elv.round()}°'), q: qEl),
      ],
    ),
  ];
}

const _polShort = {'h': 'H', 'v': 'V', 'cl': 'LHCP', 'cr': 'RHCP'};

/// Value for a small block: `off` while unpowered, the dash (rendered as
/// OFFLINE / NO LINK) once data is gone.
String _small(SlotQuality q, String v) => switch (q) {
  SlotQuality.unpowered => 'off',
  SlotQuality.down || SlotQuality.lost => '—',
  _ => v,
};

Color? _swrColor(double? swr) {
  if (swr == null) return null;
  if (swr >= swrAlarm) return AppTheme.red;
  if (swr >= swrWarn) return AppTheme.amber;
  return null;
}

String _freq(num? hz) {
  if (hz == null || hz <= 0) return '—';
  final s = hz.round().toString().padLeft(7, '0');
  final n = s.length;
  return '${s.substring(0, n - 6)}.${s.substring(n - 6, n - 3)}.${s.substring(n - 3)}';
}

String _band(String? b) {
  if (b == null || b.isEmpty) return '—';
  final match = RegExp(r'^(\d+)(cm|m)$').firstMatch(b);
  return match == null ? '—' : '${match[1]} ${match[2]}';
}

// ── Primitives ────────────────────────────────────────────────────────────

TextStyle _mono(double px, {Color? color, FontWeight weight = FontWeight.w400, double ls = 0}) =>
    // AppTheme.mono renders one point above its argument.
    AppTheme.mono(px - 1, color: color, weight: weight, letterSpacing: ls).copyWith(height: 1.0);

/// Text placed by its baseline, the way the sheet was laid out.
Widget _text(double x, double baseline, String s, TextStyle st, {TextAlign align = TextAlign.left, double? width}) {
  final top = baseline - st.fontSize! * 0.79;
  final text = Text(s, style: st, maxLines: 1, softWrap: false, overflow: TextOverflow.ellipsis, textAlign: align);
  return switch (align) {
    TextAlign.right => Positioned(left: x - (width ?? 200), top: top, width: width ?? 200, child: text),
    TextAlign.center => Positioned(left: x - (width ?? 200) / 2, top: top, width: width ?? 200, child: text),
    _ => Positioned(left: x, top: top, width: width, child: text),
  };
}

double _measure(String s, TextStyle st) {
  final tp = TextPainter(
    text: TextSpan(text: s, style: st),
    textDirection: TextDirection.ltr,
    maxLines: 1,
  )..layout();
  return tp.width;
}

/// A block: card fill, status dot top right, rows laid out by baseline.
/// [attention] marks a deliberate or transient state (standby, moving,
/// tuning) amber without calling it a fault; [off] dims the dot.
Widget _card(
  Rect r, {
  required SlotQuality q,
  required Severity sev,
  bool off = false,
  bool attention = false,
  required List<Widget> Function(double w) rows,
}) {
  final tone = switch (sev) {
    Severity.alarm => AppTheme.red,
    Severity.warn => AppTheme.amber,
    Severity.none => null,
  };
  final dot = _dotColor(q, sev, off: off, attention: attention);
  return Positioned.fromRect(
    rect: r,
    child: Container(
      decoration: BoxDecoration(
        color: tone == null ? AppTheme.card : Color.alphaBlend(AppTheme.blend(tone, 0.10), AppTheme.card),
        border: Border.all(color: tone ?? AppTheme.cardLine, width: tone == null ? 1 : 1.6),
      ),
      child: Stack(clipBehavior: Clip.none, children: [_dot(Offset(r.width - 10, 10), dot), ...rows(r.width)]),
    ),
  );
}

Color _dotColor(SlotQuality q, Severity sev, {bool off = false, bool attention = false}) {
  if (sev == Severity.alarm) return AppTheme.red;
  if (sev == Severity.warn) return AppTheme.amber;
  if (_bad(q)) return AppTheme.txtFaint;
  if (q == SlotQuality.unpowered || attention) return AppTheme.amber;
  if (off) return AppTheme.txtFaint;
  return AppTheme.green;
}

Widget _dot(Offset c, Color color) => Positioned(
  left: c.dx - 6,
  top: c.dy - 6,
  width: 12,
  height: 12,
  child: DecoratedBox(
    decoration: BoxDecoration(shape: BoxShape.circle, color: AppTheme.blend(color, 0.22)),
    child: Center(
      child: Container(
        width: 7,
        height: 7,
        decoration: BoxDecoration(shape: BoxShape.circle, color: color),
      ),
    ),
  ),
);

Widget _title(double w, double y, String t, {String? right, Color? rightColor}) {
  final st = _mono(10.5, color: AppTheme.txtMute, weight: FontWeight.w700, ls: 0.6);
  final up = t.toUpperCase();
  final r = right?.toUpperCase();
  final rst = _mono(10.5, color: rightColor ?? AppTheme.txtMute, weight: FontWeight.w700);
  // The right-hand tag only shows when it fits beside the title and dot.
  final fits = r != null && _measure(up, st) + _measure(r, rst) + 10 <= w - 18 - 14;
  return Stack(
    clipBehavior: Clip.none,
    children: [
      _text(9, y, up, st),
      if (fits) _text(w - 23, y, r, rst, align: TextAlign.right),
    ],
  );
}

/// Value row reading OFFLINE / UNREACHABLE once the data is gone.
Widget _big(double w, double y, String t, {String? unit, required SlotQuality q}) {
  if (q == SlotQuality.down || q == SlotQuality.lost) {
    return _text(
      9,
      y,
      q == SlotQuality.down ? 'OFFLINE' : 'UNREACHABLE',
      _mono(12.5, color: AppTheme.red, weight: FontWeight.w700),
    );
  }
  final vc = _bad(q) || q == SlotQuality.unpowered ? AppTheme.txtFaint : AppTheme.txt;
  final st = _mono(16, color: vc, weight: FontWeight.w600);
  // Long readings (a 23 cm frequency) shrink to the card instead of
  // running over its edge.
  return Stack(
    clipBehavior: Clip.none,
    children: [
      Positioned(
        left: 9,
        top: y - st.fontSize! * 0.79,
        width: w - 18 - (unit != null ? 44 : 0),
        height: st.fontSize!,
        child: FittedBox(
          fit: BoxFit.scaleDown,
          alignment: Alignment.centerLeft,
          child: Text(q == SlotQuality.unpowered ? 'OFF' : t, style: st, maxLines: 1, softWrap: false),
        ),
      ),
      if (unit != null) _text(w - 9, y, unit, _mono(11, color: AppTheme.txtMute), align: TextAlign.right),
    ],
  );
}

Widget _pair(double w, double y, String l, String r, {SlotQuality q = SlotQuality.ok, Color? color}) {
  final gone = (q == SlotQuality.down || q == SlotQuality.lost) && r == '—';
  final vc = gone ? AppTheme.red : color ?? (_bad(q) ? AppTheme.txtFaint : AppTheme.txt);
  return Stack(
    clipBehavior: Clip.none,
    children: [
      if (!gone) _text(9, y, l, _mono(11.5, color: AppTheme.txtMute)),
      if (r.isNotEmpty)
        _text(
          w - 9,
          y,
          gone ? (q == SlotQuality.down ? 'OFFLINE' : 'NO LINK') : r,
          _mono(gone ? 10.5 : 11.5, color: vc, weight: gone ? FontWeight.w700 : FontWeight.w600),
          align: TextAlign.right,
        ),
    ],
  );
}

Widget _note(double w, double y, String t, Color color) => _text(
  9,
  y,
  t,
  _mono(11, color: color, weight: FontWeight.w700),
  width: w - 18,
);

Widget _badge(double w, double y, String t, {required bool lit, String? right, required SlotQuality q}) {
  final dead = _bad(q);
  final on = lit && !dead;
  return Stack(
    clipBehavior: Clip.none,
    children: [
      Positioned(
        left: 9,
        top: y - 12,
        width: 38,
        height: 16,
        child: Container(
          alignment: Alignment.center,
          decoration: BoxDecoration(
            color: on ? AppTheme.red : AppTheme.pane,
            border: Border.all(color: on ? AppTheme.red : AppTheme.cardLine),
            borderRadius: BorderRadius.circular(3),
          ),
          child: Text(
            t,
            style: _mono(10.5, color: on ? AppTheme.activeButtonText : AppTheme.txtFaint, weight: FontWeight.w700),
          ),
        ),
      ),
      if (right != null)
        _text(
          w - 9,
          y,
          right,
          _mono(11.5, color: dead ? AppTheme.txtFaint : AppTheme.txt, weight: FontWeight.w600),
          align: TextAlign.right,
        ),
    ],
  );
}

/// The relay chip from the power panel, shrunk: track and knob, green when
/// the contact is closed.
Widget _chip(Offset c, {required bool on, required SlotQuality q, Severity sev = Severity.none}) {
  // An unpowered controller's relays are open, whatever its last snapshot said.
  final dead = _bad(q) || q == SlotQuality.unpowered;
  on = on && q != SlotQuality.unpowered;
  final live = on && !dead;
  return Positioned(
    left: c.dx - 13,
    top: c.dy - 7,
    width: 26,
    height: 14,
    child: Container(
      decoration: BoxDecoration(
        color: live ? Color.alphaBlend(AppTheme.blend(AppTheme.green, 0.3), AppTheme.pane) : AppTheme.cardLine,
        borderRadius: BorderRadius.circular(7),
        border: sev == Severity.alarm ? Border.all(color: AppTheme.red) : null,
      ),
      alignment: on ? Alignment.centerRight : Alignment.centerLeft,
      padding: const EdgeInsets.symmetric(horizontal: 2),
      child: Container(
        width: 10,
        height: 10,
        decoration: BoxDecoration(
          shape: BoxShape.circle,
          color: dead
              ? AppTheme.txtFaint
              : live
              ? AppTheme.green
              : AppTheme.txtMute,
        ),
      ),
    ),
  );
}

/// Logic slot as a status pill: dot, role, state word.
Widget _pill(
  Offset c,
  double w,
  double h,
  String title,
  String value, {
  String? valueRight,
  required SlotQuality q,
  required Severity sev,
  bool attention = false,
  bool off = false,
}) {
  final vst = _mono(11.5, color: _bad(q) ? AppTheme.txtFaint : AppTheme.txt, weight: FontWeight.w600);
  final tone = switch (sev) {
    Severity.alarm => AppTheme.red,
    Severity.warn => AppTheme.amber,
    Severity.none => null,
  };
  final dot = _dotColor(q, sev, off: off, attention: attention);
  return Positioned(
    left: c.dx - w / 2,
    top: c.dy - h / 2,
    width: w,
    height: h,
    child: Container(
      decoration: BoxDecoration(
        color: tone == null ? AppTheme.card : Color.alphaBlend(AppTheme.blend(tone, 0.10), AppTheme.card),
        border: Border.all(color: tone ?? AppTheme.cardLine, width: tone == null ? 1 : 1.6),
        borderRadius: BorderRadius.circular(h / 2),
      ),
      child: Stack(
        clipBehavior: Clip.none,
        children: [
          _dot(Offset(14, h / 2), dot),
          _text(
            24,
            h / 2 - 3,
            title.toUpperCase(),
            _mono(10, color: AppTheme.txtMute, weight: FontWeight.w700, ls: 0.5),
          ),
          _text(24, h / 2 + 12, value, vst, width: w - 38 - (valueRight != null ? 56 : 0)),
          if (valueRight != null) _text(w - 16, h / 2 + 12, valueRight, vst, align: TextAlign.right, width: 56),
        ],
      ),
    ),
  );
}

class _HitTarget extends StatefulWidget {
  final String label;
  final bool selected;
  final VoidCallback onTap;

  const _HitTarget({super.key, required this.label, required this.selected, required this.onTap});

  @override
  State<_HitTarget> createState() => _HitTargetState();
}

class _HitTargetState extends State<_HitTarget> {
  bool _focus = false;

  @override
  Widget build(BuildContext context) {
    return Semantics(
      button: true,
      selected: widget.selected,
      label: widget.label,
      child: FocusableActionDetector(
        mouseCursor: SystemMouseCursors.click,
        onShowFocusHighlight: (v) => setState(() => _focus = v),
        actions: {ActivateIntent: CallbackAction<ActivateIntent>(onInvoke: (_) => widget.onTap())},
        child: GestureDetector(
          behavior: HitTestBehavior.opaque,
          onTap: widget.onTap,
          child: DecoratedBox(
            decoration: BoxDecoration(
              border: _focus || widget.selected ? Border.all(color: AppTheme.accent, width: 2) : null,
            ),
          ),
        ),
      ),
    );
  }
}

// ── Wires and symbols ─────────────────────────────────────────────────────

enum _Medium { power, rf, lan, control, data }

class _SchematicPainter extends CustomPainter {
  final SchematicWires s;

  _SchematicPainter(this.s);

  @override
  bool shouldRepaint(_SchematicPainter old) => old.s != s;

  @override
  void paint(Canvas canvas, Size size) {
    final c = canvas;
    // Zone rules.
    final rule = Paint()
      ..color = AppTheme.cardLine
      ..strokeWidth = 1;
    for (final y in [112.0, 292.0, 572.0, 722.0]) {
      c.drawLine(Offset(0, y), Offset(kSchematicWidth, y), rule);
    }

    // Hosts on the LAN.
    _wire(c, const [Offset(16, 102), Offset(604, 102)], _Medium.lan, live: s.link);
    for (final hb in _hostBoxes) {
      _wire(c, [Offset(hb.x + 28, 86), Offset(hb.x + 28, 102)], _Medium.lan, live: s.link);
    }

    // House mains and its bus.
    final sym = Paint()
      ..style = PaintingStyle.stroke
      ..strokeWidth = 1.6
      ..color = AppTheme.accent;
    c.drawCircle(const Offset(30, 178), 12, sym);
    c.drawPath(
      Path()
        ..moveTo(22, 178)
        ..quadraticBezierTo(26, 171, 30, 178)
        ..quadraticBezierTo(34, 185, 38, 178),
      sym,
    );
    _wire(c, const [Offset(42, 178), Offset(640, 178)], _Medium.power, live: s.link);

    // PC mains: up across the LAN (line hop) into bwpc.
    _wire(c, const [Offset(118, 178), Offset(118, 153)], _Medium.power, live: s.link);
    _stroke(
      c,
      Path()
        ..moveTo(118, 139)
        ..lineTo(118, 107)
        ..arcToPoint(const Offset(118, 97), radius: const Radius.circular(5), clockwise: false)
        ..lineTo(118, 86),
      _Medium.power,
      live: s.pc,
    );

    // 13.8 V TRX PSU and its bus.
    _wire(c, const [Offset(250, 178), Offset(250, 199)], _Medium.power, live: s.link);
    _wire(c, const [Offset(250, 213), Offset(250, 224)], _Medium.power, live: s.psu);
    _wire(c, const [Offset(250, 258), Offset(250, 276)], _Medium.power, live: s.psu);
    _wire(c, const [Offset(24, 276), Offset(505, 276)], _Medium.power, live: s.psu);
    _wire(c, const [Offset(100, 276), Offset(100, 336)], _Medium.power, live: s.psu); // FLEX
    _wire(c, const [Offset(400, 276), Offset(400, 336)], _Medium.power, live: s.psu); // ATR-1000
    _wire(c, [const Offset(505, 276), Offset(505, _sel.dy - _selR)], _Medium.power, live: s.psu); // switch
    _wire(c, const [Offset(24, 276), Offset(24, 640), Offset(40, 640)], _Medium.power, live: s.psu); // IC-9700
    _wire(c, const [Offset(24, 512), Offset(40, 512)], _Medium.power, live: s.psu); // PLC #1
    for (final p in const [Offset(250, 276), Offset(100, 276), Offset(400, 276), Offset(24, 512)]) {
      _junction(c, p, s.psu);
    }

    // Antenna mains: rotators and the Ultrabeam controller.
    _wire(c, const [Offset(630, 178), Offset(630, 199)], _Medium.power, live: s.link);
    _wire(c, const [Offset(630, 213), Offset(630, 276)], _Medium.power, live: s.ant);
    _wire(
      c,
      const [Offset(617, 276), Offset(754, 276), Offset(754, 712), Offset(618, 712), Offset(618, 698)],
      _Medium.power,
      live: s.ant,
    );
    _wire(c, const [Offset(704, 712), Offset(704, 698)], _Medium.power, live: s.ant);
    _wire(c, const [Offset(617, 276), Offset(617, 296)], _Medium.power, live: s.ant);
    _wire(c, const [Offset(704, 276), Offset(704, 296)], _Medium.power, live: s.ant);
    for (final p in const [Offset(630, 276), Offset(704, 276), Offset(704, 712)]) {
      _junction(c, p, s.ant);
    }
    for (final p in const [Offset(118, 178), Offset(250, 178), Offset(630, 178)]) {
      _junction(c, p, s.link);
    }

    // Sequencer data links to the plugs it switches.
    _wire(c, const [Offset(358, 206), Offset(264, 206)], _Medium.data);
    _wire(c, const [Offset(482, 206), Offset(616, 206)], _Medium.data);

    // HF RF chain.
    _wire(c, const [Offset(160, 386), Offset(186, 386)], _Medium.rf, live: s.rf, carrier: s.carrier);
    _wire(c, const [Offset(316, 386), Offset(342, 386)], _Medium.rf, live: s.rf, carrier: s.carrier);
    _wire(c, [const Offset(458, 386), Offset(_sel.dx - _selR, 386)], _Medium.rf, live: s.rf, carrier: s.carrier);

    // Antenna switch: port fan, body, wiper on the selected port.
    for (final p in schematicPorts) {
      final on = s.port == p.port;
      _wire(
        c,
        [_terminal(p.ang), Offset(572, p.row), Offset(586, p.row)],
        _Medium.rf,
        live: on && s.rf,
        carrier: on && s.carrier,
      );
    }
    c.drawCircle(_sel, _selR, Paint()..color = AppTheme.pane);
    c.drawCircle(
      _sel,
      _selR,
      Paint()
        ..style = PaintingStyle.stroke
        ..strokeWidth = s.switchAlarm ? 2.2 : 1.6
        ..color = s.switchAlarm
            ? AppTheme.red
            : s.switchDead
            ? AppTheme.cardLine
            : AppTheme.accent,
    );
    final term = Paint()
      ..style = PaintingStyle.stroke
      ..strokeWidth = 1.4
      ..color = AppTheme.txtMute;
    for (final p in schematicPorts) {
      c.drawCircle(_terminal(p.ang), 3, Paint()..color = AppTheme.pane);
      c.drawCircle(_terminal(p.ang), 3, term);
    }
    final cur = schematicPorts.where((p) => p.port == s.port).firstOrNull;
    if (cur != null) {
      c.drawPath(
        Path()
          ..moveTo(_sel.dx - _selR, _sel.dy)
          ..lineTo(_sel.dx, _sel.dy)
          ..lineTo(_terminal(cur.ang).dx, _terminal(cur.ang).dy),
        Paint()
          ..style = PaintingStyle.stroke
          ..strokeWidth = 3
          ..strokeCap = StrokeCap.round
          ..strokeJoin = StrokeJoin.round
          ..color = s.carrier ? AppTheme.red : AppTheme.accent,
      );
    }
    c.drawCircle(_sel, 3.5, Paint()..color = AppTheme.txtMute);

    _antennas(c);

    // Rotator + controller act on the Ultrabeam; PLC #1 relays on FLEX/ACOM.
    _wire(c, const [Offset(617, 330), Offset(617, 336), Offset(705, 336), Offset(705, 330)], _Medium.control);
    _wire(c, const [Offset(617, 336), Offset(617, 344)], _Medium.control);
    _wire(c, const [Offset(100, 482), Offset(100, 436)], _Medium.control);
    _wire(c, const [Offset(222, 482), Offset(222, 436)], _Medium.control);
    _wire(c, const [Offset(284, 482), Offset(284, 436)], _Medium.control);
    // Antenna select drives the switch.
    _wire(c, [const Offset(484, 512), const Offset(505, 512), Offset(505, _sel.dy + _selR)], _Medium.data);

    // UHF.
    _wire(c, const [Offset(164, 640), Offset(588, 640)], _Medium.rf, live: s.uhfRf);
    _wire(c, const [Offset(530, 612), Offset(612, 612), Offset(612, 630)], _Medium.control);
    _wire(c, const [Offset(618, 664), Offset(618, 658), Offset(704, 658), Offset(704, 664)], _Medium.control);
    _wire(c, const [Offset(618, 658), Offset(618, 650)], _Medium.control);

    // Legend samples.
    const ly = 756.0;
    _wire(c, const [Offset(18, ly), Offset(44, ly)], _Medium.power, live: true);
    _wire(c, const [Offset(130, ly), Offset(156, ly)], _Medium.power, live: false);
    _wire(c, const [Offset(214, ly), Offset(240, ly)], _Medium.rf, live: true);
    _wire(c, const [Offset(324, ly), Offset(350, ly)], _Medium.rf, live: true, carrier: true);
    _wire(c, const [Offset(430, ly), Offset(456, ly)], _Medium.control);
    _wire(c, const [Offset(538, ly), Offset(564, ly)], _Medium.data);
  }

  void _antennas(Canvas c) {
    Paint ink(bool on, [double w = 2]) => Paint()
      ..style = PaintingStyle.stroke
      ..strokeWidth = w
      ..strokeJoin = StrokeJoin.round
      ..color = on ? AppTheme.txtMute : AppTheme.cardLine;
    Paint fill(bool on) => Paint()..color = on ? AppTheme.txtMute : AppTheme.cardLine;

    // Ultrabeam, top view: boom with three elements; arrowheads = direction.
    c.drawLine(const Offset(590, 358), const Offset(642, 358), ink(true));
    for (final (x, h) in const [(600.0, 22.0), (616.0, 28.0), (632.0, 20.0)]) {
      c.drawLine(Offset(x, 358 - h / 2), Offset(x, 358 + h / 2), ink(true, 2.2));
    }
    final dir = s.direction;
    if (dir == 'forward' || dir == 'bidirectional') {
      c.drawPath(
        Path()
          ..moveTo(642, 353)
          ..lineTo(650, 358)
          ..lineTo(642, 363)
          ..close(),
        fill(true),
      );
    }
    if (dir == 'reverse' || dir == 'bidirectional') {
      c.drawPath(
        Path()
          ..moveTo(590, 353)
          ..lineTo(582, 358)
          ..lineTo(590, 363)
          ..close(),
        fill(true),
      );
    }

    // Fan dipole: the generic antenna symbol, doubled.
    final dip = ink(s.port == 'port6', 1.8);
    const y = 398.0;
    c.drawPath(
      Path()
        ..moveTo(592, y)
        ..lineTo(592, y - 10)
        ..lineTo(604, y - 10)
        ..moveTo(584, y - 20)
        ..lineTo(600, y - 20)
        ..lineTo(592, y - 10)
        ..close()
        ..moveTo(596, y - 20)
        ..lineTo(612, y - 20)
        ..lineTo(604, y - 10)
        ..close(),
      dip,
    );
    // Port 5: open terminal.
    c.drawCircle(const Offset(590, 426), 3, ink(false, 1.4));
    // Dummy load: resistor to ground.
    c.drawPath(
      Path()
        ..moveTo(586, 452)
        ..lineTo(592, 452)
        ..lineTo(595, 446)
        ..lineTo(601, 458)
        ..lineTo(607, 446)
        ..lineTo(613, 458)
        ..lineTo(616, 452)
        ..lineTo(622, 452)
        ..lineTo(622, 460),
      ink(s.port == 'port1', 1.6),
    );
    _ground(c, const Offset(622, 460), ink(s.port == 'port1', 1.6));
    // Ground position.
    c.drawPath(
      Path()
        ..moveTo(586, 480)
        ..lineTo(594, 480)
        ..lineTo(594, 486),
      ink(s.port == 'off', 1.6),
    );
    _ground(c, const Offset(594, 486), ink(s.port == 'off', 1.6));

    // X-Quad: elements with the crossed element seen end-on as a dot.
    c.drawLine(const Offset(588, 640), const Offset(642, 640), ink(true));
    for (final (x, h) in const [(600.0, 20.0), (614.0, 24.0), (628.0, 18.0)]) {
      c.drawLine(Offset(x, 640 - h / 2), Offset(x, 640 + h / 2), ink(true, 2.2));
      c.drawCircle(Offset(x, 640), 3.4, fill(true));
    }
  }

  void _ground(Canvas c, Offset top, Paint p) {
    c.drawLine(top + const Offset(-7, 0), top + const Offset(7, 0), p);
    c.drawLine(top + const Offset(-4, 4), top + const Offset(4, 4), p);
    c.drawLine(top + const Offset(-1.5, 8), top + const Offset(1.5, 8), p);
  }

  void _junction(Canvas c, Offset p, bool live) =>
      c.drawCircle(p, 3, Paint()..color = live ? AppTheme.green : AppTheme.cardLine);

  void _wire(Canvas c, List<Offset> pts, _Medium m, {bool live = true, bool carrier = false}) {
    final path = Path()..moveTo(pts.first.dx, pts.first.dy);
    for (final p in pts.skip(1)) {
      path.lineTo(p.dx, p.dy);
    }
    _stroke(c, path, m, live: live, carrier: carrier);
  }

  void _stroke(Canvas c, Path path, _Medium m, {bool live = true, bool carrier = false}) {
    final p = Paint()
      ..style = PaintingStyle.stroke
      ..strokeJoin = StrokeJoin.round;
    switch (m) {
      case _Medium.power:
        c.drawPath(
          path,
          p
            ..color = live ? AppTheme.green : AppTheme.cardLine
            ..strokeWidth = 2,
        );
      case _Medium.rf:
        c.drawPath(
          path,
          p
            ..color = carrier
                ? AppTheme.red
                : live
                ? AppTheme.accent
                : AppTheme.cardLine
            ..strokeWidth = carrier ? 3.6 : 2.6,
        );
      case _Medium.lan:
        if (live) {
          c.drawPath(
            path,
            p
              ..color = AppTheme.txtMute
              ..strokeWidth = 1.5,
          );
        } else {
          _dashed(
            c,
            path,
            p
              ..color = AppTheme.red
              ..strokeWidth = 1.5,
            5,
            4,
          );
        }
      case _Medium.control:
        _dashed(
          c,
          path,
          p
            ..color = AppTheme.txtFaint
            ..strokeWidth = 1.3,
          4,
          3,
        );
      case _Medium.data:
        _dashed(
          c,
          path,
          p
            ..color = AppTheme.txtFaint
            ..strokeWidth = 1.6
            ..strokeCap = StrokeCap.round,
          0.1,
          3.6,
        );
    }
  }

  void _dashed(Canvas c, Path path, Paint p, double on, double off) {
    for (final metric in path.computeMetrics()) {
      var d = 0.0;
      while (d < metric.length) {
        final e = math.min(d + on, metric.length);
        c.drawPath(metric.extractPath(d, e), p);
        d = e + off;
      }
    }
  }
}

// ── Details popover ───────────────────────────────────────────────────────

class _Details {
  final String title;
  final String subtitle;
  final List<(String, String, Color?)> rows;
  const _Details(this.title, this.subtitle, this.rows);
}

const _slotNames = {
  'muehle/power/psu-13v8': 'PSU mains plug',
  'muehle/power/antennas': 'Antenna mains plug',
  'muehle/power/shack-pc-mains': 'PC mains plug',
  'muehle/hf/radio': 'FLEX-8400',
  'muehle/hf/pa': 'ACOM 1200S',
  'muehle/hf/tuner': 'ATR-1000',
  'muehle/hf/ant-switch': 'Antenna switch',
  'muehle/hf/ant-ctrl': 'Ultrabeam RCU-06',
  'muehle/hf/rotator': 'G-450 rotator',
  'muehle/hf/switch': 'PLC #1 relays',
  'muehle/hf/pa-arm': 'PLC #1 PA arm',
  'muehle/hf/antenna-select': 'Antenna select',
  'muehle/hf/beam-steer': 'Beam steer',
  'muehle/hf/ant-mode': 'Antenna mode',
  'muehle/hf/power-seq': 'Sequencer',
  'muehle/uhf/radio': 'IC-9700',
  'muehle/uhf/sat-track': 'Sat track',
  'muehle/uhf/pol-ctrl': 'PLC #2 polarization',
  'muehle/uhf/az-rotator': 'SPID azimuth',
  'muehle/uhf/el-rotator': 'GS-500 elevation',
};

String _clock(DateTime? t) {
  if (t == null) return '—';
  String two(int n) => n.toString().padLeft(2, '0');
  return '${two(t.hour)}:${two(t.minute)}:${two(t.second)}';
}

_Details _details(StationSchematicModel m, String id) {
  if (id.startsWith('host:')) {
    final host = id.substring(5);
    final h = stationHosts[host]!;
    final rows = <(String, String, Color?)>[
      ('Address', h.address, null),
      ('Kind', h.kind, null),
      ('Runs', h.role, null),
    ];
    if (host == 'bwpc') {
      final st = m.store.slots[shackPcSlot]?.status;
      rows.add((
        'Agent',
        st == 'online'
            ? 'awake'
            : st == 'offline'
            ? 'asleep'
            : 'not heard',
        null,
      ));
    }
    final pcAsleep = m.quality(shackPcSlot) == SlotQuality.unpowered;
    m.hostHealth(host).components.forEach((comp, up) {
      // The PC agent's LWT fires whenever bwpc sleeps — expected, not down.
      if (!up && pcAsleep && slotHosts[shackPcSlot]?.component == comp) {
        rows.add((comp, 'asleep', AppTheme.txtMute));
      } else {
        rows.add((comp, up ? 'online' : 'down', up ? AppTheme.green : AppTheme.red));
      }
    });
    return _Details(h.name, h.address, rows);
  }
  if (id == psuCardId) {
    final d = _details(m, psuSlot);
    return _Details('13.8 V TRX PSU', d.subtitle, [
      (
        'Output',
        switch (m.psuPower) {
          true => '13.8 V',
          false => 'off',
          null => 'unknown',
        },
        null,
      ),
      ('Feeds', 'FLEX, ATR-1000, antenna switch, PLC #1, IC-9700', null),
      ...d.rows,
    ]);
  }
  final q = m.quality(id);
  final (word, tone) = switch (q) {
    SlotQuality.ok => ('online', AppTheme.green),
    SlotQuality.noData => ('no data since connect', AppTheme.txtFaint),
    SlotQuality.down => ('bridge down', AppTheme.red),
    SlotQuality.lost => ('device unreachable', AppTheme.red),
    SlotQuality.unpowered => (id == shackPcSlot ? 'asleep' : 'unpowered (expected)', AppTheme.amber),
    SlotQuality.stale => ('stale, link down', AppTheme.red),
  };
  final sh = slotHosts[id];
  final slot = m.store.slots[id];
  final device = slot?.meta?['device'];
  final rows = <(String, String, Color?)>[
    ('State', word, tone),
    if (m.problemText(id) case final p?) ('Problem', p, AppTheme.red),
    ('Since', _clock(m.lastChange(id)), null),
    if (sh != null) ('Bridge', sh.component, null),
    if (sh != null) ('Host', sh.host == embeddedHost ? 'own firmware' : sh.host, null),
    if (device is Map && device['model'] is String)
      ('Device', '${device['manufacturer'] ?? ''} ${device['model']}'.trim(), null),
  ];
  slot?.state?.forEach((k, v) {
    rows.add((
      k,
      v == null
          ? '—'
          : v is Map || v is List
          ? jsonEncode(v)
          : '$v',
      null,
    ));
  });
  return _Details(_slotNames[id] ?? id, id, rows);
}

class _PopoverLayout extends SingleChildLayoutDelegate {
  final Rect anchor;
  _PopoverLayout(this.anchor);

  static const _w = 280.0, _gap = 8.0;

  @override
  BoxConstraints getConstraintsForChild(BoxConstraints c) =>
      BoxConstraints(maxWidth: math.min(_w, c.maxWidth - 2 * _gap), maxHeight: math.max(0, c.maxHeight - 2 * _gap));

  @override
  Offset getPositionForChild(Size size, Size child) {
    // Beside the block — right if it fits, else left — top-aligned with it.
    var x = anchor.right + _gap;
    if (x + child.width > size.width - _gap) x = anchor.left - _gap - child.width;
    x = x.clamp(_gap, math.max(_gap, size.width - child.width - _gap)).toDouble();
    final y = anchor.top.clamp(_gap, math.max(_gap, size.height - child.height - _gap)).toDouble();
    return Offset(x, y);
  }

  @override
  bool shouldRelayout(_PopoverLayout old) => old.anchor != anchor;
}

class _Popover extends StatelessWidget {
  final _Details details;
  final VoidCallback onClose;

  const _Popover({super.key, required this.details, required this.onClose});

  @override
  Widget build(BuildContext context) {
    return GestureDetector(
      // Taps inside the card must not reach the close-on-tap backdrop.
      onTap: () {},
      behavior: HitTestBehavior.opaque,
      child: Semantics(
        container: true,
        label: '${details.title} details',
        child: Container(
          decoration: BoxDecoration(
            color: AppTheme.card,
            border: Border.all(color: AppTheme.cardLineHi),

            boxShadow: const [BoxShadow(color: Color(0x66000000), blurRadius: 24, offset: Offset(0, 8))],
          ),
          padding: const EdgeInsets.fromLTRB(12, 4, 4, 12),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Row(
                children: [
                  Expanded(
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(details.title, style: AppTheme.body(13, weight: FontWeight.w600)),
                        Text(details.subtitle, style: AppTheme.mono(10.5, color: AppTheme.txtFaint)),
                      ],
                    ),
                  ),
                  IconButton(
                    key: const ValueKey('schematic-popover-close'),
                    tooltip: 'Close',
                    onPressed: onClose,
                    icon: Icon(Icons.close, size: 18, color: AppTheme.txtMute),
                  ),
                ],
              ),
              const SizedBox(height: 6),
              Flexible(
                child: SingleChildScrollView(
                  padding: const EdgeInsets.only(right: 8),
                  child: Column(
                    children: [
                      for (final (k, v, tone) in details.rows)
                        Padding(
                          padding: const EdgeInsets.only(bottom: 3),
                          child: Row(
                            crossAxisAlignment: CrossAxisAlignment.start,
                            children: [
                              Expanded(
                                flex: 2,
                                child: Text(k, style: AppTheme.body(11.5, color: AppTheme.txtMute)),
                              ),
                              const SizedBox(width: 12),
                              Expanded(
                                flex: 3,
                                child: Text(
                                  v,
                                  textAlign: TextAlign.right,
                                  style: AppTheme.mono(11, color: tone ?? AppTheme.txt, weight: FontWeight.w500),
                                ),
                              ),
                            ],
                          ),
                        ),
                    ],
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
