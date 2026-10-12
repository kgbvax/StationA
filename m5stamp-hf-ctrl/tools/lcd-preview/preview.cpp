// preview.cpp — renders src/ui.h on the desktop (M5GFX sprite, no hardware) for a
// set of scenarios and writes one binary PPM per scenario to the directory given
// as argv[1]. render.sh builds and runs it, then stitches a PNG contact sheet.
#include <lgfx/v1/platforms/device.hpp>
#include <lgfx/v1/LGFXBase.hpp>
#include <lgfx/v1/LGFX_Sprite.hpp>

#include <cstdio>
#include <string>
#include <vector>

#include "../../src/ui.h"

struct Scenario {
    const char* name;
    ui::Model m;
};

static ui::Model mk(ui::Tx tx, bool fault, bool pa, bool trx, ui::Link link, ui::Tone tone,
                    const char* gate) {
    ui::Model m;
    m.tx = tx;
    m.txFault = fault;
    m.paOn = pa;
    m.trxOn = trx;
    m.link = link;
    m.gateTone = tone;
    snprintf(m.gate, sizeof m.gate, "%s", gate);
    return m;
}

int main(int argc, char** argv) {
    if (argc < 2) return 2;
    using namespace ui;
    std::vector<Scenario> sc = {
        {"cold", mk(TX_OFF, false, false, false, LINK_MQTT, TONE_OK, "gates clear")},
        {"powered", mk(TX_OFF, false, true, true, LINK_MQTT, TONE_OK, "gates clear")},
        {"wait", mk(TX_WAIT, false, true, true, LINK_MQTT, TONE_CAUTION, "waiting for rx")},
        {"held", mk(TX_BLOCKED, false, true, true, LINK_MQTT, TONE_CAUTION, "ultrabeam moving")},
        {"fault", mk(TX_BLOCKED, true, true, true, LINK_MQTT, TONE_ALARM, "antenna switch offline")},
        {"armed", mk(TX_ARMED, false, true, true, LINK_MQTT, TONE_OK, "gates clear")},
        {"nolink", mk(TX_OFF, false, false, false, LINK_NONE, TONE_ALARM, "radio feed stale")},
        {"wifionly", mk(TX_BLOCKED, true, true, false, LINK_WIFI, TONE_ALARM, "relay mismatch")},
    };
    for (auto& s : sc) {
        lgfx::LGFX_Sprite spr;
        spr.setColorDepth(24);
        spr.createSprite(SCREEN_W, SCREEN_H);
        ui::Painter p;
        ui::paint(spr, s.m, p);
        std::vector<lgfx::bgr888_t> px(SCREEN_W * SCREEN_H);
        spr.readRectRGB(0, 0, SCREEN_W, SCREEN_H, px.data());
        std::string path = std::string(argv[1]) + "/" + s.name + ".ppm";
        FILE* f = fopen(path.c_str(), "wb");
        fprintf(f, "P6\n%d %d\n255\n", SCREEN_W, SCREEN_H);
        for (auto& c : px) {
            unsigned char rgb[3] = {c.r, c.g, c.b};
            fwrite(rgb, 1, 3, f);
        }
        fclose(f);
    }
    return 0;
}
