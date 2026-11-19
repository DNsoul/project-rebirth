package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/egress"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/proto"
	"awgrotator/internal/telegram"
)

// view is a message: its text and its buttons.
type view struct {
	text string
	kb   telegram.Keyboard
}

func btn(text, data string) telegram.Button { return telegram.Button{Text: text, Data: data} }

var backRow = []telegram.Button{btn("« Меню", "m:main")}

var regionNames = map[string]string{
	"sto": "Стокгольм",
	"waw": "Варшава",
	"fra": "Франкфурт",
	"ams": "Амстердам",
	"sia": "Шяуляй",
}

func regionName(code string) string {
	if n, ok := regionNames[code]; ok {
		return n
	}
	return code
}

var providerNames = map[string]string{
	"vultr":  "Vultr",
	"cherry": "Cherry",
}

func providerName(name string) string {
	if n, ok := providerNames[name]; ok {
		return n
	}
	return name
}

// placeLabel is a place as the operator reads it: the city, who hosts it, and
// a note when it is paid by the month.
func placeLabel(p lifecycle.PlaceConfig) string {
	pl := p.Place()
	label := regionName(pl.Location) + " · " + providerName(pl.Provider)
	if p.Billing == cloud.Monthly {
		label += ", месяц"
	}
	return label
}

func healthWords(v lifecycle.Verdict) string {
	switch v {
	case lifecycle.Healthy:
		return "в порядке"
	case lifecycle.Blocked:
		return "похоже, недоступен из России"
	case lifecycle.Broken:
		return "службы сервера не держатся"
	}
	return string(v)
}

func triggerWords(t lifecycle.Trigger) string {
	switch t {
	case lifecycle.TriggerCommand:
		return "по вашей команде"
	case lifecycle.TriggerAge:
		return "по возрасту"
	case lifecycle.TriggerBlocked:
		return "из-за блокировки"
	case lifecycle.TriggerBroken:
		return "из-за поломки"
	}
	return string(t)
}

func onOff(b bool) string {
	if b {
		return "вкл"
	}
	return "выкл"
}

func duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "меньше минуты"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч", int(d/time.Hour))
	}
	return fmt.Sprintf("%d сут", int(d/(24*time.Hour)))
}

// stamp writes a moment on the owners' clock.
func (b *Bot) stamp(t time.Time) string { return t.In(b.zone()).Format("02.01 15:04") }

// day writes a date on the owners' clock.
func (b *Bot) day(t time.Time) string { return t.In(b.zone()).Format("02.01") }

func bytesWords(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d КБ", n/(1<<10))
	}
	return fmt.Sprintf("%d Б", n)
}

func watchWords(round string) string {
	switch round {
	case "good":
		return "отвечают"
	case "bad":
		return "НЕ отвечают"
	case "inconclusive":
		return "неясно"
	}
	return round
}

func deviceName(name string) string {
	name = strings.TrimSpace(telegram.Clean(name))
	if name == "" {
		return "без имени"
	}
	return name
}

func (b *Bot) confirmRevoke(ctx context.Context, id string) view {
	o, err := b.overview(ctx)
	if err == nil {
		for _, d := range o.Devices {
			if d.ID == id {
				return view{
					text: fmt.Sprintf("Отозвать доступ у %s (код %s)? Устройство отключится сразу и больше не войдёт.", shownName(d), d.Code),
					kb: telegram.Keyboard{{
						telegram.Button{Text: "Отозвать", Data: "rv!:" + id, Style: telegram.StyleDanger},
						btn("Отмена", "dv:"+id),
					}},
				}
			}
		}
	}
	return view{text: "Этого устройства уже нет в списке.", kb: telegram.Keyboard{{btn("К устройствам", "m:devices")}}}
}

func (b *Bot) requestsView(ctx context.Context) view {
	o, err := b.overview(ctx)
	if err != nil {
		return view{text: "Заявки сейчас не читаются: " + err.Error(), kb: telegram.Keyboard{backRow}}
	}
	var out strings.Builder
	var kb telegram.Keyboard
	if len(o.Pending) == 0 {
		out.WriteString("Заявок нет.")
	} else {
		fmt.Fprintf(&out, "Ждут одобрения (%d):", len(o.Pending))
		for i, w := range o.Pending {
			fmt.Fprintf(&out, "\n%d. %s · код %s · просит с %s", i+1, deviceName(w.Name), w.Code, b.stamp(time.Unix(w.AskedAt, 0)))
			if w.KeyChange {
				out.WriteString(" (уже было одобрено, ключ сменился)")
			}
			kb = append(kb, []telegram.Button{
				{Text: fmt.Sprintf("Одобрить %d", i+1), Data: "ap:" + w.ID + ":l", Style: telegram.StyleSuccess},
				btn(fmt.Sprintf("Отказать %d", i+1), "dn:"+w.ID+":l"),
			})
		}
	}
	if len(o.Denied) > 0 {
		fmt.Fprintf(&out, "\n\nОтказано (%d):", len(o.Denied))
		for i, d := range o.Denied {
			n := len(o.Pending) + i + 1
			fmt.Fprintf(&out, "\n%d. %s · код %s · %s", n, deviceName(d.Name), d.Code, b.stamp(time.Unix(d.At, 0)))
			if d.KeyOnly {
				out.WriteString(" (только новый ключ)")
			}
			kb = append(kb, []telegram.Button{
				btn(fmt.Sprintf("Выдать доступ %d", n), "al:"+d.ID),
				btn(fmt.Sprintf("Забыть %d", n), "fg:"+d.ID),
			})
		}
	}
	kb = append(kb, []telegram.Button{btn("Обновить", "m:requests"), btn("Устройства", "m:devices")}, backRow)
	return view{text: out.String(), kb: kb}
}

// confirmExit asks before the way out is switched: every open connection on
// every device breaks once.
func confirmExit(on bool) view {
	if on {
		return confirmView("Выпускать трафик устройств через Cloudflare WARP? Сайты и приложения увидят адрес Cloudflare, а не адрес сервера. "+
			"Открытые соединения оборвутся и через несколько секунд восстановятся сами.",
			"Да, включить", "px!:on:"+nonce(), "m:settings")
	}
	return confirmView("Выпускать трафик устройств напрямую, с адреса сервера? "+
		"Открытые соединения оборвутся и через несколько секунд восстановятся сами.",
		"Да, выключить", "px!:off:"+nonce(), "m:settings")
}

func (b *Bot) regionView() view {
	s := b.Manager.Snapshot()
	id := nonce()
	var row []telegram.Button
	// Anywhere is the usual choice: an hourly place picked by the manager.
	kb := telegram.Keyboard{{btn("Куда угодно", "mv")}}
	for _, p := range s.Places {
		if pl := p.Place(); pl.Provider == s.Provider && pl.Location == s.Region {
			continue
		}
		// A monthly place is asked about once more: it spends a month up front.
		data := "mv!:" + id + ":" + p.ID
		if p.Billing == cloud.Monthly {
			data = "mp:" + p.ID
		}
		row = append(row, btn(placeLabel(p), data))
		if len(row) == 2 {
			kb = append(kb, row)
			row = nil
		}
	}
	if len(row) > 0 {
		kb = append(kb, row)
	}
	kb = append(kb, []telegram.Button{btn("Отмена", "m:server")})
	return view{text: fmt.Sprintf("Куда переехать? Сейчас: %s · %s.", regionName(s.Region), providerName(s.Provider)), kb: kb}
}

// monthlyView asks once more before a move to a place paid by the month.
func (b *Bot) monthlyView(place string) view {
	s := b.Manager.Snapshot()
	for _, p := range s.Places {
		if p.ID == place {
			return confirmView(fmt.Sprintf("Переехать в %s? Там оплата помесячная: месяц спишется вперёд сразу, и при раннем переезде остаток не вернётся. Сами туда переезды не идут, только по вашей команде.", placeLabel(p)),
				"Да, оплатить и переехать", "mv!:"+nonce()+":"+p.ID, "mr")
		}
	}
	return b.regionView()
}

func confirmView(question, yes, yesData, noData string) view {
	return view{text: question, kb: telegram.Keyboard{{btn(yes, yesData), btn("Отмена", noData)}}}
}

func requestText(w node.WaitingView) string {
	if w.KeyChange {
		return fmt.Sprintf("Устройство «%s» уже было одобрено, но пришло с новым ключом: переустановили приложение или это кто-то другой.\nКод: %s — его же показывает приложение.",
			deviceName(w.Name), w.Code)
	}
	return fmt.Sprintf("Новое устройство просит доступ:\n%s\nКод: %s — его же показывает приложение.", deviceName(w.Name), w.Code)
}

func requestKeyboard(w node.WaitingView) telegram.Keyboard {
	return telegram.Keyboard{{
		telegram.Button{Text: "Одобрить", Data: "ap:" + w.ID, Style: telegram.StyleSuccess},
		btn("Отказать", "dn:"+w.ID),
	}}
}

// decisionText says how a decision about a device came out.
func decisionText(op string, resp node.ControlResponse, err error) string {
	if err != nil {
		return "Не получилось: узел не отвечает (" + firstLine(err.Error()) + ")."
	}
	if !resp.OK {
		switch {
		case resp.Missing:
			return "Уже решено: этой заявки или устройства больше нет."
		case resp.Reason == proto.EnrollKeyInUse:
			return "Не получилось: этот ключ уже у другого устройства."
		case resp.Reason == proto.EnrollFull:
			return "Не получилось: на сервере нет мест для новых устройств."
		}
		return "Не получилось: " + firstLine(resp.Error)
	}
	name := deviceName(resp.Name)
	switch op {
	case "ap":
		return "Одобрено: " + name + "."
	case "dn":
		return "Отказано: " + name + "."
	case "rv!":
		return "Доступ отозван: " + name + "."
	case "al":
		return "Доступ выдан: " + name + "."
	case "fg":
		return "Отказ забыт: устройство сможет попросить доступ снова."
	}
	return "Готово."
}

func eventText(e lifecycle.Event) string {
	switch e.Kind {
	case lifecycle.EventMoveStarted:
		where := ""
		if e.Region != "" {
			where = " в " + regionName(e.Region)
		}
		return fmt.Sprintf("Начинаю переезд%s (%s). Это займёт несколько минут.", where, triggerWords(e.Trigger))
	case lifecycle.EventMoveFailed:
		return "Переезд не удался, сервер остаётся прежним.\n" + firstLine(e.Err)
	case lifecycle.EventMovePublished:
		return fmt.Sprintf("Новый сервер готов: %s. Приложения переключатся сами. Дальше отвечаю с него.", regionName(e.Region))
	case lifecycle.EventCurrent:
		return fmt.Sprintf("Сервер на связи: %s, поколение %d.", regionName(e.Region), e.Generation)
	case lifecycle.EventHealth:
		switch e.Health {
		case lifecycle.Blocked:
			return "Похоже, сервер недоступен из России: несколько проверок подряд не дошли."
		case lifecycle.Broken:
			return "Службы сервера не держатся: туннель перезапускается и снова падает."
		case lifecycle.Healthy:
			return "Сервер снова в порядке."
		}
		return "Состояние сервера: " + string(e.Health)
	case lifecycle.EventExit:
		switch e.Exit {
		case egress.StateWARP:
			return "Выход через WARP работает: сайты видят адрес Cloudflare, а не адрес сервера."
		case egress.StateFallback:
			return "WARP не отвечает. Устройства временно выходят в интернет напрямую, с адреса сервера. " +
				"Когда WARP заработает, вернусь на него сам."
		case egress.StateDirect:
			return "Выход в интернет теперь напрямую, с адреса сервера."
		}
		return "Выход в интернет: " + string(e.Exit)
	}
	return string(e.Kind)
}

// resultText says how an action came out, once the manager has done it. An
// empty text means there is nothing to add to the button's own notice.
func resultText(kind lifecycle.ActionKind, r lifecycle.Result) string {
	if r.Skipped {
		return "Это уже было сделано."
	}
	if r.Err != nil {
		return "Не получилось: " + firstLine(r.Err.Error())
	}
	switch kind {
	case lifecycle.ActRestartNode:
		return "Туннель перезапущен."
	case lifecycle.ActPolicy:
		return ""
	case lifecycle.ActCheck:
		c := r.Check
		if c == nil {
			return ""
		}
		verdict := "неясно: контроль за рубежом тоже плохо отвечает"
		switch c.Round {
		case lifecycle.Good:
			verdict = "сервер доступен из России"
		case lifecycle.Bad:
			verdict = "сервер НЕ доступен из России"
		}
		return fmt.Sprintf("Проверка: из домашних и мобильных сетей России дошли %d из %d точек, из-за рубежа %d из %d. Итог: %s.",
			c.Target.Reached, c.Target.Total, c.Control.Reached, c.Control.Total, verdict)
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}
