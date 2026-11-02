// Package loggers puts a callsign into the loggers on this PC: Log4OM through
// its Remote Control Interface (UDP) and DXLog.net through the small script
// chaseq-ui ships (dxlog/ChaseqCallsign.cs).
package loggers

import (
	"context"
	"regexp"
)

// Status is one part's outcome.
type Status string

// Part outcomes. NotConfirmed means the request was sent but nothing proved it
// landed (no reply in time).
const (
	OK           Status = "ok"
	NotConfirmed Status = "not confirmed"
	Failed       Status = "failed"
	Skipped      Status = "skipped"
)

// Result is a logger part's outcome with a short reason.
type Result struct {
	Status Status
	Detail string
}

// Sender puts a callsign into one logger.
type Sender interface {
	Name() string
	SendCall(ctx context.Context, call string) Result
}

var validCall = regexp.MustCompile(`^[A-Z0-9]{1,4}[A-Z0-9/]*$`)

// ValidCall reports whether call is plausible enough to send: upper-case
// letters, digits and '/' only, so nothing odd reaches a logger's entry line.
func ValidCall(call string) bool { return len(call) >= 3 && len(call) <= 20 && validCall.MatchString(call) }
