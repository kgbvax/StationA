import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:hf_console/ui/screens/startup_splash.dart';

void main() {
  testWidgets('splash shows target and countdown, expires at budget end', (tester) async {
    var expired = 0;
    await tester.pumpWidget(MaterialApp(
      home: StartupSplash(
        host: 'shari',
        port: 1883,
        waitSeconds: 20,
        onWaitExpired: () => expired++,
      ),
    ));

    expect(find.text('connecting to shari:1883'), findsOneWidget);
    expect(find.text('broker wait 20s'), findsOneWidget);
    expect(expired, isZero);

    // Mid-wait: countdown ticks down, no expiry yet.
    await tester.pump(const Duration(seconds: 8));
    expect(find.text('broker wait 12s'), findsOneWidget);
    expect(expired, isZero);

    // Budget end: countdown reaches zero and expiry fires exactly once,
    // even if the timer keeps running.
    await tester.pump(const Duration(seconds: 12));
    expect(find.text('broker wait 0s'), findsOneWidget);
    expect(expired, 1);
    await tester.pump(const Duration(seconds: 3));
    expect(expired, 1);
  });

  testWidgets('no-broker boot shows the neutral starting line without countdown', (tester) async {
    var expired = 0;
    await tester.pumpWidget(MaterialApp(
      home: StartupSplash(waitSeconds: null, onWaitExpired: () => expired++),
    ));

    expect(find.text('starting'), findsOneWidget);
    expect(find.textContaining('broker wait'), findsNothing);
    await tester.pump(const Duration(seconds: 5));
    expect(expired, isZero);
  });
}