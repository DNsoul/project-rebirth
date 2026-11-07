package soak

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one line of an arm's log. The log is the product of the whole
// experiment, so it records everything that happens and leaves the judgement
// to the report.
type Event struct {
	At   time.Time      `json:"t"`
	Arm  string         `json:"arm"`
	Ev   string         `json:"ev"`
	Kind string         `json:"kind,omitempty"`
	OK   *bool          `json:"ok,omitempty"`
	MS   int64          `json:"ms,omitempty"`
	Err  string         `json:"err,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

func boolp(b bool) *bool { return &b }

// eventLog writes events to one JSON-lines file per arm and, for the ones worth
// reading as they happen, a line of text to a console and a common log.
type eventLog struct {
	dir     string
	console io.Writer

	mu    sync.Mutex
	files map[string]*os.File
	text  *os.File
}

func newEventLog(dir string, console io.Writer) (*eventLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	text, err := os.OpenFile(filepath.Join(dir, "soak.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &eventLog{dir: dir, console: console, files: map[string]*os.File{}, text: text}, nil
}

// emit records an event. Quiet events (a probe that worked) go to the arm's
// file only; the rest are also written out as text.
func (l *eventLog) emit(e Event, quiet bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	f, ok := l.files[e.Arm]
	if !ok {
		var err error
		f, err = os.OpenFile(filepath.Join(l.dir, e.Arm+".jsonl"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(l.console, "cannot open the log of %s: %v\n", e.Arm, err)
			return
		}
		l.files[e.Arm] = f
	}
	if line, err := json.Marshal(e); err == nil {
		_, _ = f.Write(append(line, '\n'))
	}
	if quiet {
		return
	}
	line := e.text() + "\n"
	_, _ = io.WriteString(l.text, line)
	_, _ = io.WriteString(l.console, line)
}

func (l *eventLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		_ = f.Close()
	}
	_ = l.text.Close()
}

// text renders an event for a person.
func (e Event) text() string {
	s := fmt.Sprintf("%s  %-12s %s", e.At.Local().Format("15:04:05"), e.Arm, e.Ev)
	if e.Kind != "" {
		s += " " + e.Kind
	}
	if e.OK != nil {
		if *e.OK {
			s += " ok"
		} else {
			s += " FAILED"
		}
	}
	if e.MS != 0 {
		s += fmt.Sprintf(" %dms", e.MS)
	}
	if e.Err != "" {
		s += ": " + e.Err
	}
	for k, v := range e.Data {
		s += fmt.Sprintf(" %s=%v", k, v)
	}
	return s
}
