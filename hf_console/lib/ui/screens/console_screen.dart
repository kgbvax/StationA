import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../dxspot/dxspot_service.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import '../widgets/dx_map_container.dart';
import '../widgets/pa_panel.dart';
import '../widgets/tuner_panel.dart';
import '../widgets/ultrabeam_panel.dart';
import '../widgets/dvk_panel.dart';
import '../widgets/antenna_panel.dart';
import '../widgets/power_panel.dart';
import '../widgets/rotator_presets_bar.dart';
import '../widgets/sat_rotator_panel.dart';
import '../widgets/uhf_radio_panel.dart';
import '../widgets/pol_ctrl_panel.dart';
import '../widgets/cam_feed_panel.dart';
import '../widgets/cam_radio_controls.dart';
import '../widgets/faults_bar.dart';
import '../widgets/dx_config_sheet.dart';

class ConsoleScreen extends StatefulWidget {
  const ConsoleScreen({super.key});

  @override
  State<ConsoleScreen> createState() => _ConsoleScreenState();
}

class _ConsoleScreenState extends State<ConsoleScreen> {
  String _page = 'hf';

  // The DX-spot subscription follows the active page: the UHF dial reads
  // 2m/70cm only, and horstreporter drops every other band server-side
  // (`enabled_bands` stream parameter) — the device never downloads them.
  static const Set<String> _uhfBands = {'2m', '70cm'};

  void _setPage(String page) {
    setState(() => _page = page);
    context.read<DxSpotService>().setBands(page == 'uhf' ? _uhfBands : null);
  }

  @override
  Widget build(BuildContext context) {
    // Panels read AppTheme's static tokens directly, so a scheme change (made
    // in the settings dialog) must rebuild the whole screen; the key below
    // also resets page state that caches colours.
    return ValueListenableBuilder<AppColorScheme>(
      valueListenable: AppTheme.notifier,
      builder: (context, _, _) => _build(context),
    );
  }

  Widget _build(BuildContext context) {
    return Container(
      color: AppTheme.page,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          const LinkStatusBanner(),
          Expanded(
            key: ValueKey(AppTheme.selected),
            child: _PageContent(
              page: _page,
              onSelect: _setPage,
            ),
          ),
        ],
      ),
    );
  }
}

/// Full-width warning strip shown while the MQTT link is down. Everything on
/// screen is stale retained state and every publish is dropped — that has to
/// be unmissable, not a tiny dot in the top bar. The copy promises only what
/// is true: taps are not gated panel-by-panel, commands are simply lost.
class LinkStatusBanner extends StatelessWidget {
  const LinkStatusBanner({super.key});

  @override
  Widget build(BuildContext context) {
    final mqtt = context.read<MqttService>();
    return ValueListenableBuilder<bool>(
      valueListenable: mqtt.connected,
      builder: (context, connected, _) {
        if (connected) return const SizedBox.shrink();
        return Container(
          color: AppTheme.blend(AppTheme.red, 0.18),
          padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
          child: Text(
            'LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED',
            textAlign: TextAlign.center,
            style: AppTheme.mono(13, color: AppTheme.red, weight: FontWeight.w700, letterSpacing: 0.14),
          ),
        );
      },
    );
  }
}

class _PageContent extends StatelessWidget {
  final String page;
  final ValueChanged<String> onSelect;

  const _PageContent({
    required this.page,
    required this.onSelect,
  });

  @override
  Widget build(BuildContext context) {
    final schemeKey = ValueKey(AppTheme.selected);
    switch (page) {
      case 'station':
        return _StationPage(key: schemeKey, onSelect: onSelect);
      case 'uhf':
        return _UhfPage(key: schemeKey, onSelect: onSelect);
      case 'cam':
        return _CamPage(key: schemeKey, onSelect: onSelect);
      case 'hf':
      default:
        return _HfPage(key: schemeKey, onSelect: onSelect);
    }
  }
}

class _HfPage extends StatelessWidget {
  final ValueChanged<String> onSelect;

  const _HfPage({
    super.key,
    required this.onSelect,
  });

  @override
  Widget build(BuildContext context) {
    // Canonical iOS tablet/phone split: phones (shortestSide < 600) get a
    // single vertical scroll of every panel for full feature parity on a
    // small screen; tablets share the two-column shell with the UHF and
    // Station pages (see _TabletShell) so nothing moves on a page switch.
    final isPhone = MediaQuery.of(context).size.shortestSide < 600;

    if (isPhone) {
      return Container(
        color: AppTheme.page,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _PageTopBar(page: 'hf', onSelect: onSelect),
            Expanded(
              flex: 5,
              child: Container(
                decoration: BoxDecoration(
                  color: AppTheme.pane,
                  border: Border(bottom: BorderSide(color: AppTheme.cardLine)),
                ),
                // Presets stay in the scroll column on phones — the map
                // is too small to overlay the five-button rail.
                child: const DxMapContainer(showPresets: false),
              ),
            ),
            Expanded(
              flex: 6,
              child: SingleChildScrollView(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: const [
                    UltrabeamPanel(),
                    AntennaPanel(),
                    RotatorPresetsBar(),
                    PaPanel(),
                    TunerPanel(),
                    DvkPanel(),
                    FaultsBar(),
                  ],
                ),
              ),
            ),
          ],
        ),
      );
    }

    return _TabletShell(
      page: 'hf',
      onSelect: onSelect,
      rotator: hfRotator,
      leftUnderMap: const [UltrabeamPanel(), AntennaPanel()],
      rightChildren: const [
        PaPanel(),
        TunerPanel(),
        DvkPanel(),
      ],
    );
  }
}

/// Shared tablet skeleton for all three pages: left pane = the DX map with
/// optional panels pinned below it, right rail = top bar (page/scheme
/// toggles), a scrolling panel column, and the faults bar — identical
/// geometry on every page. Deliberate: a page switch must not move the
/// toggles under the operator's hand, so the shell — not the page — owns the
/// split (same compact-breakpoint fractions the HF page used to compute).
class _TabletShell extends StatelessWidget {
  final String page;
  final ValueChanged<String> onSelect;

  /// Which rotator the left pane's compass dial reads: the HF rotator on the
  /// HF page, the VHF az-rotator on the UHF page, none on Station.
  final RotatorSurface? rotator;

  /// Projection the left pane's map opens with (UHF: Mercator).
  final DxProjection initialMapProjection;

  /// Zoom the Mercator map opens with (UHF: 4×).
  final double? initialMapZoom;

  /// Panels pinned under the map in the left pane (HF: ultrabeam + antenna).
  final List<Widget> leftUnderMap;

  /// Optional content pinned ABOVE the map in the left pane (CAM: the video
  /// feed) — the map keeps whatever height is left below it. The chrome stays
  /// put either way; only the left pane's split changes on that page.
  final Widget? leftTop;

  /// Panels scrolled in the right rail below the top bar.
  final List<Widget> rightChildren;

  const _TabletShell({
    required this.page,
    required this.onSelect,
    required this.rotator,
    this.initialMapProjection = DxProjection.azimuth,
    this.initialMapZoom,
    this.leftTop,
    this.leftUnderMap = const [],
    required this.rightChildren,
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
                    if (leftTop != null) leftTop!,
                    Expanded(
                      child: Container(
                        decoration: BoxDecoration(
                          color: AppTheme.pane,
                          border: Border(bottom: BorderSide(color: AppTheme.cardLine)),
                        ),
                        // Tablet: direction presets live on the map's right
                        // edge (above the +/- zoom stepper) — the column no
                        // longer spends a footer row on them.
                        child: DxMapContainer(
                          rotator: rotator,
                          initialProjection: initialMapProjection,
                          initialMercatorZoom: initialMapZoom,
                        ),
                      ),
                    ),
                    ...leftUnderMap,
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
                    _PageTopBar(page: page, onSelect: onSelect),
                    Expanded(
                      child: SingleChildScrollView(
                        child: Column(
                          crossAxisAlignment: CrossAxisAlignment.stretch,
                          children: rightChildren,
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

  const _StationPage({
    super.key,
    required this.onSelect,
  });

  @override
  Widget build(BuildContext context) {
    // Phones scroll a single column (same as HF); tablets share the
    // two-column shell so the top bar never moves between pages.
    final isPhone = MediaQuery.of(context).size.shortestSide < 600;

    if (isPhone) {
      return Container(
        color: AppTheme.page,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _PageTopBar(page: 'station', onSelect: onSelect),
            Expanded(
              child: SingleChildScrollView(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: const [
                    PowerPanel(),
                    SizedBox(height: 40),
                    FaultsBar(),
                  ],
                ),
              ),
            ),
          ],
        ),
      );
    }

    return _TabletShell(
      page: 'station',
      onSelect: onSelect,
      // Station infrastructure page: the map is a bare DX compass — no
      // rotator needle, azimuth chip, presets or aim affordances.
      rotator: null,
      rightChildren: const [
        PowerPanel(),
      ],
    );
  }
}

class _UhfPage extends StatelessWidget {
  final ValueChanged<String> onSelect;

  const _UhfPage({
    super.key,
    required this.onSelect,
  });

  @override
  Widget build(BuildContext context) {
    // Panel order follows the slot docs (U8): the IC-9700 radio (U7) leads —
    // the operating object the rotators and polarization exist to serve —
    // with the sat-ops rotator surface (U8) and the Tier-2 X-Quad
    // polarization control (U11) below. The map dial reads the VHF array
    // azimuth (muehle/uhf/az-rotator) here, not the HF rotator; aims go out
    // as goto on the sat-bridge contract. Phones scroll that single column;
    // tablets share the two-column shell (map left, panels right) so the
    // top bar never moves between pages.
    final isPhone = MediaQuery.of(context).size.shortestSide < 600;

    if (isPhone) {
      return Container(
        color: AppTheme.page,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _PageTopBar(page: 'uhf', onSelect: onSelect),
            Expanded(
              child: SingleChildScrollView(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: const [
                    UhfRadioPanel(),
                    SizedBox(height: 12),
                    SatRotatorPanel(),
                    SizedBox(height: 12),
                    PolCtrlPanel(),
                    SizedBox(height: 40),
                    FaultsBar(),
                  ],
                ),
              ),
            ),
          ],
        ),
      );
    }

    return _TabletShell(
      page: 'uhf',
      onSelect: onSelect,
      rotator: vhfRotator,
      initialMapProjection: DxProjection.mercator,
      initialMapZoom: 4.0,
      rightChildren: const [
        UhfRadioPanel(),
        SatRotatorPanel(),
        PolCtrlPanel(),
      ],
    );
  }
}

class _CamPage extends StatelessWidget {
  final ValueChanged<String> onSelect;

  const _CamPage({
    super.key,
    required this.onSelect,
  });

  @override
  Widget build(BuildContext context) {
    // The antenna camera (vhfcam-restream preview) is the page's primary
    // object: the feed takes the left pane's top slot at full width (see
    // _TabletShell.leftTop) with the DX map keeping the rest below it — its
    // az dial reads the VHF array the camera is watching. The right rail is
    // the IC-9700 audio-chain surface mirrored from the :8083 page. The cam
    // server is bus-independent and ad hoc, so this page adds no faults-bar
    // slots; its own panels carry the offline states.
    final isPhone = MediaQuery.of(context).size.shortestSide < 600;

    if (isPhone) {
      return Container(
        color: AppTheme.page,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _PageTopBar(page: 'cam', onSelect: onSelect),
            Expanded(
              child: SingleChildScrollView(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: const [
                    CamFeedPanel(),
                    CamRadioControls(),
                    SizedBox(height: 40),
                    FaultsBar(),
                  ],
                ),
              ),
            ),
          ],
        ),
      );
    }

    return _TabletShell(
      page: 'cam',
      onSelect: onSelect,
      rotator: vhfRotator,
      leftTop: const CamFeedPanel(),
      rightChildren: const [
        CamRadioControls(),
      ],
    );
  }
}

class _PageTopBar extends StatelessWidget {
  final String page;
  final ValueChanged<String> onSelect;

  const _PageTopBar({
    required this.page,
    required this.onSelect,
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
          _Tab('CAM', 'cam', page == 'cam', onSelect),
          const _DxSettingsButton(),
          _ConnectionIndicator(mqtt: mqtt),
          const _OnlineTag(),
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

class _DxSettingsButton extends StatelessWidget {
  const _DxSettingsButton();

  @override
  Widget build(BuildContext context) {
    return InkWell(
      borderRadius: BorderRadius.circular(4),
      onTap: () => showDxConfigSheet(context),
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
        decoration: BoxDecoration(
          color: AppTheme.pane,
          border: Border.all(color: AppTheme.cardLineHi),
          borderRadius: BorderRadius.circular(4),
        ),
        child: Tooltip(
          message: 'Settings',
          child: Icon(Icons.tune, size: 20, color: AppTheme.txt),
        ),
      ),
    );
  }
}
