import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/main.dart';

void main() {
  testWidgets('App builds without crashing', (WidgetTester tester) async {
    await tester.pumpWidget(const HfConsoleApp());
    // The full app contains continuously-running animations (e.g. the antenna
    // pending dot), so pumpAndSettle would time out. Just verify it pumps.
    expect(find.byType(HfConsoleApp), findsOneWidget);
  });
}
