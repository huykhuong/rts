package tool

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	listDefaultDepth = 2
	listMaxDepth		 = 4
)

var listSkipDirs = map[string]bool{
	".git":         true,
	".rts":         true,
	"node_modules": true,
	"vendor":       true,
	"tmp":          true,
	"log":          true,
	"coverage":     true,
}

type listEntry struct {
	relPath	string
	isDir	bool
	lines	int
}

func ListDirectory(path string, depth int) (string, error) {
	if path == "" {
		path = "."
	}
	if depth <= 0 {
		depth = listDefaultDepth
	}
	if depth > listMaxDepth {
		depth = listMaxDepth
	}

	// Confirm the path exists and is a directory before walking
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot access %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}

	entries, err := collectEntries(path, depth)
	if err != nil {
		return "", err
	}

	return formatListing(path, depth, entries), nil
}

func collectEntries(root string, maxDepth int) ([]listEntry, error) {
	var entries []listEntry

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if path == root {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		entryDepth := strings.Count(rel, string(filepath.Separator)) + 1

		if d.IsDir() {
			if listSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			// If this directory is already at max depth, list it but don't descend.
			if entryDepth >= maxDepth {
				entries = append(entries, listEntry{relPath: rel, isDir: true})
				return filepath.SkipDir
			}
			entries = append(entries, listEntry{relPath: rel, isDir: true})

			return nil
		}

		if entryDepth > maxDepth {
			return nil
		}
		entries = append(entries, listEntry{
			relPath: rel,
			isDir: false,
			lines: countLines(path),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	return entries, nil
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}

	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	count := 0
	for scanner.Scan() {
		count++
	}
	return count
}

func formatListing(root string, depth int, entries []listEntry) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "# %s (depth %d) — %d entries\n", root, depth, len(entries))

	if len(entries) == 0 {
		buf.WriteString(" (empty or all contents filtered)\n")
		return buf.String()
	}

	for _, e := range entries {
		level := strings.Count(e.relPath, string(filepath.Separator))
		indent := strings.Repeat("  ", level+1)

		name := filepath.Base(e.relPath)

		if e.isDir {
			fmt.Fprintf(&buf, "%s%s/\n", indent, name)
		}  else {
			fmt.Fprintf(&buf, "%s%-40s (%d lines)\n", indent, name, e.lines)
		}
	}

	return buf.String()
}