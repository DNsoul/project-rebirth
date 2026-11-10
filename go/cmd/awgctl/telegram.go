package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/store"
	"awgrotator/internal/telegram"
)

// cmdTelegram sets up the operator's bot: who it answers. The token is made in
// Telegram by @BotFather and put into secrets.env by the operator; this tool
// never prints it.
func cmdTelegram(args []string) error {
	if len(args) == 0 {
		return cmdTelegramShow(nil)
	}
	switch args[0] {
	case "link":
		return cmdTelegramLink(args[1:])
	case "unlink":
		return cmdTelegramUnlink(args[1:])
	case "show":
		return cmdTelegramShow(args[1:])
	}
	return fmt.Errorf("неизвестная подкоманда %q: link | unlink | show", args[0])
}

func telegramSetup() (string, *store.State, config.Secrets, error) {
	root, err := config.Root()
	if err != nil {
		return "", nil, config.Secrets{}, err
	}
	st, err := store.Load(root)
	if err != nil {
		return "", nil, config.Secrets{}, err
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return "", nil, config.Secrets{}, err
	}
	if secrets.TelegramToken == "" {
		return "", nil, config.Secrets{}, fmt.Errorf("нет %s в secrets.env: создай бота у @BotFather и впиши его токен туда (сюда его не показывай)", config.TelegramKeyName)
	}
	return root, st, secrets, nil
}

// cmdTelegramLink adds an owner: either the id given, or whoever writes to the
// bot first within the wait.
func cmdTelegramLink(args []string) error {
	fs := flag.NewFlagSet("telegram link", flag.ExitOnError)
	id := fs.Int64("id", 0, "добавить этот Telegram ID, не дожидаясь сообщения")
	wait := fs.Duration("wait", 3*time.Minute, "сколько ждать сообщения боту")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, st, secrets, err := telegramSetup()
	if err != nil {
		return err
	}
	tg := telegram.New(secrets.TelegramToken)

	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()
	me, err := tg.GetMe(ctx)
	if err != nil {
		if telegram.IsUnauthorized(err) {
			return errors.New("Telegram не принимает токен из secrets.env: проверь, что он скопирован целиком")
		}
		return fmt.Errorf("Telegram не отвечает: %w", err)
	}
	fmt.Printf("Бот: @%s\n", me.Username)

	owner := store.TelegramOwner{ID: *id}
	if owner.ID == 0 {
		fmt.Printf("Напиши боту @%s что угодно (например /start) с того аккаунта, которому он будет отвечать. Жду до %s…\n",
			me.Username, wait.Round(time.Second))
		found, err := firstWriter(ctx, tg, *wait)
		if err != nil {
			return err
		}
		owner = found
		fmt.Printf("Написал: %s, ID %d\n", owner.Name, owner.ID)
	}
	if owner.ID <= 0 {
		return fmt.Errorf("%d — не ID человека", owner.ID)
	}

	if st.Telegram == nil {
		st.Telegram = &store.Telegram{}
	}
	for _, o := range st.Telegram.Owners {
		if o.ID == owner.ID {
			fmt.Println("Этот аккаунт уже среди владельцев.")
			return nil
		}
	}
	st.Telegram.Owners = append(st.Telegram.Owners, owner)
	if err := st.Save(root); err != nil {
		return err
	}
	fmt.Printf("Добавлен. Владельцев: %d. На сервер это попадёт через awgctl update (или -manager) и с каждым новым сервером.\n",
		len(st.Telegram.Owners))
	return nil
}

// firstWriter waits for the first private message to the bot and returns who
// sent it. It reads the bot's updates itself, so it must not run while a
// server is answering the bot: they would take each other's messages.
func firstWriter(ctx context.Context, tg *telegram.Client, wait time.Duration) (store.TelegramOwner, error) {
	deadline := time.Now().Add(wait)
	var offset int64
	for time.Now().Before(deadline) {
		poll := min(time.Until(deadline), 30*time.Second)
		ups, err := tg.GetUpdates(ctx, offset, poll)
		if telegram.IsConflict(err) {
			return store.TelegramOwner{}, errors.New("бота сейчас читает сервер; добавь ID вручную: awgctl telegram link -id <ID>")
		}
		if err != nil {
			return store.TelegramOwner{}, err
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != "private" {
				continue
			}
			// Mark it read, so the server's bot does not answer it later.
			_, _ = tg.GetUpdates(ctx, offset, 0)
			return store.TelegramOwner{ID: m.From.ID, Name: m.From.Name()}, nil
		}
	}
	return store.TelegramOwner{}, errors.New("сообщение так и не пришло")
}

func cmdTelegramUnlink(args []string) error {
	fs := flag.NewFlagSet("telegram unlink", flag.ExitOnError)
	id := fs.Int64("id", 0, "Telegram ID, которому бот перестанет отвечать")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == 0 {
		return errors.New("нужен -id")
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	if st.Telegram == nil {
		return errors.New("владельцев нет")
	}
	kept := st.Telegram.Owners[:0]
	for _, o := range st.Telegram.Owners {
		if o.ID != *id {
			kept = append(kept, o)
		}
	}
	if len(kept) == len(st.Telegram.Owners) {
		return fmt.Errorf("%d нет среди владельцев", *id)
	}
	st.Telegram.Owners = kept
	if len(kept) == 0 {
		st.Telegram.Approval = false
		fmt.Println("Владельцев не осталось: бот выключится, одобрение тоже (одобрять было бы некому).")
	}
	if err := st.Save(root); err != nil {
		return err
	}
	fmt.Println("Убран. На сервер это попадёт через awgctl update.")
	return nil
}

func cmdTelegramShow([]string) error {
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
	if secrets.TelegramToken == "" {
		fmt.Printf("Токена бота нет (%s в secrets.env).\n", config.TelegramKeyName)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if me, err := telegram.New(secrets.TelegramToken).GetMe(ctx); err != nil {
			fmt.Printf("Токен есть, но Telegram его не подтвердил: %v\n", err)
		} else {
			fmt.Printf("Бот: @%s\n", me.Username)
		}
	}
	if st.Telegram == nil || len(st.Telegram.Owners) == 0 {
		fmt.Println("Владельцев нет: awgctl telegram link")
		return nil
	}
	fmt.Println("Владельцы:")
	for _, o := range st.Telegram.Owners {
		fmt.Printf("  %d  %s\n", o.ID, o.Name)
	}
	fmt.Printf("Одобрение новых устройств: %s (awgctl update -approval on|off)\n", onOffWord(st.Telegram.Approval))
	return nil
}
