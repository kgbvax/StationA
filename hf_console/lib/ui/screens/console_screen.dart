import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../theme.dart';
import '../widgets/compass_panel.dart';
import '../widgets/pa_panel.dart';
import '../widgets/tuner_panel.dart';
import '../widgets/ultrabeam_panel.dart';
import '../widgets/dvk_panel.dart';
import '../widgets/antenna_panel.dart';
import '../widgets/power_panel.dart';
import '../widgets/climate_panel.dart';
import '../widgets/faults_bar.dart';

class ConsoleScreen extends StatefulWidget {
  const ConsoleScreen({super.key});

  @override
  State<ConsoleScreen> createState() => _ConsoleScreenState();
}

class _ConsoleScreenState extends State<ConsoleScreen> {
  String _page = 'hf';

  void _setPage(String page) => setState(() => _page = page);
  void _setScheme(AppColorScheme scheme) => setState(() => AppTheme.setScheme(scheme));

  @override
  Widget build(BuildContext context) {
    return Container(
      color: AppTheme.page,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Expanded(
            key: ValueKey(AppTheme.selected),
            child: _PageContent(
              page: _page,
              onSelect: _setPage,
              onScheme: _setScheme,
            ),
          ),
          if (_page != 'hf') const FaultsBar(),
        ],
      ),
    );
  }
}

class _PageContent extends StatelessWidget {
  final String page;
  final ValueChanged<String> onSelect;
  final ValueChanged<AppColorScheme> onScheme;

  const _PageContent({
    required this.page,
    required this.onSelect,
    required this.onScheme,
  });

  @override
  Widget build(BuildContext context) {
    final schemeKey = ValueKey(AppTheme.selected);
    switch (page) {
      case 'station':
        return _StationPage(key: schemeKey, onSelect: onSelect, onScheme: onScheme);
      case 'uhf':
        return _UhfPage(key: schemeKey, onSelect: onSelect, onScheme: onScheme);
      case 'hf':
      default:
        return _HfPage(key: schemeKey, onSelect: onSelect, onScheme: onScheme);
    }
  }
}

class _HfPage extends StatelessWidget {
  final ValueChanged<String> onSelect;
  final ValueChanged<AppColorScheme> onScheme;

  const _HfPage({
    super.key,
    required this.onSelect,
    required this.onScheme,
  });

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(
      builder: (context, constraints) {
        final isCompact = constraints.maxWidth < 1200 || constraints.maxHeight < 720;
        final rightFraction = isCompact ? 0.48 : 0.44;
        final rightMinWidth = isCompact ? 320.0 : 420.0;

        return Row(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Expanded(
              child: Container(
                decoration: AppTheme.paneDecoration(),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    Expanded(
                      child: Container(
                        decoration: BoxDecoration(
                          color: AppTheme.pane,
                          border: Border(bottom: BorderSide(color: AppTheme.cardLine)),
                        ),
                        child: const CompassPanel(),
                      ),
                    ),
                    const UltrabeamPanel(),
                    const AntennaPanel(),
                  ],
                ),
              ),
            ),
            ConstrainedBox(
              constraints: BoxConstraints(minWidth: rightMinWidth),
              child: Container(
                width: constraints.maxWidth * rightFraction,
                decoration: BoxDecoration(
                  color: AppTheme.pane,
                  border: Border(left: BorderSide(color: AppTheme.cardLine)),
                ),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    _PageTopBar(
                      page: 'hf',
                      onSelect: onSelect,
                      onScheme: onScheme,
                      showRadioReadout: true,
                    ),
                    Expanded(
                      child: SingleChildScrollView(
                        child: Column(
                          crossAxisAlignment: CrossAxisAlignment.stretch,
                          children: const [
                            PaPanel(),
                            TunerPanel(),
                            DvkPanel(),
                          ],
                        ),
                      ),
                    ),
                    const FaultsBar(),
                  ],
                ),
              ),
            ),
          ],
        );
      },
    );
  }
}

class _StationPage extends StatelessWidget {
  final ValueChanged<String> onSelect;
  final ValueChanged<AppColorScheme> onScheme;

  const _StationPage({
    super.key,
    required this.onSelect,
    required this.onScheme,
  });

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        _PageTopBar(page: 'station', onSelect: onSelect, onScheme: onScheme),
        Expanded(
          child: SingleChildScrollView(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: const [
                PowerPanel(),
                ClimatePanel(),
                SizedBox(height: 40),
              ],
            ),
          ),
        ),
      ],
    );
  }
}

class _UhfPage extends StatelessWidget {
  final ValueChanged<String> onSelect;
  final ValueChanged<AppColorScheme> onScheme;

  const _UhfPage({
    super.key,
    required this.onSelect,
    required this.onScheme,
  });

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        _PageTopBar(page: 'uhf', onSelect: onSelect, onScheme: onScheme),
        Expanded(
          child: Center(
            child: Text(
              'UHF controls are not yet wired.',
              style: AppTheme.body(18, color: AppTheme.txtFaint, weight: FontWeight.w500),
            ),
          ),
        ),
      ],
    );
  }
}

class _PageTopBar extends StatelessWidget {
  final String page;
  final ValueChanged<String> onSelect;
  final ValueChanged<AppColorScheme> onScheme;
  final bool showRadioReadout;

  const _PageTopBar({
    required this.page,
    required this.onSelect,
    required this.onScheme,
    this.showRadioReadout = false,
  });

  @override
  Widget build(BuildContext context) {
    final mqtt = context.read<MqttService>();
    return Container(
      color: AppTheme.card,
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
      child: Wrap(
        alignment: WrapAlignment.start,
        crossAxisAlignment: WrapCrossAlignment.center,
        runAlignment: WrapAlignment.center,
        spacing: 10,
        runSpacing: 6,
        children: [
          _Tab('Station', 'station', page == 'station', onSelect),
          _Tab('HF', 'hf', page == 'hf', onSelect),
          _Tab('UHF', 'uhf', page == 'uhf', onSelect),
          if (showRadioReadout) const _RadioReadout(),
          _SchemePicker(onScheme: onScheme),
          _ConnectionIndicator(mqtt: mqtt),
          const _OnlineTag(),
        ],
      ),
    );
  }
}

class _RadioReadout extends StatelessWidget {
  const _RadioReadout();

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final slot = store.slots['muehle/hf/radio'];
    final online = slot?.isOnline ?? false;
    final freqHz = store.stateValueAs<int>('muehle/hf/radio', 'freq_hz') ?? 0;
    final band = store.stateValueAs<String>('muehle/hf/radio', 'band') ?? '';
    final mode = store.stateValueAs<String>('muehle/hf/radio', 'mode') ?? '';
    final tx = store.stateValueAs<String>('muehle/hf/radio', 'tx') ?? 'rx';
    final drive = store.stateValueAs<int>('muehle/hf/radio', 'drive') ?? 0;

    if (!online) {
      return Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 4),
        decoration: BoxDecoration(
          color: AppTheme.blend(AppTheme.red, 0.12),
          border: Border.all(color: AppTheme.blend(AppTheme.red, 0.45)),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Text('RADIO OFFLINE', style: AppTheme.mono(12, color: AppTheme.red, weight: FontWeight.w700)),
      );
    }

    final mhz = (freqHz / 1e6).toStringAsFixed(3);
    final txColor = tx == 'tx' ? AppTheme.red : AppTheme.green;
    final txLabel = tx == 'tx' ? 'TX' : 'RX';

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
        ],
      ),
    );
  }
}

class _ConnectionIndicator extends StatelessWidget {
  final MqttService mqtt;

  const _ConnectionIndicator({required this.mqtt});

  @override
  Widget build(BuildContext context) {
    return ValueListenableBuilder<bool>(
      valueListenable: mqtt.connected,
      builder: (context, connected, _) {
        final color = connected ? AppTheme.green : AppTheme.red;
        return Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Container(
              width: 10,
              height: 10,
              decoration: BoxDecoration(
                color: color,
                shape: BoxShape.circle,
                boxShadow: [
                  BoxShadow(
                    color: AppTheme.blend(color, 0.6),
                    blurRadius: 6,
                    spreadRadius: 1,
                  ),
                ],
              ),
            ),
            const SizedBox(width: 6),
            Text(
              connected ? 'MQTT' : 'OFFLINE',
              style: AppTheme.mono(10, color: AppTheme.txtMute, weight: FontWeight.w600, letterSpacing: 0.08),
            ),
          ],
        );
      },
    );
  }
}

class _Tab extends StatelessWidget {
  final String label;
  final String page;
  final bool active;
  final ValueChanged<String> onSelect;

  const _Tab(this.label, this.page, this.active, this.onSelect);

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(right: 6),
      child: ElevatedButton(
        onPressed: () => onSelect(page),
        style: AppTheme.actionButton(active: active).copyWith(
          padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 18, vertical: 8)),
          minimumSize: const WidgetStatePropertyAll(Size(64, 40)),
        ),
        child: Text(label),
      ),
    );
  }
}

class _OnlineTag extends StatelessWidget {
  const _OnlineTag();

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final offline = store.offlineList.length;
    final allOk = offline == 0;
    final color = allOk ? AppTheme.green : AppTheme.red;
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 4),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: AppTheme.blend(color, 0.45)),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(
        allOk ? '● all online' : '● $offline offline',
        style: AppTheme.mono(12, color: color, weight: FontWeight.w600),
      ),
    );
  }
}

class _SchemePicker extends StatelessWidget {
  final ValueChanged<AppColorScheme> onScheme;

  const _SchemePicker({required this.onScheme});

  @override
  Widget build(BuildContext context) {
    final schemes = [
      (AppColorScheme.dc, 'DC'),
      (AppColorScheme.aether, 'AE'),
      (AppColorScheme.paper, 'PA'),
      (AppColorScheme.forest, 'FO'),
    ];
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: schemes.map((s) {
        final active = AppTheme.selected == s.$1;
        return Padding(
          padding: const EdgeInsets.only(left: 4),
          child: ElevatedButton(
            onPressed: () => onScheme(s.$1),
            style: AppTheme.actionButton(active: active).copyWith(
              minimumSize: const WidgetStatePropertyAll(Size(40, 32)),
              padding: const WidgetStatePropertyAll(EdgeInsets.symmetric(horizontal: 8, vertical: 4)),
            ),
            child: Text(s.$2, style: AppTheme.mono(10, weight: FontWeight.w700)),
          ),
        );
      }).toList(),
    );
  }
}
