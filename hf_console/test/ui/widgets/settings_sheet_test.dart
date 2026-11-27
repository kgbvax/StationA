import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/settings_sheet.dart';
import 'package:shared_preferences/shared_preferences.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/test_harness.dart';

/// Records the logins the sheet connects with.
class _RecordingMqtt extends FakeMqttService {
  final logins = <String>[];
  _RecordingMqtt(super.store);

  @override
  Future<void> connect({
    required String host,
    required int port,
    required String username,
    required String password,
    required String clientId,
  }) async {
    logins.add('$username:$password@$host:$port');
  }
}

void main() {
  group('settings sheet broker login', () {
    Future<_RecordingMqtt> open(WidgetTester tester) async {
      final store = BusStore();
      final mqtt = _RecordingMqtt(store);
      await tester.binding.setSurfaceSize(const Size(1200, 1600));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      await tester.pumpWidget(TestHarness(
        store: store,
        mqtt: mqtt,
        child: Builder(
          builder: (context) => TextButton(
            onPressed: () => showSettingsSheet(context),
            child: const Text('open'),
          ),
        ),
      ));
      await tester.tap(find.text('open'));
      await tester.pumpAndSettle();
      return mqtt;
    }

    Future<void> save(WidgetTester tester) async {
      await tester.ensureVisible(find.byKey(const ValueKey('settings-save')));
      await tester.tap(find.byKey(const ValueKey('settings-save')));
      await tester.pumpAndSettle();
    }

    testWidgets('shows the stored username and password', (tester) async {
      SharedPreferences.setMockInitialValues({
        'mqtt_host': 'scmino',
        'mqtt_port': '1883',
        'mqtt_user': 'hf',
        'mqtt_password': 'secret',
      });
      await open(tester);
      final user = tester.widget<TextField>(find.byKey(const ValueKey('settings-mqtt-user')));
      final pass = tester.widget<TextField>(find.byKey(const ValueKey('settings-mqtt-password')));
      expect(user.controller!.text, 'hf');
      expect(pass.controller!.text, 'secret');
      expect(pass.obscureText, isTrue);
    });

    testWidgets('a changed password is stored and reconnects', (tester) async {
      SharedPreferences.setMockInitialValues({
        'mqtt_host': 'scmino',
        'mqtt_port': '1883',
        'mqtt_user': 'hf',
        'mqtt_password': 'old',
      });
      final mqtt = await open(tester);
      await tester.enterText(find.byKey(const ValueKey('settings-mqtt-password')), 'new');
      await save(tester);
      final prefs = await SharedPreferences.getInstance();
      expect(prefs.getString('mqtt_password'), 'new');
      expect(mqtt.logins, ['hf:new@scmino:1883']);
    });

    testWidgets('an unchanged login does not reconnect', (tester) async {
      SharedPreferences.setMockInitialValues({
        'mqtt_host': 'scmino',
        'mqtt_port': '1883',
        'mqtt_user': 'hf',
        'mqtt_password': 'secret',
      });
      final mqtt = await open(tester);
      await save(tester);
      expect(mqtt.logins, isEmpty);
    });

    testWidgets('a first login from an empty store connects', (tester) async {
      SharedPreferences.setMockInitialValues({});
      final mqtt = await open(tester);
      await tester.enterText(find.byKey(const ValueKey('settings-mqtt-user')), 'console');
      await tester.enterText(find.byKey(const ValueKey('settings-mqtt-password')), 'pw');
      await save(tester);
      final prefs = await SharedPreferences.getInstance();
      expect(prefs.getString('mqtt_user'), 'console');
      expect(mqtt.logins, ['console:pw@scmino:1883']);
    });
  });
}
