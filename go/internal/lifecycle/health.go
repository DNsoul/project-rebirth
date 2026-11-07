package lifecycle

// Verdict is the server's opinion of its own health.
type Verdict string

const (
	// Healthy means nothing is wrong, or nothing is known to be.
	Healthy Verdict = "healthy"

	// Blocked means the server answers from abroad but not from the target
	// country, several checks in a row.
	Blocked Verdict = "blocked"

	// Broken means the server's own services would not stay up.
	Broken Verdict = "broken"
)

// Reach is how many of a set of probes could reach the server.
type Reach struct {
	Reached int
	Total   int
}

// Fraction returns the share that reached it, or zero with no probes.
func (r Reach) Fraction() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Reached) / float64(r.Total)
}

// Round is how one external check came out.
type Round int

const (
	// Inconclusive means the check says nothing either way: the probes did not
	// run, or the control group failed too, which points at the measurement
	// rather than at the server.
	Inconclusive Round = iota

	Good
	Bad
)

// Thresholds. The control group must be largely healthy for a bad result in
// the target country to mean anything, and "bad" means nearly none reached.
const (
	controlMinimum = 0.6
	blockedAtMost  = 0.2

	// localFailuresForBroken is how many local checks in a row, each after a
	// restart, must fail before the server calls itself broken.
	localFailuresForBroken = 3
)

// Classify turns one pair of measurements into a round.
//
// The control group is what separates a blocked server from a broken
// measurement: if probes in other countries cannot reach it either, nothing
// is known about the target country.
func Classify(target, control Reach) Round {
	if target.Total == 0 || control.Total == 0 {
		return Inconclusive
	}
	if control.Fraction() < controlMinimum {
		return Inconclusive
	}
	if target.Fraction() <= blockedAtMost {
		return Bad
	}
	return Good
}

// Tracker accumulates checks into a verdict, and does not flip on one bad
// round: a blocked verdict needs several in a row, and a single good round
// clears it.
type Tracker struct {
	needed int

	badRounds      int
	localFailures  int
	lastExternalOK bool
	seenExternal   bool
}

// NewTracker returns a tracker that believes "blocked" after the given number
// of consecutive bad external rounds.
func NewTracker(blockRounds int) *Tracker {
	if blockRounds < 1 {
		blockRounds = 1
	}
	return &Tracker{needed: blockRounds}
}

// External records one external check.
func (t *Tracker) External(r Round) {
	switch r {
	case Good:
		t.badRounds = 0
		t.lastExternalOK, t.seenExternal = true, true
	case Bad:
		t.badRounds++
		t.lastExternalOK, t.seenExternal = false, true
	case Inconclusive:
		// Leaves everything as it was: not knowing is not evidence.
	}
}

// Local records whether the server's own services were up on a local check.
func (t *Tracker) Local(ok bool) {
	if ok {
		t.localFailures = 0
		return
	}
	t.localFailures++
}

// Verdict returns the current opinion. A broken server outranks a blocked one:
// there is no point judging reachability of something that is not running.
func (t *Tracker) Verdict() Verdict {
	switch {
	case t.localFailures >= localFailuresForBroken:
		return Broken
	case t.badRounds >= t.needed:
		return Blocked
	}
	return Healthy
}

// BadRounds returns how many bad external rounds in a row there have been.
func (t *Tracker) BadRounds() int { return t.badRounds }
