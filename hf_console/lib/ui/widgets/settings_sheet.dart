// settings_sheet.dart — the one settings sheet of the console: the broker
// login (host, port, user, password), the colour scheme (applied live, not on
// SAVE), the DX-overlay pair (station Maidenhead locator + horstreporter base
// URL) and the antenna-cam base URL (vhfcam-restream's preview server).
//
// There is no separate setup screen. The gear in the top bar opens this sheet
// at any time, and the boot opens it over the console when no broker login is
// stored. A changed broker field reconnects with the new login on SAVE.
//
// It also takes the command key of 75auto (the 75AUTO page signs each command
// with it). The sheet never shows the stored key. It only says whether a key
// is set, and it can replace or clear the key.

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../mqtt/mqtt_service.dart';
import '../../platform/app_quit.dart';
import '../../store/auto_op.dart' show parseAutoOpKey;
import '../../store/credential_store.dart';
import '../../dxspot/dxspot_service.dart';
import '../../vhfcam/vhfcam_service.dart';
import '../theme.dart';

/// Opens the settings dialog. SAVE stores every field and live-applies it:
/// a broker reconnect when the login changed, configure + restart for the DX
/// overlay, configure for the cam service.
Future<void> showSettingsSheet(BuildContext context) async {
  await showDialog<void>(
    context: context,
    builder: (_) => const _SettingsDialog(),
  );
}

class _SettingsDialog extends StatefulWidget {
  const _SettingsDialog();

  @override
  State<_SettingsDialog> createState() => _SettingsDialogState();
}

class _SettingsDialogState extends State<_SettingsDialog> {
  final _storage = CredentialStore();
  final _broker = TextEditingController();
  final _port = TextEditingController();
  final _user = TextEditingController();
  final _pass = TextEditingController();
  bool _passHidden = true;
  // The stored login, to see on SAVE whether the operator changed it.
  String _loginStored = '';
  final _locator = TextEditingController();
  final _url = TextEditingController(text: 'https://horstreporter.kgbvax.net');
  final _callsign = TextEditingController();
  final _camUrl = TextEditingController(text: defaultVhfcamBaseUrl);
  // The 75auto command key. The field starts empty and stays empty after a
  // save: the stored key is never read back into it.
  final _autoOpKey = TextEditingController();
  bool _autoOpKeySet = false;
  bool _autoOpKeyClear = false;
  String? _autoOpKeyError;
  bool _loading = true;

  @override
  void dispose() {
    for (final c in [_broker, _port, _user, _pass, _locator, _url, _callsign, _camUrl, _autoOpKey]) {
      c.dispose();
    }
    super.dispose();
  }

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    final values = await _storage.readAll();
    final key = await _storage.readAutoOpKey();
    if (!mounted) return;
    setState(() {
      _autoOpKeySet = key != null;
      _broker.text = values['mqtt_host'] ?? 'scmino';
      _port.text = values['mqtt_port'] ?? '1883';
      _user.text = values['mqtt_user'] ?? '';
      _pass.text = values['mqtt_password'] ?? '';
      _loginStored = _loginText(values['mqtt_host'], values['mqtt_port'], values['mqtt_user'], values['mqtt_password']);
      _locator.text = values['station_locator'] ?? '';
      _url.text = values['horstreporter_base_url'] ?? 'https://horstreporter.kgbvax.net';
      _callsign.text = values['station_callsign'] ?? '';
      _camUrl.text = resolveVhfcamBaseUrl(values['vhfcam_base_url']);
      _loading = false;
    });
  }

  static String _loginText(String? host, String? port, String? user, String? pass) =>
      [host ?? '', port ?? '', user ?? '', pass ?? ''].join('\u0000');

  Future<void> _save() async {
    final broker = _broker.text.trim();
    final port = int.tryParse(_port.text.trim()) ?? 1883;
    final user = _user.text.trim();
    final pass = _pass.text;
    final locator = _locator.text.trim().toUpperCase();
    final baseUrl = _url.text.trim();
    final callsign = _callsign.text.trim().toUpperCase();
    final camUrl = _camUrl.text.trim();
    // A key that is not 64 hex digits stops the save before anything is
    // written, so the operator can correct it.
    final keyText = _autoOpKey.text.trim();
    if (keyText.isNotEmpty && parseAutoOpKey(keyText) == null) {
      setState(() => _autoOpKeyError = 'Enter exactly 64 hex digits (0-9, a-f).');
      return;
    }
    final hasLogin = broker.isNotEmpty && user.isNotEmpty && pass.isNotEmpty;
    final loginChanged = _loginText(broker, '$port', user, pass) != _loginStored;
    await _storage.writeAll({
      if (broker.isNotEmpty) 'mqtt_host': broker,
      'mqtt_port': '$port',
      'mqtt_user': user,
      'mqtt_password': pass,
      'station_locator': locator,
      'horstreporter_base_url': baseUrl,
      'station_callsign': callsign,
      'vhfcam_base_url': camUrl,
    });
    if (keyText.isNotEmpty) {
      await _storage.writeAutoOpKey(keyText);
    } else if (_autoOpKeyClear) {
      await _storage.clearAutoOpKey();
    }
    if (!mounted) return;
    final mqtt = context.read<MqttService>();
    if (loginChanged && hasLogin) {
      // Fire and forget: the connection indicator shows the outcome.
      mqtt
          .connect(
            host: broker,
            port: port,
            username: user,
            password: pass,
            clientId: 'hf-console-${DateTime.now().millisecondsSinceEpoch}',
          )
          .catchError((_) {});
    }
    final dx = context.read<DxSpotService>();
    dx.configure(baseUrl: baseUrl, locator: locator, callsign: callsign);
    dx.restart();
    // The cam service re-polls and re-probes immediately under the new URL;
    // the feed panel reacts through its listener.
    context.read<VhfcamService>().configure(baseUrl: camUrl);
    Navigator.of(context).pop();
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      // The settings no longer fit a phone in landscape.
      scrollable: true,
      backgroundColor: AppTheme.card,
      insetPadding: const EdgeInsets.symmetric(horizontal: 40, vertical: 24),
      contentPadding: const EdgeInsets.fromLTRB(20, 20, 20, 8),
      shape: RoundedRectangleBorder(side: BorderSide(color: AppTheme.cardLine)), // square frame
      content: SizedBox(
        width: 320,
        child: _loading
            ? Padding(
                padding: const EdgeInsets.all(16),
                child: Center(child: CircularProgressIndicator(color: AppTheme.accent)))
            : Column(
                mainAxisSize: MainAxisSize.min,
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: [
                  Text('SETTINGS', style: AppTheme.display(16, weight: FontWeight.w700)),
                  const SizedBox(height: 12),
                  _schemeRow(),
                  const SizedBox(height: 16),
                  _field('MQTT broker host', _broker, hint: 'e.g. scmino'),
                  const SizedBox(height: 12),
                  _field('MQTT broker port', _port, hint: '1883'),
                  const SizedBox(height: 12),
                  _field('MQTT username', _user, key: const ValueKey('settings-mqtt-user'), hint: 'e.g. console'),
                  const SizedBox(height: 12),
                  _field(
                    'MQTT password',
                    _pass,
                    key: const ValueKey('settings-mqtt-password'),
                    obscure: _passHidden,
                    suffix: IconButton(
                      tooltip: _passHidden ? 'Show password' : 'Hide password',
                      icon: Icon(_passHidden ? Icons.visibility_off : Icons.visibility, size: 18, color: AppTheme.txtMute),
                      onPressed: () => setState(() => _passHidden = !_passHidden),
                    ),
                  ),
                  ValueListenableBuilder<TextEditingValue>(
                    valueListenable: _pass,
                    builder: (context, v, _) {
                      if (v.text.isNotEmpty) return const SizedBox.shrink();
                      return Padding(
                        padding: const EdgeInsets.only(top: 4),
                        child: Text('The console connects to the broker after you enter a username and a password.',
                            style: AppTheme.body(11, color: AppTheme.amber)),
                      );
                    },
                  ),
                  const SizedBox(height: 16),
                  Text('The locator places the station on the maps and is the rotators\' aiming origin.',
                      style: AppTheme.body(11, color: AppTheme.txtMute)),
                  const SizedBox(height: 16),
                  _field('Station locator', _locator, hint: '6 characters, e.g. JO32WE'),
                  // Bearings to nearby targets come from this position: a
                  // 4-character square is 2°×1° — tens of km off.
                  ValueListenableBuilder<TextEditingValue>(
                    valueListenable: _locator,
                    builder: (context, v, _) {
                      final n = v.text.trim().length;
                      if (n == 0 || n >= 6) return const SizedBox.shrink();
                      return Padding(
                        padding: const EdgeInsets.only(top: 4),
                        child: Text(
                          'Use 6 characters: with $n, map bearings to nearby targets are off by tens of km.',
                          key: const ValueKey('locator-precision-warning'),
                          style: AppTheme.body(11, color: AppTheme.amber),
                        ),
                      );
                    },
                  ),
                  const SizedBox(height: 12),
                  _field('DX spot callsign', _callsign, hint: 'blank = all spots of the locator square'),
                  Padding(
                    padding: const EdgeInsets.only(top: 4),
                    child: Text('With a callsign the maps show only reports of this station (who heard it, whom it heard).',
                        style: AppTheme.body(11, color: AppTheme.txtMute)),
                  ),
                  const SizedBox(height: 12),
                  _field('Horstreporter URL', _url, hint: 'https://…'),
                  const SizedBox(height: 12),
                  _field('Antenna cam URL', _camUrl, hint: 'http://…:8083'),
                  const SizedBox(height: 16),
                  _autoOpKeySection(),
                  const SizedBox(height: 16),
                  Row(
                    mainAxisAlignment: MainAxisAlignment.end,
                    children: [
                      if (appQuitSupported) ...[
                        // Takes the free width and scales its label down
                        // before the row can overflow the 320 px sheet.
                        Expanded(
                          child: Align(
                            alignment: Alignment.centerLeft,
                            child: FittedBox(
                              fit: BoxFit.scaleDown,
                              child: TextButton(
                                key: const ValueKey('settings-quit'),
                                onPressed: _confirmQuit,
                                child: Text('QUIT APP',
                                    style: AppTheme.mono(13, color: AppTheme.red, weight: FontWeight.w600)),
                              ),
                            ),
                          ),
                        ),
                      ],
                      TextButton(
                        onPressed: () => Navigator.of(context).pop(),
                        child: Text('CANCEL', style: AppTheme.mono(13, color: AppTheme.txtMute, weight: FontWeight.w600)),
                      ),
                      const SizedBox(width: 8),
                      ElevatedButton(
                        key: const ValueKey('settings-save'),
                        onPressed: _save,
                        style: AppTheme.actionButton(active: true),
                        child: const Text('SAVE'),
                      ),
                    ],
                  ),
                ],
              ),
      ),
    );
  }

  /// QUIT APP behind a confirmation: on the kiosk tablet it also leaves kiosk
  /// mode until the app is started again.
  Future<void> _confirmQuit() async {
    final ok = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        backgroundColor: AppTheme.card,
        shape: RoundedRectangleBorder(side: BorderSide(color: AppTheme.cardLine)), // square frame
        title: Text('QUIT APP?', style: AppTheme.display(16, weight: FontWeight.w700)),
        content: Text(
          'The console closes and kiosk mode ends until you start the app again. '
          'The station keeps running.',
          style: AppTheme.body(13),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(false),
            child: Text('CANCEL', style: AppTheme.mono(13, color: AppTheme.txtMute, weight: FontWeight.w600)),
          ),
          ElevatedButton(
            key: const ValueKey('settings-quit-confirm'),
            onPressed: () => Navigator.of(ctx).pop(true),
            style: AppTheme.actionButton(active: true),
            child: const Text('QUIT'),
          ),
        ],
      ),
    );
    if (ok == true) await quitApp();
  }

  /// Colour-scheme picker. Applies immediately (the console rebuilds behind
  /// the dialog); setState repaints the dialog itself in the new scheme.
  Widget _schemeRow() {
    const schemes = [
      (AppColorScheme.dc, 'Dark'),
      (AppColorScheme.paper, 'Paper'),
      (AppColorScheme.aether, 'Aether'),
    ];
    return Row(
      children: [
        for (final (scheme, label) in schemes)
          Expanded(
            child: Padding(
              padding: EdgeInsets.only(right: scheme == schemes.last.$1 ? 0 : 6),
              child: ElevatedButton(
                onPressed: () => setState(() => AppTheme.setScheme(scheme)),
                style: AppTheme.actionButton(active: AppTheme.selected == scheme),
                child: Text(label),
              ),
            ),
          ),
      ],
    );
  }

  /// The 75auto command key: whether one is set, a field for a new one, and
  /// CLEAR. A new key or a clear takes effect on SAVE.
  Widget _autoOpKeySection() {
    final status = _autoOpKeyClear
        ? 'cleared on SAVE'
        : _autoOpKeySet
            ? 'set'
            : 'not set';
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      mainAxisSize: MainAxisSize.min,
      children: [
        Row(
          children: [
            Expanded(
              child: Text(
                '75auto command key: $status',
                key: const ValueKey('settings-auto-op-key-status'),
                style: AppTheme.mono(12, color: _autoOpKeySet && !_autoOpKeyClear ? AppTheme.green : AppTheme.txtMute),
              ),
            ),
            TextButton(
              key: const ValueKey('settings-auto-op-key-clear'),
              onPressed: _autoOpKeySet && !_autoOpKeyClear
                  ? () => setState(() {
                        _autoOpKeyClear = true;
                        _autoOpKey.clear();
                        _autoOpKeyError = null;
                      })
                  : null,
              child: Text('CLEAR', style: AppTheme.mono(12, weight: FontWeight.w600)),
            ),
          ],
        ),
        const SizedBox(height: 4),
        _field(
          'New 75auto command key',
          _autoOpKey,
          key: const ValueKey('settings-auto-op-key'),
          hint: '64 hex digits, blank keeps the stored key',
          obscure: true,
          errorText: _autoOpKeyError,
          onChanged: (_) {
            if (_autoOpKeyError != null || _autoOpKeyClear) {
              setState(() {
                _autoOpKeyError = null;
                _autoOpKeyClear = false;
              });
            }
          },
        ),
        Padding(
          padding: const EdgeInsets.only(top: 4),
          child: Text('The 75AUTO page signs each command with this key. It is the key file of 75auto on bwpc.',
              style: AppTheme.body(11, color: AppTheme.txtMute)),
        ),
      ],
    );
  }

  Widget _field(
    String label,
    TextEditingController controller, {
    Key? key,
    String? hint,
    bool obscure = false,
    String? errorText,
    Widget? suffix,
    ValueChanged<String>? onChanged,
  }) {
    return TextField(
      key: key,
      controller: controller,
      style: AppTheme.mono(13),
      autocorrect: false,
      obscureText: obscure,
      enableSuggestions: !obscure,
      onChanged: onChanged,
      decoration: InputDecoration(
        labelText: label,
        errorText: errorText,
        suffixIcon: suffix,
        labelStyle: AppTheme.mono(11, color: AppTheme.txtMute),
        hintText: hint,
        hintStyle: AppTheme.mono(11, color: AppTheme.txtFaint),
        filled: true,
        fillColor: AppTheme.pane,
        isDense: true,
        border: OutlineInputBorder(borderSide: BorderSide(color: AppTheme.cardLine), borderRadius: BorderRadius.circular(4)),
        enabledBorder: OutlineInputBorder(borderSide: BorderSide(color: AppTheme.cardLine), borderRadius: BorderRadius.circular(4)),
        focusedBorder: OutlineInputBorder(borderSide: BorderSide(color: AppTheme.accent), borderRadius: BorderRadius.circular(4)),
      ),
    );
  }
}
