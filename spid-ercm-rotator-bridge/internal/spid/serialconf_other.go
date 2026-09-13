// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package spid

import (
	"fmt"
	"os"
)

// configurePort refuses the real-port path on non-linux platforms: the deploy
// target is shari (linux/arm64), and a tty silently left at the wrong baud is
// a bench trap far worse than a clear error at open. Mock mode (empty serial
// port, KTD7) never reaches this and keeps working everywhere.
func configurePort(f *os.File, baud int) error {
	return fmt.Errorf("raw serial line configuration (8N1 @ %d baud) is linux-only — the bridge deploys to shari", baud)
}
