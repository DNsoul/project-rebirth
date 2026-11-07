// Command awgwatch records, over time, who can reach a server and on which
// ports.
//
// It exists because "my VPN stopped working" has several very different
// causes that look identical from one connection: the machine died, the
// address stopped being routed, or one protocol is being filtered while
// everything else still passes. Watching from the operator's machine and
// from real networks inside the target country at the same time separates
// them.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"awgrotator/internal/globalping"
)

type target struct {
	Name string
	IP   string
}

// portResult is one measurement in one round.
type portResult struct {
	Reached  int      `json:"reached"`
	Total    int      `json:"total"`
	Failures []string `json:"failures,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type record struct {
	At     string `json:"at"`
	Target string `json:"target"`
	IP     string `json:"ip"`

	// Local is what this machine can reach, keyed by "tcp/22".
	Local map[string]bool `json:"local"`

	// Remote is what the probes in the chosen country can reach, keyed by
	// "ping", "udp/53", "tcp/443".
	Remote map[string]portResult `json:"remote"`
}

func main() {
	targetsFlag := flag.String("targets", "", "список name=ip через запятую")
	udpFlag := flag.String("udp", "53,443,34567", "UDP-порты для проверки запросом DNS")
	tcpFlag := flag.String("tcp", "22,443", "TCP-порты для проверки с этой машины")
	httpsPort := flag.Int("https", 443, "TCP-порт для проверки TLS из страны (0 — не проверять)")
	country := flag.String("country", "RU", "страна проб")
	queryName := flag.String("query", "example.com", "имя для запроса DNS (наш зонд отвечает на любое)")
	probes := flag.Int("probes", 10, "сколько проб на замер")
	every := flag.Duration("every", 10*time.Minute, "период обхода")
	out := flag.String("out", "watch.jsonl", "куда дописывать записи")
	once := flag.Bool("once", false, "один обход и выход")
	flag.Parse()

	log.SetFlags(log.Ltime | log.LUTC)

	targets, err := parseTargets(*targetsFlag)
	if err != nil {
		log.Fatalf("awgwatch: %v", err)
	}
	udpPorts, err := parsePorts(*udpFlag)
	if err != nil {
		log.Fatalf("awgwatch: -udp: %v", err)
	}
	tcpPorts, err := parsePorts(*tcpFlag)
	if err != nil {
		log.Fatalf("awgwatch: -tcp: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gp := globalping.New()
	if remaining, reset, err := gp.Limits(ctx); err == nil {
		perRound := len(targets) * (1 + len(udpPorts) + boolToInt(*httpsPort > 0))
		log.Printf("замеров осталось %d (сброс через %s), обход тратит %d",
			remaining, time.Duration(reset)*time.Second, perRound)
	}

	round := func() {
		for _, t := range targets {
			rec := collect(ctx, gp, t, udpPorts, tcpPorts, *httpsPort, *country, *queryName, *probes)
			if err := appendRecord(*out, rec); err != nil {
				log.Printf("не записалось: %v", err)
			}
			log.Print(format(rec))
		}
	}
	round()
	if *once {
		return
	}

	ticker := time.NewTicker(*every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("остановлено")
			return
		case <-ticker.C:
			round()
		}
	}
}

func collect(ctx context.Context, gp *globalping.Client, t target,
	udpPorts, tcpPorts []int, httpsPort int, country, queryName string, probes int) record {

	rec := record{
		At:     time.Now().UTC().Format(time.RFC3339),
		Target: t.Name,
		IP:     t.IP,
		Local:  map[string]bool{},
		Remote: map[string]portResult{},
	}

	for _, port := range tcpPorts {
		rec.Local[fmt.Sprintf("tcp/%d", port)] = dialable(t.IP, port)
	}

	rec.Remote["ping"] = wrap(gp.Ping(ctx, t.IP, country, probes))
	for _, port := range udpPorts {
		rec.Remote[fmt.Sprintf("udp/%d", port)] =
			wrap(gp.DNSOverUDP(ctx, t.IP, port, queryName, country, probes))
	}
	if httpsPort > 0 {
		rec.Remote[fmt.Sprintf("tcp/%d", httpsPort)] =
			wrap(gp.HTTPS(ctx, t.IP, httpsPort, country, probes))
	}
	return rec
}

func wrap(r globalping.Result, err error) portResult {
	if err != nil {
		return portResult{Error: err.Error()}
	}
	return portResult{Reached: r.Reached, Total: r.Total, Failures: r.Failures()}
}

// dialable reports whether this machine can open a TCP connection.
func dialable(ip string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 6*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func format(rec record) string {
	var local, remote []string
	for _, k := range sortedKeys(rec.Local) {
		mark := "нет"
		if rec.Local[k] {
			mark = "да"
		}
		local = append(local, k+"="+mark)
	}
	// Keep ping first, then the ports in order.
	order := append([]string{"ping"}, sortedKeys(rec.Remote)...)
	seen := map[string]bool{}
	for _, k := range order {
		if seen[k] || k == "" {
			continue
		}
		seen[k] = true
		r, ok := rec.Remote[k]
		if !ok {
			continue
		}
		if r.Error != "" {
			remote = append(remote, k+"=ошибка")
			continue
		}
		remote = append(remote, fmt.Sprintf("%s %d/%d", k, r.Reached, r.Total))
	}
	return fmt.Sprintf("%-8s %-15s отсюда[%s] оттуда[%s]",
		rec.Target, rec.IP, strings.Join(local, " "), strings.Join(remote, " "))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "ping" {
			keys = append(keys, k)
		}
	}
	// Numeric-ish sort by the port after the slash.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if portOf(keys[j]) < portOf(keys[i]) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func portOf(key string) int {
	_, after, ok := strings.Cut(key, "/")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(after)
	return n
}

func appendRecord(path string, rec record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func parseTargets(s string) ([]target, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("-targets не задан, нужен вид name=ip")
	}
	var out []target
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		name, ip, ok := strings.Cut(field, "=")
		if !ok {
			return nil, fmt.Errorf("цель %q должна быть вида name=ip", field)
		}
		if net.ParseIP(strings.TrimSpace(ip)) == nil {
			return nil, fmt.Errorf("%q не похоже на адрес", ip)
		}
		out = append(out, target{Name: strings.TrimSpace(name), IP: strings.TrimSpace(ip)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("целей не осталось")
	}
	return out, nil
}

// parsePorts reads a comma separated port list. "none" means an empty list:
// passing an empty string is not reliable, because PowerShell drops empty
// arguments when it calls a native program, and the next flag then gets
// swallowed as this one's value.
func parsePorts(s string) ([]int, error) {
	if t := strings.TrimSpace(s); t == "" || t == "none" || t == "-" {
		return nil, nil
	}
	var out []int
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("порт %q некорректен", field)
		}
		out = append(out, n)
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
