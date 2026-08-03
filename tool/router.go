package tool

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type fileKind int

const (
	kindRuby	fileKind = iota
	kindPassthrough
	kindUnknown
)

// These are files without a .rb extension that are still Ruby sources.
var rubyBasenames = map[string]bool{
	"Rakefile": true,
	"Gemfile": true,
}

var knownTextExtensions = map[string]bool{
	// Markdown & plain text
	".md": true, ".txt": true,
	// Web / views
	".html": true, ".erb": true, ".css": true, ".scss": true,
	".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	// Config & data
	".yml": true, ".yaml": true, ".json": true, ".xml": true, ".toml": true,
	// Logs
	".log": true,
	// Docker / infra
	".dockerfile": true,
}

func classify(path string) fileKind {
	base := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(path))

	if ext == ".rb" || ext == ".rake" || ext == ".gemspec" {
		return kindRuby
	}

	if rubyBasenames[base] {
		return kindRuby
	}

	if knownTextExtensions[ext] {
		return kindPassthrough
	}

	// Special basenames that are text but have no useful extension
	// (Dockerfile, Gemfile.lock, .gitignore, .env.example, etc.)
	switch base {
	case "Dockerfile", "Gemfile.lock", ".gitignore", ".env.example", ".ruby-version":
		return kindPassthrough
	}
 
	return kindUnknown
}

// ViewFile is the canonical file-access entry point for RTS.
// It reads Ruby source from r and writes an appropriate representation
// to w — a skeleton or full content for Ruby, raw content with a header
// for anything else.
//
// The `path` argument is used only for classification (extension &
// basename) and for the header text. The actual file bytes come from r.
// This mirrors how Process is structured: the caller opens the file and
// hands us the reader, keeping ViewFile easy to test with strings.Reader.
func ViewFile(path string, r io.Reader, w io.Writer) error {
	kind := classify(path)

	switch kind {
	case kindRuby:
		// Ruby files will just use the existing logics.
		return Process(r, w)
	case kindPassthrough, kindUnknown:
		return passthroughWithHeader(path, kind, r, w)
	}

	return nil
}

// passthroughWithHeader writes the raw file content to w, prefixed with
// a single-line header describing what was returned. The header is
// always a `#` comment so Claude Code sees a consistent format regardless
// of the file's actual comment syntax.
func passthroughWithHeader(path string, kind fileKind, r io.Reader, w io.Writer) error {
	lines, err := readAllLines(r)
	if err != nil {
		return err
	}

	writer := bufio.NewWriter(w)
	defer writer.Flush()

	label := describeKind(path, kind)
	fmt.Fprintf(writer, "# rts: %d lines %s (full)\n", len(lines), label)

	for _, line := range lines {
		writer.WriteString(line + "\n")
	}
	return nil
}

// describeKind returns a short human-readable label for the file type,
// used in the passthrough header so Claude Code can see at a glance
// what kind of file it received.
func describeKind(path string, kind fileKind) string {
	if kind == kindUnknown {
		return "unknown type"
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext != "" {
		return ext
	}

	return filepath.Base(path)
}