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
	"awgrotator/internal/config"
	"awgrotator/internal/deploy"
	"awgrotator/internal/fleet"
	"awgrotator/internal/store"
	"awgrotator/internal/tunneltest"
	"awgrotator/internal/vultr"
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
	region := fs.String("region", "waw", "регион Vultr")
	s4List := fs.String("s4", "20,0", "значения S4 через запятую, по серверу на каждое")
	runs := fs.Int("runs", 3, "сколько раз подключаться к каждому серверу")
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
	vc := vultr.New(secrets.VultrAPIKey)
	keyID, err := ensureSSHKey(ctx, vc, st, root, func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
	if err != nil {
		return err
	}

	for _, s4 := range values {
		if err := labTunnelOne(ctx, vc, signer, keyID, binary, *region, s4, *runs); err != nil {
			fmt.Printf("  S4=%d: %v\n", s4, err)
		}
	}
	return nil
}

func labTunnelOne(ctx context.Context, vc *vultr.Client, signer ssh.Signer, keyID string,
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

	res, err := deploy.Provision(ctx, vc, deploy.Request{
		Region:   region,
		Plan:     config.Plan,
		OsID:     config.OsID,
		Tag:      LabTag,
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
		if err := vc.DeleteInstance(cleanup, res.InstanceID); err != nil {
			fmt.Printf("  ВНИМАНИЕ: сервер %s не удалился (%v), удали: awgctl lab-down -yes\n", res.InstanceID[:8], err)
			return
		}
		fmt.Printf("  лабораторный сервер %s удалён\n", res.InstanceID[:8])
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
