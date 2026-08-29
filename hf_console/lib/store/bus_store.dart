import 'dart:collection';
import 'dart:convert';
import 'package:clock/clock.dart';
import 'package:flutter/foundation.dart';

import 'wiring.dart' show expectedSlots;

class Slot {
  final String address;
  Map<String, dynamic>? meta;
  Map<String, dynamic>? state;
  String? status;
  Map<String, dynamic>? cmd;

  Slot(this.address);

  bool get bridgeOnline => status == 'online';
  bool get deviceOnline {
    // Physical-device bridges publish /state.device_online explicitly. Logic
    // slots (antenna-select, power-seq, hadiscovery) have no physical device
    // link and therefore omit the key; treat them as online once their state
    // snapshot has arrived.
    if (state == null) return false;
    if (!state!.containsKey('device_online')) return true;
    return state!['device_online'] == true;
  }

  bool get isOnline => bridgeOnline && deviceOnline;
}

/// One recorded fault or error seen on the bus.
class FaultRecord {
  final String address;
  final String text;
  String ts;
  bool active;

  FaultRecord({required this.address, required this.text, required this.ts, this.active = true});

  String get key => '$address|$text';
}

class BusStore extends ChangeNotifier {
  final Map<String, Slot> _slots = {};
  final _highFreq = <String, ValueNotifier<dynamic>>{};
  final List<FaultRecord> _faultHistory = [];

  /// When the first bus message of this session arrived. Silence reporting
  /// (expected slots never heard from) activates a grace period after it, so
  /// the console does not paint every slot red while retained states are
  /// still in flight on connect.
  DateTime? _firstMessageAt;
  static const _silenceGrace = Duration(seconds: 3);

  UnmodifiableMapView<String, Slot> get slots => UnmodifiableMapView(_slots);
  UnmodifiableListView<FaultRecord> get faultHistory => UnmodifiableListView(_faultHistory);

  ValueNotifier<T> hot<T>(String path, T initial) {
    return (_highFreq[path] ??= ValueNotifier<T>(initial)) as ValueNotifier<T>;
  }

  void apply(String topic, dynamic payload, bool retained) {
    _firstMessageAt ??= clock.now();
    final parts = topic.split('/');
    if (parts.isEmpty) return;
    final plane = parts.removeLast();
    final addr = parts.join('/');
    if (!const {'meta', 'state', 'status', 'cmd'}.contains(plane)) {
      return;
    }

    final slot = _slots.putIfAbsent(addr, () => Slot(addr));

    if (payload == null || (payload is String && payload.isEmpty)) {
      // cleared semantics: empty retained payload clears the plane
      switch (plane) {
        case 'meta':
          slot.meta = null;
        case 'state':
          slot.state = null;
        case 'status':
          slot.status = null;
        case 'cmd':
          slot.cmd = null;
      }
    } else {
      final value = _decodePayload(payload);
      switch (plane) {
        case 'meta':
          slot.meta = value as Map<String, dynamic>?;
        case 'state':
          slot.state = value as Map<String, dynamic>?;
          if (slot.state != null) {
            _updateHotValues(addr, slot.state!);
            _updateFaultHistory(addr, slot.state);
          }
        case 'status':
          slot.status = value as String?;
        case 'cmd':
          slot.cmd = value as Map<String, dynamic>?;
      }
    }
    notifyListeners();
  }

  /// Accept either a decoded object or a JSON string. The real broker path
  /// decodes before calling apply, but tests and direct callers may pass JSON.
  dynamic _decodePayload(dynamic payload) {
    if (payload is String) {
      try {
        return jsonDecode(payload);
      } catch (_) {
        return payload;
      }
    }
    return payload;
  }

  void _updateFaultHistory(String addr, Map<String, dynamic>? state) {
    final fault = _stringFrom(state?['fault']);
    final error = _stringFrom(state?['error']);
    final ts = _stringFrom(state?['ts']) ?? DateTime.now().toIso8601String();
    final activeText = _activeFaultText(fault, error);

    // Mark any previously active record for this address as cleared if it no
    // longer matches the current active fault text.
    for (final r in _faultHistory) {
      if (r.address == addr && r.active && r.text != activeText) {
        r.active = false;
      }
    }

    if (activeText.isEmpty) return;

    final key = '$addr|$activeText';
    final existing = _faultHistory.where((r) => r.key == key).lastOrNull;
    if (existing != null) {
      existing.active = true;
      // Refresh the timestamp so a repeated fault stays at the top of the
      // sorted list and signals the problem is still present.
      existing.ts = ts;
      return;
    }

    _faultHistory.add(FaultRecord(address: addr, text: activeText, ts: ts));
    if (_faultHistory.length > 30) {
      _faultHistory.removeAt(0);
    }
  }

  String? _stringFrom(dynamic v) {
    if (v == null) return null;
    if (v is String) return v.trim();
    if (v is num || v is bool) return v.toString();
    return v.toString().trim();
  }

  String _activeFaultText(String? fault, String? error) {
    final hasFault = fault != null && fault.isNotEmpty && fault != 'none';
    final hasError = error != null && error.isNotEmpty && error.toUpperCase() != 'NONE';
    if (hasError) {
      return error.toUpperCase();
    }
    if (hasFault) {
      return fault.toUpperCase();
    }
    return '';
  }

  void _updateHotValues(String addr, Map<String, dynamic> state) {
    final prefix = '$addr.';
    state.forEach((k, v) {
      final path = '$prefix$k';
      if (_highFreq.containsKey(path)) {
        _highFreq[path]!.value = v;
      }
    });
  }

  List<String> get offlineList {
    final out = <String>[];
    for (final s in _slots.values) {
      if (s.status != 'online') {
        out.add('${s.address}: bridge down');
      } else if (!s.deviceOnline) {
        out.add('${s.address}: device unreachable');
      }
    }
    // Expected slots this session has heard nothing from at all — the service
    // is down, or was never deployed since the console started. Reported only
    // after the grace period so connect-time silence doesn't trip it.
    final first = _firstMessageAt;
    if (first != null && clock.now().difference(first) > _silenceGrace) {
      for (final addr in expectedSlots) {
        if (!_slots.containsKey(addr)) {
          out.add('$addr: silent (no state since connect)');
        }
      }
    }
    return out;
  }

  dynamic stateValue(String address, String key) =>
      _slots[address]?.state?[key];

  T? stateValueAs<T>(String address, String key) {
    final v = stateValue(address, key);
    if (v is T) return v;
    return null;
  }
}
