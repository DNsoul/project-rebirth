package node

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"awgrotator/internal/awg"
)

func question(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x4242, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	raw, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTheNameComesFromTheKeyAndIsWellFormed(t *testing.T) {
	a, _ := awg.GenerateKeypair()
	b, _ := awg.GenerateKeypair()
	name := ReachName(a.Private)
	if name != ReachName(a.Private) {
		t.Fatal("the name changes between calls")
	}
	if name == ReachName(b.Private) {
		t.Fatal("two servers got the same name")
	}
	if strings.Contains(name, a.Private.Hex()[:8]) || strings.Contains(name, a.Public.Hex()[:8]) {
		t.Fatal("the name shows the key")
	}
	if _, err := dnsmessage.NewName(name + "."); err != nil || !strings.HasSuffix(name, ".example.com") {
		t.Fatalf("%q is not a usable name: %v", name, err)
	}
}

func TestThePortIsTheOneAfterTheTunnels(t *testing.T) {
	if ReachPort(52885) != 52886 || ReachPort(65535) != 65534 {
		t.Fatal(ReachPort(52885), ReachPort(65535))
	}
}

func TestOnlyTheOneQuestionIsAnswered(t *testing.T) {
	const name = "0123456789abcdef.example.com"
	reply, ok := answerReach(question(t, name, dnsmessage.TypeA), name)
	if !ok {
		t.Fatal("the right question got no answer")
	}
	var p dnsmessage.Parser
	h, err := p.Start(reply)
	if err != nil || !h.Response || h.ID != 0x4242 || h.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("header %+v, %v", h, err)
	}
	if _, err := p.AllQuestions(); err != nil {
		t.Fatal(err)
	}
	ans, err := p.AllAnswers()
	if err != nil || len(ans) != 1 {
		t.Fatalf("answers %+v, %v", ans, err)
	}
	if a, ok := ans[0].Body.(*dnsmessage.AResource); !ok || net.IP(a.A[:]).String() != ReachAnswer {
		t.Fatalf("answer %+v", ans[0].Body)
	}
	if len(reply) > len(question(t, name, dnsmessage.TypeA))+20 {
		t.Errorf("the answer is %d bytes: it must stay barely bigger than the question", len(reply))
	}

	// Letter case is not part of a name.
	if _, ok := answerReach(question(t, strings.ToUpper(name[:4])+name[4:], dnsmessage.TypeA), name); !ok {
		t.Error("the same name in other letters got no answer")
	}

	// Everything else gets silence, like a closed port.
	for what, q := range map[string][]byte{
		"another name":   question(t, "fedcba9876543210.example.com", dnsmessage.TypeA),
		"a real domain":  question(t, "ya.ru", dnsmessage.TypeA),
		"another type":   question(t, name, dnsmessage.TypeTXT),
		"junk":           []byte("GET / HTTP/1.1"),
		"empty":          nil,
		"a cut question": question(t, name, dnsmessage.TypeA)[:15],
	} {
		if _, ok := answerReach(q, name); ok {
			t.Errorf("%s got an answer", what)
		}
	}
	// An answer sent to it is not a question.
	if _, ok := answerReach(reply, name); ok {
		t.Error("a response was answered")
	}
}

func TestTheResponderAnswersOverRealUDPAndKeepsCount(t *testing.T) {
	const name = "0123456789abcdef.example.com"
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &ReachResponder{Port: pc.LocalAddr().(*net.UDPAddr).Port, Name: name}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, pc) }()

	c, err := net.Dial("udp4", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 512)

	// A wrong name: nothing comes back.
	_, _ = c.Write(question(t, "other.example.com", dnsmessage.TypeA))
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := c.Read(buf); err == nil {
		t.Fatalf("a wrong name was answered with %d bytes", n)
	}

	_, _ = c.Write(question(t, name, dnsmessage.TypeA))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("no answer: %v", err)
	}
	if r.Answered() != 1 {
		t.Errorf("answered %d", r.Answered())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the responder did not stop")
	}
}

func TestAnswersAreCappedPerSecond(t *testing.T) {
	r := &ReachResponder{}
	now := time.Unix(1_000_000, 0)
	allowed := 0
	for i := 0; i < 100; i++ {
		if r.allow(now) {
			allowed++
		}
	}
	if allowed != reachPerSecond {
		t.Fatalf("%d answers in one second", allowed)
	}
	if !r.allow(now.Add(time.Second)) {
		t.Fatal("the next second starts afresh")
	}
}
