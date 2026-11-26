/// The 75AUTO page: the session of 75auto, the rule-driven FT8/FT4 operator
/// on bwpc, read from the logic slot `muehle/hf/auto-op`. The phone shows
/// it mirrored on the center screen of the car, so the page is built for a
/// glance: big readouts, high contrast, and in landscape no page scroll.
///
/// Landscape (about 914 x 411 dp on a phone): the top bar, the readout row,
/// the main area with the band selector, the band activity, the RX
/// frequency list, and the control column, and the bottom strip with the
/// action log and the health row. Stop TX stays in view at the bottom of
/// the control column, also under the LINK DOWN banner.
///
/// Portrait: the readouts and the control column on top, the lists below in
/// a scroll view.
///
/// The page owns one [AutoCommander] for its controls. A `/state.error`
/// of 75auto shows as a red line: in landscape in place of the action log,
/// in portrait under the status strip. Refused commands show next to their
/// controls, not there.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';

import '../theme.dart';
import 'action_log_strip.dart';
import 'auto_commands.dart';
import 'auto_controls.dart';
import 'auto_readouts.dart';
import 'auto_top_bar.dart';
import 'band_selector.dart';
import 'decode_list.dart';
import 'health_row.dart';
import 'rx_list.dart';

/// Builds the console top bar with an optional status part on its right.
typedef AutoTopBarBuilder = Widget Function(Widget? trailing);

/// The heights of the landscape layout.
const autoReadoutHeight = 56.0;
const autoBottomHeight = 36.0;
const autoGap = 4.0;

class AutoPage extends StatefulWidget {
  final AutoTopBarBuilder topBar;

  const AutoPage({super.key, required this.topBar});

  @override
  State<AutoPage> createState() => _AutoPageState();
}

class _AutoPageState extends State<AutoPage> {
  late final AutoCommander _commander;

  AutoTopBarBuilder get topBar => widget.topBar;

  @override
  void initState() {
    super.initState();
    _commander = AutoCommander(store: context.read<BusStore>(), mqtt: context.read<MqttService>());
  }

  @override
  void dispose() {
    _commander.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ChangeNotifierProvider<AutoCommander>.value(
      value: _commander,
      child: LayoutBuilder(builder: (context, c) {
        final landscape = c.maxWidth > c.maxHeight;
        return Container(
          color: AppTheme.page,
          child: landscape ? _landscape(c.maxWidth) : _portrait(),
        );
      }),
    );
  }

  Widget _landscape(double width) {
    final s = AppTheme.scaleForWidth(width);
    const gap = SizedBox(width: autoGap, height: autoGap);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        topBar(const AutoStatusStrip()),
        Expanded(
          child: Padding(
            padding: const EdgeInsets.all(autoGap),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                const SizedBox(height: autoReadoutHeight, child: AutoReadouts()),
                gap,
                Expanded(
                  child: Row(
                    crossAxisAlignment: CrossAxisAlignment.stretch,
                    children: [
                      SizedBox(width: 72 * s, child: const BandSelector()),
                      gap,
                      const Expanded(child: DecodeList()),
                      gap,
                      const Expanded(child: RxList()),
                      gap,
                      SizedBox(width: 272 * s, child: const AutoControlColumn()),
                    ],
                  ),
                ),
                gap,
                const SizedBox(
                  height: autoBottomHeight,
                  child: Row(
                    crossAxisAlignment: CrossAxisAlignment.stretch,
                    children: [
                      Expanded(child: AutoErrorLine(orElse: ActionLogStrip())),
                      gap,
                      Flexible(
                        child: FittedBox(fit: BoxFit.scaleDown, alignment: Alignment.centerRight, child: HealthRow()),
                      ),
                    ],
                  ),
                ),
              ],
            ),
          ),
        ),
      ],
    );
  }

  Widget _portrait() {
    const gap = SizedBox(width: autoGap, height: autoGap);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        topBar(null),
        Padding(
          padding: const EdgeInsets.fromLTRB(autoGap, autoGap, autoGap, 0),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: const [
              AutoStatusStrip(oneRow: false),
              AutoErrorLine(),
              gap,
              AutoReadouts(twoRows: true),
              gap,
              AutoControlColumn(cardHeight: 124),
            ],
          ),
        ),
        Expanded(
          child: SingleChildScrollView(
            key: const ValueKey('auto-portrait-scroll'),
            padding: const EdgeInsets.all(autoGap),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: const [
                BandSelector(horizontal: true),
                gap,
                SizedBox(height: 300, child: DecodeList()),
                gap,
                SizedBox(height: 200, child: RxList()),
                gap,
                ActionLogStrip(vertical: true),
                gap,
                HealthRow(wrap: true),
              ],
            ),
          ),
        ),
      ],
    );
  }
}

/// The `/state.error` of 75auto as one red line. Without an error it shows
/// [orElse], or nothing. Plain text only.
class AutoErrorLine extends StatelessWidget {
  final Widget? orElse;

  const AutoErrorLine({super.key, this.orElse});

  @override
  Widget build(BuildContext context) {
    final error = context.select<BusStore, String?>((s) => s.autoOp?.error);
    if (error == null || error.isEmpty) return orElse ?? const SizedBox.shrink();
    return Container(
      key: const ValueKey('auto-error'),
      constraints: const BoxConstraints(minHeight: 28),
      padding: const EdgeInsets.symmetric(horizontal: 8),
      margin: orElse == null ? const EdgeInsets.only(top: autoGap) : EdgeInsets.zero,
      alignment: Alignment.centerLeft,
      decoration: BoxDecoration(
        color: AppTheme.blend(AppTheme.red, 0.12),
        border: Border.all(color: AppTheme.blend(AppTheme.red, 0.7)),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text('75AUTO ERROR · $error',
          maxLines: 1,
          overflow: TextOverflow.ellipsis,
          style: AppTheme.mono(12, color: AppTheme.red, weight: FontWeight.w600)),
    );
  }
}
