// vhfcam_transport_io.dart — native (Android / desktop / test) HTTP for the
// vhfcam preview server, via dart:io. One short-lived client per request: the
// polls are 2 s apart and the server is a LAN-only Go process; keep-alives
// would just add half-open-socket edge cases (see dxspot_source_io.dart for
// what those cost). Every failure mode — DNS, connect, timeout, non-200 —
// collapses into `false`/'' so the service owns the state machine and the UI
// owns the messaging.

import 'dart:async';
import 'dart:convert';
import 'dart:io';

Future<(bool, String)> vhfcamHttpGet(String url, Duration timeout) async {
  final client = HttpClient()..connectionTimeout = timeout;
  try {
    final req = await client.openUrl('GET', Uri.parse(url)).timeout(timeout);
    final resp = await req.close().timeout(timeout);
    final body = await resp.transform(utf8.decoder).join().timeout(timeout);
    return (resp.statusCode == 200, body);
  } catch (_) {
    return (false, '');
  } finally {
    client.close(force: true);
  }
}

Future<bool> vhfcamHttpPost(String url, Duration timeout) async {
  final client = HttpClient()..connectionTimeout = timeout;
  try {
    final req = await client.openUrl('POST', Uri.parse(url)).timeout(timeout);
    final resp = await req.close().timeout(timeout);
    await resp.drain<void>().timeout(timeout);
    return resp.statusCode == 200;
  } catch (_) {
    return false;
  } finally {
    client.close(force: true);
  }
}
