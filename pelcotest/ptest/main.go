package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	port := flag.String("port", "", "serial port (e.g. /dev/tty.usbserial-… or COM3)")
	list := flag.Bool("list", false, "list available serial ports and exit")
	addr := flag.Int("addr", 1, "rotor Pelco-D address (0-255; the doc's DIP range is 0-64)")
	baud := flag.Int("baud", 2400, "baud rate (the 303Z/3050DZ family is documented for 1200-9600)")
	pelcoP := flag.Bool("p", false, "send Pelco-P frames (8-byte A0/AF envelopes) instead of Pelco-D; RX is always adaptive, and 'p' in the TUI toggles this at runtime")
	tiltCal := flag.String("tilt-cal", "", "OPTIONAL hypothesis to test against the tilt readback: \"raw_at_0,raw_at_90\" for a linear raw-count→elevation map. Off by default — the meaning of the 0x5B word is not known, and elevation is not a function of it. When set, the log labels the reading \"hyp:\" and \"UNVERIFIED\"")

	sweep := flag.String("sweep", "", "record a tilt sweep instead of starting the TUI: \"up\" or \"down\". Jogs one step, halts, waits, queries tilt, appends the raw frame to -sweep-out, and repeats until the tilt word stops changing")
	sweepOut := flag.String("sweep-out", "tilt-sweep.csv", "CSV file for -sweep raw data")
	sweepMove := flag.Duration("sweep-move", time.Second, "motor-on time per sweep step")
	sweepSettle := flag.Duration("sweep-settle", 200*time.Millisecond, "wait after halting before querying tilt (readback is only trustworthy once halted)")
	sweepPostTX := flag.Duration("sweep-post-tx", 50*time.Millisecond, "pause after every frame transmitted to the rotator")
	sweepReply := flag.Duration("sweep-reply-wait", 2*time.Second, "how long to wait for the tilt reply each step")
	sweepStable := flag.Int("sweep-stable", 3, "stop once the tilt word has been identical for this many consecutive readings")
	sweepMax := flag.Int("sweep-max-steps", 200, "safety cap on sweep steps")
	sweepSpeed := flag.Uint("sweep-speed", 0x20, "tilt jog speed byte (0x00-0x3F)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"ptest — manual Pelco-D/Pelco-P rotor test TUI (8N1)\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *list {
		ports, err := ListPorts()
		if err != nil {
			fmt.Fprintf(os.Stderr, "listing ports: %v\n", err)
			os.Exit(1)
		}
		if len(ports) == 0 {
			fmt.Println("no serial ports found")
		}
		for _, p := range ports {
			fmt.Println(p)
		}
		return
	}
	if *port == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *addr < 0 || *addr > 255 {
		fmt.Fprintln(os.Stderr, "addr must be 0..255")
		os.Exit(2)
	}
	if *baud <= 0 {
		fmt.Fprintln(os.Stderr, "baud must be positive")
		os.Exit(2)
	}
	cal, err := parseTiltCal(*tiltCal)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-tilt-cal: %v\n", err)
		os.Exit(2)
	}

	sp, err := OpenPort(*port, *baud)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", *port, err)
		os.Exit(1)
	}
	defer sp.Close()

	if *sweep != "" {
		if *sweepSpeed > 0x3F {
			fmt.Fprintln(os.Stderr, "-sweep-speed must be 0x00..0x3F")
			os.Exit(2)
		}
		err := runSweep(sp, byte(*addr), *pelcoP, sweepOpts{
			dir:       *sweep,
			out:       *sweepOut,
			move:      *sweepMove,
			settle:    *sweepSettle,
			postTX:    *sweepPostTX,
			replyWait: *sweepReply,
			stable:    *sweepStable,
			maxSteps:  *sweepMax,
			speed:     byte(*sweepSpeed),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "sweep: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(sp, byte(*addr), *pelcoP, cal); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// parseTiltCal reads "raw_at_0,raw_at_90". Empty or equal values leave the
// hypothesis unset, so ptest asserts nothing about elevation.
func parseTiltCal(s string) (TiltCal, error) {
	if strings.TrimSpace(s) == "" {
		return TiltCal{}, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return TiltCal{}, fmt.Errorf("want \"raw_at_0,raw_at_90\", got %q", s)
	}
	var c TiltCal
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[0]), "%f", &c.Raw0); err != nil {
		return TiltCal{}, fmt.Errorf("raw_at_0 %q is not a number", parts[0])
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%f", &c.Raw90); err != nil {
		return TiltCal{}, fmt.Errorf("raw_at_90 %q is not a number", parts[1])
	}
	return c, nil
}
