package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"path/filepath"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/node"
	"awgrotator/internal/remote"
	"awgrotator/internal/splitdns"
	"awgrotator/internal/splitplan"
	"awgrotator/internal/store"
)

// prepareSplit gathers what split routing adds to a deployment, using the
// operator's project for the cache and the rule files.
func prepareSplit(ctx context.Context, root string, logf func(string, ...any)) (*splitplan.Plan, error) {
	return splitplan.Prepare(ctx, splitplan.Sources{
		CacheDir:    store.Dir(root),
		RulesDir:    filepath.Join(root, "rules"),
		CreateRules: true,
	}, logf)
}

// cmdSplitCheck asks the current server's nft whether it accepts the ruleset,
// without applying it. A syntax error found here costs nothing; found during a
// deployment it costs a server.
func cmdSplitCheck(args []string) error {
	fs := flag.NewFlagSet("split-check", flag.ExitOnError)
	host := fs.String("host", "", "адрес сервера (по умолчанию текущий)")
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
	syncIfPossible(root, st)
	addr, known := *host, st.ServerHostKey
	if addr == "" {
		if st.Server.IP == "" {
			return errors.New("сервера нет")
		}
		addr = st.Server.IP
	} else if addr != st.Server.IP {
		known = ""
	}

	nft := &splitdns.NFT{
		Interface: "awg0",
		Range:     netip.MustParsePrefix(node.DefaultStandInRange),
		DNS:       netip.MustParseAddr(node.DefaultDNSAddress),
	}

	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cli, err := remote.Dial(ctx, addr, "root", signer, known)
	if err != nil {
		return err
	}
	defer cli.Close()

	const path = "/tmp/awgsplit-check.nft"
	if err := cli.Upload(path, "0600", []byte(nft.Ruleset())); err != nil {
		return err
	}
	out, runErr := cli.Run("nft --version; nft -c -f " + path + " && echo OK-ПРАВИЛА-ПРИНЯТЫ; rm -f " + path)
	fmt.Println(out)
	return runErr
}
