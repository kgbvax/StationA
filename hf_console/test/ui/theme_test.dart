import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/ui/theme.dart';

void main() {
  test('panel frames are square: cards and panes carry no corner radius', () {
    for (final scheme in AppColorScheme.values) {
      AppTheme.setScheme(scheme);
      expect(AppTheme.cardDecoration().borderRadius, isNull, reason: '$scheme card');
      expect(AppTheme.cardDecoration(power: true).borderRadius, isNull, reason: '$scheme power card');
      expect(AppTheme.paneDecoration().borderRadius, isNull, reason: '$scheme pane');
    }
    AppTheme.setScheme(AppColorScheme.dc);
  });
}
