package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"reflect"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/deploy"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/remote"
	"awgrotator/internal/store"
)

// cmdUpdate replaces the node on the current server and restarts it.
//
// It exists so that changing the node does not cost a server. The server's
// address, keys and the published configuration all stay as they are; only
// the program and, with -files, the address set and the rules are replaced.
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	files := fs.Bool("files", false, "заодно залить список сетей и правила")
	managerOnly := fs.Bool("manager", false, "обновить только менеджера, узел и подключения не трогать")
	approval := fs.String("approval", "", "новые устройства ждут одобрения в боте: on или off (по умолчанию не менять)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var wantApproval *bool
	switch *approval {
	case "":
	case "on", "off":
		v := *approval == "on"
		wantApproval = &v
	default:
		return fmt.Errorf("-approval: нужно on или off, а не %q", *approval)
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
	if st.Server.IP == "" {
		return errors.New("сервера нет, запусти \"awgctl up\"")
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}

	logf := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }

	if wantApproval != nil {
		if *managerOnly {
			return errors.New("-approval меняет и узел, а -manager его не трогает: запусти без -manager")
		}
		if telegramFor(st, secrets) == nil {
			return errors.New("одобрять некому: сначала бот (TELEGRAM_BOT_TOKEN в secrets.env и awgctl telegram link)")
		}
		st.Telegram.Approval = *wantApproval
		if err := st.Save(root); err != nil {
			return err
		}
	}

	if *managerOnly {
		if *files {
			return errors.New("-manager и -files вместе не нужны: списки заливаются вместе с узлом")
		}
		return updateManagerOnly(root, st, secrets, logf)
	}

	fmt.Println("Собираю узел под Linux…")
	binary, err := buildNode(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}
	cli, err := remote.Dial(ctx, st.Server.IP, "root", signer, st.ServerHostKey)
	if err != nil {
		return err
	}
	defer cli.Close()

	// The configuration the server is running with decides whether to expect
	// the resolver, so read it from the server rather than assuming.
	raw, err := cli.Run("cat " + deploy.ConfigPath)
	if err != nil {
		return err
	}
	var cfg node.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return fmt.Errorf("конфиг на сервере не читается: %w", err)
	}
	if wantApproval != nil {
		if cfg.Enroll == nil {
			return errors.New("этот сервер не принимает новые устройства вовсе: одобрять нечего")
		}
		if cfg.Enroll.Approval != *wantApproval {
			cfg.Enroll.Approval = *wantApproval
			data, err := cfg.Marshal()
			if err != nil {
				return err
			}
			if err := cli.Upload(deploy.ConfigPath, "0600", data); err != nil {
				return err
			}
		}
		if *wantApproval {
			logf("  новые устройства будут ждать одобрения в боте")
		} else {
			logf("  одобрение выключено: новые устройства входят сами (отказанные — нет)")
		}
	}

	if *files && cfg.Split != nil {
		plan, err := prepareSplit(ctx, root, logf)
		if err != nil {
			return err
		}
		for _, f := range plan.Files {
			logf("  загружаю %s", f.What)
			if err := cli.Upload(f.Path, f.Mode, f.Content); err != nil {
				return err
			}
		}
	}

	logf("  загружаю узел (%d КБ)", len(binary)/1024)
	if err := cli.Upload(deploy.BinaryPath, "0700", binary); err != nil {
		return err
	}

	// A server that has a manager gets the new manager too. It is replaced
	// before the node restarts, so the manager that is running while the node
	// is down is the new one.
	hasManager := false
	if out, err := cli.Run("test -f " + lifecycle.DefaultConfigPath + " && echo yes || echo no"); err == nil {
		hasManager = strings.TrimSpace(out) == "yes"
	}
	if hasManager {
		logf("  собираю менеджер…")
		managerBinary, err := buildLinux(root, "awgmgr")
		if err != nil {
			return err
		}
		logf("  загружаю менеджер (%d КБ)", len(managerBinary)/1024)
		if err := cli.Upload(lifecycle.ManagerBinaryPath, "0700", managerBinary); err != nil {
			return err
		}
		if err := cli.Upload(lifecycle.ManagerUnitPath, "0644", []byte(lifecycle.ManagerUnit())); err != nil {
			return err
		}
		if err := refreshManagerConfig(cli, st, secrets, logf); err != nil {
			return err
		}
	}

	// The status file is how a restart is told from the old process still
	// running, so remove it first.
	if _, err := cli.Run("rm -f " + cfg.Status() + " && systemctl restart awgnode"); err != nil {
		journal, _ := cli.Run("journalctl -u awgnode -n 40 --no-pager")
		return fmt.Errorf("перезапуск не удался: %w\n--- журнал ---\n%s", err, journal)
	}

	status, err := deploy.WaitStatus(ctx, cli, &cfg, logf)
	if err != nil {
		return err
	}
	logf("узел перезапущен: интерфейс %s, порт %d", status.Interface, status.ListenPort)

	if hasManager {
		if _, err := cli.Run("systemctl daemon-reload && systemctl restart " + lifecycle.ManagerService); err != nil {
			return fmt.Errorf("менеджер не перезапустился: %w", err)
		}
		logf("менеджер перезапущен")
	}

	if cfg.Split != nil {
		if err := deploy.VerifySplit(cli, &cfg, status, logf); err != nil {
			return err
		}
	}
	return nil
}

// refreshManagerConfig makes the manager's configuration on the server name the
// same second place and the same bot as the operator's state does, which is how
// a server that is already running takes them up without having to be
// replaced. The manager is restarted by the caller afterwards.
func refreshManagerConfig(cli *remote.Client, st *store.State, secrets config.Secrets, logf func(string, ...any)) error {
	raw, err := cli.Run("cat " + lifecycle.DefaultConfigPath)
	if err != nil {
		return fmt.Errorf("настройки менеджера не читаются: %w", err)
	}
	cfg, err := lifecycle.ParseConfig([]byte(raw))
	if err != nil {
		return err
	}
	wantGitLab, wantBot := gitLabSyncFor(st), telegramFor(st, secrets)
	sameGitLab, sameBot := reflect.DeepEqual(cfg.GitLab, wantGitLab), reflect.DeepEqual(cfg.Telegram, wantBot)
	if sameGitLab && sameBot {
		return nil
	}
	cfg.GitLab, cfg.Telegram = wantGitLab, wantBot
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := cli.Upload(lifecycle.DefaultConfigPath, "0600", data); err != nil {
		return err
	}
	switch {
	case sameGitLab:
	case wantGitLab == nil:
		logf("  второе место убрано из настроек менеджера")
	default:
		logf("  менеджер получил второе место (GitLab) и будет писать и туда")
	}
	switch {
	case sameBot:
	case wantBot == nil:
		logf("  бот убран из настроек менеджера")
	default:
		logf("  менеджер получил бота: отвечает %d владельцам, одобрение %s", len(wantBot.Owners), onOffWord(wantBot.Approval))
	}
	return nil
}

func onOffWord(b bool) string {
	if b {
		return "включено"
	}
	return "выключено"
}

// updateManagerOnly replaces just the manager on the current server and
// restarts it. The node, and so every connection through it, is left alone: the
// manager is the part that looks after the server, not the part that carries
// traffic, so this costs nobody a reconnect.
func updateManagerOnly(root string, st *store.State, secrets config.Secrets, logf func(string, ...any)) error {
	fmt.Println("Собираю менеджер под Linux…")
	managerBinary, err := buildLinux(root, "awgmgr")
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}
	cli, err := remote.Dial(ctx, st.Server.IP, "root", signer, st.ServerHostKey)
	if err != nil {
		return err
	}
	defer cli.Close()

	if out, err := cli.Run("test -f " + lifecycle.DefaultConfigPath + " && echo yes || echo no"); err != nil || strings.TrimSpace(out) != "yes" {
		return errors.New("на сервере нет менеджера: его обновлять нечем")
	}

	logf("  загружаю менеджер (%d КБ)", len(managerBinary)/1024)
	if err := cli.Upload(lifecycle.ManagerBinaryPath, "0700", managerBinary); err != nil {
		return err
	}
	if err := cli.Upload(lifecycle.ManagerUnitPath, "0644", []byte(lifecycle.ManagerUnit())); err != nil {
		return err
	}
	if err := refreshManagerConfig(cli, st, secrets, logf); err != nil {
		return err
	}
	if _, err := cli.Run("systemctl daemon-reload && systemctl restart " + lifecycle.ManagerService); err != nil {
		journal, _ := cli.Run("journalctl -u " + lifecycle.ManagerService + " -n 30 --no-pager")
		return fmt.Errorf("менеджер не перезапустился: %w\n--- журнал ---\n%s", err, journal)
	}

	// Seen to be running a few seconds later, not just started.
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	state, _ := cli.Run("systemctl is-active " + lifecycle.ManagerService)
	if strings.TrimSpace(state) != "active" {
		journal, _ := cli.Run("journalctl -u " + lifecycle.ManagerService + " -n 30 --no-pager")
		return fmt.Errorf("менеджер после перезапуска не работает (%s)\n--- журнал ---\n%s", strings.TrimSpace(state), journal)
	}
	logf("менеджер перезапущен и работает; узел и подключения не тронуты")
	return nil
}
