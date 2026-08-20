import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/main.dart';

void main() {
  testWidgets('App builds and shows setup', (WidgetTester tester) async {
    await tester.pumpWidget(const HfConsoleApp());
    await tester.pumpAndSettle();
    expect(find.byType(MaterialApp), findsOneWidget);
  });
}
