// ui.h — front-panel LCD for the M5 Stamp PLC (240x135, landscape).
//
// The screen is three columns, one per physical button below it:
//
//     A  PA TX enable   — the arm PERMIT (never forces the relay; arm.h decides)
//     B  PA power       — PA remote-on relay
//     C  TRX power      — TRX remote-on relay
//
// Each column is a lit window whose colour is its state, with a key badge at the
// foot that repeats the button letter and says what a press does now. A strip
// across the top carries the interlock summary (why TX is held, or "gates clear")
// and the link state.
//
// Pure rendering: no Arduino, no globals. main.cpp builds a Model from live state;
// tools/lcd-preview renders the same code on a desktop. The includer must have
// pulled in the graphics library (M5GFX) first.
#pragma once

#include <stdint.h>
#include <string.h>

namespace ui {

constexpr int SCREEN_W = 240;
constexpr int SCREEN_H = 135;

// Horizontal centre of each physical button, in screen pixels. The three buttons
// are assumed evenly spaced under the screen; if the case puts them elsewhere,
// change these three numbers and nothing else.
constexpr int BTN_CX[3] = {40, 120, 200};

constexpr int STRIP_H = 20;
constexpr int TILE_W = 74;
constexpr int TILE_Y = 24;
constexpr int TILE_H = SCREEN_H - TILE_Y - 3;
constexpr int TILE_R = 8;

// Palette (0xRRGGBB).
constexpr uint32_t INK = 0x0D1013;      // screen
constexpr uint32_t SLATE = 0x1C2228;    // idle window
constexpr uint32_t SLATE_EDGE = 0x333D46;
constexpr uint32_t MIST = 0x93A0AB;     // idle text
constexpr uint32_t PAPER = 0xE6EBEF;    // key badge, text on dark
constexpr uint32_t GO = 0x2FCB73;       // energised / armed
constexpr uint32_t CAUTION = 0xF2B134;  // waiting / held by a gate
constexpr uint32_t ALARM = 0xE5484D;    // held by a fault

enum Tx : uint8_t {
    TX_OFF,      // permit off
    TX_WAIT,     // permit on, nothing blocking, arming (dwell / waiting for RX)
    TX_BLOCKED,  // permit on, a gate holds the arm open
    TX_ARMED,    // arm relay closed
};
enum Tone : uint8_t { TONE_OK, TONE_CAUTION, TONE_ALARM };
enum Link : uint8_t { LINK_NONE, LINK_WIFI, LINK_MQTT };

struct Model {
    Tx tx = TX_OFF;
    bool txFault = false;  // TX_BLOCKED because of a fault, not a normal gate
    bool paOn = false;
    bool trxOn = false;
    Link link = LINK_NONE;
    Tone gateTone = TONE_OK;
    char gate[32] = "";  // interlock summary; never empty when painted
};

// What was last drawn, so only the parts that changed are repainted — no flicker,
// and the redraw is free to run every few ms.
struct Painter {
    Model last;
    bool painted = false;
};

namespace detail {

using Gfx = lgfx::v1::LGFXBase;
namespace fnt = lgfx::v1::fonts;
namespace dat = lgfx::v1::textdatum;

struct Look {
    uint32_t fill, edge, title, state;
};

inline void text(Gfx& d, const lgfx::v1::IFont* f, uint32_t color, int x, int y,
                 lgfx::v1::textdatum::textdatum_t datum, const char* s) {
    d.setFont(f);
    d.setTextSize(1);
    d.setTextColor(color);
    d.setTextDatum(datum);
    d.drawString(s, x, y);
}

// Largest of the candidates (biggest first) that fits maxW; the smallest if none.
inline const lgfx::v1::IFont* fit(Gfx& d, const char* s, int maxW,
                                  const lgfx::v1::IFont* const* cand, int n) {
    for (int i = 0; i < n; ++i) {
        d.setFont(cand[i]);
        d.setTextSize(1);
        if (d.textWidth(s) <= maxW) return cand[i];
    }
    return cand[n - 1];
}

inline void strip(Gfx& d, const Model& m) {
    d.fillRect(0, 0, SCREEN_W, STRIP_H, INK);

    const char* word = m.link == LINK_MQTT ? "MQTT" : m.link == LINK_WIFI ? "WiFi" : "no link";
    uint32_t lc = m.link == LINK_MQTT ? GO : m.link == LINK_WIFI ? CAUTION : ALARM;
    d.setFont(&fnt::DejaVu9);
    d.setTextSize(1);
    int ww = d.textWidth(word);
    int right = SCREEN_W - 5;
    text(d, &fnt::DejaVu9, lc, right, STRIP_H / 2, dat::middle_right, word);
    d.fillCircle(right - ww - 7, STRIP_H / 2, 3, lc);

    uint32_t gc = m.gateTone == TONE_ALARM ? ALARM : m.gateTone == TONE_CAUTION ? CAUTION : MIST;
    static const lgfx::v1::IFont* const sizes[] = {&fnt::DejaVu12, &fnt::DejaVu9};
    int avail = right - ww - 7 - 8 - 6;
    text(d, fit(d, m.gate, avail, sizes, 2), gc, 6, STRIP_H / 2, dat::middle_left, m.gate);
}

// One column: lit window (acronym, noun, state word, optional sub-state) over a
// key badge ("A  turn off").
inline void tile(Gfx& d, int col, const char* acronym, const char* noun, const char* state,
                 const char* sub, bool idle, const Look& look, char key, const char* verb) {
    const int cx = BTN_CX[col];
    const int x = cx - TILE_W / 2;
    const int y = TILE_Y;

    d.fillRect(x - 3, y, TILE_W + 6, TILE_H + 3, INK);  // clear the column: no old edge pixels survive
    d.fillSmoothRoundRect(x, y, TILE_W, TILE_H, TILE_R, look.fill);
    if (idle) d.drawRoundRect(x, y, TILE_W, TILE_H, TILE_R, look.edge);

    text(d, &fnt::FreeSansBold9pt7b, look.title, cx, y + 6, dat::top_center, acronym);
    text(d, &fnt::DejaVu12, look.title, cx, y + 24, dat::top_center, noun);
    text(d, &fnt::DejaVu24, look.state, cx, y + 50, dat::middle_center, state);
    if (sub[0]) text(d, &fnt::DejaVu12, look.state, cx, y + 71, dat::middle_center, sub);

    // Key badge: a small keycap with the button letter, then what a press does.
    const int py = y + TILE_H - 4 - 22;
    d.fillSmoothRoundRect(x + 3, py, TILE_W - 6, 22, 6, INK);
    d.setFont(&fnt::DejaVu9);
    d.setTextSize(1);
    const int group = 16 + 4 + d.textWidth(verb);
    const int bx = cx - group / 2;
    d.fillSmoothRoundRect(bx, py + 3, 16, 16, 4, PAPER);
    char k[2] = {key, 0};
    text(d, &fnt::DejaVu12, INK, bx + 8, py + 11, dat::middle_center, k);
    text(d, &fnt::DejaVu9, PAPER, bx + 20, py + 11, dat::middle_left, verb);
}

// A is a toggle for the permit, so the big word is the permit (ON/OFF); the
// sub-state is how the interlock answered it.
inline void txTile(Gfx& d, const Model& m) {
    Look look{SLATE, SLATE_EDGE, MIST, MIST};
    bool idle = true;
    const char* sub = "";
    switch (m.tx) {
        case TX_OFF:
            break;
        case TX_WAIT:
            look = Look{CAUTION, CAUTION, INK, INK};
            idle = false;
            sub = "WAIT";
            break;
        case TX_BLOCKED:
            look = m.txFault ? Look{ALARM, ALARM, PAPER, PAPER} : Look{CAUTION, CAUTION, INK, INK};
            idle = false;
            sub = "BLOCKED";
            break;
        case TX_ARMED:
            look = Look{GO, GO, INK, INK};
            idle = false;
            sub = "ARMED";
            break;
    }
    tile(d, 0, "PA TX", "enable", idle ? "OFF" : "ON", sub, idle, look, 'A',
         idle ? "turn on" : "turn off");
}

inline void powerTile(Gfx& d, int col, const char* acronym, bool on, char key) {
    Look look = on ? Look{GO, GO, INK, INK} : Look{SLATE, SLATE_EDGE, MIST, MIST};
    tile(d, col, acronym, "power", on ? "ON" : "OFF", "", !on, look, key,
         on ? "turn off" : "turn on");
}

}  // namespace detail

// paint draws m, repainting only what differs from the previous call.
inline void paint(detail::Gfx& d, const Model& m, Painter& p) {
    const Model& o = p.last;
    const bool all = !p.painted;
    if (all) d.fillScreen(INK);
    if (all || m.link != o.link || m.gateTone != o.gateTone || strcmp(m.gate, o.gate) != 0)
        detail::strip(d, m);
    if (all || m.tx != o.tx || m.txFault != o.txFault) detail::txTile(d, m);
    if (all || m.paOn != o.paOn) detail::powerTile(d, 1, "PA", m.paOn, 'B');
    if (all || m.trxOn != o.trxOn) detail::powerTile(d, 2, "TRX", m.trxOn, 'C');
    p.last = m;
    p.painted = true;
}

}  // namespace ui
