import 'dart:async';

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';
import 'status_tag.dart';

/// IC-9700 radio surface (U7), receive-only (2026-09 pivot): an audio
/// capture toggle, an optional serial CI-V monitor toggle, and the
/// read-only readout (freq / mode / meters). Nothing here keys or steers
/// the radio — remote TX was removed from bridge and console; if control
/// ever returns it will be serial CI-V, not LAN.
///
/// Contract notes kept from the old panel: the readout ALWAYS renders —
/// an OFFLINE tag shows whenever the slot is unreachable over /status
/// (bridge LWT). The toggles are pending-confirm: the tap records the
/// /state.ts it keyed against and clears on the first /state with a
/// different ts (the demand flip or /state.error renders from that
/// readback); a 5 s local timeout reverts to a "no bus confirmation" ERR
/// tag. The readout comes from /state readback only — never tap optimism
/// (KTD15). One-shot publishes (cmdRetain['muehle/uhf/radio'] = false).
class UhfRadioPanel extends StatefulWidget {
  const UhfRadioPanel({super.key});

  static const _slot = 'uhf/radio';
  static const _address = 'muehle/uhf/radio';

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
  static const _noConfirmCapture = 'no bus confirmation (radio audio)';
  static const _noConfirmMonitor = 'no bus confirmation (monitor)';

  BusStore? _store;
  Timer? _confirmTimer;

  _Pending? _capturePending;
  _Pending? _monitorPending;

  /// The 5 s timeout's ERR text (bus error outranks it in the render), and
  /// the /state.ts it was raised against — any newer snapshot proves the
  /// bus is alive again and clears the complaint.
  String? _localErr;
  String? _localErrTs;

  @override
  void initState() {
    super.initState();
    // The store reference the timer callback needs (build uses context.watch;
    // no listener — _settle runs at the top of build instead).
    _store = context.read<BusStore>();
  }

  @override
  void dispose() {
    _confirmTimer?.cancel();
    super.dispose();
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
    if (_capturePending != null && ts != _capturePending!.tsAtTap) {
      _capturePending = null;
      changed = true;
    }
    if (_monitorPending != null && ts != _monitorPending!.tsAtTap) {
      _monitorPending = null;
      changed = true;
    }
    if (_localErr != null && ts != _localErrTs) {
      _localErr = null;
      _localErrTs = null;
      changed = true;
    }
    if (changed) setState(() {});
    return changed;
  }

  void _restartConfirmTimer() {
    _confirmTimer?.cancel();
    _confirmTimer = Timer(_confirmWindow, () {
      if (_capturePending != null) {
        _localErr = _noConfirmCapture;
        _localErrTs = _capturePending!.tsAtTap;
        _capturePending = null;
      }
      if (_monitorPending != null) {
        _localErr = _noConfirmMonitor;
        _localErrTs = _monitorPending!.tsAtTap;
        _monitorPending = null;
      }
      if (mounted) setState(() {});
    });
  }

  String? _stateTs(BusStore store) {
    final v = store.stateValue(UhfRadioPanel._address, 'ts');
    return v is String ? v : null;
  }

  void _sendToggle({required bool on, required _Pending? pending, required String noConfirmText, required String payload}) {
    final store = _store;
    if (store == null) return;
    // Record the pending against the ts we published under; the demand
    // flip or /state.error settles it (KTD15 — the readback is the truth).
    final p = _Pending(_stateTs(store));
    if (identical(pending, _capturePending)) {
      _capturePending = p;
      if (_localErr == _noConfirmCapture) {
        _localErr = null;
        _localErrTs = null;
      }
    } else {
      _monitorPending = p;
      if (_localErr == _noConfirmMonitor) {
        _localErr = null;
        _localErrTs = null;
      }
    }
    _restartConfirmTimer();
    context.read<MqttService>().publish(
          cmdTopic(UhfRadioPanel._slot),
          payload,
          retain: cmdRetain[UhfRadioPanel._address]!,
        );
    setState(() {});
  }

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    _settle(); // pending/timeout bookkeeping against the fresh snapshot

    const address = UhfRadioPanel._address;
    final slot = store.slots[address];
    // Bridge liveness ONLY — the audio demand is the session's connect
    // trigger; a healthy idle publishes device_online:false by design (R16).
    final bridgeUp = (slot?.bridgeOnline ?? false) && store.linkUp;

    final sessionState =
        store.stateValueAs<String>(address, 'session_state') ?? 'idle';
    final audioDemand =
        store.stateValueAs<bool>(address, 'audio_demand') ?? false;
    final monitor = store.stateValueAs<bool>(address, 'monitor') ?? false;
    final responding =
        store.stateValueAs<bool>(address, 'radio_responding') ?? false;
    final satellite = store.stateValueAs<bool>(address, 'satellite') ?? false;
    final freqHz = store.stateValueAs<num>(address, 'freq_hz')?.toInt();
    final band = store.stateValueAs<String>(address, 'band');
    final mode = store.stateValueAs<String>(address, 'mode');
    final sMeter = store.stateValueAs<int>(address, 's_meter');
    final swr = store.stateValueAs<int>(address, 'swr');
    final alc = store.stateValueAs<int>(address, 'alc');
    final txPower = store.stateValueAs<int>(address, 'tx_power');

    // Bus truth outranks the local timeout text (pol-ctrl ERR pattern).
    final busErr = slot?.state?['error'];
    final errText = busErr is String && busErr.isNotEmpty ? busErr : _localErr;

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'UHF RADIO',
            trailing: _headerTags(
              bridgeUp: bridgeUp,
              sessionState: sessionState,
              monitor: monitor,
              responding: responding,
              hasErr: errText != null,
            ),
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
          Row(
            children: [
              Expanded(
                child: ElevatedButton(
                  key: const ValueKey('uhf-capture-btn'),
                  onPressed: bridgeUp
                      ? () => _sendToggle(
                          on: !audioDemand,
                          pending: _capturePending,
                          noConfirmText: _noConfirmCapture,
                          payload: audioDemand
                              ? uhfRadioAudioOffPayload()
                              : uhfRadioAudioOnPayload())
                      : null,
                  style: AppTheme.actionButton(active: audioDemand),
                  child: Text(
                    audioDemand ? 'RADIO AUDIO: ON' : 'RADIO AUDIO: OFF',
                    style: AppTheme.mono(13, weight: FontWeight.w800),
                  ),
                ),
              ),
              const SizedBox(width: 8),
              Expanded(
                child: ElevatedButton(
                  key: const ValueKey('uhf-monitor-btn'),
                  onPressed: bridgeUp
                      ? () => _sendToggle(
                          on: !monitor,
                          pending: _monitorPending,
                          noConfirmText: _noConfirmMonitor,
                          payload: monitor
                              ? uhfRadioMonitorOffPayload()
                              : uhfRadioMonitorOnPayload())
                      : null,
                  style: AppTheme.actionButton(active: monitor),
                  child: Text(
                    monitor ? 'MONITOR: ON' : 'MONITOR: OFF',
                    style: AppTheme.mono(13, weight: FontWeight.w800),
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 10),
          _readout(
            monitor: monitor,
            responding: responding,
            freqHz: freqHz,
            band: band,
            mode: mode,
            sMeter: sMeter,
            swr: swr,
            alc: alc,
            txPower: txPower,
            satellite: satellite,
          ),
        ],
      ),
    );
  }

  /// Header tags: the capture-session state ONLY while the slot is
  /// reachable over /status (a dead bridge leaves a retained snapshot whose
  /// IDLE/LIVE must not read as current — OFFLINE wins), STANDBY while the
  /// monitor is on but the radio is deaf, ERR, OFFLINE. The audio/monitor
  /// demands are NOT repeated here — the toggle fills carry them — and SAT
  /// rides the readout's band line.
  Widget _headerTags({
    required bool bridgeUp,
    required String sessionState,
    required bool monitor,
    required bool responding,
    required bool hasErr,
  }) {
    final (tag, color) = switch (sessionState) {
      'live' => ('LIVE', AppTheme.green),
      'connecting' => ('CONNECTING', AppTheme.amber),
      'error' => ('ERROR', AppTheme.red),
      'idle' => ('IDLE', AppTheme.txtMute),
      _ => ('—', AppTheme.txtMute),
    };
    return Wrap(
      spacing: 4,
      runSpacing: 4,
      crossAxisAlignment: WrapCrossAlignment.center,
      children: [
        if (bridgeUp) StatusTag(label: tag, color: color),
        if (bridgeUp && monitor && !responding)
          StatusTag(label: 'STANDBY', color: AppTheme.amber),
        if (hasErr) StatusTag(label: 'ERR', color: AppTheme.red),
        if (!bridgeUp) StatusTag(label: 'OFFLINE', color: AppTheme.txtMute),
      ],
    );
  }

  /// The read-only readout (monitor-gated): freq/mode/meters from /state —
  /// omitted while the monitor is off or the radio is deaf, never zeroed,
  /// never frozen.
  Widget _readout({
    required bool monitor,
    required bool responding,
    required int? freqHz,
    required String? band,
    required String? mode,
    required int? sMeter,
    required int? swr,
    required int? alc,
    required int? txPower,
    required bool satellite,
  }) {
    final dashes = AppTheme.mono(20, weight: FontWeight.w600, color: AppTheme.txtMute);
    if (!monitor) {
      return Text('monitor off', style: AppTheme.mono(12, color: AppTheme.txtMute));
    }
    if (!responding) {
      return Text('radio standby / serial down',
          style: AppTheme.mono(12, color: AppTheme.txtMute));
    }
    final meters = <String>[
      if (sMeter != null) sUnits(sMeter),
      if (txPower != null) 'PWR $txPower',
      if (swr != null) 'SWR $swr',
      if (alc != null) 'ALC $alc',
    ];
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          freqHz != null ? _fmtMhz(freqHz) : '---.---',
          style: freqHz != null
              ? AppTheme.display(30, weight: FontWeight.w700, color: AppTheme.txt)
              : dashes,
        ),
        const SizedBox(height: 2),
        Text(
          [if (band != null && band.isNotEmpty) band.toUpperCase(), if (mode != null) mode.toUpperCase(), if (satellite) 'SAT']
              .join(' · '),
          style: AppTheme.mono(12, color: AppTheme.txtMute),
        ),
        if (meters.isNotEmpty) ...[
          const SizedBox(height: 4),
          Text(meters.join(' · '), style: AppTheme.mono(12, color: AppTheme.txt)),
        ],
      ],
    );
  }

  String _fmtMhz(int hz) {
    return '${(hz / 1000000.0).toStringAsFixed(3)} MHz';
  }
}

/// Icom CI-V S-meter reading (`15 02`, 0-255) as S-units: 0 = S0, 120 = S9
/// (linear, ~13.3 per S-unit), 241 = S9+60 dB (linear above S9). The raw
/// number on the bus is never shown — "S 42" read as a bogus S-unit.
@visibleForTesting
String sUnits(int raw) {
  final r = raw.clamp(0, 255);
  if (r <= 120) return 'S${(r * 9 / 120).round()}';
  final db = ((r - 120) * 60 / 121).round();
  // Icom meters step in 10 dB above S9; round to the nearest 10.
  final db10 = (db / 10).round() * 10;
  return db10 == 0 ? 'S9' : 'S9+$db10';
}
