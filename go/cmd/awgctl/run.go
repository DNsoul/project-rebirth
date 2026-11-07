package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/remote"
	"awgrotator/internal/store"
)

// cmdRun executes a command on the current server over SSH, using the key in
// the state file and checking the host key recorded when the server was
// built. It exists so diagnosing a server never means copying the private key
// somewhere else.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	host := fs.String("host", "", "адрес сервера (по умолчанию текущий)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	command := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if command == "" {
		return errors.New("нечего выполнять: awgctl run \"<команда>\"")
	}

	root, err := config.Root()
	if err != nil {
		return err
	}
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	syncIfPossible(root, st)

	addr := *host
	knownKey := st.ServerHostKey
	if addr == "" {
		if st.Server.IP == "" {
			return errors.New("сервера нет, запусти \"awgctl up\"")
		}
		addr = st.Server.IP
	} else if addr != st.Server.IP {
		// A different host has a different key; nothing to compare against.
		knownKey = ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}
	cli, err := remote.Dial(ctx, addr, "root", signer, knownKey)
	if err != nil {
		return err
	}
	defer cli.Close()

	// First connection to a host we have no key for: record it.
	if knownKey == "" && addr == st.Server.IP {
		st.ServerHostKey = cli.HostKey()
		if err := st.Save(root); err != nil {
			return err
		}
	}

	out, runErr := cli.Run(command)
	if out != "" {
		fmt.Println(out)
	}
	return runErr
}
