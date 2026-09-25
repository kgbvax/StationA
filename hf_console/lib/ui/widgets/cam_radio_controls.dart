// cam_radio_controls.dart — IC-9700 audio-chain status + commands, mirrored
// from vhfcam-restream's :8083 reference page.
//
// The LEDs read the preview server's /api/radio-status (polled by
// [VhfcamService]); the buttons POST /api/cmd/{action}, which the server
// translates to muehle/uhf/radio/cmd publishes. This is the full :8083
// control surface, operator decision 2026-09-24: audio_on deliberately GRABS
// the IC-9700's CI-V session (60 s TTL heartbeat while the demand lasts) —
// wfview at the desk yields while it is held, and audio_off (or 60 s of
// silence) gives it back. power_on sends power_on + audio_on back-to-back,
// exactly like the reference page. The console never publishes the radio
// slot's /cmd topic directly; the bus command plane stays the server's
// business.
//
// Mute is NOT here: it is local to the feed player (cam_feed_panel.dart).

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../vhfcam/vhfcam_service.dart';
import '../theme.dart';
import 'card_container.dart';

class CamRadioControls extends StatefulWidget {
  const CamRadioControls({super.key});

  @override
  State<CamRadioControls> createState() => _CamRadioControlsState();
}

class _CamRadioControlsState extends State<CamRadioControls> {
  /// Action currently in flight — disables all three buttons so a slow POST
  /// can't queue a competing command (the server serializes them anyway, but
  /// a dead-button read is honest feedback).
  String? _busy;

  Future<void> _send(String action) async {
    if (_busy != null) return;
    setState(() => _busy = action);
    final ok = await context.read<VhfcamService>().sendCommand(action);
    if (!mounted) return;
    setState(() => _busy = null);
    if (!ok) {
      // The LED row already shows the state the server is in; the failure
      // snackbar just closes the loop on the tap. No dialog — the cam is an
      // accessory, not an interlock.
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('CAM SERVER DID NOT ACCEPT $action', style: AppTheme.mono(12)), duration: const Duration(seconds: 2)),
      );
    }
  }

  @override
  Widget build(BuildContext context) {
    final svc = context.watch<VhfcamService>();
    final status = svc.status;
    final enabled = svc.serverOnline && _busy == null;

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          CardHeader(
            title: 'RADIO AUDIO',
            trailing: _ServerTag(online: svc.serverOnline),
          ),
          const SizedBox(height: 10),
          Row(
            children: [
              Expanded(child: _Led(label: 'BRIDGE', state: status?.leds['bridge'] ?? 'unk', faultWhenOff: true)),
              Expanded(child: _Led(label: 'SESSION', state: status?.leds['session'] ?? 'unk', faultWhenOff: false)),
              Expanded(child: _Led(label: 'RADIO', state: status?.leds['radio'] ?? 'unk', faultWhenOff: true)),
              Expanded(child: _Led(label: 'AUDIO', state: status?.leds['audio'] ?? 'unk', faultWhenOff: false)),
            ],
          ),
          if (status != null && status.hint.isNotEmpty) ...[
            const SizedBox(height: 8),
            Text(status.hint, style: AppTheme.mono(10, color: AppTheme.txtFaint), maxLines: 2, overflow: TextOverflow.ellipsis),
          ],
          const SizedBox(height: 12),
          // ON/OFF share a row: they are one switch the server exposes as two
          // commands (other holders — a recording — can keep audio on after
          // OFF, so this is not drawn as a single toggle). POWER ON is the
          // occasional cold-start action and sits apart below.
          Row(
            children: [
              Expanded(
                child: _ControlButton(
                  label: 'RADIO AUDIO ON',
                  active: status?.audioStream ?? false,
                  onPressed: enabled ? () => _send('audio_on') : null,
                ),
              ),
              const SizedBox(width: 8),
              Expanded(
                child: _ControlButton(
                  label: 'RADIO AUDIO OFF',
                  active: false,
                  onPressed: enabled ? () => _send('audio_off') : null,
                ),
              ),
            ],
          ),
          const SizedBox(height: 6),
          Text(
            'While radio audio is on, the console holds the IC-9700 CI-V session: '
            'wfview at the desk cannot control the radio until audio is off '
            '(or 60 s after the last keep-alive).',
            style: AppTheme.mono(10, color: AppTheme.txtFaint),
          ),
          const SizedBox(height: 12),
          _ControlButton(
            label: 'RADIO POWER ON',
            active: false,
            onPressed: enabled ? () => _send('power_on') : null,
          ),
        ],
      ),
    );
  }
}

class _ServerTag extends StatelessWidget {
  final bool online;

  const _ServerTag({required this.online});

  @override
  Widget build(BuildContext context) {
    final color = online ? AppTheme.green : AppTheme.red;
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: AppTheme.blend(color, 0.45)),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(
        online ? '● SERVER' : '○ NO SERVER',
        style: AppTheme.mono(10, color: color, weight: FontWeight.w600, letterSpacing: 0.08),
      ),
    );
  }
}

/// One status LED. "Off" is only a fault for link-type LEDs (bridge, radio):
/// for session/audio, off is the normal idle state and renders as faint text,
/// not red — a dark SESSION LED must not read as "something broke".
class _Led extends StatelessWidget {
  final String label;
  final String state;
  final bool faultWhenOff;

  const _Led({required this.label, required this.state, required this.faultWhenOff});

  @override
  Widget build(BuildContext context) {
    final color = switch (state) {
      'on' => AppTheme.green,
      'warn' || 'unk' => AppTheme.amber,
      _ => faultWhenOff ? AppTheme.red : AppTheme.txtFaint,
    };
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        Container(
          width: 9,
          height: 9,
          decoration: BoxDecoration(color: color, shape: BoxShape.circle),
        ),
        const SizedBox(width: 5),
        FittedBox(
          child: Text(label, style: AppTheme.mono(10, color: AppTheme.txtMute, weight: FontWeight.w600, letterSpacing: 0.06)),
        ),
      ],
    );
  }
}

class _ControlButton extends StatelessWidget {
  final String label;
  final bool active;
  final VoidCallback? onPressed;

  const _ControlButton({required this.label, required this.active, this.onPressed});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: onPressed,
      style: AppTheme.actionButton(active: active).copyWith(
        minimumSize: const WidgetStatePropertyAll(Size(64, 48)),
      ),
      child: FittedBox(fit: BoxFit.scaleDown, child: Text(label, maxLines: 1)),
    );
  }
}
