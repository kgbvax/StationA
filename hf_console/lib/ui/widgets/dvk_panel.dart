import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'mic_profile_row.dart';

class DvkPanel extends StatelessWidget {
  static const String slot = 'hf/radio';
  static const String topic = 'muehle/hf/radio/cmd';

  const DvkPanel({super.key});

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
          _buildHeader(context),
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

  Widget _buildHeader(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = slotState?.isOnline ?? false;

        final freqHz = store.stateValueAs<int>('muehle/$slot', 'freq_hz') ?? 0;
        final band = store.stateValueAs<String>('muehle/$slot', 'band') ?? '';
        final mode = store.stateValueAs<String>('muehle/$slot', 'mode') ?? '';
        final tx = store.stateValueAs<String>('muehle/$slot', 'tx') ?? 'rx';
        final drive = store.stateValueAs<int>('muehle/$slot', 'drive') ?? 0;

        final status = store.stateValueAs<String>('muehle/$slot', 'dvk_status') ?? 'idle';
        final id = store.stateValueAs<int>('muehle/$slot', 'dvk_id') ?? 0;

        return Row(
          mainAxisAlignment: MainAxisAlignment.spaceBetween,
          children: [
            Text('TRX · FLEX-8400'.toUpperCase(),
                style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
            _RadioReadout(
              online: online,
              freqHz: freqHz,
              band: band,
              mode: mode,
              tx: tx,
              drive: drive,
              dvkStatus: status,
              dvkId: id,
            ),
          ],
        );
      },
    );
  }

  Widget _buildBandRow(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = slotState?.isOnline ?? false;
        final currentBand = store.stateValueAs<String>('muehle/$slot', 'band') ?? '';
        final mqtt = context.read<MqttService>();

        const bands = ['80', '40', '20', '17', '15', '12', '10'];
        return Wrap(
          spacing: 4,
          runSpacing: 4,
          children: bands.map((band) {
            final full = '${band}m';
            final active = currentBand == full;
            return ElevatedButton(
              onPressed: online ? () => mqtt.publish(topic, radioSetBandPayload(full), retain: false) : null,
              style: AppTheme.actionButton(active: active).copyWith(
                minimumSize: const WidgetStatePropertyAll(Size(64, 44)),
                padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 12, vertical: 8)),
              ),
              child: Text(full),
            );
          }).toList(),
        );
      },
    );
  }

  Widget _buildButtonRow(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        final slotState = store.slots['muehle/$slot'];
        final online = slotState?.isOnline ?? false;
        final status = store.stateValueAs<String>('muehle/$slot', 'dvk_status') ?? 'idle';
        final activeId = store.stateValueAs<int>('muehle/$slot', 'dvk_id') ?? 0;
        final isPlaying = status == 'playback';

        final mqtt = context.read<MqttService>();

        return Row(
          children: [
            for (var i = 1; i <= 4; i++)
              Expanded(
                child: Padding(
                  padding: const EdgeInsets.only(right: 4),
                  child: ElevatedButton(
                    onPressed: online ? () => mqtt.publish(topic, dvkPlayPayload(i), retain: false) : null,
                    style: AppTheme.actionButton(active: online && isPlaying && activeId == i).copyWith(
                      minimumSize: const WidgetStatePropertyAll(Size(0, 44)),
                      padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 6)),
                      textStyle: WidgetStatePropertyAll(AppTheme.mono(12, weight: FontWeight.w600)),
                    ),
                    child: Text('DVK$i'),
                  ),
                ),
              ),
            ElevatedButton(
              onPressed: online ? () => mqtt.publish(topic, dvkStopPayload(), retain: false) : null,
              style: AppTheme.actionButton(danger: true).copyWith(
                minimumSize: const WidgetStatePropertyAll(Size(64, 44)),
                padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 12, vertical: 6)),
              ),
              child: const Text('STOP'),
            ),
          ],
        );
      },
    );
  }
}

class _RadioReadout extends StatelessWidget {
  final bool online;
  final int freqHz;
  final String band;
  final String mode;
  final String tx;
  final int drive;
  final String dvkStatus;
  final int dvkId;

  const _RadioReadout({
    required this.online,
    required this.freqHz,
    required this.band,
    required this.mode,
    required this.tx,
    required this.drive,
    required this.dvkStatus,
    required this.dvkId,
  });

  @override
  Widget build(BuildContext context) {
    if (!online) {
      return Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 4),
        decoration: BoxDecoration(
          color: AppTheme.blend(AppTheme.red, 0.12),
          border: Border.all(color: AppTheme.blend(AppTheme.red, 0.45)),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Text('OFFLINE', style: AppTheme.mono(12, color: AppTheme.red, weight: FontWeight.w700)),
      );
    }

    final mhz = (freqHz / 1e6).toStringAsFixed(3);
    final txColor = tx == 'tx' ? AppTheme.red : AppTheme.green;
    final txLabel = tx == 'tx' ? 'TX' : 'RX';

    final (dvkLabel, dvkColor, _) = _statusStyle(dvkStatus, online, dvkId);

    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 5),
      decoration: BoxDecoration(
        color: AppTheme.pane,
        border: Border.all(color: AppTheme.cardLine),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(mhz, style: AppTheme.mono(16, weight: FontWeight.w700)),
          Text(' MHz', style: AppTheme.mono(10, color: AppTheme.txtFaint)),
          const SizedBox(width: 12),
          Text(mode.toUpperCase(), style: AppTheme.mono(13, weight: FontWeight.w700, color: AppTheme.accent)),
          const SizedBox(width: 12),
          Text(band.toUpperCase(), style: AppTheme.mono(13, color: AppTheme.txtMute, weight: FontWeight.w600)),
          const SizedBox(width: 12),
          Container(
            padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 1),
            decoration: BoxDecoration(
              color: AppTheme.blend(txColor, 0.12),
              border: Border.all(color: txColor),
              borderRadius: BorderRadius.circular(4),
            ),
            child: Text(txLabel, style: AppTheme.mono(11, color: txColor, weight: FontWeight.w700)),
          ),
          const SizedBox(width: 12),
          Text('$drive%', style: AppTheme.mono(13, color: AppTheme.txt, weight: FontWeight.w600)),
          if (dvkStatus != 'idle') ...[
            const SizedBox(width: 12),
            Text('· $dvkLabel', style: AppTheme.mono(11, color: dvkColor, weight: FontWeight.w700)),
          ],
        ],
      ),
    );
  }

  (String, Color, Color) _statusStyle(String status, bool online, int id) {
    if (!online) {
      return ('OFFLINE', AppTheme.txtMute, AppTheme.blend(AppTheme.txt, 0.06));
    }
    switch (status) {
      case 'playback':
        final label = id > 0 ? 'PLAYBACK · M$id' : 'PLAYBACK';
        return (label, AppTheme.accent, AppTheme.blend(AppTheme.accent, 0.10));
      case 'recording':
        return ('RECORDING', AppTheme.amber, AppTheme.blend(AppTheme.amber, 0.10));
      case 'preview':
        return ('PREVIEW', AppTheme.amber, AppTheme.blend(AppTheme.amber, 0.10));
      case 'disabled':
        return ('DISABLED', AppTheme.red, AppTheme.blend(AppTheme.red, 0.10));
      case 'idle':
      default:
        return ('IDLE', AppTheme.txtMute, AppTheme.blend(AppTheme.txt, 0.06));
    }
  }
}
