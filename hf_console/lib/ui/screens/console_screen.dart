import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import '../widgets/top_bar.dart';
import '../widgets/compass_panel.dart';
import '../widgets/pa_panel.dart';
import '../widgets/tuner_panel.dart';
import '../widgets/ultrabeam_panel.dart';
import '../widgets/dvk_panel.dart';
import '../widgets/antenna_panel.dart';
import '../widgets/power_panel.dart';
import '../widgets/climate_panel.dart';

class ConsoleScreen extends StatelessWidget {
  const ConsoleScreen({super.key});

  @override
  Widget build(BuildContext context) {
    return Consumer<BusStore>(
      builder: (context, store, _) {
        return LayoutBuilder(
          builder: (context, constraints) {
            final isCompact = constraints.maxWidth < 1200 || constraints.maxHeight < 720;
            final scale = isCompact ? 0.90 : 1.0;
            final gap = isCompact ? 6.0 : 10.0;
            final outerPadding = isCompact ? 8.0 : 14.0;
            return Container(
              color: AppTheme.page,
              padding: EdgeInsets.all(outerPadding),
              child: Transform.scale(
                scale: scale,
                alignment: Alignment.topLeft,
                child: SizedBox(
                  width: constraints.maxWidth / scale,
                  height: constraints.maxHeight / scale,
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.stretch,
                    children: [
                      const TopBar(),
                      SizedBox(height: gap),
                      Expanded(
                        flex: 5,
                        child: Row(
                          crossAxisAlignment: CrossAxisAlignment.stretch,
                          children: [
                            const Expanded(flex: 2, child: CompassPanel()),
                            SizedBox(width: gap),
                            Expanded(flex: isCompact ? 1 : 2, child: const PaPanel()),
                            SizedBox(width: gap),
                            Expanded(flex: isCompact ? 1 : 2, child: const TunerPanel()),
                          ],
                        ),
                      ),
                      SizedBox(height: gap),
                      const UltrabeamPanel(),
                      SizedBox(height: gap),
                      const DvkPanel(),
                      SizedBox(height: gap),
                      const AntennaPanel(),
                      SizedBox(height: gap),
                      SizedBox(
                        height: isCompact ? 80 : 110,
                        child: Row(
                          crossAxisAlignment: CrossAxisAlignment.stretch,
                          children: [
                            const Expanded(flex: 3, child: PowerPanel()),
                            SizedBox(width: gap),
                            const Expanded(child: ClimatePanel()),
                          ],
                        ),
                      ),
                    ],
                  ),
                ),
              ),
            );
          },
        );
      },
    );
  }
}
