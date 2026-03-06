package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"
)

const stunMsgSize = 8
const (
	MAX_SESSIONS = 1024
	SESSION_TTL  = 60
)

type session struct {
	pubkey     uint16
	peer       *net.UDPAddr
	registered time.Time
	active     bool
}
type stunServer struct {
	conn     *net.UDPConn
	sessions [MAX_SESSIONS]*session
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr,
			" handshake server (Go)\nUsage: %s <port>\n", os.Args[0])
		os.Exit(1)
	}
	addr, err := net.ResolveUDPAddr("udp", ":"+os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve: %v\n", err)
		os.Exit(1)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	fmt.Fprintf(os.Stderr, "Server listening on UDP %s\n", os.Args[1])

	srv := &stunServer{conn: conn}
	srv.serve()
}

func (s *stunServer) serve() {
	buf := make([]byte, stunMsgSize)
	for {
		n, peerAddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recvfrom: %v\n", err)
			continue
		}
		if n != stunMsgSize {
			continue
		}
		sid := binary.BigEndian.Uint16(buf[6:8])
		if sid == 0 {
			continue
		}
		s.handlePacket(peerAddr, buf, sid)
	}
}

func (s *stunServer) handlePacket(peerAddr *net.UDPAddr, _ []byte, sid uint16) {
	now := time.Now()
	waiting := s.findSession(sid)
	if waiting == nil {
		slot := s.allocSession(now)
		if slot == nil {
			fmt.Fprintf(os.Stderr, "Session table full\n")
			return
		}
		slot.pubkey = sid
		slot.peer = peerAddr
		slot.registered = now
		slot.active = true
		fmt.Printf("Registration: %s session=%d waiting\n", peerAddr, sid)
	} else if samePeer(waiting.peer, peerAddr) {
		waiting.registered = now
		fmt.Printf("Keepalive: %s session=%d\n", peerAddr, sid)
	} else {
		if now.Sub(waiting.registered) > SESSION_TTL*time.Second {
			fmt.Printf("Expired session=%d replaced by %s\n", sid, peerAddr)
			waiting.peer = peerAddr
			waiting.registered = now
			return
		}
		s.sendReply(waiting.peer, peerAddr, sid)
		s.sendReply(peerAddr, waiting.peer, sid)

		fmt.Printf("Paired session=%d  %s <--> %s\n",
			sid, waiting.peer, peerAddr)

		waiting.active = false
	}
}
func (s *stunServer) sendReply(dst *net.UDPAddr, target *net.UDPAddr, sid uint16) {
	buf := make([]byte, stunMsgSize)
	ip := target.IP.To4()
	if ip == nil {
		fmt.Fprintf(os.Stderr, "sendReply: non-IPv4 address %s\n", target)
		return
	}
	copy(buf[0:4], ip)
	binary.BigEndian.PutUint16(buf[4:6], uint16(target.Port))
	binary.BigEndian.PutUint16(buf[6:8], sid)

	if _, err := s.conn.WriteToUDP(buf, dst); err != nil {
		fmt.Fprintf(os.Stderr, "sendto %s: %v\n", dst, err)
	}
}
func (s *stunServer) findSession(sid uint16) *session {
	for _, sess := range s.sessions {
		if sess != nil && sess.active && sess.pubkey == sid {
			return sess
		}
	}
	return nil
}

func (s *stunServer) allocSession(now time.Time) *session {
	for i, sess := range s.sessions {
		if sess == nil {
			s.sessions[i] = &session{}
			return s.sessions[i]
		}
		if !sess.active || now.Sub(sess.registered) > SESSION_TTL*time.Second {
			sess.active = false
			return sess
		}
	}
	return nil
}

func samePeer(a, b *net.UDPAddr) bool {
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
