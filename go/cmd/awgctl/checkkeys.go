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

// keyCheckScript asks each service, from the server, whether its key is
// accepted, and prints only the HTTP status codes.
//
// The keys are read from files rather than placed on the command line, so
// they never appear in a process listing, and the files are removed whatever
// happens.
const keyCheckScript = `#!/bin/sh
trap 'rm -f /root/.chk-vultr /root/.chk-github' EXIT
v=$(curl -s -o /dev/null -w '%{http_code}' -m 20 \
  -H "Authorization: Bearer $(cat /root/.chk-vultr)" \
  'https://api.vultr.com/v2/instances?per_page=1')
g=$(curl -s -o /dev/null -w '%{http_code}' -m 20 \
  -H "Authorization: Bearer $(cat /root/.chk-github)" \
  -H 'Accept: application/vnd.github+json' \
  'https://api.github.com/gists?per_page=1')
echo "vultr=$v github=$g"
`

// cmdCheckKeys confirms that the Vultr and GitHub credentials work from the
// server's own address.
//
// This matters because the Vultr key can be limited to a list of source
// addresses. A key that works from the operator's machine and is refused from
// the server would only be discovered the first time a server tried to build
// its replacement, which is the worst time to find out.
func cmdCheckKeys(args []string) error {
	fs := flag.NewFlagSet("check-keys", flag.ExitOnError)
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
	syncQuiet(root, st, secrets)
	if st.Server.IP == "" {
		return errors.New("сервера нет")
	}

	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cli, err := remote.Dial(ctx, st.Server.IP, "root", signer, st.ServerHostKey)
	if err != nil {
		return err
	}
	defer cli.Close()

	// Whatever happens below, do not leave the keys on the disk.
	defer cli.Run("rm -f /root/.chk-vultr /root/.chk-github /root/.chk.sh")

	for _, f := range []struct{ path, content, mode string }{
		{"/root/.chk-vultr", secrets.VultrAPIKey, "0600"},
		{"/root/.chk-github", secrets.GitHubToken, "0600"},
		{"/root/.chk.sh", keyCheckScript, "0700"},
	} {
		if err := cli.Upload(f.path, f.mode, []byte(f.content)); err != nil {
			return err
		}
	}

	out, err := cli.Run("sh /root/.chk.sh")
	if err != nil {
		return fmt.Errorf("проверка не выполнилась: %w", err)
	}

	codes := map[string]string{}
	for _, field := range strings.Fields(out) {
		if k, v, ok := strings.Cut(field, "="); ok {
			codes[k] = v
		}
	}
	report := func(name, code, hint string) bool {
		switch code {
		case "200":
			fmt.Printf("  %-8s принят (HTTP 200)\n", name)
			return true
		case "":
			fmt.Printf("  %-8s нет ответа\n", name)
		default:
			fmt.Printf("  %-8s отказ (HTTP %s). %s\n", name, code, hint)
		}
		return false
	}

	fmt.Printf("С адреса сервера %s:\n", st.Server.IP)
	okV := report("Vultr", codes["vultr"],
		"Вероятно, в «Access Control» ключа разрешён только твой адрес: нужно разрешить все IPv4.")
	okG := report("GitHub", codes["github"], "Проверь права токена: Gists — Read and write.")
	if !okV || !okG {
		return errors.New("ключи не работают с адреса сервера")
	}
	fmt.Println("Файлы с ключами на сервере удалены.")
	return nil
}
