// knob.h — the Dial's rotary encoder + knob push, as detents and press events.
//
// The encoder is a 16-detent / 64-pulse-per-revolution quadrature on GPIO
// 41 (A) and 40 (B) → 4 counts per detent. We step in whole detents and
// carry the remainder, so a wobble at a detent boundary is absorbed instead
// of accumulating or being lost.
//
// WHY OUR OWN INTERRUPT QUADRATURE (not M5Dial.Encoder): the M5Dial library
// ships the PJRC Encoder class, which attaches CHANGE interrupts only for
// pins named in its static ESP32 table — CORE_INT0_PIN .. CORE_INT39_PIN.
// GPIO 40/41 fall through to `default: return 0`: NO interrupt is attached,
// interrupts_in_use stays 0, and readAndReset() silently degenerates to
// POLLING the pins once per call. One detent is 4 quadrature states; polled
// at our loop rate (20 ms + up to ~25 ms of full-frame SPI push while the
// face animates) a detent turns more than one state between reads, and the
// state machine either lands on an unhandled transition or wraps a full
// cycle (same phase = zero counts) — the detent is LOST. This is the
// "missed dial change events" bug: counting must be edge-driven, not
// poll-driven, whenever the main loop renders. Real CHANGE interrupts on
// both pins close the hole: every edge is counted no matter what the loop
// is doing.
//
// The ISR is IRAM-resident and reads the GPIO input registers through
// pointers resolved once in begin() — RAM-indirect access keeps the ISR
// free of flash literals (Xtensa l32r literals for IRAM code land in a
// flash pool → "dangerous relocation" at link time), and digitalRead() is
// not IRAM-safe at all: an edge firing while the flash cache was disabled
// (OTA writes) would crash it. The transition table is the standard
// quadrature full-cycle decode (+/-1 per single state change, +/-2 when
// both pins change between edges).
//
// The knob push (M5Dial.BtnA, GPIO 42) is a single gesture: it reports one
// press event on release, however long it was held — press = stop, always
// safe, no mode toggles to mistype mid-operation.
//
// All methods must be called after M5Dial.update() in the same loop pass.
#pragma once

#include <Arduino.h>
#include <M5Dial.h>

#include "soc/gpio_reg.h"

// M5Dial's own pin defines — the physical truth of this board.
#ifndef DIAL_ENCODER_PIN_A
#define DIAL_ENCODER_PIN_A 41
#endif
#ifndef DIAL_ENCODER_PIN_B
#define DIAL_ENCODER_PIN_B 40
#endif

class Knob {
   public:
    void begin() {
        pinMode(DIAL_ENCODER_PIN_A, INPUT_PULLUP);
        pinMode(DIAL_ENCODER_PIN_B, INPUT_PULLUP);
        // Resolve the GPIO input registers ONCE, in flash code: the ISR
        // dereferences these stored pointers instead of loading register
        // addresses as literals. Xtensa l32r literals for an IRAM function
        // land in a flash pool ("dangerous relocation") — RAM-indirect
        // access keeps the whole ISR self-contained in IRAM. Same trick the
        // PJRC library's own ISRs use.
        inA_ = (DIAL_ENCODER_PIN_A < 32) ? (volatile uint32_t*)GPIO_IN_REG
                                          : (volatile uint32_t*)GPIO_IN1_REG;
        maskA_ = 1u << (DIAL_ENCODER_PIN_A & 31);
        inB_ = (DIAL_ENCODER_PIN_B < 32) ? (volatile uint32_t*)GPIO_IN_REG
                                         : (volatile uint32_t*)GPIO_IN1_REG;
        maskB_ = 1u << (DIAL_ENCODER_PIN_B & 31);
        // Let the passive R-C filter charge through the pullups before the
        // initial phase read (same settle the PJRC library uses).
        delayMicroseconds(2000);
        encState_ = ((*inA_ & maskA_) ? 1u : 0u) | ((*inB_ & maskB_) ? 2u : 0u);
        position_ = 0;
        countRemainder_ = 0;
        lastRaw_ = 0;
        wasPressed_ = false;
        attachInterruptArg(DIAL_ENCODER_PIN_A, Knob::isr, this, CHANGE);
        attachInterruptArg(DIAL_ENCODER_PIN_B, Knob::isr, this, CHANGE);
    }

    // Whole detents since the last call (sign = direction). The hardware
    // counter is read and cleared with interrupts masked, so no edge that
    // lands mid-read is lost or double-counted. ENCODER_INVERT flips the
    // sign if the hardware reads CW as negative (config.h).
    int pollDetents() {
        noInterrupts();
        long delta = position_;
        position_ = 0;
        interrupts();
#if ENCODER_INVERT
        delta = -delta;
#endif
        lastRaw_ = (int)delta;
        countRemainder_ += (int)delta;
        int detents = countRemainder_ / ENCODER_COUNTS_PER_DETENT;
        countRemainder_ -= detents * ENCODER_COUNTS_PER_DETENT;
        return detents;
    }

    // Raw quadrature counts of the last poll — bench telemetry: with the
    // counting edge-driven, one knob-flat must show 4 counts here, every
    // flat, even while the face animates.
    int lastRaw() const { return lastRaw_; }

    // True exactly once per press, on the release edge (any hold duration).
    bool pollPress() {
        bool pressed = M5Dial.BtnA.isPressed();  // the knob push is BtnA here
        bool released = wasPressed_ && !pressed;
        wasPressed_ = pressed;
        return released;
    }

   private:
    static void IRAM_ATTR isr(void* arg) {
        Knob* k = (Knob*)arg;
        // Pin order matches M5Dial's Encoder(41, 40): bit 0 = A (41).
        uint8_t s = ((*k->inA_ & k->maskA_) ? 1u : 0u) |
                    ((*k->inB_ & k->maskB_) ? 2u : 0u);
        uint8_t t = (k->encState_ << 2) | s;
        k->encState_ = s;
        switch (t & 0x0F) {
            case 1: case 7: case 8: case 14: k->position_ += 1; break;
            case 2: case 4: case 11: case 13: k->position_ -= 1; break;
            case 3: case 12: k->position_ += 2; break;
            case 6: case 9: k->position_ -= 2; break;
            default: break;  // same phase (bounce) — no count
        }
    }

    // The ISR-owned quadrature state. position_ is read-and-reset under
    // noInterrupts() in pollDetents(); encState_ is ISR-private. The
    // register pointers/masks are plain RAM reads for the ISR (see begin).
    volatile uint32_t* inA_ = nullptr;
    volatile uint32_t* inB_ = nullptr;
    uint32_t maskA_ = 0;
    uint32_t maskB_ = 0;
    volatile int32_t position_ = 0;
    volatile uint8_t encState_ = 0;
    int countRemainder_ = 0;
    int lastRaw_ = 0;
    bool wasPressed_ = false;
};