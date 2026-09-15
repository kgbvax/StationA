# Icom IC-9700 / RS-BA1-style LAN protocol — CI-V over UDP wire spec

Extracted from two reference implementations:

- `[wfview]` = `/Users/ingomar.otter/.claude/jobs/f52abbe4/tmp/wfview` (C++/Qt; original implementation. NOTE: its UDP module header states "This code is heavily based on 'Kappanhang'" — `wfview src/radio/icomudphandler.cpp:2-3` — so the two are siblings; the UDP code is NOT the ancient original.)
- `[kh]` = `/Users/ingomar.otter/.claude/jobs/f52abbe4/tmp/kappanhang` (Go; newer, known-good against recent firmware — preferred whenever the two disagree).

Scope: everything needed to open and hold the **CI-V (serial) data stream**. Audio-stream details are included only where they share wire structures. CI-V payload *content* (freq/mode commands) is out of scope except the addressing byte.

Conventions: all offsets are 0-based byte offsets in the UDP datagram. "LE"/"BE" = little/big endian. `r[n]` = byte n of a received datagram.

---

## 1. 16-byte packet header (present on every packet of every stream)

Both implementations agree on the field order; only the SIDs' internal byte order differs (see below).

| offset | size | field   | type on wire                         | notes |
|-------:|-----:|---------|--------------------------------------|-------|
| 0x00   | 4    | `len`   | u32 LE                               | total datagram length incl. header |
| 0x04   | 2    | `type`  | u16 LE                               | 0x0000..0x0007, see §2 |
| 0x06   | 2    | `seq`   | u16 LE                               | outer tracked send-sequence (per stream) |
| 0x08   | 4    | `sentid`| 4 raw bytes (opaque cookie)          | sender's SID, see §12 |
| 0x0c   | 4    | `rcvdid`| 4 raw bytes (opaque cookie)          | other side's SID |

- Struct definition: `wfview include/packettypes.h:47-56` (`control_packet`, `#pragma pack(1)` at `wfview include/packettypes.h:43-44`).
- `len` is written as `sizeof(packet)` (LE on x86): `wfview src/radio/icomudpbase.cpp:398`, and is the first byte of every kh literal, e.g. `0x10` for 16-byte packets `kh streamcommon.go:98`.
- `type` LE: kh literals put the low byte at offset 4 (`0x10,0x00,0x00,0x00,0x03,0x00` = type 3): `kh streamcommon.go:98`; wfview `p.type = type` native: `wfview src/radio/icomudpbase.cpp:399`.
- `seq` LE, written into `d[6],d[7]` at send time: `kh pkt0.go:135-136`; wfview same: `wfview src/radio/icomudpbase.cpp:438-439`.
- **sentid/rcvdid byte order — implementations disagree, wire-safe either way.**
  - kh derives `localSID` and writes it BIG-endian: `kh streamcommon.go:240-242` (derive) and e.g. `byte(s.common.localSID >> 24), ..., byte(s.common.localSID)` at `kh controlstream.go:50-51`.
  - wfview derives the same logical value into `myId` but writes it native (LE on x86): `wfview include/icomudpbase.h:101`, `wfview src/radio/icomudpbase.cpp:17-18`, `wfview src/radio/icomudpbase.cpp:400-401`.
  - Both work because every party echoes the 4 bytes verbatim (never re-interprets them). **Implementation rule: derive your own sentid once per socket (§12), then always copy the peer's 4 raw bytes into `rcvdid` and echo your own 4 raw bytes back — do not byteswap anything.**
- Minimum accepted datagram size is 0x10: `wfview src/radio/icomudpbase.cpp:41-44`; `kh pkt0.go:62-64`.

## 2. Packet type constants

Control-layer types live in the header `type` field (u16 LE @0x04):

| value | name                | len (hex)  | wfview cite | kh cite |
|------:|---------------------|-----------|-------------|---------|
| 0x0000| idle / tracked payload (login, auth, conninfo, CI-V data, audio — distinguished by `len`) | 0x10 idle; payload per §3-§9 | `wfview include/packettypes.h:22-38` | `kh pkt0.go:104-112` |
| 0x0001| retransmit request  | 0x10 single; 0x18+ ranges | `wfview src/radio/icomudpbase.cpp:51,203` | `kh pkt0.go:110-111` |
| 0x0003| are-you-there       | 0x10 | `wfview src/radio/icomudphandler.cpp:80,675` | `kh streamcommon.go:97-108` |
| 0x0004| i-am-here (answer)  | 0x10 | `wfview src/radio/icomudpbase.cpp:76-86` | `kh streamcommon.go:110-120` |
| 0x0005| disconnect          | 0x10 | `wfview src/radio/icomudpbase.cpp:31` | `kh streamcommon.go:199-211` |
| 0x0006| are-you-ready / i-am-ready | 0x10, `seq`=0x0001 | `wfview src/radio/icomudpbase.cpp:85` | `kh streamcommon.go:122-140` |
| 0x0007| ping / pong         | 0x15 | `wfview include/packettypes.h:24,76-97` | `kh pkt7.go:32-34,98-101` |

Fixed total lengths (`wfview include/packettypes.h:22-33`):

| constant | value | kh equivalent (packet examples) |
|----------|------:|---------------------------------|
| CONTROL_SIZE | 0x10 | 16-byte pkt0/pkt3/pkt4/pkt5/pkt6 |
| WATCHDOG_SIZE | 0x14 | (wfview-server only; type 0x04 at 0x14 len — clients never send: `wfview include/packettypes.h:60-71`) |
| PING_SIZE | 0x15 | ping pkt7 / CI-V data sub-header |
| OPENCLOSE_SIZE | 0x16 | serial open/close: `kh serialstream.go:54-57` |
| RETRANSMIT_RANGE_SIZE | 0x18 | range retransmit request: `kh streamcommon.go:163-166` |
| TOKEN_SIZE | 0x40 | auth token request/answer: `wfview include/packettypes.h:164-195`, `kh controlstream.go:96-104` |
| STATUS_SIZE | 0x50 | status answer: `wfview include/packettypes.h:198-233`, `kh controlstream.go:206-228` |
| LOGIN_RESPONSE_SIZE | 0x60 | login answer: `wfview include/packettypes.h:236-259`, `kh controlstream.go:346` |
| LOGIN_SIZE | 0x80 | login: `wfview include/packettypes.h:262-284`, `kh controlstream.go:49-69` |
| CONNINFO_SIZE | 0x90 | stream request + its answer: `wfview include/packettypes.h:287-339`, `kh controlstream.go:118-139,230-282` |
| CAPABILITIES_SIZE | 0x42 | capabilities header: `wfview include/packettypes.h:32,377-396` |
| RADIO_CAP_SIZE | 0x66 | per-radio capability block: `wfview include/packettypes.h:33,344-372` |
| (0xa8 = 0x42 + 1×0x66) | | full single-radio capabilities packet, `kh controlstream.go:159-185` |

Audio data packets (kh, TX direction): two lengths only — 0x056c (24-byte header + 1364 B PCM) starting `6c 05 00 00 00 00`, and 0x0244 (24 + 556 B) starting `44 02 00 00 00 00`; header `type`=0, audio-ident 0x0080 @0x10, sendseq BE @0x12, datalen BE @0x16: `kh audiostream.go:32-58`. wfview audio header struct: `wfview include/packettypes.h:119-134`; wfview writes ident 0x0080, datalen BE, sendseq BE: `wfview src/radio/icomudpaudio.cpp:137-143`.

## 3. Login packet (0x80, 128 bytes) and its encoding

Layout (kh construction, `kh controlstream.go:49-69`; wfview struct `wfview include/packettypes.h:262-284`, fill `wfview src/radio/icomudphandler.cpp:690-705`):

| offset | size | content |
|-------:|-----:|---------|
| 0x00-0x0f | 16 | standard header; `len`=0x80, `type`=0, `seq`=tracked pkt0 seq (first tracked packet: 1), sentid, rcvdid (rcvdid still 0 — not yet known? no: radio SID was learned from pkt4 in step 4 of the handshake, so it is filled) |
| 0x10-0x13 | 4 | `payloadsize` u32 **BE** = 0x00000070 (total − 0x10) |
| 0x14 | 1 | `requestreply` = 0x01 (this is a request) |
| 0x15 | 1 | `requesttype` = 0x00 (login) |
| 0x16 | 1 | 0x00 always |
| 0x17-0x18 | 2 | `innerseq` u16 **LE**, per-stream auth counter starting at 0, incremented after every auth-layer packet (`kh controlstream.go:52,74,108,144`; wfview instead writes its counter `authSeq` starting 0x30 BE at 0x16-0x17 — `wfview include/icomudpbase.h:103`, `wfview src/radio/icomudphandler.cpp:699`. For values < 256 both put the low byte at 0x17 and are wire-identical; the radio echo example confirms the value sits at 0x17-0x18 LE: request `...0x01,0x05,0x00,0x02,0x00...` echoed as `...0x02,0x05,0x00,0x02,0x00...` in `kh controlstream.go:81-94`. Use kh layout, byte 0x16 = 0.) |
| 0x19 | 1 | 0x00 always |
| 0x1a-0x1b | 2 | `tokrequest` / `authStartID`: 2 RANDOM bytes generated by the client (`kh controlstream.go:43-44`; wfview `tokRequest = rand()|rand()<<8`, `wfview src/radio/icomudphandler.cpp:683`). Written native/LE in wfview (`p.tokrequest = tokRequest`, no swap, `wfview src/radio/icomudphandler.cpp:700`); kh treats it as part of an opaque 6-byte block. The radio echoes bytes 0x1a-0x1f verbatim, so any 2 bytes work. |
| 0x1c-0x1f | 4 | `token`: zeros on login (no token yet) (`kh controlstream.go:53,58-65` zeros region; wfview leaves `p.token` = 0 since memset: `wfview src/radio/icomudphandler.cpp:691`) |
| 0x20-0x3f | 32 | zeros (`unusedc`) |
| 0x40-0x4f | 16 | `username`, encoded (below), padded with 0x00 to 16 (`kh controlstream.go:58-61`; wfview `char username[16]` `wfview include/packettypes.h:278`) |
| 0x50-0x5f | 16 | `password`, encoded, padded 0x00 (`kh controlstream.go:62-65`) |
| 0x60-0x6f | 16 | client/station name, plain ASCII, NUL-terminated, zero-padded. kh sends literal `icom-pc` (`kh controlstream.go:66`); wfview sends `prefs.clientName.mid(0,8) + "-wfview"` (`wfview src/radio/icomudphandler.cpp:19,703`) |
| 0x70-0x7f | 16 | zeros |

Username/password encoding — **16-byte fixed width, no terminator; unused bytes 0x00; shorter inputs are simply zero-padded** (kh `passcode()` returns exactly 16 bytes: `kh passcode.go:101-111`; wfview loops `i < in.length() && i < 16`: `wfview include/icomudpbase.h:203`).

Substitution table — identical table defined in both. **kh defines only the 95-entry map for inputs 32..126** (`kh passcode.go:3-99`); **wfview defines the full 256-entry array, entries 0..31 and 127..255 = 0x00** (`wfview include/icomudpbase.h:188-199`). There is **no reverse table anywhere** (encoding is one-way; the radio compares encoded bytes).

Verbatim 256-entry table (index = byte value; wfview `include/icomudpbase.h:190-199`, matches `kh passcode.go:3-99` for 32..126):

```
idx: 0x00-0x1f -> 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00
                  00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00
idx: 0x20-0x2f -> 47 5d 4c 42 66 20 23 46 4e 57 45 3d 67 76 60 41
idx: 0x30-0x3f -> 62 39 59 2d 68 7e 7c 65 7d 49 29 72 73 78 21 6e
idx: 0x40-0x4f -> 5a 5e 4a 3e 71 2c 2a 54 3c 3a 63 4f 43 75 27 79
idx: 0x50-0x5f -> 5b 35 70 48 6b 56 6f 34 32 6c 30 61 6d 7b 2f 4b
idx: 0x60-0x6f -> 64 38 2b 2e 50 40 3f 55 33 37 25 77 24 26 74 6a
idx: 0x70-0x7e -> 28 53 4d 69 22 5c 44 31 36 58 3b 7a 51 5f 52
idx: 0x7f-0xff -> 00 (all zero; wfview only)
```

Algorithm (`kh passcode.go:101-111`, identical `wfview include/icomudpbase.h:201-212`):

```
for i in 0..min(len(s),16)-1:
    p = int(s[i]) + i          # position-dependent
    if p > 126: p = 32 + p % 127
    out[i] = table[p]
# remaining bytes of the 16-byte field stay 0x00
```

Capability flags in login: none (all zeros) — only the conninfo request (§5) carries capability bytes.

Login answer: see §14 for the error check; layout in `wfview include/packettypes.h:236-259`: header as usual with `payloadsize` 0x50 BE @0x10, `requestreply`=0x02 @0x14, `requesttype`=0x00 @0x15, `tokrequest`+`token` echo @0x1a-0x1f, `error` u32 @0x30, connection-type string @0x40 (16 bytes, e.g. `FTTH`; wfview reads it, `wfview src/radio/icomudphandler.cpp:418-419`; kh example shows `46 54 54 48` at `kh controlstream.go:342`).

## 4. Auth/token packets (0x40, 64 bytes)

One shared layout for: post-login token submission (requesttype 0x02), periodic token renewal (requesttype 0x05, every 60 s), deauth/token-removal (requesttype 0x01), and the radio's answer (requestreply 0x02).

Construction (`kh controlstream.go:78-110`, incl. full request+reply examples at `kh controlstream.go:79-94`; wfview `wfview src/radio/icomudphandler.cpp:709-730`, struct `wfview include/packettypes.h:164-195`):

| offset | size | content |
|-------:|-----:|---------|
| 0x00-0x0f | 16 | header; `len`=0x40, `type`=0, tracked seq |
| 0x10-0x13 | 4 | `payloadsize` u32 BE = 0x00000030 |
| 0x14 | 1 | `requestreply`: 0x01 = request from client; 0x02 = answer from radio |
| 0x15 | 1 | `requesttype` ("magic"): 0x01 deauth / 0x02 post-login token / 0x05 renewal |
| 0x16 | 1 | 0x00 |
| 0x17-0x18 | 2 | `innerseq` u16 LE (same counter as login, §3) |
| 0x19 | 1 | 0x00 |
| 0x1a-0x1f | 6 | **authID block**: for 0x02 and 0x05 requests, copy the 6 bytes (2-byte tokrequest + 4-byte token) that the radio returned in the login answer at the same offset — kh: `copy(s.authID[:], r[26:32])` from the 0x60 answer `kh controlstream.go:356`, echoed back at `kh controlstream.go:100`; wfview: `token = in->token` then `p.token = token` + `p.tokrequest = tokRequest` (`wfview src/radio/icomudphandler.cpp:450,700-724`). Treat as 6 opaque bytes. |
| 0x20-0x2f | 16 | kh: all zeros. wfview additionally sets `resetcap` u16 BE 0x0798 @0x24 (`wfview src/radio/icomudphandler.cpp:723`) and can place guid/mac here in conninfo (§5) but does not for tokens. kh is known-good with zeros — prefer zeros. |
| 0x30-0x33 | 4 | requests: zeros. **Answer: `response` code** — 0x00000000 = OK, 0xffffffff = rejected (wfview parses: `wfview src/radio/icomudphandler.cpp:321-348`; kh does not parse it and accepts any 0x40 answer with `r[21]==0x05`: `kh controlstream.go:201-204`) |
| 0x34-0x3f | 12 | zeros |

Token answer (radio → client, 64 bytes): `requestreply`=0x02 @0x14, `requesttype` mirrors the request @0x15, `innerseq` echo @0x17 LE, authID echo @0x1a-0x1f, `response` @0x30. Worked example in `kh controlstream.go:87-94`.

Renewal scheduling: **every 60 s** — kh `reauthInterval = time.Minute` (`kh controlstream.go:15`, ticker at `kh controlstream.go:292,302-307`); wfview `TOKEN_RENEWAL 60000` ms (`wfview include/packettypes.h:7`, `tokenTimer->start(TOKEN_RENEWAL)` at `wfview src/radio/icomudphandler.cpp:326,452`). Renewal send uses requesttype **0x05** in both (`kh controlstream.go:305`; `wfview src/radio/icomudphandler.cpp:79`). The packet sent immediately after login uses requesttype **0x02** in both (`kh controlstream.go:358`; `wfview src/radio/icomudphandler.cpp:451`). kh additionally sends a 0x05 renewal right after the 0x02 and waits for its answer before proceeding (`kh controlstream.go:365,201-204`); wfview does not send an initial 0x05 (first one at t=60 s).

Deauth (clean shutdown): one 0x40 packet with requesttype 0x01 on the control stream (`kh controlstream.go:394-399`; wfview `sendToken(0x01)` `wfview src/radio/icomudphandler.cpp:111-113,142-144`).

## 5. Request-stream packet (conninfo, 0x90, 144 bytes) + Status answer (0x50, 80 bytes)

Request (client → radio, asks the radio to open serial+audio streams). kh construction `kh controlstream.go:112-147`; wfview `wfview src/radio/icomudphandler.cpp:622-663`, struct `wfview include/packettypes.h:287-339`:

| offset | size | content |
|-------:|-----:|---------|
| 0x00-0x0f | 16 | header; `len`=0x90, `type`=0, tracked seq |
| 0x10-0x13 | 4 | `payloadsize` u32 BE = 0x00000080 |
| 0x14 | 1 | `requestreply` = 0x01 |
| 0x15 | 1 | `requesttype` = 0x03 (conninfo / stream request) |
| 0x16 | 1 | 0x00 |
| 0x17-0x18 | 2 | `innerseq` u16 LE (same counter) |
| 0x19 | 1 | 0x00 |
| 0x1a-0x1f | 6 | authID block (as §4) |
| 0x20-0x2f | 16 | radio identity copied from the capabilities packet: kh copies the 16 bytes at r[66:82] of the 0xa8 packet = the radio's guid (`kh controlstream.go:183-184,123-124`). wfview: EITHER guid[16] @0x20, OR (older rigs, `commoncap == 0x8010`) commoncap u16 = 0x8010 (bytes `10 80`) @0x27 + macaddress[6] @0x2a (`wfview src/radio/icomudphandler.cpp:603-610,637-643`) |
| 0x30-0x3f | 16 | zeros |
| 0x40-0x5f | 32 | radio model name, plain ASCII NUL-padded — must match the name reported in the radio's capabilities packet. kh hardcodes `IC-705` (`kh controlstream.go:127`); for IC-9700 use `IC-9700` (name comes from `radios[radio].name`, `wfview src/radio/icomudphandler.cpp:612,647`) |
| 0x60-0x6f | 16 | username, encoded as §3 (`kh controlstream.go:117,131-134`) |
| 0x70 | 1 | `rxenable` = 1 (`kh controlstream.go:135`; `wfview src/radio/icomudphandler.cpp:648`) |
| 0x71 | 1 | `txenable` = 1 if TX audio supported, else 0 (kh always 1: `kh controlstream.go:135`; wfview only if `txSampleRates > 1`: `wfview src/radio/icomudphandler.cpp:649-652`) |
| 0x72 | 1 | `rxcodec` = 0x04 (LPCM16; Opus would be >=0x40 and is forced back to 4 by wfview if the peer is not wfview: `wfview src/radio/icomudphandler.cpp:425-435,653`) |
| 0x73 | 1 | `txcodec` = 0x04 (`kh controlstream.go:135`) |
| 0x74-0x77 | 4 | `rxsample` u32 BE = 48000 (`kh controlstream.go:135-136`; `kh audio-linux.go:17`; `wfview src/radio/icomudphandler.cpp:655`) |
| 0x78-0x7b | 4 | `txsample` u32 BE = 48000 (`kh controlstream.go:136`) |
| 0x7c-0x7f | 4 | **`civport` u32 BE = client's chosen LOCAL CI-V/serial UDP port** (kh: 50002, `kh controlstream.go:137`; wfview: randomly grabbed local port, `wfview src/radio/icomudphandler.cpp:590-599,657`) |
| 0x80-0x83 | 4 | **`audioport` u32 BE = client's chosen LOCAL audio UDP port** (50003) (`kh controlstream.go:138`; `wfview src/radio/icomudphandler.cpp:658`) |
| 0x84-0x87 | 4 | `txbuffer` u32 BE = TX audio buffer ms = 300 (`kh controlstream.go:115,139`; `kh txseqbuf.go:5-8`; wfview sends `txSetup.latency`: `wfview src/radio/icomudphandler.cpp:659`) |
| 0x88 | 1 | `convert` = 1 (`kh controlstream.go:139` last-but-8 byte `0x01`; `wfview src/radio/icomudphandler.cpp:660`) |
| 0x89-0x8f | 7 | zeros |

Send condition (kh): only when auth OK AND capabilities packet seen (`kh controlstream.go:149-155`); a 5 s watchdog covers a lost answer (`kh controlstream.go:370-372`).

**Status answer (radio → client, 0x50 = 80 bytes)** — the answer to requesttype 0x03 (`requesttype`=0x03 @0x15, `requestreply`=0x02 @0x14 in the example `kh controlstream.go:208-217`). Total size 0x50: `wfview include/packettypes.h:28,198-233`; kh length dispatch `kh controlstream.go:206`.

| offset | size | content |
|-------:|-----:|---------|
| 0x10-0x13 | 4 | `payloadsize` u32 BE = 0x40 |
| 0x14 | 1 | 0x02 (answer) |
| 0x15 | 1 | 0x03 (mirrors request) |
| 0x17-0x18 | 2 | innerseq echo u16 LE (example 0x52: `kh controlstream.go:210`) |
| 0x1a-0x1f | 6 | authID echo |
| 0x30-0x33 | 4 | `error` u32. `ff ff ff ff` (0xffffffff) = **connection refused / station busy / "try rebooting the radio"** — wfview: `wfview src/radio/icomudphandler.cpp:356-359`; kh checks only the first 3 bytes `ff ff ff` @48: `kh controlstream.go:219-224` |
| 0x40 | 1 | `disc` — 0x01 together with error 0 = radio-initiated disconnect (`wfview src/radio/icomudphandler.cpp:361-379`; kh `r[64]==1`: `kh controlstream.go:225-227`) |
| 0x42-0x43 | 2 | **radio-side CI-V/serial UDP port, u16 BE** (confirmed: `quint16 civport; // 0x42 // Sent bigendian` `wfview include/packettypes.h:227`, read `civPort = qFromBigEndian(in->civport)` `wfview src/radio/icomudphandler.cpp:381`). kh never parses it — it hardcodes 50002 both sides (`kh controlstream.go:12`, `kh serialstream.go:213`) |
| 0x44-0x45 | 2 | unused (BE) |
| 0x46-0x47 | 2 | **radio-side audio UDP port, u16 BE** (`wfview include/packettypes.h:229`, read at `wfview src/radio/icomudphandler.cpp:382`) |
| 0x48-0x4f | 8 | zeros |

Conninfo ANSWER (radio → client, 0x90 = 144 bytes, arrives alongside/after the 0x50): kh accepts only if `r[96]==1` (i.e. u32 1 @0x60 — wfview calls this field `busy` u32 @0x60, `wfview include/packettypes.h:316`): `kh controlstream.go:230`. Fields: device/radio name NUL-terminated @0x40 (`kh controlstream.go:253`), computer name @0x64 (`wfview include/packettypes.h:317`), radio IPv4 address (network order) u32 @0x84 (`wfview include/packettypes.h:319`, example `c0 a8 03 03` `kh controlstream.go:248-249`). **The answer also refreshes BOTH SIDs**: kh re-reads remoteSID = BE u32 @8 and localSID = BE u32 @12 ("stuff can change in the meantime because of a previous login") and re-copies authID @0x1a-0x1f — `kh controlstream.go:256-260`. Do the same.

## 6. Open/close packet for the CI-V data stream (0x16 = 22 bytes)

Sent on the **CI-V/serial data socket** (client's serial-stream socket → radio:50002), as a tracked packet. Layout (kh bytes `kh serialstream.go:54-57`; wfview struct `wfview include/packettypes.h:100-115`, fill `wfview src/radio/icomudpcivdata.cpp:87-109`):

| offset | size | content |
|-------:|-----:|---------|
| 0x00-0x0f | 16 | header; `len`=0x16, `type`=0, tracked seq (does NOT advance the CI-V inner sendseq... it is a separate pkt0-tracked packet) |
| 0x10-0x11 | 2 | `data` = 0x01c0 as bytes `c0 01` (wfview writes u16 native LE 0x01c0 → same bytes: `wfview src/radio/icomudpcivdata.cpp:101`; kh literal `0xc0, 0x01`: `kh serialstream.go:57`) |
| 0x12 | 1 | 0x00 |
| 0x13-0x14 | 2 | CI-V `sendseq` u16 **BE** (the same counter used by CI-V data packets, §8; incremented after send: `kh serialstream.go:61`; `wfview src/radio/icomudpcivdata.cpp:102-105`) |
| 0x15 | 1 | magic: **open vs close — IMPLEMENTATIONS DISAGREE**. kh (newer, known-good): open = **0x05**, close = 0x00 (`kh serialstream.go:46-52`). wfview: open = **0x04**, close = 0x00 (`wfview src/radio/icomudpcivdata.cpp:89-94`). **Use 0x05** (kappanhang is validated against recent firmware); 0x04 is what the task brief and wfview use — UNVERIFIED whether current firmware accepts both. Close 0x00 is agreed. |

- Open is sent once after the data stream's own pkt3/pkt4/pkt6 handshake: `kh serialstream.go:234`.
- Close is sent from the teardown path before the stream disconnect: `kh serialstream.go:260-262`; wfview destructor `wfview src/radio/icomudpcivdata.cpp:42-45`.
- wfview additionally RE-SENDS the open packet every 100 ms (timer `startCivDataTimer`) until the first CI-V data arrives, and whenever the watchdog (§11) fires: `wfview src/radio/icomudpcivdata.cpp:31,137-141,47-65`.
- kappanhang never sends an open/close on the audio stream: `kh audiostream.go:151-179`.

## 7. Ping packet (pkt7, 21 bytes, type 0x0007)

Layout (kh `kh pkt7.go:79-106`; wfview struct `wfview include/packettypes.h:76-97`, send `wfview src/radio/icomudpbase.cpp:416-432`):

| offset | size | content |
|-------:|-----:|---------|
| 0x00-0x0f | 16 | header; `len`=0x15, `type`=0x0007, `seq` = this stream's ping counter (LE). kh starts the counter at 2 on control (`kh controlstream.go:354`) and 1 on serial/audio (`kh serialstream.go:230`, `kh audiostream.go:164`); wfview starts `pingSendSeq` at 0 (`wfview include/icomudpbase.h:148`) |
| 0x10 | 1 | `reply` flag: 0x00 = request, 0x01 = reply (both agree: `kh pkt7.go:82-96`; `wfview src/radio/icomudpbase.cpp:103,165`) |
| 0x11-0x14 | 4 | request: arbitrary 4-byte "reply ID" that the radio echoes verbatim. kh generates `[random byte, innerseq lo, innerseq hi, 0x06]` with innerseq starting 0x8304 (`kh pkt7.go:83-94,152`). wfview instead writes local wall-clock ms-since-midnight (`p.time`, "uptime of device" comment): `wfview src/radio/icomudpbase.cpp:425-426`, `wfview include/packettypes.h:86-88`. Reply: copy the request's 4 bytes back (`kh pkt7.go:43`; `wfview src/radio/icomudpbase.cpp:166-167`). kappanhang's echo semantics are confirmed by the captured examples (`kh pkt7.go:40-41,80-81`) — implement the echo, do not interpret the value. |
| — | — | total 21 bytes: `d := []byte{0x15,0x00,0x00,0x00,0x07,0x00, seq lo, seq hi, sentid(4), rcvdid(4), replyFlag, id0, id1, id2, id3}` (`kh pkt7.go:98-101`) |

Recognition: length 21 and bytes 1-5 = `00 00 00 07 00` (byte 0 may be 0x15 or 0x00): `kh pkt7.go:32-34`.

Answering the radio's pings: if `r[16]==0`, reply with `reply`=0x01, `seq` = the request's seq, ID = request bytes 17-21 (`kh pkt7.go:37-45`). wfview same: `wfview src/radio/icomudpbase.cpp:159-170`. The radio emits pings every 100 ms on each stream (kh comment: `kh pkt7.go:11-13`); replying is unconditional once the stream exists.

Client's own ping interval: kh 3 s (`pkt7SendInterval`, `kh pkt7.go:13`); wfview 500 ms (`PING_PERIOD`, `wfview include/packettypes.h:8`; started on control after "i-am-here" `wfview src/radio/icomudphandler.cpp:262`, on CI-V stream at construction `wfview src/radio/icomudpcivdata.cpp:36`). A ping timeout of 3 s exists in kh but is NEVER armed (all three `startPeriodicSend(..., false)` call sites: `kh controlstream.go:354`, `kh serialstream.go:230`, `kh audiostream.go:164`; arming code `kh pkt7.go:156-158`).

## 8. CI-V data sub-header (21 bytes) + framing

CI-V bytes ride in tracked packets (`type`=0) whose header is followed by a 5-byte sub-header, then raw CI-V frame bytes.

Send (client → radio), kh `kh serialstream.go:33-44`:

| offset | size | content |
|-------:|-----:|---------|
| 0x00-0x0f | 16 | header; **`len` = 0x15 + datalen** (u32 LE) |
| 0x10 | 1 | reply flag = **0xc1** |
| 0x11 | 1 | `datalen` low byte |
| 0x12 | 1 | 0x00 (kh hardcodes 0; wfview defines `datalen` as u16 LE @0x11 — identical wire bytes for datalen < 256. `wfview include/packettypes.h:90-91`, `wfview src/radio/icomudpcivdata.cpp:76`) |
| 0x13-0x14 | 2 | CI-V `sendseq` u16 **BE** ("THIS IS BIG ENDIAN!": `wfview src/radio/icomudpcivdata.cpp:77`; kh `kh serialstream.go:38`), per-stream counter starting 0 (`kh serialstream.go:13` zero value; wfview `sendSeqB = 0` `wfview include/icomudpbase.h:104`), incremented per packet |
| 0x15.. | n | raw CI-V frame (`fe fe ... fd`) |

Radio → client packets have the same shape; validity check before extracting payload:

- kh: `len(r) >= 22 && r[16] == 0xc1 && r[0]-0x15 == r[17]` (first length byte equals datalen): `kh serialstream.go:114`. On delivery it strips the 21-byte sub-header: `e.data = e.data[21:]` (`kh serialstream.go:93`).
- wfview: accepts when `len(r) > 21 && r[4:6] != 0x01` (not a retransmit request) and `quint16(in->datalen + 0x15) == (quint16)in->len`, then emits `r.mid(0x15)` (everything after byte 0x14): `wfview src/radio/icomudpcivdata.cpp:146-157,231`.

Client → radio framing rules (kh): split outgoing CI-V into frames at `0xfc` or `0xfd` terminator or at **80 bytes** (`maxSerialFrameLength`, "max frame length according to Hamlib": `kh serialstream.go:9,120-165`); resync on `fe fe`; a partial frame is flushed after 100 ms of silence (`kh serialstream.go:145,151-164`). Each frame becomes one packet via `send()`.

## 9. Sequence tracking + retransmit

**Initial values.**
- Outer tracked seq (`pkt0.sendSeq`): starts at **1** on the control stream (`kh pkt0.go:213-215`, called `kh controlstream.go:328`) and on the serial stream (`kh serialstream.go:231`); the audio stream never calls init so its tracked seq starts 0 (`kh audiostream.go:151-179` has no `pkt0.init`). wfview: `sendSeq = 1` (`wfview include/icomudpbase.h:105`), cleared+restarted on rollover through 0 (`wfview src/radio/icomudpbase.cpp:448-452`).
- Auth innerseq: starts 0, one counter across login/token/conninfo (`kh controlstream.go:23,52,74,108,144`); wfview equivalent `authSeq = 0x30` (`wfview include/icomudpbase.h:103`).
- CI-V inner sendseq (BE @0x13): starts 0.
- pkt7 ping counters: §7.

**Transmit buffer (for answering retransmit requests).**
- kh: every tracked packet is stored by seq in `txSeqBuf` before sending (`kh pkt0.go:135-137`); entries older than 3 s are purged (`txSeqBufLength(300 ms) * 10`, `kh txseqbuf.go:8,29-35`).
- wfview: same, stored in `txSeqBuf` (`wfview src/radio/icomudpbase.cpp:435-460`); capped at `BUFSIZE` 500 by dropping the oldest (`wfview src/radio/icomudpbase.cpp:453-456`); `purgeOldEntries()` (10 s, `PURGE_SECONDS` `wfview include/packettypes.h:6`, `wfview src/radio/icomudpbase.cpp:489-509`) exists but is no longer called for TX (`wfview src/radio/icomudpbase.cpp:464-466`).

**Retransmit request — single packet (16 bytes, type 0x0001):** `len`=0x10, `type`=0x0001, `seq` LE = requested seq, sentid, rcvdid (`kh streamcommon.go:142-153`; received-form check `kh pkt0.go:66`). wfview receive: `wfview src/radio/icomudpbase.cpp:51-75`.

**Retransmit request — range (0x18+ bytes, type 0x0001):** `len`=0x18 (or more), `type`=0x0001, `seq`=0x0000, then a list of (u16 start LE, u16 end LE) inclusive range pairs; the whole request datagram is sent TWICE (`kh streamcommon.go:155-174`). Receiver loops over 4-byte pairs (`kh pkt0.go:90-99`). wfview's parser instead treats every 2-byte slot from 0x10 as an individual seq (`wfview src/radio/icomudpbase.cpp:203-230`) and its own sender emits each missing seq twice in a row (`wfview src/radio/icomudpbase.cpp:339-342`) — wire-compatible with single-packet ranges but it cannot parse a true multi-packet range. **Implement kh semantics.**

**Resend behavior — implementations disagree:** kh resends each requested packet **twice** (two back-to-back `s.send(d)`: `kh pkt0.go:37-39,73-78`); if the seq is no longer buffered it substitutes an UNTRACKED idle packet carrying the requested seq, also sent twice (`kh pkt0.go:44-51,82-88`, idle format `kh pkt0.go:159-167`). wfview resends once per request (`wfview src/radio/icomudpbase.cpp:63-66,221-225`), substituting an untracked idle via `sendControl(false, 0, seq)` (`wfview src/radio/icomudpbase.cpp:216`). Follow kh (twice).

**Client→radio retransmit requests (when the radio misses our packets):** nothing to implement beyond the tx buffer — the radio asks. When *we* miss radio packets:
- kh: event-driven from the RX seqbuf. On a sequence gap it locks the buffer, requests range `[expected, got-1]` via the callback (`kh seqbuf.go:317-335`), keeps waiting up to `length` (100 ms for serial and audio; `kh serialstream.go:10`, `kh audiostream.go:12`) or 2× control-stream RTT, then either accepts when the requested range has arrived (`kh seqbuf.go:252-274,291-300`) or ignores everything up to the last requested seq and moves on (`kh seqbuf.go:243-248`). Ranges > 10 packets (`maxRetransmitRequestPacketCount`) are refused with an error (`kh streamcommon.go:13,179-181`).
- wfview: timer-driven, `RETRANSMIT_PERIOD` **100 ms** (`wfview include/packettypes.h:12`, timer started `wfview src/radio/icomudpbase.cpp:20-22`): scans the missing map each tick; a single missing seq goes out as the 16-byte request, multiple as the 0x18-style request (`wfview src/radio/icomudpbase.cpp:310-388`); each missing seq is requested at most **4 times** (give-up count: `it.value() < 4`, incremented per request, erased at 4 — `wfview src/radio/icomudpbase.cpp:337-348`).

**">50 missing" flush rule (wfview):** if more than `MAX_MISSING` = 50 packets are outstanding, give up resyncing: clear `rxMissing` and `rxSeqBuf` entirely (`wfview include/packettypes.h:16`, `wfview src/radio/icomudpbase.cpp:317-327`); a single observed gap > 50 likewise clears the buffers (`wfview src/radio/icomudpbase.cpp:239-253`). kappanhang has no direct equivalent (its lock/ignore logic plays this role).

**Receive-tracking buffers, bounding/cleaning:**
- wfview: `rxSeqBuf` (seq → time) capped at `BUFSIZE` 500 by erasing the oldest entry on insert (`wfview src/radio/icomudpbase.cpp:268-271,281-284`); `rxMissing` trimmed by 25 entries whenever it exceeds 50 inside `purgeOldEntries` (`wfview src/radio/icomudpbase.cpp:534-543`); 10 s age purge available (`wfview src/radio/icomudpbase.cpp:515-529`).
- kh: RX reordering buffer `seqBuf` with a time length (100 ms serial/audio) — out-of-order/duplicate inserts are dropped (`kh seqbuf.go:181-227`), delivery halts while locked, `errOutOfOrder` entries are discarded (`kh seqbuf.go:301-306`). There is no hard entry cap; the time window bounds it.
- Duplicate detection: kh drops entries whose seq equals the newest buffered seq (`kh seqbuf.go:199-201`); wfview uses `rxMissing` removal when the "missing" seq arrives (`wfview src/radio/icomudpbase.cpp:289-301`).

## 10. Keepalives and session loss

**Idle pkt0 (untracked-format 16-byte idle, tracked when periodic):** format `10 00 00 00 00 00 <seq> <sid...>` (`kh pkt0.go:159-167`; is-idle check `kh pkt0.go:104-106`).

- kh control stream: periodic tracked idles. Interval semantics from `pkt0.go`: timer starts at the **1 s** idle interval (`kh pkt0.go:196`); every fire sends one tracked idle and then re-arms at **100 ms** if a real tracked packet was sent within the last 1 s (`pkt0IdleAfter`), else at **1 s** (`pkt0DefaultSendInterval`/`pkt0IdleAfter`/`pkt0IdleSendInterval`, `kh pkt0.go:10-12,177-186`). Any non-idle tracked send resets the timer back to 100 ms immediately (`kh pkt0.go:145-154,172-176`). The serial stream runs the same periodic idle (`kh serialstream.go:232`); the audio stream deliberately does NOT (`kh audiostream.go:165`).
- wfview: plain 100 ms tracked idle always (`IDLE_PERIOD` `wfview include/packettypes.h:9`; armed after "i-am-here" `wfview src/radio/icomudphandler.cpp:263`, on CI-V stream `wfview src/radio/icomudpcivdata.cpp:38`), reset to 100 ms on every tracked send (`wfview src/radio/icomudpbase.cpp:477-479`). No idle backoff.
- The radio likewise expects these to serve as retransmit carriers: "If there are no tracked packets to send, idle pkt0 packets are periodically sent" (`kh pkt0.go:116-117`).

**Ping interval:** wfview 500 ms (`PING_PERIOD` `wfview include/packettypes.h:8`); kh 3 s (§7). The radio pings every 100 ms and must be answered (`kh pkt7.go:11-13,37-45`).

**Session loss:**
- kh: **audio stream silent for 5 s** (`audioTimeoutDuration`, armed at stream start, reset per audio packet) → fatal error → whole session torn down and restarted after 1 s (`kh audiostream.go:11,110-113,132-134`; `kh main.go:12-14,130-141`). Control-stream reauth without answer within 3 s (`reauthTimeout`) only logs "auth timeout, audio/serial stream may stop" — not fatal (`kh controlstream.go:16,304-309`). pkt7 timeout defined (3 s) but never armed (§7). Handshake `expect()` timeout 1 s (`kh streamcommon.go:12,89-95`).
- wfview: **no client-side session-loss teardown**. "are-you-there" retries every 500 ms and only reports "Radio not responding!" after 20 tries (`AREYOUTHERE_PERIOD` `wfview include/packettypes.h:10`; `wfview src/radio/icomudphandler.cpp:85,667-675`). `STALE_CONNECTION 15` is used only by wfview's own server component (`wfview src/radio/icomserver.cpp:1110`), not the client. Radio-initiated loss arrives as the 0x50 packet with error=0 + disc=1 (§5).

## 11. CI-V data stream watchdog (wfview-only; recommended)

- A 500 ms timer (`WATCHDOG_PERIOD`, `wfview include/packettypes.h:11`) checks the last-received time on the CI-V socket; if nothing arrived for **>2000 ms**, a 100 ms timer is started that re-sends the **open variant of the openclose packet (§6, magic open)** every 100 ms until valid CI-V data arrives (`wfview src/radio/icomudpcivdata.cpp:47-65,29-33`).
- The same open packet is sent proactively when the stream's pkt6 ("i-am-ready") answer arrives (`wfview src/radio/icomudpcivdata.cpp:131-141`), and the 100 ms timer is stopped by the first valid CI-V data packet (`wfview src/radio/icomudpcivdata.cpp:152-154`).
- kappanhang has no watchdog — it sends the open packet once and relies on tracked-packet retransmit (`kh serialstream.go:234`). Recommended: implement the wfview watchdog with kh's 0x05 magic.

## 12. SID / station ID derivation (sentid / rcvdid)

- **Client sentid** (4 bytes) is derived per-socket from the local IPv4 address and the socket's local UDP port, keeping only the LAST TWO octets of the IP: `SID = (ip_octet3 << 24) | (ip_octet4 << 16) | (localPort & 0xffff)`.
  - kh: `s.localSID = binary.BigEndian.Uint32(laddr.IP[len(laddr.IP)-4:])<<16 | uint32(laddr.Port&0xffff)` (`kh streamcommon.go:240-242`).
  - wfview: `myId = (addr >> 8 & 0xff) << 24 | (addr & 0xff) << 16 | (localPort & 0xffff)` with `addr = localIP.toIPv4Address()` (`wfview src/radio/icomudpbase.cpp:17-18`) — same logical value.
  - On the wire each implementation writes it big- vs little-endian respectively (§1); treat the 4 bytes as opaque and be self-consistent (recommend kh's BE layout: `[octet3, octet4, port_hi, port_lo]`).
  - The radio's own 4-byte ID is observed to follow the same scheme (e.g. `0xbb 0x41 0x3f 0x2b` = last octets 187.65, port 16171: `kh controlstream.go:80`).
- **rcvdid** comes from the radio during each data stream's mini-handshake: the **i-am-here (0x04) answer** carries the radio's sentid at bytes 8-12; kh: `s.remoteSID = binary.BigEndian.Uint32(r[8:12])` (`kh streamcommon.go:110-120`); wfview: `remoteId = in->sentid` (`wfview src/radio/icomudpbase.cpp:76-86`). It is re-learned on each stream independently and refreshed from the 0x90 conninfo answer (`kh controlstream.go:257`) and from a 0x06 answer on the CI-V stream in wfview (`wfview src/radio/icomudpcivdata.cpp:131-135`).
- **are-you-ready (0x06) packet, both directions** — identical 16-byte layout: `10 00 00 00 06 00 01 00` + sender sentid @8 + peer rcvdid @12 (client send `kh streamcommon.go:122-133`; radio answer example `kh streamcommon.go:137`; wfview sends `sendControl(false, 0x06, 0x01)` immediately after receiving i-am-here: `wfview src/radio/icomudpbase.cpp:85`). Sent twice by kh (`kh streamcommon.go:126-131`). The expected answer must match bytes 0-7 = `10 00 00 00 06 00 01 00` (`kh streamcommon.go:138`).

## 13. Disconnect packet (type 0x0005)

- Layout: standard 16-byte header, `len`=0x10, `type`=0x0005, `seq`=0x0000, sentid, rcvdid. kh: `kh streamcommon.go:199-211`; wfview: destructor `sendControl(false, 0x05, 0x00)` `wfview src/radio/icomudpbase.cpp:31,394-413`.
- kh sends it **twice** (two `s.send(p)` calls) — do the same.
- kh sends it per stream (control, serial, audio) during teardown, only if the radio SID was learned (`kh streamcommon.go:253-256`).
- kh clean-shutdown order: (1) deauth 0x40/requesttype 0x01 on control (only if authID+remoteSID known), (2) sleep 500 ms so the radio can still request retransmits, (3) serial stream: openclose-close (§6) then disconnect ×2 then socket close, (4) audio + control disconnect ×2 (`kh controlstream.go:380-404`; `kh serialstream.go:259-271`; `kh streamcommon.go:251-265`).
- wfview order: civ/audio destructors send openclose-close; handler sends token-removal 0x01; each base destructor sends disconnect 0x05 (`wfview src/radio/icomudphandler.cpp:88-126`; `wfview src/radio/icomudpcivdata.cpp:42-45`; `wfview src/radio/icomudpbase.cpp:27-35`).

## 14. Login failure / busy radio

- **Bad credentials:** the 0x60 login answer carries `error` u32 @0x30 = wire bytes `fe ff ff ff` (0xfffffffe read LE / wfview `in->error == 0xfeffffff`). kh: `bytes.Equal(r[48:52], []byte{0xff,0xff,0xff,0xfe})` → error "invalid username/password" (`kh controlstream.go:346-352`; example answer `kh controlstream.go:334-345`). wfview: emits "Invalid Username/Password" (`wfview src/radio/icomudphandler.cpp:438-442`). kappanhang then exits with status 1 without retrying (`kh main.go:58-64`).
- **Session held / radio busy (second client or stale session):** the 0x50 status answer (§5) carries `error` @0x30 starting `ff ff ff` (0xffffffff) → kh: "auth failed, try rebooting the radio" (before the stream is open) / "auth failed" (`kh controlstream.go:219-224`); wfview: "Connection failed — try rebooting the radio" (`wfview src/radio/icomudphandler.cpp:356-359`). wfview detects this only when `!streamOpened`.
- **Radio-initiated disconnect:** 0x50 with error=0 and `disc`=1 @0x40 — kh "got radio disconnected" (immediate reconnect allowed, `kh controlstream.go:225-227`, `kh main.go:88-92`); wfview tears down the civ/audio sub-streams but keeps the control session (`wfview src/radio/icomudphandler.cpp:361-379`).
- **Token renewal rejected:** wfview only — 0x40 answer `response` @0x30 = 0xffffffff → re-run the conninfo/login path with the SIDs/tokrequest/token copied from the rejection (`wfview src/radio/icomudphandler.cpp:334-344`). kh ignores the response code (`kh controlstream.go:201-204`). UNVERIFIED against real firmware; prefer treating 0xffffffff @0x30 on the 0x40 answer as "re-login".

## 15. Local port selection and control-socket lifecycle

- **CI-V (serial) data socket:** client picks the local port declared @0x7c of the conninfo request; the radio sends CI-V packets from its @0x42 port to that local port, and the client sends to `radio:@0x42` (wfview) or `radio:50002` (kh).
  - kh binds local port **50002 = same number as the remote port** — `net.DialUDP("udp", &net.UDPAddr{Port: portNumber}, raddr)` with `portNumber = serialStreamPort` (`kh streamcommon.go:226-242`, `kh controlstream.go:12,137`). Same for audio (50003) and control (50001).
  - wfview grabs two free ports by transiently binding a throwaway socket twice (`wfview src/radio/icomudphandler.cpp:587-599`), declares them @0x7c/0x80, then binds the real sockets to them (`wfview src/radio/icomudpcivdata.cpp:13`; `wfview src/radio/icomudpbase.cpp:4-16`).
  - Both approaches work; the port numbers only matter in that radio and client must agree (the radio replies to the peer address it streams to, so any client port is acceptable as long as it is declared @0x7c).
- **Control-stream socket:** ONE UDP socket, created before any packet, connected to `radio:50001` (`controlStreamPort`, `kh controlstream.go:11`; `kh streamcommon.go:226-235`). It carries: pkt3/4/6, login+answers, auth/token packets, conninfo request+answers, 0x50 status, pkt0 idles/retransmit, pkt7 pings — and lives for the whole session. wfview binds an ephemeral local port for it (`icomUdpHandler::init` → `icomUdpBase::init(0)` → `udp->bind(0)`: `wfview src/radio/icomudphandler.cpp:66`, `wfview src/radio/icomudpbase.cpp:4-16`) and receives the radio→client answers on that same socket. All auth-layer "answers" come back to this socket's source address.
- **Default radio ports** (RS-BA1 convention, both implementations): control 50001, CI-V/serial 50002, audio 50003 (`kh controlstream.go:11-13`; wfview prefs default the same via `udpPreferences`, `wfview include/icomudpbase.h:29-42`).
- Each additional stream (serial, audio) runs the SAME pkt3→pkt4→pkt6→pkt6 mini-handshake on its OWN socket before use (`kh streamcommon.go:213-224`, called from `kh serialstream.go:226` and `kh audiostream.go:160`).

## 16. Minimal happy-path handshake packet log (control stream → CI-V stream open)

Synthesized from `kh controlstream.go:317-378`, `kh streamcommon.go:213-224`, `kh serialstream.go:212-256`. `C→R` client→radio, `R→C` radio→client. All on radio:50001 unless noted. Tracked `seq` shown where known.

| # | dir | packet | size | key fields |
|---|-----|--------|-----:|------------|
| 1 | C→R | pkt3 are-you-there (×2) | 16 | type 0x0003, seq 0 (`kh streamcommon.go:97-108,214`) |
| 2 | R→C | pkt4 i-am-here | 16 | type 0x0004; **radio sentid @8-12** → remoteSID (`kh streamcommon.go:117`) |
| 3 | C→R | pkt6 are-you-ready (×2) | 16 | type 0x0006, seq 0x0001 (`kh streamcommon.go:122-133`) |
| 4 | R→C | pkt6 i-am-ready | 16 | type 0x0006, seq 0x0001 (`kh streamcommon.go:135-140`) |
| 5 | C→R | login | 128 (0x80) | tracked seq 1; innerseq 0; random tokrequest @0x1a; encoded user @0x40, pass @0x50; `icom-pc` @0x60 (`kh controlstream.go:41-76`) |
| 6 | R→C | login answer | 96 (0x60) | requestreply 0x02 @0x14; tokrequest+token echo @0x1a-0x1f; **error @0x30 must not be `fe ff ff ff`**; connection string @0x40 (`kh controlstream.go:346-352`) |
| — | C→R | pkt7 ping every 3 s starts | 21 | first seq 2 (`kh controlstream.go:354`) |
| 7 | C→R | auth, requesttype 0x02 | 64 (0x40) | authID = answer#6 bytes 0x1a-0x1f; innerseq 1 (`kh controlstream.go:356-360`) |
| — | C→R | pkt0 tracked idle every 100 ms/1 s starts | 16 | (`kh controlstream.go:363`) |
| 8 | C→R | auth, requesttype 0x05 (renewal) | 64 | innerseq 2 (`kh controlstream.go:365-368`) |
| 9 | R→C | capabilities | 168 (0xa8) | arrives asynchronously; client takes guid = r[66:82] (`kh controlstream.go:159-185`) |
| 10 | R→C | auth answer | 64 | requestreply 0x02 @0x14, requesttype 0x05 @0x15 → auth OK (`kh controlstream.go:186-205`) |
| 11 | C→R | conninfo stream request | 144 (0x90) | requesttype 0x03; guid @0x20; `IC-9700` @0x40; user @0x60; caps 01 01 04 04 @0x70; 48000 @0x74/0x78; **local serial port @0x7c, local audio port @0x80** (u32 BE); txbuffer 300 @0x84; convert 1 @0x88 (`kh controlstream.go:112-147`); 5 s answer watchdog armed (`kh controlstream.go:370-372`) |
| 12 | R→C | status | 80 (0x50) | error @0x30 (must be 0), disc @0x40; radio-side serial port @0x42 BE, audio port @0x46 BE (kh ignores these two; wfview uses them: `wfview src/radio/icomudphandler.cpp:381-382`) |
| 13 | R→C | conninfo answer | 144 (0x90) | r[96]==1 required; radio name @0x40; **refresh remoteSID @8-12, localSID @12-16, authID @0x1a-0x1f** (`kh controlstream.go:230-282`) |
| 14 | C→R | (new socket radio:50002, local 50002) pkt3 ×2 | 16 | type 0x0003 (`kh serialstream.go:213-226`) |
| 15 | R→C | pkt4 (serial stream) | 16 | radio serial-stream sentid → that stream's remoteSID |
| 16 | C→R | pkt6 ×2 | 16 | seq 1 |
| 17 | R→C | pkt6 answer (serial) | 16 | |
| — | C→R | pkt7 (seq from 1) + pkt0 idles start on serial stream | | (`kh serialstream.go:230-232`) |
| 18 | C→R | **openclose OPEN** | 22 (0x16) | data `c0 01` @0x10; CI-V sendseq BE @0x13; magic 0x05 @0x15 (`kh serialstream.go:46-63,234`) |
| 19 | R→C | CI-V data packets | 0x15+n | 0xc1 @0x10, datalen @0x11, sendseq BE @0x13, `fe fe .. fd` from 0x15 (`kh serialstream.go:112-118`) |

(Audio stream radio:50003 runs steps 14-17 in parallel and then just streams audio — no openclose: `kh audiostream.go:151-179`.)

Teardown: deauth 0x40/0x01 on control → 500 ms wait → openclose-close + disconnect ×2 on serial → disconnect ×2 on audio and control (`kh controlstream.go:380-404`).

## 17. Misc facts useful to the implementer

- IC-9700 CI-V address = 0xA2 (`wfview rigs/IC-9700.rig:7` `CIVAddress=162`); controller address in CI-V frames = 0xE0 (all kh commands use `224`: e.g. `kh civcontrol.go:829,1017`). kh's default address is 0xA4 (IC-705), settable via `-c` (`kh args.go:59-64`).
- CI-V frames are extracted before any local processing; kh forwards everything except frames it consumes internally (`kh serialstream.go:93-104`, `kh civcontrol.go:171-216`).
- Audio TX framing (if ever needed): two tracked packets per 20 ms frame — 1364 B then 556 B PCM, headers per §2 (`kh audiostream.go:137-143`).
- Everything above is UDP; there is no TCP anywhere in this protocol.
