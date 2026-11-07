package ruset

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// RIPEURL is the RIPE NCC "delegated extended" statistics file. Russia's
// address space is delegated by RIPE, so one file covers it. The file is a
// primary source: it records who the registry handed each block to, which is
// what "an address in Russia" has to mean here.
const RIPEURL = "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-extended-latest"

// maxDownload bounds what is read. The file is about 20 MB; anything several
// times that is not the file we asked for.
const maxDownload = 128 << 20

// Fetch downloads a statistics file and keeps the country's IPv4 ranges.
func Fetch(ctx context.Context, url, country string) (*Set, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return ParseRIR(io.LimitReader(resp.Body, maxDownload), country)
}

// TelegramURL is where Telegram publishes the address ranges its servers use.
//
// The Telegram apps connect to addresses built into them and never ask DNS,
// so the split resolver cannot steer them. Their ranges go straight into the
// tunnel's routes instead.
const TelegramURL = "https://core.telegram.org/resources/cidr.txt"

// FetchPrefixes downloads a list of CIDR blocks, one per line, and returns the
// IPv4 ones in ascending order. IPv6 lines are skipped: the tunnel carries
// IPv4 only.
func FetchPrefixes(ctx context.Context, url string) ([]netip.Prefix, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	prefixes, err := ParsePrefixes(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return prefixes, nil
}

// ParsePrefixes reads CIDR blocks, one per line, keeping the IPv4 ones.
// Blank lines and '#' comments are ignored. A line that is not a block is an
// error: a list that quietly loses entries would leave part of the traffic
// outside the tunnel.
func ParsePrefixes(r io.Reader) ([]netip.Prefix, error) {
	var out []netip.Prefix
	seen := map[netip.Prefix]bool{}

	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = strings.TrimSpace(text[:i])
		}
		if text == "" {
			continue
		}
		p, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %q is not a network: %w", line, text, err)
		}
		p = p.Masked()
		if !p.Addr().Is4() || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no IPv4 networks in the list")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr().Less(out[j].Addr()) })
	return out, nil
}
