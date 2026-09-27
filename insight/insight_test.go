package insight

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// record is the happy-path helper: most tests care about what ends up in the
// store, not about the error return of each individual write.
func record(t *testing.T, root, title, body string, paths ...string) *Insight {
	t.Helper()

	in, _, err := Record(root, Insight{Title: title, Body: body, Paths: paths}, "")
	if err != nil {
		t.Fatalf("Record(%q): %v", title, err)
	}
	return in
}

func TestRecord_PersistsAndAssignsSequentialIDs(t *testing.T) {
	root := t.TempDir()

	first := record(t, root, "First finding", "Body of the first.")
	second := record(t, root, "Second finding", "Body of the second.")

	if first.ID != "i1" || second.ID != "i2" {
		t.Fatalf("ids = %q, %q; want i1, i2", first.ID, second.ID)
	}

	// Reload from disk rather than trusting the return value: the whole point
	// of the store is that it survives the process that wrote it.
	store, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(store.Insights) != 2 {
		t.Fatalf("store holds %d findings, want 2", len(store.Insights))
	}
	if store.Insights[0].Title != "First finding" {
		t.Errorf("first title = %q", store.Insights[0].Title)
	}
	if store.Insights[0].RecordedAt.IsZero() {
		t.Error("RecordedAt was not stamped")
	}
}

func TestLoad_MissingStoreIsEmptyNotAnError(t *testing.T) {
	// A project that has recorded nothing is the normal state of a fresh
	// install, and every caller wants an empty store rather than a failure.
	store, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load on an empty project: %v", err)
	}
	if len(store.Insights) != 0 {
		t.Errorf("empty project reported %d findings", len(store.Insights))
	}
}

func TestRecord_SupersedesRemovesTheOldFinding(t *testing.T) {
	root := t.TempDir()

	wrong := record(t, root, "Auth lives in app/auth", "Wrong, as it turns out.")
	right, superseded, err := Record(root, Insight{
		Title: "Auth lives in app/services/session",
		Body:  "Corrected.",
	}, wrong.ID)
	if err != nil {
		t.Fatalf("Record with supersedes: %v", err)
	}
	if !superseded {
		t.Error("Record did not report that it removed the superseded finding")
	}

	store, _ := Load(root)
	if len(store.Insights) != 1 {
		t.Fatalf("store holds %d findings after supersede, want 1", len(store.Insights))
	}
	if store.Insights[0].ID != right.ID {
		t.Errorf("survivor is %q, want %q", store.Insights[0].ID, right.ID)
	}
}

func TestNextID_DoesNotCollideAfterRemoval(t *testing.T) {
	// Deriving the next id from the count instead of the high-water mark is
	// how a removal makes the next write reuse a living id.
	root := t.TempDir()

	record(t, root, "One", "b")
	second := record(t, root, "Two", "b")
	record(t, root, "Three", "b")

	if _, err := Remove(root, second.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	fourth := record(t, root, "Four", "b")
	if fourth.ID != "i4" {
		t.Errorf("id after removal = %q, want i4", fourth.ID)
	}
}

func TestRecord_SupersedingAnUnknownIDReportsFalse(t *testing.T) {
	// A mistyped id must not come back as a confident "superseded and
	// removed": the caller would confirm a correction that never happened,
	// leaving two contradictory notes and nobody looking for the second.
	root := t.TempDir()
	record(t, root, "Existing", "b")

	in, superseded, err := Record(root, Insight{Title: "New", Body: "b"}, "i99")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if superseded {
		t.Error("Record claimed to supersede an id that does not exist")
	}

	store, _ := Load(root)
	if len(store.Insights) != 2 {
		t.Errorf("store holds %d findings, want both kept", len(store.Insights))
	}
	if in.ID == "" {
		t.Error("the finding should still have been recorded")
	}
}

func TestRecord_RejectsBlankTitleOrBody(t *testing.T) {
	root := t.TempDir()

	for _, tc := range []struct{ name, title, body string }{
		{"blank title", "   ", "a body"},
		{"blank body", "a title", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Record(root, Insight{Title: tc.title, Body: tc.body}, ""); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestRecord_TruncatesOverlongFields(t *testing.T) {
	root := t.TempDir()

	long := make([]byte, MaxBodyChars+500)
	for i := range long {
		long[i] = 'x'
	}

	in := record(t, root, string(long[:MaxTitleChars+50]), string(long))
	if len(in.Title) > MaxTitleChars {
		t.Errorf("title is %d chars, cap is %d", len(in.Title), MaxTitleChars)
	}
	if len(in.Body) > MaxBodyChars {
		t.Errorf("body is %d chars, cap is %d", len(in.Body), MaxBodyChars)
	}
}

func TestRecord_NormalizesLinks(t *testing.T) {
	root := t.TempDir()

	in, _, err := Record(root, Insight{
		Title:   "t",
		Body:    "b",
		Paths:   []string{"./app/models/user.rb", "app/models/user.rb", "  ", "lib/x.rb"},
		Symbols: []string{" User ", "User"},
	}, "")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	want := []string{"app/models/user.rb", "lib/x.rb"}
	if len(in.Paths) != len(want) {
		t.Fatalf("paths = %v, want %v", in.Paths, want)
	}
	for i, p := range want {
		if in.Paths[i] != p {
			t.Errorf("paths[%d] = %q, want %q", i, in.Paths[i], p)
		}
	}
	if len(in.Symbols) != 1 || in.Symbols[0] != "User" {
		t.Errorf("symbols = %v, want [User]", in.Symbols)
	}
}

func TestRecord_EvictsOldestPastTheCeiling(t *testing.T) {
	root := t.TempDir()

	for i := 0; i < MaxInsights+5; i++ {
		record(t, root, "finding", "body")
	}

	store, _ := Load(root)
	if len(store.Insights) != MaxInsights {
		t.Fatalf("store holds %d findings, want the %d ceiling", len(store.Insights), MaxInsights)
	}
	// The survivors must be the newest ones: eviction that dropped the tail
	// would keep the notes nobody has touched and lose today's.
	if store.Insights[0].ID != "i6" {
		t.Errorf("oldest survivor is %q, want i6", store.Insights[0].ID)
	}
}

func TestLive_DropsFindingsWhoseFilesAreAllGone(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "app/models/user.rb")

	record(t, root, "About a live file", "b", "app/models/user.rb")
	record(t, root, "About a deleted file", "b", "app/models/ghost.rb")
	record(t, root, "About the project", "b") // no links at all

	live := Live(root, mustLoad(t, root))

	titles := map[string]bool{}
	for _, in := range live {
		titles[in.Title] = true
	}

	if titles["About a deleted file"] {
		t.Error("a finding whose every file is gone should be dropped")
	}
	if !titles["About a live file"] {
		t.Error("a finding about a file that still exists should survive")
	}
	if !titles["About the project"] {
		t.Error("a finding that names no file has nothing to invalidate it and must survive")
	}
}

func TestLive_KeepsPartiallyResolvingFindingsAndDropsTheDeadLink(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "app/models/user.rb")

	record(t, root, "Spans two files", "b", "app/models/user.rb", "app/models/ghost.rb")

	live := Live(root, mustLoad(t, root))
	if len(live) != 1 {
		t.Fatalf("got %d live findings, want 1", len(live))
	}
	if len(live[0].Paths) != 1 || live[0].Paths[0] != "app/models/user.rb" {
		t.Errorf("paths = %v, want only the surviving file", live[0].Paths)
	}
}

func TestLive_NewestFirst(t *testing.T) {
	root := t.TempDir()

	older := Insight{Title: "older", Body: "b", RecordedAt: time.Now().Add(-time.Hour)}
	newer := Insight{Title: "newer", Body: "b", RecordedAt: time.Now()}
	if _, _, err := Record(root, older, ""); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, _, err := Record(root, newer, ""); err != nil {
		t.Fatalf("Record: %v", err)
	}

	live := Live(root, mustLoad(t, root))
	if live[0].Title != "newer" {
		t.Errorf("first is %q, want the newest", live[0].Title)
	}
}

func TestRemove_ReportsWhetherItFoundAnything(t *testing.T) {
	root := t.TempDir()
	in := record(t, root, "t", "b")

	removed, err := Remove(root, in.ID)
	if err != nil || !removed {
		t.Fatalf("Remove(%q) = %v, %v; want true, nil", in.ID, removed, err)
	}

	// Removing it again must report "nothing there" rather than success —
	// otherwise a stale note that survived deletion looks deleted.
	removed, err = Remove(root, in.ID)
	if err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if removed {
		t.Error("removing a missing id reported success")
	}
}

func TestRecord_ConcurrentWritersDoNotLoseFindings(t *testing.T) {
	// Two Claude Code sessions in one repo is ordinary. A plain
	// read-then-write loses one of their findings outright.
	root := t.TempDir()

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, _, err := Record(root, Insight{Title: "concurrent", Body: "b"}, ""); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Record: %v", err)
	}

	store, _ := Load(root)
	if len(store.Insights) != writers {
		t.Fatalf("store holds %d findings, want all %d", len(store.Insights), writers)
	}

	// Every id must be distinct, or a later supersede would remove two notes.
	seen := map[string]bool{}
	for _, in := range store.Insights {
		if seen[in.ID] {
			t.Fatalf("duplicate id %q", in.ID)
		}
		seen[in.ID] = true
	}
}

func TestRecord_LeavesNoLockBehind(t *testing.T) {
	root := t.TempDir()
	record(t, root, "t", "b")

	if _, err := os.Stat(filepath.Join(root, ".rts", "insights.lock")); !os.IsNotExist(err) {
		t.Error("lock file survived a successful write")
	}
}

func TestLoad_VersionMismatchIsAnError(t *testing.T) {
	// The derived stores are discarded on a version bump. This one cannot be:
	// nothing can rebuild it, so the caller has to be told.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".rts"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(Path(root), []byte(`{"version":99,"insights":[]}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Load(root); err == nil {
		t.Fatal("expected an error for a future store version")
	}
}

func mustLoad(t *testing.T, root string) *Store {
	t.Helper()

	store, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return store
}

func touch(t *testing.T, root, rel string) {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("# file\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
