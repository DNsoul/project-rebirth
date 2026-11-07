package deploy

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"awgrotator/internal/node"
	"awgrotator/internal/remote"
)

// SplitProbe is one site resolved through the new resolver.
type SplitProbe struct {
	Domain    string
	Addrs     []netip.Addr
	Tunnelled bool // every address is a stand-in
}

// SplitReport is what the check found.
type SplitReport struct {
	Probes []SplitProbe

	// MapEntries is how many translations the kernel holds.
	MapEntries int
}

// String renders the report for the operator.
func (r SplitReport) String() string {
	var b strings.Builder
	for _, p := range r.Probes {
		way := "напрямую"
		switch {
		case len(p.Addrs) == 0:
			way = "нет ответа"
		case p.Tunnelled:
			way = "туннель"
		}
		addrs := make([]string, len(p.Addrs))
		for i, a := range p.Addrs {
			addrs[i] = a.String()
		}
		fmt.Fprintf(&b, "    %-16s %-10s %s\n", p.Domain, way, strings.Join(addrs, " "))
	}
	fmt.Fprintf(&b, "    переводов в ядре: %d", r.MapEntries)
	return b.String()
}

// elementPair matches one translation in the output of "nft list map".
var elementPair = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3} : \d{1,3}(?:\.\d{1,3}){3}`)

// Domains asked during the check. One is foreign and must be tunnelled; the
// rest are local and are reported rather than required, because which view of
// a site a resolver sees can legitimately differ, and a deployment that
// destroyed the server over it would be the wrong response.
var (
	foreignProbe = "github.com"
	localProbes  = []string{"yandex.ru", "vk.com", "ozon.ru", "gosuslugi.ru"}
)

// checkScript asks the resolver on the server itself and lists the kernel's
// translation map.
func checkScript(dns string) string {
	domains := append([]string{foreignProbe}, localProbes...)
	var b strings.Builder
	b.WriteString("set -u\n")
	for _, d := range domains {
		fmt.Fprintf(&b, `echo "Q %[1]s $(dig +short +time=5 +tries=1 @%[2]s %[1]s A | tr '\n' ' ')"`+"\n", d, dns)
	}
	b.WriteString(`echo "MAP"` + "\n")
	b.WriteString(`nft list map ip awgsplit fake2real 2>&1` + "\n")
	return b.String()
}

// ParseSplitCheck reads the output of checkScript.
func ParseSplitCheck(out string, stand netip.Prefix) (SplitReport, error) {
	var rep SplitReport
	inMap := false

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "MAP":
			inMap = true
		case inMap:
			// Elements look like "198.18.0.5 : 142.250.1.1". The line that
			// declares the map's type also contains " : " ("type ipv4_addr :
			// ipv4_addr"), so counting separators would call an empty map
			// non-empty. Only a pair of addresses is an element.
			rep.MapEntries += len(elementPair.FindAllString(line, -1))
		case strings.HasPrefix(line, "Q "):
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			probe := SplitProbe{Domain: fields[1]}
			for _, f := range fields[2:] {
				if a, err := netip.ParseAddr(f); err == nil {
					probe.Addrs = append(probe.Addrs, a)
				}
			}
			if len(probe.Addrs) > 0 {
				probe.Tunnelled = true
				for _, a := range probe.Addrs {
					if !stand.Contains(a) {
						probe.Tunnelled = false
					}
				}
			}
			rep.Probes = append(rep.Probes, probe)
		}
	}
	if len(rep.Probes) == 0 {
		return rep, fmt.Errorf("the check produced no answers:\n%s", out)
	}
	return rep, nil
}

// VerifySplit proves the resolver works on the new server before the
// configuration is published. It checks the structural facts that make a
// server unusable if they are wrong, and reports the rest.
//
// It exercises the real path as far as a check on the server can: the
// resolver answers, queries reach the upstream resolvers, a foreign site is
// given a stand-in, and the kernel holds the translation for it. What it
// cannot cover is traffic through the tunnel itself, which needs a client.
func VerifySplit(cli *remote.Client, cfg *node.Config, status node.Status, logf Logf) error {
	if cfg.Split == nil {
		return nil
	}
	if status.Split == nil {
		return fmt.Errorf("узел не сообщил о резолвере: разделение не запустилось")
	}
	if status.Split.RURanges == 0 {
		return fmt.Errorf("резолвер запущен без списка российских сетей: всё пошло бы через туннель")
	}
	logf("  резолвер: %d российских диапазонов", status.Split.RURanges)

	stand, err := netip.ParsePrefix(cfg.Split.StandInRange)
	if err != nil {
		return err
	}

	out, err := cli.Run(checkScript(cfg.Split.DNSAddress))
	if err != nil {
		return fmt.Errorf("проверка резолвера не выполнилась: %w", err)
	}
	rep, err := ParseSplitCheck(out, stand)
	if err != nil {
		return err
	}
	logf("  проверка резолвера:\n%s", rep)

	var foreign *SplitProbe
	for i := range rep.Probes {
		if rep.Probes[i].Domain == foreignProbe {
			foreign = &rep.Probes[i]
		}
	}
	switch {
	case foreign == nil || len(foreign.Addrs) == 0:
		return fmt.Errorf("резолвер не ответил про %s: проверь доступ сервера к 8.8.8.8 и 1.1.1.1", foreignProbe)
	case !foreign.Tunnelled:
		return fmt.Errorf("%s получил настоящий адрес вместо подставного: зарубежный сайт пошёл бы мимо туннеля", foreignProbe)
	case rep.MapEntries == 0:
		return fmt.Errorf("подставной адрес выдан, но в ядре нет перевода для него: трафик не дошёл бы")
	}
	return nil
}
