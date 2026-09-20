# icom9700-radio-bridge — known issues and safety gaps

Component register (station-wide cross-cutting items live in `../docs/known-issues.md`).

## [safety-gap] IC-9700 firmware ignores CI-V PTT-off after the keying session dies — unkey requires a power cycle (gate 5, NO-GO)

Commissioning bench, 2026-09-20/21 (firmware as shipped; radio `uhf.kgbvax.net`):

- Keyed the radio via CI-V (`1C 00 01`) on a live LAN session, then blackholed the
  radio's UDP path — session loss while keyed.
- The bridge behaved per design: loss detected in ~1 s, armed permit dropped
  (fail-disarm, R11), and the safety redelivery loop connected fresh sessions and
  sent `1C 00 00` every ~26 s for over 6 minutes.
- The radio answered reads on the fresh sessions (it reported `1C 00 01` — still
  keyed) but never acknowledged the PTT-off set and never unkeyed. The carrier
  held from 00:08:49 until a manual power cycle at ~00:25 — over 14 minutes at
  minimal power.
- Control experiment on ONE healthy session (no loss involved): `1C 00 01` keyed,
  `1C 00 00` unkeyed within the same second, FB-acked. PTT-off over CI-V works —
  it is the post-loss zombie-keyed state that is deaf to it.

Consequence: the plan's Go/No-Go assumption "session loss during TX → the
redelivered PTT-off unkeys the radio" is **false on this firmware**. The redelivery
loop (safety.go) still runs and delivers as soon as the radio accepts it, but the
radio-side unkey needs human action (power cycle).

Mitigations in place:
- The max-TX watchdog (KTD-5, gate 6: PASS — 20 s bound force-unkeyed at ~21 s)
  bounds every keyed period **while the session is healthy** — the exposure window
  is exactly "session dies mid-key".
- Operating posture: an operator is present during bridge-driven UHF TX (the
  station model's current manual-operation posture). Unattended TX via this bridge
  is NOT approved while this gap stands.
- The bridge keeps the redelivery loop armed indefinitely and surfaces
  `ptt-off undeliverable` in `/state.error`, so the gap is always visible on the bus.

Remedy candidates (radio-side, none confirmed on this firmware): a TX timeout timer
in the radio's menu; a relay/AND-heroic external TX-inhibit (the radio has no
external TX-inhibit path — recorded in the exposure review, KTD-4 posture).

## [decision] wfview wire format, not kappanhang's — current firmware rejects the kappanhang handshake

The IC-9700 answers a kappanhang-shaped login with an undocumented 20-byte
`81 ff ff ff` (type 0x0001) rejection on every attempt — fresh boot, idle radio,
verified-correct credentials. The auth family follows wfview exactly: outer tracked
seq from 1, inner seq BE from 0x30, tokrequest echo at 0x1a, token at 0x1c, one
immediate 0x02 auth answered by the volunteered 0xa8 capabilities packet, 0x50
stream-grant. kappanhang's receive path also differs from real firmware (read
replies are FD-terminated and over-report datalen by one; set-command acks are
bare FB). All pinned in tests via the fake radio (which speaks the real forms).
