package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

func TestDetectKind(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		// Rails first: a Rails app has a Gemfile too, and answering "ruby"
		// there would be true and useless.
		{"rails beats ruby", []string{"Gemfile", "config/application.rb"}, "rails"},
		{"plain ruby", []string{"Gemfile"}, "ruby"},
		{"go", []string{"go.mod"}, "go"},
		{"node", []string{"package.json"}, "node"},
		{"nothing recognisable", []string{"notes.txt"}, "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tc.files {
				writeFile(t, root, f, "x")
			}
			if got := detectKind(root); got != tc.want {
				t.Errorf("detectKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildMap_SummarizesTheProject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Gemfile", "source 'https://rubygems.org'\n")
	writeFile(t, root, "config/application.rb", "module App; end\n")
	writeFile(t, root, "config/routes.rb", "Rails.application.routes.draw {}\n")
	writeFile(t, root, "app/models/user.rb", "class User\nend\n")

	symbols := []Symbol{
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
		{Name: "full_name", Kind: "method", Path: "app/models/user.rb", Line: 2, Container: "User"},
		{Name: "Report", Kind: "class", Path: "app/services/report.rb", Line: 1},
	}

	pm, err := BuildMap(root, symbols)
	if err != nil {
		t.Fatalf("BuildMap: %v", err)
	}

	if pm.Kind != "rails" {
		t.Errorf("Kind = %q, want rails", pm.Kind)
	}
	if pm.Counts.Symbols != 3 {
		t.Errorf("Symbols = %d, want 3", pm.Counts.Symbols)
	}
	// Files counts the files that produced symbols, not every file walked.
	if pm.Counts.Files != 2 {
		t.Errorf("Files = %d, want 2", pm.Counts.Files)
	}
	if pm.Counts.SymbolKinds["class"] != 2 || pm.Counts.SymbolKinds["method"] != 1 {
		t.Errorf("SymbolKinds = %v", pm.Counts.SymbolKinds)
	}
	if pm.Version != MapVersion {
		t.Errorf("Version = %d, want %d", pm.Version, MapVersion)
	}

	var sawRoutes bool
	for _, e := range pm.EntryPoints {
		if e == "config/routes.rb" {
			sawRoutes = true
		}
	}
	if !sawRoutes {
		t.Errorf("EntryPoints = %v, want config/routes.rb among them", pm.EntryPoints)
	}
}

func TestBuildMap_SkipsNoiseDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app/models/user.rb", "class User\nend\n")
	writeFile(t, root, "node_modules/pkg/index.js", "module.exports = {}\n")
	writeFile(t, root, "vendor/bundle/gem.rb", "class Gem\nend\n")
	writeFile(t, root, ".git/config", "[core]\n")

	pm, err := BuildMap(root, []Symbol{
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
	})
	if err != nil {
		t.Fatalf("BuildMap: %v", err)
	}

	for _, f := range pm.Largest {
		t.Errorf("largest should be empty for tiny files, got %v", f)
	}
	for _, d := range pm.Dirs {
		if d.Path == "node_modules/pkg" || d.Path == "vendor/bundle" {
			t.Errorf("noise directory %q reached the map", d.Path)
		}
	}
}

func TestRankDirs_RanksBySymbolsAndSkipsRoot(t *testing.T) {
	// Ranked by symbols, not files: a directory of 400 fixtures tells a
	// reader nothing; a directory of 40 services tells them where to look.
	dirs := rankDirs(
		map[string]int{"app/models": 3, "test/fixtures": 400, "app/services": 2, ".": 9},
		map[string]int{"app/models": 40, "test/fixtures": 1, "app/services": 90, ".": 5},
	)

	if len(dirs) != 3 {
		t.Fatalf("got %d dirs, want 3 (root excluded)", len(dirs))
	}
	if dirs[0].Path != "app/services" {
		t.Errorf("first dir = %q, want app/services", dirs[0].Path)
	}
	if dirs[0].Files != 2 {
		t.Errorf("file count = %d, want 2", dirs[0].Files)
	}
	for _, d := range dirs {
		if d.Path == "." {
			t.Error("project root leaked into the layout table")
		}
	}
}

func TestRankLargest_OnlyFilesWorthSkeletonizing(t *testing.T) {
	got := rankLargest([]FileSummary{
		{Path: "small.rb", Lines: 20},
		{Path: "big.rb", Lines: 900},
		{Path: "medium.rb", Lines: largeFileLines},
	})

	if len(got) != 2 {
		t.Fatalf("got %d files, want the 2 at or over the skeleton threshold", len(got))
	}
	if got[0].Path != "big.rb" {
		t.Errorf("first = %q, want the largest", got[0].Path)
	}
}

func TestCountLines(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name     string
		rel      string
		content  string
		want     int
		wantable bool
	}{
		{"trailing newline", "a.rb", "one\ntwo\nthree\n", 3, true},
		// Counting separators alone is short by one on every file whose last
		// line is unterminated — which is most hand-edited files.
		{"no trailing newline", "b.rb", "one\ntwo", 2, true},
		{"empty file", "c.rb", "", 0, true},
		{"unknown extension", "d.bin", "one\ntwo\n", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, root, tc.rel, tc.content)
			got, ok := countLines(path)
			if ok != tc.wantable {
				t.Fatalf("countable = %v, want %v", ok, tc.wantable)
			}
			if ok && got != tc.want {
				t.Errorf("lines = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWriteAndLoadMap_RoundTrips(t *testing.T) {
	root := t.TempDir()
	original := &ProjectMap{
		Version:     MapVersion,
		GeneratedAt: time.Now().UTC().Truncate(time.Second),
		Name:        "demo",
		Kind:        "rails",
		Counts:      Counts{Files: 2, Symbols: 3},
	}

	if err := WriteMap(root, original); err != nil {
		t.Fatalf("WriteMap: %v", err)
	}

	loaded, err := LoadMap(root)
	if err != nil {
		t.Fatalf("LoadMap: %v", err)
	}
	if loaded.Name != "demo" || loaded.Kind != "rails" || loaded.Counts.Symbols != 3 {
		t.Errorf("round trip lost data: %+v", loaded)
	}
}

func TestLoadMap_MissingAndMismatchedVersions(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if _, err := LoadMap(t.TempDir()); err == nil {
			t.Fatal("expected an error for a project with no map")
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		// A derived artifact from an older binary is discarded rather than
		// migrated: rebuilding costs one `rts index`.
		root := t.TempDir()
		writeFile(t, root, ".rts/map.json", `{"version":99,"name":"x","counts":{}}`)

		if _, err := LoadMap(root); err == nil {
			t.Fatal("expected an error for a map from another version")
		}
	})
}

func TestRankHot_IsEmptyWithoutAnIndexedFileSet(t *testing.T) {
	if got := rankHot(t.TempDir(), map[string]bool{}); got != nil {
		t.Errorf("rankHot with no indexed files = %v, want nil", got)
	}
	if got := rankHot("", map[string]bool{"a.rb": true}); got != nil {
		t.Errorf("rankHot with no project root = %v, want nil", got)
	}
}

// writeEventLog plants a fake ~/.rts/events.jsonl and points HOME at it.
func writeEventLog(t *testing.T, lines ...string) {
	t.Helper()

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".rts"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(home, ".rts", "events.jsonl"), []byte(body), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	t.Setenv("HOME", home)
}

func TestRankHot_CountsOnlyThisProjectsReads(t *testing.T) {
	// The regression this guards: the event log is one global file, relative
	// paths are what get logged, and two Rails apps both hold
	// "config/application.rb" — so scoping on the path alone handed a fresh
	// project another repo's read counts and presented them as its own.
	ours := t.TempDir()
	theirs := t.TempDir()

	writeEventLog(t,
		`{"path":"config/application.rb","root":"`+theirs+`","tool":"view_file"}`,
		`{"path":"config/application.rb","root":"`+theirs+`","tool":"view_file"}`,
		`{"path":"app/models/user.rb","root":"`+ours+`","tool":"view_file"}`,
		`{"path":"./app/models/user.rb","root":"`+ours+`","tool":"view_file"}`,
		`{"path":"app/models/user.rb"}`, // legacy event, no root
		`not json at all`,
		`{"path":"app/models/gone.rb","root":"`+ours+`","tool":"view_file"}`,
	)

	got := rankHot(ours, map[string]bool{
		"app/models/user.rb":    true,
		"config/application.rb": true,
	})

	if len(got) != 1 {
		t.Fatalf("rankHot = %v, want exactly our own file", got)
	}
	if got[0].Path != "app/models/user.rb" {
		t.Errorf("path = %q, want app/models/user.rb", got[0].Path)
	}
	// Two events, one of them written with a "./" prefix: both are ours.
	// The legacy rootless event is skipped rather than guessed at.
	if got[0].Reads != 2 {
		t.Errorf("reads = %d, want 2", got[0].Reads)
	}
}

func TestRankHot_DropsPathsNoLongerInTheIndex(t *testing.T) {
	root := t.TempDir()
	writeEventLog(t, `{"path":"app/models/deleted.rb","root":"`+root+`","tool":"view_file"}`)

	if got := rankHot(root, map[string]bool{"app/models/live.rb": true}); got != nil {
		t.Errorf("rankHot = %v, want nil for a file that left the project", got)
	}
}

func TestRankLargest_OnlyRubyFilesTheSkeletonizerCanActuallyHandle(t *testing.T) {
	// The section this feeds says "reach for view_bionic_skeleton". The
	// skeletonizer parses Ruby, so a list carrying package-lock.json, a VCR
	// cassette or a Markdown brief would be advice that fails on first use.
	got := rankLargest([]FileSummary{
		{Path: "app/models/invoice.rb", Lines: 600},
		{Path: "package-lock.json", Lines: 4000},
		{Path: "DESIGN-BRIEF.md", Lines: 324},
		{Path: "test/cassettes/vcr.yml", Lines: 9000},
		{Path: "mcp/server.go", Lines: 677},
		{Path: "app/services/billing.rb", Lines: 150},
	})

	want := []string{"app/models/invoice.rb", "app/services/billing.rb"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want only the Ruby files", got)
	}
	for i, path := range want {
		if got[i].Path != path {
			t.Errorf("[%d] = %q, want %q", i, got[i].Path, path)
		}
	}
}

func TestBuildMap_SkipsWhatBuildIndexSkips(t *testing.T) {
	// The two stores must describe the same tree. db/migrate holds hundreds
	// of files the symbol index has never seen, and a layout table counting
	// them describes a codebase nothing else in RTS agrees exists.
	root := t.TempDir()
	writeFile(t, root, "app/models/user.rb", "class User\nend\n")
	for _, name := range []string{"20240101_a.rb", "20240102_b.rb", "20240103_c.rb"} {
		writeFile(t, root, filepath.Join("db/migrate", name), "class M < ActiveRecord::Migration\nend\n")
	}

	pm, err := BuildMap(root, []Symbol{
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
	})
	if err != nil {
		t.Fatalf("BuildMap: %v", err)
	}

	for _, d := range pm.Dirs {
		if strings.Contains(d.Path, "db/migrate") {
			t.Errorf("db/migrate reached the layout table: %+v", d)
		}
	}
	for _, f := range pm.Largest {
		if strings.Contains(f.Path, "db/migrate") {
			t.Errorf("db/migrate reached the largest list: %+v", f)
		}
	}
}

func TestIsTestPath(t *testing.T) {
	// Both conventions occur: a concern's test lives under test/, and some
	// suites keep tests beside the code they cover.
	tests := []struct {
		path string
		want bool
	}{
		{"test/models/user_test.rb", true},
		{"spec/models/user_spec.rb", true},
		{"test/models/concerns/project/data_layer_manager_test.rb", true},
		{"app/models/user_test.rb", true},
		{"app/models/user.rb", false},
		{"db/schema.rb", false},
		{"scripts/latest_test_run.rb", false}, // "test" inside a name is not a test
		{"lib/testing.rb", false},
	}

	for _, tc := range tests {
		if got := isTestPath(tc.path); got != tc.want {
			t.Errorf("isTestPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestRankLargest_ExcludesTestsSoOrientationSeesSourceFiles(t *testing.T) {
	// Measured on a real Rails app, the ten largest Ruby files were eight
	// test files — a session orienting itself never saw project.rb at all.
	got := rankLargest([]FileSummary{
		{Path: "test/models/project_test.rb", Lines: 2125},
		{Path: "test/jobs/parse_job_test.rb", Lines: 2268},
		{Path: "db/schema.rb", Lines: 3069},
		{Path: "app/models/project.rb", Lines: 781},
		{Path: "app/models/content_element.rb", Lines: 592},
	})

	want := []string{"db/schema.rb", "app/models/project.rb", "app/models/content_element.rb"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want the non-test files only", got)
	}
	for i, path := range want {
		if got[i].Path != path {
			t.Errorf("[%d] = %q, want %q", i, got[i].Path, path)
		}
	}
}
