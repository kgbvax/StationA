// face.h — the analog meter face: the whole point of this device.
//
// A 1970s-style needle meter, not a UI. A fixed compass card: azimuth 0..360,
// one screen-degree per azimuth-degree, N at 12 o'clock — the way the operator
// thinks (az 390 renders at the 30° mark; azimuth wraps on the display even
// though the rotator's command space is linear 0..450 underneath). Because
// every screen position goes through sinf/cosf (periodic), raw values above
// 360 land on the right compass position with no special-casing; only the
// plaque flags the overlap pass with a "+360" hint so the operator still knows
// which way the antenna will travel. The needle is a black tapered pointer
// damped in main.cpp (exponential smoothing per 33 ms frame) so it sweeps
// like physical meter movement. The target is a thin red pointer that jumps
// instantly (a set point, not a measurement). Liveness is a ring: green OK,
// amber = rotator-link down, red = bridge/MQTT down, gray = no data yet.
//
// Rendering: one full-frame 240x240 RGB565 sprite (~115 KB, allocated once in
// faceInit), every layer redrawn per repaint (~60 primitives — cheap on the
// ESP32-S3), pushed only when the face actually changed (main.cpp gates).
// If the sprite allocation fails, the same draw code renders straight to the
// display via the template parameter (slower, no double buffer — bench-only
// fallback, never expected on an S3 with 512 KB SRAM).
#pragma once

#include <Arduino.h>
#include <M5Dial.h>

// What the face renders this frame — built by main.cpp.
struct FaceModel {
    float az;         // actual azimuth from /state (0..450, wrapped for display)
    float displayAz;  // damped needle azimuth (rendered; wrapped by trig)
    float target;     // local target azimuth (0..450, wrapped for display)
    bool moving;      // rotator reports motion
    bool linkOk;      // MQTT + bridge online + WRC link up + state seen
    bool bridgeUp;    // MQTT connected AND /status == "online"
    bool haveState;   // at least one /state received
    bool sim;         // desk-simulation build (no WiFi/MQTT; local mast model)
};

// Classic meter triad: cream card, black ink, red target.
#define COL(r, g, b) \
    ((uint16_t)(((r) & 0xF8) << 8) | (((g) & 0xFC) << 3) | ((b) >> 3))
static constexpr uint16_t C_CARD = COL(247, 242, 225);  // cream scale card
static constexpr uint16_t C_PLAQUE = COL(225, 219, 200);  // readout plaque bg
static constexpr uint16_t C_INK = COL(35, 31, 28);      // ticks, numerals, needle
static constexpr uint16_t C_INK_SOFT = COL(120, 114, 104);  // minor ticks
static constexpr uint16_t C_RED = COL(205, 32, 32);     // target pointer, arcs
static constexpr uint16_t C_OK = COL(0, 185, 60);
static constexpr uint16_t C_AMBER = COL(240, 165, 20);
static constexpr uint16_t C_DOWN = COL(215, 45, 45);
static constexpr uint16_t C_GRAY = COL(140, 140, 140);
static constexpr uint16_t C_WHITE = COL(250, 250, 250);

class Face {
   public:
    // Returns true if the sprite was allocated (double-buffered).
    bool init(M5GFX& display) {
        display_ = &display;
        canvas_.setColorDepth(16);
        if (canvas_.createSprite(240, 240)) {
            useCanvas_ = true;
            return true;
        }
        useCanvas_ = false;  // draw directly to the display (bench fallback)
        return false;
    }

    void render(const FaceModel& m) {
        if (useCanvas_) {
            draw(&canvas_, m);
            canvas_.pushSprite(display_, 0, 0);  // parent passed explicitly
        } else {
            draw(display_, m);
        }
    }

   private:
    M5GFX* display_ = nullptr;
    M5Canvas canvas_;
    bool useCanvas_ = false;

    // Azimuth → screen angle: 1:1, clockwise from 12 o'clock — a compass card.
    // Raw azimuths above 360 need no wrapping here: polX/polY go through
    // sinf/cosf (periodic), so az 390 lands on the 30° mark by itself.
    static constexpr float CX = 120.0f;
    static constexpr float CY = 120.0f;
    static constexpr float A2S = 1.0f;  // screen degrees per compass degree

    // Point at azimuth `az` (azimuth-degrees), radius `r` from the center.
    // r < 0 extends backwards through the hub (needle tail).
    static float polX(float az, float r) {
        return CX + r * sinf(az * A2S * DEG_TO_RAD);
    }
    static float polY(float az, float r) {
        return CY - r * cosf(az * A2S * DEG_TO_RAD);
    }

    template <typename G>
    void draw(G* g, const FaceModel& m) {
        drawRing(g, m);
        drawCard(g);
        drawScale(g);
        if (m.moving && fabsf(m.target - m.az) > 0.5f) drawMovingArc(g, m);
        drawTargetPointer(g, m.target);
        drawNeedle(g, m.displayAz);
        drawPlaque(g, m);  // last before the badge: the readout window sits ON
                          // TOP of the needle/target — like a real meter, the
                          // pointer passes behind the cutout window
        if (!m.linkOk) drawBadge(g, m);
        if (m.sim) drawSimLabel(g);  // never overlaps the badge: the sim is
                                     // always linkOk, the badge never is
    }

    // The desk build must never be mistaken for a live head: a small SIM tag
    // between the N mark and the hub, in badge position but soft ink — the
    // operator knows the antenna is a model without reading the plaque.
    // TRANSPARENT background: unlike the badge (drawn when the face is parked,
    // so its opaque cell never fights a live needle), the sim face slews
    // through north all the time — an opaque cell here would punch a moving
    // gap in the needle shaft on every pass.
    template <typename G>
    void drawSimLabel(G* g) {
        g->setTextDatum(middle_center);
        g->setFont(&fonts::Font0);
        g->setTextColor(C_INK_SOFT);
        g->drawString("SIM", (int)CX, (int)CY - 32);
    }

    // The readout window zone (plaque): scale text skips it,
    // exactly like the cutout window of a real meter card. Ticks stay — the
    // window's outer corners barely graze the tick ring.
    static bool inReadoutWindow(float x, float y) {
        return x > CX - 64.0f && x < CX + 64.0f && y > CY + 40.0f &&
               y < CY + 88.0f;
    }

    template <typename G>
    void drawRing(G* g, const FaceModel& m) {
        uint16_t col;
        if (!m.haveState) {
            col = C_GRAY;
        } else if (!m.bridgeUp) {
            col = C_DOWN;
        } else if (!m.linkOk) {  // bridge up but WRC link down
            col = C_AMBER;
        } else {
            col = C_OK;
        }
        // Full circle in the ring color; the card fill covers the inside.
        // fillArc, NOT drawArc — M5GFX drawArc is outline-only (1-px boundary
        // arcs and radial hairlines, the annular interior stays unfilled).
        g->fillArc((int)CX, (int)CY, 115, 119, 0, 360, col);
    }

    template <typename G>
    void drawCard(G* g) {
        // Plain compass card: no overlap band — the 360..450 pass renders as
        // its wrapped compass position (periodic trig) and the plaque's
        // "+360" hint carries the pass information instead.
        g->fillCircle((int)CX, (int)CY, 112, C_CARD);
    }

    template <typename G>
    void drawScale(G* g) {
        for (int az = 0; az < 360; az += 10) {
            bool major = (az % 30) == 0;
            float rIn = major ? 96.0f : 103.0f;
            float rOut = 110.0f;
            uint16_t col = major ? C_INK : C_INK_SOFT;
            g->drawLine(polX(az, rIn), polY(az, rIn), polX(az, rOut),
                        polY(az, rOut), col);
        }
        // Numerals every 30 compass-degrees (0 … 330; 360 is 0 — the clash
        // is exactly the ambiguity the compass card removes). The readout
        // window's cutout skips the 150/180/210 numerals (inReadoutWindow).
        g->setTextDatum(middle_center);
        g->setFont(&fonts::Font0);
        g->setTextColor(C_INK, C_CARD);
        for (int az = 0; az < 360; az += 30) {
            if (inReadoutWindow(polX(az, 84), polY(az, 84))) continue;
            char label[8];
            snprintf(label, sizeof(label), "%d", az);
            g->drawString(label, polX(az, 84), polY(az, 84));
        }
        // Cardinal letters at their true compass positions (S falls inside
        // the readout window — a real meter card omits it there too).
        g->setFont(&fonts::Font2);
        struct { int az; const char* s; } cardinals[] = {
            {0, "N"}, {90, "E"}, {180, "S"}, {270, "W"}};
        for (auto& c : cardinals) {
            if (inReadoutWindow(polX(c.az, 68), polY(c.az, 68))) continue;
            g->drawString(c.s, polX(c.az, 68), polY(c.az, 68));
        }
    }

    template <typename G>
    void drawMovingArc(G* g, const FaceModel& m) {
        // Pending sweep from the measured azimuth toward the target, drawn
        // in the direction the rotator travels (CW when the target sits
        // above the azimuth, CCW below — the arc's far end MUST coincide
        // with the target pointer, or it points away from the real motion).
        // CW: interval [az, az+sweep]. CCW: the same pixels as the interval
        // [az-sweep, az] drawn the other way. The magnitude is capped mod
        // 360 so a >360 command does not wrap the pen around the ring; the
        // plaque's "+360" hint carries the rest. An exact full turn still
        // owed paints the full ring.
        float d = m.target - m.az;
        float sweep = fmodf(fabsf(d), 360.0f);
        if (sweep < 0.5f) sweep = 360.0f;  // exact multiple: full pass owed
        float aLo, aHi;
        if (d >= 0.0f) {
            aLo = m.az * A2S - 90.0f;  // fillArc: 0 = 3 o'clock
            aHi = aLo + sweep;
        } else {
            aHi = m.az * A2S - 90.0f;
            aLo = aHi - sweep;
        }
        // Keep both endpoints inside [0, 360): the span is preserved, and
        // the call stays safe however the library treats raw angles.
        while (aLo < 0.0f) {
            aLo += 360.0f;
            aHi += 360.0f;
        }
        while (aLo >= 360.0f) {
            aLo -= 360.0f;
            aHi -= 360.0f;
        }
        if (sweep >= 359.5f) {
            aLo = 0.0f;
            aHi = 360.0f;
        }
        g->fillArc((int)CX, (int)CY, 103, 106, aLo, aHi, C_RED);
    }

    template <typename G>
    void drawPlaque(G* g, const FaceModel& m) {
        g->fillRect((int)CX - 60, (int)CY + 44, 120, 32, C_PLAQUE);
        g->setTextDatum(middle_center);
        g->setFont(&fonts::Font2);
        char line[20];
        // Both lines truncate (not round) the wrapped value: rounding could
        // print "360", the exact wrap-boundary ambiguity the compass card
        // exists to remove. One decimal on both — detents are 22.5°, so
        // targets (and their relative step chains) carry a .5.
        int az10 = (int)(fmodf(m.az, 360.0f) * 10.0f);
        snprintf(line, sizeof(line), "AZ %d.%d", az10 / 10, az10 % 10);
        g->setTextColor(C_INK, C_PLAQUE);
        g->drawString(line, (int)CX - 6, (int)CY + 52);
        int tgt10 = (int)(fmodf(m.target, 360.0f) * 10.0f);
        snprintf(line, sizeof(line), "TGT %d.%d", tgt10 / 10, tgt10 % 10);
        g->setTextColor(C_RED, C_PLAQUE);
        g->drawString(line, (int)CX - 6, (int)CY + 68);
        // Overlap hint: the raw value rides the 360..450 pass — the wrapped
        // number alone does not say which way the antenna will travel.
        g->setFont(&fonts::Font0);
        g->setTextColor(C_INK_SOFT, C_PLAQUE);
        if (m.az >= 360.0f) g->drawString("+360", (int)CX + 48, (int)CY + 52);
        if (m.target >= 360.0f) g->drawString("+360", (int)CX + 48, (int)CY + 68);
    }

    template <typename G>
    void drawTargetPointer(G* g, float target) {
        float theta = target * A2S * DEG_TO_RAD;  // from top, clockwise
        float dx = cosf(theta), dy = sinf(theta);  // perpendicular unit
        g->drawLine(polX(target, 60), polY(target, 60), polX(target, 98),
                    polY(target, 98), C_RED);
        // Arrowhead at the outer end.
        float tipX = polX(target, 106), tipY = polY(target, 106);
        float bx = polX(target, 98), by = polY(target, 98);
        g->fillTriangle((int)tipX, (int)tipY,
                        (int)(bx + 3.0f * dx), (int)(by + 3.0f * dy),
                        (int)(bx - 3.0f * dx), (int)(by - 3.0f * dy), C_RED);
    }

    template <typename G>
    void drawNeedle(G* g, float needleAz) {
        float theta = needleAz * A2S * DEG_TO_RAD;  // from top, clockwise
        float dx = cosf(theta), dy = sinf(theta);   // perpendicular unit
        // Tapered pointer: half-width 3 px at the hub → 1 px tip at r=88,
        // with a short tail past the hub like a real meter movement.
        float tailX = polX(needleAz, -22), tailY = polY(needleAz, -22);
        g->fillTriangle((int)tailX, (int)tailY, (int)(CX + 1.5f * dx),
                        (int)(CY + 1.5f * dy), (int)(CX - 1.5f * dx),
                        (int)(CY - 1.5f * dy), C_INK);
        float tipX = polX(needleAz, 88), tipY = polY(needleAz, 88);
        g->fillTriangle((int)tipX, (int)tipY, (int)(CX + 3.0f * dx),
                        (int)(CY + 3.0f * dy), (int)(CX - 3.0f * dx),
                        (int)(CY - 3.0f * dy), C_INK);
        g->fillCircle((int)CX, (int)CY, 7, C_INK);
    }

    template <typename G>
    void drawBadge(G* g, const FaceModel& m) {
        const char* text;
        if (!m.haveState) {
            text = "NO DATA";
        } else if (!m.bridgeUp) {
            text = "OFFLINE";
        } else {
            text = "NO LINK";  // bridge up, WRC link down
        }
        // Sits between the N mark and the hub, clear of the scale marks and
        // the readout window. The needle renders behind it (an alert face
        // is parked; the badge wins).
        g->fillRect((int)CX - 44, (int)CY - 42, 88, 20, C_DOWN);
        g->setTextDatum(middle_center);
        g->setFont(&fonts::Font2);
        g->setTextColor(C_WHITE, C_DOWN);
        g->drawString(text, (int)CX, (int)CY - 32);
    }
};