package lifecycle

import (
	"encoding/json"
	"fmt"
	"time"

	"awgrotator/internal/node"
)

// StatusVersion is the layout version of a published status.
const StatusVersion = 1

// MoveSummary describes the last move attempt, for the status.
type MoveSummary struct {
	At      int64   `json:"at"`
	Trigger Trigger `json:"trigger"`
	State   string  `json:"state"`
	Error   string  `json:"error,omitempty"`
}

// StatusBody is what a server publishes about itself.
//
// It goes into a gist that anyone holding the app can read, so it carries
// counts and states and never an address, a name or anything about a user.
type StatusBody struct {
	Version    int    `json:"v"`
	Generation int    `json:"generation"`
	Region     string `json:"region"`
	AgeHours   int    `json:"age_hours"`

	// NextAutoMove is when the age trigger will fire, or zero when automatic
	// moves by age are off.
	NextAutoMove int64 `json:"next_auto_move,omitempty"`

	AutoAge   bool `json:"auto_age"`
	AutoBlock bool `json:"auto_block"`

	Health Verdict `json:"health"`

	// Why is the reason the server is staying put, or the reason it moved.
	Why string `json:"why"`

	CertDaysLeft int          `json:"cert_days_left"`
	LastMove     *MoveSummary `json:"last_move,omitempty"`

	// Servers is how many instances carry our tag, which is one except
	// during a move.
	Servers int  `json:"servers"`
	Revoked bool `json:"revoked,omitempty"`

	// Node is the tunnel's own report: handshakes, traffic and the resolver's
	// counters.
	Node *NodeSummary `json:"node,omitempty"`

	// Watch is the short account of the availability log: whether this server
	// can still get answers from Russia, and when that last changed. It is for
	// the record and nothing acts on it.
	Watch *WatchSummary `json:"watch,omitempty"`

	UpdatedAt int64 `json:"updated_at"`
}

// NodeSummary is the part of the node's status worth publishing.
type NodeSummary struct {
	Peers          int `json:"peers"`
	PeersConnected int `json:"peers_connected"`
	Queries        int `json:"queries,omitempty"`
	Direct         int `json:"direct,omitempty"`
	Tunnel         int `json:"tunnel,omitempty"`
	Failures       int `json:"failures,omitempty"`
}

// Summarize reduces a node status to what is safe to publish.
func Summarize(s node.Status) *NodeSummary {
	sum := &NodeSummary{Peers: len(s.Peers)}
	for _, p := range s.Peers {
		if p.Connected(s.UpdatedAt) {
			sum.PeersConnected++
		}
	}
	if s.Split != nil {
		sum.Queries = int(s.Split.Queries)
		sum.Direct = int(s.Split.Direct)
		sum.Tunnel = int(s.Split.Tunnel)
		sum.Failures = int(s.Split.Failures)
	}
	return sum
}

// SummaryOf describes an attempt for the status.
func SummaryOf(a *Attempt) *MoveSummary {
	if a == nil {
		return nil
	}
	return &MoveSummary{At: a.StartedAt, Trigger: a.Trigger, State: a.State, Error: a.Error}
}

// ParseStatus reads a published status body.
func ParseStatus(raw []byte) (StatusBody, error) {
	var s StatusBody
	if err := json.Unmarshal(raw, &s); err != nil {
		return StatusBody{}, fmt.Errorf("status is not valid JSON: %w", err)
	}
	if s.Version != StatusVersion {
		return StatusBody{}, fmt.Errorf("status version %d, this build understands %d", s.Version, StatusVersion)
	}
	return s, nil
}

// Describe renders a status for a person.
func (s StatusBody) Describe(now time.Time) string {
	out := fmt.Sprintf("поколение %d, регион %s, возраст %.1f суток, состояние: %s",
		s.Generation, s.Region, float64(s.AgeHours)/24, s.Health)
	if s.NextAutoMove != 0 {
		out += fmt.Sprintf("\nследующий плановый переезд: %s", time.Unix(s.NextAutoMove, 0).Format("2006-01-02"))
	} else {
		out += "\nплановые переезды: выключены"
	}
	out += fmt.Sprintf("\nсертификат действует ещё %d дн.", s.CertDaysLeft)
	if s.Why != "" {
		out += "\nпочему так: " + s.Why
	}
	if s.LastMove != nil {
		out += fmt.Sprintf("\nпоследний переезд: %s, причина «%s», итог «%s»",
			time.Unix(s.LastMove.At, 0).Format("2006-01-02 15:04"), s.LastMove.Trigger, s.LastMove.State)
		if s.LastMove.Error != "" {
			out += "\n  ошибка: " + s.LastMove.Error
		}
	}
	if w := s.Watch; w != nil {
		out += fmt.Sprintf("\nсвязь сервера с российскими сайтами (для записи, ни на что не влияет): %s с %s",
			watchRoundWords(w.Round), time.Unix(w.Since, 0).Format("2006-01-02 15:04"))
		out += fmt.Sprintf("\n  Россия: %s; контроль за рубежом: %s", w.RU, w.Control)
		for _, c := range w.Changes {
			what := "проба сервера"
			if c.What == "external" {
				what = "проверка с российских точек"
			}
			out += fmt.Sprintf("\n  %s  %s: %s", time.Unix(c.At, 0).Format("2006-01-02 15:04"), what, watchRoundWords(c.To))
		}
	}
	if s.Revoked {
		out += "\nОТОЗВАН: этот ключ сервера больше не действует"
	}
	return out
}

// watchRoundWords says a round in words.
func watchRoundWords(round string) string {
	switch round {
	case "good":
		return "отвечают"
	case "bad":
		return "НЕ отвечают"
	case "inconclusive":
		return "неясно (контроль за рубежом тоже не отвечает)"
	}
	return round
}
