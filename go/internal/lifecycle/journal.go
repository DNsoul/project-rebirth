package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// States of a move attempt, in the order they are reached.
const (
	StateStarted     = "started"     // decided to move, nothing created yet
	StateProvisioned = "provisioned" // the new server exists and is installed
	StateVerified    = "verified"    // it passed the tunnel test
	StatePublished   = "published"   // clients have been told about it
	StateRetiring    = "retiring"    // waiting out the grace period
	StateRetired     = "retired"     // this server has been deleted
	StateFailed      = "failed"
)

// Attempt is one try at replacing the server.
type Attempt struct {
	ID         string  `json:"id"`
	StartedAt  int64   `json:"started_at"`
	Trigger    Trigger `json:"trigger"`
	Region     string  `json:"region,omitempty"`
	InstanceID string  `json:"instance_id,omitempty"`
	NewIP      string  `json:"new_ip,omitempty"`
	State      string  `json:"state"`
	Error      string  `json:"error,omitempty"`

	// RetireAt is when this server is due to delete itself, once published.
	RetireAt int64 `json:"retire_at,omitempty"`

	// PublishedAt is when clients were told, which the earliest early
	// retirement is measured from.
	PublishedAt int64 `json:"published_at,omitempty"`
}

// Journal is the manager's memory, kept on disk so that a restart in the
// middle of a move, which is exactly when something has gone wrong, does not
// lose track of a server it created or one it is about to delete.
type Journal struct {
	Attempts []Attempt `json:"attempts"`

	// Executed are the IDs of operator commands already carried out, so a
	// command that stays in the gist is not run again.
	Executed []string `json:"executed"`

	// LastSeq is the highest configuration sequence number published.
	LastSeq uint64 `json:"last_seq"`

	// AgeJitterSec is the random extra on the age limit, drawn once.
	AgeJitterSec int64 `json:"age_jitter_sec"`

	// MinEpoch is the lowest certificate epoch the operator has not revoked.
	MinEpoch uint32 `json:"min_epoch"`

	LastStatusAt    int64 `json:"last_status_at"`
	LastHeartbeatAt int64 `json:"last_heartbeat_at"`
}

// How much history to keep.
const (
	maxAttempts = 50
	maxExecuted = 200
)

// Latest returns the most recent attempt, or nil.
func (j Journal) Latest() *Attempt {
	if len(j.Attempts) == 0 {
		return nil
	}
	return &j.Attempts[len(j.Attempts)-1]
}

// AttemptTimes returns when each attempt started.
func (j Journal) AttemptTimes() []time.Time {
	out := make([]time.Time, len(j.Attempts))
	for i, a := range j.Attempts {
		out[i] = time.Unix(a.StartedAt, 0)
	}
	return out
}

// WasExecuted reports whether a command has already been carried out.
func (j Journal) WasExecuted(id string) bool {
	for _, e := range j.Executed {
		if e == id {
			return true
		}
	}
	return false
}

// MarkExecuted remembers a command, forgetting the oldest beyond the limit.
func (j *Journal) MarkExecuted(id string) {
	if j.WasExecuted(id) {
		return
	}
	j.Executed = append(j.Executed, id)
	if len(j.Executed) > maxExecuted {
		j.Executed = j.Executed[len(j.Executed)-maxExecuted:]
	}
}

// find returns the attempt with the given ID.
func (j *Journal) find(id string) *Attempt {
	for i := range j.Attempts {
		if j.Attempts[i].ID == id {
			return &j.Attempts[i]
		}
	}
	return nil
}

// JournalStore is a Journal and the file it lives in.
type JournalStore struct {
	path string

	mu sync.Mutex
	j  Journal
}

// OpenJournal reads the journal, or starts an empty one if there is none.
func OpenJournal(path string) (*JournalStore, error) {
	s := &JournalStore{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.j); err != nil {
		return nil, fmt.Errorf("%s is not a valid journal: %w", path, err)
	}
	return s, nil
}

// Read gives a copy of the journal.
func (s *JournalStore) Read() Journal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copy()
}

func (s *JournalStore) copy() Journal {
	c := s.j
	c.Attempts = append([]Attempt(nil), s.j.Attempts...)
	c.Executed = append([]string(nil), s.j.Executed...)
	return c
}

// Update changes the journal and saves it, atomically. If saving fails the
// change is rolled back, so what is in memory is never ahead of the disk.
func (s *JournalStore) Update(fn func(*Journal)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	before := s.copy()
	fn(&s.j)
	if len(s.j.Attempts) > maxAttempts {
		s.j.Attempts = s.j.Attempts[len(s.j.Attempts)-maxAttempts:]
	}
	if err := s.save(); err != nil {
		s.j = before
		return err
	}
	return nil
}

func (s *JournalStore) save() error {
	raw, err := json.MarshalIndent(s.j, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Begin records a new attempt.
func (s *JournalStore) Begin(a Attempt) error {
	return s.Update(func(j *Journal) { j.Attempts = append(j.Attempts, a) })
}

// Step changes an attempt.
func (s *JournalStore) Step(id string, fn func(*Attempt)) error {
	var missing bool
	err := s.Update(func(j *Journal) {
		if a := j.find(id); a != nil {
			fn(a)
		} else {
			missing = true
		}
	})
	if err == nil && missing {
		return fmt.Errorf("no attempt %s in the journal", id)
	}
	return err
}
