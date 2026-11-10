package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/config"
	"awgrotator/internal/enroll"
	"awgrotator/internal/fleet"
	"awgrotator/internal/proto"
	"awgrotator/internal/store"
	"awgrotator/internal/tunneltest"
)

// opDevice is a device this tool registers on the operator's behalf: a stock
// client's configuration, or the identity "verify" tests with.
//
// It is exactly what an app does for itself. The key is made once and kept in
// state/devices; the identifier is derived from the name, so the same name is
// the same device wherever it is registered, and registering it again changes
// nothing.
type opDevice struct {
	Name string
	ID   []byte
	Keys awg.Keypair

	// Address is what the server gave the device the last time it registered,
	// and zero if it never has. A server that inherited the device from its
	// predecessor must still know it by this address: "verify -reuse" relies
	// on that to prove devices survive a move without registering again.
	Address netip.Prefix

	root string
}

var deviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type deviceFile struct {
	Private string `json:"private"`           // base64, as in a .conf file
	Address string `json:"address,omitempty"` // with prefix, from the last registration
}

func (d *opDevice) save() error {
	raw, err := json.MarshalIndent(deviceFile{Private: d.Keys.Private.Base64(), Address: addrString(d.Address)}, "", "  ")
	if err != nil {
		return err
	}
	path := devicePath(d.root, d.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func addrString(p netip.Prefix) string {
	if !p.IsValid() {
		return ""
	}
	return p.String()
}

func devicePath(root, name string) string {
	return filepath.Join(store.Dir(root), "devices", name+".json")
}

// loadDevice reads the device called name, making it if it does not exist yet.
func loadDevice(root, name string) (*opDevice, error) {
	if !deviceName.MatchString(name) {
		return nil, fmt.Errorf("имя устройства %q не годится: нужны латинские буквы, цифры, - и _, до 32 знаков", name)
	}
	sum := sha256.Sum256([]byte("awgctl-device/" + name))
	dev := &opDevice{Name: name, ID: sum[:proto.DeviceIDSize], root: root}

	path := devicePath(root, name)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f deviceFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		private, err := awg.ParseKeyBase64(f.Private)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		public, err := awg.PublicFrom(private)
		if err != nil {
			return nil, err
		}
		dev.Keys = awg.Keypair{Private: private, Public: public}
		if f.Address != "" {
			if dev.Address, err = netip.ParsePrefix(f.Address); err != nil {
				return nil, fmt.Errorf("%s: address: %w", path, err)
			}
		}
		return dev, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	if dev.Keys, err = awg.GenerateKeypair(); err != nil {
		return nil, err
	}
	if err := dev.save(); err != nil {
		return nil, err
	}
	return dev, nil
}

// fetchBody reads the configuration that is published, and checks it the way a
// client does.
func fetchBody(ctx context.Context, st *store.State, secrets config.Secrets) (*fleet.ConfigBody, error) {
	if st.GistID == "" {
		return nil, errors.New("gist не создан, запусти \"awgctl init\"")
	}
	where, err := openPlaces(st, secrets)
	if err != nil {
		return nil, err
	}
	files, err := where.Read(ctx, proto.FileConfig)
	if err != nil {
		return nil, err
	}
	blob, ok := files[proto.FileConfig]
	if !ok {
		return nil, errors.New("нигде нет файла конфига")
	}
	trust, err := st.Trust()
	if err != nil {
		return nil, err
	}
	doc, _, err := trust.OpenDoc(blob, proto.KindConfig, time.Now(), proto.State{})
	if err != nil {
		return nil, fmt.Errorf("опубликованный конфиг не проходит проверку: %w", err)
	}
	return fleet.ParseBody([]byte(doc.Body))
}

// registrationFor describes how a device registers on the server a body points
// to.
func registrationFor(st *store.State, body *fleet.ConfigBody) (*tunneltest.Registration, error) {
	if body.Enroll == nil {
		return nil, errors.New("в опубликованном конфиге нет регистрации устройств: " +
			"сервер собран по старому образцу, подними новый (awgctl up)")
	}
	contentKey, err := st.ContentKey()
	if err != nil {
		return nil, err
	}
	endpoint, err := netip.ParseAddrPort(body.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("адрес сервера %q: %w", body.Endpoint, err)
	}
	peer, err := netip.ParsePrefix(body.Enroll.Address)
	if err != nil {
		return nil, fmt.Errorf("служебный адрес %q: %w", body.Enroll.Address, err)
	}
	server, err := netip.ParseAddrPort(body.Enroll.Server)
	if err != nil {
		return nil, fmt.Errorf("адрес регистрации %q: %w", body.Enroll.Server, err)
	}
	return &tunneltest.Registration{
		Endpoint:     endpoint,
		ServerPublic: body.ServerPublicKey,
		Params:       body.Params,
		PeerPrivate:  body.Enroll.PrivateKey,
		PeerAddr:     peer.Addr(),
		Server:       server,
		ContentKey:   contentKey,
	}, nil
}

// register registers the device and returns the address the server gave it.
func (d *opDevice) register(ctx context.Context, reg *tunneltest.Registration) (netip.Prefix, error) {
	addr, err := tunneltest.Register(ctx, *reg, tunneltest.Device{ID: d.ID, Keypair: d.Keys, Name: d.Name})
	var refused *enroll.Refused
	switch {
	case errors.As(err, &refused) && refused.Reason == proto.EnrollPending:
		code := proto.ApprovalCode(d.Keys.Public[:])
		return netip.Prefix{}, fmt.Errorf("устройство «%s» ждёт одобрения: одобри его в боте (код %s) и повтори", d.Name, code)
	case errors.As(err, &refused) && refused.Reason == proto.EnrollDenied:
		return netip.Prefix{}, fmt.Errorf("устройству «%s» отказано: выдай доступ в боте (Заявки → Выдать доступ) и повтори", d.Name)
	case err != nil:
		return netip.Prefix{}, err
	}
	prefix, err := netip.ParsePrefix(addr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("сервер ответил %q, это не адрес", addr)
	}
	d.Address = prefix
	if err := d.save(); err != nil {
		return netip.Prefix{}, fmt.Errorf("запомнить адрес устройства: %w", err)
	}
	return prefix, nil
}
