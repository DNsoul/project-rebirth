package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/gitlab"
	"awgrotator/internal/store"
)

// cmdMirrorToken makes the token servers use to write to GitLab. It is a deploy
// token of the project: it can read and write the project's package registry and
// reaches nothing else, so a server that is taken or lost gives away no more than
// the ability to spoil the copies, which an app does not trust without checking
// anyway. GitLab shows the value once; it is kept in the operator's state, next
// to the keys, and never printed.
func cmdMirrorToken(args []string) error {
	fs := flag.NewFlagSet("mirror token", flag.ExitOnError)
	renew := fs.Bool("new", false, "выпустить новый токен, даже если он уже есть (старый отзови на GitLab сам)")
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
	m, ok := st.GitLabMirror()
	if !ok {
		return errors.New("второго места ещё нет: сначала awgctl mirror set gitlab -project владелец/имя")
	}
	if m.DeployToken != "" && !*renew {
		fmt.Println("Токен для серверов уже выпущен и лежит в state/. Новый: awgctl mirror token -new")
		return nil
	}
	if secrets.GitLabToken == "" {
		return errors.New("нужен GITLAB_TOKEN в secrets.env, с правом api: им выпускается токен для серверов")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	token, err := gitlab.New(m.Base, secrets.GitLabToken).CreateDeployToken(ctx, m.ProjectID, "rebirth servers")
	if err != nil {
		return err
	}
	// Kept before anything else is tried: GitLab will not show it again.
	m.DeployToken = token
	st.SetMirror(m)
	if err := st.Save(root); err != nil {
		return fmt.Errorf("токен выпущен, но сохранить его не удалось: %w", err)
	}
	fmt.Println("Токен выпущен и сохранён в state/ (на экран не выводится).")

	// Proof that it does what a server needs: write with it, read without it.
	fmt.Println("Проверяю: запись этим токеном и чтение без токена…")
	writer := gitlab.New(m.Base, token)
	writer.Deploy = true
	if err := writer.PutFile(ctx, m.ProjectID, m.SyncPkg(), gitlab.SyncVersion, "probe", []byte("ok")); err != nil {
		return fmt.Errorf("токен выпущен, но записать им не получается: %w", err)
	}
	got, found, err := gitlab.New(m.Base, "").GetFile(ctx, m.ProjectID, m.SyncPkg(), gitlab.SyncVersion, "probe")
	if err != nil {
		return err
	}
	if !found || string(got) != "ok" {
		return errors.New("записанный файл без токена не читается: проект публичный, а реестр пакетов открыт для всех?")
	}
	fmt.Println("Работает.")
	fmt.Println("Серверам он попадёт при следующем переезде. Работающему серверу сейчас: awgctl update")
	return nil
}

// cmdMirrorSync compares the places the signed files are kept in and copies the
// newest of each file to the place that is behind, then clears away the old
// uploads GitLab keeps. The server does the same by itself now and then; this is
// for filling a place for the first time and for not waiting.
func cmdMirrorSync(args []string) error {
	fs := flag.NewFlagSet("mirror sync", flag.ExitOnError)
	keep := fs.Int("keep", 5, "сколько последних загрузок каждого файла оставить на GitLab")
	noPrune := fs.Bool("no-prune", false, "не убирать старые загрузки")
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
	m, ok := st.GitLabMirror()
	if !ok {
		return errors.New("второго места ещё нет: сначала awgctl mirror set gitlab -project владелец/имя")
	}
	where, err := openPlaces(st, secrets)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep, err := where.set.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("ни одно место не отвечает: %w", err)
	}
	for _, s := range where.set.Stores {
		name := s.Name()
		switch {
		case rep.Missed[name] != nil:
			fmt.Printf("  %-7s недоступно (%s)\n", name, firstLine(rep.Missed[name]))
		case rep.Failed[name] != nil:
			fmt.Printf("  %-7s догнать не удалось (%s)\n", name, firstLine(rep.Failed[name]))
		case len(rep.Copied[name]) > 0:
			fmt.Printf("  %-7s догнал: %v\n", name, rep.Copied[name])
		default:
			fmt.Printf("  %-7s актуально\n", name)
		}
	}

	if !*noPrune && secrets.GitLabToken != "" {
		c := gitlab.New(m.Base, secrets.GitLabToken)
		n, err := c.PruneFiles(ctx, m.ProjectID, m.SyncPkg(), gitlab.SyncVersion, *keep)
		switch {
		case err != nil:
			fmt.Printf("Старые загрузки на GitLab убрать не удалось: %v\n", firstLine(err))
		case n > 0:
			fmt.Printf("Убрано старых загрузок на GitLab: %d.\n", n)
		}
	}

	if !rep.Synced() {
		return errors.New("не всё совпало: повтори позже, сервер тоже будет догонять сам")
	}
	fmt.Println("Оба места совпадают.")
	return nil
}
