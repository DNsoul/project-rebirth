package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/deploy"
	"awgrotator/internal/fleet"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/proto"
	"awgrotator/internal/remote"
	"awgrotator/internal/store"
	"awgrotator/internal/vultr"
)

// certLifetime is how long the server key's certificate is valid.
//
// A server cannot make its own certificate, so every replacement inherits this
// one, and a line of servers stops being able to publish when it runs out.
// A year gives plenty of room to renew it, and the status shows the days left.
const certLifetime = 365 * 24 * time.Hour

// buildManagerConfig assembles what a server needs to look after itself,
// from the operator's state and credentials.
func buildManagerConfig(st *store.State, secrets config.Secrets, sshKeyID string,
	inst *vultr.Instance, cert proto.Cert, now time.Time) (*lifecycle.Config, error) {

	trust, err := st.Trust()
	if err != nil {
		return nil, err
	}
	cfg := &lifecycle.Config{
		Version:     lifecycle.ConfigVersion,
		InstanceID:  inst.ID,
		IP:          inst.MainIP,
		Region:      inst.Region,
		CreatedAt:   now.Unix(),
		Generation:  st.Generation + 1,
		VultrKey:    secrets.VultrAPIKey,
		GitHubToken: secrets.GitHubToken,
		GistID:      st.GistID,
		GitLab:      gitLabSyncFor(st),
		ServerSeed:  st.Keys.ServerSeed,
		Cert:        cert,
		ContentKey:  st.Keys.ContentKey,
		RootPub:     trust.RootPub,
		BackupPub:   trust.BackupPub,
		SSHPrivate:  st.SSHPrivate,
		SSHKeyID:    sshKeyID,
		Plan:        config.Plan,
		OsID:        config.OsID,
		Tag:         config.InstanceTag,
		Regions:     config.Regions,
		Policy:      lifecycle.DefaultPolicy(),
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("настройки менеджера: %w", err)
	}
	return cfg, nil
}

// gitLabSyncFor is what a server's configuration says about the second place the
// files are kept, from the operator's state: nothing when there is no GitLab
// mirror or no token for servers has been made yet.
func gitLabSyncFor(st *store.State) *lifecycle.GitLabSync {
	m, ok := st.GitLabMirror()
	if !ok || m.DeployToken == "" {
		return nil
	}
	return &lifecycle.GitLabSync{Base: m.Base, Project: m.ProjectID, Package: m.SyncPkg(), Token: m.DeployToken}
}

// managerFiles are the files that make a server look after itself.
func managerFiles(cfg *lifecycle.Config, body *fleet.ConfigBody, managerBinary []byte) ([]deploy.File, error) {
	cfgJSON, err := cfg.Marshal()
	if err != nil {
		return nil, err
	}
	bodyJSON, err := body.Marshal()
	if err != nil {
		return nil, err
	}
	return []deploy.File{
		{What: "менеджер", Path: lifecycle.ManagerBinaryPath, Mode: "0700", Content: managerBinary},
		{What: "служба менеджера", Path: lifecycle.ManagerUnitPath, Mode: "0644", Content: []byte(lifecycle.ManagerUnit())},
		{What: "настройки менеджера", Path: lifecycle.DefaultConfigPath, Mode: "0600", Content: cfgJSON},
		{What: "опубликованный конфиг", Path: cfg.Data() + "/body.json", Mode: "0600", Content: bodyJSON},
	}, nil
}

// ---- command

func cmdCommand(args []string) error {
	fs := flag.NewFlagSet("command", flag.ExitOnError)
	force := fs.Bool("force", false, "отправить переезд, даже если живых серверов с меткой больше одного")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New(`нужно указать команду: move | move-region <регион> | status | revoke <поколение>`)
	}

	var action, arg string
	switch rest[0] {
	case "move":
		action = proto.ActionMove
	case "move-region":
		if len(rest) < 2 {
			return fmt.Errorf("нужен регион из списка: %v", config.Regions)
		}
		action, arg = proto.ActionMoveRegion, rest[1]
		known := false
		for _, r := range config.Regions {
			known = known || r == arg
		}
		if !known {
			return fmt.Errorf("регион %q не из списка: %v", arg, config.Regions)
		}
	case "status":
		action = proto.ActionStatus
	case "revoke":
		if len(rest) < 2 {
			return errors.New("нужно указать, начиная с какого поколения ключей доверять")
		}
		if _, err := strconv.ParseUint(rest[1], 10, 32); err != nil {
			return fmt.Errorf("поколение %q не число", rest[1])
		}
		action, arg = proto.ActionRevoke, rest[1]
	default:
		return fmt.Errorf("неизвестная команда %q", rest[0])
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

	// Every live server reads the same command file and carries a command out.
	// With two of them, a move builds two replacements, and the second to
	// notice runs into the ceiling halfway. That was found by leaving the
	// previous server running with -keep-old.
	if (action == proto.ActionMove || action == proto.ActionMoveRegion) && !*force {
		lookCtx, lookCancel := context.WithTimeout(context.Background(), 30*time.Second)
		ours, err := vultr.New(secrets.VultrAPIKey).InstancesByTag(lookCtx, config.InstanceTag)
		lookCancel()
		if err == nil && len(ours) > 1 {
			var list []string
			for _, in := range ours {
				list = append(list, fmt.Sprintf("%s (%s, %s)", in.ID[:8], in.Region, in.MainIP))
			}
			return fmt.Errorf("живых серверов с меткой %d: %s.\n"+
				"Каждый из них читает команды и выполнит этот переезд, так что серверов получится больше нужного.\n"+
				"Удали лишние (awgctl down -id …) или повтори с ключом -force", len(ours), strings.Join(list, ", "))
		}
	}

	id, err := proto.NewCommandID()
	if err != nil {
		return err
	}
	rootKey, err := st.RootKey()
	if err != nil {
		return err
	}
	contentKey, err := st.ContentKey()
	if err != nil {
		return err
	}
	blob, err := proto.SealCommand(&proto.Command{
		ID: id, IssuedAt: time.Now().Unix(), Action: action, Arg: arg,
	}, proto.IssuerRoot, rootKey, contentKey)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	where, err := openPlaces(st, secrets)
	if err != nil {
		return err
	}
	if err := where.Write(ctx, map[string]string{proto.FileCommand: blob}); err != nil {
		return err
	}

	fmt.Printf("Команда «%s» отправлена.\n", rest[0])
	fmt.Println("Сервер читает команды раз в минуту. Следить: awgctl events")
	if action == proto.ActionMove || action == proto.ActionMoveRegion {
		fmt.Println("Переезд занимает несколько минут; после него прежний сервер удаляется сам.")
	}
	return nil
}

// ---- events

func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	where, err := openPlaces(st, secrets)
	if err != nil {
		return err
	}
	files, err := where.Read(ctx, proto.FileStatus)
	if err != nil {
		return err
	}
	blob := files[proto.FileStatus]
	if blob == "" {
		return errors.New("статуса нигде нет")
	}
	trust, err := st.Trust()
	if err != nil {
		return err
	}
	doc, _, err := trust.OpenDoc(blob, proto.KindStatus, time.Now(), proto.State{})
	if err != nil {
		// A placeholder is a valid envelope with nothing in it.
		if key, kerr := st.ContentKey(); kerr == nil {
			if plain, oerr := proto.Open(key, blob); oerr == nil && string(plain) == "{}" {
				return errors.New("сервер ещё не публиковал статус")
			}
		}
		return fmt.Errorf("опубликованный статус не проходит проверку: %w", err)
	}
	status, err := lifecycle.ParseStatus([]byte(doc.Body))
	if err != nil {
		return err
	}

	fmt.Println(status.Describe(time.Now()))
	age := time.Since(time.Unix(status.UpdatedAt, 0)).Round(time.Second)
	fmt.Printf("\nстатус опубликован %s назад\n", age)
	if age > 36*time.Hour {
		fmt.Println("ВНИМАНИЕ: статус давно не обновлялся, менеджер на сервере, возможно, не работает")
	}
	return nil
}

// ---- following the server

// refreshServer finds out which server is current and updates the state to
// match.
//
// Servers that replace themselves do not tell the operator's machine, so the
// state can be out of date. The published configuration is the truth: it names
// the address clients are told to use, and the instance holding that address
// is the current server.
func refreshServer(ctx context.Context, root string, st *store.State, secrets config.Secrets) (changed bool, err error) {
	where, err := openPlaces(st, secrets)
	if err != nil {
		return false, err
	}
	files, err := where.Read(ctx, proto.FileConfig)
	if err != nil {
		return false, err
	}
	blob, ok := files[proto.FileConfig]
	if !ok {
		return false, errors.New("нигде нет конфига")
	}
	trust, err := st.Trust()
	if err != nil {
		return false, err
	}
	doc, state, err := trust.OpenDoc(blob, proto.KindConfig, time.Now(), proto.State{})
	if err != nil {
		return false, fmt.Errorf("опубликованный конфиг не проходит проверку: %w", err)
	}
	body, err := fleet.ParseBody([]byte(doc.Body))
	if err != nil {
		return false, err
	}
	ip := strings.SplitN(body.Endpoint, ":", 2)[0]

	// The publication counter on this machine must never be behind what is
	// actually published, or the next configuration would be rejected as old.
	if state.LastSeq > st.Seq {
		st.Seq = state.LastSeq
		changed = true
	}

	instances, err := vultr.New(secrets.VultrAPIKey).InstancesByTag(ctx, config.InstanceTag)
	if err != nil {
		return changed, err
	}
	found := false
	for _, inst := range instances {
		if inst.MainIP != ip {
			continue
		}
		found = true
		if st.Server.ID != inst.ID {
			st.Server = store.Server{ID: inst.ID, Region: inst.Region, IP: inst.MainIP, CreatedAt: time.Now().Unix()}
			// A different machine has a different host key. It is trusted on
			// first use again.
			st.ServerHostKey = ""
			changed = true
		}
		break
	}
	if changed {
		if err := st.Save(root); err != nil {
			return changed, err
		}
	}
	if !found {
		// Saying nothing here would let every caller carry on as if the state
		// were right. It is not: clients have been told to connect to a
		// machine that does not exist.
		return changed, fmt.Errorf("%w: опубликован адрес %s, но среди наших серверов на аккаунте такого нет", errPublishedServerGone, ip)
	}
	return changed, nil
}

// errPublishedServerGone means the configuration clients are using points at
// a server that is no longer on the account.
var errPublishedServerGone = errors.New("опубликованного сервера больше нет")

// syncQuiet refreshes the current server and mentions it when it moved. A
// failure is not fatal: the commands that call it can often still work with
// what the state already says.
func syncQuiet(root string, st *store.State, secrets config.Secrets) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	before := st.Server.IP
	changed, err := refreshServer(ctx, root, st, secrets)
	switch {
	case errors.Is(err, errPublishedServerGone):
		fmt.Printf("ВНИМАНИЕ: %v.\nКлиентам некуда подключаться. Поднять новый сервер: awgctl up\n", err)
	case err != nil:
		fmt.Printf("(не удалось уточнить, какой сервер текущий: %v)\n", err)
	case changed && st.Server.IP != before:
		fmt.Printf("(сервер сменился сам: теперь %s, %s)\n", st.Server.IP, st.Server.Region)
	}
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	changed, err := refreshServer(ctx, root, st, secrets)
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("Состояние обновлено: сервер %s, %s, %s\n", st.Server.ID[:8], st.Server.Region, st.Server.IP)
	} else {
		fmt.Printf("Состояние верно: сервер %s, %s, %s\n", st.Server.ID[:8], st.Server.Region, st.Server.IP)
	}
	return nil
}

// ---- gc

// orphanMinAge is how old an unpublished server must be before it counts as
// left over. A server being built right now is unpublished too, for a few
// minutes, and must not be mistaken for one that was abandoned.
const orphanMinAge = 30 * time.Minute

func cmdGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	yes := fs.Bool("yes", false, "удалить найденные лишние серверы")
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

	if _, err := refreshServer(ctx, root, st, secrets); err != nil {
		return fmt.Errorf("не удалось узнать, какой сервер опубликован, поэтому ничего не трогаю: %w", err)
	}
	vc := vultr.New(secrets.VultrAPIKey)
	instances, err := vc.InstancesByTag(ctx, config.InstanceTag)
	if err != nil {
		return err
	}

	var orphans []vultr.Instance
	for _, inst := range instances {
		created, _ := time.Parse(time.RFC3339, inst.DateCreated)
		switch {
		case inst.ID == st.Server.ID:
			fmt.Printf("  %s  %-4s %-15s опубликован, текущий\n", inst.ID[:8], inst.Region, inst.MainIP)
		case !created.IsZero() && time.Since(created) < orphanMinAge:
			fmt.Printf("  %s  %-4s %-15s создан недавно, возможно строится сейчас\n", inst.ID[:8], inst.Region, inst.MainIP)
		default:
			fmt.Printf("  %s  %-4s %-15s НЕ опубликован, лишний\n", inst.ID[:8], inst.Region, inst.MainIP)
			orphans = append(orphans, inst)
		}
	}
	if len(orphans) == 0 {
		fmt.Println("Лишних серверов нет.")
		return nil
	}
	if !*yes {
		return fmt.Errorf("лишних серверов: %d. Чтобы удалить, добавь -yes", len(orphans))
	}
	for _, inst := range orphans {
		if err := vc.DeleteInstance(ctx, inst.ID); err != nil {
			fmt.Printf("  %s: %v\n", inst.ID[:8], err)
			continue
		}
		fmt.Printf("  %s удалён\n", inst.ID[:8])
	}
	return nil
}

// ---- auto

// policyChange is what `awgctl auto` was asked to alter; a nil or zero field is
// left as it is.
type policyChange struct {
	age, block      *bool
	ageHours        int
	jitterHours     int
	jitterSet       bool
	maxMovesPerDay  int
	minMoveMinutes  int
	graceMinutes    int
	minGraceMinutes int
}

func (c policyChange) any() bool {
	return c.age != nil || c.block != nil || c.ageHours != 0 || c.jitterSet ||
		c.maxMovesPerDay != 0 || c.minMoveMinutes != 0 || c.graceMinutes != 0
}

// apply changes the policy and checks that the result is one the manager would
// accept. A zero means "not given" for most fields, and the library would turn
// a zero into its default, so a value that was given must be positive.
func (c policyChange) apply(p *lifecycle.Policy) error {
	for name, v := range map[string]int{
		"-age-hours": c.ageHours, "-jitter-hours": c.jitterHours,
		"-moves-per-day": c.maxMovesPerDay, "-min-move-minutes": c.minMoveMinutes,
		"-grace-minutes": c.graceMinutes,
	} {
		if v < 0 {
			return fmt.Errorf("%s: число не может быть отрицательным", name)
		}
	}
	if c.age != nil {
		p.AutoAge = *c.age
	}
	if c.block != nil {
		p.AutoBlock = *c.block
	}
	if c.ageHours != 0 {
		p.MaxAgeHours = c.ageHours
	}
	if c.jitterSet {
		p.AgeJitterHours = c.jitterHours
	}
	if c.maxMovesPerDay != 0 {
		p.MaxMovesPerDay = c.maxMovesPerDay
	}
	if c.minMoveMinutes != 0 {
		p.MinMoveMinutes = c.minMoveMinutes
	}
	if c.graceMinutes != 0 {
		p.GraceMinutes = c.graceMinutes
		if p.MinGraceMinutes > p.GraceMinutes {
			p.MinGraceMinutes = p.GraceMinutes
		}
	}
	return p.Normalize()
}

func cmdAuto(args []string) error {
	fs := flag.NewFlagSet("auto", flag.ExitOnError)
	age := fs.String("age", "", "переезд по возрасту: on или off")
	block := fs.String("block", "", "переезд при блокировке или поломке: on или off")
	ageHours := fs.Int("age-hours", 0, "через сколько часов сервер заменяется")
	jitterHours := fs.Int("jitter-hours", 0, "случайная добавка к возрасту, часов (0 — без неё)")
	perDay := fs.Int("moves-per-day", 0, "потолок самостоятельных переездов в сутки")
	minMove := fs.Int("min-move-minutes", 0, "минимальная пауза между самостоятельными переездами")
	grace := fs.Int("grace-minutes", 0, "сколько минут старый сервер живёт после публикации нового")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Zero is a real answer for the jitter ("none"), so whether it was given is
	// asked of the flag set rather than guessed from the value.
	jitterGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "jitter-hours" {
			jitterGiven = true
		}
	})

	toggle := func(name, v string) (*bool, error) {
		switch v {
		case "":
			return nil, nil
		case "on":
			t := true
			return &t, nil
		case "off":
			f := false
			return &f, nil
		}
		return nil, fmt.Errorf("-%s: нужно on или off, а не %q", name, v)
	}
	ageSet, err := toggle("age", *age)
	if err != nil {
		return err
	}
	blockSet, err := toggle("block", *block)
	if err != nil {
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
	if st.ServerHostKey == "" {
		st.ServerHostKey = cli.HostKey()
		if err := st.Save(root); err != nil {
			return err
		}
	}

	raw, err := cli.Run("cat " + lifecycle.DefaultConfigPath)
	if err != nil {
		return fmt.Errorf("на сервере нет менеджера: %w", err)
	}
	tmp, err := lifecycle.ParseConfig([]byte(raw))
	if err != nil {
		return err
	}

	change := policyChange{
		age: ageSet, block: blockSet,
		ageHours: *ageHours, jitterHours: *jitterHours, jitterSet: jitterGiven,
		maxMovesPerDay: *perDay, minMoveMinutes: *minMove, graceMinutes: *grace,
	}
	if change.any() {
		if err := change.apply(&tmp.Policy); err != nil {
			return err
		}
		data, err := tmp.Marshal()
		if err != nil {
			return err
		}
		if err := cli.Upload(lifecycle.DefaultConfigPath, "0600", data); err != nil {
			return err
		}
		if _, err := cli.Run("systemctl restart " + lifecycle.ManagerService); err != nil {
			return fmt.Errorf("менеджер не перезапустился: %w", err)
		}
		fmt.Println("Менеджер перезапущен с новыми настройками.")
	}

	onOff := func(b bool) string {
		if b {
			return "включён"
		}
		return "выключен"
	}
	fmt.Printf("Переезд по возрасту:            %s (через %d ч и до %d ч сверху)\n",
		onOff(tmp.Policy.AutoAge), tmp.Policy.MaxAgeHours, tmp.Policy.AgeJitterHours)
	fmt.Printf("Переезд при блокировке/поломке: %s\n", onOff(tmp.Policy.AutoBlock))
	fmt.Printf("Потолок: %d серверов, %d переездов в сутки, пауза между переездами %d мин, старый сервер живёт ещё %d мин\n",
		tmp.Policy.MaxServers, tmp.Policy.MaxMovesPerDay, tmp.Policy.MinMoveMinutes, tmp.Policy.GraceMinutes)
	return nil
}

// syncIfPossible refreshes the current server for a command that has no
// credentials loaded of its own. Without credentials it quietly does nothing.
func syncIfPossible(root string, st *store.State) {
	if secrets, err := config.LoadSecrets(root); err == nil {
		syncQuiet(root, st, secrets)
	}
}
