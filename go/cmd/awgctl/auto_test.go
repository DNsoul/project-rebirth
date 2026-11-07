package main

import (
	"testing"

	"awgrotator/internal/lifecycle"
)

func ptr(b bool) *bool { return &b }

func TestAutoChangesOnlyWhatWasGiven(t *testing.T) {
	p := lifecycle.DefaultPolicy()
	before := p
	if err := (policyChange{age: ptr(true)}).apply(&p); err != nil {
		t.Fatal(err)
	}
	if !p.AutoAge {
		t.Error("age moves were not switched on")
	}
	p.AutoAge = false
	if p != before {
		t.Errorf("something else changed: %+v vs %+v", p, before)
	}
}

func TestAutoSetsHoursAndLimitsTogether(t *testing.T) {
	p := lifecycle.DefaultPolicy()
	err := policyChange{
		age: ptr(true), ageHours: 10, jitterHours: 2, jitterSet: true,
		maxMovesPerDay: 5, minMoveMinutes: 240,
	}.apply(&p)
	if err != nil {
		t.Fatal(err)
	}
	if !p.AutoAge || p.MaxAgeHours != 10 || p.AgeJitterHours != 2 || p.MaxMovesPerDay != 5 || p.MinMoveMinutes != 240 {
		t.Errorf("policy = %+v", p)
	}
}

func TestAutoCanSwitchTheJitterOff(t *testing.T) {
	p := lifecycle.DefaultPolicy()
	if err := (policyChange{jitterHours: 0, jitterSet: true}).apply(&p); err != nil {
		t.Fatal(err)
	}
	if p.AgeJitterHours != 0 {
		t.Errorf("jitter = %d, want 0 (an explicit zero must not turn into the default)", p.AgeJitterHours)
	}
}

func TestAutoRefusesWhatTheManagerWouldRefuse(t *testing.T) {
	for name, c := range map[string]policyChange{
		"too many moves a day": {maxMovesPerDay: 13},
		"negative hours":       {ageHours: -1},
		"negative jitter":      {jitterHours: -3, jitterSet: false, maxMovesPerDay: -1},
	} {
		p := lifecycle.DefaultPolicy()
		if err := c.apply(&p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAutoWithNothingGivenChangesNothing(t *testing.T) {
	if (policyChange{}).any() {
		t.Error("an empty request counts as a change, so the manager would be restarted for nothing")
	}
	if !(policyChange{ageHours: 10}).any() || !(policyChange{jitterSet: true}).any() {
		t.Error("a given value does not count as a change")
	}
}

func TestShorterGraceKeepsTheMinimumBelowIt(t *testing.T) {
	p := lifecycle.DefaultPolicy()
	if err := (policyChange{graceMinutes: 3}).apply(&p); err != nil {
		t.Fatal(err)
	}
	if p.MinGraceMinutes > p.GraceMinutes {
		t.Errorf("minimum grace %d is above grace %d", p.MinGraceMinutes, p.GraceMinutes)
	}
}
