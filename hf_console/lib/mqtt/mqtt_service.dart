import 'dart:async';
import 'dart:convert';
import 'package:flutter/foundation.dart';
import 'package:mqtt_client/mqtt_client.dart';
import 'package:typed_data/typed_data.dart';
import '../store/bus_store.dart';
import 'client_factory.dart';

/// MQTT link to the shack broker.
///
/// Robustness model (verified against the mqtt_client 10.11.11 source):
/// - The library's autoReconnect only arms after a FIRST successful connect
///   and can wedge permanently if an exception escapes its async loop
///   (autoReconnectInProgress never clears, and the guard then blocks even
///   manual doAutoReconnect). Every recovery path here therefore ends in a
///   FRESH client rather than trusting the library's retry.
/// - Android display-off/doze freezes timers and kills the TCP session
///   broker-side, but the app-side socket can survive half-open with
///   connectionStatus still 'connected' — the library has no pong watchdog
///   unless disconnectOnNoResponsePeriod is set. We arm that watchdog and
///   track the last PINGRESP ourselves so a resume can detect the zombie.
class MqttService {
  final BusStore store;
  final ValueNotifier<bool> connected = ValueNotifier(false);
  MqttClient? _client;
  StreamSubscription? _updates;

  // Last connect parameters — reconnect paths (lifecycle resume, retry
  // timer) run without UI interaction, so they must be cached here.
  String? _host;
  int? _port;
  String? _username;
  String? _password;

  bool _connecting = false; // in-flight guard: overlapping connects throw StateError
  DateTime? _lastPongAt; // last proof of a live broker round-trip
  Timer? _retryTimer; // backoff retry; covers failed initial connects and wedged clients
  int _retryDelay = _initialRetryDelay; // seconds, doubles up to _maxRetryDelay
  int _connectingTicks = 0; // consecutive ticks finding the library mid-(re)connect
  bool _paused = false; // lifecycle: no live client may outlive onAppPaused
  bool _disposed = false;

  static const _initialRetryDelay = 5;
  static const _maxRetryDelay = 60;
  static const _keepAlive = 20; // s, PINGREQ cadence
  static const _noResponsePeriod = 10; // s without PINGRESP -> hard disconnect, arms autoReconnect
  // A link is considered live if state==connected AND a pong arrived within
  // this window (one keep-alive cycle plus the pong watchdog).
  static const _livenessWindow = Duration(seconds: _keepAlive + _noResponsePeriod);

  MqttService(this.store);

  bool get _hasParams =>
      _host != null && _port != null && _username != null && _password != null;

  Future<void> connect({
    required String host,
    required int port,
    required String username,
    required String password,
    required String clientId,
  }) async {
    _host = host;
    _port = port;
    _username = username;
    _password = password;
    await _connectFresh(clientId: clientId);
  }

  /// Lifecycle: app resumed (screen unlocked with the app frontmost).
  /// Reconnects if the link is down or provably dead — but leaves a
  /// demonstrably live link alone, so transient inactive/resume cycles
  /// (notification shade pulls, quick app switches) don't churn it.
  void onAppResumed() {
    _paused = false;
    _ensureConnected();
  }

  /// Lifecycle: app backgrounded. Tears the link down cleanly so the broker
  /// sees a disconnect instead of a half-open corpse, and stops retries —
  /// there is nothing useful to reconnect to while nobody is looking.
  /// A connect already in flight is torn down by its own path via _paused.
  void onAppPaused() {
    _paused = true;
    _retryTimer?.cancel();
    _retryTimer = null;
    _connectingTicks = 0;
    _teardownClient();
    connected.value = false;
    store.markDisconnected();
  }

  /// Reconnect unless the link is demonstrably alive. All recovery paths
  /// converge here; a fresh client is built whenever in doubt, because the
  /// library cannot recover from a wedged auto-reconnect loop and a
  /// doze-killed socket can still report state==connected.
  void _ensureConnected() {
    if (_connecting || !_hasParams) return;
    final state = _client?.connectionStatus?.state;
    if (state == MqttConnectionState.connected && _pongFresh()) return;
    _connectFresh();
  }

  bool _pongFresh() {
    final last = _lastPongAt;
    return last != null && DateTime.now().difference(last) < _livenessWindow;
  }

  /// Full recovery: tear the old client down, build a fresh one, connect,
  /// subscribe, and re-listen. Never throws to the caller — failures are
  /// scheduled for retry instead.
  Future<void> _connectFresh({String? clientId}) async {
    if (_connecting || !_hasParams) {
      // A connect is already in flight with the previously cached params —
      // defer to the retry timer so changed parameters still take effect.
      if (_hasParams) _scheduleRetry();
      return;
    }
    _connecting = true;
    try {
      _cancelUpdates();
      _teardownClient();
      // The old client's callbacks are gone with it; the link is down until
      // the fresh client's onConnected fires (so a failed swap cannot leave
      // the indicator green over a dead link).
      connected.value = false;
      store.markDisconnected();
      final id = clientId ?? 'hf-console-${DateTime.now().millisecondsSinceEpoch}';
      final client = createMqttClient(_host!, _port!, id);
      _configure(client, id);
      try {
        await client.connect();
      } catch (_) {
        // Failed initial connects never arm the library's autoReconnect —
        // only our own retry brings the link back from here.
        if (!_disposed && !_paused) _scheduleRetry();
        return;
      }
      if (_disposed) {
        _detach(client);
        client.disconnect();
        return;
      }
      if (_paused) {
        // onAppPaused arrived mid-connect: the fresh client must not
        // outlive the pause — no live link while nobody is looking.
        _detach(client);
        client.disconnect();
        connected.value = false;
        store.markDisconnected();
        return;
      }
      _client = client;
      // The CONNACK round-trip proves the link; pongs keep the proof fresh.
      _lastPongAt = DateTime.now();
      _retryTimer?.cancel();
      _retryTimer = null;
      _retryDelay = _initialRetryDelay;
      _connectingTicks = 0;
      _updates = client.updates!.listen((List<MqttReceivedMessage> messages) {
        for (final msg in messages) {
          final rec = msg.payload as MqttPublishMessage;
          _apply(msg.topic, rec.payload.message, rec.header!.retain);
        }
      });
    } finally {
      _connecting = false;
    }
  }

  void _configure(MqttClient client, String clientId) {
    client.keepAlivePeriod = _keepAlive;
    // Pong watchdog: an unanswered keep-alive hard-disconnects and hands the
    // recovery to autoReconnect. Without this the client sits 'connected'
    // on a dead socket indefinitely (default 0 = disabled).
    client.disconnectOnNoResponsePeriod = _noResponsePeriod;
    client.autoReconnect = true;
    client.resubscribeOnAutoReconnect = true;
    client.pongCallback = () {
      if (!_disposed) _lastPongAt = DateTime.now();
    };
    client.onConnected = () {
      if (_disposed || _paused) return;
      connected.value = true;
      store.markConnected();
      // Fires on every CONNACK, auto-reconnects included; subscribing is
      // idempotent in the library.
      client.subscribe('muehle/#', MqttQos.atMostOnce);
    };
    client.onDisconnected = () {
      if (_disposed) return;
      connected.value = false;
      store.markDisconnected();
      // Nothing retries from here — the library takes the AutoReconnect
      // path on unsolicited drops, and its auto-reconnect never arms before
      // a first successful connect. Our own retry must own the recovery.
      if (!_paused) _scheduleRetry();
    };
    client.onAutoReconnect = () {
      if (_disposed) return;
      connected.value = false;
      store.markDisconnected();
      // Watchdog over the library's own reconnect loop: it retries forever
      // normally (the tick then sees connected + a fresh pong and stays
      // quiet), but an escaping exception wedges it with no public reset —
      // the tick force-recreates the client after a few tries.
      if (!_paused) _scheduleRetry();
    };
    client.onAutoReconnected = () {
      if (_disposed || _paused) return;
      connected.value = true;
      store.markConnected();
      _lastPongAt = DateTime.now();
    };
    client.connectionMessage = MqttConnectMessage()
        .withClientIdentifier(clientId)
        .authenticateAs(_username, _password)
        .startClean();
  }

  void _scheduleRetry() {
    _retryTimer?.cancel();
    if (!_hasParams) return;
    final delay = _retryDelay;
    _retryDelay = _retryDelay * 2 > _maxRetryDelay ? _maxRetryDelay : _retryDelay * 2;
    _retryTimer = Timer(Duration(seconds: delay), () {
      _retryTimer = null;
      _tickRetry();
    });
  }

  void _tickRetry() {
    if (_connecting) {
      _scheduleRetry();
      return;
    }
    final state = _client?.connectionStatus?.state;
    if (state == MqttConnectionState.connected && _pongFresh()) {
      // The library healed the link on its own.
      _connectingTicks = 0;
      _retryDelay = _initialRetryDelay;
      return;
    }
    if (state == MqttConnectionState.connecting) {
      // The library is mid-(auto)reconnect — let it work. But never trust
      // 'connecting' forever: a wedged auto-reconnect loop stays here, so
      // force a fresh client after a few consecutive ticks.
      _connectingTicks++;
      if (_connectingTicks < 4) {
        _scheduleRetry();
        return;
      }
    }
    _connectingTicks = 0;
    _retryDelay = _initialRetryDelay;
    _connectFresh();
  }

  void _cancelUpdates() {
    _updates?.cancel();
    _updates = null;
  }

  void _teardownClient() {
    final old = _client;
    _client = null;
    if (old == null) return;
    _detach(old);
    old.disconnect();
  }

  /// Detach callbacks so a racing callback cannot flip `connected` for the
  /// new client; disconnect() then destroys the connection handler (which
  /// holds its own copies of these closures) and stops any in-flight
  /// auto-reconnect loop.
  void _detach(MqttClient client) {
    client.onConnected = null;
    client.onDisconnected = null;
    client.onAutoReconnect = null;
    client.onAutoReconnected = null;
    client.pongCallback = null;
  }

  void _apply(String topic, Uint8Buffer bytes, bool retained) {
    final text = utf8.decode(bytes.toList(), allowMalformed: true);
    if (text.isEmpty) {
      store.apply(topic, null, retained);
      return;
    }
    dynamic payload;
    try {
      payload = jsonDecode(text);
    } catch (_) {
      payload = text;
    }
    store.apply(topic, payload, retained);
  }

  /// Publishes a command. Returns false (and publishes nothing) when the
  /// link is down — callers that care about delivery must check.
  bool publish(String topic, String payload,
      {required bool retain, MqttQos qos = MqttQos.atLeastOnce}) {
    final client = _client;
    if (client == null || client.connectionStatus!.state != MqttConnectionState.connected) {
      return false;
    }
    final builder = MqttClientPayloadBuilder();
    builder.addString(payload);
    client.publishMessage(topic, qos, builder.payload!, retain: retain);
    return true;
  }

  void clear(String topic) {
    publish(topic, '', retain: true, qos: MqttQos.atLeastOnce);
  }

  void dispose() {
    _disposed = true;
    _retryTimer?.cancel();
    _cancelUpdates();
    _teardownClient();
    connected.dispose();
  }
}