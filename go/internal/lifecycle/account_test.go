package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"awgrotator/internal/cloud"
)

type fakeAccounts struct {
	a     cloud.Account
	err   error
	calls int
}

func (f *fakeAccounts) Account(context.Context, string, string) (cloud.Account, error) {
	f.calls++
	return f.a, f.err
}

var msk = time.FixedZone("MSK", 3*3600)

// payDay is a payment due at 15:00 Moscow time, so that "seven days before"
// and its neighbours fall in the owners' daytime.
var payDay = time.Date(2027, 3, 20, 15, 0, 0, 0, msk)

func unpaid() cloud.Account {
	return cloud.Account{NextPayment: payDay.UTC(), Price: 3.63, Currency: "EUR", Balance: 7.36, HasBalance: true}
}

// accountRig is a manager that is the current server and asks a fake
// provider; pass looks at the account the way a tick does.
type accountRig struct {
	*rig
	m      *Manager
	src    *fakeAccounts
	events *[]Event
}

func newAccountRig(t *testing.T, a cloud.Account) *accountRig {
	r := newRig(t)
	src := &fakeAccounts{a: a}
	m := r.manager()
	m.Accounts = src
	m.current = true
	return &accountRig{rig: r, m: m, src: src, events: collect(m)}
}

func (r *accountRig) at(t time.Time) {
	r.clock.Set(t)
	r.m.maybeAccount(context.Background(), t)
	r.m.noteAccount(t)
}

func (r *accountRig) count(kind EventKind) int {
	n := 0
	for _, e := range *r.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// restart is the manager starting again over the same journal.
func (r *accountRig) restart() {
	m := r.manager()
	m.Accounts, m.current = r.src, true
	m.OnEvent = r.m.OnEvent
	r.m = m
}

func TestAPaymentIsRemindedOnceAtSevenThreeAndOneDayInTheDaytime(t *testing.T) {
	r := newAccountRig(t, unpaid())

	r.at(payDay.Add(-10 * 24 * time.Hour))
	if n := r.count(EventPayment); n != 0 {
		t.Fatalf("reminded %d times ten days ahead", n)
	}

	r.at(payDay.Add(-7*24*time.Hour + time.Hour))
	r.at(payDay.Add(-7*24*time.Hour + 2*time.Hour))
	r.restart()
	r.at(payDay.Add(-7*24*time.Hour + 3*time.Hour))
	if n := r.count(EventPayment); n != 1 {
		t.Fatalf("seven days ahead: reminded %d times, want once across a restart", n)
	}
	if e := (*r.events)[0]; e.Account == nil || !e.Account.NextPayment.Equal(payDay) {
		t.Errorf("the reminder does not carry the payment: %+v", e)
	}

	// Three days ahead falls at 15:00; seen at night it waits for the morning.
	night := time.Date(2027, 3, 17, 23, 30, 0, 0, msk)
	r.at(night)
	if n := r.count(EventPayment); n != 1 {
		t.Fatalf("a reminder was sent at night")
	}
	r.at(time.Date(2027, 3, 18, 9, 5, 0, 0, msk))
	if n := r.count(EventPayment); n != 2 {
		t.Fatalf("the morning after: %d reminders, want 2", n)
	}

	r.at(payDay.Add(-23 * time.Hour))
	r.at(payDay.Add(-2 * time.Hour))
	if n := r.count(EventPayment); n != 3 {
		t.Fatalf("the last day: %d reminders, want 3", n)
	}
}

func TestAManagerThatStartsTwoDaysAheadSendsOneReminderNotThree(t *testing.T) {
	r := newAccountRig(t, unpaid())
	r.at(payDay.Add(-2 * 24 * time.Hour))
	if n := r.count(EventPayment); n != 1 {
		t.Fatalf("%d reminders, want 1", n)
	}
	r.at(payDay.Add(-20 * time.Hour))
	if n := r.count(EventPayment); n != 2 {
		t.Fatalf("the last day: %d reminders, want 2", n)
	}
}

func TestAPeriodThatPaysItselfIsMentionedOnceThreeDaysAhead(t *testing.T) {
	a := unpaid()
	a.AutoRenew = true
	r := newAccountRig(t, a)
	r.at(payDay.Add(-6 * 24 * time.Hour))
	r.at(payDay.Add(-60 * time.Hour))
	r.at(payDay.Add(-20 * time.Hour))
	if n := r.count(EventPayment); n != 1 {
		t.Fatalf("%d reminders, want 1", n)
	}

	// Renewing by itself from a balance that does not cover it needs the
	// operator after all.
	short := unpaid()
	short.AutoRenew, short.Balance = true, 1
	r2 := newAccountRig(t, short)
	r2.at(payDay.Add(-7*24*time.Hour + time.Hour))
	if n := r2.count(EventPayment); n != 1 {
		t.Fatalf("short of money: %d reminders seven days ahead, want 1", n)
	}
}

func TestPayingIsNoticedWhenTheProvidersDateMovesOn(t *testing.T) {
	r := newAccountRig(t, unpaid())
	r.at(payDay.Add(-5 * 24 * time.Hour))
	if n := r.count(EventPaid); n != 0 {
		t.Fatal("the first look was taken for a payment")
	}

	paid := unpaid()
	paid.NextPayment = payDay.Add(30 * 24 * time.Hour).UTC()
	paid.Balance = 3.73
	r.src.a = paid
	r.at(payDay.Add(-5*24*time.Hour + 2*time.Hour))
	if n := r.count(EventPaid); n != 1 {
		t.Fatalf("paid: told %d times, want 1", n)
	}
	r.restart()
	r.at(payDay.Add(-5*24*time.Hour + 4*time.Hour))
	if n := r.count(EventPaid); n != 1 {
		t.Fatalf("after a restart: told %d times, want 1", n)
	}
	if s := (func() Snapshot { r.m.publishSnapshot(context.Background()); return r.m.Snapshot() })(); !s.PaidUntil.Equal(paid.NextPayment) {
		t.Errorf("the status says paid until %v, the provider %v", s.PaidUntil, paid.NextPayment)
	}

	// A little shift of the time of day is not a payment.
	shifted := paid
	shifted.NextPayment = paid.NextPayment.Add(2 * time.Minute)
	r.src.a = shifted
	r.at(payDay.Add(-5*24*time.Hour + 6*time.Hour))
	if n := r.count(EventPaid); n != 1 {
		t.Fatalf("a shift of minutes was told as a payment")
	}
}

func TestTrafficStepsAreToldOnceAndAgainAfterTheUseFalls(t *testing.T) {
	a := cloud.Account{TrafficLimit: 1000}
	r := newAccountRig(t, a)
	noon := time.Date(2027, 3, 1, 12, 0, 0, 0, msk)
	look := func(used int64, hours int) {
		r.src.a.TrafficUsed = used
		r.at(noon.Add(time.Duration(hours) * time.Hour))
	}
	percents := func() []int {
		var out []int
		for _, e := range *r.events {
			if e.Kind == EventQuota {
				out = append(out, e.Percent)
			}
		}
		return out
	}

	look(500, 0)
	look(720, 2)
	look(750, 4)
	look(950, 6)
	look(1010, 8)
	if got := percents(); len(got) != 3 || got[0] != 72 || got[1] != 95 || got[2] != 101 {
		t.Fatalf("told %v, want [72 95 101]", got)
	}
	look(500, 24) // the rolling thirty days let go of a heavy day
	look(710, 26)
	if got := percents(); len(got) != 4 || got[3] != 71 {
		t.Fatalf("after falling and rising again: told %v", got)
	}

	// Found at 95% straight away: one word, not two.
	r2 := newAccountRig(t, cloud.Account{TrafficLimit: 1000, TrafficUsed: 950})
	r2.at(noon)
	if n := r2.count(EventQuota); n != 1 {
		t.Fatalf("found at 95%%: told %d times", n)
	}
}

func TestOnlyTheServerTheAppsUseRemindsAndAProviderThatDoesNotTellIsLeftAlone(t *testing.T) {
	r := newAccountRig(t, unpaid())
	r.m.current = false
	r.at(payDay.Add(-2 * 24 * time.Hour))
	if len(*r.events) != 0 {
		t.Fatalf("a server that has handed over spoke: %+v", *r.events)
	}

	r2 := newAccountRig(t, cloud.Account{})
	r2.src.err = cloud.ErrNoAccount
	r2.at(payDay)
	r2.at(payDay.Add(time.Minute))
	r2.at(payDay.Add(11 * time.Minute))
	if r2.src.calls != 2 || len(*r2.events) != 0 {
		t.Fatalf("calls %d, events %+v", r2.src.calls, *r2.events)
	}
	if s := r2.m.Snapshot(); s.Account != nil {
		t.Errorf("snapshot has an account: %+v", s.Account)
	}

	r3 := newAccountRig(t, unpaid())
	r3.src.err = errors.New("timeout")
	r3.at(payDay.Add(-2 * 24 * time.Hour))
	if len(*r3.events) != 0 {
		t.Fatalf("a failed look produced %+v", *r3.events)
	}
}

func TestQuietHoursAreTheOwnersNight(t *testing.T) {
	tomsk := time.FixedZone("TOMSK", 7*3600)
	for hour, quiet := range map[int]bool{8: true, 9: false, 14: false, 21: false, 22: true, 0: true} {
		at := time.Date(2027, 1, 1, hour, 30, 0, 0, tomsk)
		if Quiet(at, tomsk) != quiet {
			t.Errorf("%02d:30 in Tomsk: quiet = %v", hour, !quiet)
		}
	}
	c := &Config{Telegram: &TelegramConfig{UTCOffset: "+07:00"}}
	if _, off := time.Unix(0, 0).In(c.OwnerZone()).Zone(); off != 7*3600 {
		t.Errorf("owner zone offset %d", off)
	}
	if _, off := time.Unix(0, 0).In((&Config{}).OwnerZone()).Zone(); off != 3*3600 {
		t.Errorf("default zone offset %d, want Moscow", off)
	}
	if err := (&TelegramConfig{Token: "t", Owners: []int64{1}, UTCOffset: "Tomsk"}).validate(); err == nil {
		t.Error("an offset that is not one was accepted")
	}
}

func TestTheForecastJudgesThePaceSinceTheServerWasMade(t *testing.T) {
	created := time.Date(2026, 11, 12, 17, 54, 0, 0, time.UTC)
	now := created.Add(7 * 24 * time.Hour)
	a := cloud.Account{TrafficUsed: 420e9, TrafficLimit: 1e12}
	perDay, out := QuotaForecast(a, created, now)
	if perDay < 59.9e9 || perDay > 60.1e9 {
		t.Errorf("per day %.0f, want 60 GB", perDay)
	}
	if want := now.Add(time.Duration(580.0 / 60 * 24 * float64(time.Hour))); out.Sub(want).Abs() > time.Minute {
		t.Errorf("runs out %v, want %v", out, want)
	}
	if QuotaPercent(a) != 42 || QuotaPercent(cloud.Account{}) != -1 {
		t.Error("percent")
	}
	if _, out := QuotaForecast(cloud.Account{TrafficUsed: 2e12, TrafficLimit: 1e12}, created, now); !out.IsZero() {
		t.Error("a spent allowance has a date it runs out")
	}
}
