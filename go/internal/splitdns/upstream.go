package splitdns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Resolver asks an upstream DNS server a question. It is an interface so the
// server can be tested against canned answers.
type Resolver interface {
	Query(ctx context.Context, name string, qtype dnsmessage.Type) (*dnsmessage.Message, error)
}

// ecsOptionCode is the EDNS option number for Client Subnet (RFC 7871).
const ecsOptionCode = 8

// udpPayload is the EDNS payload size we advertise. 1232 is the value the DNS
// flag day settled on to avoid fragmentation.
const udpPayload = 1232

// Upstream is one recursive resolver reached over UDP, with TCP as the
// fallback when an answer is truncated.
type Upstream struct {
	// Addr is host:port.
	Addr string

	// ECS, when valid, is sent as the client subnet. It makes a resolver that
	// supports it answer as if asked from that network, which is how a
	// content network hands out the addresses it would give a local client.
	// The subnet is a fixed stand-in; nothing about the real client is sent.
	ECS netip.Prefix

	// Timeout bounds one exchange. The default is three seconds.
	Timeout time.Duration

	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Query sends one question and returns the parsed answer.
func (u *Upstream) Query(ctx context.Context, name string, qtype dnsmessage.Type) (*dnsmessage.Message, error) {
	query, id, err := u.buildQuery(name, qtype)
	if err != nil {
		return nil, err
	}

	raw, err := u.exchange(ctx, "udp", query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u.Addr, err)
	}
	msg, err := checkReply(raw, id, name, qtype)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u.Addr, err)
	}

	if msg.Truncated {
		raw, err = u.exchange(ctx, "tcp", query)
		if err != nil {
			return nil, fmt.Errorf("%s (tcp): %w", u.Addr, err)
		}
		if msg, err = checkReply(raw, id, name, qtype); err != nil {
			return nil, fmt.Errorf("%s (tcp): %w", u.Addr, err)
		}
	}
	return msg, nil
}

func (u *Upstream) buildQuery(name string, qtype dnsmessage.Type) ([]byte, uint16, error) {
	// An empty name would become the root zone below and go out as a valid
	// query for ".", which no client has a reason to ask and we have no reason
	// to forward.
	if strings.Trim(name, ".") == "" {
		return nil, 0, errors.New("empty query name")
	}
	fqdn := name
	if !strings.HasSuffix(fqdn, ".") {
		fqdn += "."
	}
	qname, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return nil, 0, fmt.Errorf("%q is not a usable query name: %w", name, err)
	}

	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, 0, err
	}
	id := binary.BigEndian.Uint16(idBytes[:])

	opt := dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(".")},
		Body:   &dnsmessage.OPTResource{},
	}
	if err := opt.Header.SetEDNS0(udpPayload, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, 0, err
	}
	if u.ECS.IsValid() {
		opt.Body = &dnsmessage.OPTResource{Options: []dnsmessage.Option{{
			Code: ecsOptionCode,
			Data: ecsData(u.ECS),
		}}}
	}

	msg := dnsmessage.Message{
		Header:      dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions:   []dnsmessage.Question{{Name: qname, Type: qtype, Class: dnsmessage.ClassINET}},
		Additionals: []dnsmessage.Resource{opt},
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil, 0, err
	}
	return packed, id, nil
}

// ecsData encodes a Client Subnet option: family, source prefix length, scope
// prefix length (zero in a query), then only as many address bytes as the
// prefix covers.
func ecsData(p netip.Prefix) []byte {
	p = p.Masked()
	family := uint16(1)
	addr := p.Addr().AsSlice()
	if p.Addr().Is6() {
		family = 2
	}
	n := (p.Bits() + 7) / 8

	data := make([]byte, 4, 4+n)
	binary.BigEndian.PutUint16(data, family)
	data[2] = byte(p.Bits())
	data[3] = 0
	return append(data, addr[:n]...)
}

func (u *Upstream) exchange(ctx context.Context, network string, query []byte) ([]byte, error) {
	timeout := u.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dial := u.dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	conn, err := dial(ctx, network, u.Addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if network == "tcp" {
		framed := make([]byte, 2, 2+len(query))
		binary.BigEndian.PutUint16(framed, uint16(len(query)))
		if _, err := conn.Write(append(framed, query...)); err != nil {
			return nil, err
		}
		var lenBuf [2]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return nil, err
		}
		reply := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
		if _, err := io.ReadFull(conn, reply); err != nil {
			return nil, err
		}
		return reply, nil
	}

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// checkReply parses an answer and confirms it answers what was asked. A reply
// with the wrong ID or question is not an answer to us, whoever sent it.
func checkReply(raw []byte, id uint16, name string, qtype dnsmessage.Type) (*dnsmessage.Message, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(raw); err != nil {
		return nil, fmt.Errorf("unreadable reply: %w", err)
	}
	if !msg.Response {
		return nil, errors.New("reply is not marked as a response")
	}
	if msg.ID != id {
		return nil, errors.New("reply has the wrong ID")
	}
	if len(msg.Questions) != 1 {
		return nil, errors.New("reply does not repeat the question")
	}
	q := msg.Questions[0]
	if q.Type != qtype || !strings.EqualFold(strings.TrimSuffix(q.Name.String(), "."), strings.TrimSuffix(name, ".")) {
		return nil, errors.New("reply answers a different question")
	}
	return &msg, nil
}

// addrsOf collects the A records in an answer, with the smallest TTL among
// them. CNAME records are skipped: the resolver already followed them, and the
// answer we give back names the original query directly.
func addrsOf(msg *dnsmessage.Message) (addrs []netip.Addr, ttl uint32) {
	if msg == nil {
		return nil, 0
	}
	ttl = ^uint32(0)
	for _, r := range msg.Answers {
		a, ok := r.Body.(*dnsmessage.AResource)
		if !ok {
			continue
		}
		addrs = append(addrs, netip.AddrFrom4(a.A))
		if r.Header.TTL < ttl {
			ttl = r.Header.TTL
		}
	}
	if len(addrs) == 0 {
		ttl = 0
	}
	return addrs, ttl
}
