package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// indexed writes a project, fingerprints it, and returns the root.
func indexed(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for rel, content := range files {
		writeFile(t, root, rel, content)
	}

	fp, err := BuildFingerprints(root)
	if err != nil {
		t.Fatalf("BuildFingerprints: %v", err)
	}
	if err := WriteFingerprints(root, fp); err != nil {
		t.Fatalf("WriteFingerprints: %v", err)
	}
	return root
}

// restamp moves a file's mtime into the future without touching its bytes —
// what a rebase, checkout or stash pop does to the whole working tree.
func restamp(t *testing.T, root, rel string) {
	t.Helper()

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, rel), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestCheckDrift_CleanProjectHasNoDrift(t *testing.T) {
	root := indexed(t, map[string]string{
		"app/models/user.rb":    "class User\nend\n",
		"app/models/invoice.rb": "class Invoice\nend\n",
	})

	drift, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift.Stale() {
		t.Errorf("clean project reported drift: %+v", drift)
	}
}

func TestCheckDrift_RestampedButIdenticalFilesAreNotDrift(t *testing.T) {
	// The regression this whole file exists for. Measured on a real Rails
	// repo an hour after a rebase, the mtime check reported 493 changed
	// files — 492 of them byte-identical to HEAD. A warning that fires that
	// loudly on a day nothing changed is one nobody reads on the day
	// something does.
	root := indexed(t, map[string]string{
		"app/models/user.rb":    "class User\nend\n",
		"app/models/invoice.rb": "class Invoice\nend\n",
		"app/models/payment.rb": "class Payment\nend\n",
	})

	restamp(t, root, "app/models/user.rb")
	restamp(t, root, "app/models/invoice.rb")
	restamp(t, root, "app/models/payment.rb")

	drift, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}

	if drift.Stale() {
		t.Errorf("a git restamp was reported as drift: %+v", drift)
	}
	if drift.Touched != 3 {
		t.Errorf("Touched = %d, want 3 — the restamp should be seen and dismissed", drift.Touched)
	}
}

func TestCheckDrift_RealEditIsDrift(t *testing.T) {
	root := indexed(t, map[string]string{"app/models/user.rb": "class User\nend\n"})

	writeFile(t, root, "app/models/user.rb", "class User\n  def name; end\nend\n")
	restamp(t, root, "app/models/user.rb")

	drift, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift.Changed != 1 {
		t.Errorf("Changed = %d, want 1", drift.Changed)
	}
	if !drift.Stale() {
		t.Error("a real edit should be reported as drift")
	}
}

func TestCheckDrift_SeesAdditionsAndDeletions(t *testing.T) {
	// Deletions were structurally invisible to the mtime check: a file that
	// is gone has no mtime to be late. They are exactly what leaves
	// find_symbol pointing at files that no longer exist.
	root := indexed(t, map[string]string{
		"app/models/user.rb":  "class User\nend\n",
		"app/models/ghost.rb": "class Ghost\nend\n",
	})

	if err := os.Remove(filepath.Join(root, "app/models/ghost.rb")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeFile(t, root, "app/models/fresh.rb", "class Fresh\nend\n")

	drift, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift.Removed != 1 {
		t.Errorf("Removed = %d, want 1", drift.Removed)
	}
	if drift.Added != 1 {
		t.Errorf("Added = %d, want 1", drift.Added)
	}
}

func TestCheckDrift_IgnoresWhatTheIndexIgnores(t *testing.T) {
	root := indexed(t, map[string]string{"app/models/user.rb": "class User\nend\n"})

	// Neither of these belongs to the symbol index, so neither is drift.
	writeFile(t, root, "db/migrate/20260101_add_thing.rb", "class AddThing; end\n")
	writeFile(t, root, "README.md", "# not ruby\n")
	writeFile(t, root, "node_modules/pkg/thing.rb", "class Vendored; end\n")

	drift, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift.Stale() {
		t.Errorf("files outside the index were counted as drift: %+v", drift)
	}
}

func TestCheckDrift_WithoutFingerprintsIsAnError(t *testing.T) {
	// An index written by an older rts has no fingerprints beside it. That
	// must surface as an error the caller reports, never as a confident
	// "nothing changed" — and never as a fallback to the mtime check this
	// replaced, which would reintroduce the false positives.
	if _, err := CheckDrift(t.TempDir()); err == nil {
		t.Fatal("expected an error when no fingerprints exist")
	}
}

func TestDriftDescribe(t *testing.T) {
	tests := []struct {
		drift Drift
		want  string
	}{
		{Drift{}, "nothing changed"},
		{Drift{Changed: 3}, "3 changed"},
		{Drift{Changed: 3, Added: 1}, "3 changed and 1 added"},
		{Drift{Changed: 3, Added: 1, Removed: 2}, "3 changed, 1 added and 2 deleted"},
		{Drift{Removed: 5}, "5 deleted"},
		// Touched is noise, never part of the sentence.
		{Drift{Touched: 492}, "nothing changed"},
	}

	for _, tc := range tests {
		if got := tc.drift.Describe(); got != tc.want {
			t.Errorf("Describe(%+v) = %q, want %q", tc.drift, got, tc.want)
		}
	}
}

func TestShouldIndex(t *testing.T) {
	// One predicate, shared by the indexer, the map builder and this check.
	// They disagreed once, and the two stores described different codebases.
	tests := []struct {
		path string
		want bool
	}{
		{"app/models/user.rb", true},
		{"lib/tasks/thing.rb", true},
		{"README.md", false},
		{"app/assets/x.js", false},
		{filepath.Join("db", "migrate", "20260101_x.rb"), false},
	}

	for _, tc := range tests {
		if got := shouldIndex(tc.path); got != tc.want {
			t.Errorf("shouldIndex(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestFingerprint_DetectsContentChange(t *testing.T) {
	a := fingerprint([]byte("class User\nend\n"))
	b := fingerprint([]byte("class User\n  def name; end\nend\n"))

	if a == b {
		t.Error("different content produced the same fingerprint")
	}
	if a != fingerprint([]byte("class User\nend\n")) {
		t.Error("identical content produced different fingerprints")
	}
}
