// Package control implements the optional inbound network-control server that
// lets external tracking software command the rotator over the Hamlib rotctld
// protocol, translated to a common Command that the engine executes. Position
// queries are answered from a thread-safe Pos snapshot the engine publishes.
//
// This server moves the rotator on remote request: it is the explicit, opt-in
// counterpart to the TUI's local controls.
package control

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"sync"
)

// Command is a decoded, protocol-independent rotator action.
type Command struct {
	Kind   Kind
	Az, El float64 // KindSetPos: target degrees (azimuth/elevation)
	Source string  // protocol that produced the command ("rotctld")
	Raw    string  // original request line, for trace logging
}

// Kind enumerates the rotator actions a network client can request.
type Kind int

const (
	// KindStop halts all motion.
	KindStop Kind = iota
	// KindSetPos commands an absolute move to (Az, El) degrees.
	KindSetPos
)

// Submit hands a decoded Command to the engine for execution.
type Submit func(Command)

// Pos is a thread-safe holder for the latest known rotator position, written by
// the engine and read by the server to answer queries.
type Pos struct {
	mu     sync.Mutex
	az, el float64
	valid  bool
}

// Set records the latest position (degrees).
func (p *Pos) Set(az, el float64) {
	p.mu.Lock()
	p.az, p.el, p.valid = az, el, true
	p.mu.Unlock()
}

// Get returns the latest position and whether any readback has arrived yet.
func (p *Pos) Get() (az, el float64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.az, p.el, p.valid
}

// Server is a running inbound-control listener. Construct with Start; stop with
// Close.
type Server struct {
	ln     net.Listener
	pos    *Pos
	submit Submit

	conns sync.Map // active net.Conn -> struct{}, closed by Close
}

// Start binds a TCP listener on bind:port and serves the rotctld protocol,
// translating client requests into Commands via submit and answering position
// queries from pos. The accept loop runs in the background until Close.
func Start(bind string, port int, pos *Pos, submit Submit) (*Server, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, pos: pos, submit: submit}
	go s.accept()
	return s, nil
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops accepting connections, unblocks the accept loop, and shuts down
// already-accepted connections so their handler goroutines do not leak waiting
// on a peer that never sends EOF.
func (s *Server) Close() error {
	err := s.ln.Close()
	s.conns.Range(func(k, _ any) bool {
		if c, ok := k.(net.Conn); ok {
			_ = c.Close()
		}
		return true
	})
	return err
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.conns.Store(conn, struct{}{})
		go s.handle(conn)
	}
}

// maxLineLen bounds a single inbound request. A client streaming without the
// delimiter gets its connection dropped at this size rather than buffering
// unbounded memory.
const maxLineLen = 4096

// handle reads one newline-delimited request at a time, dispatches it to the
// rotctld decoder, and writes any reply. The loop ends on EOF, error, a
// protocol close request, or an over-long line.
func (s *Server) handle(conn net.Conn) {
	defer s.conns.Delete(conn)
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 256), maxLineLen)
	sc.Split(splitOn('\n'))
	for sc.Scan() {
		reply, stop := s.rotctld(sc.Text())
		if reply != "" {
			if _, werr := conn.Write([]byte(reply)); werr != nil {
				return
			}
		}
		if stop {
			return
		}
	}
	_ = sc.Err() // EOF or read error: either way this connection is done
}

// splitOn returns a bufio.SplitFunc that splits on the given delimiter byte,
// including the delimiter in each token (the decoder trims it). A final token
// without a delimiter is returned at EOF.
func splitOn(delim byte) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.IndexByte(data, delim); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil // need more data
	}
}
