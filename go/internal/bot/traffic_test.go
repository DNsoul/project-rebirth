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
	"awgrotator/internal/splitdns"
	"awgrotator/internal/telegram"
	"awgrotator/internal/usage"
)

// trafficRig is the rig with two approved devices, both called "windows",
// and a phone, a traffic history behind the node, and an empty list of sites
// sent direct.
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
	list, err := splitdns.OpenDirectList(filepath.Join(t.TempDir(), "direct.txt"))
	if err != nil {
		t.Fatal(err)
	}
	trackers, _ := splitdns.NewRules("mc.yandex.ru")
	r.bot.Devices = direct{&node.Controller{
		Srv: r.srv, Now: func() time.Time { return r.now }, Usage: store,
		Direct: node.NewDirectRouting(list, &splitdns.Decider{ForceTunnel: trackers}, nil),
	}}
	return r, store
}

// someTraffic is half an hour ago: the three devices on a few sites, and a
// removed device.
func someTraffic(r *rig, store *usage.Store) time.Time {
	at := r.now.Add(-30 * time.Minute)
	store.Add(at, []usage.Record{
		{Device: idOf(1), Site: "googlevideo.com", Bytes: usage.Bytes{Down: 120e9}},
		{Device: idOf(1), Site: "steamcontent.com", Bytes: usage.Bytes{Down: 40e9, Up: 2e9}},
		{Device: idOf(2), Site: "ttvnw.net", Bytes: usage.Bytes{Down: 12e9}},
		{Device: idOf(3), Site: usage.SiteTelegram, Bytes: usage.Bytes{Down: 6e9}},
		{Device: idOf(3), Site: "example.com", Bytes: usage.Bytes{Down: 1.5e9}},
		{Device: idOf(3), Site: "steamcontent.com", Bytes: usage.Bytes{Down: 1e9}},
		{Device: idOf(3), Site: usage.SiteUnknown, Bytes: usage.Bytes{Down: 700e6}},
		{Device: idOf(3), Site: usage.SiteServer, Bytes: usage.Bytes{Up: 300e3}},
		{Device: "gone", Site: "googlevideo.com", Bytes: usage.Bytes{Down: 1e9}},
	})
	store.AddServer(at, usage.Bytes{Down: 190e9, Up: 192e9})
	return at
}

func row(n, name, amount string) string {
	return strings.TrimRight(fmt.Sprintf("%2s %-19s %7s", n, name, amount), " ")
}

func TestTheTrafficScreenNumbersTheLargestSites(t *testing.T) {
	r, store := trafficRig(t)
	ctx := context.Background()
	if v := r.bot.trafficView(ctx, usage.Day); !strings.HasPrefix(v.text, "Трафик за сутки") || !strings.HasSuffix(v.text, ": пока ничего.") {
		t.Errorf("empty: %q", v.text)
	}

	at := someTraffic(r, store)
	v := r.bot.trafficView(ctx, usage.Day)
	want := "Трафик за сутки (учёт с " + time.Unix(at.UTC().Truncate(time.Hour).Unix(), 0).In(tomsk).Format("02.01 15:04") + "): 184 ГБ.\n" +
		telegram.Mono(strings.Join([]string{
			row("1", "googlevideo.com", "121 ГБ"),
			row("2", "steamcontent.com", "43 ГБ"),
			row("3", "ttvnw.net", "12 ГБ"),
			row("4", "Telegram", "6,0 ГБ"),
			row("5", "example.com", "1,5 ГБ"),
			row("6", "Неизвестно", "700 МБ"),
			row("", "Остальное", "меньше 1 МБ"),
			"",
			row("", "Сервер отправил", "192 ГБ"),
		}, "\n"))
	if v.text != want {
		t.Errorf("traffic:\n%s\nwant\n%s", v.text, want)
	}
	if len(v.kb) != 5 || v.kb[0][1].Data != "tr:w" || len(v.kb[1]) != 5 || v.kb[1][1].Data != "ts:d:steamcontent.com" ||
		len(v.kb[2]) != 1 || v.kb[2][0].Text != "6" || v.kb[3][0].Data != "tv:d" || v.kb[3][1].Text != "Напрямую" ||
		v.kb[3][1].Data != "dp:0" || v.kb[4][0].Data != "m:main" || v.kb[4][1].Data != "ux" {
		t.Errorf("buttons = %+v", v.kb)
	}

	card := r.bot.deviceView(ctx, idOf(1))
	for _, want := range []string{"windows\n", "Код          " + r.bot.mustCode(t, 1), "Сутки        162 ГБ", "Месяц        162 ГБ",
		"googlevideo.com      120 ГБ", "steamcontent.com      42 ГБ"} {
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

func TestTheDevicesOfThePeriodAreOneButtonAway(t *testing.T) {
	r, store := trafficRig(t)
	someTraffic(r, store)
	ctx := context.Background()
	r.bot.handle(ctx, press(owner, "tv:d"))
	_, edits, _ := r.api.snapshot()
	if len(edits) != 1 {
		t.Fatalf("edits = %+v", edits)
	}
	v := edits[0]
	want := telegram.Mono(strings.Join([]string{
		row("1", "windows · "+r.bot.mustCode(t, 1), "162 ГБ"),
		row("2", "windows · "+r.bot.mustCode(t, 2), "12 ГБ"),
		row("3", "phone", "9,2 ГБ"),
		row("", "Остальные", "1,0 ГБ"),
	}, "\n"))
	if !strings.HasPrefix(v.text, "Устройства за сутки") || !strings.HasSuffix(v.text, want) {
		t.Errorf("devices:\n%s\nwant the block\n%s", v.text, want)
	}
	if len(v.kb) != 3 || v.kb[0][2].Data != "tv:m" || v.kb[1][2].Data != "dv:"+idOf(3) || v.kb[2][0].Data != "tr:d" {
		t.Errorf("buttons = %+v", v.kb)
	}
}

func TestASiteFromTheListIsSentDirectAndBack(t *testing.T) {
	r, store := trafficRig(t)
	someTraffic(r, store)
	ctx := context.Background()

	r.bot.handle(ctx, press(owner, "ts:w:steamcontent.com"))
	_, edits, _ := r.api.snapshot()
	card := edits[len(edits)-1]
	want := "steamcontent.com · Steam\nЗа неделю: 43 ГБ. Больше всего: windows · " + r.bot.mustCode(t, 1) + ".\n" +
		"Если сайт закрыт в России, напрямую он не откроется ни у кого."
	if card.text != want {
		t.Errorf("card:\n%s\nwant\n%s", card.text, want)
	}
	if len(card.kb) != 2 || card.kb[0][0].Data != "da:w:steamcontent.com" || card.kb[1][0].Data != "tr:w" {
		t.Fatalf("buttons = %+v", card.kb)
	}

	r.bot.handle(ctx, press(owner, card.kb[0][0].Data))
	_, edits, answers := r.api.snapshot()
	card = edits[len(edits)-1]
	if answers[len(answers)-1] != switchedNotice || !strings.HasSuffix(card.text, "Идёт напрямую, мимо сервера.") ||
		card.kb[0][0].Data != "dr:w:steamcontent.com" {
		t.Fatalf("after sending direct: %q, %+v, notice %q", card.text, card.kb, answers[len(answers)-1])
	}
	if names, _ := r.bot.directList(ctx); len(names) != 1 || names[0] != "steamcontent.com" {
		t.Errorf("list = %v", names)
	}

	list := r.bot.trafficView(ctx, usage.Week)
	for _, want := range []string{" 1  googlevideo.com      121 ГБ\n 2→ steamcontent.com      43 ГБ\n",
		"\n    Остальное ", "\n    Сервер отправил      192 ГБ"} {
		if !strings.Contains(list.text, want) {
			t.Errorf("the traffic list lacks %q:\n%s", want, list.text)
		}
	}
	if !strings.HasSuffix(list.text, "\n→ напрямую, мимо сервера") || list.kb[len(list.kb)-2][1].Text != "Напрямую · 1" {
		t.Errorf("the traffic list does not show it: %q, %+v", list.text, list.kb)
	}
	devCard := r.bot.deviceView(ctx, idOf(1))
	if !strings.Contains(devCard.text, "\n  googlevideo.com      120 ГБ\n→ steamcontent.com      42 ГБ") ||
		!strings.HasSuffix(devCard.text, "\n→ напрямую, мимо сервера") {
		t.Errorf("the device card does not mark it:\n%s", devCard.text)
	}
	if other := r.bot.deviceView(ctx, idOf(2)); strings.Contains(other.text, "напрямую") || !strings.Contains(other.text, "\nttvnw.net") {
		t.Errorf("a card with nothing direct has a mark:\n%s", other.text)
	}

	r.bot.handle(ctx, press(owner, "dr:w:steamcontent.com"))
	_, edits, answers = r.api.snapshot()
	if answers[len(answers)-1] != "steamcontent.com снова идёт через сервер." || edits[len(edits)-1].kb[0][0].Data != "da:w:steamcontent.com" {
		t.Errorf("after taking back: %+v, notice %q", edits[len(edits)-1], answers[len(answers)-1])
	}
}

func TestALineThatIsNotASiteHasNoButton(t *testing.T) {
	r, store := trafficRig(t)
	someTraffic(r, store)
	ctx := context.Background()
	for site, why := range map[string]string{
		usage.SiteTelegram: "по адресам, а не по именам",
		usage.SiteUnknown:  "сервер не знает",
		"co.uk":            "нельзя пустить напрямую",
	} {
		v := r.bot.siteView(ctx, usage.Day, site)
		if !strings.Contains(v.text, why) || len(v.kb) != 1 || v.kb[0][0].Data != "tr:d" {
			t.Errorf("%s: %q, %+v", site, v.text, v.kb)
		}
	}
}

func TestTheListOfDirectSitesIsEditedFromTheBot(t *testing.T) {
	r, _ := trafficRig(t)
	ctx := context.Background()
	if v := r.bot.directView(ctx, 0); v.text != "Напрямую пока ничего не идёт." || v.kb[0][0].Data != "dq" || v.kb[1][0].Data != "tr:d" {
		t.Errorf("empty list: %q, %+v", v.text, v.kb)
	}

	reply := func(id int64, text string) telegram.Update {
		u := message(owner, "private", text)
		u.Message.ReplyTo = &telegram.Message{MessageID: id}
		return u
	}
	r.bot.handle(ctx, press(owner, "dq"))
	r.bot.handle(ctx, reply(501, " https://Store.SteamPowered.com:443/app/570/ "))
	sends, _, _ := r.api.snapshot()
	want := "store.steampowered.com идёт напрямую. " + switchedNotice + "\n\nНапрямую, мимо сервера: 1.\n" + telegram.Mono(" 1 store.steampowered.com")
	if len(sends) != 1 || sends[0].text != want {
		t.Fatalf("after the reply: %+v\nwant %q", sends, want)
	}

	for i, c := range []struct{ text, want string }{
		{"com", "Слишком широкое имя"},
		{"mc.yandex.ru", "всегда идёт через сервер"},
		{"не сайт", "не похоже на имя сайта"},
	} {
		r.bot.handle(ctx, press(owner, "dq"))
		r.bot.handle(ctx, reply(int64(502+i), c.text))
		sends, _, _ = r.api.snapshot()
		if last := sends[len(sends)-1].text; !strings.Contains(last, c.want) {
			t.Errorf("%q: %q", c.text, last)
		}
	}
	// An answered question is not answered twice; the text is a command then.
	r.bot.handle(ctx, reply(501, "steamcontent.com"))
	if names, _ := r.bot.directList(ctx); len(names) != 1 {
		t.Errorf("an old question was answered again: %v", names)
	}

	r.bot.handle(ctx, press(owner, "dd:store.steampowered.com"))
	_, edits, _ := r.api.snapshot()
	item := edits[len(edits)-1]
	if item.text != "store.steampowered.com · Steam\nИдёт напрямую, мимо сервера." || item.kb[0][0].Data != "dx:store.steampowered.com" {
		t.Errorf("item: %q, %+v", item.text, item.kb)
	}
	r.bot.handle(ctx, press(owner, item.kb[0][0].Data))
	_, edits, answers := r.api.snapshot()
	if edits[len(edits)-1].text != "Напрямую пока ничего не идёт." || answers[len(answers)-1] != "store.steampowered.com снова идёт через сервер." {
		t.Errorf("after removal: %q, %q", edits[len(edits)-1].text, answers[len(answers)-1])
	}
}

func TestALongListIsPaged(t *testing.T) {
	r, _ := trafficRig(t)
	ctx := context.Background()
	for i := range 12 {
		if msg := r.bot.sendDirect(ctx, fmt.Sprintf("site%02d.com", i+1)); msg != "" {
			t.Fatal(msg)
		}
	}
	first := r.bot.directView(ctx, 0)
	if !strings.HasPrefix(first.text, "Напрямую, мимо сервера: 12.") || len(first.kb) != 5 || first.kb[2][0].Data != "dp:1" {
		t.Errorf("first page: %q, %+v", first.text, first.kb)
	}
	second := r.bot.directView(ctx, 1)
	if !strings.Contains(second.text, "\n12 site12.com") || second.kb[0][0].Data != "dd:site11.com" || second.kb[1][0].Data != "dp:0" {
		t.Errorf("second page: %q, %+v", second.text, second.kb)
	}
}

func TestALongNameTravelsAsAKey(t *testing.T) {
	r, _ := trafficRig(t)
	long := strings.Repeat("a", 60) + ".example.com"
	data := r.bot.siteRef("ts:d:", long)
	if len(data) > 64 || !strings.HasPrefix(data, "ts:d:~") {
		t.Fatalf("data = %q", data)
	}
	if site, ok := r.bot.siteOf(strings.TrimPrefix(data, "ts:d:")); !ok || site != long {
		t.Errorf("siteOf = %q, %v", site, ok)
	}
	r.bot.handle(context.Background(), press(owner, "dd:~0123456789abcdef"))
	if _, _, answers := r.api.snapshot(); answers[len(answers)-1] != "Список устарел, откройте его заново." {
		t.Errorf("a forgotten key: %q", answers)
	}
}

func TestANameIsTakenOutOfAPastedAddress(t *testing.T) {
	for in, want := range map[string]string{
		"steamcontent.com":                      "steamcontent.com",
		" https://store.steampowered.com/app/1": "store.steampowered.com",
		"http://user@host.example.com:8080/x":   "host.example.com",
		"example.com:443":                       "example.com",
		"*.riotcdn.net":                         "*.riotcdn.net",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
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
	store.Add(r.now.AddDate(0, 0, -3), []usage.Record{{Device: idOf(3), Site: "googlevideo.com", Bytes: usage.Bytes{Down: 5e9}}})
	ctx := context.Background()
	r.bot.handle(ctx, press(owner, "tr:w"))
	r.bot.handle(ctx, press(owner, "td:m:"+idOf(3)))
	r.bot.handle(ctx, message(owner, "private", "/traffic"))
	_, edits, _ := r.api.snapshot()
	if len(edits) != 2 || !strings.HasPrefix(edits[0].text, "Трафик за неделю") || !strings.Contains(edits[0].text, "googlevideo.com") {
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

func TestTheTrafficHistoryIsResetAfterAConfirmation(t *testing.T) {
	r, store := trafficRig(t)
	someTraffic(r, store)
	ctx := context.Background()

	r.bot.handle(ctx, press(owner, "ux"))
	_, edits, _ := r.api.snapshot()
	ask := edits[len(edits)-1]
	if !strings.HasPrefix(ask.text, "Стереть учёт трафика?") || ask.kb[0][0].Data != "ux!" ||
		ask.kb[0][0].Style != telegram.StyleDanger || ask.kb[0][1].Data != "tr:d" {
		t.Fatalf("confirmation: %q, %+v", ask.text, ask.kb)
	}
	if v := r.bot.trafficView(ctx, usage.Day); strings.Contains(v.text, "пока ничего") {
		t.Fatal("the confirmation alone reset the history")
	}

	r.mgr.snap.Moving = true
	r.bot.handle(ctx, press(owner, "ux!"))
	if _, _, answers := r.api.snapshot(); !strings.HasPrefix(answers[len(answers)-1], "Идёт переезд") {
		t.Errorf("during a move: %q", answers[len(answers)-1])
	}
	r.mgr.snap.Moving = false

	r.bot.handle(ctx, press(owner, "ux!"))
	_, edits, answers := r.api.snapshot()
	if answers[len(answers)-1] != "Учёт трафика начат заново." || edits[len(edits)-1].text != "Трафик за сутки: пока ничего." {
		t.Errorf("after reset: %q, notice %q", edits[len(edits)-1].text, answers[len(answers)-1])
	}
	if card := r.bot.deviceView(ctx, idOf(1)); !strings.Contains(card.text, "Сутки        0") {
		t.Errorf("the device card still counts:\n%s", card.text)
	}
}
