// cam_record_controls.dart — start/stop a recording of the antenna-cam
// preview (overlay + IC-9700 audio) on vhfcam-restream, with its progress.
//
// The server does the recording, enforces the limits (max length, free-disk
// floor) and holds the radio audio while it records; the console only asks
// and shows the server's answer. Recordings are listed and downloaded on the
// server's :8083 page — its address is shown here as selectable text.

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../vhfcam/rec_status.dart';
import '../../vhfcam/vhfcam_service.dart';
import '../theme.dart';
import 'card_container.dart';

class CamRecordControls extends StatefulWidget {
  const CamRecordControls({super.key});

  @override
  State<CamRecordControls> createState() => _CamRecordControlsState();
}

class _CamRecordControlsState extends State<CamRecordControls> {
  bool _busy = false;

  Future<void> _toggle(bool start) async {
    if (_busy) return;
    setState(() => _busy = true);
    final err = await context.read<VhfcamService>().setRecording(start);
    if (!mounted) return;
    setState(() => _busy = false);
    if (err != null) {
      ScaffoldMessenger.of(context).showSnackBar(SnackBar(
        content: Text('${start ? 'Recording not started' : 'Recording not stopped'}: $err', style: AppTheme.mono(12)),
        duration: const Duration(seconds: 3),
      ));
    }
  }

  @override
  Widget build(BuildContext context) {
    final svc = context.watch<VhfcamService>();
    final rec = svc.status?.rec;

    final String line;
    final bool canStart;
    if (!svc.serverOnline) {
      line = 'Cam server offline.';
      canStart = false;
    } else if (rec == null) {
      line = 'This cam server cannot record (older version).';
      canStart = false;
    } else if (!rec.enabled) {
      line = 'Recording is off on the cam server.';
      canStart = false;
    } else if (rec.recording) {
      line = '${_clock(rec.elapsedS)} recorded · ${_clock(rec.remainingS)} left · free ${_gb(rec.freeBytes)}'
          '${rec.stalled ? ' · no new video' : ''}';
      canStart = false;
    } else if (rec.finalizing) {
      line = 'Saving ${rec.name}…';
      canStart = false;
    } else {
      line = _idleLine(rec);
      canStart = svc.streamLive;
    }

    final recording = rec?.recording ?? false;
    final VoidCallback? onPressed = _busy
        ? null
        : recording
            ? () => _toggle(false)
            : canStart
                ? () => _toggle(true)
                : null;

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          // No REC badge here: the feed header carries it (camcorder
          // convention, visible beside or above this card on every layout),
          // and the button + progress line below already say "recording".
          const CardHeader(title: 'RECORDING'),
          const SizedBox(height: 10),
          SizedBox(
            height: 48,
            child: ElevatedButton(
              key: const ValueKey('rec-toggle'),
              onPressed: onPressed,
              // Recording renders solid red: the one state here that is live
              // and costs disk (the console's "red = shouting" rule).
              style: AppTheme.actionButton(dangerActive: recording),
              child: Text(recording ? 'STOP RECORDING' : 'START RECORDING'),
            ),
          ),
          const SizedBox(height: 8),
          Text(line, key: const ValueKey('rec-line'), style: AppTheme.mono(11, color: AppTheme.txtMute)),
          const SizedBox(height: 6),
          SelectableText(
            'Download: ${svc.recordingsUrl}',
            style: AppTheme.mono(10, color: AppTheme.txtFaint),
          ),
        ],
      ),
    );
  }

  String _idleLine(RecStatus rec) {
    if (rec.lastError.isNotEmpty) return 'Last recording: ${rec.lastError}';
    const reasons = {
      'max_duration': 'stopped at the time limit',
      'low_disk': 'stopped: disk almost full',
      'shutdown': 'stopped by a service shutdown',
      'recovered': 'recovered after an interruption',
    };
    if (rec.lastFile.isNotEmpty) {
      final why = reasons[rec.stopReason];
      return 'Saved ${rec.lastFile}${why == null ? '' : ' ($why)'}';
    }
    return 'Up to ${rec.maxS ~/ 60} min · free ${_gb(rec.freeBytes)}';
  }
}

/// REC state badge: red `● REC mm:ss` while recording, amber while saving.
/// Shown on the cam feed header.
class RecBadge extends StatelessWidget {
  final RecStatus rec;

  const RecBadge({super.key, required this.rec});

  @override
  Widget build(BuildContext context) {
    if (!rec.recording && !rec.finalizing) return const SizedBox.shrink();
    final color = rec.recording ? AppTheme.red : AppTheme.amber;
    final text = rec.recording ? '● REC ${_clock(rec.elapsedS)}' : 'SAVING';
    return Container(
      key: const ValueKey('rec-badge'),
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(text, style: AppTheme.mono(10, color: color, weight: FontWeight.w700, letterSpacing: 0.08)),
    );
  }
}

String _two(int n) => n.toString().padLeft(2, '0');
String _clock(int s) => '${_two(s ~/ 60)}:${_two(s % 60)}';
String _gb(int bytes) => '${(bytes / 1e9).toStringAsFixed(1)} GB';
