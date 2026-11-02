// dvk_auto_repeat.dart — the DVK auto-repeat loop behind a held TRX button.
//
// One DVK memory is played, the console waits N s after the message ends, and
// plays it again, until the operator or the radio says stop. The loop lives
// here, not in DvkPanel, so a layout switch (phone <-> tablet rebuilds the
// panel) does not kill a running loop. Everything is client-side: it reads
// the radio's retained /state off the [BusStore] and publishes `dvk_play_<n>`
// / `dvk_stop` on the radio cmd topic.
//
// This keys a transmitter without anyone watching, so every automatic play is
// preceded by a fresh look at the bus (link up, radio online, no carrier, DVK
// idle) and any doubt resolves to [DvkAutoPhase.paused] — armed but silent.
// The loop never resumes by itself after a pause.

import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/foundation.dart';

import '../mqtt/mqtt_service.dart';
import 'bus_store.dart';
import 'wiring.dart';

enum DvkAutoPhase {
  /// Play sent, waiting for the radio to report this memory in playback.
  starting,

  /// This loop's own message is on air; TX is expected and not a reason to stop.
  playing,

  /// Message over; counting down to the next play.
  countdown,

  /// Armed but idle: stopped by a foreign TX, a tap, a timeout or a lost link.
  paused,
}

class DvkAutoRepeat extends ChangeNotifier {
  static const String _radio = 'muehle/hf/radio';
  static const String _cmdTopic = 'muehle/hf/radio/cmd';

  /// How long a sent play may take to show up as `dvk_status == playback`.
  /// Past it the radio ignored the command (interlock, DVK disabled, PTT
  /// held): pause instead of retrying a transmitter blind.
  static const Duration startTimeout = Duration(seconds: 3);

  final BusStore _store;
  final MqttService _mqtt;

  int? _button;
  DvkAutoPhase? _phase;
  int _memory = 1;
  int _repeatS = 3;

  /// Memory of the play currently in flight. Kept apart from [_memory] so a
  /// settings change mid-message cannot make the running playback look foreign.
  int _playId = 1;
  int _remaining = 0;

  /// Carrier state at the previous bus update, for rising-edge detection.
  bool _lastRf = false;

  /// A bus loss (link or radio slot down) paused the loop while its own play
  /// was starting or on air. The phase says nothing about the message any more,
  /// so this remembers that [_playId] may still be on the air until a stop is
  /// sent or a new play replaces it. Checked against the live bus at use.
  bool _ownPlayLost = false;

  Timer? _startTimer;
  Timer? _tickTimer;

  DvkAutoRepeat(this._store, this._mqtt) {
    _lastRf = _rfKeyed;
    _store.addListener(_onStore);
  }

  /// 1-based DVK button index in auto mode, null when none is.
  int? get button => _button;

  /// Null when no button is in auto mode.
  DvkAutoPhase? get phase => _phase;

  /// Seconds left in the countdown; 0 outside [DvkAutoPhase.countdown].
  int get remaining => _phase == DvkAutoPhase.countdown ? _remaining : 0;

  bool isAuto(int button) => _button != null && _button == button;

  // --- Bus reads -------------------------------------------------------------

  /// Link up and radio slot online. With either down the retained snapshot is
  /// stale and a command would be dropped or land late.
  bool get _available => _store.linkUp && (_store.slots[_radio]?.isOnline ?? false);

  /// Any carrier: the radio's tx bit, the PA keyed, or an ATU tune carrier.
  /// A tune carrier is as foreign to the loop as an operator's PTT.
  bool get _rfKeyed => _store.rfState != RfState.rx;

  String get _dvkStatus => _store.stateValueAs<String>(_radio, 'dvk_status') ?? 'idle';

  int get _dvkId => _store.stateValueAs<num>(_radio, 'dvk_id')?.toInt() ?? 0;

  /// The DVK is in use by someone: a message on air, or the operator recording
  /// or previewing a memory.
  bool get _dvkBusy => const {'playback', 'recording', 'preview'}.contains(_dvkStatus);

  // --- Public API ------------------------------------------------------------

  /// Puts [button] into auto mode and plays [memory] now. Any other button's
  /// loop is exited first (one active button). When the bus does not allow a
  /// play right now the button is armed in [DvkAutoPhase.paused] instead.
  void enter(int button, int memory, int repeatS) {
    if (_button == button) {
      // Already armed: take the new settings and, if idle, restart.
      updateConfig(button, memory, repeatS);
      resume();
      return;
    }
    exit();
    _button = button;
    _memory = _clampMemory(memory);
    _repeatS = _clampRepeat(repeatS);
    _play();
  }

  /// Leaves auto mode entirely. A playback this loop started (or is about to
  /// start) is stopped; a foreign one is left alone.
  void exit() {
    if (_phase == null) return;
    if (_ownPlayMayBeOnAir) _publishStop();
    _cancelTimers();
    _button = null;
    _phase = null;
    _remaining = 0;
    _ownPlayLost = false;
    notifyListeners();
  }

  /// Stops looping but stays armed. With [stopPlayback] the loop's own
  /// message in flight is cut off with `dvk_stop`.
  void pause({bool stopPlayback = false}) {
    if (_phase == null) return;
    if (stopPlayback && _ownPlayMayBeOnAir) _publishStop();
    _toPaused();
  }

  /// Plays now from [DvkAutoPhase.paused]. Stays paused when the bus does not
  /// allow a play (carrier up, DVK busy, radio or link down).
  void resume() {
    if (_phase != DvkAutoPhase.paused) return;
    _play();
  }

  /// New memory / interval for the armed [button] (settings dialog Save).
  /// Takes effect from the next cycle: the memory at the next play, the
  /// interval at the next countdown. A running message or countdown is left
  /// alone.
  void updateConfig(int button, int memory, int repeatS) {
    if (_button != button) return;
    final m = _clampMemory(memory);
    final r = _clampRepeat(repeatS);
    if (m == _memory && r == _repeatS) return;
    _memory = m;
    _repeatS = r;
    notifyListeners();
  }

  @override
  void dispose() {
    _store.removeListener(_onStore);
    _cancelTimers();
    super.dispose();
  }

  // --- Internals -------------------------------------------------------------

  /// A play we sent may be on air: confirmed (playing) or still in flight
  /// (starting, the radio may yet act on it).
  bool get _ownPlaybackPossible =>
      _phase == DvkAutoPhase.starting || _phase == DvkAutoPhase.playing;

  /// [_ownPlaybackPossible], or a play a bus loss paused us under that the
  /// radio now reports on air again. Read live: in `starting` the radio still
  /// says idle, so only the report of our memory in playback confirms it.
  bool get _ownPlayMayBeOnAir =>
      _ownPlaybackPossible ||
      (_ownPlayLost && _dvkStatus == 'playback' && _dvkId == _playId);

  // DvkPlayPayload clamps to 1..12; clamp here too so the dvk_id comparison
  // sees the memory that was actually requested.
  static int _clampMemory(int m) => m.clamp(1, 12);

  static int _clampRepeat(int s) => math.max(1, s);

  // The memory goes along on purpose: an id-less stop is resolved from the
  // bridge's /state.dvk_id, which stays 0 until the radio's playback frame has
  // arrived, so while the play is still `starting` the bridge would drop it
  // and the message would run on with nobody tracking it. An explicit id is
  // sent to the radio whatever the bridge has seen so far, queued behind the
  // play.
  void _publishStop() {
    _ownPlayLost = false;
    _mqtt.publish(_cmdTopic, dvkStopPayload(_playId), retain: false);
  }

  void _cancelTimers() {
    _startTimer?.cancel();
    _startTimer = null;
    _tickTimer?.cancel();
    _tickTimer = null;
  }

  void _toPaused() {
    _cancelTimers();
    _phase = DvkAutoPhase.paused;
    _remaining = 0;
    notifyListeners();
  }

  /// Sends the play for [_memory] unless the bus says someone else owns the
  /// air or the DVK. Re-checked on every call, so the state a countdown
  /// started under never authorises the play at its end.
  void _play() {
    if (!_available || _rfKeyed || _dvkBusy) {
      _toPaused();
      return;
    }
    final id = _memory;
    if (!_mqtt.publish(_cmdTopic, dvkPlayPayload(id), retain: false)) {
      _toPaused();
      return;
    }
    _cancelTimers();
    _playId = id;
    _ownPlayLost = false;
    _phase = DvkAutoPhase.starting;
    _remaining = 0;
    _startTimer = Timer(startTimeout, _onStartTimeout);
    notifyListeners();
  }

  void _onStartTimeout() {
    _startTimer = null;
    if (_phase == DvkAutoPhase.starting) _toPaused();
  }

  void _beginCountdown() {
    _cancelTimers();
    _phase = DvkAutoPhase.countdown;
    _remaining = _repeatS;
    _tickTimer = Timer.periodic(const Duration(seconds: 1), (_) => _tick());
    notifyListeners();
  }

  void _tick() {
    if (_phase != DvkAutoPhase.countdown) return;
    // Carrier still up at a tick is the TX-tail grace expiring (the first tick
    // is 1 s after the message ended) or a TX that began between updates.
    if (!_available || _rfKeyed || _dvkBusy) {
      _toPaused();
      return;
    }
    _remaining--;
    if (_remaining > 0) {
      notifyListeners();
    } else {
      _play();
    }
  }

  void _onStore() {
    final rf = _rfKeyed;
    final risingRf = rf && !_lastRf;
    _lastRf = rf;

    final phase = _phase;
    if (phase == null || phase == DvkAutoPhase.paused) return;

    if (!_available) {
      // The phase is about to stop saying whether our message may be on air.
      if (_ownPlaybackPossible) _ownPlayLost = true;
      _toPaused();
      return;
    }

    final playback = _dvkStatus == 'playback';
    switch (phase) {
      case DvkAutoPhase.starting:
        // TX alone proves nothing here: our own playback keys the radio, and
        // its tx bit can land before dvk_status does. A foreign key-up that
        // keeps the play from starting ends in the start timeout.
        if (playback) {
          if (_dvkId == _playId) {
            _cancelTimers();
            _phase = DvkAutoPhase.playing;
            notifyListeners();
          } else {
            _toPaused(); // another memory beat us to it
          }
        }
      case DvkAutoPhase.playing:
        if (!playback) {
          _beginCountdown();
        } else if (_dvkId != _playId) {
          _toPaused(); // operator started another memory over ours
        }
      case DvkAutoPhase.countdown:
        // A rising edge is a key-up that is not ours (we are between plays).
        // A carrier already up when the countdown began is the TX-tail grace,
        // judged at the first tick instead.
        if (risingRf || _dvkBusy) _toPaused();
      case DvkAutoPhase.paused:
        break;
    }
  }
}
