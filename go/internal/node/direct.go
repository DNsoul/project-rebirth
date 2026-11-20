package node

import (
	"errors"

	"awgrotator/internal/splitdns"
)

// ErrAlwaysTunnel refuses a name that the rules always send through the
// tunnel: a counter or a pixel that would learn the home address. Sending it
// direct would not happen anyway, since those rules win, and accepting it
// would only make the list say something untrue.
var ErrAlwaysTunnel = errors.New("always sent through the tunnel")

// DirectRouting is the operator's list of sites sent past the tunnel, as the
// control socket reads and changes it.
type DirectRouting interface {
	Names() []string
	Add(name string) (string, error)
	Remove(name string) error
}

// directRouting ties the list to the running resolver: each change is saved,
// handed to the decider and the remembered decisions dropped, so it takes
// effect on the next query rather than at the next restart.
type directRouting struct {
	list    *splitdns.DirectList
	decider *splitdns.Decider
	forget  func()
}

// NewDirectRouting ties list to decider and hands the list over at once;
// forget, when set, is called after every change to drop remembered
// decisions.
func NewDirectRouting(list *splitdns.DirectList, decider *splitdns.Decider, forget func()) *directRouting {
	d := &directRouting{list: list, decider: decider, forget: forget}
	d.apply()
	return d
}

func (d *directRouting) Names() []string { return d.list.Names() }

func (d *directRouting) Add(name string) (string, error) {
	n, err := splitdns.CheckDirectName(name)
	if err != nil {
		return "", err
	}
	if d.decider.ForceTunnel.Match(n) {
		return "", ErrAlwaysTunnel
	}
	if n, err = d.list.Add(n); err != nil {
		return "", err
	}
	d.apply()
	return n, nil
}

func (d *directRouting) Remove(name string) error {
	if err := d.list.Remove(name); err != nil {
		return err
	}
	d.apply()
	return nil
}

func (d *directRouting) apply() {
	d.decider.Chosen.Store(d.list.Rules())
	if d.forget != nil {
		d.forget()
	}
}
