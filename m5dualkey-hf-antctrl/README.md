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

### Chain Key — DVK 2

An M5Stack **Chain Key** on either HY2.0-4P Chain-bus port (both are probed;
hot-plug is handled) plays **DVK memory 2** on the FLEX via `muehle/hf/radio`
(`flexbridge`). DVK playback keys the transmitter.

| Chain Key | Publishes to `muehle/hf/radio/cmd` (not retained) |
|-----------|---------------------------------------------------|
| Press, DVK idle | `{"action":"dvk_play_2"}` |
| Press, any DVK memory playing | `{"action":"dvk_stop"}` |
| Long press (3 s) | `{"action":"dvk_stop"}` |

Press is ignored while the DVK is recording/previewing/disabled, and while the
radio link is not live (`hf/radio/status` online AND `/state.device_online`).
Chain Key LED: red = DVK 2 on the air, amber = another memory playing, dim
green = ready, off = radio link down. The memory number is `DVK_MEMORY` in
`include/config.h`.

### LEDs (Key A / B)

The LEDs follow `/state.direction`: forward = A green, reverse = B red,
bidirectional = A + B orange; alternating white while `/state.moving`. The
LEDs stay off, and key presses are not sent, unless the bridge `/status` is
`online` **and** `/state.device_online` is true (two-layer liveness).

> **History.** Written 2026-06 as `m5btn2` against the pre-stationa
> `ubctrl/*` topics; ported to `muehle/hf/ant-ctrl` on 2026-10-03.
> `REQUIREMENTS.md` §3 still describes the old binding.

## Build / flash

```bash
cp include/secrets.example.h.x include/secrets.h   # then fill in (incl. OTA password)
./deploy.sh usb      # first flash / recovery over USB-C
./deploy.sh          # routine update over the air (m5dualkey-antctrl-1.local)
./deploy.sh ota 192.168.1.x   # OTA to an explicit IP if mDNS fails
```

OTA (ArduinoOTA, password-protected) starts once WiFi first associates, so
only the first flash needs USB. If the Mac shows no `/dev/cu.usbmodem*`, use a
data cable, or hold Key A (GPIO0) while plugging in to force download mode.
Serial log: `pio device monitor`. Its `OTA,LISTENING` line gives the IP.

Pins: Key A = GPIO0, Key B = GPIO17, LED data = GPIO21, LED power = GPIO40.
Requirements and test checklist: `REQUIREMENTS.md`, `test/VALIDATION_LOG.md`.
