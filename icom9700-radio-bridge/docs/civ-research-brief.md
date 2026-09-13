# CI-V over LAN protocol brief

Committed reference for U2/U3 (provenance: wfview source — `icomudpbase/icomudphandler/icomudpcivdata`, `packettypes.h`, `CI-V.md`, `rigs/IC-9700.rig`; kappanhang; official IC-9700 CI-V Reference Guide; firmware v1.50 2025-08-21, protocol unchanged since v1.30). U1 copies this appendix into `icom9700-radio-bridge/docs/civ-research-brief.md`.

**Ports and streams (all UDP):**

| Port | Direction | Purpose |
|---|---|---|
| 50001 | client -> radio (replies to client source port) | Control: are-you-there handshake, login/token auth, stream request; radio assigns data ports |
| 50002 | client <-> radio | CI-V data stream (all `FE FE...FD` frames incl. transceive broadcasts) |
| 50003 | radio -> client | Audio only — UNUSED by this bridge (v1) |

No broadcast/discovery exists; the client must know the radio address. `60000` is a Kenwood preset, not Icom.

**Connection sequence:** (1) send are-you-there (16-byte control packet, type 0x03) to radio:50001 every 500 ms until "I am here" (0x04); (2) exchange are-you-ready/I-am-ready (type 0x06) — establishes 4-byte IDs (`sentid` derived from client IP+port, `rcvdid` from radio) and sequence tracking; (3) login packet (~0x80 bytes): username+password obfuscated by a fixed 1-byte substitution table (public in wfview source — treat as plaintext-equivalent), client name; radio answers with a session token; (4) token renewal (0x40-byte packet, type 0x02) every 60 s; (5) request-stream packet carrying the client's chosen local CI-V port; radio answers with a status packet (~0x50 bytes) containing the radio-side CI-V port (normally 50002) big-endian at offset 0x42 (audio port at 0x46 — unused); error 0xffffffff = connection refused (stale/other session); (6) open a second UDP socket from the chosen local port to radio:50002 and send the open packet (0x16-byte openclose, data 0x01c0, magic 0x04).

**Packet layer (all streams):** 16-byte header (len, type, seq, sentid, rcvdid) + payload; CI-V payloads carry a 0x15-byte sub-header (reply=0xc1, datalen, big-endian send-seq) then raw CI-V. Both sides track sequence numbers and request retransmission of gaps (single- and multi-packet retransmit requests, 100 ms timer, give up after 4 tries, flush if >50 missing). Keepalives: idle control packets every 100 ms; ping (21-byte, type 0x07, radio-uptime ms) every 500 ms. CI-V watchdog: no CI-V data for 2 s -> re-send the start-data packet. Disconnect: control packet type 0x05. Parse CI-V frames by the sub-header `datalen` field, never by scanning for `FD` (payloads can embed it).

**CI-V layer:** address `A2` (radio), `E0` (controller); framing `FE FE A2 E0 <cmd> [<sub>] [<data>] FD`; OK reply `...FB FD`, NG `...FA FD` (rejection — e.g. freq/mode sets in satellite or memory mode). Frequency = 10 BCD digits little-endian, 10 Hz -> 100 MHz digit (144.500.000 -> `00 00 50 41 01`). Mode = 2 bytes (mode + filter): `00` LSB, `01` USB, `02` AM, `03` CW, `04` RTTY, `05` FM, `07` CW-R (normalizes to `cw` on the bus — reverse-filter bit is not representable), `08` RTTY-R, `17` DV (unsupported on the bus), `22` DD (unsupported); data mode = `06 <00/01> <filter>` modifier.

**Command table (this bridge's set):** read freq `03`; set freq `05 <10 BCD>`; freq transceive broadcast `00` (radio -> controller, MAIN and SUB changes); read mode `04`; set mode+filter `06 <mode> <filter>`; mode transceive `01`; data mode `06 <00/01> <filter>`; select main band `07 D0` / sub band `07 D1` (read selected: `07 D2 00`); satellite mode on/off/read `16 5A 00/01` (no data = read); PTT on/off + transceive `1C 00 00/01`; transceiver ID `19` (liveness/identity probe); S-meter `15 02` (0-255; S9=120); SWR/ALC/comp `15 12/13/14`; preamp `16 02`; attenuator `16 11`; RF output power `14 0A` (0-255, applies to the currently selected band's VFO); CI-V Transceive on/off `1A 05 0127`. MAIN power on/off `18 01/00` exists but is OUT OF SCOPE (never emitted). Main/sub exchange `07 B0` and split `0F` are not used by v1.

**Satellite semantics:** in satellite mode the radio transmits on **SUB** (uplink) and receives on **MAIN** (downlink); freq/mode `05`/`06` act per the satellite memory's uplink/downlink (`1A 07 <ch>`), and `25`/`26` selected/unselected-VFO commands return NG. The bridge's top-level active-TX fields therefore mirror SUB while satellite mode is on.

**Radio-side prerequisites (deploy gate 3):** SET > Network: Remote Control ON (enabling restarts the radio), username/password set, Remote IP `0.0.0.0`; SET > Connectors > CI-V: CI-V Address `A2`, CI-V Transceive ON. Single session: exactly one LAN client at a time; a second login is refused (stale-session error 0xffffffff may require a radio reboot to clear).
