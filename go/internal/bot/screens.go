package bot

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"awgrotator/internal/egress"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/telegram"
	"awgrotator/internal/usage"
)

// The screens. Each is a line or two of words and, where there is a list or a
// set of values, a monospace block whose columns line up. Buttons are only
// the ones that are needed; a list's entries are numbered and opened by
// their number.

// devicesPerPage is how many devices one page of the list shows.
const devicesPerPage = 8

// staleAfter is how long a device may go unseen before it is folded away
// under "Давно не заходили".
const staleAfter = 7 * 24 * time.Hour

// columns lays rows out in a fixed-width font: a positive width pads the
// value on the right and cuts it with "…" when longer, a negative one
// right-aligns it. The last column is not padded.
func columns(rows [][]string, widths ...int) string {
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		var line strings.Builder
		for i, v := range r {
			if i >= len(widths) {
				break
			}
			w := widths[i]
			if i > 0 {
				line.WriteByte(' ')
			}
			n := utf8.RuneCountInString(v)
			switch {
			case w < 0:
				line.WriteString(strings.Repeat(" ", max(0, -w-n)) + v)
			case n > w:
				line.WriteString(string([]rune(v)[:w-1]) + "…")
			case i == len(r)-1:
				line.WriteString(v)
			default:
				line.WriteString(v + strings.Repeat(" ", w-n))
			}
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	return strings.Join(lines, "\n")
}

// shownName is a device's name as every screen shows it: the operator's own,
// or else the app's, with nothing in it that could break a message.
func shownName(d node.DeviceView) string { return deviceName(d.Shown()) }

// lastSeen is when the device was last heard from, zero if never.
func lastSeen(d node.DeviceView) time.Time {
	t := d.LastHandshake
	if d.LastSeen > d.EnrolledAt && d.LastSeen > t {
		t = d.LastSeen
	}
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0)
}

// ago is how long ago, as short as it can be said: "6 мин", "35 ч", "12 дн".
func ago(t, now time.Time) string {
	if t.IsZero() {
		return "не был"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч", int(d/time.Hour))
	}
	return fmt.Sprintf("%d дн", int(d/(24*time.Hour)))
}

func status(d node.DeviceView, now time.Time) string {
	if d.Online {
		return "в сети"
	}
	return ago(lastSeen(d), now)
}

func stale(d node.DeviceView, now time.Time) bool {
	if d.Online {
		return false
	}
	t := lastSeen(d)
	if t.IsZero() {
		t = time.Unix(d.EnrolledAt, 0)
	}
	return now.Sub(t) > staleAfter
}

// ordered splits the devices into those in use, online ones first and then by
// when they were last seen, and those not seen for a long while.
func ordered(devices []node.DeviceView, now time.Time) (active, old []node.DeviceView) {
	for _, d := range devices {
		if stale(d, now) {
			old = append(old, d)
		} else {
			active = append(active, d)
		}
	}
	byUse := func(a, b node.DeviceView) int {
		if a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		if a.Online {
			return cmp.Or(cmp.Compare(shownName(a), shownName(b)), cmp.Compare(a.Code, b.Code))
		}
		return cmp.Or(lastSeen(b).Compare(lastSeen(a)), cmp.Compare(a.Code, b.Code))
	}
	slices.SortFunc(active, byUse)
	slices.SortFunc(old, byUse)
	return active, old
}

// numberRows is the buttons that open the listed devices, four to a row.
func numberRows(first int, devices []node.DeviceView) telegram.Keyboard {
	perRow := 4
	if len(devices) == 5 {
		perRow = 5 // one row of five rather than four and one
	}
	var kb telegram.Keyboard
	var row []telegram.Button
	for i, d := range devices {
		row = append(row, btn(strconv.Itoa(first+i), "dv:"+d.ID))
		if len(row) == perRow {
			kb = append(kb, row)
			row = nil
		}
	}
	if len(row) > 0 {
		kb = append(kb, row)
	}
	return kb
}

func deviceRows(first int, devices []node.DeviceView, now time.Time, when func(node.DeviceView, time.Time) string) string {
	rows := make([][]string, 0, len(devices))
	for i, d := range devices {
		rows = append(rows, []string{strconv.Itoa(first + i), shownName(d), d.Code, when(d, now)})
	}
	return columns(rows, -2, 16, 4, 6)
}

// ---- the main screen

// mainView is what /start and "« Меню" show: how things stand, in a few
// lines, and the way to everything else.
func (b *Bot) mainView(ctx context.Context) view {
	s := b.Manager.Snapshot()
	now := b.now()
	var rows [][]string

	state := healthWords(s.Health)
	if s.Moving {
		state = "идёт переезд"
	}
	rows = append(rows, []string{regionName(s.Region), state + ", " + left(now.Sub(s.CreatedAt))})

	waiting := 0
	if o, err := b.overview(ctx); err == nil {
		online := 0
		for _, d := range o.Devices {
			if d.Online {
				online++
			}
		}
		rows = append(rows, []string{"В сети", fmt.Sprintf("%d из %d", online, len(o.Devices))})
		if n := len(o.Pending); n > 0 {
			rows = append(rows, []string{"Заявки", fmt.Sprintf("%d ждут", n)})
		}
		waiting = len(o.Pending) + len(o.Denied)
	}
	if a := s.Account; a != nil && a.TrafficLimit > 0 {
		rows = append(rows, []string{"Трафик", usedOf(*a)})
	}
	if !s.PaidUntil.IsZero() {
		rows = append(rows, []string{"Оплачен", "до " + b.day(s.PaidUntil)})
	}

	kb := telegram.Keyboard{{btn("Устройства", "m:devices"), btn("Трафик", "tr:d")}}
	if waiting > 0 {
		kb = append(kb, []telegram.Button{btn("Сервер", "m:server"), btn(fmt.Sprintf("Заявки · %d", waiting), "m:requests")})
	} else {
		kb = append(kb, []telegram.Button{btn("Сервер", "m:server")})
	}
	return view{text: telegram.Mono(columns(rows, 10, 22)), kb: kb}
}

// ---- devices

// devicesView is one page of the devices in use.
func (b *Bot) devicesView(ctx context.Context, page int) view {
	o, err := b.overview(ctx)
	if err != nil {
		return view{text: "Список устройств сейчас не читается: " + firstLine(err.Error()), kb: telegram.Keyboard{backRow}}
	}
	if len(o.Devices) == 0 {
		return view{text: "Устройств с доступом нет.", kb: telegram.Keyboard{backRow}}
	}
	now := b.now()
	active, old := ordered(o.Devices, now)
	online := 0
	for _, d := range o.Devices {
		if d.Online {
			online++
		}
	}
	head := fmt.Sprintf("Устройства: %d в сети из %d.", online, len(o.Devices))

	pages := max(1, (len(active)+devicesPerPage-1)/devicesPerPage)
	page = min(max(page, 0), pages-1)
	from := page * devicesPerPage
	shown := active[from:min(from+devicesPerPage, len(active))]

	kb := numberRows(from+1, shown)
	var nav []telegram.Button
	if page > 0 {
		nav = append(nav, btn("‹", "dl:"+strconv.Itoa(page-1)))
	}
	if page < pages-1 {
		nav = append(nav, btn("›", "dl:"+strconv.Itoa(page+1)))
	} else if len(old) > 0 {
		nav = append(nav, btn(fmt.Sprintf("Давно не заходили · %d", len(old)), "ds:0"))
	}
	if len(nav) > 0 {
		kb = append(kb, nav)
	}
	kb = append(kb, backRow)

	if len(shown) == 0 {
		return view{text: head, kb: kb}
	}
	return view{text: head + "\n" + telegram.Mono(deviceRows(from+1, shown, now, status)), kb: kb}
}

// staleView is one page of the devices nobody has used for over a week. Their
// numbers go on from the list of those in use.
func (b *Bot) staleView(ctx context.Context, page int) view {
	o, err := b.overview(ctx)
	if err != nil {
		return view{text: "Список устройств сейчас не читается: " + firstLine(err.Error()), kb: telegram.Keyboard{backRow}}
	}
	now := b.now()
	active, old := ordered(o.Devices, now)
	back := []telegram.Button{btn("‹ Устройства", "dl:"+strconv.Itoa(max(0, (len(active)-1)/devicesPerPage)))}
	if len(old) == 0 {
		return view{text: "Все устройства заходили за последнюю неделю.", kb: telegram.Keyboard{back}}
	}
	pages := (len(old) + devicesPerPage - 1) / devicesPerPage
	page = min(max(page, 0), pages-1)
	from := page * devicesPerPage
	shown := old[from:min(from+devicesPerPage, len(old))]
	first := len(active) + from + 1

	kb := numberRows(first, shown)
	var nav []telegram.Button
	if page > 0 {
		nav = append(nav, btn("‹", "ds:"+strconv.Itoa(page-1)))
	}
	if page < pages-1 {
		nav = append(nav, btn("›", "ds:"+strconv.Itoa(page+1)))
	}
	if len(nav) > 0 {
		kb = append(kb, nav)
	}
	kb = append(kb, back)
	daysAgo := func(d node.DeviceView, now time.Time) string {
		t := lastSeen(d)
		if t.IsZero() {
			return "не был"
		}
		return fmt.Sprintf("%d дн", int(now.Sub(t)/(24*time.Hour)))
	}
	head := fmt.Sprintf("Не заходили больше недели: %d.", len(old))
	return view{text: head + "\n" + telegram.Mono(deviceRows(first, shown, now, daysAgo)), kb: kb}
}

// deviceView is one device's card: who it is, when it was last here, how much
// it used and where most of it went today.
func (b *Bot) deviceView(ctx context.Context, id string) view {
	o, err := b.overview(ctx)
	if err != nil {
		return view{text: "Список устройств сейчас не читается: " + firstLine(err.Error()), kb: telegram.Keyboard{backRow}}
	}
	i := slices.IndexFunc(o.Devices, func(d node.DeviceView) bool { return d.ID == id })
	if i < 0 {
		return view{text: "Этого устройства уже нет в списке.", kb: telegram.Keyboard{{btn("‹ Устройства", "m:devices")}}}
	}
	d := o.Devices[i]
	now := b.now()

	rows := [][]string{{"Код", d.Code}}
	if d.Label != "" {
		rows = append(rows, []string{"В приложении", deviceName(d.Name)})
	}
	if d.Online {
		rows = append(rows, []string{"Сейчас", "в сети"})
	} else {
		when := "ни разу"
		if t := lastSeen(d); !t.IsZero() {
			when = ago(t, now) + " назад"
		}
		rows = append(rows, []string{"Был", when})
	}
	block := ""
	if day, err := b.usage(ctx, usage.Day, id); err == nil {
		rows = append(rows, []string{"Сутки", amount(day.Total.Total())})
		if month, err := b.usage(ctx, usage.Month, id); err == nil {
			rows = append(rows, []string{"Месяц", amount(month.Total.Total())})
		}
		var sites [][]string
		for _, it := range day.Sites {
			if len(sites) == 3 || it.Total() == 0 {
				break
			}
			if it.Key == usage.SiteOther {
				continue
			}
			sites = append(sites, []string{siteName(it.Key), amount(it.Total())})
		}
		if len(sites) > 0 {
			block = "\n\n" + columns(sites, 16, -7)
		}
	}
	text := shownName(d) + "\n" + telegram.Mono(columns(rows, 12, 18)+block)
	return view{text: text, kb: telegram.Keyboard{
		{btn("Переименовать", "nm:"+id), btn("Отозвать", "rv:"+id)},
		{btn("‹ Устройства", "m:devices")},
	}}
}

// ---- the server

// serverView is how the server stands, and what can be done with it.
func (b *Bot) serverView() view {
	s := b.Manager.Snapshot()
	now := b.now()
	where := regionName(s.Region) + " · " + providerName(s.Provider)
	if s.Generation > 0 {
		where += fmt.Sprintf(", %d-й", s.Generation)
	}
	rows := [][]string{
		{"Сервер", where},
		{"Работает", left(now.Sub(s.CreatedAt))},
		{"Состояние", healthWords(s.Health)},
	}
	if s.Moving {
		rows = append(rows, []string{"Переезд", "идёт сейчас"})
	}
	if w := s.Watch; w != nil {
		rows = append(rows, []string{"Россия", "сайты " + watchWords(w.Round)})
		if w.Stuck > 0 {
			rows = append(rows, []string{"Не входят", fmt.Sprintf("%d устр.", w.Stuck)})
		}
	}
	if !s.PaidUntil.IsZero() {
		rows = append(rows, []string{"Оплачен", "до " + b.stamp(s.PaidUntil)})
	}
	if a := s.Account; a != nil && !a.NextPayment.IsZero() {
		renew := "само"
		switch {
		case lifecycle.ShortOfMoney(*a):
			renew = "не хватает денег"
		case !a.AutoRenew:
			renew = "вручную"
		}
		rows = append(rows, []string{"Продление", renew})
	}
	if a := s.Account; a != nil && a.TrafficLimit > 0 {
		rows = append(rows, []string{"Трафик", usedOf(*a)})
		if out := b.runsOut(*a, s.CreatedAt); out != "" {
			rows = append(rows, []string{"Хватит до", out})
		}
	}
	rows = append(rows, []string{"Выход", exitShort(s.ExitWant, s.Exit)})
	if s.CertDaysLeft > 0 && s.CertDaysLeft < 30 {
		rows = append(rows, []string{"Сертификат", fmt.Sprintf("ещё %d дн", s.CertDaysLeft)})
	}

	text := telegram.Mono(columns(rows, 10, 23))
	if m := s.LastMove; m != nil && m.Error != "" && now.Sub(time.Unix(m.At, 0)) < 7*24*time.Hour {
		text += fmt.Sprintf("\nПереезд %s не удался: %s", b.stamp(time.Unix(m.At, 0)), firstLine(m.Error))
	}
	return view{text: text, kb: telegram.Keyboard{
		{btn("Переехать", "mr"), btn("Перезапустить туннель", "rs")},
		{btn("Настройки", "m:settings")},
		backRow,
	}}
}

// exitShort says in a word or two where the devices' traffic leaves from.
func exitShort(want egress.Mode, st *egress.Status) string {
	if want != egress.WARP {
		if st != nil && st.State != egress.StateDirect {
			return "переключаю на прямой"
		}
		return "напрямую"
	}
	if st == nil {
		return "WARP, нет вестей"
	}
	switch st.State {
	case egress.StateWARP:
		return "через WARP"
	case egress.StateFallback:
		return "WARP упал, напрямую"
	}
	return "включаю WARP"
}

func (b *Bot) settingsView() view { return b.settingsViewWith(nil, nil, nil) }

// settingsViewWith draws the switches, as given when one has just been
// changed: the manager applies it a moment later.
func (b *Bot) settingsViewWith(age, block *bool, exit *egress.Mode) view {
	s := b.Manager.Snapshot()
	autoAge, autoBlock, exitWant := s.AutoAge, s.AutoBlock, s.ExitWant
	if age != nil {
		autoAge = *age
	}
	if block != nil {
		autoBlock = *block
	}
	if exit != nil {
		exitWant = *exit
	}
	rows := [][]string{
		{"Автопереезд", onOff(autoAge)},
		{"При блокировке", onOff(autoBlock)},
		{"WARP", onOff(exitWant == egress.WARP)},
	}
	if autoAge && age == nil && !s.NextAutoMove.IsZero() {
		rows = append(rows[:1], append([][]string{{"Следующий", "около " + b.day(s.NextAutoMove)}}, rows[1:]...)...)
	}
	flip := func(op string, on bool) string {
		if on {
			return op + ":off"
		}
		return op + ":on"
	}
	text := telegram.Mono(columns(rows, 14, 16))
	if s.Moving {
		text += "\nСейчас идёт переезд."
	}
	return view{text: text, kb: telegram.Keyboard{
		{btn("Автопереезд", flip("pa", autoAge)), btn("При блокировке", flip("pb", autoBlock))},
		{btn("WARP", flip("px", exitWant == egress.WARP))},
		{btn("Проверить доступ из России", "ck")},
		{btn("‹ Сервер", "m:server")},
	}}
}

// ---- naming a device

// renamePrompt asks for a device's new name; the answer comes as a reply to
// this very message.
func (b *Bot) renamePrompt(ctx context.Context, chat int64, id string) string {
	o, err := b.overview(ctx)
	if err != nil {
		return "Список устройств сейчас не читается."
	}
	i := slices.IndexFunc(o.Devices, func(d node.DeviceView) bool { return d.ID == id })
	if i < 0 {
		return "Этого устройства уже нет в списке."
	}
	text := fmt.Sprintf("Как назвать %s? Ответьте на это сообщение новым именем; «-» вернёт имя из приложения.",
		shownName(o.Devices[i]))
	m, err := b.API.Ask(ctx, chat, text, "Новое имя")
	if err != nil {
		b.log().Warn("could not ask for a name", "error", err)
		return "Не получилось спросить имя."
	}
	b.mu.Lock()
	if b.renames == nil {
		b.renames = map[int64]string{}
	}
	b.renames[m.MessageID] = id
	b.mu.Unlock()
	return ""
}

// renamed carries out an answer to renamePrompt, if m is one, and reports
// whether it was.
func (b *Bot) renamed(ctx context.Context, m *telegram.Message) bool {
	if m.ReplyTo == nil {
		return false
	}
	b.mu.Lock()
	id, ok := b.renames[m.ReplyTo.MessageID]
	if ok {
		delete(b.renames, m.ReplyTo.MessageID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	label := telegram.Clean(strings.TrimSpace(m.Text))
	if label == "-" {
		label = ""
	}
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpRename, ID: id, Label: label})
	switch {
	case err != nil:
		b.reply(ctx, m.Chat.ID, view{text: "Не получилось: узел не отвечает."})
	case !resp.OK && resp.Missing:
		b.reply(ctx, m.Chat.ID, view{text: "Этого устройства уже нет в списке."})
	case !resp.OK:
		b.reply(ctx, m.Chat.ID, view{text: "Не получилось: имя должно быть не длиннее 32 знаков."})
	default:
		b.reply(ctx, m.Chat.ID, b.deviceView(ctx, id))
	}
	return true
}
