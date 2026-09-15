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

/// IC-9700 radio surface (U7): the `muehle/uhf/radio` slot published by
/// icom9700-radio-bridge, on the UHF tab.
///
/// Deliberate deviations from the pol-ctrl contract, per the plan's
/// on-demand session model:
///
/// - The readout ALWAYS renders — idle/connecting/live/error is the panel's
///   core value, not a fault. The OFFLINE tag renders whenever the slot is
///   not reachable over /status (bridge LWT); the session-state tag renders
///   only while it is: a dead bridge leaves a retained snapshot whose
///   session state must not read as current.
/// - ARM/DISARM gates on bridge liveness ONLY (store.linkUp && /status
///   online) — never slot.isOnline, which folds in device_online, and
///   device_online:false is the HEALTHY idle here (R16). Arm-while-idle is
///   the bridge's connect trigger (R1); gating the toggle on live, or on
///   device_online, would make the loop unreachable from the panel. PTT and
///   tuning gate on armed ∧ session_state=live (R10).
/// - Arm and PTT are pending-confirm toggles: the tap records the /state.ts
///   it was keyed against and clears on the first /state with a different
///   ts (the armed/tx flip or /state.error renders from that readback). A
///   5 s local timeout reverts to a "no bus confirmation" ERR tag. The
///   readout comes from /state readback only — never tap optimism (KTD15).
///
/// Publishes one-shot cmds (cmdRetain['muehle/uhf/radio'] = false) with the
/// per-VFO value-key payload builders from wiring.dart. sat_mode,
/// set_preamp, set_attenuator, set_data and set_power are out of panel for
/// v1 (the bus actions remain per icom9700-radio-bridge/docs/mqtt-api.md).
class UhfRadioPanel extends StatefulWidget {
  const UhfRadioPanel({super.key});

  static const _slot = 'uhf/radio';
  static const _address = 'muehle/uhf/radio';

  /// Canonical settable modes (mqtt-api.md set_mode: cw|usb|lsb|am|fm;
  /// `data` is the set_data modifier — out of panel v1).
  static const _modes = ['cw', 'usb', 'lsb', 'am', 'fm'];

  /// Frequency stepper quantum: 5 kHz — the satellite (Doppler) step.
  static const _stepHz = 5000;

  @override
  State<UhfRadioPanel> createState() => _UhfRadioPanelState();
}

/// One tap awaiting its /state confirmation: the tap is keyed against the
/// /state.ts it was published under, so any later snapshot (flip OR error)
/// settles it.
class _Pending {
  final String? tsAtTap;
  const _Pending(this.tsAtTap);
}

class _UhfRadioPanelState extends State<UhfRadioPanel> {
  static const _confirmWindow = Duration(seconds: 5);
  static const _noConfirmArm = 'no bus confirmation (arm)';
  static const _noConfirmPtt = 'no bus confirmation (ptt)';

  final _freqControllers = <String, TextEditingController>{
    'main': TextEditingController(),
    'sub': TextEditingController(),
  };

  BusStore? _store;
  Timer? _confirmTimer;

  _Pending? _armPending;
  _Pending? _pttPending;

  /// The 5 s timeout's ERR text (bus error outranks it in the render), and
  /// the /state.ts it was raised against — any newer snapshot proves the
  /// bus is alive again and clears the complaint, even though the timeout
  /// already consumed the pending.
  String? _localErr;
  String? _localErrTs;

  @override
  void initState() {
    super.initState();
    _store = context.read<BusStore>();
    _store!.addListener(_onStoreChanged);
  }

  @override
  void dispose() {
    _store?.removeListener(_onStoreChanged);
    _confirmTimer?.cancel();
    for (final c in _freqControllers.values) {
      c.dispose();
    }
    super.dispose();
  }

  void _onStoreChanged() {
    if (_settle()) setState(() {});
  }

  /// A pending clears on the first /state newer than the tap (a different
  /// ts — the bridge republishes the snapshot on every settle-worthy event),
  /// and a standing timeout complaint clears on any newer snapshot. Returns
  /// whether anything changed.
  bool _settle() {
    final store = _store;
    if (store == null) return false;
    final ts = _stateTs(store);
    var changed = false;
    if (_armPending != null && ts != _armPending!.tsAtTap) {
      _armPending = null;
      changed = true;
    }
    if (_pttPending != null && ts != _pttPending!.tsAtTap) {
      _pttPending = null;
      changed = true;
    }
    if (_localErr != null && _localErrTs != ts) {
      _localErr = null;
      _localErrTs = null;
      changed = true;
    }
    return changed;
  }

  /// The 5 s local timeout: a pending still keyed against the tap's ts has
  /// seen no confirmation — revert it into the no-confirmation ERR tag. A
  /// pending that settled meanwhile is skipped (one timer serves both
  /// toggles; only the latest tap re-arms it).
  void _onConfirmTimeout() {
    final store = _store;
    if (store == null) return;
    final ts = _stateTs(store);
    var changed = false;
    if (_armPending != null && ts == _armPending!.tsAtTap) {
      _armPending = null;
      _localErr = _noConfirmArm;
      _localErrTs = ts;
      changed = true;
    }
    if (_pttPending != null && ts == _pttPending!.tsAtTap) {
      _pttPending = null;
      _localErr = _noConfirmPtt;
      _localErrTs = ts;
      changed = true;
    }
    if (changed) setState(() {});
  }

  void _restartConfirmTimer() {
    _confirmTimer?.cancel();
    _confirmTimer = Timer(_confirmWindow, _onConfirmTimeout);
  }

  String? _stateTs(BusStore store) {
    final v = store.stateValue(UhfRadioPanel._address, 'ts');
    return v is String ? v : null;
  }

  void _tapArm(MqttService mqtt, {required bool armed, required String? ts}) {
    _armPending = _Pending(ts);
    if (_localErr == _noConfirmArm) {
      _localErr = null;
      _localErrTs = null;
    }
    _restartConfirmTimer();
    mqtt.publish(
      cmdTopic(UhfRadioPanel._slot),
      armed ? uhfRadioDisarmPayload() : uhfRadioArmPayload(),
      retain: cmdRetain[UhfRadioPanel._address]!,
    );
    setState(() {});
  }

  void _tapPtt(MqttService mqtt, {required String tx, required String? ts}) {
    _pttPending = _Pending(ts);
    if (_localErr == _noConfirmPtt) {
      _localErr = null;
      _localErrTs = null;
    }
    _restartConfirmTimer();
    // Toggle against the READBACK, never the last tap: the published tx
    // always follows the radio (mqtt-api.md).
    mqtt.publish(
      cmdTopic(UhfRadioPanel._slot),
      uhfRadioPttPayload(tx == 'tx' ? 'off' : 'on'),
      retain: cmdRetain[UhfRadioPanel._address]!,
    );
    setState(() {});
  }

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    const address = UhfRadioPanel._address;
    final slot = store.slots[address];
    // Bridge liveness ONLY — see the class doc for why device_online is
    // excluded from the arm gate (R16: false is the healthy idle here).
    final bridgeUp = (slot?.bridgeOnline ?? false) && store.linkUp;

    final sessionState = store.stateValueAs<String>(address, 'session_state');
    final live = sessionState == 'live';
    final armed = store.stateValueAs<bool>(address, 'armed') ?? false;
    final tx = store.stateValueAs<String>(address, 'tx') ?? 'rx';
    final selectedVfo = store.stateValueAs<String>(address, 'selected_vfo');
    final satellite = store.stateValueAs<bool>(address, 'satellite') ?? false;
    final ts = _stateTs(store);

    // Bus truth outranks the local timeout text (pol-ctrl ERR pattern).
    final busErr = slot?.state?['error'];
    final errText = busErr is String && busErr.isNotEmpty ? busErr : _localErr;

    // R10 gate: tuning and PTT need the permit AND a live session.
    final tuneEnabled = bridgeUp && armed && live;
    final armEnabled = bridgeUp && _armPending == null;
    final pttEnabled = tuneEnabled && _pttPending == null;

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'UHF RADIO · IC-9700',
            trailing: Row(
              mainAxisSize: MainAxisSize.min,
              children: [
                if (errText != null) ...[
                  StatusTag(label: 'ERR', color: AppTheme.red),
                  const SizedBox(width: 4),
                ],
                if (!bridgeUp) StatusTag(label: 'OFFLINE', color: AppTheme.txtMute),
              ],
            ),
          ),
          const SizedBox(height: 10),
          _sessionRow(
            bridgeUp: bridgeUp,
            sessionState: sessionState,
            live: live,
            armed: armed,
            tx: tx,
            satellite: satellite,
          ),
          if (errText != null) ...[
            const SizedBox(height: 4),
            Text(
              errText,
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.mono(11, color: AppTheme.red),
            ),
          ],
          const SizedBox(height: 10),
          _vfoSection(
            store: store,
            mqtt: mqtt,
            vfo: 'main',
            label: 'MAIN',
            selected: selectedVfo == 'main',
            detail: _vfoMap(store, 'main'),
            enabled: tuneEnabled,
          ),
          Divider(height: 20, thickness: 1, color: AppTheme.cardLine),
          _vfoSection(
            store: store,
            mqtt: mqtt,
            vfo: 'sub',
            label: 'SUB',
            selected: selectedVfo == 'sub',
            detail: _vfoMap(store, 'sub'),
            enabled: tuneEnabled,
          ),
          if (live) ...[
            const SizedBox(height: 8),
            _metersRow(store),
          ],
          const SizedBox(height: 10),
          Row(
            children: [
              Expanded(
                child: ElevatedButton(
                  key: const ValueKey('uhf-arm-btn'),
                  onPressed:
                      armEnabled ? () => _tapArm(mqtt, armed: armed, ts: ts) : null,
                  style: AppTheme.actionButton(active: armed),
                  child: Text(armed ? 'DISARM' : 'ARM',
                      style: AppTheme.mono(13, weight: FontWeight.w800)),
                ),
              ),
              const SizedBox(width: 8),
              Expanded(
                child: ElevatedButton(
                  key: const ValueKey('uhf-ptt-btn'),
                  onPressed:
                      pttEnabled ? () => _tapPtt(mqtt, tx: tx, ts: ts) : null,
                  style: AppTheme.actionButton(danger: true, dangerActive: tx == 'tx'),
                  child: Text('PTT', style: AppTheme.mono(13, weight: FontWeight.w800)),
                ),
              ),
            ],
          ),
        ],
      ),
    );
  }

  /// Session liveness row: the session-state tag ONLY while the slot is
  /// reachable over /status (a dead bridge leaves a retained snapshot whose
  /// IDLE/LIVE must not read as current — OFFLINE wins), the RX/TX chip from
  /// the string-enum readback (dvk pattern), and the ARMED / SAT tags.
  Widget _sessionRow({
    required bool bridgeUp,
    required String? sessionState,
    required bool live,
    required bool armed,
    required String tx,
    required bool satellite,
  }) {
    final (tag, color) = switch (sessionState) {
      'live' => ('LIVE', AppTheme.green),
      'connecting' => ('CONNECTING', AppTheme.amber),
      'error' => ('ERROR', AppTheme.red),
      'idle' => ('IDLE', AppTheme.txtMute),
      _ => ('—', AppTheme.txtMute),
    };
    return Row(
      children: [
        if (bridgeUp) StatusTag(label: tag, color: color),
        if (live) ...[
          const SizedBox(width: 6),
          StatusTag(
            label: tx == 'tx' ? 'TX' : 'RX',
            color: tx == 'tx' ? AppTheme.red : AppTheme.green,
          ),
        ],
        if (armed) ...[
          const SizedBox(width: 6),
          StatusTag(label: 'ARMED', color: AppTheme.amber),
        ],
        if (satellite) ...[
          const SizedBox(width: 6),
          StatusTag(label: 'SAT', color: AppTheme.accent),
        ],
      ],
    );
  }

  /// One VFO: readout (SEL marker from /state.selected_vfo readback), the
  /// freq entry (text field + steppers, the sat-rotator pattern) and the
  /// mode button row (the pol-ctrl pattern). The field stays editable while
  /// gated so the next target can be pre-typed; steppers/SET/mode taps gate
  /// on [enabled] (armed ∧ live ∧ bridge up).
  Widget _vfoSection({
    required BusStore store,
    required MqttService mqtt,
    required String vfo,
    required String label,
    required bool selected,
    required Map<String, dynamic>? detail,
    required bool enabled,
  }) {
    final controller = _freqControllers[vfo]!;
    final parsed = int.tryParse(controller.text.trim());
    final currentHz = _asHz(detail?['freq_hz']);
    final activeMode = _asStr(detail?['mode']);
    final band = _asStr(detail?['band']);

    void publishFreq() {
      final hz = int.tryParse(controller.text.trim());
      if (hz == null) return;
      mqtt.publish(
        cmdTopic(UhfRadioPanel._slot),
        uhfRadioSetFreqPayload(hz, vfo),
        retain: cmdRetain[UhfRadioPanel._address]!,
      );
    }

    void step(int dir) {
      if (!enabled) return;
      // Step from the typed value; fall back to the live readback so an
      // empty field still steps from somewhere (sat-rotator pattern).
      final base = parsed ?? currentHz ?? 0;
      controller.text = (base + dir * UhfRadioPanel._stepHz).toString();
      setState(() {});
    }

    final modeButtons = <Widget>[];
    for (var i = 0; i < UhfRadioPanel._modes.length; i++) {
      if (i > 0) modeButtons.add(const SizedBox(width: 4));
      final m = UhfRadioPanel._modes[i];
      modeButtons.add(Expanded(
        child: ElevatedButton(
          key: ValueKey('uhf-$vfo-mode-$m'),
          onPressed: enabled
              ? () => mqtt.publish(
                    cmdTopic(UhfRadioPanel._slot),
                    uhfRadioSetModePayload(m, vfo),
                    retain: cmdRetain[UhfRadioPanel._address]!,
                  )
              : null,
          style: AppTheme.actionButton(active: activeMode == m),
          child: Text(m.toUpperCase(),
              style: AppTheme.mono(12, weight: FontWeight.w800)),
        ),
      ));
    }

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            Text(label,
                style: AppTheme.mono(12,
                    weight: FontWeight.w700,
                    letterSpacing: 0.14,
                    color: AppTheme.txtMute)),
            const SizedBox(width: 8),
            if (selected) StatusTag(label: 'SEL', color: AppTheme.accent),
            const Spacer(),
            Text(
              currentHz != null ? '${_fmtMhz(currentHz)} MHz' : '—',
              style: AppTheme.mono(20, weight: FontWeight.w700),
            ),
            const SizedBox(width: 8),
            Text(
              (activeMode ?? '—').toUpperCase(),
              style: AppTheme.mono(13, weight: FontWeight.w700, color: AppTheme.accent),
            ),
            if (band != null) ...[
              const SizedBox(width: 8),
              Text(band.toUpperCase(),
                  style: AppTheme.mono(12,
                      color: AppTheme.txtMute, weight: FontWeight.w600)),
            ],
          ],
        ),
        const SizedBox(height: 8),
        Row(
          children: [
            _stepButton(vfo: vfo, dir: -1, enabled: enabled, onStep: () => step(-1)),
            const SizedBox(width: 6),
            SizedBox(
              width: 130,
              child: TextField(
                key: ValueKey('uhf-$vfo-freq-input'),
                controller: controller,
                onChanged: (_) => setState(() {}),
                keyboardType: TextInputType.number,
                inputFormatters: [
                  FilteringTextInputFormatter.allow(RegExp(r'^\d*')),
                ],
                style: AppTheme.mono(15, weight: FontWeight.w600),
                decoration: InputDecoration(
                  hintText: 'Hz',
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
            _stepButton(vfo: vfo, dir: 1, enabled: enabled, onStep: () => step(1)),
            const Spacer(),
            ElevatedButton(
              key: ValueKey('uhf-$vfo-freq-set'),
              onPressed: parsed != null && enabled ? publishFreq : null,
              style: AppTheme.actionButton(),
              child: Text('SET', style: AppTheme.mono(12, weight: FontWeight.w800)),
            ),
          ],
        ),
        const SizedBox(height: 6),
        Row(children: modeButtons),
      ],
    );
  }

  /// Raw 0-255 meter readbacks (mqtt-api.md: present when read, live only).
  Widget _metersRow(BusStore store) {
    String m(String key) =>
        _asNum(store.stateValue(UhfRadioPanel._address, key))?.toString() ?? '—';
    return Text(
      'S ${m('s_meter')} · PWR ${m('tx_power')} · SWR ${m('swr')} · ALC ${m('alc')}',
      style: AppTheme.mono(12, color: AppTheme.txtMute, weight: FontWeight.w600),
    );
  }

  Widget _stepButton({
    required String vfo,
    required int dir,
    required bool enabled,
    required VoidCallback onStep,
  }) {
    return SizedBox(
      width: 44,
      height: 48,
      child: ElevatedButton(
        key: ValueKey('uhf-$vfo-step-${dir > 0 ? 'up' : 'down'}'),
        onPressed: enabled ? onStep : null,
        style: AppTheme.actionButton().copyWith(
          padding: const WidgetStatePropertyAll(EdgeInsets.zero),
        ),
        child: Icon(dir > 0 ? Icons.add : Icons.remove, size: 18, color: AppTheme.txt),
      ),
    );
  }

  // --- safe accessors (the sat-panel lesson: a type-confused payload
  // degrades to a dash, never a TypeError out of build) ---

  Map<String, dynamic>? _vfoMap(BusStore store, String key) {
    final v = store.stateValue(UhfRadioPanel._address, key);
    return v is Map<String, dynamic> ? v : null;
  }

  static int? _asHz(dynamic v) => v is int ? v : (v is num ? v.toInt() : null);
  static num? _asNum(dynamic v) => v is num ? v : null;
  static String? _asStr(dynamic v) => v is String ? v : null;

  static String _fmtMhz(int hz) => (hz / 1e6).toStringAsFixed(3);
}

