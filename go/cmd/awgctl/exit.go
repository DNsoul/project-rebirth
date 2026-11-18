package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/egress"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/node"
	"awgrotator/internal/remote"
	"awgrotator/internal/store"
)

// cmdExit switches where the devices' traffic leaves the server from: the same
// switch as the bot's, for when Telegram cannot be reached.
//
// "off" goes to the node directly as well as into the manager's settings, so it
// works even when the manager does not.
func cmdExit(args []string) error {
	what := "status"
	if len(args) > 0 {
		what = args[0]
	}
	var want *egress.Mode
	switch what {
	case "on":
		w := egress.WARP
		want = &w
	case "off":
		d := egress.Direct
		want = &d
	case "status":
	default:
		return fmt.Errorf("нужно on, off или status, а не %q", what)
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
	if st.ServerHostKey == "" {
		st.ServerHostKey = cli.HostKey()
		if err := st.Save(root); err != nil {
			return err
		}
	}

	if want != nil {
		if *want == egress.Direct {
			if err := cli.Upload(egress.DefaultWantPath, "0600", egress.MarshalWant(egress.Direct)); err != nil {
				return fmt.Errorf("узлу не удалось сказать «напрямую»: %w", err)
			}
		}
		raw, err := cli.Run("cat " + lifecycle.DefaultConfigPath)
		if err != nil {
			return fmt.Errorf("на сервере нет менеджера: %w", err)
		}
		cfg, err := lifecycle.ParseConfig([]byte(raw))
		if err != nil {
			return err
		}
		cfg.Policy.Exit = string(*want)
		data, err := cfg.Marshal()
		if err != nil {
			return err
		}
		if err := cli.Upload(lifecycle.DefaultConfigPath, "0600", data); err != nil {
			return err
		}
		if _, err := cli.Run("systemctl restart " + lifecycle.ManagerService); err != nil {
			return fmt.Errorf("менеджер не перезапустился: %w", err)
		}
		if *want == egress.WARP {
			fmt.Println("Записано: выход через WARP. Узел переключится в течение полуминуты; проверить: awgctl exit")
		} else {
			fmt.Println("Записано: выход напрямую. Узел переключится в течение десяти секунд; проверить: awgctl exit")
		}
		return nil
	}

	raw, err := cli.Run("cat " + node.DefaultStatusPath)
	if err != nil {
		return fmt.Errorf("узел не пишет статус: %w", err)
	}
	ns, err := node.ParseStatus([]byte(raw))
	if err != nil {
		return err
	}
	if ns.Exit == nil {
		fmt.Println("Узел не сообщает о выходе: на сервере версия без переключателя.")
		return nil
	}
	e := ns.Exit
	fmt.Printf("Просили: %s\nСейчас:  %s, с %s\n", e.Want, e.State, e.Since.Local().Format("02.01 15:04:05"))
	if !e.CheckedAt.IsZero() {
		fmt.Printf("Последняя проверка через WARP: %s\n", e.CheckedAt.Local().Format("02.01 15:04:05"))
	}
	if !e.Handshake.IsZero() {
		fmt.Printf("WARP последний раз отвечал туннелю: %s\n", e.Handshake.Local().Format("02.01 15:04:05"))
	}
	if e.Error != "" {
		fmt.Printf("Ошибка: %s\n", e.Error)
	}
	return nil
}
