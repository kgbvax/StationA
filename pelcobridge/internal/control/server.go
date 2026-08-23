package control

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"sync"
)

// Protocol selects the wire format a Server speaks.
type Protocol int

const (
	// Rotctld is the Hamlib net-rotator protocol (newline-delimited).
	Rotctld Protocol = iota
	// GS232 is the Yaesu GS-232A protocol (carriage-return-delimited).
	GS232
	// PstRotator is the PstRotator UDP control protocol (datagram, <PST>…</PST>).
	PstRotator
)

// Name returns a short human-readable protocol name.
func (p Protocol) Name() string {
	switch p {
	case GS232:
		return "gs232"
	case PstRotator:
		return "pstrotator"
	}
	return "rotctld"
}

// Server is a running inbound-control listener. Construct with Start; stop with
// Close.
type Server struct {
	proto  Protocol
	ln     net.Listener
	pos    *Pos
	submit Submit

	conns sync.Map // active net.Conn -> struct{}, closed by Close
}

// Start binds a TCP listener on bind:port and serves the given protocol,
// translating client requests into Commands via submit and answering position
// queries from pos. The accept loop runs in the background until Close.
func Start(proto Protocol, bind string, port int, pos *Pos, submit Submit) (*Server, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	s := &Server{proto: proto, ln: ln, pos: pos, submit: submit}
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

// handle reads one delimited request at a time, dispatches it to the protocol
// decoder, and writes any reply. The loop ends on EOF, error, a protocol close
// request, or an over-long line.
func (s *Server) handle(conn net.Conn) {
	defer s.conns.Delete(conn)
	defer conn.Close()
	delim := byte('\n')
	if s.proto == GS232 {
		delim = '\r'
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 256), maxLineLen)
	sc.Split(splitOn(delim))
	for sc.Scan() {
		line := sc.Text()
		var (
			reply string
			stop  bool
		)
		if s.proto == GS232 {
			reply = s.gs232(line)
		} else {
			reply, stop = s.rotctld(line)
		}
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
// including the delimiter in each token (the decoders trim it). A final token
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
