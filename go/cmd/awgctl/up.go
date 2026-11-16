package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/config"
	"awgrotator/internal/deploy"
	"awgrotator/internal/fleet"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/proto"
	"awgrotator/internal/store"
)

// How long a published document stays valid. The certificate, issued once for
// the whole line of servers, outlives it by a wide margin.
const docValidity = 60 * 24 * time.Hour

// oldServerGrace is how long the previous server keeps running after a new
// one is published, so connected clients have time to move over.
const oldServerGrace = 15 * time.Minute

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	wantPlace := fs.String("place", "", "место: хостер:город, например vultr:sto или cherry:ams (по умолчанию случайное почасовое)")
	wantRegion := fs.String("region", "", "город Vultr (то же, что -place vultr:<город>)")
	keepOld := fs.Bool("keep-old", false, "не удалять предыдущий сервер")
	noSplit := fs.Bool("no-split", false, "полный туннель: всё через сервер, без разделения по сайтам")
	noManager := fs.Bool("no-manager", false, "не ставить менеджер: сервер не будет следить за собой и переезжать сам")
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
	if st.GistID == "" {
		return errors.New("gist не создан, запусти \"awgctl init\"")
	}

	logf := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }

	// Servers that replace themselves do not tell this machine, so first find
	// out which one is current: it is the one to retire, and the publication
	// counter must not be behind what is published.
	syncQuiet(root, st, secrets)

	logf("Собираю узел под Linux…")
	binary, err := buildNode(root)
	if err != nil {
		return err
	}
	logf("  %d КБ", len(binary)/1024)

	want := *wantPlace
	if want == "" && *wantRegion != "" {
		want = "vultr:" + *wantRegion
	}
	clouds, err := openClouds(st, secrets, config.InstanceTag)
	if err != nil {
		return err
	}
	place, err := pickPlace(want, placesFor(cloudSettings(st, secrets, config.InstanceTag)),
		cloud.Place{Provider: st.Server.Provider, Location: st.Server.Region})
	if err != nil {
		return err
	}
	region := place.Place().Location
	provider, err := clouds.Get(place.Place().Provider)
	if err != nil {
		return err
	}
	if place.Billing == cloud.Monthly {
		logf("Место %s оплачивается помесячно: месяц спишется вперёд.", place.ID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// The key is registered with every provider, not only this one: the
	// server may later build its replacement at any of them.
	var keyID string
	for name, p := range clouds {
		id, err := ensureSSHKey(ctx, p, st, root, logf)
		if err != nil {
			return err
		}
		if name == provider.Name() {
			keyID = id
		}
	}

	// Fresh keys and obfuscation parameters for this generation, so two
	// servers never look alike on the wire.
	contentKey, err := st.ContentKey()
	if err != nil {
		return err
	}
	tg := telegramFor(st, secrets)
	gen, err := fleet.NewGeneration(fleet.GenOptions{
		Split: !*noSplit, ContentKey: contentKey, Approval: tg != nil && tg.Approval,
	})
	if err != nil {
		return err
	}
	nodeCfg, port := gen.Node, gen.Port

	// Split routing is the default. The client then sends only the stand-in
	// range and Telegram into the tunnel and everything else goes out over its
	// own connection; the resolver on the server decides per site.
	clientNet := fleet.FullTunnel
	var extraFiles []deploy.File
	if !*noSplit {
		plan, err := prepareSplit(ctx, root, logf)
		if err != nil {
			return err
		}
		extraFiles = plan.Files
		clientNet = fleet.ClientNet{DNS: plan.DNS, AllowedIPs: plan.AllowedIPs}
	}

	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}

	now := time.Now()
	// One certificate for the whole line of servers: a server cannot make its
	// own, so every replacement inherits this one.
	cert, err := st.NewCert(certLifetime, now)
	if err != nil {
		return err
	}

	// With a manager the server looks after itself and builds its own
	// replacements; the files for that need the instance's identity, so they
	// are made once it exists. The configuration published for clients is
	// built at the same moment and published unchanged, so what the manager
	// later republishes is exactly what clients were given.
	var body *fleet.ConfigBody
	var filesFor func(inst *cloud.Server) ([]deploy.File, error)
	var services []string
	if !*noManager {
		logf("Собираю менеджер под Linux…")
		managerBinary, err := buildLinux(root, "awgmgr")
		if err != nil {
			return err
		}
		services = []string{lifecycle.ManagerService}
		filesFor = func(inst *cloud.Server) ([]deploy.File, error) {
			body = gen.Body(region, inst.IP, clientNet, time.Now())
			cfg, err := buildManagerConfig(st, secrets, inst, place, cert, time.Now())
			if err != nil {
				return nil, err
			}
			return managerFiles(cfg, body, managerBinary)
		}
	}

	previous := st.Server
	res, err := deploy.Provision(ctx, provider, deploy.Request{
		Location: region,
		Billing:  place.Billing,
		Label:    fmt.Sprintf("awg-%s-%d", region, time.Now().Unix()),
		SSHKeyID: keyID,
		Binary:   binary,
		Config:   nodeCfg,
		Files:    extraFiles,
		FilesFor: filesFor,
		Services: services,
		Signer:   signer,
	}, logf)
	if err != nil {
		return err
	}

	// Publish before touching the old server: if publishing fails, clients
	// still have somewhere to connect.
	logf("Публикую конфиг…")
	if body == nil {
		body = gen.Body(res.Region, res.IP, clientNet, time.Now())
	}
	if err := publishConfig(ctx, st, secrets, body, cert); err != nil {
		return fmt.Errorf("сервер работает (%s), но конфиг не опубликован: %w", res.IP, err)
	}

	st.Server = store.Server{
		Provider:  res.Provider,
		ID:        res.InstanceID,
		Region:    res.Region,
		IP:        res.IP,
		CreatedAt: time.Now().Unix(),
	}
	st.ServerHostKey = res.HostKey
	st.Generation++
	if err := st.Save(root); err != nil {
		return fmt.Errorf("опубликовано, но состояние не сохранено: %w", err)
	}

	logf("")
	logf("Сервер работает.")
	logf("  Место     %s:%s (%s)", res.Provider, res.Region, place.Billing)
	logf("  Адрес     %s", res.IP)
	logf("  Порт      %d", port)
	logf("  Публикация №%d", st.Seq)
	logf("")
	logf("Конфиг для клиента:")
	logf("  awgctl client-conf -o awg.conf")

	prevCloud, prevErr := clouds.Get(previous.Provider)
	if previous.ID != "" && previous.ID != res.InstanceID && prevErr != nil {
		logf("")
		logf("Прежний сервер %s у %s: этого хостера здесь нет, удали его сам.", shortID(previous.ID), previous.Provider)
	} else if previous.ID != "" && previous.ID != res.InstanceID {
		// It may already be gone: deleted by hand, or by an earlier run.
		// Waiting out the grace period for a server that does not exist
		// just blocks the command for a quarter of an hour.
		if _, err := prevCloud.Get(context.Background(), previous.ID); err != nil {
			logf("")
			logf("Прежний сервер %s уже удалён, ждать нечего.", shortID(previous.ID))
		} else if *keepOld {
			logf("")
			logf("Прежний сервер %s (%s) оставлен, удали сам: awgctl down -id %s",
				shortID(previous.ID), previous.IP, previous.ID)
		} else {
			logf("")
			logf("Прежний сервер %s удалю через %s, чтобы подключённые успели перейти.",
				shortID(previous.ID), oldServerGrace)
			logf("Не закрывай окно, либо удали потом сам: awgctl down -id %s", previous.ID)
			select {
			case <-time.After(oldServerGrace):
			case <-ctx.Done():
			}
			if err := prevCloud.Delete(context.Background(), previous.ID); err != nil {
				logf("Удалить прежний сервер не удалось: %v", err)
				logf("Удали сам: awgctl down -id %s", previous.ID)
			} else {
				logf("Прежний сервер удалён.")
			}
		}
	}
	return nil
}

// buildNode cross-compiles the node for the servers. The operator's machine
// has a Go toolchain; a server building its replacement later copies the
// binary it is already running instead.
func buildNode(root string) ([]byte, error) { return buildLinux(root, "awgnode") }

func publishConfig(ctx context.Context, st *store.State, secrets config.Secrets, body *fleet.ConfigBody, cert proto.Cert) error {
	if err := body.Validate(); err != nil {
		return err
	}
	raw, err := body.Marshal()
	if err != nil {
		return err
	}

	now := time.Now()
	serverKey, err := st.ServerKey()
	if err != nil {
		return err
	}
	contentKey, err := st.ContentKey()
	if err != nil {
		return err
	}
	trust, err := st.Trust()
	if err != nil {
		return err
	}

	where, err := openPlaces(st, secrets)
	if err != nil {
		return err
	}

	// Servers that replace themselves keep publishing, so the number that is
	// published, in whichever place it is newest, can be ahead of this
	// machine's. Numbering from the larger of the two is what keeps a new
	// configuration from being rejected as old.
	if files, err := where.Read(ctx, proto.FileConfig); err == nil {
		if blob, ok := files[proto.FileConfig]; ok {
			if _, state, err := trust.OpenDoc(blob, proto.KindConfig, now, proto.State{}); err == nil && state.LastSeq > st.Seq {
				st.Seq = state.LastSeq
			}
		}
	}

	doc := &proto.Doc{
		Kind:      proto.KindConfig,
		Seq:       st.NextSeq(),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(docValidity).Unix(),
		Cert:      cert,
		Body:      string(raw),
	}
	blob, err := proto.SealDoc(doc, serverKey, contentKey)
	if err != nil {
		return err
	}

	// Read it back the way a client will, so a configuration that cannot be
	// verified never sits in the gist unnoticed.
	if _, _, err := trust.OpenDoc(blob, proto.KindConfig, now, proto.State{}); err != nil {
		return fmt.Errorf("свой же конфиг не проходит проверку: %w", err)
	}

	return where.Write(ctx, map[string]string{proto.FileConfig: blob})
}
