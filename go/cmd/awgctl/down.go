package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/config"
	"awgrotator/internal/proto"
	"awgrotator/internal/store"
	"awgrotator/internal/vultr"
)

func cmdDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	id := fs.String("id", "", "удалить только этот сервер (по умолчанию текущий)")
	all := fs.Bool("all", false, "удалить все наши серверы")
	yes := fs.Bool("yes", false, "подтвердить удаление")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	vc := vultr.New(secrets.VultrAPIKey)

	// Only ever look at instances carrying our tag, so nothing else on the
	// account can be deleted by accident.
	ours, err := vc.InstancesByTag(ctx, config.InstanceTag)
	if err != nil {
		return err
	}

	var targets []vultr.Instance
	switch {
	case *all:
		targets = ours
	case *id != "":
		for _, i := range ours {
			if i.ID == *id {
				targets = append(targets, i)
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("сервер %s не найден среди наших", *id)
		}
	case st.Server.ID != "":
		for _, i := range ours {
			if i.ID == st.Server.ID {
				targets = append(targets, i)
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("текущий сервер %s уже удалён; состояние можно почистить через -all", st.Server.ID)
		}
	default:
		return errors.New("нечего удалять")
	}

	fmt.Println("Будут удалены:")
	for _, i := range targets {
		fmt.Printf("  %s  %-4s %s\n", i.ID[:8], i.Region, i.MainIP)
	}
	if !*yes {
		return errors.New("это необратимо. Если согласен — повтори с -yes")
	}

	var failed []string
	for _, i := range targets {
		if err := vc.DeleteInstance(ctx, i.ID); err != nil {
			fmt.Printf("  %s: %v\n", i.ID[:8], err)
			failed = append(failed, i.ID)
			continue
		}
		fmt.Printf("  %s удалён\n", i.ID[:8])
		if i.ID == st.Server.ID {
			st.Server = store.Server{}
			st.ServerHostKey = ""
		}
	}
	if err := st.Save(root); err != nil {
		return err
	}

	if st.Server.ID == "" && st.GistID != "" {
		// Nothing is serving now, so retract the configuration rather than
		// leave clients retrying a dead address.
		fmt.Println("Серверов нет — убираю конфиг.")
		blank, err := blankConfig(st)
		if err != nil {
			return err
		}
		where, err := openPlaces(st, secrets)
		if err != nil {
			return err
		}
		if err := where.Write(ctx, map[string]string{proto.FileConfig: blank}); err != nil {
			fmt.Printf("Не удалось: %v\n", err)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("не удалось удалить: %v", failed)
	}
	return nil
}

// blankConfig is an encrypted empty object: clients cannot parse it as a
// configuration, so they report that nothing is published.
func blankConfig(st *store.State) (string, error) {
	contentKey, err := st.ContentKey()
	if err != nil {
		return "", err
	}
	return proto.Seal(contentKey, []byte("{}"))
}

// cmdClientConf writes the client configuration that is currently published,
// read back the same way an app would.
//
// It can write the file itself, and that is the way to use it. Redirecting
// the output through a shell is how a byte order mark ends up at the start of
// the file, and the AmneziaWG client rejects the whole file over it with
// "Line must occur in a section" — which reads like a broken configuration
// rather than an encoding problem.
func cmdClientConf(args []string) error {
	fs := flag.NewFlagSet("client-conf", flag.ExitOnError)
	outPath := fs.String("o", "", "записать в файл (иначе вывод на экран)")
	name := fs.String("device", "operator", "имя устройства: у каждого имени свой ключ и свой адрес")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	body, err := fetchBody(ctx, st, secrets)
	if err != nil {
		return err
	}

	var conf string
	switch {
	case body.Enroll != nil:
		// Each device has a key of its own and registers it, as the apps do.
		reg, err := registrationFor(st, body)
		if err != nil {
			return err
		}
		dev, err := loadDevice(root, *name)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Регистрирую устройство «%s»…\n", dev.Name)
		addr, err := dev.register(ctx, reg)
		if err != nil {
			return fmt.Errorf("регистрация не удалась: %w", err)
		}
		fmt.Fprintf(os.Stderr, "  выдан адрес %s\n", addr.Addr())
		conf = body.ClientConf(dev.Keys.Private, addr.String())
	case body.Shared != nil:
		// A server that was built before devices registered themselves.
		fmt.Fprintln(os.Stderr, "Сервер старого образца: конфиг общий для всех устройств.")
		conf = body.Shared.Conf
	default:
		return errors.New("в опубликованном конфиге нет ни регистрации устройств, ни готового клиента")
	}
	if err := checkConfText(conf); err != nil {
		return err
	}
	if *outPath == "" {
		fmt.Print(conf)
		fmt.Fprintln(os.Stderr, "\nСовет: сохраняй через -o awg.conf, а не через \">\" —"+
			"\nоболочка добавляет метку BOM, и клиент отказывается читать файл.")
		return nil
	}

	// Written as raw bytes: no BOM, and the newlines stay as rendered.
	if err := os.WriteFile(*outPath, []byte(conf), 0o600); err != nil {
		return err
	}
	fmt.Printf("Записано в %s (%d байт)\n", *outPath, len(conf))
	fmt.Println("Файл содержит закрытый ключ, поэтому он в .gitignore.")
	return nil
}

// checkConfText guards the shape of the file the client has to parse: a byte
// order mark or a stray line outside a section makes the client reject
// everything, so it is worth catching here rather than in the client.
func checkConfText(conf string) error {
	if strings.HasPrefix(conf, awg.BOM) {
		return errors.New("в конфиге метка BOM — клиент такой файл не примет")
	}
	if !strings.HasPrefix(conf, "[Interface]") {
		return fmt.Errorf("конфиг начинается не с [Interface], а с %q", first20(conf))
	}
	section := ""
	for n, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section == "" {
			return fmt.Errorf("строка %d вне секции: %q", n+1, line)
		}
		if !strings.Contains(line, " = ") {
			return fmt.Errorf("строка %d не вида \"Ключ = значение\": %q", n+1, line)
		}
	}
	return nil
}

func first20(s string) string {
	if len(s) > 20 {
		return s[:20]
	}
	return s
}
