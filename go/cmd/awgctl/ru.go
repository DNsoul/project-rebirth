package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/ruset"
	"awgrotator/internal/store"
)

// ruSetPath is where the downloaded set is kept between runs.
func ruSetPath(root string) string { return filepath.Join(store.Dir(root), "ru4.txt") }

func cmdRuUpdate(args []string) error {
	fs := flag.NewFlagSet("ru-update", flag.ExitOnError)
	country := fs.String("country", "RU", "страна, чьи сети считать «своими»")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := config.Root()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	fmt.Println("Скачиваю таблицу делегирования RIPE (около 20 МБ)…")
	start := time.Now()
	set, err := ruset.Fetch(ctx, ruset.RIPEURL, *country)
	if err != nil {
		return err
	}
	fmt.Printf("  разобрано за %s\n", time.Since(start).Round(100*time.Millisecond))

	if err := os.MkdirAll(store.Dir(root), 0o700); err != nil {
		return err
	}
	path := ruSetPath(root)

	// Replace atomically: a half written set would silently misroute traffic.
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := set.WriteTo(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}

	fmt.Printf("\n%s: %d диапазонов, %.1f млн адресов\n", *country, set.Ranges(), float64(set.Addresses())/1e6)
	fmt.Printf("Записано в %s\n", path)
	return nil
}

// defaultCheckList is a spread of sites: clearly Russian, clearly foreign, and
// ones that could go either way, so the report shows where the set is useful
// and where it is not.
var defaultCheckList = []string{
	"yandex.ru", "vk.com", "ozon.ru", "wildberries.ru", "avito.ru", "mail.ru",
	"sberbank.ru", "gosuslugi.ru", "kinopoisk.ru", "rutube.ru", "2gis.ru", "dzen.ru",
	"tinkoff.ru", "hh.ru", "rbc.ru", "lenta.ru", "ria.ru", "habr.com",
	"google.com", "youtube.com", "github.com", "openai.com", "wikipedia.org",
	"instagram.com", "telegram.org", "cloudflare.com", "reddit.com", "twitch.tv",
}

func cmdRuCheck(args []string) error {
	fs := flag.NewFlagSet("ru-check", flag.ExitOnError)
	resolver := fs.String("resolver", "", "DNS-сервер для запросов (по умолчанию системный)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	domains := fs.Args()
	if len(domains) == 0 {
		domains = defaultCheckList
	}

	root, err := config.Root()
	if err != nil {
		return err
	}
	set, err := loadRuSet(root)
	if err != nil {
		return err
	}

	r := net.DefaultResolver
	if *resolver != "" {
		server := net.JoinHostPort(*resolver, "53")
		r = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "udp", server)
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var direct, tunnel, mixed, failed int
	for _, domain := range domains {
		ips, err := r.LookupNetIP(ctx, "ip4", domain)
		if err != nil || len(ips) == 0 {
			fmt.Printf("  %-18s не резолвится\n", domain)
			failed++
			continue
		}
		sort.Slice(ips, func(i, j int) bool { return ips[i].Less(ips[j]) })

		var ru, foreign []string
		for _, ip := range ips {
			if set.Contains(ip) {
				ru = append(ru, ip.String())
			} else {
				foreign = append(foreign, ip.String())
			}
		}

		// The rule the splitter will apply: direct only when every address
		// is Russian. One foreign address is enough to send it through.
		verdict := "ТУННЕЛЬ"
		switch {
		case len(foreign) == 0:
			verdict = "НАПРЯМУЮ"
			direct++
		case len(ru) == 0:
			tunnel++
		default:
			verdict = "ТУННЕЛЬ (смешанный)"
			mixed++
		}
		fmt.Printf("  %-18s %-20s ru=%d зарубеж=%d  %s\n",
			domain, verdict, len(ru), len(foreign), summarize(ips, 3))
	}

	fmt.Printf("\nнапрямую %d, туннель %d, из них смешанных %d, не резолвится %d\n",
		direct, tunnel+mixed, mixed, failed)
	return nil
}

func loadRuSet(root string) (*ruset.Set, error) {
	f, err := os.Open(ruSetPath(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("набора российских сетей ещё нет, запусти \"awgctl ru-update\"")
		}
		return nil, err
	}
	defer f.Close()
	return ruset.Read(f)
}

func summarize(ips []netip.Addr, max int) string {
	parts := make([]string, 0, max+1)
	for i, ip := range ips {
		if i == max {
			parts = append(parts, fmt.Sprintf("+%d", len(ips)-max))
			break
		}
		parts = append(parts, ip.String())
	}
	return strings.Join(parts, " ")
}
