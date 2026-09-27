package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event type describes one RTS tool invocation.
type Event struct {
	Time    string `json:"time"` // RFC3339 timestamp
	Session string `json:"session,omitempty"`

	// Root is the absolute project root the call was made in. The log is a
	// single global file, and a relative path is not unique across projects:
	// two Rails apps both hold "config/application.rb". Without this, any
	// per-project reading of the log silently attributes one repo's reads to
	// another. See index.rankHot, which is that reading.
	Root          string `json:"root,omitempty"`
	Tool          string `json:"tool"`                  // "view_bionic_skeleton" etc.
	Path          string `json:"path,omitempty"`        // file the tool acted on
	Name          string `json:"name,omitempty"`        // for find_symbol
	Matches       int    `json:"matches,omitempty"`     // for find_symbol
	ResponseChars int    `json:"response_chars"`        // size of what we returned
	NaiveChars    int    `json:"naive_chars,omitempty"` // size of the full-file alternative
	SavedChars    int    `json:"saved_chars,omitempty"` // NaiveChars - ResponseChars, when applicable
}

// EventLogger writes JSONL events to ~/.rts/events.jsonl. It's safe for
// concurrent use, and it fails silently — a broken log file must never
// break the tool call it was trying to record.
type EventLogger struct {
	mu       sync.Mutex
	path     string
	disabled bool
	session  string
	root     string
}

// NewEventLogger creates a logger writing to ~/.rts/events.jsonl.
// If RTS_LOG=0 is set in the environment, the logger is disabled and
// Log() becomes a no-op. If the home directory can't be determined,
// the logger is also disabled (fail-safe).
// NewEventLogger creates a logger for one session in one project. `root` is
// stamped on every event so the log can later be read back per-project — see
// Event.Root. An empty root is allowed and simply leaves the field off; those
// events are invisible to any per-project reading, which is the safe failure.
func NewEventLogger(session, root string) *EventLogger {
	if os.Getenv("RTS_LOG") == "0" {
		return &EventLogger{disabled: true, session: session, root: root}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return &EventLogger{disabled: true, session: session, root: root}
	}

	return &EventLogger{
		path:    filepath.Join(home, ".rts", "events.jsonl"),
		session: session,
		root:    root,
	}
}

func (l *EventLogger) Log(ev Event) {
	if l.disabled {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if ev.Time == "" {
		ev.Time = time.Now().UTC().Format(time.RFC3339)
	}

	if ev.Session == "" {
		ev.Session = l.session
	}

	if ev.Root == "" {
		ev.Root = l.root
	}

	if ev.NaiveChars > 0 {
		ev.SavedChars = ev.NaiveChars - ev.ResponseChars
	}

	// Ensure the directory of the log exists
	_ = os.MkdirAll(filepath.Dir(l.path), 0755)

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	line, err := json.Marshal(ev)
	if err != nil {
		return
	}

	_, _ = f.Write(append(line, '\n'))
}
