import 'package:flutter/gestures.dart';
import 'package:flutter/material.dart';
import 'package:flutter/semantics.dart' show CustomSemanticsAction;
import 'package:provider/provider.dart';
import 'package:shared_preferences/shared_preferences.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/dvk_auto_repeat.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'mic_profile_row.dart';
import 'status_pill.dart';

/// The TRX card: band row, mic profiles and the DVK keys.
///
/// Each DVK key is bound to a memory (default: its own number) and an
/// auto-repeat interval, both persisted. Gestures on a key:
///
/// - **Tap** plays the memory. In auto mode it pauses the loop (and cuts the
///   loop's own message), or resumes a paused one.
/// - **Hold 0.7 s** enters auto mode and plays now; in auto mode it leaves it.
/// - **Double tap** opens the register / interval menu.
///
/// Recognising a double tap delays a plain tap by Flutter's double-tap window
/// (~300 ms). Firing on the first tap instead would transmit on every double
/// tap, so the delay is the right trade. The loop itself is [DvkAutoRepeat];
/// it lives above the panel so a layout switch does not kill it.
class DvkPanel extends StatefulWidget {
  static const String slot = 'hf/radio';
  static const String topic = 'muehle/hf/radio/cmd';

  /// Play buttons for DVK memories 1..buttonCount (the radio has 12).
  static const int buttonCount = 2;

  /// Memories the radio has, and the interval range the menu offers (s).
  static const int memoryCount = 12;
  static const int minRepeatS = 1;
  static const int maxRepeatS = 30;
  static const int defaultRepeatS = 3;

  /// Hold time that enters / leaves auto mode.
  static const Duration holdDuration = Duration(milliseconds: 700);

  const DvkPanel({super.key});

  @override
  State<DvkPanel> createState() => _DvkPanelState();

  /// DVK memory names as set in SmartSDR, keyed by memory id, from the
  /// bridge's `/state.dvk_memories` list (`[{id, name, duration_ms}]`).
  /// Malformed entries and blank names are skipped so the button falls back
  /// to `DVK<n>`.
  static Map<int, String> memoryNames(dynamic raw) {
    final out = <int, String>{};
    if (raw is! List) return out;
    for (final e in raw) {
      if (e is! Map) continue;
      final id = e['id'];
      final name = e['name'];
      if (id is int && name is String && name.trim().isNotEmpty) out[id] = name.trim();
    }
    return out;
  }
}

class _DvkPanelState extends State<DvkPanel> {
  static const String slot = DvkPanel.slot;
  static const String topic = DvkPanel.topic;
  static const int buttonCount = DvkPanel.buttonCount;

  /// Per-button bound memory and repeat interval, index = button - 1. Loaded
  /// from SharedPreferences (same pattern as MicProfileRow).
  final List<int> _memory = [for (var n = 1; n <= buttonCount; n++) n];
  final List<int> _repeatS = List<int>.filled(buttonCount, DvkPanel.defaultRepeatS);

  /// Until the stored bindings are read a gesture would act on the defaults
  /// (the wrong memory on a transmitter), so the keys ignore gestures.
  bool _loaded = false;

  /// Pressed look for the buttons: they sit under an IgnorePointer (the
  /// gesture recognizers own the pointer), so the states are driven by hand.
  late final List<WidgetStatesController> _pressed =
      List.generate(buttonCount, (_) => WidgetStatesController());

  @override
  void initState() {
    super.initState();
    _loadBindings();
  }

  @override
  void dispose() {
    for (final c in _pressed) {
      c.dispose();
    }
    super.dispose();
  }

  String _memoryKey(int n) => 'dvk_btn${n}_memory';
  String _repeatKey(int n) => 'dvk_btn${n}_repeat_s';

  Future<void> _loadBindings() async {
    final prefs = await SharedPreferences.getInstance();
    if (!mounted) return;
    setState(() {
      for (var n = 1; n <= buttonCount; n++) {
        final m = prefs.getInt(_memoryKey(n));
        final r = prefs.getInt(_repeatKey(n));
        if (m != null && m >= 1 && m <= DvkPanel.memoryCount) _memory[n - 1] = m;
        if (r != null && r >= DvkPanel.minRepeatS && r <= DvkPanel.maxRepeatS) _repeatS[n - 1] = r;
      }
      _loaded = true;
    });
  }

  Future<void> _persist(int n, int memory, int repeatS) async {
    final prefs = await SharedPreferences.getInstance();
    await prefs.setInt(_memoryKey(n), memory);
    await prefs.setInt(_repeatKey(n), repeatS);
  }

  @override
  Widget build(BuildContext context) {
    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border(top: BorderSide(color: AppTheme.cardLine), bottom: BorderSide(color: AppTheme.cardLine)),
      ),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              Text('TRX',
                  style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
              // A live transmit is the one TRX state that must surface even
              // with the readout pill gone — the DVK buttons don't show it.
              Consumer<BusStore>(builder: (context, store, _) {
                final tx = store.stateValueAs<String>('muehle/$slot', 'tx');
                return StatusPill(
                  slots: const ['muehle/hf/radio'],
                  label: 'FLEX-8400',
                  suffix: tx == 'tx' ? 'TX' : null,
                  suffixColor: AppTheme.red,
                );
              }),
            ],
          ),
          const SizedBox(height: 8),
          _buildBandRow(context),
          const SizedBox(height: 8),
          const MicProfileRow(),
          const SizedBox(height: 8),
          _buildButtonRow(context),
        ],
      ),
    );
  }

  Widget _buildBandRow(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = (slotState?.isOnline ?? false) && store.linkUp;
        final currentBand = store.stateValueAs<String>('muehle/$slot', 'band') ?? '';
        final mode = store.stateValueAs<String>('muehle/$slot', 'mode');
        final freqHz = store.stateValueAs<num>('muehle/$slot', 'freq_hz');
        final mqtt = context.read<MqttService>();

        // Tapping the band the radio is already on toggles CW <-> SSB
        // (sideband by frequency, toggledSsbCwMode) instead of re-stacking.
        void onBand(String band) {
          final payload = band == currentBand
              ? radioSetModePayload(toggledSsbCwMode(mode, freqHz))
              : radioSetBandPayload(band);
          mqtt.publish(topic, payload, retain: false);
        }

        const bands = ['80', '40', '20', '17', '15', '12', '10'];
        // One row of equal-width buttons (a Wrap split 5 + 2 on the tablet).
        // Each carries a short underline in its horstreporter band colour
        // (AppTheme.bandColor, fixed across themes) so a band button and the
        // spot dots of that band on the map read as the same thing.
        return Row(
          children: [
            for (final (i, band) in bands.indexed)
              Expanded(
                child: Padding(
                  padding: EdgeInsets.only(right: i < bands.length - 1 ? 6 : 0),
                  child: _bandButton(
                    '${band}m',
                    active: currentBand == '${band}m',
                    onPressed: online ? () => onBand('${band}m') : null,
                  ),
                ),
              ),
          ],
        );
      },
    );
  }

  Widget _bandButton(String band, {required bool active, VoidCallback? onPressed}) {
    final stripe = AppTheme.bandColor(band);
    return ElevatedButton(
      onPressed: onPressed,
      style: AppTheme.actionButton(active: active).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(0, 48)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 4, vertical: 6)),
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          FittedBox(fit: BoxFit.scaleDown, child: Text(band, maxLines: 1)),
          const SizedBox(height: 4),
          // Full button width, 6 px tall: a short 3 px dash was too small to
          // tell the band colours apart at arm's length on the tablet.
          Container(
            key: ValueKey('band-stripe-$band'),
            width: double.infinity,
            height: 6,
            decoration: BoxDecoration(
              color: onPressed == null ? AppTheme.blend(stripe, 0.35) : stripe,
              borderRadius: BorderRadius.circular(3),
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildButtonRow(BuildContext context) {
    return Consumer2<BusStore, DvkAutoRepeat>(
      builder: (context, store, auto, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = (slotState?.isOnline ?? false) && store.linkUp;
        final status = store.stateValueAs<String>('muehle/$slot', 'dvk_status') ?? 'idle';
        final activeId = store.stateValueAs<int>('muehle/$slot', 'dvk_id') ?? 0;
        final isPlaying = status == 'playback';
        final names = DvkPanel.memoryNames(store.stateValue('muehle/$slot', 'dvk_memories'));

        return Row(
          children: [
            for (var n = 1; n <= buttonCount; n++)
              Expanded(
                child: Padding(
                  padding: const EdgeInsets.only(right: 8),
                  child: _dvkButton(n, online, auto, names, active: online && isPlaying && activeId == _memory[n - 1]),
                ),
              ),
            ElevatedButton(
              onPressed: online ? _onStop : null,
              style: AppTheme.actionButton(danger: true).copyWith(
                minimumSize: const WidgetStatePropertyAll(Size(64, 48)),
                padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 12, vertical: 6)),
              ),
              child: const Text('STOP'),
            ),
          ],
        );
      },
    );
  }

  /// Button [n]'s label: the memory's name, plus the loop's state while it is
  /// in auto mode: the seconds left (`[3]`), or `[–]` when it is armed but
  /// idle. Playing and starting show the name only (the message is on air).
  String _label(int n, DvkAutoRepeat auto, Map<int, String> names) {
    final m = _memory[n - 1];
    final name = names[m] ?? 'DVK$m';
    if (!auto.isAuto(n)) return name;
    return switch (auto.phase) {
      DvkAutoPhase.countdown => '$name [${auto.remaining}]',
      DvkAutoPhase.paused => '$name [–]',
      _ => name,
    };
  }

  Widget _dvkButton(int n, bool online, DvkAutoRepeat auto, Map<int, String> names, {required bool active}) {
    final isAuto = auto.isAuto(n);
    final label = _label(n, auto, names);
    final controller = _pressed[n - 1];
    // Tap / hold act on the radio and need it; leaving auto mode and the
    // settings menu do not, so a stuck loop can always be disarmed.
    final canPlay = online && _loaded;
    final canHold = (online && _loaded) || isAuto;

    return Semantics(
      container: true,
      button: true,
      enabled: canPlay,
      label: isAuto ? '$label, auto repeat every ${_repeatS[n - 1]} s' : label,
      onTap: canPlay ? () => _onTap(n) : null,
      onLongPress: canHold ? () => _onLongPress(n) : null,
      customSemanticsActions: {
        const CustomSemanticsAction(label: 'DVK button settings'): () => _openSettings(n),
      },
      child: RawGestureDetector(
        key: ValueKey('dvk-btn-$n'),
        behavior: HitTestBehavior.opaque,
        gestures: {
          if (canPlay)
            TapGestureRecognizer: GestureRecognizerFactoryWithHandlers<TapGestureRecognizer>(
              TapGestureRecognizer.new,
              (t) => t
                ..onTapDown = ((_) => controller.update(WidgetState.pressed, true))
                ..onTapUp = ((_) => controller.update(WidgetState.pressed, false))
                ..onTapCancel = (() => controller.update(WidgetState.pressed, false))
                ..onTap = (() => _onTap(n)),
            ),
          DoubleTapGestureRecognizer: GestureRecognizerFactoryWithHandlers<DoubleTapGestureRecognizer>(
            DoubleTapGestureRecognizer.new,
            (t) => t.onDoubleTap = () => _openSettings(n),
          ),
          if (canHold)
            LongPressGestureRecognizer: GestureRecognizerFactoryWithHandlers<LongPressGestureRecognizer>(
              () => LongPressGestureRecognizer(duration: DvkPanel.holdDuration),
              (t) => t.onLongPress = () => _onLongPress(n),
            ),
        },
        // The recognizers own the pointer; the button is the picture. A no-op
        // onPressed keeps the enabled look while the radio is reachable.
        child: ExcludeSemantics(
          child: IgnorePointer(
            child: ElevatedButton(
              statesController: controller,
              onPressed: online ? () {} : null,
              style: AppTheme.actionButton(active: active).copyWith(
                minimumSize: const WidgetStatePropertyAll(Size(0, 48)),
                padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 6)),
                // Armed (running or paused) is visible at a glance.
                side: isAuto ? WidgetStatePropertyAll(BorderSide(color: AppTheme.accent, width: 2.6)) : null,
              ),
              // Radio names can be long ("dpidp dl9et"): shrink to
              // fit rather than wrap or clip, like the band buttons.
              child: FittedBox(
                fit: BoxFit.scaleDown,
                child: Text(label, maxLines: 1),
              ),
            ),
          ),
        ),
      ),
    );
  }

  // --- interactions -------------------------------------------------------

  void _onTap(int n) {
    final auto = context.read<DvkAutoRepeat>();
    if (!auto.isAuto(n)) {
      context.read<MqttService>().publish(topic, dvkPlayPayload(_memory[n - 1]), retain: false);
    } else if (auto.phase == DvkAutoPhase.paused) {
      auto.resume();
    } else {
      // Running: pause, and cut the loop's own message if one is on air.
      auto.pause(stopPlayback: true);
    }
  }

  void _onLongPress(int n) {
    final auto = context.read<DvkAutoRepeat>();
    if (auto.isAuto(n)) {
      auto.exit();
    } else {
      auto.enter(n, _memory[n - 1], _repeatS[n - 1]);
    }
  }

  /// STOP: the radio's DVK, whoever started it, and any loop. The loop's own
  /// stop names its memory, which still reaches the radio before the bridge
  /// has seen the playback; the plain one cuts a message started by hand.
  void _onStop() {
    final mqtt = context.read<MqttService>();
    context.read<DvkAutoRepeat>().pause(stopPlayback: true);
    mqtt.publish(topic, dvkStopPayload(), retain: false);
  }

  Future<void> _openSettings(int n) async {
    final store = context.read<BusStore>();
    final names = DvkPanel.memoryNames(store.stateValue('muehle/$slot', 'dvk_memories'));
    final chosen = await showDialog<(int, int)>(
      context: context,
      builder: (ctx) => _DvkSettingsDialog(
        button: n,
        memory: _memory[n - 1],
        repeatS: _repeatS[n - 1],
        names: names,
      ),
    );
    if (chosen == null || !mounted) return;
    final (memory, repeatS) = chosen;
    setState(() {
      _memory[n - 1] = memory;
      _repeatS[n - 1] = repeatS;
    });
    // A button in auto mode picks the new values up at its next cycle.
    context.read<DvkAutoRepeat>().updateConfig(n, memory, repeatS);
    await _persist(n, memory, repeatS);
  }
}

/// Double-tap menu of a DVK key: which of the radio's 12 memories it plays and
/// how long the auto-repeat waits after the message. Pops `(memory, repeatS)`
/// on Save, null on Cancel.
class _DvkSettingsDialog extends StatefulWidget {
  final int button;
  final int memory;
  final int repeatS;
  final Map<int, String> names;

  const _DvkSettingsDialog({
    required this.button,
    required this.memory,
    required this.repeatS,
    required this.names,
  });

  @override
  State<_DvkSettingsDialog> createState() => _DvkSettingsDialogState();
}

class _DvkSettingsDialogState extends State<_DvkSettingsDialog> {
  late int _memory = widget.memory;
  late int _repeatS = widget.repeatS;

  void _step(int d) =>
      setState(() => _repeatS = (_repeatS + d).clamp(DvkPanel.minRepeatS, DvkPanel.maxRepeatS));

  @override
  Widget build(BuildContext context) {
    final stepStyle = AppTheme.actionButton().copyWith(
      minimumSize: const WidgetStatePropertyAll(Size(48, 48)),
      padding: const WidgetStatePropertyAll(EdgeInsets.zero),
    );
    return AlertDialog(
      title: Text('DVK button ${widget.button}', style: AppTheme.mono(14, weight: FontWeight.w700)),
      content: SizedBox(
        width: double.maxFinite,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Text('REPEAT AFTER MESSAGE',
                style: AppTheme.mono(11, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
            const SizedBox(height: 4),
            Row(
              mainAxisAlignment: MainAxisAlignment.center,
              children: [
                ElevatedButton(
                  key: const ValueKey('dvk-repeat-minus'),
                  onPressed: _repeatS > DvkPanel.minRepeatS ? () => _step(-1) : null,
                  style: stepStyle,
                  child: const Text('–'),
                ),
                SizedBox(
                  width: 72,
                  child: Text('$_repeatS s',
                      textAlign: TextAlign.center, style: AppTheme.mono(14, weight: FontWeight.w700)),
                ),
                ElevatedButton(
                  key: const ValueKey('dvk-repeat-plus'),
                  onPressed: _repeatS < DvkPanel.maxRepeatS ? () => _step(1) : null,
                  style: stepStyle,
                  child: const Text('+'),
                ),
              ],
            ),
            const SizedBox(height: 12),
            Text('REGISTER',
                style: AppTheme.mono(11, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
            const SizedBox(height: 4),
            Flexible(
              child: ListView(
                shrinkWrap: true,
                children: [
                  for (var id = 1; id <= DvkPanel.memoryCount; id++)
                    Padding(
                      padding: const EdgeInsets.only(bottom: 4),
                      child: ElevatedButton(
                        key: ValueKey('dvk-register-$id'),
                        onPressed: () => setState(() => _memory = id),
                        style: AppTheme.actionButton(active: id == _memory).copyWith(
                          minimumSize: const WidgetStatePropertyAll(Size.fromHeight(48)),
                        ),
                        child: Text(widget.names[id] ?? 'DVK$id',
                            style: AppTheme.mono(12), maxLines: 1, overflow: TextOverflow.ellipsis),
                      ),
                    ),
                ],
              ),
            ),
          ],
        ),
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('Cancel'),
        ),
        ElevatedButton(
          style: AppTheme.actionButton(active: true),
          onPressed: () => Navigator.of(context).pop((_memory, _repeatS)),
          child: const Text('Save'),
        ),
      ],
    );
  }
}
