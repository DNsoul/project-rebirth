package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/gist"
	"awgrotator/internal/proto"
	"awgrotator/internal/store"
)

// releaseFile is one installer that goes into an announcement.
type releaseFile struct {
	OS   string // proto.OSWindows or proto.OSAndroid
	Path string // the file on this machine; it is hashed, not uploaded
	URL  string // where people download it; if empty, BaseURL plus the file name
}

// releaseRequest is what the operator asks to announce.
type releaseRequest struct {
	Version  string
	Build    uint32
	MinBuild uint32
	Notes    string
	BaseURL  string
	Files    []releaseFile
	Now      time.Time
}

// release makes the announcement, unsigned. The installers are only read: the
// announcement says where they are and what they must hash to.
func (r releaseRequest) release() (*proto.Release, error) {
	if len(r.Files) == 0 {
		return nil, errors.New("не указано ни одного установщика: -windows и/или -android")
	}
	rel := &proto.Release{
		Version:  r.Version,
		Build:    r.Build,
		MinBuild: r.MinBuild,
		IssuedAt: r.Now.Unix(),
		Notes:    r.Notes,
	}
	for _, f := range r.Files {
		sum, size, err := hashFile(f.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.OS, err)
		}
		addr := f.URL
		if addr == "" {
			if r.BaseURL == "" {
				return nil, fmt.Errorf("%s: не сказано, откуда файл будут скачивать: нужен -base-url или -%s-url", f.OS, f.OS)
			}
			addr = strings.TrimRight(r.BaseURL, "/") + "/" + url.PathEscape(filepath.Base(f.Path))
		}
		rel.Assets = append(rel.Assets, proto.ReleaseAsset{OS: f.OS, URL: addr, SHA256: sum, Size: size})
	}
	return rel, nil
}

func hashFile(path string) (sum string, size uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), uint64(n), nil
}

var pubspecVersion = regexp.MustCompile(`^version:\s*(\d+\.\d+\.\d+)\+(\d+)\s*$`)

// readPubspecVersion returns the version and build number the app is built as,
// which is what an announcement of that build has to say.
func readPubspecVersion(path string) (version string, build uint32, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		m := pubspecVersion.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		n, err := strconv.ParseUint(m[2], 10, 32)
		if err != nil || n == 0 {
			return "", 0, fmt.Errorf("%s: номер сборки %q не годится", path, m[2])
		}
		return m[1], uint32(n), nil
	}
	return "", 0, fmt.Errorf("%s: нет строки вида «version: 1.2.3+4»", path)
}

// releaseGist is the part of the gist client the release commands use.
type releaseGist interface {
	Get(ctx context.Context, id string) (map[string]string, error)
	Update(ctx context.Context, id string, files map[string]string) error
}

// currentRelease is the announcement that is published now, or nil if there is
// none that verifies: the placeholder written at the start is not one.
func currentRelease(files map[string]string, trust proto.Trust) *proto.Release {
	blob := files[proto.FileRelease]
	if blob == "" {
		return nil
	}
	rel, _, err := trust.OpenRelease(blob, proto.State{})
	if err != nil {
		return nil
	}
	return rel
}

// publishRelease signs the announcement with the root key and puts it in the
// gist. It returns the one it replaced, if there was one.
//
// A build number lower than the published one is refused. Every app that has
// seen the higher one remembers it and would throw this one away, so it would
// reach only the apps that have not looked yet, which is worse than not
// publishing.
func publishRelease(ctx context.Context, g releaseGist, st *store.State, rel *proto.Release, force bool) (replaced *proto.Release, err error) {
	trust, err := st.Trust()
	if err != nil {
		return nil, err
	}
	files, err := g.Get(ctx, st.GistID)
	if err != nil {
		return nil, err
	}
	replaced = currentRelease(files, trust)
	if replaced != nil && replaced.Build > rel.Build && !force {
		return replaced, fmt.Errorf("уже опубликована сборка %d (%s), она новее, чем %d.\n"+
			"Приложения, которые её видели, запомнили номер и отбросят эту. Увеличь номер сборки в app/pubspec.yaml",
			replaced.Build, replaced.Version, rel.Build)
	}

	rootKey, err := st.RootKey()
	if err != nil {
		return replaced, err
	}
	contentKey, err := st.ContentKey()
	if err != nil {
		return replaced, err
	}
	blob, err := proto.SealRelease(rel, proto.IssuerRoot, rootKey, contentKey)
	if err != nil {
		return replaced, err
	}
	if err := g.Update(ctx, st.GistID, map[string]string{proto.FileRelease: blob}); err != nil {
		return replaced, err
	}
	return replaced, nil
}

func describeRelease(rel *proto.Release) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Версия %s, сборка %d", rel.Version, rel.Build)
	if rel.MinBuild > 0 {
		fmt.Fprintf(&b, ", обязательна для сборок ниже %d", rel.MinBuild)
	}
	b.WriteString("\n")
	for _, a := range rel.Assets {
		fmt.Fprintf(&b, "  %-8s %6.1f МБ  sha256 %s…\n           %s\n",
			a.OS, float64(a.Size)/(1<<20), a.SHA256[:16], a.URL)
	}
	if rel.Notes != "" {
		fmt.Fprintf(&b, "Что нового:\n  %s\n", strings.ReplaceAll(rel.Notes, "\n", "\n  "))
	}
	return b.String()
}

// ---- release

func cmdRelease(args []string) error {
	if len(args) == 0 {
		return errors.New("нужно указать: publish | show")
	}
	switch args[0] {
	case "publish":
		return cmdReleasePublish(args[1:])
	case "show":
		return cmdReleaseShow(args[1:])
	}
	return fmt.Errorf("неизвестная подкоманда %q: publish | show", args[0])
}

func cmdReleasePublish(args []string) error {
	fs := flag.NewFlagSet("release publish", flag.ExitOnError)
	version := fs.String("version", "", "версия для людей, 1.2.3 (по умолчанию из app/pubspec.yaml)")
	build := fs.Uint("build", 0, "номер сборки (по умолчанию из app/pubspec.yaml)")
	minBuild := fs.Uint("min-build", 0, "самая старая сборка, которая ещё работает; ниже — обновление обязательно")
	notes := fs.String("notes", "", "что нового, текстом")
	notesFile := fs.String("notes-file", "", "что нового, из файла")
	baseURL := fs.String("base-url", "", "папка, откуда качают, например https://github.com/<кто>/<репо>/releases/download/v1.2.0/")
	windows := fs.String("windows", "", "установщик для Windows (файл на этом компьютере)")
	android := fs.String("android", "", "установщик для Android (файл на этом компьютере)")
	windowsURL := fs.String("windows-url", "", "адрес установщика Windows, если не base-url плюс имя файла")
	androidURL := fs.String("android-url", "", "адрес установщика Android, если не base-url плюс имя файла")
	dryRun := fs.Bool("dry-run", false, "показать, что будет объявлено, и ничего не публиковать")
	force := fs.Bool("force", false, "не сверять с pubspec.yaml и с уже опубликованной сборкой")
	noMirror := fs.Bool("no-mirror", false, "не загружать установщики на запасные места (awgctl mirror)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := config.Root()
	if err != nil {
		return err
	}

	// The number in the announcement has to be the number built into the app: an
	// installer that says it is build 11 while the announcement says 12 would be
	// offered again and again.
	pv, pb, perr := readPubspecVersion(filepath.Join(root, "app", "pubspec.yaml"))
	v, b := *version, uint32(*build)
	switch {
	case perr == nil && v == "" && b == 0:
		v, b = pv, pb
	case perr == nil && (v != pv || b != pb) && !*force:
		return fmt.Errorf("в app/pubspec.yaml сборка %s+%d, а объявляется %s+%d.\n"+
			"Установщики собраны из pubspec.yaml, их номер внутри именно тот. Поправь pubspec и пересобери, либо -force", pv, pb, v, b)
	case perr != nil && (v == "" || b == 0):
		return fmt.Errorf("версию и номер сборки не взять из pubspec.yaml (%v): укажи -version и -build", perr)
	}

	text := *notes
	if *notesFile != "" {
		raw, err := os.ReadFile(*notesFile)
		if err != nil {
			return err
		}
		// PowerShell's editors like to start a file with a byte order mark.
		text = strings.TrimSpace(string(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})))
	}

	req := releaseRequest{
		Version: v, Build: b, MinBuild: uint32(*minBuild),
		Notes: text, BaseURL: *baseURL, Now: time.Now(),
	}
	if *windows != "" {
		req.Files = append(req.Files, releaseFile{OS: proto.OSWindows, Path: *windows, URL: *windowsURL})
	}
	if *android != "" {
		req.Files = append(req.Files, releaseFile{OS: proto.OSAndroid, Path: *android, URL: *androidURL})
	}
	rel, err := req.release()
	if err != nil {
		return err
	}
	fmt.Print(describeRelease(rel))

	st, stErr := store.Load(root)
	if *dryRun {
		if stErr == nil && len(st.Mirrors) > 0 && !*noMirror {
			fmt.Print("\nКопии установщиков будут загружены и на запасные места:\n" + describeMirrors(st))
		}
		fmt.Println("\nЭто пробный прогон: ничего не опубликовано.")
		return nil
	}

	if stErr != nil {
		return stErr
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}
	// Long enough for tens of megabytes to go to a mirror and come back.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// The copies first: the announcement is what sends the apps to look for
	// them, so they must be there, and checked, before it is made.
	if !*noMirror && len(st.Mirrors) > 0 {
		fmt.Println("\nКопии на запасных местах:")
		if err := uploadToMirrors(ctx, os.Stdout, st, secrets.GitLabToken, rel, req.Files); err != nil {
			return fmt.Errorf("%w\n(объявить можно и без копий: -no-mirror)", err)
		}
	}

	replaced, err := publishRelease(ctx, gist.New(secrets.GitHubToken), st, rel, *force)
	if err != nil {
		return err
	}
	if replaced != nil {
		fmt.Printf("\nЗаменяет сборку %d (%s).\n", replaced.Build, replaced.Version)
	}
	fmt.Println("\nОбъявлено. Приложения увидят это при следующей проверке: раз в несколько часов, а подключённые — раз в три минуты.")
	fmt.Println("Проверить: awgctl release show")
	return nil
}

func cmdReleaseShow(args []string) error {
	fs := flag.NewFlagSet("release show", flag.ExitOnError)
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
	trust, err := st.Trust()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	files, err := gist.New(secrets.GitHubToken).Get(ctx, st.GistID)
	if err != nil {
		return err
	}
	rel := currentRelease(files, trust)
	if rel == nil {
		fmt.Println("Анонса версии нет, или он не проходит проверку.")
		return nil
	}
	fmt.Print(describeRelease(rel))
	fmt.Printf("\nОбъявлено %s назад.\n", rel.ReleaseAge(time.Now()).Round(time.Minute))
	return nil
}
