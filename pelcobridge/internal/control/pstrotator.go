package control

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// UDPServer is a running inbound-control UDP listener for the PstRotator
// protocol. Unlike the TCP Server it is connectionless: each datagram is one
// request, and query replies are sent back to the client's IP on the listen
// port + 1 — PstRotator's documented convention, where the client listens on
// port+1 for answers.
type UDPServer struct {
	conn      *net.UDPConn
	pos       *Pos
	submit    Submit
	replyPort int
}

// StartUDP binds a UDP listener on bind:port and serves the PstRotator
// protocol, translating datagrams into Commands via submit and answering
// position queries from pos. The serve loop runs in the background until Close.
func StartUDP(bind string, port int, pos *Pos, submit Submit) (*UDPServer, error) {
	// Replies go to the client on port+1 (PstRotator convention), so the listen
	// port must leave a valid reply port: 65535 would overflow to 65536.
	if port < 1 || port > 65534 {
		return nil, fmt.Errorf("pstrotator: listen port %d out of range (1-65534; replies use port+1)", port)
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	s := &UDPServer{conn: conn, pos: pos, submit: submit, replyPort: port + 1}
	go s.serve()
	return s, nil
}

// Addr returns the address the server is listening on.
func (s *UDPServer) Addr() string { return s.conn.LocalAddr().String() }

// Close stops the listener and unblocks the serve loop.
func (s *UDPServer) Close() error { return s.conn.Close() }

func (s *UDPServer) serve() {
	buf := make([]byte, 1024)
	for {
		n, remote, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return // listener closed
		}
		if reply := s.pstrotator(string(buf[:n])); reply != "" {
			dst := &net.UDPAddr{IP: remote.IP, Port: s.replyPort}
			_, _ = s.conn.WriteToUDP([]byte(reply), dst)
		}
	}
}

// pstrotator decodes one PstRotator UDP datagram and returns the reply (empty
// for motion commands, which the protocol does not ack). Queries are answered
// from the latest position; like GS-232, they are withheld until the first
// readback arrives.
//
// Supported: <AZIMUTH> / <ELEVATION> (absolute move, either or both), <STOP>
// (stop), AZ? / EL? (query). Azimuth↔pan, elevation↔tilt.
func (s *UDPServer) pstrotator(pkt string) string {
	up := strings.ToUpper(strings.TrimSpace(pkt))
	if !strings.HasPrefix(up, "<PST>") || !strings.HasSuffix(up, "</PST>") {
		return ""
	}
	body := up[len("<PST>") : len(up)-len("</PST>")]

	// Queries are bare markers with no value (e.g. <PST>AZ?</PST>).
	if strings.Contains(body, "AZ?") {
		az, _, ok := s.pos.Get()
		if !ok {
			return ""
		}
		return fmt.Sprintf("AZ:%.1f\r", az)
	}
	if strings.Contains(body, "EL?") {
		_, el, ok := s.pos.Get()
		if !ok {
			return ""
		}
		return fmt.Sprintf("EL:%.1f\r", el)
	}

	if strings.Contains(body, "<STOP>") {
		s.submit(Command{Kind: KindStop})
		return ""
	}

	azStr, hasAz := tagValue(body, "AZIMUTH")
	elStr, hasEl := tagValue(body, "ELEVATION")
	if !hasAz && !hasEl {
		return ""
	}
	az, el, _ := s.pos.Get() // keep the current axis when only one is given (0 if unknown)
	if hasAz {
		if v, err := strconv.ParseFloat(strings.TrimSpace(azStr), 64); err == nil {
			az = v
		}
	}
	if hasEl {
		if v, err := strconv.ParseFloat(strings.TrimSpace(elStr), 64); err == nil {
			el = v
		}
	}
	s.submit(Command{Kind: KindSetPos, Az: az, El: el})
	return ""
}

// tagValue returns the text content of the first <tag>…</tag> pair in s and
// whether it was found. s is expected to be already upper-cased.
func tagValue(s, tag string) (string, bool) {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	i += len(open)
	j := strings.Index(s[i:], close)
	if j < 0 {
		return "", false
	}
	return s[i : i+j], true
}
