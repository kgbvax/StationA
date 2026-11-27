/// The tooltips of the controls of the 75AUTO page. Each text says what a
/// tap does at 75auto, in plain words. The facts come from the contract
/// `auto-op-mqtt-api.md` and the rule set `rules/sets/everyday.gess` in
/// the 75auto repository.
///
/// The tooltips show only in the web build, where a mouse can hover. On the
/// tablet and the car screen a long press would open them and cover the
/// page, so there the controls have none.
library;

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';

import 'auto_commands.dart';

/// Whether the controls of the 75AUTO page get a tooltip. True in the web
/// build. Tests set it to see the tooltips.
@visibleForTesting
bool autoTipsOn = kIsWeb;

extension AutoControlTip on AutoControl {
  /// The tooltip of the control.
  String get tip => switch (this) {
    AutoControl.stop =>
      'Stop TX: 75auto turns transmit off at once and sets the stop latch. '
          'It also ends Arm CQ. The latch stays on until you clear it at the station.',
    AutoControl.approve =>
      'Approve: 75auto carries out the pending proposal, '
          'if the gate of 75auto lets it pass.',
    AutoControl.reject => 'Reject: 75auto drops the pending proposal and sends nothing for it.',
    AutoControl.observe => 'Observe mode: 75auto shows its proposals and carries out none of them.',
    AutoControl.confirm =>
      'Confirm mode: each proposal waits for Approve or Reject. '
          'A halt goes out without approval.',
    AutoControl.auto =>
      'Auto mode: 75auto carries out its proposals without approval. '
          'Tap the presence chip before the presence time ends, or 75auto halts.',
    AutoControl.follow =>
      'Follow the station in the DX call field. While its attempt runs, '
          '75auto starts no call to another station. Tap again to clear the follow.',
    AutoControl.skip =>
      'Skip the station in the DX call field for 60 minutes. '
          '75auto starts no call to it and does not answer its calls. '
          'If WSJT-X calls it, 75auto halts after the transmission that runs now.',
    AutoControl.presence =>
      'Presence: tells 75auto that you are at the station. '
          'This starts the presence time again. When it ends, 75auto halts.',
    AutoControl.armCq =>
      'Arm CQ: the rules can call CQ for 30 minutes from now. Auto mode only. '
          'Stop TX or a change out of auto mode ends it.',
    AutoControl.band =>
      'Ask 75auto for this band. The rules change band when no QSO runs '
          'and WSJT-X does not transmit and has Tx off.',
  };
}

/// [child] with the tooltip [message] in the web build, else [child] alone.
Widget autoTip(String message, Widget child) => autoTipsOn ? Tooltip(message: message, child: child) : child;
