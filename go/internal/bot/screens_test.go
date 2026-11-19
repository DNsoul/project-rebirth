package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/enroll"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/telegram"
)

func TestColumnsLineUpInAFixedWidthFont(t *testing.T) {
	got := columns([][]string{
		{"1", "windows", "2176", "в сети"},
		{"10", "Xiaomi Redmi Note 9S", "8324", "6 мин"},
		{"3", "", "0001", ""},
	}, -2, 16, 4, 6)
	want := " 1 windows          2176 в сети\n" +
		"10 Xiaomi Redmi No… 8324 6 мин\n" +
		" 3                  0001"
	if got != want {
		t.Errorf("columns:\n%s\nwant\n%s", got, want)
	}
}

// devicesRig is the rig with n approved devices called dev01, dev02…
func devicesRig(t *testing.T, n int) *rig {
	r := newRig(t)
	for i := 1; i <= n; i++ {
		b := byte(i)
		r.ask(t, b, 0x40+b, fmt.Sprintf("dev%02d", i))
		if _, err := r.srv.Approve(idOf(b), r.now); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func data(kb telegram.Keyboard) []string {
	var out []string
	for _, row := range kb {
		for _, b := range row {
			out = append(out, b.Data)
		}
	}
	return out
}

func TestTheDeviceListIsPagedAndNumbered(t *testing.T) {
	r := devicesRig(t, 10)
	ctx := context.Background()

	first := r.bot.devicesView(ctx, 0)
	if !strings.HasPrefix(first.text, "Устройства: 0 в сети из 10.\n") || strings.Count(first.text, "\n") != 8 {
		t.Errorf("first page:\n%s", first.text)
	}
	if len(first.kb) != 4 || len(first.kb[0]) != 4 || len(first.kb[1]) != 4 ||
		first.kb[2][0].Data != "dl:1" || first.kb[3][0].Data != "m:main" {
		t.Errorf("first page buttons = %+v", first.kb)
	}
	if first.kb[0][0].Text != "1" || !strings.HasPrefix(first.kb[0][0].Data, "dv:") {
		t.Errorf("number button = %+v", first.kb[0][0])
	}

	second := r.bot.devicesView(ctx, 1)
	if !strings.Contains(second.text, " 9 dev") || !strings.Contains(second.text, "10 dev") || strings.Contains(second.text, " 8 dev") {
		t.Errorf("second page:\n%s", second.text)
	}
	if fmt.Sprint(data(second.kb)) != fmt.Sprint([]string{second.kb[0][0].Data, second.kb[0][1].Data, "dl:0", "m:main"}) {
		t.Errorf("second page buttons = %v", data(second.kb))
	}
	if again := r.bot.devicesView(ctx, 7); again.text != second.text {
		t.Error("a page past the end did not show the last one")
	}
}

func TestDevicesNotSeenForAWeekAreFoldedAway(t *testing.T) {
	r := devicesRig(t, 10)
	ctx := context.Background()
	r.now = r.now.Add(8 * 24 * time.Hour) // none of them has connected since

	v := r.bot.devicesView(ctx, 0)
	if v.text != "Устройства: 0 в сети из 10." || fmt.Sprint(data(v.kb)) != "[ds:0 m:main]" {
		t.Errorf("all stale: %q %v", v.text, data(v.kb))
	}
	if v.kb[0][0].Text != "Давно не заходили · 10" {
		t.Errorf("fold button = %q", v.kb[0][0].Text)
	}
	old := r.bot.staleView(ctx, 0)
	if !strings.HasPrefix(old.text, "Не заходили больше недели: 10.\n") || !strings.Contains(old.text, " 1 dev") ||
		!strings.Contains(old.text, "не был") {
		t.Errorf("stale page:\n%s", old.text)
	}
	if d := data(old.kb); d[len(d)-2] != "ds:1" || d[len(d)-1] != "dl:0" {
		t.Errorf("stale buttons = %v", d)
	}
	if more := r.bot.staleView(ctx, 1); !strings.Contains(more.text, " 9 dev") {
		t.Errorf("stale page 2 numbers go on:\n%s", more.text)
	}
}

func TestOnlineDevicesComeFirstThenTheRecentlySeen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dev := func(name, code string, online bool, seen time.Duration) node.DeviceView {
		d := node.DeviceView{Device: enroll.Device{ID: code, Name: name, EnrolledAt: now.Add(-30 * 24 * time.Hour).Unix()}, Code: code, Online: online}
		if seen > 0 {
			d.LastHandshake = now.Add(-seen).Unix()
		}
		return d
	}
	active, old := ordered([]node.DeviceView{
		dev("pc", "1", false, 12*time.Hour),
		dev("windows", "2", true, time.Minute),
		dev("old", "3", false, 9*24*time.Hour),
		dev("android", "4", true, time.Minute),
		dev("tv", "5", false, 6*time.Minute),
	}, now)
	var names []string
	for _, d := range active {
		names = append(names, d.Name+" "+status(d, now))
	}
	if strings.Join(names, ", ") != "android в сети, windows в сети, tv 6 мин, pc 12 ч" || len(old) != 1 || old[0].Name != "old" {
		t.Errorf("active = %v, old = %d", names, len(old))
	}
	if ago(now.Add(-50*time.Hour), now) != "2 дн" || ago(time.Time{}, now) != "не был" || ago(now.Add(-20*time.Second), now) != "только что" {
		t.Error("ago")
	}
}

func TestADeviceIsRenamedByReplyingToTheQuestion(t *testing.T) {
	r := devicesRig(t, 1)
	ctx := context.Background()
	r.bot.handle(ctx, press(owner, "nm:"+idOf(1)))
	r.api.mu.Lock()
	asks := append([]sent(nil), r.api.asks...)
	r.api.mu.Unlock()
	if len(asks) != 1 || !strings.HasPrefix(asks[0].text, "Как назвать dev01?") {
		t.Fatalf("asks = %+v", asks)
	}

	reply := func(id int64, text string) telegram.Update {
		u := message(owner, "private", text)
		u.Message.ReplyTo = &telegram.Message{MessageID: id}
		return u
	}
	r.bot.handle(ctx, reply(501, "  Ноутбук Пети\uE000 "))
	sends, _, _ := r.api.snapshot()
	if len(sends) != 1 || !strings.HasPrefix(sends[0].text, "Ноутбук Пети\n") || !strings.Contains(sends[0].text, "В приложении dev01") {
		t.Fatalf("after the reply: %+v", sends)
	}
	if d, _ := r.srv.Registry.Lookup(idOf(1)); d.Label != "Ноутбук Пети" {
		t.Errorf("label = %q", d.Label)
	}

	// The same question cannot be answered twice; any other text is a command.
	r.bot.handle(ctx, reply(501, "Другое"))
	if d, _ := r.srv.Registry.Lookup(idOf(1)); d.Label != "Ноутбук Пети" {
		t.Error("an old question was answered again")
	}

	r.bot.handle(ctx, press(owner, "nm:"+idOf(1)))
	r.bot.handle(ctx, reply(502, strings.Repeat("я", 40)))
	sends, _, _ = r.api.snapshot()
	if last := sends[len(sends)-1].text; !strings.Contains(last, "не длиннее 32") {
		t.Errorf("too long: %q", last)
	}
	r.bot.handle(ctx, press(owner, "nm:"+idOf(1)))
	r.bot.handle(ctx, reply(503, "-"))
	if d, _ := r.srv.Registry.Lookup(idOf(1)); d.Label != "" {
		t.Errorf("«-» did not go back to the app's name: %q", d.Label)
	}
}

func TestTheDeviceCardHasItsActions(t *testing.T) {
	r := devicesRig(t, 1)
	v := r.bot.deviceView(context.Background(), idOf(1))
	if !strings.HasPrefix(v.text, "dev01\n") || !strings.Contains(v.text, "Был          ни разу") {
		t.Errorf("card:\n%s", v.text)
	}
	if fmt.Sprint(data(v.kb)) != fmt.Sprint([]string{"nm:" + idOf(1), "rv:" + idOf(1), "m:devices"}) {
		t.Errorf("card buttons = %v", data(v.kb))
	}
	if gone := r.bot.deviceView(context.Background(), idOf(9)); !strings.Contains(gone.text, "уже нет") {
		t.Errorf("unknown device: %q", gone.text)
	}
}

func TestTheMainScreenShowsOnlyWhatThereIs(t *testing.T) {
	r := devicesRig(t, 2)
	ctx := context.Background()
	v := r.bot.mainView(ctx)
	if strings.Contains(v.text, "Трафик") || strings.Contains(v.text, "Оплачен") || strings.Contains(fmt.Sprint(v.kb), "Заявки") {
		t.Errorf("hourly server, nothing waiting:\n%s %+v", v.text, v.kb)
	}
	r.mgr.set(func(s *lifecycle.Snapshot) {
		s.Account = &cloud.Account{TrafficUsed: 426e9, TrafficLimit: 1e12}
		s.PaidUntil = r.now.Add(10 * 24 * time.Hour)
		s.Moving = true
	})
	v = r.bot.mainView(ctx)
	for _, want := range []string{"идёт переезд", "Трафик     426 из 1000 ГБ", "Оплачен    до "} {
		if !strings.Contains(v.text, want) {
			t.Errorf("main lacks %q:\n%s", want, v.text)
		}
	}
}

func TestTheServerScreenNamesTheCertificateAndAFailedMoveOnlyWhenItMatters(t *testing.T) {
	r := newRig(t)
	r.mgr.set(func(s *lifecycle.Snapshot) { s.CertDaysLeft = 200 })
	if strings.Contains(r.bot.serverView().text, "Сертификат") {
		t.Error("a certificate far from expiring is shown")
	}
	r.mgr.set(func(s *lifecycle.Snapshot) {
		s.CertDaysLeft = 12
		s.LastMove = &lifecycle.MoveSummary{At: r.now.Add(-time.Hour).Unix(), State: lifecycle.StateFailed, Error: "no capacity at 203.0.113.1"}
	})
	v := r.bot.serverView()
	if !strings.Contains(v.text, "Сертификат ещё 12 дн") || !strings.Contains(v.text, "не удался: no capacity") {
		t.Errorf("server:\n%s", v.text)
	}
	if fmt.Sprint(data(v.kb)) != "[mr rs m:settings m:main]" {
		t.Errorf("server buttons = %v", data(v.kb))
	}
	if region := r.bot.regionView(); region.kb[0][0].Data != "mv" || region.kb[len(region.kb)-1][0].Data != "m:server" {
		t.Errorf("move choices = %v", data(region.kb))
	}
}

func TestSettingsShowTheSwitchesAndFlipThem(t *testing.T) {
	r := newRig(t)
	r.mgr.set(func(s *lifecycle.Snapshot) { s.NextAutoMove = r.now.Add(5 * 24 * time.Hour) })
	v := r.bot.settingsView()
	for _, want := range []string{"Автопереезд    вкл", "Следующий      около ", "При блокировке выкл", "WARP           выкл"} {
		if !strings.Contains(v.text, want) {
			t.Errorf("settings lack %q:\n%s", want, v.text)
		}
	}
	if fmt.Sprint(data(v.kb)) != "[pa:off pb:on px:on ck m:server]" {
		t.Errorf("settings buttons = %v", data(v.kb))
	}
}
