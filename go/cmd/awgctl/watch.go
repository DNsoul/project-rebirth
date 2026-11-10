package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/remote"
	"awgrotator/internal/store"
)

// cmdWatch reads the availability log the server keeps and says what it shows.
//
// The log is a record of whether the server could still get answers from Russia,
// written every few minutes next to what its own clients were doing and what the
// outside check (hosts in Russia pinging the server) found. Nothing acts on it.
// It is here to answer, after the fact, whether noticing a block from the server
// itself would have worked: for each time the server lost Russia, when did the
// outside check notice, and when did the clients go quiet.
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	since := fs.Duration("since", 7*24*time.Hour, "за какой срок показать")
	raw := fs.Bool("raw", false, "вывести строки журнала как есть")
	file := fs.String("file", "", "прочитать журнал из этого файла, а не с сервера")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var data []byte
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		data = b
	} else {
		b, err := fetchWatchLog()
		if err != nil {
			return err
		}
		data = b
	}
	if *raw {
		fmt.Print(string(data))
		return nil
	}

	samples := lifecycle.ParseSamples(data)
	cut := time.Now().Add(-*since).Unix()
	kept := samples[:0:0]
	for _, s := range samples {
		if s.At >= cut {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		fmt.Println("В журнале нет записей за этот срок. Менеджер пишет в него раз в две минуты, начиная с версии, где он появился.")
		return nil
	}
	fmt.Print(describeWatch(kept, time.Local))
	return nil
}

// fetchWatchLog reads the log, older file first, from the current server.
func fetchWatchLog() ([]byte, error) {
	root, err := config.Root()
	if err != nil {
		return nil, err
	}
	st, err := store.Load(root)
	if err != nil {
		return nil, err
	}
	syncIfPossible(root, st)
	if st.Server.IP == "" {
		return nil, errors.New("сервера нет, запусти \"awgctl up\"")
	}
	signer, err := st.SSHSigner()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli, err := remote.Dial(ctx, st.Server.IP, "root", signer, st.ServerHostKey)
	if err != nil {
		return nil, fmt.Errorf("%w\n(если сервер закрыт от вашей сети, свод его записей приходит и через GitHub и GitLab: awgctl events)", err)
	}
	defer cli.Close()

	log := path.Join(lifecycle.DefaultDataDir, lifecycle.WatchLogName)
	out, err := cli.Run(fmt.Sprintf("for f in %s.1 %s; do [ -f \"$f\" ] && cat \"$f\"; done; true", log, log))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, errors.New("журнала на сервере ещё нет: менеджер на нём старой версии, обнови: awgctl update -manager")
	}
	return []byte(out), nil
}

// period is a stretch of samples in which something stayed the same.
type period struct {
	round    string
	from, to int64
	samples  int
}

// rounds splits the samples into stretches of the same reverse round. A sample
// that could not look says nothing, so the stretch runs through it.
func rounds(samples []lifecycle.Sample) []period {
	var out []period
	for _, s := range samples {
		if s.Round == "unknown" {
			if n := len(out); n > 0 {
				out[n-1].to = s.At
			}
			continue
		}
		if n := len(out); n > 0 && out[n-1].round == s.Round {
			out[n-1].to = s.At
			out[n-1].samples++
			continue
		}
		out = append(out, period{round: s.Round, from: s.At, to: s.At, samples: 1})
	}
	return out
}

// outsideRounds does the same for the outside check, whose results ride on the
// lines that follow it.
func outsideRounds(samples []lifecycle.Sample) []period {
	var out []period
	for _, s := range samples {
		e := s.Ext
		if e == nil || e.Round == "unknown" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].round == e.Round {
			out[n-1].to = e.At
			out[n-1].samples++
			continue
		}
		out = append(out, period{round: e.Round, from: e.At, to: e.At, samples: 1})
	}
	return out
}

const incidentSlack = 30 * 60 // how far apart in time two ways of noticing may be and still be one event

func clock(at int64, loc *time.Location) string {
	return time.Unix(at, 0).In(loc).Format("2006-01-02 15:04")
}

func span(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	if d < 0 {
		d = -d
	}
	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("%d с", int(d/time.Second))
	case d < 90*time.Minute:
		return fmt.Sprintf("%d мин", int((d+30*time.Second)/time.Minute))
	}
	return fmt.Sprintf("%.1f ч", d.Hours())
}

// describeWatch tells what the log shows, in words, and for each time the server
// lost Russia sets the ways of noticing side by side.
func describeWatch(samples []lifecycle.Sample, loc *time.Location) string {
	var b strings.Builder
	first, last := samples[0], samples[len(samples)-1]

	gens := map[int]bool{}
	for _, s := range samples {
		gens[s.Gen] = true
	}
	fmt.Fprintf(&b, "Записей %d, с %s по %s, поколений сервера: %d.\n", len(samples), clock(first.At, loc), clock(last.At, loc), len(gens))

	ps := rounds(samples)
	outside := outsideRounds(samples)

	// What the server saw of Russia, as a list of stretches.
	b.WriteString("\nСвязь сервера с российскими сайтами (проба с самого сервера):\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "  %s — %s  %-12s %s\n", clock(p.from, loc), clock(p.to, loc), watchWords(p.round), span(p.to-p.from))
	}

	// Each stretch of "bad" against the other witnesses.
	var incidents []period
	for _, p := range ps {
		if p.round == "bad" {
			incidents = append(incidents, p)
		}
	}
	if len(incidents) == 0 {
		b.WriteString("\nПотерь связи с Россией за это время не было.\n")
	}
	for i, p := range incidents {
		ongoing := p == ps[len(ps)-1]
		end := "продолжается"
		if !ongoing {
			end = "закончилась в " + clock(p.to, loc)
		}
		fmt.Fprintf(&b, "\nСлучай %d: сервер перестал получать ответы из России в %s, %s.\n", i+1, clock(p.from, loc), end)

		// the outside check
		var seen *period
		for j := range outside {
			o := outside[j]
			if o.round == "bad" && o.from <= p.to+incidentSlack && o.to >= p.from-incidentSlack {
				seen = &outside[j]
				break
			}
		}
		switch {
		case seen == nil && hasExt(samples, p.from-incidentSlack, p.to+incidentSlack):
			b.WriteString("  Проверка с российских точек: НЕ заметила (всё это время считала сервер доступным).\n")
		case seen == nil:
			b.WriteString("  Проверка с российских точек: в это время не работала.\n")
		case seen.from >= p.from:
			fmt.Fprintf(&b, "  Проверка с российских точек: заметила в %s, на %s позже.\n", clock(seen.from, loc), span(seen.from-p.from))
		default:
			fmt.Fprintf(&b, "  Проверка с российских точек: заметила в %s, на %s раньше.\n", clock(seen.from, loc), span(p.from-seen.from))
		}

		// the clients
		b.WriteString("  " + clientsDuring(samples, p, loc) + "\n")

		// what failed
		if f := failedDuring(samples, p); f != "" {
			b.WriteString("  Что не отвечало: " + f + ".\n")
		}
		if ms := slowBefore(samples, p); ms != "" {
			b.WriteString("  " + ms + "\n")
		}
	}

	// The outside check seeing a block the server's own look did not.
	for _, o := range outside {
		if o.round != "bad" {
			continue
		}
		covered := false
		for _, p := range incidents {
			if o.from <= p.to+incidentSlack && o.to >= p.from-incidentSlack {
				covered = true
			}
		}
		if !covered {
			fmt.Fprintf(&b, "\nПроверка с российских точек видела блокировку с %s по %s, а проба с самого сервера нет.\n", clock(o.from, loc), clock(o.to, loc))
		}
	}

	// the most recent lines
	b.WriteString("\nПоследние записи:\n")
	from := len(samples) - 5
	if from < 0 {
		from = 0
	}
	for _, s := range samples[from:] {
		b.WriteString("  " + oneLine(s, loc) + "\n")
	}
	return b.String()
}

func watchWords(round string) string {
	switch round {
	case "good":
		return "отвечают"
	case "bad":
		return "НЕ ОТВЕЧАЮТ"
	case "inconclusive":
		return "неясно"
	}
	return round
}

func hasExt(samples []lifecycle.Sample, from, to int64) bool {
	for _, s := range samples {
		if s.Ext != nil && s.Ext.Round != "unknown" && s.Ext.At >= from && s.Ext.At <= to {
			return true
		}
	}
	return false
}

// clientsDuring says what the server's own clients were doing around an incident:
// the plainest evidence of whether people were actually cut off.
func clientsDuring(samples []lifecycle.Sample, p period, loc *time.Location) string {
	var before, during *lifecycle.Sample
	for i := range samples {
		s := &samples[i]
		if s.Peers == nil {
			continue
		}
		if s.At < p.from {
			before = s
		}
		if s.At >= p.from-10*60 && s.At <= p.to && s.Peers.Up == 0 && during == nil {
			during = s
		}
	}
	switch {
	case before != nil && before.Peers.Up == 0 && during != nil:
		return "Клиенты: до этого уже никто не был подключён, судить по ним нельзя."
	case during != nil:
		return fmt.Sprintf("Клиенты: к %s подключённых не осталось (перед этим было %d).", clock(during.At, loc), upBefore(before))
	case before == nil:
		return "Клиенты: в журнале нет данных о них."
	}
	return "Клиенты: подключённые оставались, несмотря на потерю связи с Россией."
}

func upBefore(s *lifecycle.Sample) int {
	if s == nil || s.Peers == nil {
		return 0
	}
	return s.Peers.Up
}

// failedDuring names what did not answer, from the last line of the incident.
func failedDuring(samples []lifecycle.Sample, p period) string {
	var last *lifecycle.Sample
	for i := range samples {
		if samples[i].Round == "bad" && samples[i].At >= p.from && samples[i].At <= p.to {
			last = &samples[i]
		}
	}
	if last == nil {
		return ""
	}
	var tcp, udp int
	for _, f := range last.Failed {
		switch {
		case strings.HasSuffix(f, "/tcp"):
			tcp++
		case strings.HasSuffix(f, "/udp"):
			udp++
		}
	}
	return fmt.Sprintf("TCP %d из %d, UDP %d из %d", tcp, last.RU.TCP[1], udp, last.RU.UDP[1])
}

// slowBefore notes whether the path to Russia had slowed down before it broke:
// a throttled path would be seen coming.
func slowBefore(samples []lifecycle.Sample, p period) string {
	var base, before int
	var nBase int
	for _, s := range samples {
		if s.At >= p.from || s.RUms == 0 {
			continue
		}
		if p.from-s.At > 60*60 {
			base += s.RUms
			nBase++
		} else {
			before = s.RUms
		}
	}
	if nBase == 0 || before == 0 {
		return ""
	}
	if usual := base / nBase; before > usual*3/2 {
		return fmt.Sprintf("Перед этим медленнее: время соединения выросло с %d до %d мс.", usual, before)
	}
	return ""
}

func oneLine(s lifecycle.Sample, loc *time.Location) string {
	line := fmt.Sprintf("%s  %-11s РФ tcp %d/%d udp %d/%d icmp %d/%d · контроль tcp %d/%d",
		clock(s.At, loc), watchWords(s.Round),
		s.RU.TCP[0], s.RU.TCP[1], s.RU.UDP[0], s.RU.UDP[1], s.RU.ICMP[0], s.RU.ICMP[1],
		s.Ctl.TCP[0], s.Ctl.TCP[1])
	if s.Peers != nil {
		line += fmt.Sprintf(" · клиентов %d из %d", s.Peers.Up, s.Peers.Known)
	}
	if s.Ext != nil {
		line += fmt.Sprintf(" · снаружи %d/%d (%s)", s.Ext.RU[0], s.Ext.RU[1], watchWords(s.Ext.Round))
	}
	if s.Err != "" {
		line += " · проба не сработала: " + s.Err
	}
	return line
}
