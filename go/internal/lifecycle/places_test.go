package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/cloud"
)

// withCherry adds Cherry to the rig's line: Amsterdam and Šiauliai, paid by
// the month, next to the four Vultr cities paid by the hour.
func withCherry(r *rig) {
	r.cfg.Clouds["cherry"] = cloud.Settings{
		Key: "cherry-key", Plan: "B2-1-1gb-20s-shared", Image: "debian_13_64bit",
		Tag: "awg-rotator", Project: 77, SSHKeyID: "9",
	}
	r.cfg.Places = append(r.cfg.Places,
		PlaceConfig{ID: "cherry:ams", Billing: cloud.Monthly},
		PlaceConfig{ID: "cherry:sia", Billing: cloud.Monthly},
	)
}

func TestAVersionOneConfigIsReadAsTheCurrentOne(t *testing.T) {
	cfg := testConfig(t, newTestKeys(t))
	v := cfg.Clouds["vultr"]
	cfg.Version = 1
	cfg.LegacyVultrKey, cfg.LegacyPlan, cfg.LegacyTag, cfg.LegacySSHKeyID = v.Key, v.Plan, v.Tag, v.SSHKeyID
	cfg.LegacyOsID = 2625
	cfg.LegacyRegions = []string{"sto", "waw", "fra", "ams"}
	cfg.Clouds, cfg.Places, cfg.Provider, cfg.Billing = nil, nil, "", ""
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("a version 1 configuration was refused: %v", err)
	}
	if got.Version != ConfigVersion || got.Provider != "vultr" || got.Billing != cloud.Hourly {
		t.Errorf("version %d, provider %q, billing %q", got.Version, got.Provider, got.Billing)
	}
	if got.Clouds["vultr"] != v {
		t.Errorf("vultr settings = %+v, want %+v", got.Clouds["vultr"], v)
	}
	if strings.Join(got.PlaceIDs(), ",") != "vultr:sto,vultr:waw,vultr:fra,vultr:ams" {
		t.Errorf("places = %v", got.PlaceIDs())
	}
	for _, p := range got.Places {
		if p.Billing != cloud.Hourly {
			t.Errorf("%s billed %s", p.ID, p.Billing)
		}
	}

	out, err := got.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"vultr_key", "os_id", "regions", "plan", "tag", "ssh_key_id"} {
		if _, ok := top[old]; ok {
			t.Errorf("the version 1 field %s is still written", old)
		}
	}
	if strings.Count(string(out), `"key": "vultr-key"`) != 1 {
		t.Errorf("the Vultr key is not where it belongs:\n%s", out)
	}
}

func TestAutomaticMovesNeverGoToAMonthlyPlace(t *testing.T) {
	r := newRig(t)
	withCherry(r)
	if err := r.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, trig := range []Trigger{TriggerAge, TriggerBlocked, TriggerBroken} {
		for i := 0; i < 20; i++ {
			r.eng.Intn = func(n int) int { return i % n }
			got, err := r.eng.choosePlaces(MoveRequest{Trigger: trig})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range got {
				if p.Billing != cloud.Hourly {
					t.Fatalf("%s: an automatic move would go to %s, paid by the month", trig, p.ID)
				}
			}
		}
	}
	// "Move now" without a place is the operator's, but names no place: it
	// should not be the way €3 gets spent by surprise either.
	got, err := r.eng.choosePlaces(MoveRequest{Trigger: TriggerCommand})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.Billing == cloud.Monthly {
			t.Fatalf("a move to anywhere chose %s, paid by the month", p.ID)
		}
	}
}

func TestTheOperatorCanChooseAMonthlyPlace(t *testing.T) {
	r := newRig(t)
	withCherry(r)
	res := mustMove(t, r, MoveRequest{Trigger: TriggerCommand, Region: "cherry:ams"})
	req := r.prov.reqs[0]
	if req.Place != (cloud.Place{Provider: "cherry", Location: "ams"}) || req.Billing != cloud.Monthly {
		t.Fatalf("built %v billed %s", req.Place, req.Billing)
	}
	if res.Provider != "cherry" || res.Region != "ams" {
		t.Errorf("result = %+v", res)
	}

	// The successor knows where it is and how it is paid for.
	var next *Config
	for _, f := range r.prov.files {
		if f.Path == DefaultConfigPath {
			c, err := ParseConfig(f.Content)
			if err != nil {
				t.Fatal(err)
			}
			next = c
		}
	}
	if next == nil {
		t.Fatal("the successor got no configuration")
	}
	if next.Provider != "cherry" || next.Billing != cloud.Monthly || next.Region != "ams" {
		t.Errorf("successor: provider %q, billing %q, region %q", next.Provider, next.Billing, next.Region)
	}
	if next.PaidUntil().IsZero() {
		t.Error("a monthly successor does not know when its month ends")
	}
}

func TestABlockedServerLeavesForAnotherProviderFirst(t *testing.T) {
	r := newRig(t)
	r.cfg.Clouds["cherry"] = cloud.Settings{Key: "k", Plan: "p", Image: "i", Tag: "awg-rotator", Project: 77, SSHKeyID: "9"}
	r.cfg.Places = append(r.cfg.Places, PlaceConfig{ID: "cherry:ams", Billing: cloud.Hourly}, PlaceConfig{ID: "cherry:sia", Billing: cloud.Hourly})

	for i := 0; i < 10; i++ {
		r.eng.Intn = func(n int) int { return i % n }
		got, err := r.eng.choosePlaces(MoveRequest{Trigger: TriggerBlocked})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 5 || got[0].Place().Provider != "cherry" || got[1].Place().Provider != "cherry" {
			t.Fatalf("blocked at Vultr, the order is %v", got)
		}
	}
	// Getting old is no reason to prefer anyone: both providers come first
	// at some point.
	firsts := map[string]bool{}
	for i := 0; i < 10; i++ {
		r.eng.Intn = func(n int) int { return i % n }
		got, _ := r.eng.choosePlaces(MoveRequest{Trigger: TriggerAge})
		firsts[got[0].Place().Provider] = true
	}
	if len(firsts) != 2 {
		t.Errorf("moving by age, the first choice was always %v", firsts)
	}
}

func TestAPlaceWithNothingFreeIsPassedOverOnlyWhenThereIsAChoice(t *testing.T) {
	r := newRig(t)
	var sold cloud.Place
	r.prov.errFor = func(p cloud.Place) error {
		if sold == (cloud.Place{}) {
			sold = p // the first place tried has nothing free
		}
		if p == sold {
			return fmt.Errorf("vultr: %w", cloud.ErrNoCapacity)
		}
		return nil
	}

	// Automatic: the first place has nothing, the next one is used.
	var order []string
	res := mustMove(t, r, MoveRequest{Trigger: TriggerAge})
	for _, q := range r.prov.reqs {
		order = append(order, q.Place.String())
	}
	if res.Region == sold.Location || len(order) != 2 {
		t.Fatalf("went to %s after trying %v", res.Region, order)
	}

	// The operator's choice is not swapped for another.
	r2 := newRig(t)
	sold = cloud.Place{Provider: "vultr", Location: "waw"}
	r2.prov.errFor = r.prov.errFor
	_, err := r2.eng.Move(t.Context(), MoveRequest{Trigger: TriggerCommand, Region: "vultr:waw"})
	if !errors.Is(err, cloud.ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
	if len(r2.prov.reqs) != 1 {
		t.Errorf("tried %d places for one the operator named", len(r2.prov.reqs))
	}
}

func TestOnlyMonthlyPlacesLeaveNothingForAnAutomaticMove(t *testing.T) {
	r := newRig(t)
	withCherry(r)
	r.cfg.Places = r.cfg.Places[4:] // only Cherry, by the month
	r.cfg.Provider, r.cfg.Region, r.cfg.Billing = "cherry", "ams", cloud.Monthly
	if _, err := r.eng.choosePlaces(MoveRequest{Trigger: TriggerAge}); err == nil {
		t.Fatal("an automatic move found a monthly place to go to")
	}
	if got, err := r.eng.choosePlaces(MoveRequest{Trigger: TriggerCommand, Region: "sia"}); err != nil || got[0].ID != "cherry:sia" {
		t.Errorf("the operator's choice = %v, %v", got, err)
	}
}

func TestAMonthlyServerLeavesBeforeItsNextMonth(t *testing.T) {
	p := DefaultPolicy()
	p.AutoAge = true
	p.MaxAgeHours = 29 * 24
	created := t0
	paid := created.Add(cloud.PaidPeriod)
	jitter := 3 * 24 * time.Hour // the policy alone would allow 32 days

	in := Inputs{CreatedAt: created, AgeJitter: jitter, PaidUntil: paid, Health: Healthy}
	in.Now = paid.Add(-MonthlyMargin - time.Minute)
	if d := Decide(p, in); d.Move {
		t.Fatalf("moved %s before the margin: %s", paid.Sub(in.Now), d.Reason)
	}
	in.Now = paid.Add(-MonthlyMargin)
	if d := Decide(p, in); !d.Move || d.Request.Trigger != TriggerAge {
		t.Fatalf("did not leave %s before the month ends: %s", MonthlyMargin, d.Reason)
	}

	next, ok := NextAutoMove(p, created, jitter, paid)
	if !ok || !next.Equal(paid.Add(-MonthlyMargin)) {
		t.Errorf("next move %v, want %v", next, paid.Add(-MonthlyMargin))
	}
	// Paid by the hour, the policy alone decides.
	next, _ = NextAutoMove(p, created, jitter, time.Time{})
	if !next.Equal(created.Add(32 * 24 * time.Hour)) {
		t.Errorf("hourly next move %v", next)
	}
}

func TestFindPlacePrefersHourlyForABareCity(t *testing.T) {
	r := newRig(t)
	withCherry(r)
	if p, err := r.cfg.FindPlace("ams"); err != nil || p.ID != "vultr:ams" {
		t.Errorf("FindPlace(ams) = %v, %v", p, err)
	}
	if p, err := r.cfg.FindPlace("sia"); err != nil || p.ID != "cherry:sia" {
		t.Errorf("FindPlace(sia) = %v, %v", p, err)
	}
	if _, err := r.cfg.FindPlace("cherry:waw"); err == nil {
		t.Error("a place not on the list was found")
	}
}

func TestOldAttemptsWereAtVultr(t *testing.T) {
	if (Attempt{}).ProviderName() != "vultr" || (Attempt{Provider: "cherry"}).ProviderName() != "cherry" {
		t.Error("ProviderName")
	}
}
