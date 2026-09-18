import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';
import 'status_pill.dart';
import 'status_tag.dart';

/// Sat-ops rotator surface (U8): per-axis readouts, goto, and the STOP
/// button — the operator's e-stop for the spid/ercm mount.
///
/// One panel for both axes (the plan's single-column shape), living on the
/// UHF tab. Reads the two sibling slots published by
/// spid-ercm-rotator-bridge:
///
/// - `muehle/uhf/az-rotator` — SPID azimuth, /state key `az`
/// - `muehle/uhf/el-rotator` — GS-500 elevation via ERC-M, /state key `el`
///
/// Gating is the station two-layer AND: per-axis controls require
/// `slot.isOnline && store.linkUp`, so stale pre-sleep state can never
/// render operable. The STOP is never disabled while ANY axis is operable
/// (`store.linkUp && (az online || el online)`) and every tap publishes
/// `{'action':'stop'}` to BOTH slots' /cmd topics — R8's bridge semantics
/// halt both axes from either topic, so the dual publish is belt-and-braces
/// against a dead slot path.
///
/// Goto targets are validated client-side against the axis travel limits in
/// /meta `capabilities.limits` (inclusive); with no /meta yet the panel
/// validates parse-only and the bridge's R10 refusal is the backstop,
/// surfacing via the faults bar. Both actions are one-shot (KTD13): the
/// payload builders are published with `cmdRetain[...]!` = false.
class SatRotatorPanel extends StatelessWidget {
  const SatRotatorPanel({super.key});

  static const _azSlot = 'uhf/az-rotator';
  static const _elSlot = 'uhf/el-rotator';
  static const _azAddress = 'muehle/uhf/az-rotator';
  static const _elAddress = 'muehle/uhf/el-rotator';

  /// Worst case across the two axes: the pill goes red while either rotor
  /// is in motion.
  bool _anyMoving(BusStore store) =>
      store.stateValueAs<bool>(_azAddress, 'moving') == true ||
      store.stateValueAs<bool>(_elAddress, 'moving') == true;

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final az = store.slots[_azAddress];
    final el = store.slots[_elAddress];
    final azOnline = (az?.isOnline ?? false) && store.linkUp;
    final elOnline = (el?.isOnline ?? false) && store.linkUp;
    // E-stop liveness: any operable axis keeps STOP tappable. linkUp alone
    // is not enough (both axes may be down), and per-slot liveness alone is
    // not enough (retained state survives a link loss).
    final stopEnabled =
        store.linkUp && ((az?.isOnline ?? false) || (el?.isOnline ?? false));

    void sendStop() {
      for (final (slot, address) in [(_azSlot, _azAddress), (_elSlot, _elAddress)]) {
        mqtt.publish(
          cmdTopic(slot),
          satRotatorStopPayload(),
          retain: cmdRetain[address]!,
        );
      }
    }

    final (pillSuffix, pillColor) = _anyMoving(store)
        ? ('MOVING', AppTheme.red)
        : ('', null);

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'SAT ROTATORS',
            trailing: StatusPill(
              slots: const [_azAddress, _elAddress],
              label: 'Rotators',
              useMetaName: false,
              suffix: pillSuffix.isEmpty ? null : pillSuffix,
              suffixColor: pillColor,
            ),
          ),
          const SizedBox(height: 12),
          _AxisControl(
            axis: 'az',
            label: 'AZIMUTH',
            slotName: _azSlot,
            slot: az,
            online: azOnline,
          ),
          Divider(height: 24, thickness: 1, color: AppTheme.cardLine),
          _AxisControl(
            axis: 'el',
            label: 'ELEVATION',
            slotName: _elSlot,
            slot: el,
            online: elOnline,
          ),
          const SizedBox(height: 16),
          // The e-stop: full-width, red, unmissable. Never disabled while
          // any axis is operable — it must outlive one dead serial port.
          ElevatedButton(
            key: const ValueKey('sat-stop'),
            onPressed: stopEnabled ? sendStop : null,
            style: AppTheme.actionButton(danger: true, fullWidth: true).copyWith(
              padding: const WidgetStatePropertyAll(
                EdgeInsets.symmetric(vertical: 14),
              ),
            ),
            child: Text(
              'STOP',
              style: AppTheme.mono(16,
                  color: AppTheme.txt, weight: FontWeight.w800, letterSpacing: 0.2),
            ),
          ),
        ],
      ),
    );
  }
}

/// One axis: position / target / moving readout, plus a goto input — a
/// decimal-degree text field with ±1° steppers, validated against the
/// travel limits from /meta capabilities — and a GOTO publish button.
///
/// The text field stays editable while offline so an operator can pre-type
/// the next target; only the publish (GOTO) and the steppers gate on
/// liveness.
class _AxisControl extends StatefulWidget {
  final String axis; // 'az' | 'el' — the /state position key
  final String label; // 'AZIMUTH' | 'ELEVATION'
  final String slotName; // 'uhf/az-rotator' | 'uhf/el-rotator'
  final Slot? slot;
  final bool online;

  const _AxisControl({
    required this.axis,
    required this.label,
    required this.slotName,
    required this.slot,
    required this.online,
  });

  @override
  State<_AxisControl> createState() => _AxisControlState();
}

class _AxisControlState extends State<_AxisControl> {
  final _controller = TextEditingController();

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final mqtt = context.read<MqttService>();
    final store = context.read<BusStore>();
    final address = 'muehle/${widget.slotName}';
    final state = widget.slot?.state;

    // Position key is the axis name; an invalid readback omits it (KTD9's
    // never-a-fabricated-position rule, bus side) — render a dash. The
    // store's safe accessor (like pol_ctrl_panel) also degrades a
    // type-confused payload (the readwrite hf account can publish `az` as a
    // String) to a dash instead of throwing a TypeError out of build.
    final position = store.stateValueAs<num>(address, widget.axis)?.toDouble();
    final target = store.stateValueAs<num>(address, 'target')?.toDouble();
    final moving = state?['moving'] == true;
    final error = state?['error'];
    final errorText = error is String && error.isNotEmpty ? error : null;

    // Travel envelope from /meta capabilities — the same limits the bridge
    // refuses on (R10). Absent /meta ⇒ null limits ⇒ parse-only validation.
    // Guarded with `is num` so a malformed /meta degrades the same way.
    final caps = widget.slot?.meta?['capabilities'];
    final limits = caps is Map ? caps['limits'] : null;
    final minRaw = limits is Map ? limits['min'] : null;
    final maxRaw = limits is Map ? limits['max'] : null;
    final minLimit = minRaw is num ? minRaw.toDouble() : null;
    final maxLimit = maxRaw is num ? maxRaw.toDouble() : null;

    final text = _controller.text.trim();
    final parsed = text.isEmpty ? null : double.tryParse(text);
    final valid = parsed != null &&
        parsed.isFinite &&
        (minLimit == null || parsed >= minLimit) &&
        (maxLimit == null || parsed <= maxLimit);
    final gotoEnabled = widget.online && valid;

    void publishGoto() {
      // Re-parse at publish time: the build-frame `parsed` goes stale when
      // step() rewrites the controller text (programmatic writes fire no
      // onChanged), and a stale closure here once published the pre-step
      // bearing. The controller is the single source of truth.
      final deg = double.tryParse(_controller.text.trim());
      if (deg == null) return;
      mqtt.publish(
        cmdTopic(widget.slotName),
        satRotatorGotoPayload(deg),
        retain: cmdRetain['muehle/${widget.slotName}']!,
      );
    }

    void step(int dir) {
      if (!widget.online) return;
      // Step from the typed value; fall back to the live position, then to
      // the envelope floor, so an empty input still steps from somewhere.
      final base = parsed?.isFinite == true
          ? parsed!
          : position ?? minLimit ?? 0.0;
      var next = base + dir;
      if (minLimit != null && next < minLimit) next = minLimit;
      if (maxLimit != null && next > maxLimit) next = maxLimit;
      _controller.text = _fmtDeg(next);
      // Rebuild: without this, parsed/valid/gotoEnabled and the GOTO button's
      // closure keep the pre-step frame (a programmatic controller write fires
      // no onChanged) — GOTO then published the stale bearing.
      setState(() {});
    }

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            Text(
              widget.label,
              style: AppTheme.mono(12,
                  weight: FontWeight.w700,
                  letterSpacing: 0.14,
                  color: AppTheme.txtMute),
            ),
            const SizedBox(width: 10),
            if (moving) StatusTag(label: 'MOVING', color: AppTheme.amber),
            if (errorText != null) ...[
              StatusTag(label: 'ERR', color: AppTheme.red),
              const SizedBox(width: 4),
              Expanded(
                child: Text(
                  errorText,
                  maxLines: 1,
                  overflow: TextOverflow.ellipsis,
                  style: AppTheme.mono(11, color: AppTheme.red),
                ),
              ),
            ] else
              const Spacer(),
            if (!widget.online) StatusTag(label: 'OFFLINE', color: AppTheme.txtMute),
          ],
        ),
        const SizedBox(height: 6),
        Row(
          crossAxisAlignment: CrossAxisAlignment.center,
          children: [
            // Position readout — mono, large, degrees.
            Text(
              position != null ? '${_fmtDeg(position)}°' : '—',
              style: AppTheme.mono(20, weight: FontWeight.w700),
            ),
            if (target != null) ...[
              const SizedBox(width: 8),
              Text('→', style: AppTheme.mono(14, color: AppTheme.txtFaint)),
              const SizedBox(width: 8),
              Text(
                '${_fmtDeg(target)}°',
                style:
                    AppTheme.mono(20, weight: FontWeight.w700, color: AppTheme.txtMute),
              ),
            ],
          ],
        ),
        const SizedBox(height: 8),
        Row(
          children: [
            _stepButton(dir: -1, onStep: () => step(-1)),
            const SizedBox(width: 6),
            SizedBox(
              width: 96,
              child: TextField(
                key: ValueKey('sat-${widget.axis}-input'),
                controller: _controller,
                onChanged: (_) => setState(() {}),
                keyboardType:
                    const TextInputType.numberWithOptions(decimal: true, signed: true),
                inputFormatters: [
                  FilteringTextInputFormatter.allow(RegExp(r'^-?\d*\.?\d*')),
                ],
                style: AppTheme.mono(16, weight: FontWeight.w600),
                decoration: InputDecoration(
                  hintText: 'deg',
                  hintStyle: AppTheme.mono(13, color: AppTheme.txtFaint),
                  isDense: true,
                  contentPadding:
                      const EdgeInsets.symmetric(horizontal: 10, vertical: 10),
                  enabledBorder: OutlineInputBorder(
                    borderSide: BorderSide(color: AppTheme.cardLineHi),
                  ),
                  focusedBorder: OutlineInputBorder(
                    borderSide: BorderSide(color: AppTheme.green),
                  ),
                ),
              ),
            ),
            const SizedBox(width: 6),
            _stepButton(dir: 1, onStep: () => step(1)),
            const Spacer(),
            ElevatedButton(
              key: ValueKey('sat-${widget.axis}-goto'),
              onPressed: gotoEnabled ? publishGoto : null,
              style: AppTheme.actionButton().copyWith(
                padding: const WidgetStatePropertyAll(
                  EdgeInsets.symmetric(horizontal: 20, vertical: 12),
                ),
              ),
              child: Text('GOTO', style: AppTheme.mono(12, weight: FontWeight.w800)),
            ),
          ],
        ),
      ],
    );
  }

  Widget _stepButton({required int dir, required VoidCallback onStep}) {
    return SizedBox(
      width: 44,
      height: 53,
      child: ElevatedButton(
        key: ValueKey('sat-${widget.axis}-step-${dir > 0 ? 'up' : 'down'}'),
        onPressed: widget.online ? onStep : null,
        style: AppTheme.actionButton().copyWith(
          padding: const WidgetStatePropertyAll(EdgeInsets.zero),
        ),
        child: Icon(
          dir > 0 ? Icons.add : Icons.remove,
          size: 18,
          color: AppTheme.txt,
        ),
      ),
    );
  }
}

/// Whole degrees print without decimals (45 not 45.0); fractional keep theirs.
String _fmtDeg(double v) =>
    v == v.truncateToDouble() ? v.truncate().toString() : v.toString();