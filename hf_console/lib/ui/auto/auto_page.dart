/// The 75AUTO page: the session of 75auto, the rule-driven FT8/FT4 operator
/// on bwpc, read from the logic slot `muehle/hf/auto-op`. The phone shows
/// it mirrored on the center screen of the car, so the page is built for a
/// glance: big readouts, high contrast, and in landscape no page scroll.
///
/// Landscape (about 914 x 411 dp on a phone, scaled up as a whole on a
/// larger screen): the top bar, the readout row,
/// the main area with the band selector, the band activity with Target and
/// Mode under it, the RX frequency list with Arm CQ and Stop TX under it,
/// and the DX card, and the bottom strip with the action log. Stop TX stays
/// in view under the RX list, also under the LINK DOWN banner.
///
/// Portrait: the readouts and the control column on top, the lists below in
/// a scroll view.
///
/// The page owns one [AutoCommander] for its controls. A `/state.error`
/// of 75auto and the faults of its inputs (see [autoInputFaults]) show as
/// a red line: in landscape in place of the action log, in portrait under
/// the status strip. Healthy inputs show nothing. Refused commands show next to their
/// controls, not there.
library;

import 'dart:math' as math;

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
import 'dx_card.dart';
import 'input_faults.dart';
import 'rx_list.dart';

/// Builds the console top bar with an optional status part on its right.
typedef AutoTopBarBuilder = Widget Function(Widget? trailing);

/// The heights of the landscape layout.
const autoReadoutHeight = autoReadoutRowHeight;

/// The landscape design size of the area under the top bar: a phone in
/// landscape. On a larger screen (the mirrored phone on the car screen, a
/// tablet) that area lays out at this size and scales up as a whole, so
/// text and controls grow with the width. The factor is at most
/// [autoMaxScale]. The top bar does not scale: it is the bar of every
/// page, and its page tabs stay in place.
const autoDesignWidth = 914.0;
const autoDesignHeight = 350.0;
const autoMaxScale = 2.0;

/// The scale factor of the landscape page for a [width] x [height] area.
/// Up to 5 % over the design size gives room, not scale.
double autoScaleFor(double width, double height) {
  final k = math.min(width / autoDesignWidth, height / autoDesignHeight);
  if (k < 1.05) return 1.0;
  return k.clamp(1.0, autoMaxScale);
}
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
        if (!landscape) return Container(color: AppTheme.page, child: _portrait());
        return Container(
          color: AppTheme.page,
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              topBar(const AutoStatusStrip()),
              Expanded(child: LayoutBuilder(builder: (context, b) => _scaled(context, b.biggest))),
            ],
          ),
        );
      }),
    );
  }

  /// The area under the top bar, scaled up as a whole on a larger screen.
  /// Taps scale with it.
  Widget _scaled(BuildContext context, Size size) {
    final k = autoScaleFor(size.width, size.height);
    if (k <= 1.0) return _landscape();
    final inner = Size(size.width / k, size.height / k);
    return FittedBox(
      key: const ValueKey('auto-page-scaled'),
      fit: BoxFit.contain,
      alignment: Alignment.topCenter,
      child: MediaQuery(
        data: MediaQuery.of(context).copyWith(size: inner),
        child: SizedBox(width: inner.width, height: inner.height, child: _landscape()),
      ),
    );
  }

  Widget _landscape() {
    const gap = SizedBox(width: autoGap, height: autoGap);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
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
                      const SizedBox(width: 94, child: BandSelector()),
                      gap,
                      const Expanded(
                        child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
                          Expanded(child: DecodeList()),
                          SizedBox(height: autoGap),
                          AutoTargetMode(),
                        ]),
                      ),
                      gap,
                      const Expanded(
                        child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
                          Expanded(child: RxList()),
                          SizedBox(height: autoGap),
                          AutoControls(targetMode: false),
                        ]),
                      ),
                      gap,
                      const SizedBox(width: 256, child: DxCard()),
                    ],
                  ),
                ),
                gap,
                const SizedBox(
                  height: autoBottomHeight,
                  child: AutoErrorLine(orElse: ActionLogStrip()),
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
              SizedBox(height: 208, child: DxCard()),
              gap,
              AutoControls(),
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
              ],
            ),
          ),
        ),
      ],
    );
  }
}

/// The `/state.error` of 75auto and the input faults as one red line.
/// Without either it shows [orElse], or nothing. Plain text only.
class AutoErrorLine extends StatelessWidget {
  final Widget? orElse;

  const AutoErrorLine({super.key, this.orElse});

  @override
  Widget build(BuildContext context) {
    final text = context.select<BusStore, String>((s) => autoErrorText(s.autoOp?.error, autoInputFaultsNow(s)));
    if (text.isEmpty) return orElse ?? const SizedBox.shrink();
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
      child: Text(text,
          maxLines: 1,
          overflow: TextOverflow.ellipsis,
          style: AppTheme.mono(12, color: AppTheme.red, weight: FontWeight.w600)),
    );
  }
}

/// The text of the error line: the 75auto error first, then the input
/// faults. Empty when there is nothing to show.
String autoErrorText(String? error, List<String> faults) => [
      if (error != null && error.isNotEmpty) '75AUTO ERROR · $error',
      if (faults.isNotEmpty) 'INPUT FAULT · ${faults.join(' · ')}',
    ].join(' · ');
