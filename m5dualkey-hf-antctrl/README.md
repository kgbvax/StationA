# m5dualkey-hf-antctrl — M5Stack Chain DualKey Ultrabeam direction keys

Firmware for an [M5Stack Chain DualKey](https://docs.m5stack.com/en/chain/Chain_DualKey)
(ESP32-S3, two keys with RGB LEDs) that works as a two-button **Ultrabeam
direction control**. It is a consumer and command stimulator, not a slot.

It drives slot `muehle/hf/ant-ctrl` (`ultrabridge`, Ultrabeam RCU-06); see
`../ultrabridge/ultrabeam-mqtt-api.md`.

| Input | Publishes to `muehle/hf/ant-ctrl/cmd` (not retained) |
|-------|------------------------------------------------------|
| Key A | `{"action":"direction","value":"forward"}` |
| Key B | `{"action":"direction","value":"reverse"}` |
| A + B (within 100 ms) | `{"action":"direction","value":"bidirectional"}` |
| A + B held 5 s | reboots the key |

The LEDs follow `/state.direction`: forward = A green, reverse = B red,
bidirectional = A + B orange; alternating white while `/state.moving`. The
LEDs stay off, and key presses are not sent, unless the bridge `/status` is
`online` **and** `/state.device_online` is true (two-layer liveness).

> **History.** Written 2026-06 as `m5btn2` against the pre-stationa
> `ubctrl/*` topics; ported to `muehle/hf/ant-ctrl` on 2026-10-03.
> `REQUIREMENTS.md` §3 still describes the old binding.

## Build / flash

```bash
cp include/secrets.example.h.x include/secrets.h   # then fill in
pio run -e m5stack_chain_dualkey
pio run -e m5stack_chain_dualkey -t upload          # USB-C
```

Pins: Key A = GPIO0, Key B = GPIO17, LED data = GPIO21, LED power = GPIO40.
Requirements and test checklist: `REQUIREMENTS.md`, `test/VALIDATION_LOG.md`.
