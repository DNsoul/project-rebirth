package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"awgrotator/internal/awg"
	"awgrotator/internal/cloud"
	"awgrotator/internal/config"
	"awgrotator/internal/deploy"
	"awgrotator/internal/egress"
	"awgrotator/internal/fleet"
	"awgrotator/internal/node"
	"awgrotator/internal/remote"
	"awgrotator/internal/store"
	"awgrotator/internal/tunneltest"
)

// cmdLabTunnel measures how long a fresh client takes to complete its
// handshake against a real server, for a given S4.
//
// It exists because the same measurement between two devices in one process
// said something surprising, and only a real server with a real tunnel device
// can say whether that is a property of the protocol or of the test. Each
// server it creates is deleted before the command returns, whatever happens.
func cmdLabTunnel(args []string) error {
	fs := flag.NewFlagSet("lab-tunnel", flag.ExitOnError)
	placeFlag := fs.String("place", "vultr:waw", "место: хостер:город (опытные серверы всегда почасовые)")
	s4List := fs.String("s4", "20,0", "значения S4 через запятую, по серверу на каждое")
	runs := fs.Int("runs", 3, "сколько раз подключаться к каждому серверу")
	full := fs.Bool("full", false, "собрать узел как настоящий (разделение трафика, резолвер, регистрация) и проверить его так же, как менеджер проверяет преемника")
	exitCheck := fs.Bool("exit", false, "вместе с -full: включить выход через WARP, проверить его, его отказ и выключение")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *exitCheck && !*full {
		return fmt.Errorf("-exit проверяется только на полном узле: добавь -full")
	}

	var values []int
	for _, f := range strings.Split(*s4List, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 0 || n > 200 {
			return fmt.Errorf("S4 %q некорректен", f)
		}
		values = append(values, n)
	}

	root, err := config.Root()
	if err != nil {
		return err
	}
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}
	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}

	fmt.Println("Собираю узел под Linux…")
	binary, err := buildNode(root)
	if err != nil {
		return err
	}

	limit := 20 * time.Minute
	if *exitCheck {
		limit = 35 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	where, err := cloud.ParsePlace(*placeFlag)
	if err != nil {
		return err
	}
	labs, err := openClouds(st, secrets, LabTag)
	if err != nil {
		return err
	}
	p, err := labs.Get(where.Provider)
	if err != nil {
		return err
	}
	keyID, err := ensureSSHKey(ctx, p, st, root, func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
	if err != nil {
		return err
	}

	if *full {
		return labFull(ctx, root, st, p, signer, keyID, binary, where.Location, *exitCheck)
	}
	for _, s4 := range values {
		if err := labTunnelOne(ctx, p, signer, keyID, binary, where.Location, s4, *runs); err != nil {
			fmt.Printf("  S4=%d: %v\n", s4, err)
		}
	}
	return nil
}

func labTunnelOne(ctx context.Context, p cloud.Provider, signer ssh.Signer, keyID string,
	binary []byte, region string, s4, runs int) error {

	params, err := awg.GenerateParams()
	if err != nil {
		return err
	}
	params.S4 = s4
	if s4 < awg.HeaderNonceSize {
		// Header protection needs every S to be at least the nonce size, so a
		// smaller S4 means running without it.
		params.HeaderProtectionKey = awg.Key{}
	}
	gen, err := fleet.NewGeneration(fleet.GenOptions{Params: &params})
	if err != nil {
		return err
	}

	fmt.Printf("\nS4=%d: создаю лабораторный сервер в %s…\n", s4, region)
	logf := func(format string, a ...any) { fmt.Printf("  "+format+"\n", a...) }

	res, err := deploy.Provision(ctx, p, deploy.Request{
		Location: region,
		Billing:  cloud.Hourly,
		Label:    fmt.Sprintf("lab-tunnel-s4-%d", s4),
		SSHKeyID: keyID,
		Binary:   binary,
		Config:   gen.Node,
		Signer:   signer,
	}, logf)
	if err != nil {
		return err
	}
	// The server is deleted whatever happens below, and with a context of its
	// own so that an interrupted run still cleans up.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := p.Delete(cleanup, res.InstanceID); err != nil {
			fmt.Printf("  ВНИМАНИЕ: сервер %s не удалился (%v), удали: awgctl lab-down -yes\n", shortID(res.InstanceID), err)
			return
		}
		fmt.Printf("  лабораторный сервер %s удалён\n", shortID(res.InstanceID))
	}()

	ip, err := netip.ParseAddr(res.IP)
	if err != nil {
		return err
	}
	target := tunneltest.Target{
		Endpoint:      netip.AddrPortFrom(ip, uint16(gen.Port)),
		ServerPublic:  gen.Server.Public,
		Params:        gen.Params,
		ClientPrivate: gen.Health.Private,
		ClientAddr:    netip.MustParseAddr("10.8.0.250"),
		Resolver:      netip.MustParseAddrPort("198.18.0.1:53"),
		StandIn:       netip.MustParsePrefix("198.18.0.0/15"),
	}

	fmt.Printf("  рукопожатие с S4=%d (защита заголовков: %v):\n", s4, !gen.Params.HeaderProtectionKey.IsZero())
	for i := 1; i <= runs; i++ {
		r, err := tunneltest.Handshake(ctx, target, netip.MustParseAddr("10.8.0.1"), 40*time.Second)
		if err != nil {
			fmt.Printf("    попытка %d: %v\n", i, err)
			continue
		}
		fmt.Printf("    попытка %d: %-8s запросов до ответа: %d\n", i, r.Elapsed.Round(10*time.Millisecond), r.Requests)
	}
	return nil
}

// labFull builds a node the way a real move does, with split routing and its
// resolver, installs it the same way (so the install script and the resolver
// check run against this provider's image), and then checks it from here
// with the same test the manager runs on a successor: the tunnel, the
// resolver, a site through the tunnel and a new device registering. The
// server is paid by the hour and deleted whatever happens.
func labFull(ctx context.Context, root string, st *store.State, p cloud.Provider, signer ssh.Signer, keyID string,
	binary []byte, region string, exitCheck bool) error {

	logf := func(format string, a ...any) { fmt.Printf("  "+format+"\n", a...) }
	contentKey, err := st.ContentKey()
	if err != nil {
		return err
	}
	gen, err := fleet.NewGeneration(fleet.GenOptions{Split: true, ContentKey: contentKey})
	if err != nil {
		return err
	}
	plan, err := prepareSplit(ctx, root, logf)
	if err != nil {
		return err
	}

	fmt.Printf("\nСоздаю опытный сервер в %s:%s, почасово…\n", p.Name(), region)
	res, err := deploy.Provision(ctx, p, deploy.Request{
		Location: region,
		Billing:  cloud.Hourly,
		Label:    fmt.Sprintf("lab-full-%s-%d", region, time.Now().Unix()),
		SSHKeyID: keyID,
		Binary:   binary,
		Config:   gen.Node,
		Files:    plan.Files,
		Signer:   signer,
	}, logf)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := p.Delete(cleanup, res.InstanceID); err != nil {
			fmt.Printf("  ВНИМАНИЕ: сервер %s не удалился (%v), удали: awgctl lab-down -yes\n", shortID(res.InstanceID), err)
			return
		}
		fmt.Printf("  опытный сервер %s удалён\n", shortID(res.InstanceID))
	}()

	ip, err := netip.ParseAddr(res.IP)
	if err != nil {
		return err
	}
	endpoint := netip.AddrPortFrom(ip, uint16(gen.Port))
	target := tunneltest.Target{
		Endpoint:      endpoint,
		ServerPublic:  gen.Server.Public,
		Params:        gen.Params,
		ClientPrivate: gen.Health.Private,
		ClientAddr:    netip.MustParseAddr("10.8.0.250"),
		Resolver:      netip.AddrPortFrom(netip.MustParseAddr(node.DefaultDNSAddress), 53),
		StandIn:       netip.MustParsePrefix(node.DefaultStandInRange),
		Registration: &tunneltest.Registration{
			Endpoint:     endpoint,
			ServerPublic: gen.Server.Public,
			Params:       gen.Params,
			PeerPrivate:  gen.Enroll.Private,
			PeerAddr:     netip.MustParseAddr(node.DefaultEnrollPeerAddress),
			ContentKey:   contentKey,
			SelfTest:     &gen.SelfTest,
		},
	}
	fmt.Println("  проверяю, как менеджер проверяет преемника…")
	report, err := tunneltest.Run(ctx, target, tunneltest.Options{})
	if err != nil {
		return fmt.Errorf("проверка не выполнилась: %w", err)
	}
	fmt.Println(report)
	if !report.Passed() {
		return fmt.Errorf("проверка не пройдена")
	}
	fmt.Println("  ВСЁ РАБОТАЕТ: сюда можно переезжать")
	if exitCheck {
		return labExit(ctx, res, signer, target)
	}
	return nil
}

// labExit switches the lab node's way out to WARP and back and checks each step
// from a client in the tunnel: where Cloudflare's trace page says the request
// came from, before, through WARP, while WARP is cut off (the clients must fall
// back to the direct path), once it is back, and after switching off.
func labExit(ctx context.Context, res *deploy.Result, signer ssh.Signer, target tunneltest.Target) error {
	cli, err := remote.Dial(ctx, res.IP, "root", signer, res.HostKey)
	if err != nil {
		return err
	}
	defer cli.Close()
	n, stop, err := tunneltest.Connect(target)
	if err != nil {
		return err
	}
	defer stop()

	trace := func(what string) (map[string]string, error) {
		t, err := tunneltest.Trace(ctx, n, target.Resolver)
		if err != nil {
			fmt.Printf("  %s: страница Cloudflare не открылась: %v\n", what, err)
			return nil, err
		}
		from := "адрес сервера"
		if t["ip"] != res.IP {
			from = "НЕ адрес сервера"
		}
		fmt.Printf("  %s: warp=%s, выход с %s (%s), loc=%s\n", what, t["warp"], from, t["ip"], t["loc"])
		return t, nil
	}
	want := func(m egress.Mode) error {
		return cli.Upload(egress.DefaultWantPath, "0600", egress.MarshalWant(m))
	}
	wait := func(within time.Duration, states ...egress.State) (*egress.Status, error) {
		deadline := time.Now().Add(within)
		var last egress.State
		for {
			raw, err := cli.Run("cat " + node.DefaultStatusPath)
			if err == nil {
				if ns, err := node.ParseStatus([]byte(raw)); err == nil && ns.Exit != nil {
					if ns.Exit.State != last {
						last = ns.Exit.State
						fmt.Printf("    узел: %s %s\n", last, ns.Exit.Error)
					}
					for _, s := range states {
						if ns.Exit.State == s {
							return ns.Exit, nil
						}
					}
				}
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("за %s узел не пришёл в %v (сейчас %s)", within, states, last)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
	nodeLog := func() {
		out, _ := cli.Run("journalctl -u awgnode --no-pager -n 300 | grep -iE 'exit|warp' | tail -25")
		fmt.Println("  журнал узла (выход):\n" + out)
	}
	fail := func(err error) error {
		nodeLog()
		return err
	}

	fmt.Println("\nПроверяю выход через WARP…")
	if t, err := trace("до включения"); err != nil {
		return fail(err)
	} else if t["ip"] != res.IP || t["warp"] == "on" {
		return fail(fmt.Errorf("до включения трафик выходит не с адреса сервера"))
	}

	if err := want(egress.WARP); err != nil {
		return err
	}
	start := time.Now()
	if _, err := wait(2*time.Minute, egress.StateWARP); err != nil {
		return fail(err)
	}
	fmt.Printf("  WARP включился за %s\n", time.Since(start).Round(time.Second))
	if t, err := trace("через WARP"); err != nil {
		return fail(err)
	} else if t["warp"] != "on" || t["ip"] == res.IP {
		return fail(fmt.Errorf("трафик клиента не идёт через WARP"))
	}

	fmt.Println("  отрезаю серверу WARP (UDP 2408 и 500 наружу)…")
	if _, err := cli.Run(`nft add table ip awgtest && nft add chain ip awgtest out '{ type filter hook output priority 0; }' && ` +
		`nft add rule ip awgtest out udp dport '{ 2408, 500, 1701, 4500 }' drop`); err != nil {
		return fail(fmt.Errorf("не удалось отрезать WARP: %w", err))
	}
	start = time.Now()
	_, err = wait(3*time.Minute, egress.StateFallback)
	if _, derr := cli.Run("nft delete table ip awgtest"); derr != nil {
		fmt.Printf("  ВНИМАНИЕ: правило не снялось: %v\n", derr)
	}
	if err != nil {
		return fail(err)
	}
	fmt.Printf("  узел ушёл на прямой путь за %s\n", time.Since(start).Round(time.Second))
	if t, err := trace("без WARP"); err != nil {
		return fail(err)
	} else if t["ip"] != res.IP {
		return fail(fmt.Errorf("при отрезанном WARP трафик не вернулся на прямой путь"))
	}

	fmt.Println("  WARP снова доступен, жду возврата (должно занять больше двух минут)…")
	start = time.Now()
	if _, err := wait(5*time.Minute, egress.StateWARP); err != nil {
		return fail(err)
	}
	fmt.Printf("  вернулся на WARP через %s\n", time.Since(start).Round(time.Second))
	if t, err := trace("снова через WARP"); err != nil {
		return fail(err)
	} else if t["warp"] != "on" {
		return fail(fmt.Errorf("после возврата трафик не идёт через WARP"))
	}

	if err := want(egress.Direct); err != nil {
		return err
	}
	start = time.Now()
	if _, err := wait(time.Minute, egress.StateDirect); err != nil {
		return fail(err)
	}
	fmt.Printf("  выключился за %s\n", time.Since(start).Round(time.Second))
	if t, err := trace("после выключения"); err != nil {
		return fail(err)
	} else if t["ip"] != res.IP || t["warp"] == "on" {
		return fail(fmt.Errorf("после выключения трафик не вернулся на адрес сервера"))
	}
	nodeLog()
	fmt.Println("  ВЫХОД ЧЕРЕЗ WARP РАБОТАЕТ: включение, отказ, возврат и выключение")
	return nil
}
