import 'package:flutter/material.dart';
import '../../store/credential_store.dart';
import '../../ui/theme.dart';

class SetupScreen extends StatefulWidget {
  final void Function(String host, int port, String username, String password, String locator, String baseUrl) onSave;

  const SetupScreen({super.key, required this.onSave});

  @override
  State<SetupScreen> createState() => _SetupScreenState();
}

class _SetupScreenState extends State<SetupScreen> {
  final _storage = CredentialStore();
  final _host = TextEditingController(text: '192.168.1.139');
  final _port = TextEditingController(text: '1883');
  final _user = TextEditingController(text: 'console');
  final _pass = TextEditingController();
  final _locator = TextEditingController(); // station Maidenhead grid → DX overlay
  final _url = TextEditingController(text: 'https://horstreporter.kgbvax.net');
  bool _obscure = true;

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    final values = await _storage.readAll();
    setState(() {
      _host.text = values['mqtt_host'] ?? '192.168.1.139';
      _port.text = values['mqtt_port'] ?? '1883';
      _user.text = values['mqtt_user'] ?? 'console';
      _pass.text = values['mqtt_password'] ?? '';
      _locator.text = values['station_locator'] ?? '';
      _url.text = values['horstreporter_base_url'] ?? 'https://horstreporter.kgbvax.net';
    });
  }

  Future<void> _save() async {
    final host = _host.text.trim();
    final port = int.tryParse(_port.text.trim()) ?? 1883;
    final user = _user.text.trim();
    final pass = _pass.text;
    if (host.isEmpty || user.isEmpty || pass.isEmpty) return;
    final locator = _locator.text.trim().toUpperCase();
    final baseUrl = _url.text.trim();
    await _storage.writeAll({
      'mqtt_host': host,
      'mqtt_port': port.toString(),
      'mqtt_user': user,
      'mqtt_password': pass,
      'station_locator': locator,
      'horstreporter_base_url': baseUrl,
    });
    widget.onSave(host, port, user, pass, locator, baseUrl);
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      backgroundColor: AppTheme.page,
      resizeToAvoidBottomInset: true,
      body: Center(
        child: SingleChildScrollView(
          padding: const EdgeInsets.all(24),
          child: Container(
            width: 420,
            decoration: AppTheme.cardDecoration(),
            padding: const EdgeInsets.all(24),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Text('MÜHLE · HF', style: AppTheme.display(22, weight: FontWeight.w700)),
                const SizedBox(height: 8),
                Text('Broker credentials', style: AppTheme.body(12, color: AppTheme.txtMute)),
                const SizedBox(height: 20),
                _field('Host', _host, false),
                const SizedBox(height: 12),
                _field('Port', _port, false),
                const SizedBox(height: 12),
                _field('Username', _user, false),
                const SizedBox(height: 12),
                _field('Password', _pass, true),
                const SizedBox(height: 20),
                Text('DX overlay (optional)',
                    style: AppTheme.body(12, color: AppTheme.txtMute)),
                const SizedBox(height: 12),
                _field('Station locator', _locator, false),
                const SizedBox(height: 12),
                _field('Horstreporter URL', _url, false),
                const SizedBox(height: 24),
                ElevatedButton(
                  onPressed: _save,
                  style: AppTheme.actionButton(active: true).copyWith(
                    minimumSize: const WidgetStatePropertyAll(Size(double.infinity, 48)),
                  ),
                  child: const Text('CONNECT'),
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }

  Widget _field(String label, TextEditingController controller, bool obscure) {
    return TextField(
      controller: controller,
      obscureText: obscure ? _obscure : false,
      style: AppTheme.mono(13),
      decoration: InputDecoration(
        labelText: label,
        labelStyle: AppTheme.mono(11, color: AppTheme.txtMute),
        filled: true,
        fillColor: AppTheme.pane,
        border: OutlineInputBorder(borderSide: BorderSide(color: AppTheme.cardLine), borderRadius: BorderRadius.circular(4)),
        enabledBorder: OutlineInputBorder(borderSide: BorderSide(color: AppTheme.cardLine), borderRadius: BorderRadius.circular(4)),
        focusedBorder: OutlineInputBorder(borderSide: BorderSide(color: AppTheme.accent), borderRadius: BorderRadius.circular(4)),
        suffixIcon: obscure
            ? IconButton(
                icon: Icon(_obscure ? Icons.visibility_off : Icons.visibility, size: 18, color: AppTheme.txtMute),
                onPressed: () => setState(() => _obscure = !_obscure),
              )
            : null,
      ),
    );
  }
}
