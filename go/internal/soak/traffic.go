package soak

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// Foreign and Russian sites, a mix of what a person opens in a day.
var (
	foreignSites = []string{
		"https://en.wikipedia.org/wiki/Special:Random",
		"https://www.bbc.com/news",
		"https://github.com/trending",
		"https://stackoverflow.com/questions",
		"https://www.reddit.com/",
		"https://news.ycombinator.com/",
		"https://www.cloudflare.com/",
		"https://www.youtube.com/",
		"https://www.twitch.tv/",
		"https://www.theguardian.com/international",
		"https://www.nytimes.com/",
		"https://www.ikea.com/",
	}
	russianSites = []string{
		"https://yandex.ru/",
		"https://vk.com/",
		"https://mail.ru/",
		"https://ria.ru/",
		"https://lenta.ru/",
		"https://www.sberbank.ru/",
		"https://www.ozon.ru/",
		"https://www.wildberries.ru/",
		"https://www.avito.ru/",
		"https://www.kinopoisk.ru/",
		"https://habr.com/ru/",
		"https://www.gosuslugi.ru/",
	}

	// bigFiles are public test files a long download can come from, tried in
	// turn. A video is a long download at a steady speed, which is what these
	// stand in for.
	bigFiles = []string{
		"https://proof.ovh.net/files/100Mb.dat",
		"https://speed.hetzner.de/100MB.bin",
		"https://cachefly.cachefly.net/100mb.test",
		"http://ipv4.download.thinkbroadband.com/100MB.zip",
	}
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

var assetRef = regexp.MustCompile(`(?i)(?:src|href)=["']([^"'#]+\.(?:js|css|png|jpg|jpeg|webp|svg|woff2?))["']`)

// assetsOf picks up to max same-host assets a page refers to, as a browser
// would go on to fetch them.
func assetsOf(page string, base *url.URL, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range assetRef.FindAllStringSubmatch(page, -1) {
		ref, err := url.Parse(m[1])
		if err != nil {
			continue
		}
		abs := base.ResolveReference(ref)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			continue
		}
		s := abs.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == max {
			break
		}
	}
	return out
}

// behaviour is what a person does next.
type behaviour int

const (
	browse behaviour = iota
	stream
	idle
)

// choose draws the next behaviour. About a quarter of the time nothing is
// happening and only keepalives cross, which matters as much as the busy times.
func choose(r *rand.Rand, overBudget bool) behaviour {
	n := r.Intn(100)
	switch {
	case n < 55:
		return browse
	case n < 75 && !overBudget:
		return stream
	default:
		return idle
	}
}

// trafficLoop carries a person's traffic through the tunnel until ctx ends.
func (a *armRunner) trafficLoop(ctx context.Context, tun Tunnel) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for ctx.Err() == nil {
		switch choose(r, a.overBudget()) {
		case browse:
			a.browse(ctx, tun, r)
		case stream:
			a.stream(ctx, tun, r)
		case idle:
			d := time.Duration(20+r.Intn(70)) * time.Second
			a.record("traffic", "idle", d, nil, true)
			_ = a.sleep(ctx, d)
		}
	}
}

// overBudget says whether the arm has already carried more than its share of
// the daily volume, so that the average stays near the target.
func (a *armRunner) overBudget() bool {
	if a.cfg.DailyGB <= 0 {
		return false
	}
	elapsed := a.clock().Sub(a.started)
	allowed := a.cfg.DailyGB*1e9*float64(elapsed)/float64(24*time.Hour) + 200e6
	return float64(a.bytesIn.Load()+a.bytesOut.Load()) > allowed
}

func (a *armRunner) httpClient(tun Tunnel) *http.Client {
	return &http.Client{
		Timeout: 45 * time.Second,
		Transport: &http.Transport{
			DialContext:         tun.DialContext,
			TLSHandshakeTimeout: 12 * time.Second,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// browse opens a few pages the way a person does: the page, the things it
// refers to fetched side by side, then a pause to read.
func (a *armRunner) browse(ctx context.Context, tun Tunnel, r *rand.Rand) {
	client := a.httpClient(tun)
	defer client.CloseIdleConnections()
	pages := 3 + r.Intn(5)
	for i := 0; i < pages && ctx.Err() == nil; i++ {
		site := pick(r, foreignSites, russianSites)
		page := a.fetch(ctx, client, site, 2<<20, true)
		if page != "" {
			base, err := url.Parse(site)
			if err == nil {
				var wg sync.WaitGroup
				for _, asset := range assetsOf(page, base, 6) {
					wg.Add(1)
					go func() {
						defer wg.Done()
						a.fetch(ctx, client, asset, 1<<20, false)
					}()
				}
				wg.Wait()
			}
		}
		_ = a.sleep(ctx, time.Duration(3+r.Intn(18))*time.Second)
	}
}

// pick chooses a site, half the time a foreign one and half a Russian one.
func pick(r *rand.Rand, foreign, russian []string) string {
	if r.Intn(2) == 0 {
		return foreign[r.Intn(len(foreign))]
	}
	return russian[r.Intn(len(russian))]
}

// fetch gets a URL, reads up to limit bytes, and records the outcome. It
// returns the body when keep is set.
func (a *armRunner) fetch(ctx context.Context, client *http.Client, target string, limit int64, keep bool) string {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		a.record("traffic", "fetch", 0, err, true)
		return ""
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "ru,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		a.reqErr.Add(1)
		a.recordURL("fetch", target, time.Since(started), err)
		return ""
	}
	defer resp.Body.Close()
	var body []byte
	if keep {
		body, err = io.ReadAll(io.LimitReader(resp.Body, limit))
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
	}
	if err != nil && ctx.Err() == nil {
		a.reqErr.Add(1)
		a.recordURL("fetch", target, time.Since(started), err)
		return ""
	}
	a.reqOK.Add(1)
	a.recordURL("fetch", target, time.Since(started), nil)
	return string(body)
}

// stream downloads a large file at a steady speed for a minute or so, like a
// video being watched.
func (a *armRunner) stream(ctx context.Context, tun Tunnel, r *rand.Rand) {
	client := a.httpClient(tun)
	client.Timeout = 0
	defer client.CloseIdleConnections()

	duration := time.Duration(30+r.Intn(61)) * time.Second
	started := time.Now()
	for _, file := range shuffled(r, bigFiles) {
		sctx, cancel := context.WithTimeout(ctx, duration)
		got, err := a.download(sctx, client, file)
		cancel()
		if err != nil && ctx.Err() == nil && sctx.Err() == nil {
			a.reqErr.Add(1)
			a.record("traffic", "stream", time.Since(started), fmt.Errorf("%s: %w", file, err), false)
			continue
		}
		a.reqOK.Add(1)
		a.recordData("traffic", "stream", time.Since(started), map[string]any{"url": file, "bytes": got})
		return
	}
}

// download reads a URL at the configured speed until the context ends.
func (a *armRunner) download(ctx context.Context, client *http.Client, target string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	const chunk = 32 << 10
	buf := make([]byte, chunk)
	perChunk := time.Duration(float64(time.Second) * float64(chunk) / (float64(a.cfg.StreamKBps) * 1024))
	var total int64
	for {
		started := time.Now()
		n, err := io.ReadFull(resp.Body, buf)
		total += int64(n)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF || ctx.Err() != nil {
				return total, nil
			}
			return total, err
		}
		if wait := perChunk - time.Since(started); wait > 0 {
			if a.sleep(ctx, wait) != nil {
				return total, nil
			}
		}
	}
}

func shuffled(r *rand.Rand, in []string) []string {
	out := append([]string(nil), in...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
