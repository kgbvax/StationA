package control

import (
	"bufio"
	"net"
	"strconv"
)

// Protocol selects the wire format a Server speaks.
type Protocol int

const (
	// Rotctld is the Hamlib net-rotator protocol (newline-delimited).
	Rotctld Protocol = iota
	// GS232 is the Yaesu GS-232A protocol (carriage-return-delimited).
	GS232
)

// Name returns a short human-readable protocol name.
func (p Protocol) Name() string {
	if p == GS232 {
		return "gs232"
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

// Close stops accepting connections and unblocks the accept loop.
func (s *Server) Close() error { return s.ln.Close() }

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handle(conn)
	}
}

// handle reads one delimited request at a time, dispatches it to the protocol
// decoder, and writes any reply. The loop ends on EOF, error, or a protocol
// close request.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	delim := byte('\n')
	if s.proto == GS232 {
		delim = '\r'
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString(delim)
		if len(line) > 0 {
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
		if err != nil {
			return
		}
	}
}
