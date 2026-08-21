// dx_config_sheet.dart — in-console editor for the DX-overlay settings
// (station Maidenhead locator + horstreporter base URL).
//
// The full setup screen only shows when broker credentials are missing, so an
// already-provisioned tablet (creds stored) boots straight to the console and has
// no way to reach the locator/URL fields added for the DX overlay. This sheet is
// reachable from a gear in the top bar and edits ONLY the DX-overlay keys — broker
// credentials are left untouched.

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/credential_store.dart';
import '../../dxspot/dxspot_service.dart';
import '../theme.dart';

/// Opens a modal dialog to edit the station locator and horstreporter URL, then
/// live-applies them to the running [DxSpotService] (configure + restart).
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
  bool _loading = true;

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
      _loading = false;
    });
  }

  Future<void> _save() async {
    final locator = _locator.text.trim().toUpperCase();
    final baseUrl = _url.text.trim();
    await _storage.writeAll({
      'station_locator': locator,
      'horstreporter_base_url': baseUrl,
    });
    if (!mounted) return;
    final dx = context.read<DxSpotService>();
    dx.configure(baseUrl: baseUrl, locator: locator);
    dx.restart();
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
                  Text('DX OVERLAY', style: AppTheme.display(16, weight: FontWeight.w700)),
                  const SizedBox(height: 4),
                  Text('Locator enables the compass DX-spot projection.',
                      style: AppTheme.body(11, color: AppTheme.txtMute)),
                  const SizedBox(height: 16),
                  _field('Station locator', _locator, hint: 'e.g. JN58sd'),
                  const SizedBox(height: 12),
                  _field('Horstreporter URL', _url, hint: 'https://…'),
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

  Widget _field(String label, TextEditingController controller, {String? hint}) {
    return TextField(
      controller: controller,
      style: AppTheme.mono(13),
      autocorrect: false,
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