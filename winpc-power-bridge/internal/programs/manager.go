// Package programs starts, checks and ends the station programs on the shack
// PC (AetherSDR, WSJT-X, 75auto) for the /cmd actions ensure_program and
// stop_program, and keeps the PC awake while a start chain or a 75auto session
// needs it.
//
// The rule is: use a copy that already runs, and never end a copy that the
// agent did not start. The agent records the copies that it started itself
// (PID plus process start time) in a file next to its configuration, so the
// record survives an agent restart, and a reused PID is not taken as owned.
//
// The process table, the clock and the power request are injected. The
// Windows implementation is in proc_windows.go; on other platforms the
// functions return ErrUnsupported, so the decision logic is tested on any
// platform.
package programs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"winpc-power-bridge/internal/config"
)

// Program states, published as /state program_<name>.
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateReady    = "ready"
	StateFailed   = "failed"
)

// Refusals of Ensure and Stop.
var (
	ErrUnknownProgram = errors.New("unknown program")
	ErrNotOwned       = errors.New("not started by the chain")
	ErrSessionActive  = errors.New("75auto session active")
	ErrUnsupported    = errors.New("unsupported on this platform")
)

// Probe timeouts of one tcp check attempt, and the pause between attempts.
const (
	dialTimeout   = 2 * time.Second
	replyTimeout  = 3 * time.Second
	retryInterval = time.Second
)

// Process identifies one running process. Start is the process creation time
// in the platform's own units; with PID it tells a reused PID apart.
type Process struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

// Spec is what Proc.Start runs.
type Spec struct {
	Exe     string
	Args    []string
	Workdir string
}

// Proc is the process table of the platform.
type Proc interface {
	// Find lists the running processes with this image name (case-insensitive).
	Find(image string) ([]Process, error)
	// Alive reports whether p still runs with the same start time.
	Alive(p Process) bool
	// Start runs a program outside the agent's own job, so that the end of
	// the agent does not end it.
	Start(s Spec) (Process, error)
	// End ends p and returns when it has exited.
	End(p Process) error
}

// Clock is the time source of the settle and retry waits.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx ends.
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the wall clock.
type RealClock struct{}

// Now returns time.Now().
func (RealClock) Now() time.Time { return time.Now() }

// Sleep waits for d or until ctx ends.
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Options configure a Manager.
type Options struct {
	Programs []config.ProgramConfig
	Proc     Proc
	// OwnedFile is the record of the programs that the agent started.
	OwnedFile string
	// SessionActive reports whether a 75auto session is active; stop_program
	// is refused then.
	SessionActive func() bool
	Clock         Clock
	Log           *slog.Logger
}

// Field is one program's /state fields: program_<Name> and
// program_<Name>_error.
type Field struct {
	Name  string
	State string
	Error string
}

type program struct {
	cfg   config.ProgramConfig
	reply *regexp.Regexp
	state string
	err   string
}

// Manager runs the program decisions. It is not safe for concurrent use: all
// calls run on the single jobs worker.
type Manager struct {
	ctx           context.Context
	progs         []*program
	proc          Proc
	ownedFile     string
	owned         map[string]Process
	sessionActive func() bool
	clock         Clock
	log           *slog.Logger
}

// New builds a Manager and loads the owned record. A record whose PID does not
// run with the same start time is dropped.
func New(ctx context.Context, o Options) *Manager {
	m := &Manager{
		ctx:           ctx,
		proc:          o.Proc,
		ownedFile:     o.OwnedFile,
		owned:         map[string]Process{},
		sessionActive: o.SessionActive,
		clock:         o.Clock,
		log:           o.Log,
	}
	if m.clock == nil {
		m.clock = RealClock{}
	}
	if m.sessionActive == nil {
		m.sessionActive = func() bool { return false }
	}
	for _, c := range o.Programs {
		p := &program{cfg: c, state: StateStopped}
		if c.Check == config.CheckTCP {
			p.reply = regexp.MustCompile(c.ReplyPattern) // validated by config
		}
		m.progs = append(m.progs, p)
	}
	m.loadOwned()
	return m
}

// Fields returns the program fields in configuration order.
func (m *Manager) Fields() []Field {
	out := make([]Field, 0, len(m.progs))
	for _, p := range m.progs {
		out = append(out, Field{Name: p.cfg.Name, State: p.state, Error: p.err})
	}
	return out
}

// Owned reports whether the agent started the running copy of name.
func (m *Manager) Owned(name string) bool {
	_, ok := m.owned[name]
	return ok
}

// Ensure makes the program run and pass its ready check. It sets the program
// to starting and calls progress (which publishes /state) before any work,
// then leaves the result in the program's fields. The only error is
// ErrUnknownProgram, with no field change.
//
//  1. An owned copy with restart_owned is ended and started again.
//  2. A copy that runs and passes its check is used (an operator copy is not
//     recorded as owned).
//  3. An owned copy that fails its check is ended and started again.
//  4. An operator copy that fails its check fails with "not started by the
//     chain"; it is not ended.
//  5. A program that does not run is started and recorded as owned.
func (m *Manager) Ensure(name string, progress func()) error {
	p := m.lookup(name)
	if p == nil {
		return ErrUnknownProgram
	}
	p.state, p.err = StateStarting, ""
	progress()

	if err := m.ensure(p); err != nil {
		p.state, p.err = StateFailed, err.Error()
		m.log.Warn("program not ready", "program", name, "err", err)
		return nil
	}
	p.state = StateReady
	m.log.Info("program ready", "program", name, "owned", m.Owned(name))
	return nil
}

func (m *Manager) ensure(p *program) error {
	name := p.cfg.Name
	if own, ok := m.ownedAlive(p); ok {
		if p.cfg.RestartOwned {
			m.log.Info("restarting owned program (restart_owned)", "program", name, "pid", own.PID)
			return m.restart(p, own)
		}
		err := m.check(p, own, false)
		if err == nil {
			return nil
		}
		m.log.Warn("owned program fails its check; restarting", "program", name, "pid", own.PID, "err", err)
		return m.restart(p, own)
	}

	running, err := m.proc.Find(p.cfg.Process)
	if err != nil {
		return fmt.Errorf("process list: %w", err)
	}
	if len(running) > 0 {
		if err := m.check(p, running[0], false); err != nil {
			return fmt.Errorf("%w: %v", ErrNotOwned, err)
		}
		return nil
	}
	return m.start(p)
}

func (m *Manager) restart(p *program, own Process) error {
	if err := m.proc.End(own); err != nil {
		return fmt.Errorf("end: %w", err)
	}
	m.forget(p.cfg.Name)
	return m.start(p)
}

func (m *Manager) start(p *program) error {
	c := p.cfg
	spec := Spec{Exe: c.Exe, Args: c.Args, Workdir: c.Workdir}
	if spec.Workdir == "" {
		if i := strings.LastIndexAny(c.Exe, `\/`); i > 0 {
			spec.Workdir = c.Exe[:i]
		}
	}
	proc, err := m.proc.Start(spec)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	m.log.Info("program started", "program", c.Name, "pid", proc.PID)
	m.owned[c.Name] = proc
	m.saveOwned()
	if err := m.check(p, proc, true); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	return nil
}

// check runs the ready check on proc. fresh means that proc was just started:
// the process check waits settle_s first, and the tcp check tries again until
// check_timeout_s has passed. A copy that already ran gets one attempt.
func (m *Manager) check(p *program, proc Process, fresh bool) error {
	c := p.cfg
	switch c.Check {
	case config.CheckNone:
		return nil
	case config.CheckProcess:
		if fresh {
			if err := m.clock.Sleep(m.ctx, time.Duration(c.SettleS)*time.Second); err != nil {
				return err
			}
		}
		if !m.proc.Alive(proc) {
			return fmt.Errorf("process exited within %d s", c.SettleS)
		}
		return nil
	case config.CheckTCP:
		deadline := m.clock.Now()
		if fresh {
			deadline = deadline.Add(time.Duration(c.CheckTimeoutS) * time.Second)
		}
		for {
			err := probe(c.CheckAddress, c.Probe, p.reply)
			if err == nil {
				return nil
			}
			if fresh && !m.proc.Alive(proc) {
				return fmt.Errorf("process exited: %v", err)
			}
			if !m.clock.Now().Before(deadline) {
				return err
			}
			if serr := m.clock.Sleep(m.ctx, retryInterval); serr != nil {
				return err
			}
		}
	}
	return fmt.Errorf("unknown check kind %q", c.Check)
}

// probe connects to addr, sends line and matches the first reply line.
func probe(addr, line string, reply *regexp.Regexp) error {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(replyTimeout))
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		return fmt.Errorf("send probe: %w", err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && got == "" {
		return fmt.Errorf("no reply: %w", err)
	}
	got = strings.TrimSpace(got)
	if !reply.MatchString(got) {
		return fmt.Errorf("reply %q", got)
	}
	return nil
}

// Stop ends an owned program. It refuses while a 75auto session is active, and
// it never ends a copy that the agent did not start.
func (m *Manager) Stop(name string) error {
	p := m.lookup(name)
	if p == nil {
		return ErrUnknownProgram
	}
	if m.sessionActive() {
		return ErrSessionActive
	}
	if own, ok := m.ownedAlive(p); ok {
		if err := m.proc.End(own); err != nil {
			return fmt.Errorf("end: %w", err)
		}
		m.forget(name)
		p.state, p.err = StateStopped, ""
		m.log.Info("program stopped", "program", name, "pid", own.PID)
		return nil
	}
	running, err := m.proc.Find(p.cfg.Process)
	if err != nil {
		return fmt.Errorf("process list: %w", err)
	}
	if len(running) > 0 {
		return ErrNotOwned
	}
	p.state, p.err = StateStopped, ""
	return nil
}

// Refresh re-checks the programs on the /state tick: an owned copy that no
// longer runs is dropped from the record, and a ready program whose process
// is gone shows as stopped.
func (m *Manager) Refresh() {
	for _, p := range m.progs {
		if _, ok := m.owned[p.cfg.Name]; ok {
			if _, alive := m.ownedAlive(p); !alive {
				m.log.Warn("owned program exited", "program", p.cfg.Name)
				p.state, p.err = StateStopped, ""
			}
			continue
		}
		if p.state != StateReady {
			continue
		}
		if running, err := m.proc.Find(p.cfg.Process); err == nil && len(running) == 0 {
			m.log.Info("program exited", "program", p.cfg.Name)
			p.state, p.err = StateStopped, ""
		}
	}
}

func (m *Manager) lookup(name string) *program {
	for _, p := range m.progs {
		if p.cfg.Name == name {
			return p
		}
	}
	return nil
}

// ownedAlive returns the owned copy of p if it still runs; a dead record is
// dropped.
func (m *Manager) ownedAlive(p *program) (Process, bool) {
	own, ok := m.owned[p.cfg.Name]
	if !ok {
		return Process{}, false
	}
	if !m.proc.Alive(own) {
		m.forget(p.cfg.Name)
		return Process{}, false
	}
	return own, true
}

func (m *Manager) forget(name string) {
	delete(m.owned, name)
	m.saveOwned()
}

// loadOwned reads the owned record and keeps only entries of configured
// programs whose PID still runs with the recorded start time.
func (m *Manager) loadOwned() {
	data, err := os.ReadFile(m.ownedFile)
	if err != nil {
		if !os.IsNotExist(err) {
			m.log.Warn("owned record unreadable; no program is owned", "file", m.ownedFile, "err", err)
		}
		return
	}
	var rec map[string]Process
	if err := json.Unmarshal(data, &rec); err != nil {
		m.log.Warn("owned record malformed; no program is owned", "file", m.ownedFile, "err", err)
		return
	}
	changed := false
	for name, proc := range rec {
		p := m.lookup(name)
		if p == nil || !m.proc.Alive(proc) {
			changed = true
			continue
		}
		m.owned[name] = proc
		p.state = StateReady
	}
	if changed {
		m.saveOwned()
	}
}

// saveOwned writes the owned record (temp file + rename). A failed write is
// logged: the copy then counts as an operator copy after an agent restart and
// is never ended, which is the safe side.
func (m *Manager) saveOwned() {
	data, err := json.MarshalIndent(m.owned, "", "  ")
	if err != nil {
		m.log.Error("marshal owned record", "err", err)
		return
	}
	tmp := m.ownedFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		m.log.Warn("owned record not saved", "file", m.ownedFile, "err", err)
		return
	}
	if err := os.Rename(tmp, m.ownedFile); err != nil {
		m.log.Warn("owned record not saved", "file", m.ownedFile, "err", err)
	}
}
