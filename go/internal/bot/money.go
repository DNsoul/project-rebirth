package bot

import (
	"fmt"
	"math"
	"strings"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/telegram"
)

// What the bot says about paying for the server and its traffic allowance.
// Kept short on purpose: a line or two, and a button only where it saves the
// operator a search.

// providerPortals are where the operator pays, for the button under a
// reminder. A provider not listed gets no button.
var providerPortals = map[string]string{
	"cherry": "https://portal.cherryservers.com/",
}

// money writes an amount the Russian way: "7,36 €".
func money(v float64, currency string) string {
	sign := map[string]string{"EUR": "€", "USD": "$"}[strings.ToUpper(currency)]
	if sign == "" {
		sign = currency
	}
	return strings.Replace(fmt.Sprintf("%.2f", v), ".", ",", 1) + " " + sign
}

// gigabytes writes traffic in the provider's decimal gigabytes.
func gigabytes(n int64) string { return fmt.Sprintf("%d ГБ", int64(math.Round(float64(n)/1e9))) }

// usedOf writes the traffic used against the allowance: "420 из 1000 ГБ".
func usedOf(a cloud.Account) string {
	return fmt.Sprintf("%d из %s", int64(math.Round(float64(a.TrafficUsed)/1e9)), gigabytes(a.TrafficLimit))
}

// plural picks the Russian form for n: one, few (2-4), many.
func plural(n int, one, few, many string) string {
	switch n10, n100 := n%10, n%100; {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	}
	return many
}

// left says how long until a moment, in days or, on the last day, hours.
func left(d time.Duration) string {
	if d < 36*time.Hour {
		h := max(1, int(math.Round(d.Hours())))
		return fmt.Sprintf("%d %s", h, plural(h, "час", "часа", "часов"))
	}
	n := int(math.Round(d.Hours() / 24))
	return fmt.Sprintf("%d %s", n, plural(n, "день", "дня", "дней"))
}

// runsOut is the day the allowance runs out at the present pace, or "" when
// it will not within the provider's thirty days.
func (b *Bot) runsOut(a cloud.Account, created time.Time) string {
	now := b.now()
	_, out := lifecycle.QuotaForecast(a, created, now)
	if out.IsZero() || out.Sub(now) > cloud.PaidPeriod {
		return ""
	}
	return b.day(out)
}

// eventView is a message about something that happened, with its button if it
// has one.
func (b *Bot) eventView(e lifecycle.Event) view {
	a := e.Account
	switch {
	case e.Kind == lifecycle.EventPayment && a != nil:
		return b.paymentReminder(*a)
	case e.Kind == lifecycle.EventPaid && a != nil:
		return view{text: fmt.Sprintf("Оплата прошла: сервер оплачен до %s.", b.stamp(a.NextPayment))}
	case e.Kind == lifecycle.EventQuota && a != nil:
		if e.Percent >= 100 {
			return view{text: "Лимит трафика исчерпан: " + usedOf(*a) + ". Хостер может снизить скорость."}
		}
		text := fmt.Sprintf("Трафик: %d%% лимита, %s.", e.Percent, usedOf(*a))
		if out := b.runsOut(*a, e.CreatedAt); out != "" {
			text += " При таком темпе кончится " + out + "."
		}
		return view{text: text}
	case e.Kind == lifecycle.EventHeavy && e.Heavy != nil:
		return heavyView(*e.Heavy)
	}
	return view{text: eventText(e)}
}

func (b *Bot) paymentReminder(a cloud.Account) view {
	when := fmt.Sprintf("Через %s, %s, кончается оплаченный месяц сервера.", left(a.NextPayment.Sub(b.now())), b.stamp(a.NextPayment))
	if !lifecycle.NeedsPayment(a) {
		return view{text: fmt.Sprintf("Через %s, %s, месяц сервера продлится сам.", left(a.NextPayment.Sub(b.now())), b.stamp(a.NextPayment))}
	}
	text := when + "\nАвтопродления нет: без оплаты сервер выключится."
	if a.AutoRenew {
		text = when + "\nНа автопродление не хватит денег."
	}
	if a.HasBalance && a.Price > 0 {
		text += fmt.Sprintf(" Баланс %s, месяц %s.", money(a.Balance, a.Currency), money(a.Price, a.Currency))
	}
	var kb telegram.Keyboard
	if url := providerPortals[b.Manager.Snapshot().Provider]; url != "" {
		kb = telegram.Keyboard{{telegram.Button{Text: "Оплатить", URL: url}}}
	}
	return view{text: text, kb: kb}
}
