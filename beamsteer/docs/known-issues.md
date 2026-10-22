# beamsteer — known issues and open checks

## [open] G-450 overlap (360..450) readback unverified

wrc-rotator-bridge `/meta` advertises `az` 0..450. Nobody has checked whether `/state.az` reads above 360 inside the overlap, or whether the WRC accepts `set_az` above 360.

Until that is checked on the hardware, `steer.max_az` stays at 360. At that value the decision never targets the overlap. Once the check passes, setting it to 450 lets the cost function use the overlap, for example 440 instead of 80 when the rotator sits at 400.

## [open] PstRotator AZ? reply shape per logger

The two existing listeners reply in different shapes:
- spid-ercm-rotator-bridge uses the manual shape, `AZ:xxx.x\r`.
- wrc-rotator-bridge uses `<PST><AZIMUTH>n</AZIMUTH></PST>`.

beamsteer supports both through `pstrotator.reply`. The default is `manual`. Which shape the shack logger (DXLog / N1MM / Log4OM) actually parses still needs to be confirmed on the bench.

## [decision] Logger target moves from wrc :12040 to beamsteer :12050

For smart rotation to work, the logger must send to beamsteer. The wrc-rotator-bridge listener on :12040 stays up and drives the rotator directly, without smart rotation. It remains a fallback, for example when beamsteer is down.

## [decision] Antenna-mode lock: stand down, logger STOP not forwarded

While `hf/ant-mode` is storm or maintenance, beamsteer ignores every rotate request (toggle on or off), drops a held flip and sends nothing to `hf/rotator` or `hf/ant-ctrl`. The bridges enforce the lock and run the park themselves; beamsteer only keeps out of the way.

The logger's PstRotator `STOP` is not forwarded either. A contest logger sends STOP on its own (for example on a band change or a key macro), and a STOP reaching wrc-rotator-bridge during the park cancels the park until a repark. The operator's STOP still works: the console and the M5 dial send it straight to the rotator bridge.

Nothing dropped while locked is replayed on unlock; the next logger request decides afresh.
