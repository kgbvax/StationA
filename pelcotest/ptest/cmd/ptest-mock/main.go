// ptest-mock is a tiny canned 303Z/3050DZ rotor for loopback-testing ptest
// without hardware. Like the adaptive real unit it accepts both Pelco-D
// (7-byte 0xFF) and Pelco-P (8-byte 0xA0) query envelopes and replies in the
// protocol the query arrived in.
//
// The tilt reply is a RAW 16-BIT VALUE with no model attached (-tilt-word),
// because nobody knows what the 0x5B word means on this head. The manual's
// "hundredths of a degree" claim is disproved (every real reading lands outside
// the 0..90° travel), and the linear raw-encoder-count model that replaced it
// is disproved too — re-checked on the bench 2026-08-27, elevation does not
// appear in the tilt word at all. So the mock does not pretend to model
// elevation: it echoes a value you choose, defaulting to one actually observed
// live. Pan IS Pelco-standard hundredths, so -pan-deg does model pan.
// -doc-mode reproduces the manual's example frames verbatim.
//
// Pair it with ptest over a null-modem: on macOS/Linux use socat
//
//	socat -d -d pty,raw,echo=0 pty,raw,echo=0
//
// and pass the two printed /dev/ttys… to ptest-mock and ptest. On Windows,
// com0com provides the paired ports.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.bug.st/serial"
)

const (
	respTilt = 0x5B // tilt position response
	respPan  = 0x59 // pan position response
	// The manual's example values, kept only for -doc-mode.
	docTiltWord = 0x1F3F // "79.99°" under the disproved hundredths claim
	docPanWord  = 0x752F // 299.99°, which pan really does use
	// An actually-observed live tilt word (0x8E90). Not a modelled elevation:
	// it is a sample of real traffic, used so the loopback fixture exercises
	// the same byte range the head really emits.
	liveTiltWord = 36496
)

// dReply builds a 7-byte Pelco-D response frame.
func dReply(addr, cmd2 byte, word uint16) []byte {
	d1, d2 := byte(word>>8), byte(word)
	return []byte{0xFF, addr, 0x00, cmd2, d1, d2, addr + cmd2 + d1 + d2}
}

// pReply builds the same response re-wrapped as an 8-byte Pelco-P frame.
func pReply(addr, cmd2 byte, word uint16) []byte {
	w := []byte{0xA0, addr, 0x00, cmd2, byte(word >> 8), byte(word), 0xAF, 0}
	c := byte(0)
	for _, b := range w[:7] {
		c ^= b
	}
	w[7] = c
	return w
}

// dChkOK validates a 7-byte Pelco-D frame; pChkOK validates an 8-byte Pelco-P
// frame including its ETX. The mock used to frame on 0xA0/0xFF without checking
// either, so a single stray 0xA0 byte consumed the following genuine Pelco-D
// query and the mock silently never answered it.
func dChkOK(w []byte) bool {
	return len(w) == 7 && w[0] == 0xFF &&
		byte(uint16(w[1])+uint16(w[2])+uint16(w[3])+uint16(w[4])+uint16(w[5])) == w[6]
}

func pChkOK(w []byte) bool {
	if len(w) != 8 || w[0] != 0xA0 || w[6] != 0xAF {
		return false
	}
	c := byte(0)
	for _, b := range w[:7] {
		c ^= b
	}
	return c == w[7]
}

func main() {
	port := flag.String("port", "", "serial port for the mock rotor")
	tiltWordF := flag.Uint("tilt-word", liveTiltWord, "raw 16-bit value to return for a tilt query. NOT an elevation: the meaning of the 0x5B word is unknown, so the mock echoes what you give it (default: a value observed live)")
	az := flag.Float64("pan-deg", 299.99, "azimuth in degrees for a pan query (pan really is hundredths, so this is modelled)")
	docMode := flag.Bool("doc-mode", false, "reply with the manual's example frames (tilt 0x1F3F, pan 0x752F)")
	flag.Parse()
	if *port == "" {
		flag.Usage()
		os.Exit(2)
	}

	if *tiltWordF > 0xFFFF {
		log.Fatalf("-tilt-word %d does not fit in 16 bits", *tiltWordF)
	}
	tiltWord := uint16(*tiltWordF)
	panWord := uint16(*az*100 + 0.5)
	if *docMode {
		tiltWord, panWord = docTiltWord, docPanWord
	}

	p, err := serial.Open(*port, &serial.Mode{BaudRate: 2400, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit})
	if err != nil {
		log.Fatalf("open %s: %v", *port, err)
	}
	defer p.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		p.Close()
		os.Exit(0)
	}()

	if *docMode {
		fmt.Printf("ptest-mock on %s (2400 8N1, DOC MODE): 0x53→tilt %04X, 0x51→pan %04X\n", *port, tiltWord, panWord)
	} else {
		fmt.Printf("ptest-mock on %s (2400 8N1): 0x53→tilt %04X (%d raw, meaning unknown), 0x51→pan %04X (%.2f°)\n",
			*port, tiltWord, tiltWord, panWord, float64(panWord)/100)
	}

	buf := make([]byte, 64)
	var acc []byte // carry buffer: a frame may split across reads
	for {
		n, err := p.Read(buf)
		if err != nil {
			log.Fatalf("read: %v", err)
		}
		if n == 0 {
			continue
		}
		acc = append(acc, buf[:n]...)
		// Scan the accumulator for query frames (any address), in either
		// envelope, validating the checksum before consuming. An incomplete
		// frame at the tail is kept for the next read.
		i := 0
		for i < len(acc) {
			fl := 0
			switch acc[i] {
			case 0xA0:
				fl = 8 // Pelco-P
			case 0xFF:
				fl = 7 // Pelco-D
			default:
				i++
				continue
			}
			if i+fl > len(acc) {
				break // wait for the rest of the frame
			}
			q := acc[i : i+fl]
			isP := q[0] == 0xA0
			if (isP && !pChkOK(q)) || (!isP && !dChkOK(q)) {
				// Not a valid frame at this offset: drop one byte and resync
				// rather than swallowing fl bytes of possibly-genuine traffic.
				i++
				continue
			}
			var reply []byte
			switch q[3] {
			case 0x53:
				if isP {
					reply = pReply(q[1], respTilt, tiltWord)
				} else {
					reply = dReply(q[1], respTilt, tiltWord)
				}
			case 0x51:
				if isP {
					reply = pReply(q[1], respPan, panWord)
				} else {
					reply = dReply(q[1], respPan, panWord)
				}
			}
			if reply == nil {
				fmt.Printf("%s  RX % X (no reply)\n", time.Now().Format("15:04:05.000"), q)
			} else {
				time.Sleep(30 * time.Millisecond) // device-like turnaround
				if _, err := p.Write(reply); err != nil {
					log.Fatalf("write: %v", err)
				}
				fmt.Printf("%s  RX % X → TX % X\n", time.Now().Format("15:04:05.000"), q, reply)
			}
			i += fl
		}
		acc = append([]byte(nil), acc[i:]...)
	}
}
