package splitdns

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// queryTimeout bounds how long one query may take end to end, which is
	// longer than a single upstream attempt because two views are asked.
	queryTimeout = 5 * time.Second

	// maxInFlight bounds concurrent queries per transport. Only authenticated
	// tunnel peers can reach the resolver, so this guards against a runaway
	// client, not an attacker.
	maxInFlight = 256

	// tcpIdle is how long a TCP connection may sit without a query.
	tcpIdle = 10 * time.Second

	// plainUDPLimit is the largest UDP reply allowed when the client did not
	// advertise a bigger buffer.
	plainUDPLimit = 512
)

// Serve answers queries on both transports until the context ends.
func (s *Server) Serve(ctx context.Context, udp *net.UDPConn, tcp net.Listener) error {
	errc := make(chan error, 2)
	go func() { errc <- s.serveUDP(ctx, udp) }()
	go func() { errc <- s.serveTCP(ctx, tcp) }()

	// Closing the sockets is how the blocked reads are woken up.
	go func() {
		<-ctx.Done()
		udp.Close()
		tcp.Close()
	}()

	first := <-errc
	udp.Close()
	tcp.Close()
	<-errc
	if ctx.Err() != nil {
		return nil
	}
	return first
}

func (s *Server) serveUDP(ctx context.Context, conn *net.UDPConn) error {
	sem := make(chan struct{}, maxInFlight)
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		pkt := append([]byte(nil), buf[:n]...)

		select {
		case sem <- struct{}{}:
		default:
			// Overloaded: dropping is the correct DNS response, the client
			// retries.
			continue
		}
		go func() {
			defer func() { <-sem }()
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()

			resp := s.Handle(qctx, pkt)
			if resp == nil {
				return
			}
			_, _ = conn.WriteToUDP(truncate(resp, udpLimit(pkt)), from)
		}()
	}
}

func (s *Server) serveTCP(ctx context.Context, ln net.Listener) error {
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer conn.Close()
			s.serveConn(ctx, conn)
		}()
	}
}

// serveConn answers length-prefixed queries on one TCP connection until it
// goes idle or closes.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	for {
		_ = conn.SetDeadline(time.Now().Add(tcpIdle))

		var lenBuf [2]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		size := binary.BigEndian.Uint16(lenBuf[:])
		if size == 0 {
			return
		}
		pkt := make([]byte, size)
		if _, err := io.ReadFull(conn, pkt); err != nil {
			return
		}

		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		resp := s.Handle(qctx, pkt)
		cancel()
		if resp == nil {
			return
		}

		framed := make([]byte, 2, 2+len(resp))
		binary.BigEndian.PutUint16(framed, uint16(len(resp)))
		if _, err := conn.Write(append(framed, resp...)); err != nil {
			return
		}
	}
}

// udpLimit returns the largest reply the client said it can take: the EDNS
// buffer size if it sent one, 512 bytes otherwise.
func udpLimit(query []byte) int {
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil {
		return plainUDPLimit
	}
	for _, r := range msg.Additionals {
		if r.Header.Type == dnsmessage.TypeOPT {
			// For an OPT record the class field carries the buffer size.
			return max(int(r.Header.Class), plainUDPLimit)
		}
	}
	return plainUDPLimit
}

// truncate shrinks a reply that does not fit. It keeps the header and
// question, empties the rest and sets TC, which tells the client to ask again
// over TCP.
func truncate(resp []byte, limit int) []byte {
	if len(resp) <= limit {
		return resp
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(resp); err != nil {
		return resp
	}
	msg.Truncated = true
	msg.Answers, msg.Authorities, msg.Additionals = nil, nil, nil
	out, err := msg.Pack()
	if err != nil {
		return resp
	}
	return out
}
