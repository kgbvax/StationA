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

class BusStore extends ChangeNotifier {
  final Map<String, Slot> _slots = {};
  final _highFreq = <String, ValueNotifier<dynamic>>{};

  UnmodifiableMapView<String, Slot> get slots => UnmodifiableMapView(_slots);

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
        case 'status':
          slot.status = payload as String?;
        case 'cmd':
          slot.cmd = payload as Map<String, dynamic>?;
      }
    }
    notifyListeners();
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
