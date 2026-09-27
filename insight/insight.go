// Package insight is RTS's memory: the conclusions a session reached, kept
// where the next session will find them.
//
// The distinction it exists to hold, and the whole reason it is a separate
// store from the symbol index and the project map:
//
//	A symbol is what the code says. It is re-derived by `rts index`, it is
//	always true of the code as it stands, and it is disposable.
//
//	An insight is what somebody concluded from reading the code. Nothing
//	re-derives it, it can be wrong, and it can go stale the moment the code
//	under it changes.
//
// Every read labels which one it is handing back, so a past session's guess
// can never be quoted as a fact about the codebase. Merging the two stores
// would be easier and would quietly turn RTS from a code lens into a
// confident liar.
//
// Insights link to files by path and to code by symbol name rather than by
// any index identity, because `rts index` prunes and re-mints the symbol
// list on every run. A link that no longer resolves simply drops out of the
// read — the right outcome for a note about something that has left the
// codebase.
package insight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// StoreVersion is bumped when the on-disk shape changes. Unlike the derived
// stores, insights are NOT disposable — they are the one thing in .rts that
// cannot be rebuilt — so a version mismatch is an error the caller must see,
// never a silent discard.
const StoreVersion = 1

const (
	// MaxTitleChars keeps a title to one scannable line in the brief.
	MaxTitleChars = 120

	// MaxBodyChars bounds one finding. Past this it is an essay, and the
	// brief that carries it stops being read.
	MaxBodyChars = 1200

	// MaxInsights is the ceiling on the store. Past it the oldest are
	// dropped on write: a memory with no eviction becomes a context tax
	// that grows forever, and the note nobody has touched in months is the
	// one to lose. Volume is a deliberate design ceiling, not a limit
	// anyone should hit by accident.
	MaxInsights = 60
)

// Insight is one recorded finding.
type Insight struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Paths      []string  `json:"paths,omitempty"`   // files it rests on
	Symbols    []string  `json:"symbols,omitempty"` // symbols it concerns
	RecordedAt time.Time `json:"recorded_at"`
	Session    string    `json:"session,omitempty"`
}

// Store is the on-disk collection, serialized to .rts/insights.json.
type Store struct {
	Version  int       `json:"version"`
	Insights []Insight `json:"insights"`
}

// Load reads .rts/insights.json. A project that has recorded nothing yet is
// not an error — it is the normal state of a fresh install, and every caller
// wants an empty store rather than a failure.
func Load(projectRoot string) (*Store, error) {
	data, err := os.ReadFile(Path(projectRoot))
	if os.IsNotExist(err) {
		return &Store{Version: StoreVersion}, nil
	}
	if err != nil {
		return nil, err
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parsing %s: %w (this file holds recorded findings and is not rebuildable - move it aside rather than deleting it)", Path(projectRoot), err)
	}
	if store.Version != StoreVersion {
		return nil, fmt.Errorf("%s was written by a different rts version (found %d, expected %d)", Path(projectRoot), store.Version, StoreVersion)
	}

	return &store, nil
}

// Path is where the store lives.
func Path(projectRoot string) string {
	return filepath.Join(projectRoot, ".rts", "insights.json")
}

// Record appends a finding and writes the store back, returning the stored
// insight and whether `supersedes` actually removed anything.
//
// `supersedes` removes an earlier finding in the same write — the correction
// path. Without it a memory only ever accumulates, so the first wrong note
// about a file stays there arguing with every later one, and a reader has no
// way to tell which is current.
//
// The bool is why this does not just return the insight: a mistyped id must
// not come back as a confident "superseded and removed [i99]". The caller
// reports what happened, and a correction that silently failed to correct
// anything is the one outcome nobody would think to check for.
//
// Read-modify-write under an exclusive lock file: two Claude Code sessions in
// one repo is an ordinary situation, and a plain read-then-write loses one of
// their findings outright.
func Record(projectRoot string, in Insight, supersedes string) (*Insight, bool, error) {
	unlock, err := lock(projectRoot)
	if err != nil {
		return nil, false, err
	}
	defer unlock()

	store, err := Load(projectRoot)
	if err != nil {
		return nil, false, err
	}

	in.Title = truncate(strings.TrimSpace(in.Title), MaxTitleChars)
	in.Body = truncate(strings.TrimSpace(in.Body), MaxBodyChars)
	in.Paths = clean(in.Paths)
	in.Symbols = clean(in.Symbols)

	if in.Title == "" || in.Body == "" {
		return nil, false, fmt.Errorf("a finding needs both a title and a body")
	}

	if in.RecordedAt.IsZero() {
		in.RecordedAt = time.Now().UTC()
	}

	superseded := false
	kept := store.Insights[:0]
	for _, existing := range store.Insights {
		if supersedes != "" && existing.ID == supersedes {
			superseded = true
			continue
		}
		kept = append(kept, existing)
	}
	store.Insights = kept

	in.ID = nextID(store.Insights)
	store.Insights = append(store.Insights, in)

	// Evict oldest-first once over the ceiling — see MaxInsights.
	if len(store.Insights) > MaxInsights {
		store.Insights = store.Insights[len(store.Insights)-MaxInsights:]
	}

	if err := write(projectRoot, store); err != nil {
		return nil, false, err
	}
	return &in, superseded, nil
}

// Remove deletes one finding by ID. Reports whether it found anything, so a
// caller can tell "removed" from "there was nothing by that name" — rendering
// both as success is how a stale note survives being deleted twice.
func Remove(projectRoot, id string) (bool, error) {
	unlock, err := lock(projectRoot)
	if err != nil {
		return false, err
	}
	defer unlock()

	store, err := Load(projectRoot)
	if err != nil {
		return false, err
	}

	kept := make([]Insight, 0, len(store.Insights))
	for _, existing := range store.Insights {
		if existing.ID == id {
			continue
		}
		kept = append(kept, existing)
	}

	if len(kept) == len(store.Insights) {
		return false, nil
	}

	store.Insights = kept
	return true, write(projectRoot, store)
}

// Live returns the findings whose links still resolve, newest first, with
// dead paths dropped from each.
//
// A finding that named three files and has one left is still worth reading,
// so it survives with the surviving link. A finding whose every named file is
// gone is about code that no longer exists, and it is dropped: it can only
// mislead the session that reads it. A finding that named no file at all is
// about the project rather than about a file, and nothing can invalidate it
// here — those always survive.
func Live(projectRoot string, store *Store) []Insight {
	live := make([]Insight, 0, len(store.Insights))

	for _, in := range store.Insights {
		if len(in.Paths) > 0 {
			var alive []string
			for _, p := range in.Paths {
				if _, err := os.Stat(filepath.Join(projectRoot, p)); err == nil {
					alive = append(alive, p)
				}
			}
			if len(alive) == 0 {
				continue
			}
			in.Paths = alive
		}
		live = append(live, in)
	}

	sort.SliceStable(live, func(i, j int) bool {
		return live[i].RecordedAt.After(live[j].RecordedAt)
	})
	return live
}

// nextID mints the next "i<n>" label. Short because it is quoted back by a
// model correcting a note, and derived from the high-water mark rather than
// from the count so a removal can never make the next ID collide with a
// living one.
func nextID(existing []Insight) string {
	high := 0
	for _, in := range existing {
		var n int
		if _, err := fmt.Sscanf(in.ID, "i%d", &n); err == nil && n > high {
			high = n
		}
	}
	return fmt.Sprintf("i%d", high+1)
}

// write serializes the store through a temp file and a rename, so a crash
// mid-write leaves the previous findings intact rather than a truncated file.
// This store is the one thing under .rts that `rts index` cannot rebuild.
func write(projectRoot string, store *Store) error {
	dir := filepath.Join(projectRoot, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating .rts directory: %w", err)
	}

	store.Version = StoreVersion

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling insights: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "insights-*.json")
	if err != nil {
		return fmt.Errorf("writing insights: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing insights: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing insights: %w", err)
	}

	if err := os.Rename(tmpPath, Path(projectRoot)); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing insights: %w", err)
	}
	return nil
}

// lockTimeout bounds the wait for a concurrent writer. Long enough that a
// normal write is never contended, short enough that a lock left behind by a
// killed process costs one pause rather than a hang.
const lockTimeout = 3 * time.Second

// lock takes an exclusive lock via O_EXCL create, which is atomic on every
// filesystem this runs on. A lock older than the timeout is broken rather
// than waited on: a session killed mid-write would otherwise lock the
// project's memory permanently, and that failure is silent and total.
func lock(projectRoot string) (func(), error) {
	dir := filepath.Join(projectRoot, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating .rts directory: %w", err)
	}

	lockPath := filepath.Join(dir, "insights.lock")
	deadline := time.Now().Add(lockTimeout)

	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("locking insights: %w", err)
		}

		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > lockTimeout {
			_ = os.Remove(lockPath) // stale: the holder died
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another rts process is writing %s; try again", Path(projectRoot))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// truncate caps a string at max BYTES, ellipsis included — the caps are a
// budget on what a brief carries, so overshooting them by the width of the
// marker defeats the point. The cut backs off to a rune boundary so a
// multi-byte character is never split into mojibake.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}

	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// clean normalizes a caller-supplied list: trimmed, de-duplicated, blanks
// dropped, "./" prefixes removed so a path matches what the symbol index and
// the map already say.
func clean(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))

	for _, v := range values {
		v = strings.TrimPrefix(strings.TrimSpace(v), "./")
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}
