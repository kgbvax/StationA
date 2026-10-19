import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/station_hosts.dart';
import 'package:hf_console/store/wiring.dart';

/// The array-of-tables entries of the portal inventory, each as its flat
/// `key = value` pairs (strings unquoted, arrays as lists of strings). A
/// minimal parser for the inventory's flat shape: one key per line, `#`
/// comments, single-line arrays.
List<(String, Map<String, Object>)> _tables(String toml) {
  final out = <(String, Map<String, Object>)>[];
  Map<String, Object>? cur;
  for (final raw in toml.split('\n')) {
    final line = raw.trim();
    if (line.isEmpty || line.startsWith('#')) continue;
    final table = RegExp(r'^\[\[\s*([a-z_]+)\s*\]\]').firstMatch(line);
    if (table != null) {
      cur = {};
      out.add((table.group(1)!, cur));
      continue;
    }
    if (line.startsWith('[')) {
      cur = null;
      continue;
    }
    final kv = RegExp(r'^([a-z_]+)\s*=\s*(.*)$').firstMatch(line);
    if (cur == null || kv == null) continue;
    final v = kv.group(2)!;
    cur[kv.group(1)!] = v.startsWith('[')
        ? RegExp(r'"([^"]*)"').allMatches(v).map((m) => m.group(1)!).toList()
        : RegExp(r'^"([^"]*)"').firstMatch(v)?.group(1) ?? v;
  }
  return out;
}

Iterable<Map<String, Object>> _of(List<(String, Map<String, Object>)> t, String name) =>
    t.where((e) => e.$1 == name).map((e) => e.$2);

void main() {
  final file = File('../stationportal/internal/inventory/inventory.toml');

  test('every slot the schematic draws has a host', () {
    for (final a in expectedSlots) {
      expect(slotHosts, contains(a), reason: '$a missing from slotHosts');
    }
  });

  test('slotHosts agrees with the stationportal inventory', () {
    final tables = _tables(file.readAsStringSync());
    final software = {
      for (final t in _of(tables, 'software'))
        t['name']! as String: (t['host'] as String? ?? '', (t['slots'] as List<String>?) ?? const <String>[]),
    };
    final slotTable = {for (final t in _of(tables, 'slot')) t['address']! as String: t['host'] as String? ?? ''};
    slotHosts.forEach((slot, sh) {
      // [[slot]].host: the same host, or `embedded` for firmware bridges.
      expect(slotTable[slot], sh.host, reason: '[[slot]] $slot in the inventory says host ${slotTable[slot]}');
      final entry = software[sh.component];
      expect(entry, isNotNull, reason: '${sh.component} is not in the inventory [[software]] table');
      final (host, slots) = entry!;
      expect(slots, contains(slot), reason: '${sh.component} does not list $slot in the inventory');
      if (sh.host == embeddedHost) {
        expect(stationHosts.keys, isNot(contains(host)),
            reason: '$slot runs on ${sh.component}\'s own firmware, inventory says host $host');
      } else {
        expect(host, sh.host, reason: '$slot: inventory runs ${sh.component} on $host');
      }
    });
  }, skip: file.existsSync() ? false : 'stationportal inventory not found (standalone checkout)');

  test('stationHosts addresses match the inventory [[host]] table', () {
    final hosts = {for (final t in _of(_tables(file.readAsStringSync()), 'host')) t['name']! as String: t['addr'] as String? ?? ''};
    stationHosts.forEach((name, h) {
      expect(hosts, contains(name), reason: '$name is not an inventory [[host]]');
      expect(hosts[name], startsWith(h.address), reason: '$name address');
    });
    for (final sh in slotHosts.values) {
      if (sh.host != embeddedHost) expect(stationHosts, contains(sh.host), reason: '${sh.component} runs on an undrawn host');
    }
  }, skip: file.existsSync() ? false : 'stationportal inventory not found (standalone checkout)');

  test('the parser sees comments, spacing and every table kind', () {
    final t = _tables('''
# a comment
[[software]]
name  = "x"   # trailing
host  = "scmino"
slots = ["muehle/a", "muehle/b"]
[ [ignored] ]
[[slot]]
address = "muehle/a"
host = "embedded"
''');
    expect(_of(t, 'software').single['slots'], ['muehle/a', 'muehle/b']);
    expect(_of(t, 'software').single['host'], 'scmino');
    expect(_of(t, 'slot').single['host'], 'embedded');
  });
}
