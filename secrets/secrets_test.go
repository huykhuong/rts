package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKey has the shape of a Rails master key (32 hex characters) without
// being one.
const fakeKey = "0123456789abcdef0123456789abcdef"

func TestIsProtected_RefusesCredentialFiles(t *testing.T) {
	for _, path := range []string{
		"config/master.key",
		"./config/master.key",
		"/abs/project/config/master.key",
		"config/Master.KEY",
		"config/../config/master.key",
		"config/credentials/production.key",
		"config/credentials.yml.enc",
		"config/credentials/staging.yml.enc",
		".env",
		".env.local",
		".env.production",
		".envrc",
		"certs/server.pem",
		"/Users/someone/.ssh/config",
		"/Users/someone/.ssh",
		"/Users/someone/.aws/credentials",
		"id_ed25519",
	} {
		if !IsProtected(path) {
			t.Errorf("IsProtected(%q) = false, want true", path)
		}
	}
}

func TestIsProtected_AllowsOrdinaryFiles(t *testing.T) {
	for _, path := range []string{
		"",
		"config/application.rb",
		"config/credentials",
		"app/models/user.rb",
		".env.example",
		".env.sample",
		"config/environments/production.rb",
		"lib/keyboard.rb",
		"app/models/api_key.rb",
	} {
		if IsProtected(path) {
			t.Errorf("IsProtected(%q) = true, want false", path)
		}
	}
}

// A link with an ordinary name still opens the key, so it must be refused
// by what it points at.
func TestIsProtected_FollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "master.key")
	if err := os.WriteFile(key, []byte(fakeKey), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "notes.txt")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}

	if !IsProtected(link) {
		t.Errorf("IsProtected(symlink to master.key) = false, want true")
	}
}

func writeKey(t *testing.T, dir, rel, value string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRedactor_ScrubsKeysFromEveryDirectory(t *testing.T) {
	t.Setenv("RAILS_MASTER_KEY", "")
	worktree, mainCheckout := t.TempDir(), t.TempDir()
	envKey := "fedcba9876543210fedcba9876543210"
	writeKey(t, worktree, "config/master.key", fakeKey)
	writeKey(t, mainCheckout, "config/credentials/production.key", envKey)

	r := NewRedactor(worktree, mainCheckout)
	got := r.Redact("a " + fakeKey + " b " + envKey + " c")

	if strings.Contains(got, fakeKey) || strings.Contains(got, envKey) {
		t.Fatalf("Redact left a key in place: %q", got)
	}
	if want := "a " + Redacted + " b " + Redacted + " c"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestRedactor_ScrubsTheEnvironmentKey(t *testing.T) {
	t.Setenv("RAILS_MASTER_KEY", fakeKey)

	got := NewRedactor().Redact("value=" + fakeKey)

	if strings.Contains(got, fakeKey) {
		t.Errorf("Redact left RAILS_MASTER_KEY in place: %q", got)
	}
}

// The MCP server redacts encoded JSON lines, where a value with quotes or
// backslashes appears in its escaped form.
func TestRedactor_ScrubsTheJSONEscapedForm(t *testing.T) {
	t.Setenv("RAILS_MASTER_KEY", "")
	dir := t.TempDir()
	odd := `abc"def\ghi0123456789`
	writeKey(t, dir, "config/master.key", odd)

	got := NewRedactor(dir).Redact(`{"text":"abc\"def\\ghi0123456789"}`)

	if strings.Contains(got, `abc\"def`) {
		t.Errorf("Redact left the escaped key in place: %q", got)
	}
}

// A blank or placeholder key file must not become a pattern that blanks out
// ordinary output.
func TestRedactor_IgnoresShortValues(t *testing.T) {
	t.Setenv("RAILS_MASTER_KEY", "")
	dir := t.TempDir()
	writeKey(t, dir, "config/master.key", "x")

	if got := NewRedactor(dir).Redact("x marks the spot"); got != "x marks the spot" {
		t.Errorf("Redact = %q, want the text unchanged", got)
	}
}

// A key seeded after the redactor was built is still caught, because the
// values are reread on every call.
func TestRedactor_PicksUpAKeySeededLater(t *testing.T) {
	t.Setenv("RAILS_MASTER_KEY", "")
	dir := t.TempDir()
	r := NewRedactor(dir)

	writeKey(t, dir, "config/master.key", fakeKey)

	if got := r.Redact(fakeKey); got != Redacted {
		t.Errorf("Redact = %q, want %q", got, Redacted)
	}
}
