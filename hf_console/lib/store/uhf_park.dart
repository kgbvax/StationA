import 'package:flutter/foundation.dart';

/// Where the UHF array rests: the PARK key on the sat-rotator panel sends
/// both axes here. Configurable in Settings (stored under [azKey]/[elKey]);
/// the defaults are this station's park position.
class UhfPark {
  static const azKey = 'uhf_park_az';
  static const elKey = 'uhf_park_el';
  static const defaultAz = 200.0;
  static const defaultEl = 3.0;

  static final ValueNotifier<({double az, double el})> notifier = ValueNotifier(
    (az: defaultAz, el: defaultEl),
  );

  /// Azimuth 0–360°, elevation 0–90°; null when the text is not a number in range.
  static double? parseAz(String? text) => _parse(text, 0, 360);
  static double? parseEl(String? text) => _parse(text, 0, 90);

  static double? _parse(String? text, double min, double max) {
    final v = double.tryParse((text ?? '').trim());
    return (v != null && v.isFinite && v >= min && v <= max) ? v : null;
  }

  /// Applies stored settings; a missing or invalid value falls back to the default.
  static void load(Map<String, String?> values) {
    notifier.value = (
      az: parseAz(values[azKey]) ?? defaultAz,
      el: parseEl(values[elKey]) ?? defaultEl,
    );
  }
}
