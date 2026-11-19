//go:build linux

package node

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"time"

	"awgrotator/internal/usage"
)

// How often counted traffic is put into the history, and how often the
// history is written to disk. A crash loses at most the last few minutes.
const (
	usageCollectEvery = time.Minute
	usageSaveEvery    = 5 * time.Minute
)

// startUsage opens the traffic history and starts putting what the meter
// counts into it. It never stops the tunnel from coming up: without the
// history the server still serves, it only cannot say where traffic went.
func (n *Node) startUsage(context.Context) error {
	if n.meter == nil {
		return nil
	}
	path := DefaultUsagePath
	store, err := usage.Open(path)
	if err != nil {
		aside := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
		_ = os.Rename(path, aside)
		n.log.Error("the traffic history could not be read; it was set aside and counting starts afresh", "error", err, "kept_as", aside)
		store, _ = usage.Open(path)
	}
	n.usage = store

	classify := n.classifier()
	nic := newNICCounter(n.cfg.Interface)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	n.usageStop, n.usageDone = cancel, done
	go func() {
		defer close(done)
		collect := time.NewTicker(usageCollectEvery)
		defer collect.Stop()
		save := time.NewTicker(usageSaveEvery)
		defer save.Stop()
		step := func() {
			now := time.Now()
			store.Add(now, usage.Records(n.meter.Drain(), n.deviceByAddress(), classify))
			if b, err := nic.Delta(); err == nil {
				store.AddServer(now, b)
			}
		}
		persist := func() {
			if err := store.Save(time.Now()); err != nil {
				n.log.Warn("could not save the traffic history", "error", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				step()
				persist()
				return
			case <-collect.C:
				step()
			case <-save.C:
				persist()
			}
		}
	}()
	return nil
}

// stopUsage puts the last counts into the history and writes it.
func (n *Node) stopUsage() {
	if n.usageStop == nil {
		return
	}
	n.usageStop()
	<-n.usageDone
	n.usageStop, n.usageDone = nil, nil
}

// deviceByAddress maps tunnel addresses to the devices that hold them, from
// the device list as it is now.
func (n *Node) deviceByAddress() func(netip.Addr) (string, bool) {
	byAddr := map[netip.Addr]string{}
	if n.reg != nil {
		for _, d := range n.reg.Devices() {
			if p, err := netip.ParsePrefix(d.Address); err == nil {
				byAddr[p.Addr()] = d.ID
			}
		}
	}
	return func(a netip.Addr) (string, bool) {
		id, ok := byAddr[a]
		return id, ok
	}
}

// classifier tells the sites of addresses beyond the server: stand-ins by
// the label the resolver gave them, the server's own addresses, and
// Telegram's networks, which the clients route through the tunnel directly.
func (n *Node) classifier() usage.Classifier {
	var c usage.Classifier
	if p, err := netip.ParsePrefix(n.cfg.Subnet); err == nil {
		c.Server = append(c.Server, p.Masked())
	}
	if s := n.cfg.Split; s != nil {
		c.StandIns = netip.MustParsePrefix(s.StandInRange)
		c.Server = append(c.Server, netip.PrefixFrom(netip.MustParseAddr(s.DNSAddress), 32))
		c.Names = func(fake netip.Addr) string {
			n.mu.Lock()
			rt := n.split
			n.mu.Unlock()
			if rt == nil {
				return ""
			}
			return rt.pool.Site(fake)
		}
	}
	for _, s := range TelegramNets {
		if p, err := netip.ParsePrefix(s); err == nil {
			c.Telegram = append(c.Telegram, p)
		}
	}
	return c
}

// nicCounter follows what the server's network cards received and sent, as
// the provider sees it: every card but the loopback and the tunnels.
type nicCounter struct {
	skip []string
	last map[string][2]uint64
}

func newNICCounter(tunnel string) *nicCounter {
	return &nicCounter{skip: []string{"lo", tunnel, "warp", "awg", "docker", "veth"}, last: map[string][2]uint64{}}
}

// Delta is what the cards moved since the last call. The first call only
// takes the starting point. A counter that went down was reset (a card came
// back); what it shows then is all new.
func (c *nicCounter) Delta() (usage.Bytes, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return usage.Bytes{}, err
	}
	defer f.Close()
	cur, err := parseNetDev(f, c.skip)
	if err != nil {
		return usage.Bytes{}, err
	}
	var out usage.Bytes
	first := len(c.last) == 0
	for name, v := range cur {
		prev, seen := c.last[name]
		if !first && seen {
			out.Down += since(prev[0], v[0])
			out.Up += since(prev[1], v[1])
		}
	}
	c.last = cur
	return out, nil
}
