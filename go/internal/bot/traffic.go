package bot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/telegram"
	"awgrotator/internal/usage"
)

// Where the traffic goes: a short list of the largest sites and devices for a
// day, a week or a month, and the same for one device.

// trafficTop is how many sites and devices a report lists by name.
const trafficTop = 5

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

func (b *Bot) trafficView(ctx context.Context, period string) view {
	kb := telegram.Keyboard{periodRow("tr:", ""), backRow}
	r, err := b.usage(ctx, period, "")
	if err != nil {
		return view{text: "Учёт трафика сейчас не читается: " + firstLine(err.Error()), kb: kb}
	}
	head := fmt.Sprintf("Трафик за %s%s", periodWords[period], b.since(r.Report))
	if r.Total.Total() == 0 {
		return view{text: head + ": пока ничего.", kb: kb}
	}
	sites := topRows(r.Sites, siteName, func(int) string { return "Остальное" })
	devices := topRows(r.Devices, func(id string) string { return deviceLabel(id, r.Names) }, func(n int) string {
		return fmt.Sprintf("Ещё %d", n)
	})
	block := columns(sites, 18, -7) + "\n\n" + columns(devices, 18, -7)
	if r.Server.Up > 0 {
		block += "\n\n" + columns([][]string{{"Сервер отправил", amount(r.Server.Up)}}, 18, -7)
	}
	return view{text: fmt.Sprintf("%s: %s.\n", head, amount(r.Total.Total())) + telegram.Mono(block), kb: kb}
}

// topRows is the largest items as rows of a name and an amount, and the rest
// as one more row.
func topRows(items []usage.Item, name func(string) string, rest func(n int) string) [][]string {
	var rows [][]string
	var other uint64
	others := 0
	for _, it := range items {
		if it.Total() == 0 {
			continue
		}
		if len(rows) < trafficTop && it.Key != usage.SiteOther {
			rows = append(rows, []string{name(it.Key), amount(it.Total())})
			continue
		}
		other += it.Total()
		others++
	}
	if other > 0 {
		rows = append(rows, []string{rest(others), amount(other)})
	}
	return rows
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
