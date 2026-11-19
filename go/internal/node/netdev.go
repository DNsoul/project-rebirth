package node

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// since is how much a counter grew. A counter that went down was reset (a
// card came back); what it shows then is all new.
func since(prev, cur uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// parseNetDev reads /proc/net/dev: for each card the bytes received, the
// first number after its name, and sent, the ninth. Cards named in skip, or
// starting with one of those names (other than "lo"), are left out.
func parseNetDev(r io.Reader, skip []string) (map[string][2]uint64, error) {
	out := map[string][2]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if skipped(name, skip) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[name] = [2]uint64{rx, tx}
	}
	return out, sc.Err()
}

func skipped(name string, skip []string) bool {
	for _, s := range skip {
		if s == "" {
			continue
		}
		if name == s || (s != "lo" && strings.HasPrefix(name, s)) {
			return true
		}
	}
	return false
}
