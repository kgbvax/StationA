/// The short words of the 75AUTO page for the action and result names of
/// 75auto (contract: 75auto `docs/interfaces/auto-op-mqtt-api.md`).
library;

/// The short capital verb for an action name of 75auto. An unknown name
/// shows in capitals with spaces for its hyphens.
String autoVerb(String? action) {
  if (action == null || action.isEmpty) return '?';
  return switch (action) {
    'call-station' => 'CALL',
    'send-cq' => 'CQ',
    'halt' => 'HALT',
    'end-tx' => 'END TX',
    'change-band' || 'qsy' => 'QSY',
    'log-qso' || 'logged' => 'LOGGED',
    _ => action.replaceAll('-', ' ').toUpperCase(),
  };
}

/// The results that need no word on the page: a logged QSO, a shown line.
const _quietResults = {'logged', 'shown'};

/// The outcome word for a result of 75auto: OK for `took-effect`, else the
/// result in capitals. Null when the result needs no word.
String? autoOutcome(String? result) {
  if (result == null || result.isEmpty || _quietResults.contains(result)) return null;
  if (result == 'took-effect') return 'OK';
  return result.replaceAll('-', ' ').toUpperCase();
}
