import 'dart:collection';
import 'package:flutter/foundation.dart';

class Slot {
  final String address;
  Map<String, dynamic>? meta;
  Map<String, dynamic>? state;
  String? status;
  Map<String, dynamic>? cmd;

  Slot(this.address);

  bool get bridgeOnline => status == 'online';
  bool get deviceOnline => (state?['device_online'] ?? false) == true;
  bool get isOnline => bridgeOnline && deviceOnline;
}

/// One recorded fault or error seen on the bus.
class FaultRecord {
  final String address;
  final String text;
  final String ts;
  bool active;

  FaultRecord({required this.address, required this.text, required this.ts, this.active = true});

  String get key => '$address|$text';
}

class BusStore extends ChangeNotifier {
  final Map<String, Slot> _slots = {};
  final _highFreq = <String, ValueNotifier<dynamic>>{};
  final List<FaultRecord> _faultHistory = [];

  UnmodifiableMapView<String, Slot> get slots => UnmodifiableMapView(_slots);
  UnmodifiableListView<FaultRecord> get faultHistory => UnmodifiableListView(_faultHistory);

  ValueNotifier<T> hot<T>(String path, T initial) {
    return (_highFreq[path] ??= ValueNotifier<T>(initial)) as ValueNotifier<T>;
  }

  void apply(String topic, dynamic payload, bool retained) {
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
      switch (plane) {
        case 'meta':
          slot.meta = payload as Map<String, dynamic>?;
        case 'state':
          slot.state = payload as Map<String, dynamic>?;
          _updateHotValues(addr, slot.state!);
          _updateFaultHistory(addr, slot.state);
        case 'status':
          slot.status = payload as String?;
        case 'cmd':
          slot.cmd = payload as Map<String, dynamic>?;
      }
    }
    notifyListeners();
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
    if (error != null && error.isNotEmpty) {
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
