import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:provider/provider.dart';
import 'store/bus_store.dart';
import 'store/credential_store.dart';
import 'mqtt/mqtt_service.dart';
import 'ui/theme.dart';
import 'ui/screens/console_screen.dart';
import 'ui/screens/setup_screen.dart';

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  SystemChrome.setPreferredOrientations([
    DeviceOrientation.landscapeLeft,
    DeviceOrientation.landscapeRight,
  ]);
  runApp(const HfConsoleApp());
}

class HfConsoleApp extends StatelessWidget {
  const HfConsoleApp({super.key});

  @override
  Widget build(BuildContext context) {
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
      home: const _AppRoot(),
    );
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
  bool _ready = false;

  @override
  void initState() {
    super.initState();
    _mqtt = MqttService(_store);
    _tryAutoConnect();
  }

  Future<void> _tryAutoConnect() async {
    final values = await _storage.readAll();
    final host = values['mqtt_host'];
    final port = int.tryParse(values['mqtt_port'] ?? '');
    final user = values['mqtt_user'];
    final pass = values['mqtt_password'];
    if (host != null && port != null && user != null && pass != null && pass.isNotEmpty) {
      await _connect(host, port, user, pass);
    }
    setState(() => _ready = true);
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
    _mqtt.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    if (!_ready) {
      return const Scaffold(body: Center(child: CircularProgressIndicator(color: AppTheme.cyan)));
    }
    return MultiProvider(
      providers: [
        ChangeNotifierProvider<BusStore>.value(value: _store),
        Provider<MqttService>.value(value: _mqtt),
      ],
      child: Scaffold(
        body: SetupScreen(
          onSave: (host, port, user, pass) async {
            await _storage.writeAll({
              'mqtt_host': host,
              'mqtt_port': port.toString(),
              'mqtt_user': user,
              'mqtt_password': pass,
            });
            await _connect(host, port, user, pass);
            if (mounted) {
              Navigator.of(context).pushReplacement(
                MaterialPageRoute(builder: (_) => const ConsoleScreen()),
              );
            }
          },
        ),
      ),
    );
  }
}
