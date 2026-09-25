// dx_config_sheet.dart — in-console editor for the non-broker settings: the
// colour scheme (applied live, not on SAVE), the DX-overlay pair (station Maidenhead locator + horstreporter base URL) and
// the antenna-cam base URL (vhfcam-restream's preview server) and the UHF
// array's park position (the PARK key on the sat-rotator panel).
//
// The full setup screen only shows when broker credentials are missing, so an
// already-provisioned tablet (creds stored) boots straight to the console and has
// no way to reach these fields otherwise. This sheet is reachable from a gear in
// the top bar and edits ONLY the overlay/cam keys — broker credentials are left
// untouched.

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/credential_store.dart';
import '../../store/uhf_park.dart';
import '../../dxspot/dxspot_service.dart';
import '../../vhfcam/vhfcam_service.dart';
import '../theme.dart';

/// Opens a modal dialog to edit the station locator, horstreporter URL and
/// antenna-cam URL, then live-applies them to the running services (configure
/// + restart for the DX overlay; configure for the cam service).
Future<void> showDxConfigSheet(BuildContext context) async {
  await showDialog<void>(
    context: context,
    builder: (_) => const _DxConfigDialog(),
  );
}

class _DxConfigDialog extends StatefulWidget {
  const _DxConfigDialog();

  @override
  State<_DxConfigDialog> createState() => _DxConfigDialogState();
}

class _DxConfigDialogState extends State<_DxConfigDialog> {
  final _storage = CredentialStore();
  final _locator = TextEditingController();
  final _url = TextEditingController(text: 'https://horstreporter.kgbvax.net');
  final _camUrl = TextEditingController(text: defaultVhfcamBaseUrl);
  final _parkAz = TextEditingController();
  final _parkEl = TextEditingController();
  bool _loading = true;

  bool get _parkValid =>
      UhfPark.parseAz(_parkAz.text) != null && UhfPark.parseEl(_parkEl.text) != null;

  @override
  void dispose() {
    for (final c in [_locator, _url, _camUrl, _parkAz, _parkEl]) {
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
    if (!mounted) return;
    setState(() {
      _locator.text = values['station_locator'] ?? '';
      _url.text = values['horstreporter_base_url'] ?? 'https://horstreporter.kgbvax.net';
      _camUrl.text = values['vhfcam_base_url'] ?? defaultVhfcamBaseUrl;
      final park = UhfPark.notifier.value;
      _parkAz.text = _fmtDeg(UhfPark.parseAz(values[UhfPark.azKey]) ?? park.az);
      _parkEl.text = _fmtDeg(UhfPark.parseEl(values[UhfPark.elKey]) ?? park.el);
      _loading = false;
    });
  }

  Future<void> _save() async {
    final locator = _locator.text.trim().toUpperCase();
    final baseUrl = _url.text.trim();
    final camUrl = _camUrl.text.trim();
    final parkAz = UhfPark.parseAz(_parkAz.text);
    final parkEl = UhfPark.parseEl(_parkEl.text);
    if (parkAz == null || parkEl == null) return;
    await _storage.writeAll({
      'station_locator': locator,
      'horstreporter_base_url': baseUrl,
      'vhfcam_base_url': camUrl,
      UhfPark.azKey: _fmtDeg(parkAz),
      UhfPark.elKey: _fmtDeg(parkEl),
    });
    if (!mounted) return;
    UhfPark.notifier.value = (az: parkAz, el: parkEl);
    final dx = context.read<DxSpotService>();
    dx.configure(baseUrl: baseUrl, locator: locator);
    dx.restart();
    // The cam service re-polls and re-probes immediately under the new URL;
    // the feed panel reacts through its listener.
    context.read<VhfcamService>().configure(baseUrl: camUrl);
    Navigator.of(context).pop();
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      backgroundColor: AppTheme.card,
      insetPadding: const EdgeInsets.symmetric(horizontal: 40, vertical: 24),
      contentPadding: const EdgeInsets.fromLTRB(20, 20, 20, 8),
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(6), side: BorderSide(color: AppTheme.cardLine)),
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
                  _field('Horstreporter URL', _url, hint: 'https://…'),
                  const SizedBox(height: 12),
                  _field('Antenna cam URL', _camUrl, hint: 'http://…:8083'),
                  const SizedBox(height: 16),
                  Text('Where PARK on the UHF tab sends the rotators.',
                      style: AppTheme.body(11, color: AppTheme.txtMute)),
                  const SizedBox(height: 8),
                  Row(
                    children: [
                      Expanded(child: _field('Park azimuth °', _parkAz, hint: '0–360', key: const ValueKey('park-az'), number: true)),
                      const SizedBox(width: 8),
                      Expanded(child: _field('Park elevation °', _parkEl, hint: '0–90', key: const ValueKey('park-el'), number: true)),
                    ],
                  ),
                  if (!_parkValid)
                    Padding(
                      padding: const EdgeInsets.only(top: 4),
                      child: Text(
                        'Azimuth 0–360°, elevation 0–90°.',
                        key: const ValueKey('park-invalid'),
                        style: AppTheme.body(11, color: AppTheme.amber),
                      ),
                    ),
                  const SizedBox(height: 16),
                  Row(
                    mainAxisAlignment: MainAxisAlignment.end,
                    children: [
                      TextButton(
                        onPressed: () => Navigator.of(context).pop(),
                        child: Text('CANCEL', style: AppTheme.mono(13, color: AppTheme.txtMute, weight: FontWeight.w600)),
                      ),
                      const SizedBox(width: 8),
                      ElevatedButton(
                        key: const ValueKey('settings-save'),
                        onPressed: _parkValid ? _save : null,
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

  Widget _field(String label, TextEditingController controller, {String? hint, Key? key, bool number = false}) {
    return TextField(
      key: key,
      controller: controller,
      style: AppTheme.mono(13),
      autocorrect: false,
      keyboardType: number ? const TextInputType.numberWithOptions(decimal: true) : null,
      onChanged: number ? (_) => setState(() {}) : null,
      decoration: InputDecoration(
        labelText: label,
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

/// Whole degrees print without decimals (200 not 200.0); fractional keep theirs.
String _fmtDeg(double v) =>
    v == v.truncateToDouble() ? v.truncate().toString() : v.toString();
