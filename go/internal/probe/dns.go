// Package probe answers just enough DNS to be measurable from the outside.
//
// Globalping can only generate UDP traffic as DNS queries, so a tiny
// authoritative responder is how we find out which UDP ports still carry
// traffic from Russia.
//
// It is deliberately not a resolver. It answers A queries with one fixed
// address, refuses everything else, and rate limits each source, so it cannot
// be used for a reflection attack: the reply is barely larger than the query
// and there is nothing recursive behind it.
package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Answer is the address every A query is answered with. It is in the
// documentation range, so nothing can be driven anywhere real by it.
var Answer = net.IPv4(192, 0, 2, 1).To4()

// Rate limiting: a source may ask this often before being ignored.
const (
	rateBurst  = 6
	rateWindow = 10 * time.Second
)

// DNS response codes used here.
const (
	rcodeOK      = 0
	rcodeRefused = 5
)

var errTooShort = errors.New("query too short")

// Responder answers queries on one UDP port and counts what it saw.
type Responder struct {
	Port int

	conn *net.UDPConn

	mu       sync.Mutex
	received uint64
	answered uint64
	refused  uint64
	limited  uint64
	lastFrom string
	lastSeen time.Time
	seen     map[string]*bucket
}

type bucket struct {
	count int
	since time.Time
}

// Stats is what the responder reports about itself.
type Stats struct {
	Port     int    `json:"port"`
	Received uint64 `json:"received"`
	Answered uint64 `json:"answered"`
	Refused  uint64 `json:"refused"`
	Limited  uint64 `json:"limited"`
	LastFrom string `json:"last_from,omitempty"`
	LastSeen string `json:"last_seen,omitempty"`
}

// NewResponder binds the port, on one address when bindIP is given and on
// every address otherwise.
//
// Binding one address matters on port 53: a resolver stub on 127.0.0.53 makes
// a wildcard bind fail, and we only ever want to answer on the public
// interface anyway.
//
// Port 0 picks a free one, which the tests use; the Responder reports
// whichever port it actually got.
func NewResponder(bindIP string, port int) (*Responder, error) {
	addr := &net.UDPAddr{Port: port}
	if bindIP != "" {
		ip := net.ParseIP(bindIP)
		if ip == nil {
			return nil, fmt.Errorf("%q is not an address", bindIP)
		}
		addr.IP = ip
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	bound := port
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		bound = addr.Port
	}
	return &Responder{Port: bound, conn: conn, seen: map[string]*bucket{}}, nil
}

// Serve answers until the connection is closed.
func (r *Responder) Serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		r.note(from)

		if !r.allow(from) {
			continue
		}
		reply, err := buildReply(buf[:n])
		if err != nil {
			r.bump(&r.refused)
			continue
		}
		if _, err := r.conn.WriteToUDP(reply, from); err == nil {
			r.bump(&r.answered)
		}
	}
}

// Close stops the responder.
func (r *Responder) Close() error { return r.conn.Close() }

// Stats returns a snapshot.
func (r *Responder) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Stats{
		Port:     r.Port,
		Received: r.received,
		Answered: r.answered,
		Refused:  r.refused,
		Limited:  r.limited,
		LastFrom: r.lastFrom,
	}
	if !r.lastSeen.IsZero() {
		s.LastSeen = r.lastSeen.UTC().Format(time.RFC3339)
	}
	return s
}

func (r *Responder) note(from *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.received++
	r.lastFrom = from.IP.String()
	r.lastSeen = time.Now()
}

func (r *Responder) bump(field *uint64) {
	r.mu.Lock()
	*field++
	r.mu.Unlock()
}

// allow applies the per-source rate limit.
func (r *Responder) allow(from *net.UDPAddr) bool {
	key := from.IP.String()
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Keep the table from growing without bound.
	if len(r.seen) > 4096 {
		for k, b := range r.seen {
			if now.Sub(b.since) > rateWindow {
				delete(r.seen, k)
			}
		}
	}

	b, ok := r.seen[key]
	if !ok || now.Sub(b.since) > rateWindow {
		r.seen[key] = &bucket{count: 1, since: now}
		return true
	}
	b.count++
	if b.count > rateBurst {
		r.limited++
		return false
	}
	return true
}

// buildReply turns a query into a minimal authoritative answer.
//
// Only a single A/IN question is answered; anything else is REFUSED, and a
// malformed query gets no reply at all.
func buildReply(query []byte) ([]byte, error) {
	if len(query) < 12 {
		return nil, errTooShort
	}
	// A response, not a query, and only one question.
	if query[2]&0x80 != 0 {
		return nil, errors.New("not a query")
	}
	if binary.BigEndian.Uint16(query[4:6]) != 1 {
		return nil, errors.New("expected exactly one question")
	}

	// Walk the name to find where the question ends.
	i := 12
	for {
		if i >= len(query) {
			return nil, errTooShort
		}
		l := int(query[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 {
			// Compression pointers have no place in a question.
			return nil, errors.New("compressed question name")
		}
		i += l + 1
	}
	if i+4 > len(query) {
		return nil, errTooShort
	}
	qtype := binary.BigEndian.Uint16(query[i : i+2])
	qclass := binary.BigEndian.Uint16(query[i+2 : i+4])
	questionEnd := i + 4

	out := make([]byte, 0, questionEnd+16)
	out = append(out, query[:questionEnd]...)

	// QR=1, AA=1, RD copied from the query, RA=0.
	out[2] = 0x84 | (query[2] & 0x01)
	out[3] = 0x00

	const aRecord, classIN = 1, 1
	if qtype != aRecord || qclass != classIN {
		out[3] = rcodeRefused
		binary.BigEndian.PutUint16(out[6:8], 0) // no answers
		binary.BigEndian.PutUint16(out[8:10], 0)
		binary.BigEndian.PutUint16(out[10:12], 0)
		return out, nil
	}

	out[3] = rcodeOK
	binary.BigEndian.PutUint16(out[6:8], 1) // one answer
	binary.BigEndian.PutUint16(out[8:10], 0)
	binary.BigEndian.PutUint16(out[10:12], 0)

	// Answer: point back at the question's name rather than repeating it.
	out = append(out, 0xC0, 0x0C)
	out = binary.BigEndian.AppendUint16(out, aRecord)
	out = binary.BigEndian.AppendUint16(out, classIN)
	out = binary.BigEndian.AppendUint32(out, 60)
	out = binary.BigEndian.AppendUint16(out, 4)
	out = append(out, Answer...)
	return out, nil
}
