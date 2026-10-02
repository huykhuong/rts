package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretMarker sits in every secret fixture and in one ordinary file, so a
// single search shows both that ordinary results still come back and that
// the secret ones never do.
const secretMarker = "NEEDLE_7f3a"

// secretProject lays out a project holding every kind of file the search
// must keep out, plus one ordinary file and one .env template that it must
// keep in.
func secretProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"app/models/user.rb":                "# " + secretMarker + " ordinary\n",
		"config/master.key":                 secretMarker + "-master\n",
		"config/credentials/production.key": secretMarker + "-production\n",
		"config/credentials.yml.enc":        secretMarker + "-encrypted\n",
		".env.local":                        "TOKEN=" + secretMarker + "-env\n",
		".env.example":                      "TOKEN=" + secretMarker + "-template\n",
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
	return dir
}

func assertNoSecrets(t *testing.T, text string) {
	t.Helper()
	for _, leak := range []string{"-master", "-production", "-encrypted", "-env"} {
		if strings.Contains(text, secretMarker+leak) {
			t.Errorf("search output contains a secret (%s):\n%s", leak, text)
		}
	}
	for _, name := range []string{"master.key", "production.key", "credentials.yml.enc", ".env.local", "notes.txt"} {
		if strings.Contains(text, name) {
			t.Errorf("search output names protected file %s:\n%s", name, text)
		}
	}
}

// Every way through ripgrep found while auditing the leak: a plain search,
// a file_glob naming the key (which overrides .gitignore in rg) in either
// case, and a glob for the whole key family.
func TestRunRipgrep_NeverReturnsSecrets(t *testing.T) {
	rgPath, ok := findRipgrep()
	if !ok {
		t.Skip("ripgrep not installed")
	}
	dir := secretProject(t)

	for _, glob := range []string{"", "master.key", "MASTER.KEY", "*.key", "*.enc", "*", "**/*"} {
		t.Run("glob="+glob, func(t *testing.T) {
			got, err := runRipgrep(rgPath, secretMarker, dir, glob)
			if err != nil {
				t.Fatalf("runRipgrep: %v", err)
			}
			assertNoSecrets(t, got.Text)
		})
	}

	got, _ := runRipgrep(rgPath, secretMarker, dir, "")
	if !strings.Contains(got.Text, "user.rb") {
		t.Errorf("ordinary file missing from results — the guard is over-blocking:\n%s", got.Text)
	}
}

// grep honours no .gitignore and matches --exclude case-sensitively, so on
// this path the line filter is doing all of the work.
func TestRunGrep_NeverReturnsSecrets(t *testing.T) {
	dir := secretProject(t)

	for _, glob := range []string{"", "master.key", "MASTER.KEY", "*.key", "*"} {
		t.Run("glob="+glob, func(t *testing.T) {
			got, err := runGrep(secretMarker, dir, glob)
			if err != nil {
				t.Fatalf("runGrep: %v", err)
			}
			assertNoSecrets(t, got.Text)
		})
	}

	got, _ := runGrep(secretMarker, dir, "")
	if !strings.Contains(got.Text, "user.rb") || !strings.Contains(got.Text, ".env.example") {
		t.Errorf("ordinary files missing from results — the guard is over-blocking:\n%s", got.Text)
	}
}

// Naming the key directly as the search path is refused outright, since
// single-file output carries no file name for the line filter to check.
func TestSearchText_RefusesAProtectedPath(t *testing.T) {
	dir := secretProject(t)

	for _, rel := range []string{"config/master.key", "config/MASTER.KEY", "app/notes.txt", "config/credentials.yml.enc"} {
		got, err := SearchText(secretMarker, filepath.Join(dir, rel), "")
		if err == nil {
			t.Errorf("SearchText(path=%s) succeeded, want a refusal; got:\n%s", rel, got.Text)
			continue
		}
		if !strings.Contains(err.Error(), "Refused") {
			t.Errorf("SearchText(path=%s) error = %v, want a refusal", rel, err)
		}
	}
}

func TestDropProtected_KeepsOrdinaryLines(t *testing.T) {
	raw := "app/models/user.rb:3:has_many :posts\n" +
		"./config/master.key:1:abc\n" +
		"config/credentials.yml.enc:1:ciphertext\n" +
		"lib/tasks/seed.rake:12:  path = 'config/master.key'\n"

	got := dropProtected(raw)

	want := "app/models/user.rb:3:has_many :posts\n" +
		"lib/tasks/seed.rake:12:  path = 'config/master.key'\n"
	if got != want {
		t.Errorf("dropProtected =\n%s\nwant\n%s", got, want)
	}
}

// A search where only protected files matched reports zero matches rather
// than an empty block with a nonzero count.
func TestFormatKept_AllProtectedIsEmpty(t *testing.T) {
	got := formatKept("x", "config/master.key:1:x\n")

	if got.TotalMatches != 0 || got.NaiveChars != 0 {
		t.Errorf("formatKept = %+v, want an empty outcome", got)
	}
}
