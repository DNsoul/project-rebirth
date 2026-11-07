package soak

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A run of this many failed probes in a row is taken to mean the thing is
// gone, not that one packet was lost.
const (
	directDeadRun = 5
	tunnelDeadRun = 4
)

// probeStat sums up one kind of probe.
type probeStat struct {
	N, Failed int

	// run is the current streak of failures, and runStart where it began.
	run      int
	runStart time.Time

	// DeadSince is when the first streak long enough to mean "gone" began.
	DeadSince time.Time
	deadRun   int
}

func (p *probeStat) add(at time.Time, ok bool, deadRun int) {
	p.N++
	if ok {
		p.run = 0
		return
	}
	p.Failed++
	if p.run == 0 {
		p.runStart = at
	}
	p.run++
	if p.run >= deadRun && p.DeadSince.IsZero() {
		p.DeadSince = p.runStart
	}
}

// reconnection is one attempt to bring a tunnel up again.
type reconnection struct {
	At      time.Time
	OK      bool
	Seconds int
	Attempt int
	Err     string
}

// ArmReport is what the logs say about one arm.
type ArmReport struct {
	Name        string
	Kind        string
	First, Last time.Time

	InBytes, OutBytes int64

	// Inside is the tunnel tried from inside, by kind of probe, and Outside the
	// server tried directly.
	Inside, Outside map[string]*probeStat

	Reconnections []reconnection
	Stalls        []time.Time
	FetchOK       int
	FetchFailed   int
}

// Summarize reads every arm's log in dir.
func Summarize(dir string) ([]ArmReport, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []ArmReport
	for _, f := range files {
		r, err := summarizeFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func summarizeFile(path string) (ArmReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return ArmReport{}, err
	}
	defer f.Close()

	r := ArmReport{Inside: map[string]*probeStat{}, Outside: map[string]*probeStat{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // a half-written last line is not worth stopping for
		}
		r.apply(e)
	}
	return r, sc.Err()
}

func (r *ArmReport) apply(e Event) {
	if r.Name == "" {
		r.Name = e.Arm
		r.First = e.At
	}
	r.Last = e.At
	ok := e.OK != nil && *e.OK

	switch e.Ev {
	case "start":
		if k, isString := e.Data["kind"].(string); isString {
			r.Kind = k
		}
	case "probe":
		stat(r.Inside, e.Kind).add(e.At, ok, tunnelDeadRun)
	case "direct":
		stat(r.Outside, e.Kind).add(e.At, ok, directDeadRun)
	case "volume":
		r.InBytes += int64(num(e.Data["in"]))
		r.OutBytes += int64(num(e.Data["out"]))
		r.FetchOK += int(num(e.Data["requests_ok"]))
		r.FetchFailed += int(num(e.Data["requests_failed"]))
	case "reconnect", "up":
		r.Reconnections = append(r.Reconnections, reconnection{
			At: e.At, OK: ok, Seconds: int(num(e.Data["seconds"])), Attempt: int(num(e.Data["attempt"])),
		})
	case "connect":
		r.Reconnections = append(r.Reconnections, reconnection{
			At: e.At, OK: false, Attempt: int(num(e.Data["attempt"])), Err: e.Err,
		})
	case "stalled":
		r.Stalls = append(r.Stalls, e.At)
	}
}

func stat(m map[string]*probeStat, kind string) *probeStat {
	if m[kind] == nil {
		m[kind] = &probeStat{}
	}
	return m[kind]
}

// num reads a number out of decoded JSON.
func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// Verdict is the short answer for one arm: what stopped working first, and when.
func (r ArmReport) Verdict() []string {
	var out []string
	if t, what := r.firstDirectDeath(); !t.IsZero() {
		out = append(out, fmt.Sprintf("сервер перестал отвечать напрямую с %s (%s), через %s после старта",
			clock(t), what, elapsed(t.Sub(r.First))))
	}
	for _, c := range r.Reconnections {
		if !c.OK {
			out = append(out, fmt.Sprintf("первое неудачное подключение в %s, через %s после старта",
				clock(c.At), elapsed(c.At.Sub(r.First))))
			break
		}
	}
	if p := r.Inside["http"]; p != nil && !p.DeadSince.IsZero() {
		out = append(out, fmt.Sprintf("туннель перестал пропускать трафик с %s, через %s после старта",
			clock(p.DeadSince), elapsed(p.DeadSince.Sub(r.First))))
	}
	if len(out) == 0 {
		out = append(out, "сбоев нет")
	}
	return out
}

// firstDirectDeath is the earliest moment any direct probe began a streak long
// enough to mean the server is unreachable.
func (r ArmReport) firstDirectDeath() (time.Time, string) {
	var at time.Time
	var what string
	for kind, p := range r.Outside {
		if p.DeadSince.IsZero() {
			continue
		}
		if at.IsZero() || p.DeadSince.Before(at) {
			at, what = p.DeadSince, kind
		}
	}
	return at, what
}

// elapsed writes a span of time the way it is read aloud: "4 ч 20 мин".
func elapsed(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%d ч %d мин", h, m)
	}
	return fmt.Sprintf("%d мин", m)
}

func clock(t time.Time) string { return t.Local().Format("02.01 15:04") }

func percent(p *probeStat) string {
	if p == nil || p.N == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%% из %d", 100*float64(p.N-p.Failed)/float64(p.N), p.N)
}

// Render writes the reports for a person.
func Render(reports []ArmReport) string {
	var b strings.Builder
	for _, r := range reports {
		fmt.Fprintf(&b, "== %s (%s)  %s → %s, длится %s, пропущено %.2f ГБ\n",
			r.Name, r.Kind, clock(r.First), clock(r.Last), elapsed(r.Last.Sub(r.First)),
			float64(r.InBytes+r.OutBytes)/1e9)
		fmt.Fprintf(&b, "   изнутри туннеля:  http %s, tcp %s, icmp %s\n",
			percent(r.Inside["http"]), percent(r.Inside["tcp"]), percent(r.Inside["icmp"]))
		var direct []string
		var kinds []string
		for k := range r.Outside {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			direct = append(direct, fmt.Sprintf("%s %s", k, percent(r.Outside[k])))
		}
		fmt.Fprintf(&b, "   сервер напрямую:  %s\n", strings.Join(direct, ", "))
		ok, bad := 0, 0
		for _, c := range r.Reconnections {
			if c.OK {
				ok++
			} else {
				bad++
			}
		}
		fmt.Fprintf(&b, "   подключения:      удачных %d, неудачных попыток %d, зависаний туннеля %d\n", ok, bad, len(r.Stalls))
		for _, c := range r.Reconnections {
			if c.OK {
				fmt.Fprintf(&b, "      %s  удачно за %d с (попытка %d)\n", clock(c.At), c.Seconds, c.Attempt)
			} else {
				fmt.Fprintf(&b, "      %s  НЕ УДАЛОСЬ (попытка %d): %s\n", clock(c.At), c.Attempt, c.Err)
			}
		}
		for _, v := range r.Verdict() {
			fmt.Fprintf(&b, "   >> %s\n", v)
		}
		b.WriteString("\n")
	}
	return b.String()
}
