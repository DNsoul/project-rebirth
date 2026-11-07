package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/node"
	"awgrotator/internal/store"
	"awgrotator/internal/tunneltest"
)

// cmdVerify connects to the server whose configuration is published, the way
// a client would, and checks that the whole path works.
//
// It registers a device of its own first, exactly as an app does, and tests
// with that device's key. So it proves the part a new server most needs to get
// right: that a device can arrive, be accepted, and use the tunnel. The device
// is called "verify", so running this again changes nothing, and a connected
// client never shares its key.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	timeout := fs.Duration("timeout", time.Minute, "сколько ждать рукопожатия")
	reuse := fs.Bool("reuse", false, "не регистрировать заново: проверить, что сервер уже знает устройство «verify» "+
		"(так видно, что устройства переехали вместе с сервером)")
	if err := fs.Parse(args); err != nil {
		return err
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

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+30*time.Second)
	defer cancel()

	body, err := fetchBody(ctx, st, secrets)
	if err != nil {
		return err
	}
	endpoint, err := netip.ParseAddrPort(body.Endpoint)
	if err != nil {
		return fmt.Errorf("адрес сервера %q: %w", body.Endpoint, err)
	}

	target := tunneltest.Target{
		Endpoint:     endpoint,
		ServerPublic: body.ServerPublicKey,
		Params:       body.Params,
		Resolver:     netip.AddrPortFrom(netip.MustParseAddr(node.DefaultDNSAddress), 53),
		StandIn:      netip.MustParsePrefix(node.DefaultStandInRange),
	}

	fmt.Printf("Проверяю %s (%s) настоящим клиентом…\n", body.Endpoint, body.Region)
	if body.Enroll == nil {
		return errors.New("в опубликованном конфиге нет регистрации устройств: " +
			"сервер собран по старому образцу, подними новый (awgctl up)")
	}

	reg, err := registrationFor(st, body)
	if err != nil {
		return err
	}
	dev, err := loadDevice(root, "verify")
	if err != nil {
		return err
	}
	var addr netip.Prefix
	if *reuse {
		if !dev.Address.IsValid() {
			return errors.New("устройство «verify» ещё нигде не регистрировалось: сначала запусти verify без -reuse")
		}
		addr = dev.Address
		fmt.Printf("Не регистрирую заново: устройство «verify» должно быть известно серверу под адресом %s.\n\n", addr.Addr())
	} else {
		fmt.Println("Регистрирую устройство «verify»…")
		regCtx, regCancel := context.WithTimeout(ctx, *timeout)
		addr, err = dev.register(regCtx, reg)
		regCancel()
		if err != nil {
			return fmt.Errorf("регистрация не удалась: %w", err)
		}
		fmt.Printf("  выдан адрес %s\n\n", addr.Addr())
	}

	target.ClientPrivate, target.ClientAddr = dev.Keys.Private, addr.Addr()
	target.Registration = reg

	rep, err := tunneltest.Run(ctx, target, tunneltest.Options{Timeout: *timeout})
	if err != nil {
		return err
	}
	fmt.Println(rep)
	if !rep.Passed() {
		return errors.New("сервер не прошёл проверку")
	}
	fmt.Println("\nСервер работает: регистрация устройства, рукопожатие, DNS, подмена адреса и выход в интернет проверены.")
	return nil
}
