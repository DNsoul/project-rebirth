package bot

import (
	"bytes"
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/proto"
	"awgrotator/internal/telegram"
)

const owner, stranger int64 = 1001, 2002

type sent struct {
	chat, msg int64
	text      string
	kb        telegram.Keyboard
}

type fakeAPI struct {
	mu       sync.Mutex
	batches  [][]telegram.Update
	conflict int // how many polls answer 409 first
	polls    []int64
	sends    []sent
	edits    []sent
	answers  []string
}

func (f *fakeAPI) GetUpdates(ctx context.Context, offset int64, _ time.Duration) ([]telegram.Update, error) {
	f.mu.Lock()
	f.polls = append(f.polls, offset)
	if f.conflict > 0 {
		f.conflict--
		f.mu.Unlock()
		return nil, &telegram.Error{Method: "getUpdates", Code: 409, Description: "Conflict"}
	}
	if len(f.batches) > 0 {
		b := f.batches[0]
		f.batches = f.batches[1:]
		f.mu.Unlock()
		return b, nil
	}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Millisecond):
	}
	return nil, nil
}

func (f *fakeAPI) Send(_ context.Context, chat int64, text string, kb telegram.Keyboard) (telegram.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sent{chat: chat, text: text, kb: kb})
	return telegram.Message{MessageID: int64(len(f.sends)), Chat: telegram.Chat{ID: chat, Type: "private"}}, nil
}

func (f *fakeAPI) Edit(_ context.Context, chat, msg int64, text string, kb telegram.Keyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, sent{chat: chat, msg: msg, text: text, kb: kb})
	return nil
}

func (f *fakeAPI) Answer(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, text)
	return nil
}

func (f *fakeAPI) SetCommands(context.Context, []telegram.Command) error { return nil }

func (f *fakeAPI) snapshot() (sends, edits []sent, answers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sends...), append([]sent(nil), f.edits...), append([]string(nil), f.answers...)
}

type fakeManager struct {
	mu      sync.Mutex
	snap    lifecycle.Snapshot
	actions []lifecycle.Action
}

func (m *fakeManager) Snapshot() lifecycle.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap
}

func (m *fakeManager) Do(a lifecycle.Action) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.actions = append(m.actions, a)
	return nil
}

func (m *fakeManager) set(f func(*lifecycle.Snapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.snap)
}

type direct struct{ c *node.Controller }

func (d direct) Control(_ context.Context, req node.ControlRequest) (node.ControlResponse, error) {
	return d.c.Handle(req), nil
}

type tunnelSink struct{}

func (tunnelSink) SetPeer(awg.Key, string) error { return nil }
func (tunnelSink) RemovePeer(awg.Key) error      { return nil }

var content = bytes.Repeat([]byte{5}, proto.ContentKeySize)

type rig struct {
	bot *Bot
	api *fakeAPI
	mgr *fakeManager
	srv *enroll.Server
	now time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pool, err := enroll.NewPool("10.8.0.0/24", 2, 249)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := &rig{api: &fakeAPI{}, now: time.Unix(1_800_000_000, 0)}
	r.srv = &enroll.Server{
		Registry: enroll.NewRegistry(filepath.Join(dir, "r.json"), pool, enroll.Options{}),
		Peers:    tunnelSink{}, ContentKey: content, From: netip.MustParseAddr("10.8.0.251"),
		Access:   enroll.NewAccess(filepath.Join(dir, "a.json"), enroll.AccessOptions{}),
		Approval: true,
		Now:      func() time.Time { return r.now },
	}
	r.mgr = &fakeManager{snap: lifecycle.Snapshot{
		Current: true, CurrentKnown: true, Region: "sto", Generation: 7,
		CreatedAt: r.now.Add(-50 * time.Hour), Health: lifecycle.Healthy,
		Regions: []string{"sto", "waw", "fra"}, AutoAge: true,
	}}
	r.bot = &Bot{
		API: r.api, Manager: r.mgr, Owners: []int64{owner},
		Devices: direct{&node.Controller{Srv: r.srv, Now: func() time.Time { return r.now }}},
		Now:     func() time.Time { return r.now },
		Wait:    time.Millisecond, Idle: time.Millisecond, WatchEvery: time.Millisecond,
	}
	r.bot.init()
	return r
}

// ask makes device id/key ask to come in, the way the app does.
func (r *rig) ask(t *testing.T, id, key byte, name string) string {
	t.Helper()
	req := &proto.EnrollRequest{
		V: proto.FormatVersion, Kind: proto.KindEnroll,
		DeviceID:  proto.Encode(bytes.Repeat([]byte{id}, proto.DeviceIDSize)),
		PublicKey: proto.Encode(bytes.Repeat([]byte{key}, proto.TunnelKeySize)),
		Nonce:     proto.Encode(bytes.Repeat([]byte{0xAB}, proto.EnrollNonceSize)), IssuedAt: r.now.Unix(), Name: name,
	}
	blob, err := req.Seal(content)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := proto.OpenEnrollResponse(content, string(r.srv.Handle(netip.MustParseAddr("10.8.0.251"), []byte(blob))), req.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Error != "" {
		return reply.Error
	}
	return reply.Address
}

func message(from int64, chatType, text string) telegram.Update {
	return telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 1, From: &telegram.User{ID: from}, Chat: telegram.Chat{ID: from, Type: chatType}, Text: text,
	}}
}

func press(from int64, data string) telegram.Update {
	return telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{
		ID: "cb", From: telegram.User{ID: from}, Data: data,
		Message: &telegram.Message{MessageID: 9, Chat: telegram.Chat{ID: from, Type: "private"}},
	}}
}

func idOf(b byte) string { return proto.Encode(bytes.Repeat([]byte{b}, proto.DeviceIDSize)) }

func TestStrangersGetNoAnswerAtAll(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.ask(t, 1, 0x11, "phone")
	r.bot.handle(ctx, message(stranger, "private", "/status"))
	r.bot.handle(ctx, press(stranger, "ap:"+idOf(1)))
	r.bot.handle(ctx, message(owner, "group", "/status"))
	if s, e, a := r.api.snapshot(); len(s)+len(e)+len(a) != 0 {
		t.Fatalf("answered someone it should not: sends %v edits %v answers %v", s, e, a)
	}
	if got := r.srv.Overview(r.now); len(got.Devices) != 0 {
		t.Fatal("a stranger's button press approved a device")
	}
}

func TestTheOwnerSeesTheStatus(t *testing.T) {
	r := newRig(t)
	r.mgr.set(func(s *lifecycle.Snapshot) { s.Why = "could not reach 203.0.113.9:22" })
	r.ask(t, 1, 0x11, "phone")
	r.bot.handle(context.Background(), message(owner, "private", "/status"))
	sends, _, _ := r.api.snapshot()
	if len(sends) != 1 || sends[0].chat != owner {
		t.Fatalf("sends = %+v", sends)
	}
	text := sends[0].text
	for _, want := range []string{"Стокгольм", "поколение 7", "в порядке", "Ждут одобрения: 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}
}

func TestNoAddressIsEverSent(t *testing.T) {
	if got := scrubAddresses("dial tcp 203.0.113.9:22: refused, and 10.8.0.7"); strings.Contains(got, "203.0.113.9") || strings.Contains(got, "10.8.0.7") {
		t.Fatalf("an address got through: %q", got)
	}
	r := newRig(t)
	r.bot.later(owner, "failed at 198.51.100.4")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sends, _, _ := r.api.snapshot(); len(sends) == 1 {
			if strings.Contains(sends[0].text, "198.51.100.4") {
				t.Fatalf("an address was sent: %q", sends[0].text)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the message was not sent")
}

func TestANewRequestIsAnnouncedOnceWithItsButtons(t *testing.T) {
	r := newRig(t)
	r.bot.Owners = []int64{owner, 3003}
	if got := r.ask(t, 1, 0x11, "Xiaomi Redmi Note 12"); got != proto.EnrollPending {
		t.Fatalf("ask = %s", got)
	}
	ctx := context.Background()
	r.bot.announceRequests(ctx)
	r.bot.announceRequests(ctx)

	sends, _, _ := r.api.snapshot()
	if len(sends) != 2 {
		t.Fatalf("want one announcement per owner, got %d: %+v", len(sends), sends)
	}
	code := proto.ApprovalCode(bytes.Repeat([]byte{0x11}, proto.TunnelKeySize))
	if !strings.Contains(sends[0].text, "Xiaomi Redmi Note 12") || !strings.Contains(sends[0].text, code) {
		t.Errorf("announcement = %q", sends[0].text)
	}
	if kb := sends[0].kb; len(kb) != 1 || kb[0][0].Data != "ap:"+idOf(1) || kb[0][1].Data != "dn:"+idOf(1) {
		t.Errorf("buttons = %+v", kb)
	}
}

func TestNothingIsAnnouncedFromAServerThatDoesNotAnswerOrIsMoving(t *testing.T) {
	r := newRig(t)
	r.ask(t, 1, 0x11, "phone")
	ctx := context.Background()
	r.mgr.set(func(s *lifecycle.Snapshot) { s.Current = false })
	r.bot.announceRequests(ctx)
	r.mgr.set(func(s *lifecycle.Snapshot) { s.Current, s.Moving = true, true })
	r.bot.announceRequests(ctx)
	if sends, _, _ := r.api.snapshot(); len(sends) != 0 {
		t.Fatalf("announced: %+v", sends)
	}
	if r.srv.Overview(r.now).Pending[0].Notified {
		t.Fatal("marked as told without telling")
	}
}

func TestApprovingFromTheAnnouncementLetsTheDeviceIn(t *testing.T) {
	r := newRig(t)
	r.ask(t, 1, 0x11, "phone")
	r.bot.handle(context.Background(), press(owner, "ap:"+idOf(1)))
	if got := r.ask(t, 1, 0x11, "phone"); !strings.HasPrefix(got, "10.8.0.") {
		t.Fatalf("after approval the device got %q", got)
	}
	_, edits, answers := r.api.snapshot()
	if len(edits) != 1 || !strings.HasPrefix(edits[0].text, "Одобрено: phone") || edits[0].kb != nil && len(edits[0].kb) != 0 {
		t.Fatalf("edits = %+v", edits)
	}
	if len(answers) != 1 {
		t.Fatalf("the press was not acknowledged: %v", answers)
	}

	// The same button again: already decided, nothing breaks.
	r.bot.handle(context.Background(), press(owner, "ap:"+idOf(1)))
	_, edits, _ = r.api.snapshot()
	if !strings.Contains(edits[1].text, "Уже решено") {
		t.Fatalf("second press = %q", edits[1].text)
	}
}

func TestAListButtonRedrawsTheList(t *testing.T) {
	r := newRig(t)
	r.ask(t, 1, 0x11, "one")
	r.ask(t, 2, 0x22, "two")
	r.bot.handle(context.Background(), press(owner, "dn:"+idOf(1)+":l"))
	_, edits, _ := r.api.snapshot()
	if len(edits) != 1 || !strings.Contains(edits[0].text, "Отказано: one") || !strings.Contains(edits[0].text, "Ждут одобрения (1)") {
		t.Fatalf("edit = %+v", edits)
	}
}

func TestRevokingAsksFirstAndThenCutsTheDeviceOff(t *testing.T) {
	r := newRig(t)
	r.ask(t, 1, 0x11, "phone")
	if _, err := r.srv.Approve(idOf(1), r.now); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r.bot.handle(ctx, press(owner, "rv:"+idOf(1)))
	_, edits, _ := r.api.snapshot()
	if len(edits) != 1 || edits[0].kb[0][0].Data != "rv!:"+idOf(1) {
		t.Fatalf("no confirmation: %+v", edits)
	}
	if len(r.srv.Overview(r.now).Devices) != 1 {
		t.Fatal("revoked before it was confirmed")
	}
	r.bot.handle(ctx, press(owner, "rv!:"+idOf(1)))
	if got := r.ask(t, 1, 0x11, "phone"); got != proto.EnrollDenied {
		t.Fatalf("a revoked device got %q", got)
	}
	_, edits, _ = r.api.snapshot()
	if !strings.Contains(edits[1].text, "Доступ отозван: phone") {
		t.Fatalf("edit = %q", edits[1].text)
	}
}

func TestNoDecisionsAboutDevicesDuringAMove(t *testing.T) {
	r := newRig(t)
	r.ask(t, 1, 0x11, "phone")
	r.mgr.set(func(s *lifecycle.Snapshot) { s.Moving = true })
	r.bot.handle(context.Background(), press(owner, "ap:"+idOf(1)))
	if len(r.srv.Overview(r.now).Devices) != 0 {
		t.Fatal("approved during a move: the replacement would not know")
	}
	_, _, answers := r.api.snapshot()
	if len(answers) != 1 || !strings.Contains(answers[0], "переезд") {
		t.Fatalf("answers = %v", answers)
	}
}

func TestAMoveIsConfirmedAndThenHandedToTheManagerOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.bot.handle(ctx, press(owner, "mv"))
	_, edits, _ := r.api.snapshot()
	data := edits[0].kb[0][0].Data
	if !strings.HasPrefix(data, "mv!:") {
		t.Fatalf("confirmation button = %q", data)
	}
	if len(r.mgr.actions) != 0 {
		t.Fatal("moved before it was confirmed")
	}
	r.bot.handle(ctx, press(owner, data))
	if len(r.mgr.actions) != 1 || r.mgr.actions[0].Kind != lifecycle.ActMove || r.mgr.actions[0].ID == "" || r.mgr.actions[0].Region != "" {
		t.Fatalf("actions = %+v", r.mgr.actions)
	}
	if len(data) > 64 {
		t.Errorf("button data is %d bytes, Telegram allows 64", len(data))
	}
}

func TestAMoveToAChosenRegionNamesIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.bot.handle(ctx, press(owner, "mr"))
	_, edits, _ := r.api.snapshot()
	var waw string
	for _, row := range edits[0].kb {
		for _, b := range row {
			if b.Text == "Стокгольм" {
				t.Error("the current region is offered as a destination")
			}
			if b.Text == "Варшава" {
				waw = b.Data
			}
		}
	}
	if waw == "" {
		t.Fatalf("no button for Warsaw: %+v", edits[0].kb)
	}
	r.bot.handle(ctx, press(owner, waw))
	if len(r.mgr.actions) != 1 || r.mgr.actions[0].Region != "waw" {
		t.Fatalf("actions = %+v", r.mgr.actions)
	}
}

func TestSwitchingAutomaticMovesSaysWhatItWillBe(t *testing.T) {
	r := newRig(t)
	r.bot.handle(context.Background(), press(owner, "pa:off"))
	if len(r.mgr.actions) != 1 || r.mgr.actions[0].AutoAge == nil || *r.mgr.actions[0].AutoAge || r.mgr.actions[0].AutoBlock != nil {
		t.Fatalf("actions = %+v", r.mgr.actions)
	}
	_, edits, _ := r.api.snapshot()
	found := false
	for _, row := range edits[0].kb {
		for _, b := range row {
			if b.Data == "pa:on" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the menu still offers to switch it off: %+v", edits[0].kb)
	}
}

func TestTheOutcomeOfAnActionArrivesAsAMessage(t *testing.T) {
	r := newRig(t)
	r.bot.handle(context.Background(), press(owner, "ck"))
	a := r.mgr.actions[0]
	a.Done(lifecycle.Result{Check: &lifecycle.CheckResult{
		Target: lifecycle.Reach{Reached: 9, Total: 10}, Control: lifecycle.Reach{Reached: 10, Total: 10}, Round: lifecycle.Good,
	}})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sends, _, _ := r.api.snapshot(); len(sends) == 1 {
			if !strings.Contains(sends[0].text, "9 из 10") || !strings.Contains(sends[0].text, "доступен из России") {
				t.Fatalf("message = %q", sends[0].text)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no message about the check")
}

func TestOnlyTheServerTheAppsUseReadsUpdates(t *testing.T) {
	r := newRig(t)
	r.mgr.set(func(s *lifecycle.Snapshot) { s.Current = false })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = r.bot.Run(ctx)
	r.api.mu.Lock()
	defer r.api.mu.Unlock()
	if len(r.api.polls) != 0 {
		t.Fatalf("a server that does not answer read updates %d times", len(r.api.polls))
	}
}

func TestUpdatesAreHandledOnceAndAConflictIsWaitedOut(t *testing.T) {
	r := newRig(t)
	r.api.conflict = 1
	r.api.batches = [][]telegram.Update{{
		{UpdateID: 40, Message: &telegram.Message{From: &telegram.User{ID: owner}, Chat: telegram.Chat{ID: owner, Type: "private"}, Text: "/start"}},
		{UpdateID: 41, Message: &telegram.Message{From: &telegram.User{ID: owner}, Chat: telegram.Chat{ID: owner, Type: "private"}, Text: "/service"}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.bot.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.api.mu.Lock()
		n := len(r.api.polls)
		var last int64
		if n > 0 {
			last = r.api.polls[n-1]
		}
		r.api.mu.Unlock()
		if n >= 3 && last == 42 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	sends, _, _ := r.api.snapshot()
	if len(sends) != 2 {
		t.Fatalf("want two answers, got %+v", sends)
	}
	r.api.mu.Lock()
	defer r.api.mu.Unlock()
	if r.api.polls[len(r.api.polls)-1] != 42 {
		t.Fatalf("offsets = %v: the handled updates were not confirmed", r.api.polls)
	}
}

func TestEventsComeFromTheServerThatAnswersAndTheOneHandingOver(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.bot.watch(ctx)

	r.mgr.set(func(s *lifecycle.Snapshot) { s.Current = false })
	r.bot.Notify(lifecycle.Event{Kind: lifecycle.EventHealth, Health: lifecycle.Blocked})
	r.bot.Notify(lifecycle.Event{Kind: lifecycle.EventMovePublished, Region: "waw"})
	// The manager tells it has taken over a moment before its snapshot shows
	// it; that news must not be lost.
	r.bot.Notify(lifecycle.Event{Kind: lifecycle.EventCurrent, Region: "sto", Generation: 7})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sends, _, _ := r.api.snapshot(); len(sends) >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	sends, _, _ := r.api.snapshot()
	if len(sends) != 2 || !strings.Contains(sends[0].text, "Варшава") || !strings.Contains(sends[1].text, "на связи") {
		t.Fatalf("sends = %+v", sends)
	}
}

func TestEveryButtonFitsTelegramsLimit(t *testing.T) {
	r := newRig(t)
	for i := byte(1); i <= 5; i++ {
		r.ask(t, i, 0x10+i, strings.Repeat("Длинное имя устройства ", 3)[:32])
	}
	ctx := context.Background()
	views := []view{r.bot.requestsView(ctx), r.bot.serviceView(), r.bot.regionView(), menuView()}
	if _, err := r.srv.Approve(idOf(1), r.now); err != nil {
		t.Fatal(err)
	}
	views = append(views, r.bot.devicesView(ctx))
	for _, v := range views {
		for _, row := range v.kb {
			for _, b := range row {
				if len(b.Data) > 64 || b.Data == "" {
					t.Errorf("button %q has %d bytes of data", b.Text, len(b.Data))
				}
			}
		}
	}
}
