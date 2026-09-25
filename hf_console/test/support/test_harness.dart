import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'package:hf_console/mqtt/mqtt_service.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/vhfcam/vhfcam_service.dart';
import 'fake_mqtt_service.dart';

/// Wraps a widget with the providers it needs for isolated widget tests.
///
/// Provides a [BusStore], a [FakeMqttService] and an idle [DxSpotService] (no
/// station locator → overlay off, beam-only compass) so panels can read slot
/// state and publish commands without touching a real broker or SSE feed.
class TestHarness extends StatelessWidget {
  final Widget child;
  final BusStore? store;
  final FakeMqttService? mqtt;
  final DxSpotService? dxSpot;
  final VhfcamService? vhfcam;

  const TestHarness({
    super.key,
    required this.child,
    this.store,
    this.mqtt,
    this.dxSpot,
    this.vhfcam,
  });

  @override
  Widget build(BuildContext context) {
    final busStore = store ?? BusStore();
    // The console under test is assumed link-up: panels gate their controls
    // on store.linkUp in addition to per-slot liveness. No grace timer —
    // widget tests must not leave a pending timer at teardown.
    busStore.markConnected(scheduleGraceNotify: false);
    final fakeMqtt = mqtt ?? FakeMqttService(busStore);
    // Keep the service's link flag in step with the store, as the real
    // MqttService does — otherwise the LINK DOWN banner and the device
    // chip disagree in every rendered test.
    fakeMqtt.connected.value = true;
    final dx = dxSpot ?? DxSpotService();
    return MultiProvider(
      providers: [
        ChangeNotifierProvider<BusStore>.value(value: busStore),
        ChangeNotifierProvider<DxSpotService>.value(value: dx),
        // Never polled under test (the poll loop only runs after start()).
        ChangeNotifierProvider<VhfcamService>.value(value: vhfcam ?? VhfcamService()),
        Provider<MqttService>.value(value: fakeMqtt),
      ],
      child: MaterialApp(
        debugShowCheckedModeBanner: false,
        theme: ThemeData.dark().copyWith(
          scaffoldBackgroundColor: AppTheme.page,
          textTheme: ThemeData.dark().textTheme.apply(
            fontFamily: 'IBMPlexSans',
            bodyColor: AppTheme.txt,
            displayColor: AppTheme.txt,
          ),
        ),
        home: Scaffold(
          // Freeze animations (the PulsingAmberButton pulse repeats forever
          // by design) so pumpAndSettle always returns in widget tests. The
          // pulse itself is exercised by pulsing_amber_button_test with an
          // explicit disableAnimations:false override.
          body: Builder(
            builder: (context) => MediaQuery(
              data: MediaQuery.of(context).copyWith(disableAnimations: true),
              child: child,
            ),
          ),
        ),
      ),
    );
  }
}
