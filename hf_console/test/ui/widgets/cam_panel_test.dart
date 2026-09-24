// cam_panel_test.dart — offline/off-state rendering of the CAM surfaces.
//
// The live-feed state itself is not widget-testable (video_player needs the
// platform HLS stack); the state machine around it is — the panel must render
// honest offline states and the controls must gate on server reachability.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:provider/provider.dart';

import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/cam_feed_panel.dart';
import 'package:hf_console/ui/widgets/cam_radio_controls.dart';
import 'package:hf_console/vhfcam/radio_status.dart';
import 'package:hf_console/vhfcam/vhfcam_service.dart';

Widget _wrap(Widget child, {required VhfcamService vhfcam}) {
  final busStore = BusStore();
  // Same link-up assumption as TestHarness; no grace timer (no pending timers
  // at teardown).
  busStore.markConnected(scheduleGraceNotify: false);
  return MultiProvider(
    providers: [
      ChangeNotifierProvider<BusStore>.value(value: busStore),
      ChangeNotifierProvider<VhfcamService>.value(value: vhfcam),
    ],
    child: MaterialApp(
      theme: ThemeData.dark().copyWith(
        scaffoldBackgroundColor: AppTheme.page,
        textTheme: ThemeData.dark().textTheme.apply(fontFamily: 'IBMPlexSans', bodyColor: AppTheme.txt, displayColor: AppTheme.txt),
      ),
      home: Scaffold(body: SingleChildScrollView(child: child)),
    ),
  );
}

void main() {
  testWidgets('feed panel: server offline renders honest state with retry, bus readouts dashed', (tester) async {
    final svc = VhfcamService()..seedStatus(online: false);

    await tester.pumpWidget(_wrap(const CamFeedPanel(), vhfcam: svc));

    expect(find.text('CAM SERVER OFFLINE'), findsOneWidget);
    expect(find.text('RETRY'), findsOneWidget);
    expect(find.text('● CAM OFFLINE'), findsOneWidget);
    // No bus state yet → dashes, not fabricated numbers.
    expect(find.text('AZ ---'), findsOneWidget);
    expect(find.text('EL ---'), findsOneWidget);
    expect(find.text('FREQ ---'), findsOneWidget);
  });

  testWidgets('feed panel: reachable server with stopped sink says so', (tester) async {
    final svc = VhfcamService()..seedStatus(online: true, stream: false);

    await tester.pumpWidget(_wrap(const CamFeedPanel(), vhfcam: svc));

    expect(find.text('PREVIEW SINK STOPPED'), findsOneWidget);
    expect(find.text('● SINK OFF'), findsOneWidget);
    expect(find.text('RETRY'), findsOneWidget);
  });

  testWidgets('radio controls: no server → all buttons disabled', (tester) async {
    final svc = VhfcamService()..seedStatus(online: false);

    await tester.pumpWidget(_wrap(const CamRadioControls(), vhfcam: svc));

    expect(find.text('○ NO SERVER'), findsOneWidget);
    ElevatedButton buttonFor(String label) =>
        tester.widget<ElevatedButton>(find.ancestor(of: find.text(label), matching: find.byType(ElevatedButton)));
    expect(buttonFor('RADIO AUDIO ON').onPressed, isNull);
    expect(buttonFor('RADIO AUDIO OFF').onPressed, isNull);
    expect(buttonFor('RADIO POWER ON').onPressed, isNull);
  });

  testWidgets('radio controls: server online → buttons enabled, LEDs and hint render', (tester) async {
    final svc = VhfcamService()..seedStatus(
      online: true,
      status: RadioStatus(
        bridgeOnline: true,
        sessionHeld: true,
        radioReady: true,
        audioStream: false,
        hint: 'session held by console',
        leds: {'bridge': 'on', 'session': 'on', 'radio': 'on', 'audio': 'off'},
      ),
    );

    await tester.pumpWidget(_wrap(const CamRadioControls(), vhfcam: svc));

    expect(find.text('● SERVER'), findsOneWidget);
    expect(find.text('session held by console'), findsOneWidget);
    ElevatedButton buttonFor(String label) =>
        tester.widget<ElevatedButton>(find.ancestor(of: find.text(label), matching: find.byType(ElevatedButton)));
    expect(buttonFor('RADIO AUDIO ON').onPressed, isNotNull);
    expect(buttonFor('RADIO AUDIO OFF').onPressed, isNotNull);
    expect(buttonFor('RADIO POWER ON').onPressed, isNotNull);
    // The CI-V session-grab caveat is always visible next to the buttons.
    expect(find.textContaining('CI-V session'), findsOneWidget);
  });
}
