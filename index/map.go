package index

// The project map: RTS's answer to "what is this codebase?", derived once at
// index time and served at the start of every session.
//
// The symbol index answers a question you already know how to ask ("where is
// calculate_metrics defined?"). It cannot answer the question a fresh session
// actually opens with, which is "what is this and where do I start?" — so
// every session re-derived that by hand, with directory listings and
// exploratory reads, and threw the answer away when it ended.
//
// Everything here is *derived*: regenerated wholesale by `rts index`, never
// edited, safe to delete. That is the line between this file and insight.go,
// which holds the things a session concluded rather than the things the code
// states. Keeping them in separate stores is what stops a past session's
// guess being read back as a fact about the code.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MapVersion is bumped when the on-disk shape changes. A map written by an
// older binary is discarded rather than migrated — it costs one `rts index`
// to rebuild, which is cheaper than carrying migration code for a cache.
const MapVersion = 1

// Bounds on what the map holds. These are rendering budgets, not analysis
// limits: the brief is injected into every session, so anything unbounded
// here is a tax paid on every single one.
const (
	maxDirs        = 14
	maxHotFiles    = 10
	maxLargeFiles  = 10
	maxEntryPoints = 6

	// A file over this many lines should be reached through a skeleton
	// rather than read whole — the threshold view_file already routes on.
	largeFileLines = 100

	// Line-counting reads the file. Skip anything implausibly large for
	// source: a vendored bundle or a checked-in dump is not what the map
	// is describing, and reading it at index time is pure cost.
	maxCountableBytes = 2 << 20
)

// ProjectMap is the whole derived picture, serialized to .rts/map.json.
type ProjectMap struct {
	Version     int           `json:"version"`
	GeneratedAt time.Time     `json:"generated_at"`
	Name        string        `json:"name"`
	Kind        string        `json:"kind"`                   // "rails" | "ruby" | "go" | "node" | "unknown"
	EntryPoints []string      `json:"entry_points,omitempty"` // where a reader starts
	Dirs        []DirSummary  `json:"dirs,omitempty"`
	Hot         []HotFile     `json:"hot,omitempty"`
	Largest     []FileSummary `json:"largest,omitempty"`
	Counts      Counts        `json:"counts"`
}

// DirSummary is one row of the layout table: a directory that holds code,
// with enough shape that the next tool choice is informed.
type DirSummary struct {
	Path    string `json:"path"`
	Files   int    `json:"files"`
	Symbols int    `json:"symbols"`
}

// FileSummary names a file and its size in lines.
type FileSummary struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
}

// HotFile is a file earlier sessions actually reached for, counted from
// ~/.rts/events.jsonl.
//
// This is the one part of the map that no static analysis could produce:
// symbol density says which file is biggest, and read frequency says which
// file the work keeps landing in. They are routinely different files, and
// the second one is the better answer to "where do I start".
type HotFile struct {
	Path  string `json:"path"`
	Reads int    `json:"reads"`
}

// Counts are the headline totals.
type Counts struct {
	Files       int            `json:"files"` // files indexed for symbols
	Symbols     int            `json:"symbols"`
	SymbolKinds map[string]int `json:"symbol_kinds,omitempty"`
}

// entryPointCandidates are the files a reader opens first, in the order a
// reader would want them. Presence is checked; absence is silent.
var entryPointCandidates = []string{
	"config/routes.rb",
	"config/application.rb",
	"main.go",
	"app.rb",
	"lib/tasks",
	"Gemfile",
	"go.mod",
	"package.json",
	"README.md",
	"CLAUDE.md",
	"AGENTS.md",
}

// countableExtensions are the file types worth a line count. Anything else
// is counted as a file but never read.
var countableExtensions = map[string]bool{
	".rb": true, ".rake": true, ".erb": true, ".go": true,
	".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".yml": true, ".yaml": true, ".json": true, ".md": true,
	".css": true, ".scss": true, ".sql": true,
}

// BuildMap derives the project map from the symbol list plus one filesystem
// walk. It takes the symbols rather than re-parsing, so `rts index` pays for
// parsing exactly once.
//
// A walk error is never fatal: the map is a convenience, and a map missing
// one unreadable directory is worth far more than no map at all.
func BuildMap(projectRoot string, symbols []Symbol) (*ProjectMap, error) {
	pm := &ProjectMap{
		Version:     MapVersion,
		GeneratedAt: time.Now().UTC(),
		Name:        filepath.Base(projectRoot),
		Counts:      Counts{SymbolKinds: map[string]int{}},
	}

	symbolsByDir := map[string]int{}
	indexedFiles := map[string]bool{}
	for _, sym := range symbols {
		pm.Counts.Symbols++
		pm.Counts.SymbolKinds[sym.Kind]++
		symbolsByDir[filepath.Dir(sym.Path)]++
		indexedFiles[sym.Path] = true
	}
	pm.Counts.Files = len(indexedFiles)

	filesByDir := map[string]int{}
	var sized []FileSummary

	err := filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree — see the doc comment
		}
		if d.IsDir() {
			if skipDir(path, d.Name(), projectRoot) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip what BuildIndex skips, or the two stores describe different
		// trees: db/migrate holds hundreds of files the symbol index has
		// never seen, and a layout table counting them is describing a
		// codebase nothing else in RTS agrees exists. Ruby files go through
		// the shared predicate; everything else is counted for layout only.
		if filepath.Ext(path) == ".rb" && !shouldIndex(path) {
			return nil
		}

		rel, err := filepath.Rel(projectRoot, path)
		if err != nil {
			return nil
		}

		filesByDir[filepath.Dir(rel)]++

		lines, ok := countLines(path)
		if ok {
			sized = append(sized, FileSummary{Path: rel, Lines: lines})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	pm.Kind = detectKind(projectRoot)
	pm.EntryPoints = detectEntryPoints(projectRoot)
	pm.Dirs = rankDirs(filesByDir, symbolsByDir)
	pm.Largest = rankLargest(sized)
	pm.Hot = rankHot(projectRoot, indexedFiles)

	return pm, nil
}

// detectKind names the project shape, which is what tells a reader whether
// "app/models" means anything. Checked most-specific-first: a Rails app has
// a Gemfile too, and answering "ruby" there would be true and useless.
func detectKind(root string) string {
	switch {
	case exists(root, "config/application.rb"):
		return "rails"
	case exists(root, "go.mod"):
		return "go"
	case exists(root, "Gemfile"):
		return "ruby"
	case exists(root, "package.json"):
		return "node"
	default:
		return "unknown"
	}
}

func detectEntryPoints(root string) []string {
	var found []string
	for _, candidate := range entryPointCandidates {
		if exists(root, candidate) {
			found = append(found, candidate)
		}
		if len(found) == maxEntryPoints {
			break
		}
	}
	return found
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// rankDirs returns the directories that actually hold code, busiest first.
// Ranked by symbol count rather than file count: a directory of 400 fixtures
// tells a reader nothing, and a directory of 40 services tells them where
// the program lives.
func rankDirs(files, symbols map[string]int) []DirSummary {
	rows := make([]DirSummary, 0, len(symbols))
	for dir, count := range symbols {
		if dir == "." {
			continue
		}
		rows = append(rows, DirSummary{Path: dir, Files: files[dir], Symbols: count})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Symbols != rows[j].Symbols {
			return rows[i].Symbols > rows[j].Symbols
		}
		return rows[i].Path < rows[j].Path
	})

	if len(rows) > maxDirs {
		rows = rows[:maxDirs]
	}
	return rows
}

// rankLargest returns the files big enough that reading one whole is a
// mistake AND that RTS can offer something better than a whole read for.
//
// Ruby only, and that is the whole point of the filter. The section exists to
// say "reach for the skeleton instead", and the skeletonizer parses Ruby — so
// a list that also carried package-lock.json, a VCR cassette or a Markdown
// brief would be handing out advice that fails on first use. The threshold is
// view_file's own routing threshold, so the list names exactly the files that
// tool would skeletonize.
//
// Tests are excluded, and that exclusion is the difference between a useful
// list and a useless one. Measured on a real Rails app, the ten largest Ruby
// files were eight test files: a session orienting itself was handed
// parse_job_test.rb and transcript_segment_test.rb, and never saw project.rb
// or content_element.rb at all. A test file is something you open because the
// task took you there, never something you orient on. They stay in the layout
// table, where knowing the suite is large is worth something.
//
// Below the threshold there is nothing to warn about, so a small project
// correctly produces an empty list rather than a padded one.
func rankLargest(sized []FileSummary) []FileSummary {
	var big []FileSummary
	for _, f := range sized {
		if f.Lines >= largeFileLines && strings.HasSuffix(f.Path, ".rb") && !isTestPath(f.Path) {
			big = append(big, f)
		}
	}

	sort.Slice(big, func(i, j int) bool {
		if big[i].Lines != big[j].Lines {
			return big[i].Lines > big[j].Lines
		}
		return big[i].Path < big[j].Path
	})

	if len(big) > maxLargeFiles {
		big = big[:maxLargeFiles]
	}
	return big
}

// isTestPath recognises a test file by either convention Rails projects use:
// living under test/ or spec/, or carrying the _test/_spec suffix wherever it
// sits. Both are checked because both occur — a concern's test lives at
// test/models/concerns/..., and some suites keep tests beside the code.
func isTestPath(path string) bool {
	if strings.HasSuffix(path, "_test.rb") || strings.HasSuffix(path, "_spec.rb") {
		return true
	}

	switch first, _, _ := strings.Cut(path, string(filepath.Separator)); first {
	case "test", "spec":
		return true
	}
	return false
}

// rankHot counts how often past sessions reached for each file, from the
// global event log.
//
// Scoped on the project root the event was logged under, NOT on whether this
// project's index happens to hold the path. Matching on the path alone looks
// like it works and is wrong: the log is one file for every project on the
// machine, relative paths are what get logged, and two Rails apps both hold
// "config/application.rb" — so a fresh project inherited a busier one's read
// counts and presented them as its own. The index membership check is kept
// on top of the root match, to drop paths that have since left the project.
//
// Events written before roots were logged carry none and are skipped. An
// empty "most-read" section is the correct output there: the section omits
// itself when it has nothing, and a wrong list of hot files is worse than no
// list at all.
func rankHot(projectRoot string, indexed map[string]bool) []HotFile {
	if len(indexed) == 0 || projectRoot == "" {
		return nil
	}

	absRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	f, err := os.Open(filepath.Join(home, ".rts", "events.jsonl"))
	if err != nil {
		return nil // no log yet is the normal case on a fresh install
	}
	defer f.Close()

	counts := map[string]int{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		var ev struct {
			Path string `json:"path"`
			Root string `json:"root"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue // old format or a torn write; neither is worth failing over
		}
		if ev.Path == "" || ev.Root != absRoot {
			continue
		}
		if rel := strings.TrimPrefix(ev.Path, "./"); indexed[rel] {
			counts[rel]++
		}
	}

	if len(counts) == 0 {
		return nil
	}

	rows := make([]HotFile, 0, len(counts))
	for path, n := range counts {
		rows = append(rows, HotFile{Path: path, Reads: n})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Reads != rows[j].Reads {
			return rows[i].Reads > rows[j].Reads
		}
		return rows[i].Path < rows[j].Path
	})

	if len(rows) > maxHotFiles {
		rows = rows[:maxHotFiles]
	}
	return rows
}

// countLines returns the line count of a text file. The bool is false for
// anything not worth counting — an unknown extension, an unreadable file, or
// something too large to be source.
func countLines(path string) (int, bool) {
	if !countableExtensions[strings.ToLower(filepath.Ext(path))] {
		return 0, false
	}

	info, err := os.Stat(path)
	if err != nil || info.Size() > maxCountableBytes {
		return 0, false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	if len(data) == 0 {
		return 0, true
	}

	// Count separators, then add the final line when the file does not end
	// in one. Counting newlines alone is short by one on every file whose
	// last line is unterminated.
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n, true
}

// WriteMap serializes the map to .rts/map.json.
func WriteMap(projectRoot string, pm *ProjectMap) error {
	dir := filepath.Join(projectRoot, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating .rts directory: %w", err)
	}

	data, err := json.MarshalIndent(pm, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling map: %w", err)
	}

	outPath := filepath.Join(dir, "map.json")
	if err := writeFileAtomic(outPath, data); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	fmt.Fprintf(os.Stderr, "[rts index] wrote %s\n", outPath)
	return nil
}

// LoadMap reads .rts/map.json. A map from an older binary is reported as
// absent rather than returned half-understood — see MapVersion.
func LoadMap(projectRoot string) (*ProjectMap, error) {
	path := filepath.Join(projectRoot, ".rts", "map.json")

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no project map at %s - run `rts index` in your project root first.", path)
	}
	if err != nil {
		return nil, err
	}

	var pm ProjectMap
	if err := json.Unmarshal(data, &pm); err != nil {
		return nil, fmt.Errorf("parsing %s (map may be corrupted, try re-running `rts index`): %w", path, err)
	}
	if pm.Version != MapVersion {
		return nil, fmt.Errorf("project map at %s was written by a different rts version - re-run `rts index`", path)
	}

	return &pm, nil
}
