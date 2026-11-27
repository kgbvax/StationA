/// The automatic digimode operator, the logic slot `muehle/hf/auto-op` of
/// 75auto on bwpc. This is the console side of the contract
/// `auto-op-mqtt-api.md` in the 75auto repository.
///
/// 75auto publishes the view of its session as one retained `/state`
/// document, a small `/beat` between two documents, a retained `/meta`, and
/// a retained `/status`. It takes signed one-shot commands on `/cmd`.
///
/// Reading rules:
///
/// - Each key is read with a type guard. A missing value or a value of the
///   wrong type gives null. The parser never puts a default such as "auto"
///   or false in the place of an unknown value.
/// - The booleans of a decode are left out when they are false (contract
///   §5.3). There a missing boolean reads as false.
/// - A document whose structure is wrong (an object that is not an object,
///   a list that is not a list, or a list entry that is not an object) is
///   malformed. [AutoOpView.fromState] then returns null, and [BusStore]
///   keeps the last good view and counts the payload in
///   `malformedPayloads`.
/// - `ts` is never compared with the clock of the phone. Freshness is the
///   business of [BusStore] (`stateFresh`, `stateAge`).
library;

import 'dart:convert';
import 'dart:math';
import 'dart:typed_data';

import 'package:crypto/crypto.dart';

import 'wiring.dart' show antModeTs;

const autoOpSlot = 'muehle/hf/auto-op';

/// The modes of a session, as 75auto names them.
const autoOpModes = ['observe', 'confirm', 'auto'];

/// The values of `capabilities.commands` in `/meta`.
const autoOpCommandLevels = {'none', 'stop', 'operate'};

// --- Type guards ---------------------------------------------------------------

String? _str(dynamic v) => v is String ? v : null;

bool? _bool(dynamic v) => v is bool ? v : null;

/// An integer, or null. A JSON number with a fraction is not an integer.
int? _int(dynamic v) {
  if (v is int) return v;
  if (v is double && v.isFinite && v == v.roundToDouble()) return v.toInt();
  return null;
}

/// Thrown inside the parser for a document with a wrong structure. It never
/// leaves this file: [AutoOpView.fromState] turns it into null.
class _Malformed implements Exception {
  const _Malformed();
}

/// The object at [v]. Null when [v] is null or missing. Malformed when [v]
/// is something other than an object.
Map<String, dynamic>? _obj(dynamic v) {
  if (v == null) return null;
  if (v is Map<String, dynamic>) return v;
  throw const _Malformed();
}

/// The list of objects at [v], each parsed by [each]. Null when [v] is null
/// or missing. Malformed when [v] is not a list, or when an entry is not an
/// object.
List<T>? _list<T>(dynamic v, T Function(Map<String, dynamic>) each) {
  if (v == null) return null;
  if (v is! List) throw const _Malformed();
  return [
    for (final e in v)
      if (e is Map<String, dynamic>) each(e) else throw const _Malformed(),
  ];
}

// --- /state ----------------------------------------------------------------------

/// `session` of the document.
class AutoOpSession {
  /// `not-started`, `connecting`, `running`, or `ended`.
  final String? state;
  final String? ruleSet;
  final String? level;

  /// `observe`, `confirm`, or `auto`. Empty before the first start.
  final String? mode;

  /// `live` or `replay`.
  final String? source;

  const AutoOpSession({this.state, this.ruleSet, this.level, this.mode, this.source});

  factory AutoOpSession._parse(Map<String, dynamic> m) => AutoOpSession(
        state: _str(m['state']),
        ruleSet: _str(m['rule_set']),
        level: _str(m['level']),
        mode: _str(m['mode']),
        source: _str(m['source']),
      );
}

/// `station.band_change`: a band change in progress.
class AutoOpBandChange {
  final String? target;
  final String? result;

  const AutoOpBandChange({this.target, this.result});

  factory AutoOpBandChange._parse(Map<String, dynamic> m) =>
      AutoOpBandChange(target: _str(m['target']), result: _str(m['result']));
}

/// `station` of the document.
class AutoOpStation {
  final String? band;
  final int? dialHz;
  final String? mode;
  final String? dxCall;
  final String? dxEntity;

  /// The operator name and the picture URL of the DX call from the
  /// callbook (QRZ.com), and the distance to it in km. Empty or 0 when
  /// 75auto does not know them.
  final String? dxName;
  final String? dxImage;
  final int? dxKm;
  final int? txDf;
  final int? rxDf;
  final bool? txEnabled;
  final bool? transmitting;
  final int? periodMs;

  /// The time into the period when 75auto published the document. The page
  /// moves it on with its own clock.
  final int? slotOffsetMs;
  final AutoOpBandChange? bandChange;
  final int? decodeCount;
  final int? lastDecodeEndMs;

  /// `CALL`, `RPT`, `R+RPT`, `RR73`, `LOG`, or empty.
  final String? qsoStage;
  final int? tryCount;
  final int? repeatCount;

  /// `even`, `odd`, or empty before the first transmission.
  final String? txParity;

  /// `follow` or `skip` for the DX call, else empty.
  final String? targetChoice;

  const AutoOpStation({
    this.band,
    this.dialHz,
    this.mode,
    this.dxCall,
    this.dxEntity,
    this.dxName,
    this.dxImage,
    this.dxKm,
    this.txDf,
    this.rxDf,
    this.txEnabled,
    this.transmitting,
    this.periodMs,
    this.slotOffsetMs,
    this.bandChange,
    this.decodeCount,
    this.lastDecodeEndMs,
    this.qsoStage,
    this.tryCount,
    this.repeatCount,
    this.txParity,
    this.targetChoice,
  });

  factory AutoOpStation._parse(Map<String, dynamic> m) {
    final bc = _obj(m['band_change']);
    return AutoOpStation(
      band: _str(m['band']),
      dialHz: _int(m['dial_hz']),
      mode: _str(m['mode']),
      dxCall: _str(m['dx_call']),
      dxEntity: _str(m['dx_entity']),
      dxName: _str(m['dx_name']),
      dxImage: _str(m['dx_image']),
      dxKm: _int(m['dx_km']),
      txDf: _int(m['tx_df']),
      rxDf: _int(m['rx_df']),
      txEnabled: _bool(m['tx_enabled']),
      transmitting: _bool(m['transmitting']),
      periodMs: _int(m['period_ms']),
      slotOffsetMs: _int(m['slot_offset_ms']),
      bandChange: bc == null ? null : AutoOpBandChange._parse(bc),
      decodeCount: _int(m['decode_count']),
      lastDecodeEndMs: _int(m['last_decode_end_ms']),
      qsoStage: _str(m['qso_stage']),
      tryCount: _int(m['try_count']),
      repeatCount: _int(m['repeat_count']),
      txParity: _str(m['tx_parity']),
      targetChoice: _str(m['target_choice']),
    );
  }
}

/// `pending`: the proposal that waits for Approve or Reject.
class AutoOpPending {
  final int? id;
  final String? rule;
  final String? action;
  final String? call;
  final String? band;
  final String? reason;
  final int? priority;
  final int? deadlineMs;
  final bool? expired;

  const AutoOpPending({
    this.id,
    this.rule,
    this.action,
    this.call,
    this.band,
    this.reason,
    this.priority,
    this.deadlineMs,
    this.expired,
  });

  factory AutoOpPending._parse(Map<String, dynamic> m) => AutoOpPending(
        id: _int(m['id']),
        rule: _str(m['rule']),
        action: _str(m['action']),
        call: _str(m['call']),
        band: _str(m['band']),
        reason: _str(m['reason']),
        priority: _int(m['priority']),
        deadlineMs: _int(m['deadline_ms']),
        expired: _bool(m['expired']),
      );
}

/// One line of the action log, and `current`.
class AutoOpLogLine {
  final int? id;
  final int? timeMs;
  final String? rule;
  final String? action;
  final String? call;
  final String? band;
  final String? result;
  final String? cause;
  final String? gateReason;
  final String? reason;

  const AutoOpLogLine({
    this.id,
    this.timeMs,
    this.rule,
    this.action,
    this.call,
    this.band,
    this.result,
    this.cause,
    this.gateReason,
    this.reason,
  });

  factory AutoOpLogLine._parse(Map<String, dynamic> m) => AutoOpLogLine(
        id: _int(m['id']),
        timeMs: _int(m['time_ms']),
        rule: _str(m['rule']),
        action: _str(m['action']),
        call: _str(m['call']),
        band: _str(m['band']),
        result: _str(m['result']),
        cause: _str(m['cause']),
        gateReason: _str(m['gate_reason']),
        reason: _str(m['reason']),
      );
}

/// One block of the rules in `idle.blocks`.
class AutoOpIdleBlock {
  final String? call;
  final String? why;

  const AutoOpIdleBlock({this.call, this.why});

  factory AutoOpIdleBlock._parse(Map<String, dynamic> m) => AutoOpIdleBlock(call: _str(m['call']), why: _str(m['why']));
}

/// `idle`: why the latest cycle chose no proposal.
class AutoOpIdle {
  final int? proposed;
  final List<AutoOpIdleBlock>? blocks;
  final int? moreBlocks;

  const AutoOpIdle({this.proposed, this.blocks, this.moreBlocks});

  factory AutoOpIdle._parse(Map<String, dynamic> m) => AutoOpIdle(
        proposed: _int(m['proposed']),
        blocks: _list(m['blocks'], AutoOpIdleBlock._parse),
        moreBlocks: _int(m['more_blocks']),
      );
}

/// `arm`: the arm of CQ calls.
class AutoOpArm {
  final String? source;
  final int? untilMs;

  const AutoOpArm({this.source, this.untilMs});

  factory AutoOpArm._parse(Map<String, dynamic> m) => AutoOpArm(source: _str(m['source']), untilMs: _int(m['until_ms']));
}

/// One decode of `decodes[]`. 75auto leaves out the booleans that are
/// false, so a missing boolean reads as false here (contract §5.3).
class AutoOpDecode {
  final int? periodStartMs;
  final int? df;
  final int? snr;

  /// Decode text from the air. Show it as plain text only.
  final String? text;
  final String? call;
  final bool needed;
  final bool cq;
  final bool toMe;
  final bool latest;

  const AutoOpDecode({
    this.periodStartMs,
    this.df,
    this.snr,
    this.text,
    this.call,
    this.needed = false,
    this.cq = false,
    this.toMe = false,
    this.latest = false,
  });

  factory AutoOpDecode._parse(Map<String, dynamic> m) => AutoOpDecode(
        periodStartMs: _int(m['period_start_ms']),
        df: _int(m['df']),
        snr: _int(m['snr']),
        text: _str(m['text']),
        call: _str(m['call']),
        needed: m['needed'] == true,
        cq: m['cq'] == true,
        toMe: m['to_me'] == true,
        latest: m['latest'] == true,
      );
}

/// One transmitted period of `tx_lines[]`.
class AutoOpTxLine {
  final int? periodStartMs;
  final String? band;
  final int? df;
  final String? text;

  const AutoOpTxLine({this.periodStartMs, this.band, this.df, this.text});

  factory AutoOpTxLine._parse(Map<String, dynamic> m) => AutoOpTxLine(
        periodStartMs: _int(m['period_start_ms']),
        band: _str(m['band']),
        df: _int(m['df']),
        text: _str(m['text']),
      );
}

/// The latest decode count of one band, in `bands[]`.
class AutoOpBandCount {
  final String? band;
  final int? dialHz;

  /// False for a band without a batch in this session.
  final bool? seen;
  final int? count;
  final int? periodStartMs;

  /// The points of the band from the last band survey of 75auto. Null
  /// without a survey, without award points, or while the survey is stale.
  final int? value;

  const AutoOpBandCount({this.band, this.dialHz, this.seen, this.count, this.periodStartMs, this.value});

  factory AutoOpBandCount._parse(Map<String, dynamic> m) => AutoOpBandCount(
        band: _str(m['band']),
        dialHz: _int(m['dial_hz']),
        seen: _bool(m['seen']),
        count: _int(m['count']),
        periodStartMs: _int(m['period_start_ms']),
        value: _int(m['value']),
      );
}

/// The health of one driver interface, in `health[]`.
class AutoOpHealth {
  final String? interface;
  final String? name;
  final bool? fresh;
  final String? reason;

  const AutoOpHealth({this.interface, this.name, this.fresh, this.reason});

  factory AutoOpHealth._parse(Map<String, dynamic> m) => AutoOpHealth(
        interface: _str(m['interface']),
        name: _str(m['name']),
        fresh: _bool(m['fresh']),
        reason: _str(m['reason']),
      );
}

/// `rig`: the health of the rig server.
class AutoOpRig {
  final bool? present;
  final bool? ok;
  final String? name;
  final String? reason;

  const AutoOpRig({this.present, this.ok, this.name, this.reason});

  factory AutoOpRig._parse(Map<String, dynamic> m) =>
      AutoOpRig(present: _bool(m['present']), ok: _bool(m['ok']), name: _str(m['name']), reason: _str(m['reason']));
}

/// `remote.last_cmd`: the result of the latest command.
class AutoOpCmdResult {
  final String? cmdId;
  final String? action;

  /// `accepted` or `refused`.
  final String? result;
  final String? reason;
  final int? atMs;

  const AutoOpCmdResult({this.cmdId, this.action, this.result, this.reason, this.atMs});

  factory AutoOpCmdResult._parse(Map<String, dynamic> m) => AutoOpCmdResult(
        cmdId: _str(m['cmd_id']),
        action: _str(m['action']),
        result: _str(m['result']),
        reason: _str(m['reason']),
        atMs: _int(m['at_ms']),
      );
}

/// `remote`: the state of the remote command path.
class AutoOpRemote {
  /// `none`, `stop`, or `operate`.
  final String? commands;
  final AutoOpCmdResult? lastCmd;

  const AutoOpRemote({this.commands, this.lastCmd});

  factory AutoOpRemote._parse(Map<String, dynamic> m) {
    final last = _obj(m['last_cmd']);
    return AutoOpRemote(commands: _str(m['commands']), lastCmd: last == null ? null : AutoOpCmdResult._parse(last));
  }
}

/// The whole `/state` document of the slot.
class AutoOpView {
  /// RFC 3339 UTC time of the publish, on the clock of bwpc. Show it, but
  /// never compare it with the clock of the phone.
  final String? ts;
  final int? seq;

  /// The time of the view in Unix ms. In a replay this is the replay time.
  final int? nowMs;
  final AutoOpSession? session;
  final AutoOpStation? station;
  final int? presenceEndMs;
  final AutoOpPending? pending;
  final AutoOpLogLine? current;
  final AutoOpIdle? idle;
  final String? hint;
  final AutoOpArm? arm;
  final bool? latched;
  final bool? stopUnconfirmed;
  final String? stopReason;
  final String? cycleError;
  final List<AutoOpDecode>? decodes;
  final List<AutoOpTxLine>? txLines;
  final List<AutoOpBandCount>? bands;
  final List<AutoOpLogLine>? log;
  final List<AutoOpHealth>? health;
  final AutoOpRig? rig;
  final bool? clockFailing;
  final AutoOpRemote? remote;
  final String? error;

  const AutoOpView({
    this.ts,
    this.seq,
    this.nowMs,
    this.session,
    this.station,
    this.presenceEndMs,
    this.pending,
    this.current,
    this.idle,
    this.hint,
    this.arm,
    this.latched,
    this.stopUnconfirmed,
    this.stopReason,
    this.cycleError,
    this.decodes,
    this.txLines,
    this.bands,
    this.log,
    this.health,
    this.rig,
    this.clockFailing,
    this.remote,
    this.error,
  });

  /// Parses a `/state` document. Returns null for a malformed document. It
  /// never throws.
  static AutoOpView? fromState(Map<String, dynamic> s) {
    try {
      final session = _obj(s['session']);
      final station = _obj(s['station']);
      final pending = _obj(s['pending']);
      final current = _obj(s['current']);
      final idle = _obj(s['idle']);
      final arm = _obj(s['arm']);
      final rig = _obj(s['rig']);
      final remote = _obj(s['remote']);
      return AutoOpView(
        ts: _str(s['ts']),
        seq: _int(s['seq']),
        nowMs: _int(s['now_ms']),
        session: session == null ? null : AutoOpSession._parse(session),
        station: station == null ? null : AutoOpStation._parse(station),
        presenceEndMs: _int(s['presence_end_ms']),
        pending: pending == null ? null : AutoOpPending._parse(pending),
        current: current == null ? null : AutoOpLogLine._parse(current),
        idle: idle == null ? null : AutoOpIdle._parse(idle),
        hint: _str(s['hint']),
        arm: arm == null ? null : AutoOpArm._parse(arm),
        latched: _bool(s['latched']),
        stopUnconfirmed: _bool(s['stop_unconfirmed']),
        stopReason: _str(s['stop_reason']),
        cycleError: _str(s['cycle_error']),
        decodes: _list(s['decodes'], AutoOpDecode._parse),
        txLines: _list(s['tx_lines'], AutoOpTxLine._parse),
        bands: _list(s['bands'], AutoOpBandCount._parse),
        log: _list(s['log'], AutoOpLogLine._parse),
        health: _list(s['health'], AutoOpHealth._parse),
        rig: rig == null ? null : AutoOpRig._parse(rig),
        clockFailing: _bool(s['clock_failing']),
        remote: remote == null ? null : AutoOpRemote._parse(remote),
        error: _str(s['error']),
      );
    } on _Malformed {
      return null;
    }
  }
}

// --- /beat -----------------------------------------------------------------------

/// One `/beat`: the small heartbeat between two `/state` documents.
class AutoOpBeat {
  final String? ts;

  /// The `seq` of the latest `/state`. A value above the `seq` that the page
  /// has tells that a `/state` was lost.
  final int? stateSeq;
  final int? nowMs;
  final int? slotOffsetMs;

  const AutoOpBeat({this.ts, this.stateSeq, this.nowMs, this.slotOffsetMs});

  static AutoOpBeat fromBeat(Map<String, dynamic> b) => AutoOpBeat(
        ts: _str(b['ts']),
        stateSeq: _int(b['state_seq']),
        nowMs: _int(b['now_ms']),
        slotOffsetMs: _int(b['slot_offset_ms']),
      );
}

// --- /meta -----------------------------------------------------------------------

/// The `/meta` birth certificate of the slot.
class AutoOpMeta {
  final String? host;
  final String? location;

  /// `none`, `stop`, or `operate`. Null when missing or when the value is
  /// not one of these.
  final String? commands;

  /// The bands of the band table, in dial order. Null when missing.
  final List<String>? bands;
  final bool? signed;

  const AutoOpMeta({this.host, this.location, this.commands, this.bands, this.signed});

  static AutoOpMeta fromMeta(Map<String, dynamic> m) {
    final caps = m['capabilities'];
    final c = caps is Map<String, dynamic> ? caps : const <String, dynamic>{};
    final cmds = _str(c['commands']);
    final bands = c['bands'];
    return AutoOpMeta(
      host: _str(m['host']),
      location: _str(m['location']),
      commands: autoOpCommandLevels.contains(cmds) ? cmds : null,
      bands: bands is List && bands.every((b) => b is String) ? List<String>.unmodifiable(bands) : null,
      signed: _bool(c['signed']),
    );
  }
}

// --- /cmd ------------------------------------------------------------------------

/// The length of the command key in bytes.
const autoOpKeyBytes = 32;

/// Parses the command key from its text form: exactly 64 hex digits, upper
/// or lower case, with spaces around it allowed. Returns null for any other
/// text.
Uint8List? parseAutoOpKey(String text) {
  final t = text.trim();
  if (!RegExp(r'^[0-9a-fA-F]{64}$').hasMatch(t)) return null;
  final out = Uint8List(autoOpKeyBytes);
  for (var i = 0; i < autoOpKeyBytes; i++) {
    out[i] = int.parse(t.substring(2 * i, 2 * i + 2), radix: 16);
  }
  return out;
}

/// The text that the signature covers (contract §7.2):
/// `{"action":…,"value":…,"ts":…,"cmd_id":…}`, keys in this order, no
/// spaces. [valueJson] is the compact JSON of the value, or `null`.
String autoOpSignedText(String action, String valueJson, String ts, String cmdId) =>
    '{"action":${jsonEncode(action)},"value":$valueJson,"ts":${jsonEncode(ts)},"cmd_id":${jsonEncode(cmdId)}}';

/// The lowercase hex of HMAC-SHA256 with [key] over [autoOpSignedText].
String signAutoOpCmd(List<int> key, String action, String valueJson, String ts, String cmdId) =>
    Hmac(sha256, key).convert(utf8.encode(autoOpSignedText(action, valueJson, ts, cmdId))).toString();

/// A random command id: 16 lowercase hex digits from a secure source.
String newAutoOpCmdId([Random? random]) {
  final r = random ?? Random.secure();
  return List.generate(8, (_) => r.nextInt(256).toRadixString(16).padLeft(2, '0')).join();
}

/// One signed command for `/cmd` of the slot, as a JSON text:
/// `{action, value, ts, cmd_id, sig}`. Publish it not retained.
///
/// [value] is the value of the action, or null. Its JSON text in the
/// payload and in the signed text is the same, so a map keeps its key
/// order in both. [now] and [cmdId] are for tests.
String autoOpCmd(String action, Object? value, {required List<int> key, DateTime? now, String? cmdId}) {
  final ts = antModeTs(now);
  final id = cmdId ?? newAutoOpCmdId();
  final valueJson = jsonEncode(value);
  final sig = signAutoOpCmd(key, action, valueJson, ts, id);
  // Built by hand, so the value text is the same bytes as in the signed
  // text.
  return '{"action":${jsonEncode(action)},"value":$valueJson,"ts":${jsonEncode(ts)},'
      '"cmd_id":${jsonEncode(id)},"sig":"$sig"}';
}
