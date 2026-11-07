//go:build linux

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"awgrotator/internal/ruset"
	"awgrotator/internal/splitdns"
)

const (
	// poolSaveEvery is how often the mapping table is written to disk, so a
	// restart brings back the stand-ins clients have already cached.
	poolSaveEvery = 30 * time.Second

	// poolMaxIdle is how long a mapping may go without being asked for before
	// it is dropped. Clients cache an address for a minute; a week is far
	// longer than any cache.
	poolMaxIdle = 7 * 24 * time.Hour

	// expireEvery is how often idle mappings are looked for.
	expireEvery = time.Hour
)

// splitRuntime is the running resolver and what is needed to stop it.
type splitRuntime struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	server *splitdns.Server
	pool   *splitdns.Pool
	cfg    *SplitConfig

	ruRanges, forceTunnel, forceDirect int
}

func (s *splitRuntime) stop() {
	s.cancel()
	s.wg.Wait()
}

// startSplit brings the split-routing resolver up. It is called after the
// tunnel and NAT are in place.
func (n *Node) startSplit(ctx context.Context) error {
	cfg := n.cfg.Split
	if cfg == nil {
		return nil
	}

	rng := netip.MustParsePrefix(cfg.StandInRange)
	dns := netip.MustParseAddr(cfg.DNSAddress)

	// The resolver's own address goes on the tunnel device, so queries that
	// arrive through the tunnel are delivered locally.
	if err := n.run(ctx, "ip", "address", "replace", cfg.DNSAddress+"/32", "dev", n.cfg.Interface); err != nil {
		return fmt.Errorf("add resolver address: %w", err)
	}

	nft := &splitdns.NFT{Interface: n.cfg.Interface, Range: rng, DNS: dns}
	if err := nft.Setup(ctx); err != nil {
		return err
	}

	set, err := n.loadRUSet(cfg.RUSetPath)
	if err != nil {
		return err
	}
	forceTunnel := n.loadRules(cfg.ForceTunnelPath)
	forceDirect := n.loadRules(cfg.ForceDirectPath)

	pool, err := splitdns.NewPool(rng, dns)
	if err != nil {
		return err
	}
	entries, err := splitdns.LoadEntries(cfg.PoolPath)
	if err != nil {
		n.log.Warn("ignoring the saved mapping table", "error", err)
		entries = nil
	}
	if err := pool.Restore(entries); err != nil {
		// A bad entry here would become a wrong translation, so none of
		// the table is trusted.
		n.log.Warn("ignoring the saved mapping table", "error", err)
		entries = nil
		_ = pool.Restore(nil)
	}
	if err := nft.Replace(entries); err != nil {
		return fmt.Errorf("restore translations: %w", err)
	}
	n.log.Info("restored translations", "count", len(entries))

	clientSubnet := netip.MustParsePrefix(cfg.ClientSubnet)
	srv, err := splitdns.NewServer(splitdns.Config{
		Decider:   &splitdns.Decider{RU: set, ForceTunnel: forceTunnel, ForceDirect: forceDirect},
		Pool:      pool,
		Mapper:    nft,
		Primary:   &splitdns.Upstream{Addr: cfg.PrimaryUpstream, ECS: clientSubnet},
		Secondary: &splitdns.Upstream{Addr: cfg.SecondaryUpstream},
		Log:       n.log,
	})
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(cfg.DNSAddress, strconv.Itoa(53))
	udp, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort(addr)))
	if err != nil {
		return fmt.Errorf("listen on %s/udp: %w", addr, err)
	}
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		udp.Close()
		return fmt.Errorf("listen on %s/tcp: %w", addr, err)
	}

	// The resolver outlives this call, so it gets its own context rather than
	// the start-up one, and is stopped through the runtime.
	runCtx, cancel := context.WithCancel(context.Background())
	rt := &splitRuntime{
		cancel: cancel, server: srv, pool: pool, cfg: cfg,
		ruRanges: set.Ranges(), forceTunnel: forceTunnel.Len(), forceDirect: forceDirect.Len(),
	}
	rt.wg.Add(2)
	go func() {
		defer rt.wg.Done()
		if err := srv.Serve(runCtx, udp, tcp); err != nil {
			n.log.Error("resolver stopped", "error", err)
		}
	}()
	go func() {
		defer rt.wg.Done()
		n.maintainPool(runCtx, pool, nft, cfg.PoolPath)
	}()

	n.split = rt
	n.log.Info("split resolver is listening", "address", addr,
		"ru_ranges", rt.ruRanges, "force_tunnel", rt.forceTunnel, "force_direct", rt.forceDirect)
	return nil
}

// maintainPool saves the mapping table regularly and drops mappings nobody has
// asked for in a long while. It saves once more on the way out.
func (n *Node) maintainPool(ctx context.Context, pool *splitdns.Pool, nft *splitdns.NFT, path string) {
	save := time.NewTicker(poolSaveEvery)
	defer save.Stop()
	expire := time.NewTicker(expireEvery)
	defer expire.Stop()

	persist := func() {
		if err := splitdns.SaveEntries(path, pool.Snapshot()); err != nil {
			n.log.Warn("could not save the mapping table", "error", err)
		}
	}
	defer persist()

	for {
		select {
		case <-ctx.Done():
			return
		case <-save.C:
			persist()
		case <-expire.C:
			for _, e := range pool.Expire(time.Now().Add(-poolMaxIdle)) {
				if err := nft.Delete(e.Fake); err != nil {
					n.log.Warn("could not remove an idle translation", "error", err)
				}
			}
		}
	}
}

// loadRUSet reads the local address set. A missing file is survivable: with
// an empty set every site is tunnelled, which hides the home address at the
// cost of not splitting anything. It is logged loudly and shows up in the
// status, so it cannot go unnoticed.
func (n *Node) loadRUSet(path string) (*ruset.Set, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			n.log.Error("the local address set is missing; every site will go through the tunnel", "path", path)
			return &ruset.Set{}, nil
		}
		return nil, err
	}
	defer f.Close()
	set, err := ruset.Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return set, nil
}

// loadRules reads a rules file. A missing one is the same as an empty one; a
// malformed one is reported and treated as empty rather than preventing the
// tunnel from coming up.
func (n *Node) loadRules(path string) *splitdns.Rules {
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			n.log.Warn("could not read a rules file", "path", path, "error", err)
		}
		rules, _ := splitdns.NewRules()
		return rules
	}
	defer f.Close()
	rules, err := splitdns.ParseRules(f)
	if err != nil {
		n.log.Warn("ignoring a malformed rules file", "path", path, "error", err)
		rules, _ = splitdns.NewRules()
	}
	return rules
}

// splitStatus reports on the running resolver, or nil when there is none.
func (n *Node) splitStatus() *SplitStatus {
	n.mu.Lock()
	rt := n.split
	n.mu.Unlock()
	if rt == nil {
		return nil
	}
	return &SplitStatus{
		Stats:       rt.server.Stats(),
		RURanges:    rt.ruRanges,
		ForceTunnel: rt.forceTunnel,
		ForceDirect: rt.forceDirect,
	}
}
