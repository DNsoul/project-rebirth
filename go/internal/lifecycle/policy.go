package lifecycle

import (
	"fmt"
	"time"
)

// Trigger is why a move happens.
type Trigger string

const (
	TriggerCommand Trigger = "command" // the operator asked
	TriggerAge     Trigger = "age"     // the server got old
	TriggerBlocked Trigger = "blocked" // it looks unreachable from the target country
	TriggerBroken  Trigger = "broken"  // its own services will not stay up
)

// MoveRequest is a decision to replace the server.
type MoveRequest struct {
	Trigger Trigger

	// Region is where the operator asked the new server to be, or empty for
	// any region other than this one.
	Region string
}

// Inputs is everything a decision depends on. It is gathered by the caller so
// that the decision itself is a pure function of it.
type Inputs struct {
	Now       time.Time
	CreatedAt time.Time

	// AgeJitter is the random extra added to the age limit, drawn once.
	AgeJitter time.Duration

	// Attempts are the times of earlier move attempts, whatever their outcome.
	Attempts []time.Time

	Health Verdict

	// Command is a move the operator has asked for and that has not yet been
	// carried out. Nil when there is none.
	Command *MoveRequest
}

// Decision says whether to move, and if not, why not.
type Decision struct {
	Move    bool
	Request MoveRequest

	// Reason is for the log and the status: why it moves, or why it stays.
	Reason string
}

// Decide applies the policy.
//
// An operator's command always goes ahead: the limits exist to stop the server
// from running away with the account, not to second-guess a person. Automatic
// triggers are off unless enabled, and even when on they are rate limited, so
// a fault that keeps recurring cannot turn into a loop of paid servers.
func Decide(p Policy, in Inputs) Decision {
	if in.Command != nil {
		return Decision{Move: true, Request: *in.Command, Reason: "the operator asked for it"}
	}

	trigger, why := autoTrigger(p, in)
	if trigger == "" {
		return Decision{Reason: why}
	}

	if wait := waitBeforeNextAttempt(p, in); wait > 0 {
		return Decision{Reason: fmt.Sprintf("%s, but the last attempt was too recent: wait %s",
			why, wait.Round(time.Minute))}
	}
	if n := attemptsInLast(in.Attempts, in.Now, 24*time.Hour); n >= p.MaxMovesPerDay {
		return Decision{Reason: fmt.Sprintf("%s, but %d attempts have already been made today", why, n)}
	}
	return Decision{Move: true, Request: MoveRequest{Trigger: trigger}, Reason: why}
}

// autoTrigger says which automatic condition applies, if any.
func autoTrigger(p Policy, in Inputs) (Trigger, string) {
	age := in.Now.Sub(in.CreatedAt)
	limit := p.MaxAge() + in.AgeJitter

	switch {
	case p.AutoAge && age >= limit:
		return TriggerAge, fmt.Sprintf("it is %s old, past its limit of %s", roundDays(age), roundDays(limit))
	case p.AutoBlock && in.Health == Blocked:
		return TriggerBlocked, "it looks unreachable from the target country"
	case p.AutoBlock && in.Health == Broken:
		return TriggerBroken, "its own services will not stay up"
	}

	if !p.AutoAge && !p.AutoBlock {
		return "", "automatic moves are off"
	}
	return "", "nothing calls for a move"
}

// waitBeforeNextAttempt returns how much longer to hold off, or zero.
func waitBeforeNextAttempt(p Policy, in Inputs) time.Duration {
	var last time.Time
	for _, t := range in.Attempts {
		if t.After(last) {
			last = t
		}
	}
	if last.IsZero() {
		return 0
	}
	if wait := p.MinMoveInterval() - in.Now.Sub(last); wait > 0 {
		return wait
	}
	return 0
}

func attemptsInLast(attempts []time.Time, now time.Time, window time.Duration) int {
	n := 0
	for _, t := range attempts {
		if now.Sub(t) < window && !t.After(now) {
			n++
		}
	}
	return n
}

// NextAutoMove returns when the age trigger will fire, for the status. The
// second value is false when automatic moves by age are off.
func NextAutoMove(p Policy, createdAt time.Time, jitter time.Duration) (time.Time, bool) {
	if !p.AutoAge {
		return time.Time{}, false
	}
	return createdAt.Add(p.MaxAge() + jitter), true
}

func roundDays(d time.Duration) string {
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}
