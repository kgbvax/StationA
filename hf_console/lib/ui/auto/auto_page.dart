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
/// This build is read-only. The controls are in place and disabled.
library;

import 'package:flutter/material.dart';

import '../theme.dart';
import 'action_log_strip.dart';
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

class AutoPage extends StatelessWidget {
  final AutoTopBarBuilder topBar;

  const AutoPage({super.key, required this.topBar});

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(builder: (context, c) {
      final landscape = c.maxWidth > c.maxHeight;
      return Container(
        color: AppTheme.page,
        child: landscape ? _landscape(c.maxWidth) : _portrait(),
      );
    });
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
                      Expanded(child: ActionLogStrip()),
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
