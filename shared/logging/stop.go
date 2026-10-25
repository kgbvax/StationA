package logging

import (
	"context"
	"errors"
)

// CleanStop reports whether err is just the consequence of a requested
// shutdown: the root context (cancelled by SIGINT/SIGTERM) is done and err
// wraps context.Canceled. main() treats that as a clean stop (log Info, exit 0)
// so `systemctl stop/restart` neither logs level=ERROR nor records "Failed with
// result 'exit-code'" (docs/conventions/logging.md). A context.Canceled that
// surfaces while ctx is still live is a real failure and returns false.
func CleanStop(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() != nil && errors.Is(err, context.Canceled)
}
