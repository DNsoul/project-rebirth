package probe

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

// query builds a well-formed DNS query for the given name and type.
func query(id uint16, name string, qtype, qclass uint16) []byte {
	out := make([]byte, 12)
	binary.BigEndian.PutUint16(out[0:2], id)
	out[2] = 0x01 // RD set, as a normal client would
	binary.BigEndian.PutUint16(out[4:6], 1)

	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	out = append(out, 0)
	out = binary.BigEndian.AppendUint16(out, qtype)
	out = binary.BigEndian.AppendUint16(out, qclass)
	return out
}

const (
	typeA    = 1
	typeAAAA = 28
	classIN  = 1
)

func TestBuildReplyAnswersA(t *testing.T) {
	q := query(0xBEEF, "probe.example.com", typeA, classIN)
	reply, err := buildReply(q)
	if err != nil {
		t.Fatalf("buildReply: %v", err)
	}

	if got := binary.BigEndian.Uint16(reply[0:2]); got != 0xBEEF {
		t.Errorf("id = %#x, want 0xBEEF", got)
	}
	if reply[2]&0x80 == 0 {
		t.Error("QR bit not set: the reply does not say it is a response")
	}
	if reply[2]&0x04 == 0 {
		t.Error("AA bit not set")
	}
	if reply[2]&0x01 == 0 {
		t.Error("RD was not copied from the query")
	}
	if rcode := reply[3] & 0x0F; rcode != rcodeOK {
		t.Errorf("rcode = %d, want 0", rcode)
	}
	if got := binary.BigEndian.Uint16(reply[6:8]); got != 1 {
		t.Errorf("answer count = %d, want 1", got)
	}

	// The answer address must be the documentation address.
	if got := net.IP(reply[len(reply)-4:]).String(); got != "192.0.2.1" {
		t.Errorf("answer = %s, want 192.0.2.1", got)
	}

	// Amplification check: the reply must stay close to the query in size,
	// or this responder becomes a reflection tool.
	if ratio := float64(len(reply)) / float64(len(q)); ratio > 1.6 {
		t.Errorf("reply is %.2fx the query (%d vs %d bytes)", ratio, len(reply), len(q))
	}
}

func TestBuildReplyRefusesEverythingElse(t *testing.T) {
	cases := []struct {
		name   string
		q      []byte
		qtype  uint16
		qclass uint16
	}{
		{"AAAA", query(1, "x.test", typeAAAA, classIN), typeAAAA, classIN},
		{"ANY", query(2, "x.test", 255, classIN), 255, classIN},
		{"CHAOS class", query(3, "x.test", typeA, 3), typeA, 3},
		{"TXT", query(4, "x.test", 16, classIN), 16, classIN},
	}
	for _, tc := range cases {
		reply, err := buildReply(tc.q)
		if err != nil {
			t.Errorf("%s: buildReply: %v", tc.name, err)
			continue
		}
		if rcode := reply[3] & 0x0F; rcode != rcodeRefused {
			t.Errorf("%s: rcode = %d, want REFUSED", tc.name, rcode)
		}
		if got := binary.BigEndian.Uint16(reply[6:8]); got != 0 {
			t.Errorf("%s: answered %d records, want none", tc.name, got)
		}
	}
}

func TestBuildReplyRejectsMalformed(t *testing.T) {
	good := query(1, "x.test", typeA, classIN)

	cases := map[string][]byte{
		"empty":           {},
		"header only":     good[:12],
		"truncated name":  good[:14],
		"truncated qtype": good[:len(good)-2],
		"two questions":   twoQuestions(),
		"already a reply": asReply(good),
		"compressed name": compressedName(),
	}
	for name, q := range cases {
		if _, err := buildReply(q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func twoQuestions() []byte {
	q := query(1, "x.test", typeA, classIN)
	binary.BigEndian.PutUint16(q[4:6], 2)
	return q
}

func asReply(q []byte) []byte {
	out := append([]byte(nil), q...)
	out[2] |= 0x80
	return out
}

func compressedName() []byte {
	out := make([]byte, 12)
	binary.BigEndian.PutUint16(out[4:6], 1)
	out = append(out, 0xC0, 0x0C) // a pointer where a label belongs
	out = binary.BigEndian.AppendUint16(out, typeA)
	out = binary.BigEndian.AppendUint16(out, classIN)
	return out
}

// The responder has to satisfy a real DNS client, not just our own parser.
func TestResponderAnswersGoResolver(t *testing.T) {
	r, err := NewResponder("", 0)
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}
	defer r.Close()
	go r.Serve()

	addr := r.conn.LocalAddr().String()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", addr)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addrs, err := resolver.LookupHost(ctx, "probe.example.com")
	if err != nil {
		t.Fatalf("LookupHost: %v", err)
	}
	if len(addrs) == 0 || addrs[0] != "192.0.2.1" {
		t.Fatalf("got %v, want [192.0.2.1]", addrs)
	}

	stats := r.Stats()
	if stats.Received == 0 || stats.Answered == 0 {
		t.Errorf("counters did not move: %+v", stats)
	}
	if stats.LastFrom == "" || stats.LastSeen == "" {
		t.Errorf("source was not recorded: %+v", stats)
	}
}

func TestRateLimitKicksIn(t *testing.T) {
	r, err := NewResponder("", 0)
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}
	defer r.Close()

	from := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 1234}
	allowed := 0
	for i := 0; i < rateBurst*3; i++ {
		if r.allow(from) {
			allowed++
		}
	}
	if allowed != rateBurst {
		t.Errorf("allowed %d of %d, want %d", allowed, rateBurst*3, rateBurst)
	}
	if got := r.Stats().Limited; got == 0 {
		t.Error("the limited counter did not move")
	}

	// A different source must not be affected by the first one's budget.
	other := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 8), Port: 1234}
	if !r.allow(other) {
		t.Error("a second source was limited by the first one's traffic")
	}
}

func TestStatsStartEmpty(t *testing.T) {
	r, err := NewResponder("", 0)
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}
	defer r.Close()

	s := r.Stats()
	if s.Received != 0 || s.Answered != 0 || s.LastFrom != "" {
		t.Errorf("fresh responder reports %+v", s)
	}
	if s.Port == 0 {
		t.Error("port was not recorded")
	}
}
