package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/telegram"
	"awgrotator/internal/usage"
)

// trafficRig is the rig with two approved devices, both called "windows",
// and a phone, and a traffic history behind the node.
func trafficRig(t *testing.T) (*rig, *usage.Store) {
	r := newRig(t)
	r.bot.Zone = tomsk
	for i, name := range []string{"windows", "windows", "phone"} {
		b := byte(i + 1)
		r.ask(t, b, 0x20+b, name)
		if _, err := r.srv.Approve(idOf(b), r.now); err != nil {
			t.Fatal(err)
		}
	}
	store, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.bot.Devices = direct{&node.Controller{Srv: r.srv, Now: func() time.Time { return r.now }, Usage: store}}
	return r, store
}

func TestTheTrafficScreenListsTheLargestSitesAndDevices(t *testing.T) {
	r, store := trafficRig(t)
	ctx := context.Background()
	if v := r.bot.trafficView(ctx, usage.Day); !strings.HasPrefix(v.text, "Трафик за сутки") || !strings.HasSuffix(v.text, ": пока ничего.") {
		t.Errorf("empty: %q", v.text)
	}

	at := r.now.Add(-30 * time.Minute)
	recs := []usage.Record{
		{Device: idOf(1), Site: "YouTube", Bytes: usage.Bytes{Down: 120e9}},
		{Device: idOf(1), Site: "Steam", Bytes: usage.Bytes{Down: 40e9, Up: 2e9}},
		{Device: idOf(2), Site: "Twitch", Bytes: usage.Bytes{Down: 12e9}},
		{Device: idOf(3), Site: usage.SiteTelegram, Bytes: usage.Bytes{Down: 6e9}},
		{Device: idOf(3), Site: "example.com", Bytes: usage.Bytes{Down: 1.5e9}},
		{Device: idOf(3), Site: usage.SiteUnknown, Bytes: usage.Bytes{Down: 700e6}},
		{Device: idOf(3), Site: usage.SiteServer, Bytes: usage.Bytes{Up: 300e3}},
		{Device: "gone", Site: "YouTube", Bytes: usage.Bytes{Down: 1e9}},
	}
	store.Add(at, recs)
	store.AddServer(at, usage.Bytes{Down: 190e9, Up: 192e9})

	v := r.bot.trafficView(ctx, usage.Day)
	line := func(name, amount string) string {
		return strings.TrimRight(fmt.Sprintf("%-18s %7s", name, amount), " ")
	}
	want := "Трафик за сутки (учёт с " + time.Unix(at.UTC().Truncate(time.Hour).Unix(), 0).In(tomsk).Format("02.01 15:04") + "): 183 ГБ.\n" +
		telegram.Mono(strings.Join([]string{
			line("YouTube", "121 ГБ"),
			line("Steam", "42 ГБ"),
			line("Twitch", "12 ГБ"),
			line("Telegram", "6,0 ГБ"),
			line("example.com", "1,5 ГБ"),
			line("Остальное", "700 МБ"),
			"",
			line("windows · "+r.bot.mustCode(t, 1), "162 ГБ"),
			line("windows · "+r.bot.mustCode(t, 2), "12 ГБ"),
			line("phone", "8,2 ГБ"),
			line("удалённое", "1,0 ГБ"),
			"",
			line("Сервер отправил", "192 ГБ"),
		}, "\n"))
	if v.text != want {
		t.Errorf("traffic:\n%s\nwant\n%s", v.text, want)
	}
	if len(v.kb) != 2 || v.kb[0][1].Data != "tr:w" || v.kb[1][0].Data != "m:main" {
		t.Errorf("buttons = %+v", v.kb)
	}

	card := r.bot.deviceView(ctx, idOf(1))
	for _, want := range []string{"windows\n", "Код          " + r.bot.mustCode(t, 1), "Сутки        162 ГБ", "Месяц        162 ГБ",
		line("YouTube", "120 ГБ")[:0] + "YouTube           120 ГБ", "Steam              42 ГБ"} {
		if !strings.Contains(card.text, want) {
			t.Errorf("card lacks %q:\n%s", want, card.text)
		}
	}
	for _, row := range append(v.kb, card.kb...) {
		for _, b := range row {
			if len(b.Data) > 64 || b.Data == "" {
				t.Errorf("button %q data %q", b.Text, b.Data)
			}
		}
	}
}

func (b *Bot) mustCode(t *testing.T, id byte) string {
	t.Helper()
	resp, err := b.Devices.Control(context.Background(), node.ControlRequest{Op: node.OpOverview})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Overview.Devices {
		if d.ID == idOf(id) {
			return d.Code
		}
	}
	t.Fatalf("no device %d", id)
	return ""
}

func TestTrafficButtonsRedrawTheScreenForThePeriod(t *testing.T) {
	r, store := trafficRig(t)
	store.Add(r.now.AddDate(0, 0, -3), []usage.Record{{Device: idOf(3), Site: "YouTube", Bytes: usage.Bytes{Down: 5e9}}})
	ctx := context.Background()
	r.bot.handle(ctx, press(owner, "tr:w"))
	r.bot.handle(ctx, press(owner, "td:m:"+idOf(3)))
	r.bot.handle(ctx, message(owner, "private", "/traffic"))
	_, edits, _ := r.api.snapshot()
	if len(edits) != 2 || !strings.HasPrefix(edits[0].text, "Трафик за неделю") || !strings.Contains(edits[0].text, "YouTube") {
		t.Fatalf("edits = %+v", edits)
	}
	if !strings.HasPrefix(edits[1].text, "phone\n") {
		t.Errorf("an old \"Куда\" button does not open the card: %q", edits[1].text)
	}
	if sends, _, _ := r.api.snapshot(); len(sends) != 1 || !strings.HasPrefix(sends[0].text, "Трафик за сутки") {
		t.Errorf("/traffic: %+v", sends)
	}
	if m := r.bot.mainView(ctx); m.kb[0][1].Data != "tr:d" {
		t.Errorf("the main screen has no traffic button: %+v", m.kb)
	}
}

func TestADeviceThatUsesALotIsNamedWithItsShareAndAButton(t *testing.T) {
	r := newRig(t)
	v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventHeavy, Heavy: &lifecycle.HeavyUsage{
		ID: idOf(1), Name: "DESKTOP-UE0P2DK", Code: "0235", Bytes: 184e9, Total: 192e9,
	}})
	if v.text != "DESKTOP-UE0P2DK (код 0235) за сутки: 184 ГБ, 96% всего трафика." {
		t.Errorf("text = %q", v.text)
	}
	if len(v.kb) != 1 || v.kb[0][0].Data != "dv:"+idOf(1) || len(v.kb[0][0].Data) > 64 {
		t.Errorf("buttons = %+v", v.kb)
	}
}

func TestAmountsAreShortAndDecimal(t *testing.T) {
	for n, want := range map[uint64]string{
		0: "0", 500e3: "меньше 1 МБ", 700e6: "700 МБ", 1.5e9: "1,5 ГБ", 9.96e9: "10,0 ГБ", 10e9: "10 ГБ", 184.4e9: "184 ГБ",
	} {
		if got := amount(n); got != want {
			t.Errorf("amount(%d) = %q, want %q", n, got, want)
		}
	}
	if siteName(usage.SiteUnknown) != "Неизвестно" || siteName("example.com") != "example.com" || siteName("YouTube") != "YouTube" {
		t.Error("site names")
	}
}
