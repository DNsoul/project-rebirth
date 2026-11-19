package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"awgrotator/internal/cloud"
)

// Money and traffic. A server paid by the month at a provider without a card
// on the account is not renewed by itself: if nobody pays by hand it stops at
// the end of the period, and every device loses the tunnel at once. The
// allowance of traffic is just as hard a wall, with the speed cut when it is
// spent. The manager asks the provider about both once an hour and reminds
// the operator in good time, in their daytime, once per step.

// AccountSource tells how a server is paid for; cloud.Set is one.
type AccountSource interface {
	Account(ctx context.Context, provider, id string) (cloud.Account, error)
}

// DefaultAccountEvery is how often the provider is asked.
const DefaultAccountEvery = time.Hour

// accountRetry is how soon a failed first look is repeated.
const accountRetry = 10 * time.Minute

// PaymentReminders are how long before a payment the operator is reminded,
// most distant first.
var PaymentReminders = []time.Duration{7 * 24 * time.Hour, 3 * 24 * time.Hour, 24 * time.Hour}

// QuotaSteps are the shares of the traffic allowance, in percent, at which the
// operator is told, lowest first. A step is told again only after the use has
// fallen QuotaRearm points below it: the allowance is counted over a rolling
// thirty days, so it does fall.
var QuotaSteps = []int{70, 90, 100}

// QuotaRearm is how far below a step the use must fall before it is told again.
const QuotaRearm = 10

// Quiet hours on the owners' clock: reminders found due at night wait for the
// morning. Nothing in them needs acting on within hours.
const (
	quietFrom = 22
	quietTill = 9
)

// Quiet reports whether t falls in the owners' night.
func Quiet(t time.Time, loc *time.Location) bool {
	h := t.In(loc).Hour()
	return h >= quietFrom || h < quietTill
}

// QuotaForecast is the average use per day and when the allowance runs out at
// that pace, judged from how long the server has been counting: since it was
// created, or the provider's thirty days once it is older. The day is zero when
// the allowance is not known, already spent, or nothing is being used.
func QuotaForecast(a cloud.Account, created, now time.Time) (perDay float64, runsOut time.Time) {
	window := now.Sub(created)
	if window > cloud.PaidPeriod {
		window = cloud.PaidPeriod
	}
	if window < time.Hour || a.TrafficUsed <= 0 {
		return 0, time.Time{}
	}
	perDay = float64(a.TrafficUsed) / window.Hours() * 24
	if a.TrafficLimit <= 0 || a.TrafficUsed >= a.TrafficLimit {
		return perDay, time.Time{}
	}
	days := float64(a.TrafficLimit-a.TrafficUsed) / perDay
	return perDay, now.Add(time.Duration(days * 24 * float64(time.Hour)))
}

// QuotaPercent is the share of the allowance used, or -1 when it is not known.
func QuotaPercent(a cloud.Account) int {
	if a.TrafficLimit <= 0 {
		return -1
	}
	return int(a.TrafficUsed * 100 / a.TrafficLimit)
}

// NeedsPayment says the operator has to act for the next period to be paid:
// the provider will not renew by itself, or the balance does not cover it.
func NeedsPayment(a cloud.Account) bool {
	return !a.AutoRenew || ShortOfMoney(a)
}

// ShortOfMoney says the balance is known and does not cover one period.
func ShortOfMoney(a cloud.Account) bool {
	return a.HasBalance && a.Price > 0 && a.Balance < a.Price
}

// maybeAccount asks the provider about money and traffic when it is time.
func (m *Manager) maybeAccount(ctx context.Context, now time.Time) {
	if m.Accounts == nil || m.Cfg.InstanceID == "" {
		return
	}
	every := m.Every.Account
	if m.account == nil {
		every = min(every, accountRetry)
	}
	if !m.lastAccountTry.IsZero() && now.Sub(m.lastAccountTry) < every {
		return
	}
	m.lastAccountTry = now

	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a, err := m.Accounts.Account(actx, m.Cfg.Provider, m.Cfg.InstanceID)
	if err != nil {
		if !errors.Is(err, cloud.ErrNoAccount) {
			m.log().Debug("could not ask the provider about money and traffic", "error", err)
		}
		return
	}
	m.account, m.accountAt = &a, now
	m.notePaid(a)
}

// notePaid tells the operator when the provider's next payment date has moved
// on: the period was paid. It is said at once, whatever the hour, because the
// operator has just done it.
func (m *Manager) notePaid(a cloud.Account) {
	if a.NextPayment.IsZero() {
		return
	}
	next := a.NextPayment.Unix()
	seen := m.J.Read().PaidSeen
	if seen == next {
		return
	}
	if err := m.J.Update(func(j *Journal) { j.PaidSeen = next }); err != nil {
		m.log().Warn("could not record the next payment date", "error", err)
		return
	}
	// A day's slack: providers shift the time of day a little.
	if seen != 0 && next > seen+24*3600 && m.current {
		m.log().Info("the server's next period has been paid", "next_payment", a.NextPayment.Format(time.RFC3339))
		m.emit(Event{Kind: EventPaid, Account: &a})
	}
}

// noteAccount sends the reminders that have come due. Only the server the apps
// use speaks, and only in the owners' daytime; a reminder that waits for the
// morning is sent then, once.
func (m *Manager) noteAccount(now time.Time) {
	a := m.account
	if a == nil || !m.current || Quiet(now, m.Cfg.OwnerZone()) {
		return
	}
	m.notePayment(now, *a)
	m.noteQuota(*a)
}

func (m *Manager) notePayment(now time.Time, a cloud.Account) {
	if a.NextPayment.IsZero() {
		return
	}
	left := a.NextPayment.Sub(now)
	if left <= 0 {
		return
	}
	// The nearest step already reached. One reminder for it, and every step
	// further away counts as told: a manager that starts two days before the
	// payment sends one reminder, not three.
	reached := -1
	for i, step := range PaymentReminders {
		if left <= step {
			reached = i
		}
	}
	if reached < 0 {
		return
	}
	// A period that pays itself, from a balance that covers it, is worth one
	// word three days ahead and no more.
	if !NeedsPayment(a) && PaymentReminders[reached] > 3*24*time.Hour {
		return
	}
	key := func(step time.Duration) string {
		return fmt.Sprintf("pay:%s:%d:%dh", m.Cfg.InstanceID, a.NextPayment.Unix(), int(step.Hours()))
	}
	j := m.J.Read()
	if j.Noticed(key(PaymentReminders[reached])) {
		return
	}
	var keys []string
	for _, step := range PaymentReminders[:reached+1] {
		keys = append(keys, key(step))
	}
	if !NeedsPayment(a) {
		keys = nil
		for _, step := range PaymentReminders {
			keys = append(keys, key(step))
		}
	}
	if err := m.J.Update(func(j *Journal) { j.MarkNoticed(keys...) }); err != nil {
		m.log().Warn("could not record a payment reminder; not sending it", "error", err)
		return
	}
	m.log().Info("reminding the operator of the next payment", "left_hours", int(left.Hours()), "auto_renew", a.AutoRenew)
	m.emit(Event{Kind: EventPayment, Account: &a})
}

func (m *Manager) noteQuota(a cloud.Account) {
	pct := QuotaPercent(a)
	if pct < 0 {
		return
	}
	key := func(step int) string { return fmt.Sprintf("quota:%s:%d", m.Cfg.InstanceID, step) }
	j := m.J.Read()
	var rearm []string
	reached := -1
	for i, step := range QuotaSteps {
		if pct < step-QuotaRearm && j.Noticed(key(step)) {
			rearm = append(rearm, key(step))
		}
		if pct >= step {
			reached = i
		}
	}
	tell := reached >= 0 && !j.Noticed(key(QuotaSteps[reached]))
	if len(rearm) == 0 && !tell {
		return
	}
	var keys []string
	if tell {
		for _, step := range QuotaSteps[:reached+1] {
			keys = append(keys, key(step))
		}
	}
	if err := m.J.Update(func(j *Journal) {
		for _, k := range rearm {
			j.Unnotice(k)
		}
		j.MarkNoticed(keys...)
	}); err != nil {
		m.log().Warn("could not record a traffic reminder; not sending it", "error", err)
		return
	}
	if tell {
		m.log().Info("telling the operator about the traffic allowance", "percent", pct)
		m.emit(Event{Kind: EventQuota, Account: &a, Percent: pct})
	}
}
