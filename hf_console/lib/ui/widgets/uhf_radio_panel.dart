import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';
import 'status_tag.dart';

/// IC-9700 radio surface (U7): the UHF tab's operating panel for the
/// `muehle/uhf/radio` slot published by icom9700-radio-bridge.
///
/// Deliberate deviations from the standard panel contract, both driven by
/// the on-demand session model (R1/R14):
///
/// - The readout ALWAYS renders — `session_state`
///   (idle/connecting/live/error) is the panel's core value, and a healthy
///   idle radio is `device_online:false` by design (R16), so the OFFLINE
///   tag is suppressed whenever a session-bearing snapshot exists and the
///   session tag renders instead. Healthy idle is not a fault.
/// - The ARM/DISARM toggle gates on the bus link ONLY: arm-while-idle is
///   the connect trigger, so gating it on a live session would make the
///   loop unreachable from this panel. A dead bridge simply never executes
///   the one-shot cmd — the faults bar carries the bridge-down row.
///
/// PTT and tuning actions gate on `armed ∧ session_state=live` plus the
/// two-layer link (bridge status + linkUp). Every readout comes from
/// `/state` — never tap optimism (the pol-ctrl rule). PTT is a toggle with
/// a pending window: set on tap, ended by the first `/state` whose `ts`
/// differs from the tapped snapshot (clock-free "newer than the tap" — the
/// bridge stamps every republish); a 5 s silence times out into the ERR
/// rendering ("no bus confirmation"). `tx` is read as the canonical
/// `"tx"|"rx"` string enum (the antenna/dvk pattern, not a bool). Safe
/// accessors throughout: a type-confused payload renders dashes, never
/// throws (the sat-panel lesson).
///
/// There is no select-VFO cmd (R9), so tuning cmds carry the target VFO —
/// the local MAIN/SUB picker defaults to the bus `selected_vfo` readback
/// and overrides stay console-local until a cmd is published.
class UhfRadioPanel extends StatefulWidget {
  const UhfRadioPanel({super.key});

  @override
  State<UhfRadioPanel> createState() => _UhfRadioPanelState();
}

class _UhfRadioPanelState extends State<UhfRadioPanel> {
  static const _slot = 'uhf/radio';
  static const _address = 'muehle/uhf/radio';
  static const _modes = ['cw', 'usb', 'lsb', 'am', 'fm', 'data'];
  static const _pttConfirmTimeout = Duration(seconds: 5);

  final _freqController = TextEditingController();
  Timer? _pttTimer;

  /// The `/state` `ts` captured when PTT was tapped — the pending window's
  /// baseline. Any differing ts on a later rebuild ends the window.
  String? _pttTsAtTap;
  bool _pttPending = false;
  bool _pttTimedOut = false;

  /// Local tune-target VFO. Null = follow the bus `selected_vfo` readback.
  String? _targetVfo;

  @override
  void dispose() {
    _pttTimer?.cancel();
    _freqController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();
    final slot = store.slots[_address];
    final bridgeUp = (slot?.bridgeOnline ?? false) && store.linkUp;

    final session = store.stateValueAs<String>(_address, 'session_state');
    final armed = store.stateValueAs<bool>(_address, 'armed') ?? false;
    final live = session == 'live';
    final tx = store.stateValueAs<String>(_address, 'tx');
    final error = slot?.state?['error'];
    final errorText = error is String && error.isNotEmpty ? error : null;
    final stateTs = slot?.state?['ts'];

    // PTT pending resolution: the first `/state` whose ts differs from the
    // tapped snapshot ends the window (success and error rendering stay
    // readback-driven); a state arriving after a timeout clears the timeout.
    if ((_pttPending || _pttTimedOut) && stateTs != _pttTsAtTap) {
      _pttPending = false;
      _pttTimedOut = false;
      _pttTsAtTap = null;
      _pttTimer?.cancel();
      _pttTimer = null;
    }

    // Two-layer gate + the settled safety preconditions. Arm is the
    // deliberate exception (see class doc): the bus link alone.
    final armEnabled = store.linkUp;
    final pttGated = bridgeUp && armed && live;
    final pttEnabled = pttGated && !_pttPending;
    final tuneEnabled = pttGated;

    final selectedVfo = store.stateValueAs<String>(_address, 'selected_vfo');
    final targetVfo =
        _targetVfo ?? (selectedVfo == 'sub' ? 'sub' : 'main');

    final freqText = _freqController.text.trim();
    final freqMhz = double.tryParse(freqText);
    final freqValid = freqMhz != null && freqMhz.isFinite && freqMhz > 0;

    void publish(String payload) =>
        mqtt.publish(cmdTopic(_slot), payload, retain: cmdRetain[_address]!);

    void toggleArm() {
      if (!armEnabled) return;
      publish(armed ? uhfRadioDisarmPayload() : uhfRadioArmPayload());
    }

    void togglePtt() {
      if (!pttEnabled) return;
      setState(() {
        _pttTsAtTap = stateTs is String ? stateTs : stateTs?.toString();
        _pttPending = true;
        _pttTimedOut = false;
      });
      _pttTimer?.cancel();
      _pttTimer = Timer(_pttConfirmTimeout, () {
        if (!mounted) return;
        setState(() {
          if (_pttPending) {
            _pttPending = false;
            _pttTimedOut = true;
          }
        });
      });
      publish(uhfRadioPttPayload(tx != 'tx'));
    }

    void setFreq() {
      if (!tuneEnabled || !freqValid) return;
      publish(uhfRadioSetFreqPayload(targetVfo, (freqMhz * 1e6).round()));
      _freqController.clear();
    }

    final sessionTag = _sessionTag(session, bridgeUp);
    final txLive = tx == 'tx';
    final errLine = _pttTimedOut ? 'PTT: no bus confirmation' : errorText;
    final meters = _meters(slot);

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'IC-9700 UHF RADIO',
            trailing: Row(
              mainAxisSize: MainAxisSize.min,
              children: [
                if (txLive) ...[
                  StatusTag(label: 'TX', color: AppTheme.red),
                  const SizedBox(width: 4),
                ],
                if (errLine != null) ...[
                  StatusTag(label: 'ERR', color: AppTheme.red),
                  const SizedBox(width: 4),
                ],
                if (!bridgeUp)
                  StatusTag(label: 'OFFLINE', color: AppTheme.txtMute)
                else if (sessionTag != null)
                  sessionTag,
              ],
            ),
          ),
          const SizedBox(height: 12),
          _VfoRow(
            label: 'MAIN',
            state: _vfoState(slot, 'main'),
            selected: selectedVfo == 'main',
          ),
          const SizedBox(height: 8),
          _VfoRow(
            label: 'SUB',
            state: _vfoState(slot, 'sub'),
            selected: selectedVfo == 'sub',
          ),
          ...[if (meters != null) ...[
            const SizedBox(height: 8),
            meters,
          ]],
          if (errLine != null) ...[
            const SizedBox(height: 4),
            Text(
              errLine,
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.mono(11, color: AppTheme.red),
            ),
          ],
          const SizedBox(height: 12),
          Row(
            children: [
              Expanded(
                child: ElevatedButton(
                  key: const ValueKey('uhf-arm-toggle'),
                  onPressed: armEnabled ? toggleArm : null,
                  style: AppTheme.actionButton(dangerActive: armed),
                  child: Text(
                    armed ? 'DISARM' : 'ARM',
                    style: AppTheme.mono(13, weight: FontWeight.w800),
                  ),
                ),
              ),
              const SizedBox(width: 6),
              Expanded(
                child: ElevatedButton(
                  key: const ValueKey('uhf-ptt-toggle'),
                  onPressed: pttEnabled ? togglePtt : null,
                  style: AppTheme.actionButton(dangerActive: txLive),
                  child: Text(
                    _pttPending
                        ? 'PTT …'
                        : (txLive ? 'PTT OFF' : 'PTT ON'),
                    style: AppTheme.mono(13, weight: FontWeight.w800),
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 14),
          Text(
            'TUNE · TARGET $targetVfo'.toUpperCase(),
            style: AppTheme.mono(10,
                color: AppTheme.txtFaint,
                weight: FontWeight.w600,
                letterSpacing: 0.14),
          ),
          const SizedBox(height: 6),
          Row(
            children: [
              _vfoTargetButton('main', targetVfo),
              const SizedBox(width: 6),
              _vfoTargetButton('sub', targetVfo),
            ],
          ),
          const SizedBox(height: 6),
          Row(
            children: [
              Expanded(
                child: TextField(
                  key: const ValueKey('uhf-freq-input'),
                  controller: _freqController,
                  onChanged: (_) => setState(() {}),
                  keyboardType: const TextInputType.numberWithOptions(
                      decimal: true, signed: false),
                  inputFormatters: [
                    FilteringTextInputFormatter.allow(RegExp(r'^\d*\.?\d*')),
                  ],
                  style: AppTheme.mono(16, weight: FontWeight.w600),
                  decoration: InputDecoration(
                    hintText: 'MHz',
                    hintStyle: AppTheme.mono(13, color: AppTheme.txtFaint),
                    isDense: true,
                    contentPadding: const EdgeInsets.symmetric(
                        horizontal: 10, vertical: 10),
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
              ElevatedButton(
                key: const ValueKey('uhf-freq-set'),
                onPressed: tuneEnabled && freqValid ? setFreq : null,
                style: AppTheme.actionButton().copyWith(
                  padding: const WidgetStatePropertyAll(
                    EdgeInsets.symmetric(horizontal: 20, vertical: 12),
                  ),
                ),
                child:
                    Text('SET FREQ', style: AppTheme.mono(12, weight: FontWeight.w800)),
              ),
            ],
          ),
          const SizedBox(height: 6),
          Row(
            children: [
              for (var i = 0; i < _modes.length; i++) ...[
                if (i > 0) const SizedBox(width: 4),
                Expanded(
                  child: ElevatedButton(
                    key: ValueKey('uhf-mode-${_modes[i]}'),
                    onPressed: tuneEnabled
                        ? () => publish(
                            uhfRadioSetModePayload(targetVfo, _modes[i]))
                        : null,
                    style: AppTheme.actionButton(
                      active: _vfoState(slot, targetVfo)?['mode'] == _modes[i],
                    ).copyWith(
                      minimumSize:
                          const WidgetStatePropertyAll(Size(0, 40)),
                      padding: const WidgetStatePropertyAll(
                          EdgeInsets.symmetric(horizontal: 4, vertical: 6)),
                      textStyle: WidgetStatePropertyAll(
                          AppTheme.mono(11, weight: FontWeight.w700)),
                    ),
                    child: Text(_modes[i].toUpperCase()),
                  ),
                ),
              ],
            ],
          ),
        ],
      ),
    );
  }

  Widget _vfoTargetButton(String vfo, String targetVfo) {
    return Expanded(
      child: ElevatedButton(
        key: ValueKey('uhf-vfo-target-$vfo'),
        // Local targeting stays tappable offline (pre-select the next
        // target — the sat-panel pre-typed-input convention); only the
        // publish gates on liveness.
        onPressed: () => setState(() => _targetVfo = vfo),
        style: AppTheme.actionButton(active: targetVfo == vfo),
        child: Text(vfo.toUpperCase(),
            style: AppTheme.mono(12, weight: FontWeight.w800)),
      ),
    );
  }

  /// Per-VFO detail object from the hybrid state shape (R5). A type-confused
  /// payload degrades to null — the row renders dashes.
  Map<String, dynamic>? _vfoState(Slot? slot, String vfo) {
    final v = slot?.state?[vfo];
    return v is Map<String, dynamic> ? v : null;
  }

  /// Meters row — only while a live snapshot carries numeric meter fields
  /// (R6: radio-measured fields are omitted off-session; absent = no chip,
  /// never a fabricated zero). Null when there is nothing to show.
  Widget? _meters(Slot? slot) {
    const meters = {'s_meter': 'S', 'tx_power': 'PWR', 'swr': 'SWR', 'alc': 'ALC'};
    final state = slot?.state;
    final chips = <Widget>[];
    if (state == null) return null;
    meters.forEach((key, label) {
      final v = state[key];
      if (v is num) {
        if (chips.isNotEmpty) chips.add(const SizedBox(width: 14));
        chips.add(Text(
          '$label ${_fmtNum(v)}',
          style: AppTheme.mono(13, color: AppTheme.txtMute, weight: FontWeight.w600),
        ));
      }
    });
    return chips.isEmpty ? null : Wrap(children: chips);
  }

  /// The session tag is the panel's core readout (R14): LIVE green,
  /// CONNECTING amber, IDLE muted, ERROR red. Unknown values render raw in
  /// muted ink (honest bus truth, the pol-ctrl rule) and a missing value
  /// renders nothing rather than a guessed state.
  StatusTag? _sessionTag(String? session, bool bridgeUp) {
    switch (session) {
      case 'live':
        return StatusTag(label: 'LIVE', color: AppTheme.green);
      case 'connecting':
        return StatusTag(label: 'CONNECTING', color: AppTheme.amber);
      case 'idle':
        return StatusTag(label: 'IDLE', color: AppTheme.txtMute);
      case 'error':
        return StatusTag(label: 'ERROR', color: AppTheme.red);
      default:
        return session == null
            ? null
            : StatusTag(label: session.toUpperCase(), color: AppTheme.txtMute);
    }
  }
}

class _VfoRow extends StatelessWidget {
  final String label;
  final Map<String, dynamic>? state;
  final bool selected;

  const _VfoRow({
    required this.label,
    required this.state,
    required this.selected,
  });

  @override
  Widget build(BuildContext context) {
    final freqHz = state?['freq_hz'];
    final mode = state?['mode'];
    final band = state?['band'];
    final dataMode = state?['data_mode'];

    final freqText = freqHz is num ? _fmtMhz(freqHz) : '—';
    final modeText = mode is String && mode.isNotEmpty ? mode.toUpperCase() : '—';
    final bandText = band is String && band.isNotEmpty ? band.toUpperCase() : '';

    return Row(
      crossAxisAlignment: CrossAxisAlignment.center,
      children: [
        SizedBox(
          width: 44,
          child: Text(
            label,
            style: AppTheme.mono(12,
                weight: FontWeight.w700,
                letterSpacing: 0.14,
                color: AppTheme.txtMute),
          ),
        ),
        Text(
          freqText,
          style: AppTheme.mono(20, weight: FontWeight.w700),
        ),
        if (freqText != '—') ...[
          const SizedBox(width: 2),
          Text(' MHz', style: AppTheme.mono(10, color: AppTheme.txtFaint)),
        ],
        const SizedBox(width: 12),
        Text(modeText,
            style: AppTheme.mono(14, weight: FontWeight.w700, color: AppTheme.accent)),
        if (bandText.isNotEmpty) ...[
          const SizedBox(width: 10),
          Text(bandText,
              style: AppTheme.mono(12,
                  color: AppTheme.txtMute, weight: FontWeight.w600)),
        ],
        if (dataMode == true) ...[
          const SizedBox(width: 8),
          Text('DATA',
              style: AppTheme.mono(10, color: AppTheme.txtFaint, weight: FontWeight.w700)),
        ],
        const Spacer(),
        if (selected) StatusTag(label: 'SEL', color: AppTheme.accent),
      ],
    );
  }
}

/// 432100000 → '432.100' (the dvk-panel MHz format).
String _fmtMhz(num hz) => (hz / 1e6).toStringAsFixed(3);

/// Integral nums print without decimals (120 not 120.0).
String _fmtNum(num v) =>
    v == v.truncate() ? v.truncate().toString() : v.toString();
