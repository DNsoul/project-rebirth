package bot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/splitdns"
	"awgrotator/internal/telegram"
	"awgrotator/internal/usage"
)

// Where the traffic goes: the largest sites for a day, a week or a month, each
// of which can be sent past the server; the largest devices; and the same for
// one device.

// trafficTop is how many sites and devices a report lists by name.
const trafficTop = 10

var periodWords = map[string]string{usage.Day: "сутки", usage.Week: "неделю", usage.Month: "месяц"}

// periodKeys are the periods as they travel in button data.
var periodKeys = map[string]string{"d": usage.Day, "w": usage.Week, "m": usage.Month}

func periodKey(p string) string {
	for k, v := range periodKeys {
		if v == p {
			return k
		}
	}
	return "d"
}

// amount writes traffic in decimal units, as providers count it.
func amount(n uint64) string {
	switch {
	case n >= 10e9:
		return fmt.Sprintf("%d ГБ", int64(math.Round(float64(n)/1e9)))
	case n >= 1e9:
		return strings.Replace(fmt.Sprintf("%.1f ГБ", float64(n)/1e9), ".", ",", 1)
	case n >= 1e6:
		return fmt.Sprintf("%d МБ", int64(math.Round(float64(n)/1e6)))
	case n == 0:
		return "0"
	}
	return "меньше 1 МБ"
}

// siteName is a site as it is shown: the labels the count uses for traffic
// with no site get a capital letter like the rest.
func siteName(s string) string {
	if s == usage.SiteServer {
		return "Сам сервер"
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || !unicode.IsLower(r) || strings.Contains(s, ".") {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func (b *Bot) usage(ctx context.Context, period, device string) (*node.UsageView, error) {
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpUsage, Period: period, ID: device})
	if err != nil {
		return nil, err
	}
	if !resp.OK || resp.Usage == nil {
		return nil, errors.New(resp.Error)
	}
	return resp.Usage, nil
}

// since says when counting began, when that is inside the period.
func (b *Bot) since(r usage.Report) string {
	start := periodStart(r.Period, b.now())
	if r.From <= start.Unix() {
		return ""
	}
	from := time.Unix(r.From, 0)
	if r.Period == usage.Day {
		return " (учёт с " + b.stamp(from) + ")"
	}
	return " (учёт с " + b.day(from) + ")"
}

func periodStart(period string, now time.Time) time.Time {
	day := func(back int) time.Time {
		y, m, d := now.UTC().Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -back)
	}
	switch period {
	case usage.Week:
		return day(6)
	case usage.Month:
		return day(29)
	}
	return now.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
}

func periodRow(prefix, suffix string) []telegram.Button {
	return []telegram.Button{
		btn("Сутки", prefix+"d"+suffix), btn("Неделя", prefix+"w"+suffix), btn("Месяц", prefix+"m"+suffix),
	}
}

// deviceLabel is a device as the report shows it: its name, with the code
// when another device has the same name.
func deviceLabel(id string, names map[string]node.DeviceLabel) string {
	l, ok := names[id]
	if !ok {
		return "удалённое"
	}
	same := 0
	for _, o := range names {
		if o.Name == l.Name {
			same++
		}
	}
	if same > 1 {
		return deviceName(l.Name) + " · " + l.Code
	}
	return deviceName(l.Name)
}

// trafficView is the largest sites over a period, numbered: a number opens
// the site, and from there it can be sent past the server.
func (b *Bot) trafficView(ctx context.Context, period string) view {
	pk := periodKey(period)
	names, listErr := b.directList(ctx)
	directBtn := btn("Напрямую", "dp:0")
	if listErr == nil && len(names) > 0 {
		directBtn.Text = fmt.Sprintf("Напрямую · %d", len(names))
	}
	tail := telegram.Keyboard{
		{btn("Устройства", "tv:"+pk), directBtn},
		{backRow[0], btn("Сбросить", "ux")},
	}
	kb := append(telegram.Keyboard{periodRow("tr:", "")}, tail...)

	r, err := b.usage(ctx, period, "")
	if err != nil {
		return view{text: "Учёт трафика сейчас не читается: " + firstLine(err.Error()), kb: kb}
	}
	head := fmt.Sprintf("Трафик за %s%s", periodWords[period], b.since(r.Report))
	if r.Total.Total() == 0 {
		return view{text: head + ": пока ничего.", kb: kb}
	}

	var (
		rows     [][]string
		buttons  []telegram.Button
		isDirect []bool
		rest     uint64
	)
	for _, it := range r.Sites {
		if it.Total() == 0 {
			continue
		}
		// The server's own traffic is the resolver's: small, and nothing to
		// decide about.
		if len(buttons) == trafficTop || it.Key == usage.SiteOther || it.Key == usage.SiteServer {
			rest += it.Total()
			continue
		}
		n := strconv.Itoa(len(buttons) + 1)
		rows = append(rows, []string{n, siteName(it.Key), amount(it.Total())})
		buttons = append(buttons, btn(n, b.siteRef("ts:"+pk+":", it.Key)))
		isDirect = append(isDirect, slices.Contains(names, it.Key))
	}
	// A site sent direct is no longer counted, but stays in a longer
	// period's list for a while; its number carries a mark that says so.
	numWidth := -2
	if slices.Contains(isDirect, true) {
		numWidth = -3
		for i, d := range isDirect {
			if d {
				rows[i][0] += directMark
			} else {
				rows[i][0] += " "
			}
		}
	}
	if rest > 0 {
		rows = append(rows, []string{"", "Остальное", amount(rest)})
	}
	block := columns(rows, numWidth, 19, -7)
	if r.Server.Up > 0 {
		block += "\n\n" + columns([][]string{{"", "Сервер отправил", amount(r.Server.Up)}}, numWidth, 19, -7)
	}
	text := fmt.Sprintf("%s: %s.\n", head, amount(r.Total.Total())) + telegram.Mono(block)
	if numWidth == -3 {
		text += "\n" + directLegend()
	}

	kb = telegram.Keyboard{periodRow("tr:", "")}
	kb = append(kb, rowsOf(buttons, 5)...)
	return view{text: text, kb: append(kb, tail...)}
}

// rowsOf lays buttons out perRow to a row.
func rowsOf(buttons []telegram.Button, perRow int) telegram.Keyboard {
	var kb telegram.Keyboard
	for len(buttons) > 0 {
		n := min(perRow, len(buttons))
		kb = append(kb, buttons[:n])
		buttons = buttons[n:]
	}
	return kb
}

// trafficDevicesView is the devices that used the most over a period; a
// number opens the device.
func (b *Bot) trafficDevicesView(ctx context.Context, period string) view {
	pk := periodKey(period)
	back := []telegram.Button{btn("‹ Трафик", "tr:"+pk)}
	kb := telegram.Keyboard{periodRow("tv:", ""), back}
	r, err := b.usage(ctx, period, "")
	if err != nil {
		return view{text: "Учёт трафика сейчас не читается: " + firstLine(err.Error()), kb: kb}
	}
	head := fmt.Sprintf("Устройства за %s%s", periodWords[period], b.since(r.Report))
	if r.Total.Total() == 0 {
		return view{text: head + ": пока ничего.", kb: kb}
	}
	var (
		rows    [][]string
		buttons []telegram.Button
		rest    uint64
	)
	for _, it := range r.Devices {
		if it.Total() == 0 {
			continue
		}
		// A removed device has no card to open; it goes with the rest.
		if _, known := r.Names[it.Key]; !known || len(buttons) == trafficTop {
			rest += it.Total()
			continue
		}
		n := strconv.Itoa(len(buttons) + 1)
		rows = append(rows, []string{n, deviceLabel(it.Key, r.Names), amount(it.Total())})
		buttons = append(buttons, btn(n, "dv:"+it.Key))
	}
	if rest > 0 {
		rows = append(rows, []string{"", "Остальные", amount(rest)})
	}
	kb = telegram.Keyboard{periodRow("tv:", "")}
	kb = append(kb, rowsOf(buttons, 5)...)
	kb = append(kb, back)
	text := fmt.Sprintf("%s: %s.\n", head, amount(r.Total.Total())) + telegram.Mono(columns(rows, -2, 19, -7))
	return view{text: text, kb: kb}
}

// siteView is one site of the traffic list: how much went to it, who used it
// most, and the way to send it past the server or back.
func (b *Bot) siteView(ctx context.Context, period, site string) view {
	pk := periodKey(period)
	back := []telegram.Button{btn("‹ Трафик", "tr:"+pk)}
	head := siteName(site)
	if s := usage.ServiceOf(site); s != "" {
		head += " · " + s
	}
	lines := []string{head}
	if r, err := b.siteUsage(ctx, period, site); err == nil {
		line := fmt.Sprintf("За %s: %s.", periodWords[period], amount(r.Total.Total()))
		if len(r.Devices) > 0 && r.Devices[0].Total() > 0 {
			line += " Больше всего: " + deviceLabel(r.Devices[0].Key, r.Names) + "."
		}
		lines = append(lines, line)
	}

	if why := notDirect(site); why != "" {
		return view{text: strings.Join(append(lines, why), "\n"), kb: telegram.Keyboard{back}}
	}
	names, err := b.directList(ctx)
	switch {
	case err != nil:
		lines = append(lines, "Список «напрямую» сейчас не читается: "+firstLine(err.Error()))
		return view{text: strings.Join(lines, "\n"), kb: telegram.Keyboard{back}}
	case slices.Contains(names, site):
		lines = append(lines, "Идёт напрямую, мимо сервера.")
		return view{text: strings.Join(lines, "\n"), kb: telegram.Keyboard{
			{btn("Вернуть через сервер", b.siteRef("dr:"+pk+":", site))}, back,
		}}
	}
	lines = append(lines, "Если сайт закрыт в России, напрямую он не откроется ни у кого.")
	return view{text: strings.Join(lines, "\n"), kb: telegram.Keyboard{
		{btn("Пустить напрямую", b.siteRef("da:"+pk+":", site))}, back,
	}}
}

// notDirect says why a line of the traffic list cannot be sent direct, or ""
// when it can.
func notDirect(site string) string {
	switch site {
	case usage.SiteTelegram:
		return "Telegram идёт через сервер по адресам, а не по именам, поэтому напрямую его не пустить."
	case usage.SiteUnknown:
		return "Имён этих сайтов сервер не знает, поэтому напрямую их не пустить."
	case usage.SiteServer:
		return "Это служебный трафик самого VPN."
	case usage.SiteElsewhere:
		return "Это трафик не по именам сайтов, поэтому напрямую его не пустить."
	}
	if _, err := splitdns.CheckDirectName(site); err != nil {
		return "Это имя нельзя пустить напрямую."
	}
	return ""
}

func (b *Bot) siteUsage(ctx context.Context, period, site string) (*node.UsageView, error) {
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpUsage, Period: period, Site: site})
	if err != nil {
		return nil, err
	}
	if !resp.OK || resp.Usage == nil {
		return nil, errors.New(resp.Error)
	}
	return resp.Usage, nil
}

// heavyView is the notice about a device that used a lot in a day.
func heavyView(h lifecycle.HeavyUsage) view {
	name := deviceName(h.Name)
	if h.Name == "" && h.Code == "" {
		name = "Устройство"
	}
	text := fmt.Sprintf("%s (код %s) за сутки: %s", name, h.Code, amount(h.Bytes))
	if h.Total > 0 {
		text += fmt.Sprintf(", %d%% всего трафика", int(math.Round(float64(h.Bytes)*100/float64(h.Total))))
	}
	return view{text: text + ".", kb: telegram.Keyboard{{btn("Куда", "dv:"+h.ID)}}}
}

// periodOf reads a period from button data; anything unknown is a day.
func periodOf(key string) string {
	if p, ok := periodKeys[key]; ok {
		return p
	}
	return usage.Day
}

// confirmReset asks before the traffic history is forgotten.
func confirmReset() view {
	return view{
		text: "Стереть учёт трафика? Сайты и устройства начнут считаться с нуля. Счёт хостера это не меняет.",
		kb: telegram.Keyboard{{
			telegram.Button{Text: "Стереть", Data: "ux!", Style: telegram.StyleDanger},
			btn("Отмена", "tr:d"),
		}},
	}
}

// resetUsage forgets the traffic history and says how that went, in a line
// for a notice.
func (b *Bot) resetUsage(ctx context.Context) string {
	if b.Manager.Snapshot().Moving {
		// The history goes to the new server as it was when the move began.
		return "Идёт переезд. Сбросить учёт можно будет через пару минут, на новом сервере."
	}
	resp, err := b.Devices.Control(ctx, node.ControlRequest{Op: node.OpUsageReset})
	switch {
	case err != nil:
		return "Не получилось: узел не отвечает."
	case !resp.OK:
		return "Не получилось: " + firstLine(resp.Error)
	}
	return "Учёт трафика начат заново."
}
