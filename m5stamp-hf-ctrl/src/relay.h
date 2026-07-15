// relay.h — thin relay HAL over the M5StamPLC library.
//
// The M5 Stamp PLC (StamPLC K141) is an ESP32-S3 with 4 relays + 8 digital
// inputs on an AW9523B I²C expander @0x59, accessed via the official M5StamPLC
// Arduino library. The library declares a global `M5StamPLC` (type
// m5::M5_STAMPLC); this HAL wraps its relay calls so the rest of the firmware
// never touches a raw channel number or library call directly. If the library
// API changes across versions, only this file changes.
//
// Library API (M5StamPLC.h):
//   M5StamPLC.begin();
//   M5StamPLC.writePlcRelay(channel /*0..3*/, state /*bool*/);
//   bool M5StamPLC.readPlcRelay(channel /*0..3*/);
//   bool M5StamPLC.readPlcInput(channel /*0..7*/);   // digital inputs (unused now)
#pragma once

#include <M5StamPLC.h>

// relayInit prepares the PLC library (expander + relays). All relays start
// DE-ENERGIZED (open) — the pa-arm fail-safe-open state on cold boot.
inline void relayInit() {
    M5StamPLC.begin();
    for (int ch = 0; ch < 4; ++ch) {
        M5StamPLC.writePlcRelay(ch, false);  // open on boot
    }
}

// relaySet energizes (on=true) or de-energizes (on=false) a relay channel.
inline void relaySet(int ch, bool on) { M5StamPLC.writePlcRelay(ch, on); }

// relayGet reads the relay position back (best-effort readback). Returns false
// on any read failure (fail-safe: treat as open).
inline bool relayGet(int ch) { return M5StamPLC.readPlcRelay(ch); }