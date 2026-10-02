// Package secrets keeps credential files out of everything RTS returns.
//
// It works in two layers, because neither one is enough on its own:
//
//   - IsProtected refuses a path before anything reads it. It recognises a
//     credentials file by name, whatever its case or a symlink pointing at it
//     is called, but it cannot recognise a copy saved under an ordinary name.
//   - Redactor removes the master key's actual value from text crossing the
//     MCP boundary, so a copy, a hard link or a pasted value is caught by its
//     content instead of its name.
//
// Both layers fail closed: a path that might be a secret is refused, and a
// line that might name one is dropped.
package secrets

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// protectedExtensions are file types that hold key material. Rails' master
// key and per-environment keys are *.key, and *.enc covers the encrypted
// credentials files themselves (credentials.yml.enc and the per-environment
// config/credentials/*.yml.enc).
var protectedExtensions = map[string]bool{
	".key": true,
	".enc": true,
	".pem": true,
	".p12": true,
	".pfx": true,
}

// protectedNames are well-known secret files without a telling extension.
var protectedNames = map[string]bool{
	".env":       true,
	".envrc":     true,
	".netrc":     true,
	"id_rsa":     true,
	"id_dsa":     true,
	"id_ecdsa":   true,
	"id_ed25519": true,
}

// protectedDirs are directories where every file is treated as a secret.
var protectedDirs = map[string]bool{
	".ssh":   true,
	".gnupg": true,
	".aws":   true,
}

// envTemplates are the committed .env.* files that hold placeholders, not
// values. Every other .env.* (.env.local, .env.production, ...) is refused.
var envTemplates = map[string]bool{
	".env.example":  true,
	".env.sample":   true,
	".env.template": true,
	".env.dist":     true,
}

// SearchExcludes are glob patterns a text search should never descend into.
// They are an early filter only: the search output is still checked line by
// line with IsProtected, which is what makes the exclusion a guarantee.
// .env.* is deliberately absent — a glob cannot exclude .env.local without
// also excluding .env.example, so that case is left to the line check.
var SearchExcludes = []string{
	"*.key", "*.enc", "*.pem", "*.p12", "*.pfx",
	".env", ".envrc", ".netrc",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	".ssh", ".gnupg", ".aws",
}

// IsProtected reports whether path names a file RTS must never return. The
// path is checked as given and again after resolving symlinks, so an
// innocently named link to config/master.key is refused too.
func IsProtected(path string) bool {
	if path == "" {
		return false
	}
	if matches(path) {
		return true
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && matches(resolved) {
		return true
	}
	return false
}

// matches applies the name rules. Comparison is case-insensitive because
// macOS file systems are: config/Master.KEY opens the same file.
func matches(path string) bool {
	parts := strings.Split(filepath.ToSlash(strings.ToLower(filepath.Clean(path))), "/")
	base := parts[len(parts)-1]

	for _, dir := range parts[:len(parts)-1] {
		if protectedDirs[dir] {
			return true
		}
	}

	if protectedNames[base] || protectedDirs[base] {
		return true
	}
	if strings.HasPrefix(base, ".env.") && !envTemplates[base] {
		return true
	}
	return protectedExtensions[filepath.Ext(base)]
}

// Refusal is the error text for a protected path. It names the rule rather
// than suggesting another way in.
func Refusal(path string) string {
	return fmt.Sprintf("Refused: %s is a protected secrets file (keys, encrypted credentials, .env files). "+
		"RTS never returns its contents.", path)
}

// keyGlobs are the files, relative to a project directory, whose values the
// Redactor scrubs. They are all single-line keys, which is what makes
// matching on the literal value reliable.
var keyGlobs = []string{
	"config/master.key",
	"config/credentials/*.key",
}

// minSecretLen keeps a near-empty or placeholder key file from turning into
// a pattern that redacts ordinary text. Rails keys are 32 hex characters.
const minSecretLen = 16

// Redactor scrubs known key values out of text. It reloads the values on
// every call, so a key seeded into a worktree after the server started is
// still caught.
type Redactor struct {
	dirs []string
}

// NewRedactor returns a Redactor reading key files under each of dirs.
func NewRedactor(dirs ...string) Redactor {
	return Redactor{dirs: dirs}
}

// Values returns the secret values currently on disk under the Redactor's
// directories, plus RAILS_MASTER_KEY from the environment.
func (r Redactor) Values() []string {
	seen := map[string]bool{}
	var values []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen || seen[v] {
			return
		}
		seen[v] = true
		values = append(values, v)
	}

	add(os.Getenv("RAILS_MASTER_KEY"))
	for _, dir := range r.dirs {
		if dir == "" {
			continue
		}
		for _, pattern := range keyGlobs {
			files, _ := filepath.Glob(filepath.Join(dir, pattern))
			for _, f := range files {
				if data, err := os.ReadFile(f); err == nil {
					add(string(data))
				}
			}
		}
	}
	return values
}

// Redacted replaces each secret value in the output.
const Redacted = "[redacted secret]"

// Redact replaces every known secret value in s. Both the raw value and its
// JSON-escaped form are replaced, so the result is safe whether s is plain
// text or an already-encoded JSON-RPC line.
func (r Redactor) Redact(s string) string {
	for _, v := range r.Values() {
		s = strings.ReplaceAll(s, v, Redacted)
		if encoded, err := json.Marshal(v); err == nil {
			escaped := string(encoded[1 : len(encoded)-1])
			if escaped != v {
				s = strings.ReplaceAll(s, escaped, Redacted)
			}
		}
	}
	return s
}
