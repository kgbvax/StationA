import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:provider/provider.dart';
import 'store/bus_store.dart';
import 'store/credential_store.dart';
import 'mqtt/mqtt_service.dart';
import 'dxspot/dxspot_service.dart';
import 'ui/theme.dart';
import 'ui/screens/console_screen.dart';
import 'ui/screens/setup_screen.dart';

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  if (!kIsWeb) {
    // Allow all orientations; the console layout adapts via LayoutBuilder
    // (phones stack vertically, tablets stay side-by-side). immersiveSticky
    // is an Android-only full-screen mode (no-op on iOS/desktop).
    SystemChrome.setPreferredOrientations(DeviceOrientation.values);
    SystemChrome.setEnabledSystemUIMode(SystemUiMode.immersiveSticky);
  }
  runApp(const HfConsoleApp());
}

class HfConsoleApp extends StatelessWidget {
  const HfConsoleApp({super.key});

  @override
  Widget build(BuildContext context) {
    return const _AppRoot();
  }
}

class _AppRoot extends StatefulWidget {
  const _AppRoot();

  @override
  State<_AppRoot> createState() => _AppRootState();
}

class _AppRootState extends State<_AppRoot> {
  final _storage = CredentialStore();
  final _store = BusStore();
  late final MqttService _mqtt;
  final _dxSpot = DxSpotService();
  bool _ready = false;
  bool _showConsole = false;

  @override
  void initState() {
    super.initState();
    _mqtt = MqttService(_store);
    _store.addListener(_onBusStoreUpdate);
    _enforceFullScreen();
    _tryAutoConnect();
  }

  void _onBusStoreUpdate() {
    // Drive the DX-spot SNR filter from the live radio mode. Defaults to SSB
    // threshold when the radio is in a phone/data mode, switches to CW in cw,
    // and disables gating when no mode is known.
    final mode = _store.stateValue('muehle/hf/radio', 'mode') as String?;
    _dxSpot.setMode(mode);
  }

  void _enforceFullScreen() {
    if (!kIsWeb) {
      SystemChrome.setEnabledSystemUIMode(SystemUiMode.immersiveSticky);
    }
  }

  Future<void> _tryAutoConnect() async {
    final values = await _storage.readAll();
    final host = values['mqtt_host'];
    final port = int.tryParse(values['mqtt_port'] ?? '');
    final user = values['mqtt_user'];
    final pass = values['mqtt_password'];
    // DX-spot overlay is independent of the broker; start it whenever a station
    // locator is configured (DxSpotService.start() no-ops without one).
    _dxSpot.configure(
      baseUrl: values['horstreporter_base_url'],
      locator: values['station_locator'],
      callsign: values['station_callsign'],
    );
    _dxSpot.start();
    if (host != null && port != null && user != null && pass != null && pass.isNotEmpty) {
      try {
        await _connect(host, port, user, pass);
      } catch (_) {
        // offline start is allowed; the indicator and faults bar show the state
      }
      if (mounted) {
        setState(() => _showConsole = true);
      }
    }
    if (mounted) {
      setState(() => _ready = true);
    }
  }

  Future<void> _connect(String host, int port, String username, String password) async {
    await _mqtt.connect(
      host: host,
      port: port,
      username: username,
      password: password,
      clientId: 'hf-console-${DateTime.now().millisecondsSinceEpoch}',
    );
  }

  @override
  void dispose() {
    _store.removeListener(_onBusStoreUpdate);
    _dxSpot.dispose();
    _mqtt.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MultiProvider(
      providers: [
        ChangeNotifierProvider<BusStore>.value(value: _store),
        ChangeNotifierProvider<DxSpotService>.value(value: _dxSpot),
        Provider<MqttService>.value(value: _mqtt),
      ],
      child: ValueListenableBuilder<AppColorScheme>(
        valueListenable: AppTheme.notifier,
        builder: (context, scheme, _) {
          return MaterialApp(
            debugShowCheckedModeBanner: false,
            title: 'Mühle HF',
            theme: ThemeData.dark().copyWith(
              scaffoldBackgroundColor: AppTheme.page,
              textTheme: ThemeData.dark().textTheme.apply(
                fontFamily: 'IBMPlexSans',
                bodyColor: AppTheme.txt,
                displayColor: AppTheme.txt,
              ),
            ),
            home: _ready
                ? (_showConsole
                    ? const Scaffold(body: ConsoleScreen())
                    : Scaffold(
                        body: Builder(
                          builder: (context) {
                            return SetupScreen(
                              onSave: (host, port, user, pass, locator, baseUrl) async {
                                await _storage.writeAll({
                                  'mqtt_host': host,
                                  'mqtt_port': port.toString(),
                                  'mqtt_user': user,
                                  'mqtt_password': pass,
                                  'station_locator': locator,
                                  'horstreporter_base_url': baseUrl,
                                });
                                try {
                                  await _connect(host, port, user, pass);
                                } catch (_) {
                                  // keep going; console will show the offline indicator
                                }
                                _dxSpot.configure(baseUrl: baseUrl, locator: locator);
                                _dxSpot.restart();
                                if (mounted) {
                                  setState(() => _showConsole = true);
                                }
                              },
                            );
                          },
                        ),
                      ))
                : Scaffold(body: Center(child: CircularProgressIndicator(color: AppTheme.accent))),
          );
        },
      ),
    );
  }
}
