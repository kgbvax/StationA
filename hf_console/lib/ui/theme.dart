import 'package:flutter/material.dart';

class AppTheme {
  static const page = Color(0xFF050506);
  static const bg = Color(0xFF0C0E12);
  static const card = Color(0xFF13161B);
  static const cardLine = Color(0xFF20252D);
  static const cardLineHi = Color(0xFF2D333D);
  static const txt = Color(0xFFE7EBF0);
  static const txtMute = Color(0xFF6F7A8A);
  static const txtFaint = Color(0xFF414A58);
  static const cyan = Color(0xFF27D7D8);
  static const cyanDim = Color(0xFF1E7A7A);
  static const green = Color(0xFF36B37E);
  static const amber = Color(0xFFE0A23C);
  static const red = Color(0xFFD9533A);
  static const blue = Color(0xFF4A90C2);
  static const purpleCard = Color(0x141A0F2F);
  static const purpleBorder = Color(0x597C5CFF);

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

  static ButtonStyle actionButton({bool active = false, bool danger = false, bool amber = false}) =>
      ElevatedButton.styleFrom(
        backgroundColor: active
            ? cyanDim
            : danger
                ? red.withOpacity(0.10)
                : amber
                    ? Color(0x16E0A23C)
                    : Color(0xFF23272F),
        foregroundColor: danger
            ? red
            : amber
                ? AppTheme.amber
                : AppTheme.txt,
        side: BorderSide(
          color: active
              ? cyan
              : danger
                  ? red
                  : amber
                      ? AppTheme.amber
                      : cardLineHi,
        ),
        shadowColor: Colors.black.withOpacity(0.3),
        elevation: 1,
        padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 7),
        minimumSize: const Size(44, 36),
        textStyle: mono(13, weight: FontWeight.w600, color: AppTheme.txt),
        shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(4)),
      );

  static BoxDecoration cardDecoration({bool power = false}) => BoxDecoration(
        color: power ? const Color(0x141A0F2F) : card,
        border: Border.all(color: power ? purpleBorder : cardLine),
        borderRadius: BorderRadius.circular(6),
      );
}
