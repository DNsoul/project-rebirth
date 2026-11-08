// Command awgctl is the operator's tool. It creates the keys, the gist and
// the servers, and later hands the running fleet over to the phone.
//
// It reads secrets.env from the project root and keeps its state in state/.
// Neither is ever committed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/gist"
	"awgrotator/internal/proto"
	"awgrotator/internal/store"
	"awgrotator/internal/vultr"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"init":          cmdInit,
		"up":            cmdUp,
		"down":          cmdDown,
		"client-conf":   cmdClientConf,
		"run":           cmdRun,
		"ru-update":     cmdRuUpdate,
		"ru-check":      cmdRuCheck,
		"split-check":   cmdSplitCheck,
		"update":        cmdUpdate,
		"command":       cmdCommand,
		"events":        cmdEvents,
		"release":       cmdRelease,
		"sync":          cmdSync,
		"gc":            cmdGC,
		"auto":          cmdAuto,
		"verify":        cmdVerify,
		"check-keys":    cmdCheckKeys,
		"lab-tunnel":    cmdLabTunnel,
		"lab-up":        cmdLabUp,
		"lab-list":      cmdLabList,
		"lab-down":      cmdLabDown,
		"status":        cmdStatus,
		"trust":         cmdTrust,
		"mirror":        cmdMirror,
		"export-backup": cmdExportBackup,
	}
	fn, ok := cmds[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := fn(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "awgctl %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `awgctl — управление своим VPN

  init            создать ключи и секретный gist (один раз)
  up              поднять новый сервер и опубликовать конфиг
  down            удалить сервер
  client-conf     вывести опубликованный конфиг клиента
  run "<cmd>"     выполнить команду на сервере по SSH
  ru-update       скачать российские сети из RIPE
  ru-check        проверить сайты по набору российских сетей
  split-check     проверить правила nft на сервере (без применения)
  update          обновить узел и менеджер на текущем сервере (-files — и списки)
  command         приказать серверу: move | move-region <регион> | status | revoke <N>
  events          показать, что сервер сообщает о себе
  release         объявить новую версию приложений: publish | show
  mirror          второе место (GitLab) для установщиков и файлов конфигурации:
                  set gitlab -project владелец/имя | token | sync | show
  sync            узнать, какой сервер сейчас текущий (после самостоятельного переезда)
  gc              найти и удалить лишние серверы, которые никому не нужны
  auto            включить или выключить самостоятельные переезды (-age, -block)
  verify          проверить опубликованный сервер настоящим клиентом
  check-keys      проверить, что ключи Vultr и GitHub работают с адреса сервера
  lab-up          поднять опытный сервер (-probe — с зондом)
  lab-tunnel      замерить рукопожатие на настоящем сервере (создаёт и удаляет сервер)
  lab-list        показать опытные серверы
  lab-down        удалить опытные серверы
  status          показать состояние: ключи, gist, серверы
  trust           выдать набор для сборки приложений
  export-backup   показать запасной ключ для офлайн-хранения

Корень проекта ищется от текущей папки или берётся из AWGCTL_DIR.
`)
}

// ---------------------------------------------------------------- init

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("force", false, "перезаписать существующее состояние (старые ключи пропадут)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := config.Root()
	if err != nil {
		return err
	}
	if store.Exists(root) && !*force {
		return fmt.Errorf("состояние уже есть: %s\nПовтор init создаст новые ключи, и все выданные приложения перестанут подходить. Если это нужно — добавь -force", store.Path(root))
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Println("Создаю ключи…")
	st, err := store.New()
	if err != nil {
		return err
	}
	contentKey, err := st.ContentKey()
	if err != nil {
		return err
	}

	// Проверяем Vultr до того, как что-то создавать на стороне GitHub.
	fmt.Println("Проверяю доступ к Vultr…")
	vc := vultr.New(secrets.VultrAPIKey)
	regions, err := vc.Regions(ctx)
	if err != nil {
		return fmt.Errorf("Vultr не отвечает: %w", err)
	}
	if err := checkRegions(regions); err != nil {
		return err
	}

	fmt.Println("Создаю секретный gist…")
	files := map[string]string{}
	for _, name := range proto.AllFiles {
		// Каждый файл шифруется своим вызовом, поэтому одинаковое
		// содержимое не выглядит одинаково.
		blob, err := proto.Seal(contentKey, []byte("{}"))
		if err != nil {
			return err
		}
		files[name] = blob
	}

	gc := gist.New(secrets.GitHubToken)
	id, owner, err := gc.Create(ctx, files)
	if err != nil {
		return fmt.Errorf("не удалось создать gist: %w", err)
	}
	st.GistID = id
	st.GistOwner = owner

	if err := st.Save(root); err != nil {
		return fmt.Errorf("ключи созданы, но сохранить не удалось: %w", err)
	}

	trust, err := st.Trust()
	if err != nil {
		return err
	}

	fmt.Printf(`
Готово.

  Состояние   %s
  Gist        %s (владелец %s)
  Поколение   %d
  Главный     %s
  Запасной    %s

Что важно сделать сейчас:

  1. Сохрани запасной ключ вне этого компьютера:
       awgctl export-backup -yes
     Он нужен, если телефон с главным ключом потеряется.

  2. Папка state/ — это все ключи сразу. Её потеря означает потерю
     управления серверами, а утечка — возможность выдавать свои конфиги.

Серверов пока нет. Следующий шаг — "awgctl up".
`, store.Path(root), id, owner, st.Epoch, short(trust.RootPub), short(trust.BackupPub))

	return nil
}

func checkRegions(regions []vultr.Region) error {
	have := map[string]bool{}
	for _, r := range regions {
		have[r.ID] = true
	}
	var missing []string
	for _, want := range config.Regions {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("Vultr не знает регионы: %s", strings.Join(missing, ", "))
	}
	return nil
}

// -------------------------------------------------------------- status

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
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
	trust, err := st.Trust()
	if err != nil {
		return err
	}

	syncIfPossible(root, st)

	fmt.Printf("Состояние  %s\n", store.Path(root))
	fmt.Printf("Создано    %s\n", time.Unix(st.CreatedAt, 0).Format("2006-01-02 15:04"))
	fmt.Printf("Поколение  %d, публикаций %d\n", st.Epoch, st.Seq)
	fmt.Printf("Главный    %s\n", short(trust.RootPub))
	fmt.Printf("Запасной   %s\n", short(trust.BackupPub))

	secrets, err := config.LoadSecrets(root)
	if err != nil {
		fmt.Printf("\nСекреты: %v\n", err)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	fmt.Printf("\nГде лежат файлы (gist %s)\n", st.GistID)
	reportPlaces(ctx, secrets, st)

	fmt.Println("\nVultr")
	reportVultr(ctx, secrets.VultrAPIKey, st)
	return nil
}

func reportPlaces(ctx context.Context, secrets config.Secrets, st *store.State) {
	if st.GistID == "" {
		fmt.Println("  не создано")
		return
	}
	where, err := openPlaces(st, secrets)
	if err != nil {
		fmt.Printf("  %v\n", err)
		return
	}
	fmt.Print(describePlaces(ctx, where))
}

func reportVultr(ctx context.Context, key string, st *store.State) {
	vc := vultr.New(key)
	ours, err := vc.InstancesByTag(ctx, config.InstanceTag)
	if err != nil {
		fmt.Printf("  API не отвечает: %v\n", err)
		return
	}
	if len(ours) == 0 {
		fmt.Println("  наших серверов нет")
	}
	for _, i := range ours {
		mark := " "
		if i.ID == st.Server.ID {
			mark = "*"
		}
		fmt.Printf("  %s %s  %-4s %-15s %s/%s\n", mark, i.ID[:8], i.Region, i.MainIP, i.Status, i.PowerStatus)
	}

	all, err := vc.Instances(ctx)
	if err == nil && len(all) > len(ours) {
		fmt.Printf("  (ещё %d чужих серверов на аккаунте, их не трогаем)\n", len(all)-len(ours))
	}
}

// --------------------------------------------------------------- trust

func cmdTrust(args []string) error {
	fs := flag.NewFlagSet("trust", flag.ExitOnError)
	outPath := fs.String("o", "", "записать в файл (для сборки приложения)")
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
	trust, err := st.Trust()
	if err != nil {
		return err
	}
	if st.GistID == "" {
		return errors.New("gist ещё не создан")
	}

	raw, err := json.MarshalIndent(newBootstrapBundle(trust, st), "", "  ")
	if err != nil {
		return err
	}
	if *outPath == "" {
		fmt.Println(string(raw))
		fmt.Fprintln(os.Stderr, "\nСовет: для сборки приложения пиши в файл через -o: оболочка добавляет метку BOM.")
		return nil
	}
	// The bundle is what holds access: whoever has it can read the published
	// configuration. It is written with owner-only permissions.
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*outPath, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("Записано в %s. Этот файл и есть доступ: он не должен попадать в репозиторий.\n", *outPath)
	return nil
}

// ------------------------------------------------------- export-backup

func cmdExportBackup(args []string) error {
	fs := flag.NewFlagSet("export-backup", flag.ExitOnError)
	yes := fs.Bool("yes", false, "подтвердить вывод закрытого ключа на экран")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*yes {
		return errors.New("это выведет закрытый ключ на экран. Если рядом никого и экран не пишется — повтори с -yes")
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	backup, err := st.BackupKey()
	if err != nil {
		return err
	}

	fmt.Printf(`Запасной ключ. Перенеси его на бумагу или на флешку и убери
подальше от этого компьютера. С ним можно отозвать потерянный телефон.

  %s

Открытая часть (её же показывает "awgctl trust"):

  %s
`, st.Keys.BackupSeed, proto.Encode(proto.PublicOf(backup)))
	return nil
}

func short(key string) string {
	if len(key) <= 16 {
		return key
	}
	return key[:16] + "…"
}
