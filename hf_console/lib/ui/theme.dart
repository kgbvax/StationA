import 'package:flutter/material.dart';

enum AppColorScheme {
  dc,
  paper,
  forest,
}

class AppTheme extends ChangeNotifier {
  static final AppTheme _instance = AppTheme._internal();
  factory AppTheme() => _instance;
  AppTheme._internal();

  static final ValueNotifier<AppColorScheme> _notifier = ValueNotifier(AppColorScheme.dc);
  static ValueNotifier<AppColorScheme> get notifier => _notifier;

  static final Map<AppColorScheme, _Palette> _palettes = {
    AppColorScheme.dc: _Palette(
      page: const Color(0xFF0A0C10),
      pane: const Color(0xFF0F1218),
      card: const Color(0xFF151923),
      land: const Color(0xFF232C3E),
      line: const Color(0xFF2A3142),
      lineHi: const Color(0xFF3D4558),
      txt: const Color(0xFFDEE3EC),
      txtMute: const Color(0xFF8C97AB),
      txtFaint: const Color(0xFF5B6579),
      accent: const Color(0xFF5FB2C9),
      accentDim: const Color(0x1E5FB2C9),
      green: const Color(0xFF5CCB8A),
      amber: const Color(0xFFD7B04A),
      red: const Color(0xFFD9685C),
      orange: const Color(0xFFD99A5F),
    ),
    AppColorScheme.paper: _Palette(
      page: const Color(0xFFE5E3DD),
      pane: const Color(0xFFEFEDE7),
      card: const Color(0xFFF9F8F5),
      land: const Color(0xFFD0C6B4),
      line: const Color(0xFFD4D1CA),
      lineHi: const Color(0xFFB8B4AB),
      txt: const Color(0xFF1A1A1A),
      txtMute: const Color(0xFF5E5A53),
      txtFaint: const Color(0xFF8C887E),
      accent: const Color(0xFF005F99),
      accentDim: const Color(0x1A005F99),
      green: const Color(0xFF1E6B48),
      amber: const Color(0xFFA36A00),
      red: const Color(0xFFB52E1D),
      orange: const Color(0xFFC45F18),
    ),
    AppColorScheme.forest: _Palette(
      page: const Color(0xFF151A17),
      pane: const Color(0xFF1C231E),
      card: const Color(0xFF242C26),
      land: const Color(0xFF34403A),
      line: const Color(0xFF3D4740),
      lineHi: const Color(0xFF515C54),
      txt: const Color(0xFFDDE4DD),
      txtMute: const Color(0xFF8D9A8D),
      txtFaint: const Color(0xFF5E6B5E),
      accent: const Color(0xFFC8A45C),
      accentDim: const Color(0x1EC8A45C),
      green: const Color(0xFF6DB88B),
      amber: const Color(0xFFE0B35A),
      red: const Color(0xFFD2786C),
      orange: const Color(0xFFCF9A68),
    ),
  };

  static AppColorScheme _selected = AppColorScheme.dc;

  static AppColorScheme get selected => _selected;

  static void setScheme(AppColorScheme scheme) {
    _selected = scheme;
    _notifier.value = scheme;
  }

  static _Palette get _p => _palettes[_selected]!;

  static Color get page => _p.page;
  static Color get pane => _p.pane;
  static Color get card => _p.card;
  static Color get land => _p.land;
  static Color get cardLine => _p.line;
  static Color get cardLineHi => _p.lineHi;
  static Color get txt => _p.txt;
  static Color get txtMute => _p.txtMute;
  static Color get txtFaint => _p.txtFaint;
  static Color get accent => _p.accent;
  static Color get accentDim => _p.accentDim;
  static Color get green => _p.green;
  static Color get amber => _p.amber;
  static Color get red => _p.red;
  static Color get orange => _p.orange;

  static bool get isLight => _selected == AppColorScheme.paper;

  static Color get activeButtonText => isLight ? const Color(0xFFFFFFFF) : const Color(0xFF000000);

  /// Colour for a DX-spot dot by its horstreporter `sourceType`
  /// (`mqtt`→FT8/FT4 green, `dxcluster`→cyan, `rbn`→amber, `wspr`→orange).
  /// Used as a fallback when a spot has no `band` tag.
  static Color spotColor(String sourceType) {
    switch (sourceType) {
      case 'mqtt':
        return green;
      case 'dxcluster':
        return accent;
      case 'rbn':
        return amber;
      case 'wspr':
        return orange;
      default:
        return txtMute;
    }
  }

  /// Colour for a DX-spot dot by its amateur band (`20m`, `40m`, …). Mirrors
  /// horstreporter's `static/utils.js:577-592` `bandColors` *exactly* so a
  /// spot dot in this console matches the colour the user sees in the web
  /// frontend for the same band — the visual cross-component invariant is
  /// deliberate. Hex values are fixed (not theme tokens); see
  /// `docs/conventions/band-mode-reference.md` for the canonical table and
  /// rationale. Falls back to grey for unknown / blank labels.
  static Color bandColor(String band) => _bandColorFor(band);

  static Color _bandColorFor(String band) {
    const palette = <String, Color>{
      '160m': Color(0xFF8B0000), // dark red
      '80m':  Color(0xFF800080), // purple
      '60m':  Color(0xFF4B0082), // indigo
      '40m':  Color(0xFF0000FF), // blue
      '30m':  Color(0xFF03B1B1), // teal
      '20m':  Color(0xFF008000), // green
      '17m':  Color(0xFF808000), // olive
      '15m':  Color(0xFFFFA500), // orange
      '12m':  Color(0xFF00FFFF), // cyan
      '10m':  Color(0xFFFF0000), // red
      '6m':   Color(0xFFFF00FF), // magenta
      '4m':   Color(0xFFFF1493), // deep pink
      '2m':   Color(0xFF008080), // dark teal
    };
    final c = palette[band];
    return c ?? const Color(0xFF555555); // matches horstreporter's bandColors.all
  }

  /// Continuous opacity for a grid-square fill from its top-quartile mean SNR
  /// (dB). Mirrors `horstreporter/static/utils.js:656-661` `gridSnrOpacity`:
  /// 0 dB → 0.45, -10 dB → 0.15, floor 0.10, cap 0.75.
  static double gridSnrOpacity(double scoreDb) {
    if (!scoreDb.isFinite) return 0.10;
    if (scoreDb < 0) {
      return (0.45 + 0.03 * scoreDb).clamp(0.10, 0.75);
    }
    return (0.45 + 0.015 * scoreDb).clamp(0.10, 0.75);
  }

  static Color get purpleBorder => const Color(0x597C5CFF);

  static TextStyle display(double size, {Color? color, FontWeight weight = FontWeight.w600, double letterSpacing = 0.04}) =>
      TextStyle(fontFamily: 'SairaCondensed', fontSize: size + 1, fontWeight: weight, color: color ?? txt, letterSpacing: letterSpacing, decoration: TextDecoration.none);

  static TextStyle body(double size, {Color? color, FontWeight weight = FontWeight.w400, double letterSpacing = 0.0}) =>
      TextStyle(fontFamily: 'IBMPlexSans', fontSize: size + 1, fontWeight: weight, color: color ?? txt, letterSpacing: letterSpacing, decoration: TextDecoration.none);

  static TextStyle mono(double size, {Color? color, FontWeight weight = FontWeight.w400, double letterSpacing = 0.0}) =>
      TextStyle(fontFamily: 'IBMPlexMono', fontSize: size + 1, fontWeight: weight, color: color ?? txt, letterSpacing: letterSpacing, decoration: TextDecoration.none);

  static double scaleForWidth(double width) {
    if (width >= 1400) return 1.25;
    if (width >= 1200) return 1.1;
    return 1.0;
  }

  static ButtonStyle actionButton({bool active = false, bool danger = false, bool amber = false, bool dangerActive = false, bool fullWidth = false}) =>
      ElevatedButton.styleFrom(
        backgroundColor: dangerActive
            ? red
            : active
                ? accent
                : danger
                ? blend(red, 0.12)
                : amber
                    ? blend(AppTheme.amber, 0.12)
                    : pane,
        foregroundColor: dangerActive
            ? activeButtonText
            : active
                ? activeButtonText
                : danger
                    ? red
                    : txt,
        side: BorderSide(
          color: dangerActive
              ? red
              : active
                  ? accent
                  : danger
                  ? red
                  : amber
                      ? AppTheme.amber
                      : cardLineHi,
        ),
        shadowColor: const Color(0x40000000),
        elevation: 0,
        padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
        minimumSize: const Size(44, 40),
        textStyle: mono(13, weight: FontWeight.w600),
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(4)),
      );

  /// Icon-button sibling of [actionButton]. Used by the compass +/- zoom
  /// buttons. Same `pane` / `cardLineHi` chrome as the inactive preset row,
  /// 48 dp hit target per hf_console/CLAUDE.md ("All touch targets ≥48dp").
  static ButtonStyle iconActionButton({bool active = false, bool danger = false}) =>
      ElevatedButton.styleFrom(
        backgroundColor: active ? blend(accent, 0.18) : pane,
        foregroundColor: danger ? red : txt,
        side: BorderSide(color: active ? accent : cardLineHi),
        shadowColor: const Color(0x40000000),
        elevation: 0,
        padding: EdgeInsets.zero,
        minimumSize: const Size(48, 48),
        iconSize: 18,
        iconColor: txt,
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(4)),
      );

  static BoxDecoration cardDecoration({bool power = false}) => BoxDecoration(
        color: power ? const Color(0x141A0F2F) : card,
        border: Border.all(color: power ? purpleBorder : cardLine),
        borderRadius: BorderRadius.circular(6),
      );

  static BoxDecoration paneDecoration() => BoxDecoration(
        color: pane,
        border: Border.all(color: cardLine),
        borderRadius: BorderRadius.circular(6),
      );

  static Color blend(Color c, double alpha) => Color.fromARGB(
        (alpha * 255).round(),
        (c.r * 255).round().clamp(0, 255),
        (c.g * 255).round().clamp(0, 255),
        (c.b * 255).round().clamp(0, 255),
      );
}

class _Palette {
  final Color page;
  final Color pane;
  final Color card;
  final Color land;
  final Color line;
  final Color lineHi;
  final Color txt;
  final Color txtMute;
  final Color txtFaint;
  final Color accent;
  final Color accentDim;
  final Color green;
  final Color amber;
  final Color red;
  final Color orange;

  _Palette({
    required this.page,
    required this.pane,
    required this.card,
    required this.land,
    required this.line,
    required this.lineHi,
    required this.txt,
    required this.txtMute,
    required this.txtFaint,
    required this.accent,
    required this.accentDim,
    required this.green,
    required this.amber,
    required this.red,
    required this.orange,
  });
}
