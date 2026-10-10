package logging

import "log/slog"

// Streak keeps a reconnect loop against an absent device (powered off,
// unplugged) to one Warn per outage: the first Fail of a streak logs at Warn,
// every repeat at Info with a "repeat" count, so `journalctl -p warning`
// shows the outage once instead of every backoff cycle. Reset ends the
// streak — call it once the link has proven healthy (the loops use their
// existing "run lasted long enough" backoff-reset test), so the next failure
// warns again. The zero value is ready; not safe for concurrent use (one
// reconnect loop owns it).
type Streak struct {
	n int
}

// Fail logs one failure of the current streak.
func (s *Streak) Fail(log *slog.Logger, msg string, args ...any) {
	s.n++
	if s.n == 1 {
		log.Warn(msg, args...)
		return
	}
	log.Info(msg, append(args, "repeat", s.n)...)
}

// Reset ends the streak; the next Fail logs at Warn again.
func (s *Streak) Reset() { s.n = 0 }
