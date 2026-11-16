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
	"awgrotator/internal/fleet"
	"awgrotator/internal/node"
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
	if err := fs.Parse(args); err != nil {
		return err
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
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
		return labFull(ctx, root, st, p, signer, keyID, binary, where.Location)
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
	binary []byte, region string) error {

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
	return nil
}
