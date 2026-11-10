package main

import (
	"strings"
	"testing"
	"time"

	"awgrotator/internal/lifecycle"
)

var base = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).Unix()

// line makes a sample at minute m after base.
func line(m int, round string, mutate func(*lifecycle.Sample)) lifecycle.Sample {
	s := lifecycle.Sample{
		At: base + int64(m)*60, Gen: 6, Round: round, Health: "healthy", RUms: 22,
		RU:    lifecycle.Tally{TCP: [2]int{12, 12}, UDP: [2]int{2, 2}, ICMP: [2]int{9, 12}},
		Ctl:   lifecycle.Tally{TCP: [2]int{4, 4}, UDP: [2]int{2, 2}, ICMP: [2]int{4, 4}},
		Peers: &lifecycle.PeerTally{Known: 3, Up: 2, NewestS: 20},
	}
	if round == "bad" {
		s.RU.TCP, s.RU.UDP, s.RU.ICMP = [2]int{0, 12}, [2]int{0, 2}, [2]int{0, 12}
		s.Failed = []string{"ya.ru/tcp", "vk.com/tcp", "77.88.8.8/udp"}
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func ext(round string, ru int) func(*lifecycle.Sample) {
	return func(s *lifecycle.Sample) {
		s.Ext = &lifecycle.ExtSample{At: s.At - 30, RU: [2]int{ru, 10}, Control: [2]int{6, 6}, Round: round}
	}
}

func noClients(s *lifecycle.Sample) { s.Peers.Up = 0 }

func both(fs ...func(*lifecycle.Sample)) func(*lifecycle.Sample) {
	return func(s *lifecycle.Sample) {
		for _, f := range fs {
			if f != nil {
				f(s)
			}
		}
	}
}

// timeline: two quiet hours, a slowdown, the server loses Russia at minute 150,
// the clients go quiet ten minutes later, the outside check notices at minute
// 172, and everything is back at minute 190.
func timeline() []lifecycle.Sample {
	var out []lifecycle.Sample
	for m := 0; m < 150; m += 2 {
		round := "good"
		var mut func(*lifecycle.Sample)
		if m%10 == 0 {
			mut = ext("good", 10)
		}
		if m >= 120 {
			mut = both(mut, func(s *lifecycle.Sample) { s.RUms = 70 }) // the path slows before it breaks
		}
		out = append(out, line(m, round, mut))
	}
	for m := 150; m < 190; m += 2 {
		var mut func(*lifecycle.Sample)
		if m >= 160 {
			mut = noClients
		}
		if m >= 172 && m%2 == 0 {
			mut = both(mut, ext("bad", 0))
		} else if m%10 == 0 {
			mut = both(mut, ext("good", 10))
		}
		out = append(out, line(m, "bad", mut))
	}
	for m := 190; m < 210; m += 2 {
		out = append(out, line(m, "good", nil))
	}
	return out
}

func TestTheReportSetsTheWaysOfNoticingSideBySide(t *testing.T) {
	out := describeWatch(timeline(), time.UTC)

	for _, want := range []string{
		"Случай 1: сервер перестал получать ответы из России в 2026-11-10 14:30",
		"закончилась в 2026-11-10 15:08",
		"Проверка с российских точек: заметила в 2026-11-10 14:51, на 22 мин позже",
		"Клиенты: к 2026-11-10 14:40 подключённых не осталось (перед этим было 2)",
		"Что не отвечало: TCP 2 из 12, UDP 1 из 2",
		"Перед этим медленнее: время соединения выросло с 22 до 70 мс",
		"Последние записи:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Случай 2") {
		t.Errorf("one loss is one case:\n%s", out)
	}
}

func TestAnIncidentThatIsStillGoingOnSaysSo(t *testing.T) {
	var s []lifecycle.Sample
	for m := 0; m < 20; m += 2 {
		s = append(s, line(m, "good", nil))
	}
	for m := 20; m < 40; m += 2 {
		s = append(s, line(m, "bad", ext("bad", 1)))
	}
	out := describeWatch(s, time.UTC)
	if !strings.Contains(out, "продолжается") || !strings.Contains(out, "заметила") {
		t.Errorf("%s", out)
	}
}

func TestWhenTheOutsideCheckMissedItTheReportSaysSo(t *testing.T) {
	var s []lifecycle.Sample
	for m := 0; m < 20; m += 2 {
		s = append(s, line(m, "good", ext("good", 10)))
	}
	for m := 20; m < 60; m += 2 {
		mut := noClients
		if m%10 == 0 {
			s = append(s, line(m, "bad", both(noClients, ext("good", 10))))
			continue
		}
		s = append(s, line(m, "bad", mut))
	}
	s = append(s, line(60, "good", nil))
	out := describeWatch(s, time.UTC)
	if !strings.Contains(out, "НЕ заметила") {
		t.Errorf("a block the outside check never saw must be said plainly:\n%s", out)
	}
}

func TestTheOutsideCheckSeeingWhatTheServerDidNotIsReported(t *testing.T) {
	var s []lifecycle.Sample
	for m := 0; m < 60; m += 2 {
		var mut func(*lifecycle.Sample)
		if m >= 20 && m < 40 {
			mut = ext("bad", 0)
		} else if m%10 == 0 {
			mut = ext("good", 10)
		}
		s = append(s, line(m, "good", mut))
	}
	out := describeWatch(s, time.UTC)
	if !strings.Contains(out, "а проба с самого сервера нет") || strings.Contains(out, "Случай 1") {
		t.Errorf("%s", out)
	}
}

func TestNothingToReportIsSaidAsSuch(t *testing.T) {
	var s []lifecycle.Sample
	for m := 0; m < 30; m += 2 {
		s = append(s, line(m, "good", nil))
	}
	// A look that could not run says nothing and does not split the stretch.
	s[5].Round = "unknown"
	out := describeWatch(s, time.UTC)
	if !strings.Contains(out, "Потерь связи с Россией за это время не было") {
		t.Errorf("%s", out)
	}
	if got := len(rounds(s)); got != 1 {
		t.Errorf("%d stretches, want 1", got)
	}
}

func TestIfNoClientsWereConnectedTheyCannotBeTheEvidence(t *testing.T) {
	var s []lifecycle.Sample
	for m := 0; m < 20; m += 2 {
		s = append(s, line(m, "good", noClients))
	}
	for m := 20; m < 40; m += 2 {
		s = append(s, line(m, "bad", noClients))
	}
	out := describeWatch(s, time.UTC)
	if !strings.Contains(out, "судить по ним нельзя") {
		t.Errorf("%s", out)
	}
}

func TestSpansAreRenderedForPeople(t *testing.T) {
	for in, want := range map[int64]string{45: "45 с", 600: "10 мин", 5399: "90 мин", 5400: "1.5 ч", 7 * 3600: "7.0 ч", -600: "10 мин"} {
		if got := span(in); got != want {
			t.Errorf("span(%d) = %q, want %q", in, got, want)
		}
	}
}
