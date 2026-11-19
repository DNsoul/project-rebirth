package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/lifecycle"
)

var tomsk = time.FixedZone("TOMSK", 7*3600)

// cherryRig is the rig on a monthly Cherry server, with the owners in Tomsk.
func cherryRig(t *testing.T, a cloud.Account) *rig {
	r := newRig(t)
	r.bot.Zone = tomsk
	r.mgr.set(func(s *lifecycle.Snapshot) {
		s.Provider, s.Region, s.Billing = "cherry", "ams", cloud.Monthly
		s.CreatedAt = r.now.Add(-7 * 24 * time.Hour)
		s.Account = &a
		s.PaidUntil = a.NextPayment
	})
	return r
}

func dueIn(r *rig, d time.Duration) cloud.Account {
	return cloud.Account{
		NextPayment: r.now.Add(d), Price: 3.63, Currency: "EUR", Balance: 7.36, HasBalance: true,
		TrafficUsed: 420e9, TrafficLimit: 1e12,
	}
}

func TestTheServerSaysUntilWhenItIsPaidHowAndWhatTrafficIsLeft(t *testing.T) {
	r := newRig(t)
	a := dueIn(r, 23*24*time.Hour)
	r = cherryRig(t, a)
	text := r.bot.serverView().text
	paid := a.NextPayment.In(tomsk).Format("02.01 15:04")
	out := r.now.Add(time.Duration(580.0 / 60 * 24 * float64(time.Hour))).In(tomsk).Format("02.01")
	for _, want := range []string{
		"Оплачен    до " + paid, "Продление  вручную", "Трафик     420 из 1000 ГБ", "Хватит до  " + out,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("server screen lacks %q:\n%s", want, text)
		}
	}

	a.AutoRenew = true
	r = cherryRig(t, a)
	if text := r.bot.serverView().text; !strings.Contains(text, "Продление  само") {
		t.Errorf("auto renewal not said:\n%s", text)
	}
	a.Balance = 1
	r = cherryRig(t, a)
	if text := r.bot.serverView().text; !strings.Contains(text, "Продление  не хватает денег") {
		t.Errorf("shortage not said:\n%s", text)
	}

	// Little used: no date it runs out within the month.
	a.TrafficUsed = 10e9
	r = cherryRig(t, a)
	if text := r.bot.serverView().text; !strings.Contains(text, "Трафик     10 из 1000 ГБ") || strings.Contains(text, "Хватит до") {
		t.Errorf("light use:\n%s", text)
	}
}

func TestThePaymentReminderIsShortAndHasOneButtonToPay(t *testing.T) {
	r := newRig(t)
	a := dueIn(r, 7*24*time.Hour-3*time.Hour)
	r = cherryRig(t, a)
	v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventPayment, Account: &a})
	want := "Через 7 дней, " + a.NextPayment.In(tomsk).Format("02.01 15:04") + ", кончается оплаченный месяц сервера.\n" +
		"Автопродления нет: без оплаты сервер выключится. Баланс 7,36 €, месяц 3,63 €."
	if v.text != want {
		t.Errorf("reminder:\n%s\nwant\n%s", v.text, want)
	}
	if len(v.kb) != 1 || len(v.kb[0]) != 1 || v.kb[0][0].URL != "https://portal.cherryservers.com/" || v.kb[0][0].Data != "" {
		t.Errorf("buttons = %+v", v.kb)
	}

	last := dueIn(r, 20*time.Hour)
	if v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventPayment, Account: &last}); !strings.HasPrefix(v.text, "Через 20 часов, ") {
		t.Errorf("last day: %q", v.text)
	}

	self := dueIn(r, 3*24*time.Hour)
	self.AutoRenew = true
	if v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventPayment, Account: &self}); !strings.HasSuffix(v.text, "месяц сервера продлится сам.") || v.kb != nil {
		t.Errorf("self-renewing: %q %+v", v.text, v.kb)
	}
	self.Balance = 2
	if v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventPayment, Account: &self}); !strings.Contains(v.text, "На автопродление не хватит денег. Баланс 2,00 €") {
		t.Errorf("short for renewal: %q", v.text)
	}
}

func TestPaidAndTrafficEventsAreOneLine(t *testing.T) {
	r := newRig(t)
	a := dueIn(r, 30*24*time.Hour)
	r = cherryRig(t, a)
	if v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventPaid, Account: &a}); v.text != "Оплата прошла: сервер оплачен до "+a.NextPayment.In(tomsk).Format("02.01 15:04")+"." {
		t.Errorf("paid: %q", v.text)
	}

	a.TrafficUsed = 720e9
	created := r.now.Add(-12 * 24 * time.Hour)
	v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventQuota, Account: &a, Percent: 72, CreatedAt: created})
	if !strings.HasPrefix(v.text, "Трафик: 72% лимита, 720 из 1000 ГБ. При таком темпе кончится ") || strings.Contains(v.text, "\n") {
		t.Errorf("quota: %q", v.text)
	}
	a.TrafficUsed = 1010e9
	if v := r.bot.eventView(lifecycle.Event{Kind: lifecycle.EventQuota, Account: &a, Percent: 101}); v.text != "Лимит трафика исчерпан: 1010 из 1000 ГБ. Хостер может снизить скорость." {
		t.Errorf("spent: %q", v.text)
	}
}

func TestAPaymentReminderGoesOutWithItsButton(t *testing.T) {
	r := newRig(t)
	a := dueIn(r, 3*24*time.Hour)
	r = cherryRig(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.bot.watch(ctx)
	r.bot.Notify(lifecycle.Event{Kind: lifecycle.EventPayment, Account: &a})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sends, _, _ := r.api.snapshot(); len(sends) == 1 {
			if sends[0].kb == nil || sends[0].kb[0][0].URL == "" || !strings.Contains(sends[0].text, "Через 3 дня") {
				t.Fatalf("sent %+v", sends[0])
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the reminder was not sent")
}

func TestWordsForMoneyAndTime(t *testing.T) {
	for n, want := range map[int]string{1: "день", 2: "дня", 4: "дня", 5: "дней", 11: "дней", 12: "дней", 21: "день", 22: "дня", 25: "дней"} {
		if got := plural(n, "день", "дня", "дней"); got != want {
			t.Errorf("plural(%d) = %s, want %s", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		7*24*time.Hour - 3*time.Hour: "7 дней", 60 * time.Hour: "3 дня", 35 * time.Hour: "35 часов",
		21 * time.Hour: "21 час", 10 * time.Minute: "1 час",
	} {
		if got := left(d); got != want {
			t.Errorf("left(%v) = %s, want %s", d, got, want)
		}
	}
	if got := money(7.36, "EUR"); got != "7,36 €" {
		t.Errorf("money = %q", got)
	}
	if got := money(5, "GBP"); got != "5,00 GBP" {
		t.Errorf("money = %q", got)
	}
}
