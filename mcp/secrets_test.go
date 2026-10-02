package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rts/insight"
	"rts/secrets"
)

// fakeMasterKey has the shape of a Rails master key without being one. No
// test here touches a real key.
const fakeMasterKey = "0123456789abcdef0123456789abcdef"

// keyedProject builds a project holding a master key, an innocently named
// copy of it, and an ordinary Ruby file, and points the server's redactor
// at it. HOME is moved too, so the event log these calls write is one the
// test can read back.
func keyedProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RAILS_MASTER_KEY", "")

	files := map[string]string{
		"config/master.key":  fakeMasterKey + "\n",
		"tmp/copied_key.txt": "pasted here: " + fakeMasterKey + "\n",
		"app/models/user.rb": "class User\n  KEY_HINT = \"" + fakeMasterKey + "\"\nend\n",
	}
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "config/master.key"), filepath.Join(dir, "app/notes.txt")); err != nil {
		t.Fatal(err)
	}

	previous := redactor
	redactor = secrets.NewRedactor(dir)
	t.Cleanup(func() { redactor = previous })
	return dir
}

func callTool(t *testing.T, root, name string, args map[string]interface{}) string {
	t.Helper()
	return captureStdout(t, func() {
		handleToolsCall(
			toolCall(t, name, args),
			json.RawMessage("1"),
			root,
			"test-session",
			NewFileCache(),
			NewIndexCache(root),
			NewEventLogger("test-session", root),
		)
	})
}

func TestPathTools_RefuseProtectedFiles(t *testing.T) {
	root := keyedProject(t)

	for _, name := range []string{"view_file", "view_bionic_skeleton", "view_bionic_method"} {
		for _, rel := range []string{"config/master.key", "config/MASTER.KEY", "app/notes.txt"} {
			out := callTool(t, root, name, map[string]interface{}{
				"path":         filepath.Join(root, rel),
				"method_names": []string{"anything"},
			})
			if !strings.Contains(out, "Refused") {
				t.Errorf("%s(%s) was not refused:\n%s", name, rel, out)
			}
			if strings.Contains(out, fakeMasterKey) {
				t.Errorf("%s(%s) returned the key:\n%s", name, rel, out)
			}
		}
	}
}

// The name rules cannot know that tmp/copied_key.txt holds the key; the
// redactor catches it by value, in every tool that can return file text.
func TestEveryTool_NeverReturnsTheKeyValue(t *testing.T) {
	root := keyedProject(t)

	calls := []struct {
		name string
		args map[string]interface{}
	}{
		{"view_file", map[string]interface{}{"path": filepath.Join(root, "tmp/copied_key.txt")}},
		{"view_file", map[string]interface{}{"path": filepath.Join(root, "app/models/user.rb")}},
		{"view_bionic_skeleton", map[string]interface{}{"path": filepath.Join(root, "app/models/user.rb")}},
		{"search_text", map[string]interface{}{"pattern": "0123456789", "path": root}},
		{"search_text", map[string]interface{}{"pattern": "pasted", "path": root, "file_glob": "*.txt"}},
		{"search_text", map[string]interface{}{"pattern": fakeMasterKey, "path": root}},
		{"list_directory", map[string]interface{}{"path": root, "depth": 3}},
		{"view_file", map[string]interface{}{"path": filepath.Join(root, fakeMasterKey)}},
	}

	for _, c := range calls {
		out := callTool(t, root, c.name, c.args)
		if strings.Contains(out, fakeMasterKey) {
			t.Errorf("%s %v returned the key:\n%s", c.name, c.args, out)
		}
	}

	// The copy is still readable — only the value is gone — so the guard is
	// not simply refusing everything.
	out := callTool(t, root, "view_file", map[string]interface{}{"path": filepath.Join(root, "tmp/copied_key.txt")})
	if !strings.Contains(out, "pasted here: "+secrets.Redacted) {
		t.Errorf("view_file on the copy should return it with the value redacted:\n%s", out)
	}
}

func TestSearchText_RefusesAPatternHoldingTheKey(t *testing.T) {
	root := keyedProject(t)

	out := callTool(t, root, "search_text", map[string]interface{}{"pattern": "x" + fakeMasterKey, "path": root})

	if !strings.Contains(out, "Refused") {
		t.Errorf("a pattern holding the key should be refused:\n%s", out)
	}
}

// A finding is replayed into every later session's brief, so a key pasted
// into one must never reach the store on disk.
func TestRecordInsight_NeverStoresTheKey(t *testing.T) {
	root := keyedProject(t)

	callTool(t, root, "record_insight", map[string]interface{}{
		"title": "Master key is " + fakeMasterKey,
		"body":  "Seeded from " + fakeMasterKey,
	})

	store, err := insight.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(store.Insights) != 1 {
		t.Fatalf("store holds %d findings, want 1", len(store.Insights))
	}
	stored := store.Insights[0]
	if strings.Contains(stored.Title+stored.Body, fakeMasterKey) {
		t.Errorf("the stored finding holds the key: %+v", stored)
	}

	if out := callTool(t, root, "project_map", map[string]interface{}{}); strings.Contains(out, fakeMasterKey) {
		t.Errorf("project_map replayed the key:\n%s", out)
	}
}

func TestEventLog_NeverRecordsTheKey(t *testing.T) {
	root := keyedProject(t)

	callTool(t, root, "view_file", map[string]interface{}{"path": filepath.Join(root, "tmp/copied_key.txt")})
	callTool(t, root, "search_text", map[string]interface{}{"pattern": fakeMasterKey, "path": root})

	home, _ := os.UserHomeDir()
	log, err := os.ReadFile(filepath.Join(home, ".rts", "events.jsonl"))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	if strings.Contains(string(log), fakeMasterKey) {
		t.Errorf("the event log holds the key:\n%s", log)
	}
}
