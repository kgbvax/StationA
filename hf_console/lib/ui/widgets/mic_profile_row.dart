import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';

/// MicProfileRow is a row of three persisted mic-profile buttons for the TRX
/// card. Each button is bound (by name, on-device via SharedPreferences) to a
/// SmartSDR native mic profile:
///
/// - **Tap** a bound button → load that profile (`set_mic_profile`).
///   Tap an unbound button → pick an existing profile (from the radio's
///   available list), bind it to the button, and activate it.
/// - **Long-press** a bound button → Associate (bind a different profile) or
///   Unbind (clear the button's binding). Long-press an unbound button →
///   Associate only.
///
/// There is no Save: `profile mic save` is obsolete on SmartSDR v4+, so profiles
/// are created/edited in SmartSDR itself; the console only loads and binds.
///
/// The currently-active profile is shown two ways: the bound button whose name
/// equals the active profile is highlighted, and a `MIC: <name>` label renders
/// the active name even when it is not bound to any button (the active profile
/// may not be selected by or assigned to a button).
///
/// The available-profile list (`mic_profiles`) comes from `/state` — the bridge
/// queries it from the radio via the `profile mic info` command on connect, so
/// it populates shortly after the radio comes online. The active name
/// (`mic_profile`) is tracked by the bridge as "last loaded via
/// `set_mic_profile`" (SmartSDR reports no active mic profile), so it is empty
/// until the first load via the bus. Assignment is to existing profiles only
/// (binding to a name that isn't on the radio is pointless — the bridge drops
/// the load once the list is populated), so manual name entry is not offered.
class MicProfileRow extends StatefulWidget {
  static const String slot = 'hf/radio';
  static const String address = 'muehle/hf/radio';
  static const String cmdTopic = 'muehle/hf/radio/cmd';
  static const int buttonCount = 3;

  const MicProfileRow({super.key});

  @override
  State<MicProfileRow> createState() => _MicProfileRowState();
}

class _MicProfileRowState extends State<MicProfileRow> {
  /// Per-button bound profile name; null = unbound. Loaded from SharedPreferences.
  final List<String?> _names = List<String?>.filled(MicProfileRow.buttonCount, null);

  @override
  void initState() {
    super.initState();
    _loadBindings();
  }

  String _key(int i) => 'mic_profile_btn${i + 1}';

  Future<void> _loadBindings() async {
    final prefs = await SharedPreferences.getInstance();
    if (!mounted) return;
    setState(() {
      for (var i = 0; i < MicProfileRow.buttonCount; i++) {
        _names[i] = prefs.getString(_key(i));
      }
    });
  }

  Future<void> _bind(int i, String? name) async {
    final prefs = await SharedPreferences.getInstance();
    if (name == null || name.isEmpty) {
      await prefs.remove(_key(i));
    } else {
      await prefs.setString(_key(i), name);
    }
    if (!mounted) return;
    setState(() => _names[i] = (name == null || name.isEmpty) ? null : name);
  }

  void _publish(MqttService mqtt, String payload) =>
      mqtt.publish(MicProfileRow.cmdTopic, payload, retain: false);

  @override
  Widget build(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots[MicProfileRow.address];
        final online = slotState?.isOnline ?? false;
        final active =
            store.stateValueAs<String>(MicProfileRow.address, 'mic_profile') ?? '';
        final availableRaw = store.stateValue(MicProfileRow.address, 'mic_profiles');
        final available = <String>[];
        if (availableRaw is List) {
          for (final e in availableRaw) {
            available.add(e.toString());
          }
        }
        available.sort();

        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _buildActiveLabel(active),
            const SizedBox(height: 4),
            Row(
              children: [
                for (var i = 0; i < MicProfileRow.buttonCount; i++)
                  Expanded(
                    child: Padding(
                      padding: EdgeInsets.only(right: i < MicProfileRow.buttonCount - 1 ? 4 : 0),
                      child: _buildButton(context, i, online, active, available),
                    ),
                  ),
              ],
            ),
          ],
        );
      },
    );
  }

  Widget _buildActiveLabel(String active) {
    final has = active.isNotEmpty;
    return Row(
      children: [
        Text('MIC',
            style: AppTheme.mono(11, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
        const SizedBox(width: 6),
        Expanded(
          child: Text(
            has ? active : '—',
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.mono(11,
                weight: FontWeight.w600, color: has ? AppTheme.accent : AppTheme.txtFaint),
          ),
        ),
      ],
    );
  }

  Widget _buildButton(
      BuildContext context, int i, bool online, String active, List<String> available) {
    final name = _names[i];
    final bound = name != null && name.isNotEmpty;
    final isActive = bound && name == active;

    return ElevatedButton(
      onPressed: online ? () => _onTap(i, online, available) : null,
      onLongPress: online ? () => _onLongPress(i, available) : null,
      style: AppTheme.actionButton(active: isActive).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(0, 44)),
        padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 6, vertical: 6)),
      ),
      child: FittedBox(
        fit: BoxFit.scaleDown,
        child: Text(
          bound ? name : '—',
          maxLines: 1,
          style: AppTheme.mono(12, weight: FontWeight.w600),
        ),
      ),
    );
  }

  // --- interactions -------------------------------------------------------

  void _onTap(int i, bool online, List<String> available) {
    final mqtt = context.read<MqttService>();
    final name = _names[i];
    if (name != null && name.isNotEmpty) {
      _publish(mqtt, radioSetMicProfilePayload(name));
      return;
    }
    // Unbound: pick an EXISTING profile, bind it to this button, and activate it.
    _pickExistingProfile(available, title: 'Bind mic profile to button ${i + 1}')
        .then((chosen) {
      if (chosen == null || chosen.isEmpty) return;
      _bind(i, chosen);
      _publish(mqtt, radioSetMicProfilePayload(chosen));
    });
  }

  void _onLongPress(int i, List<String> available) {
    final bound = _names[i] != null && _names[i]!.isNotEmpty;
    showDialog<void>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text('Button ${i + 1}', style: AppTheme.mono(14, weight: FontWeight.w700)),
        content: Text(bound ? 'Bound to “${_names[i]}”' : 'Unbound'),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(),
            child: const Text('Cancel'),
          ),
          if (bound)
            ElevatedButton(
              style: AppTheme.actionButton(danger: true),
              onPressed: () {
                Navigator.of(ctx).pop();
                _bind(i, null); // Unbind: clear the button's association.
              },
              child: const Text('Unbind'),
            ),
          ElevatedButton(
            style: AppTheme.actionButton(active: true),
            onPressed: () {
              Navigator.of(ctx).pop();
              _associate(i, available);
            },
            child: const Text('Associate'),
          ),
        ],
      ),
    );
  }

  /// Associate: bind an EXISTING profile to the button without activating it.
  void _associate(int i, List<String> available) {
    _pickExistingProfile(available, title: 'Associate mic profile').then((chosen) {
      if (chosen == null || chosen.isEmpty) return;
      _bind(i, chosen);
    });
  }

  /// Pick an EXISTING mic profile from the radio's available list. Used for
  /// assignment (bind/activate) — binding a button to a profile that doesn't
  /// exist on the radio is pointless (the bridge drops the load once the list
  /// is populated), so manual entry is not offered here. If no profiles have
  /// been reported yet, shows an informational message and returns null.
  Future<String?> _pickExistingProfile(List<String> available, {required String title}) {
    if (available.isEmpty) {
      return showDialog<String>(
        context: context,
        builder: (ctx) => AlertDialog(
          title: Text(title, style: AppTheme.mono(14, weight: FontWeight.w700)),
          content: const Text(
              'No mic profiles are available yet.\n\n'
              'The bridge queries the radio\'s profile list when it connects, '
              'so this should populate shortly after the radio comes online. '
              'If it stays empty, make sure the radio is online.\n\n'
              'Mic profiles are created and edited in SmartSDR itself.'),
          actions: [
            ElevatedButton(
              style: AppTheme.actionButton(active: true),
              onPressed: () => Navigator.of(ctx).pop(),
              child: const Text('OK'),
            ),
          ],
        ),
      ).then((_) => null);
    }
    return showDialog<String>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text(title, style: AppTheme.mono(14, weight: FontWeight.w700)),
        content: SizedBox(
          width: double.maxFinite,
          child: ListView(
            shrinkWrap: true,
            children: [
              for (final name in available)
                Padding(
                  padding: const EdgeInsets.only(bottom: 4),
                  child: ElevatedButton(
                    onPressed: () => Navigator.of(ctx).pop(name),
                    style: AppTheme.actionButton().copyWith(
                      minimumSize: const WidgetStatePropertyAll(Size.fromHeight(40)),
                    ),
                    child: Text(name, style: AppTheme.mono(12), maxLines: 1, overflow: TextOverflow.ellipsis),
                  ),
                ),
            ],
          ),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(),
            child: const Text('Cancel'),
          ),
        ],
      ),
    );
  }
}