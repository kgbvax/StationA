import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'package:hf_console/mqtt/mqtt_service.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/dxspot/dxspot_service.dart';
import 'package:hf_console/ui/theme.dart';
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

  const TestHarness({
    super.key,
    required this.child,
    this.store,
    this.mqtt,
    this.dxSpot,
  });

  @override
  Widget build(BuildContext context) {
    final busStore = store ?? BusStore();
    final fakeMqtt = mqtt ?? FakeMqttService(busStore);
    final dx = dxSpot ?? DxSpotService();
    return MultiProvider(
      providers: [
        ChangeNotifierProvider<BusStore>.value(value: busStore),
        ChangeNotifierProvider<DxSpotService>.value(value: dx),
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
        home: Scaffold(body: child),
      ),
    );
  }
}
