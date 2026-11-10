package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

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

var backRow = []telegram.Button{btn("« Меню", "m:menu")}

func menuView() view {
	return view{
		text: "Rebirth: управление сервером.",
		kb: telegram.Keyboard{
			{btn("Состояние", "m:status"), btn("Устройства", "m:devices")},
			{btn("Заявки", "m:requests"), btn("Обслуживание", "m:service")},
		},
	}
}

var regionNames = map[string]string{
	"sto": "Стокгольм",
	"waw": "Варшава",
	"fra": "Франкфурт",
	"ams": "Амстердам",
}

func regionName(code string) string {
	if n, ok := regionNames[code]; ok {
		return n
	}
	return code
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

func stamp(t time.Time) string { return t.Local().Format("02.01 15:04") }

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

func (b *Bot) statusView(ctx context.Context) view {
	s := b.Manager.Snapshot()
	now := b.now()
	var out strings.Builder
	fmt.Fprintf(&out, "Сервер: %s, поколение %d, работает %s.\n", regionName(s.Region), s.Generation, duration(now.Sub(s.CreatedAt)))
	fmt.Fprintf(&out, "Состояние: %s.\n", healthWords(s.Health))
	if s.Moving {
		out.WriteString("Сейчас идёт переезд.\n")
	}
	if w := s.Watch; w != nil {
		fmt.Fprintf(&out, "Связь с российскими сайтами: %s с %s.\n", watchWords(w.Round), stamp(time.Unix(w.Since, 0)))
	}
	if o, err := b.overview(ctx); err == nil {
		online := 0
		for _, d := range o.Devices {
			if d.Online {
				online++
			}
		}
		fmt.Fprintf(&out, "Устройства: %d в сети из %d.", online, len(o.Devices))
		if n := len(o.Pending); n > 0 {
			fmt.Fprintf(&out, " Ждут одобрения: %d.", n)
		}
		if o.Approval {
			out.WriteString("\nНовые устройства ждут вашего одобрения.\n")
		} else {
			out.WriteString("\nОдобрение выключено: новые устройства входят сами.\n")
		}
	} else {
		out.WriteString("Список устройств сейчас не читается.\n")
	}
	fmt.Fprintf(&out, "Автопереезд по возрасту: %s", onOff(s.AutoAge))
	if !s.NextAutoMove.IsZero() {
		fmt.Fprintf(&out, " (следующий около %s)", stamp(s.NextAutoMove))
	}
	fmt.Fprintf(&out, "; при блокировке: %s.\n", onOff(s.AutoBlock))
	fmt.Fprintf(&out, "Сертификат действует ещё %d дн.\n", s.CertDaysLeft)
	if m := s.LastMove; m != nil {
		fmt.Fprintf(&out, "Последний переезд: %s, %s, итог: %s.", stamp(time.Unix(m.At, 0)), triggerWords(m.Trigger), m.State)
		if m.Error != "" {
			fmt.Fprintf(&out, "\nОшибка: %s", firstLine(m.Error))
		}
	}
	return view{text: strings.TrimRight(out.String(), "\n"), kb: telegram.Keyboard{
		{btn("Обновить", "m:status"), btn("Устройства", "m:devices")},
		backRow,
	}}
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

func seen(d node.DeviceView, now time.Time) string {
	switch {
	case d.Online:
		return "в сети"
	case d.LastHandshake > 0:
		return "был " + duration(now.Sub(time.Unix(d.LastHandshake, 0))) + " назад"
	case d.LastSeen > d.EnrolledAt:
		return "был " + duration(now.Sub(time.Unix(d.LastSeen, 0))) + " назад"
	}
	return "ещё не подключалось"
}

func deviceName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "без имени"
	}
	return name
}

// short trims a name to fit on a button.
func short(name string, n int) string {
	r := []rune(deviceName(name))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func (b *Bot) devicesView(ctx context.Context) view {
	o, err := b.overview(ctx)
	if err != nil {
		return view{text: "Список устройств сейчас не читается: " + err.Error(), kb: telegram.Keyboard{backRow}}
	}
	if len(o.Devices) == 0 {
		return view{text: "Устройств с доступом нет.", kb: telegram.Keyboard{{btn("Заявки", "m:requests")}, backRow}}
	}
	now := b.now()
	var out strings.Builder
	fmt.Fprintf(&out, "Устройства с доступом (%d):\n", len(o.Devices))
	var kb telegram.Keyboard
	for i, d := range o.Devices {
		fmt.Fprintf(&out, "\n%d. %s · код %s · %s", i+1, deviceName(d.Name), d.Code, seen(d, now))
		if d.RxBytes+d.TxBytes > 0 {
			fmt.Fprintf(&out, " · %s", bytesWords(d.RxBytes+d.TxBytes))
		}
		kb = append(kb, []telegram.Button{btn(fmt.Sprintf("Отозвать: %d. %s", i+1, short(d.Name, 20)), "rv:"+d.ID)})
	}
	kb = append(kb, []telegram.Button{btn("Обновить", "m:devices"), btn("Заявки", "m:requests")}, backRow)
	return view{text: out.String(), kb: kb}
}

func (b *Bot) confirmRevoke(ctx context.Context, id string) view {
	o, err := b.overview(ctx)
	if err == nil {
		for _, d := range o.Devices {
			if d.ID == id {
				return confirmView(fmt.Sprintf("Отозвать доступ у «%s» (код %s)? Устройство отключится сразу и больше не войдёт, пока вы не выдадите доступ снова.",
					deviceName(d.Name), d.Code), "Да, отозвать", "rv!:"+id, "m:devices")
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
			fmt.Fprintf(&out, "\n%d. %s · код %s · просит с %s", i+1, deviceName(w.Name), w.Code, stamp(time.Unix(w.AskedAt, 0)))
			if w.KeyChange {
				out.WriteString(" (уже было одобрено, ключ сменился)")
			}
			kb = append(kb, []telegram.Button{
				btn(fmt.Sprintf("Одобрить %d", i+1), "ap:"+w.ID+":l"),
				btn(fmt.Sprintf("Отказать %d", i+1), "dn:"+w.ID+":l"),
			})
		}
	}
	if len(o.Denied) > 0 {
		fmt.Fprintf(&out, "\n\nОтказано (%d):", len(o.Denied))
		for i, d := range o.Denied {
			n := len(o.Pending) + i + 1
			fmt.Fprintf(&out, "\n%d. %s · код %s · %s", n, deviceName(d.Name), d.Code, stamp(time.Unix(d.At, 0)))
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

func (b *Bot) serviceView() view { return b.serviceViewWith(nil, nil) }

// serviceViewWith draws the maintenance menu, with the automatic-move switches
// as given when they have just been changed.
func (b *Bot) serviceViewWith(age, block *bool) view {
	s := b.Manager.Snapshot()
	autoAge, autoBlock := s.AutoAge, s.AutoBlock
	if age != nil {
		autoAge = *age
	}
	if block != nil {
		autoBlock = *block
	}
	toggle := func(label string, on bool, op string) telegram.Button {
		if on {
			return btn(label+": вкл → выключить", op+":off")
		}
		return btn(label+": выкл → включить", op+":on")
	}
	text := "Обслуживание."
	if s.Moving {
		text += "\nСейчас идёт переезд."
	}
	return view{text: text, kb: telegram.Keyboard{
		{btn("Переехать сейчас", "mv"), btn("Переехать в регион…", "mr")},
		{btn("Перезапустить туннель", "rs")},
		{toggle("Автопереезд по возрасту", autoAge, "pa")},
		{toggle("Переезд при блокировке", autoBlock, "pb")},
		{btn("Проверить доступность из России", "ck")},
		backRow,
	}}
}

func (b *Bot) regionView() view {
	s := b.Manager.Snapshot()
	id := nonce()
	var row []telegram.Button
	var kb telegram.Keyboard
	for _, r := range s.Regions {
		if r == s.Region {
			continue
		}
		row = append(row, btn(regionName(r), "mv!:"+id+":"+r))
		if len(row) == 2 {
			kb = append(kb, row)
			row = nil
		}
	}
	if len(row) > 0 {
		kb = append(kb, row)
	}
	kb = append(kb, []telegram.Button{btn("Отмена", "m:service")})
	return view{text: fmt.Sprintf("Куда переехать? Сейчас: %s.", regionName(s.Region)), kb: kb}
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
	return telegram.Keyboard{{btn("Одобрить", "ap:"+w.ID), btn("Отказать", "dn:"+w.ID)}}
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
		return fmt.Sprintf("Проверка: из России дошли %d из %d точек, из-за рубежа %d из %d. Итог: %s.",
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
