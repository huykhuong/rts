package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rts/index"
)

// spawned records every root a test asked spawnIndex to reindex.
var spawned []string

// TestMain replaces spawnIndex for the whole package, so no test can fork a
// real `rts index` by accident — a fixture that happened to write
// symbols.json would otherwise start one.
func TestMain(m *testing.M) {
	spawnIndex = func(root string) error {
		spawned = append(spawned, root)
		return nil
	}
	os.Exit(m.Run())
}

func resetSpawned(t *testing.T) {
	t.Helper()
	spawned = nil
}

// fullyIndexed writes a project and every artifact `rts index` produces.
func fullyIndexed(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeProjectFile(t, root, "Gemfile", "source 'x'\n")
	writeProjectFile(t, root, "config/application.rb", "module App; end\n")
	writeProjectFile(t, root, "app/models/user.rb", "class User\nend\n")

	symbols, err := index.BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if err := index.Write(root, symbols); err != nil {
		t.Fatalf("Write: %v", err)
	}
	fp, err := index.BuildFingerprints(root)
	if err != nil {
		t.Fatalf("BuildFingerprints: %v", err)
	}
	if err := index.WriteFingerprints(root, fp); err != nil {
		t.Fatalf("WriteFingerprints: %v", err)
	}
	pm, err := index.BuildMap(root, symbols)
	if err != nil {
		t.Fatalf("BuildMap: %v", err)
	}
	if err := index.WriteMap(root, pm); err != nil {
		t.Fatalf("WriteMap: %v", err)
	}
	return root
}

// editLater changes a file's content and pushes its mtime past the index, as
// real work does.
func editLater(t *testing.T, root, rel, content string) {
	t.Helper()

	writeProjectFile(t, root, rel, content)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, rel), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestRefreshIfStale_NeverTouchesAnUnindexedProject(t *testing.T) {
	// Opening Claude Code in some other repo must not leave a .rts directory
	// behind in it. Running `rts index` once is how a project opts in.
	resetSpawned(t)
	root := t.TempDir()
	writeProjectFile(t, root, "app/models/user.rb", "class User\nend\n")

	if _, refreshing := refreshIfStale(root); refreshing {
		t.Error("started a reindex in a project that was never indexed")
	}
	if len(spawned) != 0 {
		t.Errorf("spawned %v", spawned)
	}
	if _, err := os.Stat(filepath.Join(root, ".rts")); !os.IsNotExist(err) {
		t.Error("left a .rts directory in an unindexed project")
	}
}

func TestRefreshIfStale_FreshIndexIsLeftAlone(t *testing.T) {
	resetSpawned(t)
	root := fullyIndexed(t)

	drift, refreshing := refreshIfStale(root)
	if refreshing || len(spawned) != 0 {
		t.Errorf("reindexed a fresh index (spawned %v)", spawned)
	}
	if drift == nil || drift.Stale() {
		t.Errorf("drift = %+v, want a checked, non-stale result", drift)
	}
}

func TestRefreshIfStale_DriftStartsABackgroundReindex(t *testing.T) {
	resetSpawned(t)
	root := fullyIndexed(t)
	editLater(t, root, "app/models/user.rb", "class User\n  def added; end\nend\n")

	drift, refreshing := refreshIfStale(root)
	if !refreshing {
		t.Fatal("drift did not start a reindex")
	}
	if len(spawned) != 1 || spawned[0] != root {
		t.Errorf("spawned %v, want exactly [%s]", spawned, root)
	}
	if drift == nil || drift.Changed != 1 {
		t.Errorf("drift = %+v, want the one changed file reported", drift)
	}
}

func TestRefreshIfStale_AGitRestampIsNotDrift(t *testing.T) {
	// Every rebase restamps the whole tree. Reindexing on that would mean
	// reindexing after every git operation, for no change at all.
	resetSpawned(t)
	root := fullyIndexed(t)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "app/models/user.rb"), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, refreshing := refreshIfStale(root); refreshing {
		t.Error("a restamp with identical content started a reindex")
	}
}

func TestRefreshIfStale_IndexWithoutFingerprintsIsRebuilt(t *testing.T) {
	// An index written before fingerprints existed cannot be checked. A
	// rebuild is what gives it fingerprints, so it is refreshed rather than
	// left unverifiable forever.
	resetSpawned(t)
	root := fullyIndexed(t)
	if err := os.Remove(filepath.Join(root, ".rts", "fingerprints.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	drift, refreshing := refreshIfStale(root)
	if !refreshing {
		t.Error("an unverifiable index was not rebuilt")
	}
	if drift != nil {
		t.Errorf("drift = %+v, want nil when it could not be checked", drift)
	}
}

func TestRefreshIfStale_ASpawnFailureIsNotReportedAsRefreshing(t *testing.T) {
	// The brief tells the reader a refresh is under way. That must only be
	// said when one actually started.
	root := fullyIndexed(t)
	editLater(t, root, "app/models/user.rb", "class User\n  def added; end\nend\n")

	saved := spawnIndex
	spawnIndex = func(string) error { return errors.New("no executable") }
	defer func() { spawnIndex = saved }()

	if _, refreshing := refreshIfStale(root); refreshing {
		t.Error("reported refreshing although the reindex failed to start")
	}
}

func TestSessionContext_BriefSaysTheRefreshIsUnderWay(t *testing.T) {
	resetSpawned(t)
	root := fullyIndexed(t)
	editLater(t, root, "app/models/user.rb", "class User\n  def added; end\nend\n")

	got := SessionContext(PreToolUseInput{HookEventName: "SessionStart", Source: "startup", Cwd: root}, time.Now())

	if !strings.Contains(got, "Refreshing it in the background now") {
		t.Errorf("brief does not say a refresh started:\n%s", got)
	}
	if strings.Contains(got, "Run `rts index` to refresh") {
		t.Errorf("brief tells the reader to do what is already happening:\n%s", got)
	}
}

func TestSessionContext_ResumeRefreshesButStaysSilent(t *testing.T) {
	// A resumed session skips the brief, which its transcript already
	// carries, but it still calls find_symbol against the index.
	resetSpawned(t)
	root := fullyIndexed(t)
	editLater(t, root, "app/models/user.rb", "class User\n  def added; end\nend\n")

	got := SessionContext(PreToolUseInput{HookEventName: "SessionStart", Source: "resume", Cwd: root}, time.Now())

	if got != "" {
		t.Errorf("resume produced a brief:\n%s", got)
	}
	if len(spawned) != 1 {
		t.Errorf("resume did not refresh the index (spawned %v)", spawned)
	}
}

func TestSessionContext_KillSwitchStartsNothing(t *testing.T) {
	resetSpawned(t)
	root := fullyIndexed(t)
	editLater(t, root, "app/models/user.rb", "class User\n  def added; end\nend\n")
	t.Setenv("RTS_DISABLE", "1")

	SessionContext(PreToolUseInput{HookEventName: "SessionStart", Source: "startup", Cwd: root}, time.Now())

	if len(spawned) != 0 {
		t.Errorf("RTS_DISABLE=1 still started a reindex (spawned %v)", spawned)
	}
}
