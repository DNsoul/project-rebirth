// Package splitplan gathers what split routing adds to a server: the set of
// local address ranges, the rules, and the routes a client needs.
//
// It is shared because two different programs do the same thing. The
// operator's tool prepares it when it builds the first server, and a server
// prepares it again when it builds its replacement, which is also what keeps
// the address set from going stale: every new server starts with a fresh one.
package splitplan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"awgrotator/internal/deploy"
	"awgrotator/internal/node"
	"awgrotator/internal/ruset"
	"awgrotator/internal/splitdns"
)

// Defaults.
const (
	// MaxAge is how fresh a saved address set must be to be reused.
	MaxAge = 7 * 24 * time.Hour

	// MinRanges is the sanity floor for a downloaded set. The real set has
	// thousands of ranges; a file with a handful is a truncated or wrong
	// download, and sending traffic by it would be wrong in a way nobody
	// would notice.
	MinRanges = 1000
)

// Cache file names.
const (
	ruSetFile    = "ru4.txt"
	telegramFile = "telegram4.txt"
)

// Rule file names, the same on the operator's machine and on a server.
const (
	ForceTunnelFile = "force_vpn.txt"
	ForceDirectFile = "force_direct.txt"
)

// Sources says where things are kept.
type Sources struct {
	// CacheDir holds the downloaded address set and Telegram list between
	// runs.
	CacheDir string

	// RulesDir holds the two rule files.
	RulesDir string

	// CreateRules writes the default rule files when they are missing, so
	// there is something to edit. A server leaves it false: its rules arrive
	// with it, and a missing file there means empty rules.
	CreateRules bool
}

// Plan is everything split routing adds to a deployment.
type Plan struct {
	// Files are uploaded to the server.
	Files []deploy.File

	// DNS and AllowedIPs are what the client configuration is built with.
	DNS        []string
	AllowedIPs []string
}

// Logf receives progress messages.
type Logf func(format string, args ...any)

// TelegramFallback is used when neither the download nor a cached copy is
// available. It is the list as of this writing; the apps connect to these
// addresses directly, so a stale list degrades gradually rather than at once.
var TelegramFallback = []string{
	"91.105.192.0/23", "91.108.4.0/22", "91.108.8.0/22", "91.108.12.0/22",
	"91.108.16.0/22", "91.108.20.0/22", "91.108.56.0/22", "149.154.160.0/20",
	"185.76.151.0/24",
}

// DefaultForceTunnel is written when the file does not exist.
const DefaultForceTunnel = `# Сайты, которые всегда идут через туннель, даже если у них российский адрес.
# Один сайт на строку; поддомены входят сами; "#" начинает комментарий.
#
# Счётчики и пиксели: через них российские компании узнают, какие страницы вы
# открываете и с какого адреса. Они встроены в множество зарубежных сайтов, и
# без этого правила запрос к ним ушёл бы с вашего домашнего адреса.
mc.yandex.ru
mc.yandex.com
an.yandex.ru
top.mail.ru
top-fwz1.mail.ru
counter.yadro.ru
`

// DefaultForceDirect is written when the file does not exist.
const DefaultForceDirect = `# Сайты, которые всегда идут напрямую, даже если у них зарубежный адрес.
# Нужно, например, банку, который не пускает с адресов VPN.
#
# sberbank.ru
`

// Prepare gathers the address set, the Telegram ranges and the rules,
// refreshing what is stale and falling back to what is cached.
func Prepare(ctx context.Context, src Sources, logf Logf) (*Plan, error) {
	logf("Готовлю разделение трафика…")

	ruData, err := ensureRUSet(ctx, src, logf)
	if err != nil {
		return nil, err
	}
	telegram := ensureTelegram(ctx, src, logf)
	forceTunnel, forceDirect, err := ensureRules(src, logf)
	if err != nil {
		return nil, err
	}

	allowed := []string{node.DefaultStandInRange}
	for _, p := range telegram {
		allowed = append(allowed, p.String())
	}

	return &Plan{
		Files: []deploy.File{
			{What: "список российских сетей", Path: node.DefaultRUSetPath, Mode: "0644", Content: ruData},
			{What: "правила: через туннель", Path: node.DefaultForceTunnelPath, Mode: "0644", Content: forceTunnel},
			{What: "правила: напрямую", Path: node.DefaultForceDirectPath, Mode: "0644", Content: forceDirect},
		},
		DNS:        []string{node.DefaultDNSAddress},
		AllowedIPs: allowed,
	}, nil
}

// ensureRUSet returns the address set, downloading it when the saved copy is
// missing, stale or implausible.
func ensureRUSet(ctx context.Context, src Sources, logf Logf) ([]byte, error) {
	path := filepath.Join(src.CacheDir, ruSetFile)

	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < MaxAge {
		if data, err := os.ReadFile(path); err == nil {
			if set, err := ruset.Read(bytes.NewReader(data)); err == nil && set.Ranges() >= MinRanges {
				logf("  российские сети: %d диапазонов (сохранённый список)", set.Ranges())
				return data, nil
			}
		}
	}

	logf("  скачиваю российские сети из RIPE…")
	set, err := ruset.Fetch(ctx, ruset.RIPEURL, "RU")
	if err == nil && set.Ranges() < MinRanges {
		err = fmt.Errorf("в скачанном списке только %d диапазонов, это не похоже на настоящий", set.Ranges())
	}
	if err != nil {
		// A stale set is better than none: the address space changes slowly.
		if data, readErr := os.ReadFile(path); readErr == nil {
			if old, parseErr := ruset.Read(bytes.NewReader(data)); parseErr == nil && old.Ranges() >= MinRanges {
				logf("  не удалось обновить (%v), беру старый список", err)
				return data, nil
			}
		}
		return nil, fmt.Errorf("нет списка российских сетей: %w", err)
	}

	var buf bytes.Buffer
	if _, err := set.WriteTo(&buf); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return nil, err
	}
	logf("  российские сети: %d диапазонов", set.Ranges())
	return buf.Bytes(), nil
}

// ensureTelegram returns Telegram's IPv4 ranges: freshly downloaded when
// possible, otherwise the cached copy, otherwise the built-in list. It never
// fails, because losing Telegram routing is better than refusing to deploy.
func ensureTelegram(ctx context.Context, src Sources, logf Logf) []netip.Prefix {
	cache := filepath.Join(src.CacheDir, telegramFile)

	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	prefixes, err := ruset.FetchPrefixes(fetchCtx, ruset.TelegramURL)
	if err == nil {
		var b strings.Builder
		for _, p := range prefixes {
			b.WriteString(p.String() + "\n")
		}
		_ = os.MkdirAll(filepath.Dir(cache), 0o700)
		_ = os.WriteFile(cache, []byte(b.String()), 0o600)
		logf("  Telegram: %d сетей", len(prefixes))
		return prefixes
	}
	logf("  список Telegram не скачался (%v)", err)

	if data, readErr := os.ReadFile(cache); readErr == nil {
		if cached, parseErr := ruset.ParsePrefixes(bytes.NewReader(data)); parseErr == nil {
			logf("  Telegram: %d сетей (сохранённый список)", len(cached))
			return cached
		}
	}

	builtin, _ := ruset.ParsePrefixes(strings.NewReader(strings.Join(TelegramFallback, "\n")))
	logf("  Telegram: %d сетей (встроенный список)", len(builtin))
	return builtin
}

// ensureRules reads the two rule files, creating them with defaults on first
// use when allowed. A rules file that does not parse stops the deployment: a
// typo that silently matched nothing would quietly send a site the wrong way.
func ensureRules(src Sources, logf Logf) (forceTunnel, forceDirect []byte, err error) {
	if src.CreateRules {
		if err := os.MkdirAll(src.RulesDir, 0o755); err != nil {
			return nil, nil, err
		}
	}

	load := func(name, defaults string) ([]byte, error) {
		path := filepath.Join(src.RulesDir, name)
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist) && src.CreateRules:
			if err := os.WriteFile(path, []byte(defaults), 0o644); err != nil {
				return nil, err
			}
			logf("  создал %s, его можно править", path)
			data = []byte(defaults)
		case errors.Is(err, os.ErrNotExist):
			// A server with no rules file has no rules.
			logf("  %s: файла нет, правил нет", name)
			return nil, nil
		case err != nil:
			return nil, err
		}
		rules, err := splitdns.ParseRules(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		logf("  %s: %d имён", name, rules.Len())
		return data, nil
	}

	if forceTunnel, err = load(ForceTunnelFile, DefaultForceTunnel); err != nil {
		return nil, nil, err
	}
	if forceDirect, err = load(ForceDirectFile, DefaultForceDirect); err != nil {
		return nil, nil, err
	}
	return forceTunnel, forceDirect, nil
}
