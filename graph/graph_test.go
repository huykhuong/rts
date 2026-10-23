package graph

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rts/index"
)

// project builds a small Rails-shaped project and the index that describes it.
func project(t *testing.T) (string, *index.IndexFile) {
	t.Helper()

	root := t.TempDir()
	write(t, root, "app/models/board.rb", "class Board\nend\n")
	write(t, root, "app/models/board_template.rb", "class BoardTemplate\n  def apply; end\nend\n")
	write(t, root, "app/controllers/board_templates_controller.rb", "class BoardTemplatesController\nend\n")
	write(t, root, "app/jobs/create_board_from_template_job.rb", "class CreateBoardFromTemplateJob\nend\n")
	write(t, root, "app/views/board_templates/index.html.erb", "<h1>Templates</h1>\n")
	write(t, root, "app/models/admin/template.rb", "module Admin\n  class Template\n  end\nend\n")
	write(t, root, "app/models/report/template.rb", "module Report\n  class Template\n  end\nend\n")
	write(t, root, "test/models/board_template_test.rb", "class BoardTemplate\nend\n")
	write(t, root, "config/master.key", "0123456789abcdef0123456789abcdef\n")

	idx := &index.IndexFile{Symbols: []index.Symbol{
		{Name: "Board", Kind: "class", Path: "app/models/board.rb", Line: 1},
		{Name: "BoardTemplate", Kind: "class", Path: "test/models/board_template_test.rb", Line: 1},
		{Name: "BoardTemplate", Kind: "class", Path: "app/models/board_template.rb", Line: 1},
		{Name: "apply", Kind: "method", Path: "app/models/board_template.rb", Line: 2, Container: "BoardTemplate"},
		{Name: "BoardTemplatesController", Kind: "class", Path: "app/controllers/board_templates_controller.rb", Line: 1},
		{Name: "CreateBoardFromTemplateJob", Kind: "class", Path: "app/jobs/create_board_from_template_job.rb", Line: 1},
		{Name: "Admin", Kind: "module", Path: "app/models/admin/template.rb", Line: 1},
		{Name: "Template", Kind: "class", Path: "app/models/admin/template.rb", Line: 2, Container: "Admin"},
		{Name: "Report", Kind: "module", Path: "app/models/report/template.rb", Line: 1},
		{Name: "Template", Kind: "class", Path: "app/models/report/template.rb", Line: 2, Container: "Report"},
	}}
	return root, idx
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func boardTemplates() Feature {
	return Feature{
		Name:    "board templates",
		Summary: "Admins save a board layout as a template; new boards are created from it.",
		Members: []Member{
			{Ref: "BoardTemplate", Role: "model"},
			{Ref: "BoardTemplatesController", Role: "controller"},
			{Ref: "CreateBoardFromTemplateJob", Role: "job"},
			{Ref: "app/views/board_templates", Role: "view"},
		},
		Edges: []Edge{
			{Src: "BoardTemplatesController", Dst: "BoardTemplate", Kind: "uses"},
			{Src: "Board", Dst: "BoardTemplate", Kind: "has_many"},
			{Src: "CreateBoardFromTemplateJob", Dst: "BoardTemplate", Kind: "uses"},
		},
		Notes: []string{"Templates are copied, not linked, so editing one never changes existing boards."},
	}
}

// saved resolves and records a feature, failing the test on any problem.
func saved(t *testing.T, db *sql.DB, root string, idx *index.IndexFile, f Feature) {
	t.Helper()

	if problems := Resolve(root, "", idx, &f); len(problems) > 0 {
		t.Fatalf("Resolve: %v", problems)
	}
	if _, err := Record(db, f); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func open_(t *testing.T, root string) *sql.DB {
	t.Helper()

	db, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestResolve_FillsPathsAndFingerprintsFromTheCode(t *testing.T) {
	root, idx := project(t)
	f := boardTemplates()

	if problems := Resolve(root, "", idx, &f); len(problems) > 0 {
		t.Fatalf("a valid map was refused: %v", problems)
	}

	model := f.Members[0]
	if model.Path != "app/models/board_template.rb" {
		t.Errorf("BoardTemplate should resolve to its model file, not the test that reopens it; got %q", model.Path)
	}
	if model.Fingerprint == "" {
		t.Error("member file was not fingerprinted")
	}
	if dir := f.Members[3]; dir.Path != "app/views/board_templates" || dir.Fingerprint != "" {
		t.Errorf("a directory member keeps its path and has nothing to hash; got %+v", dir)
	}
}

func TestResolve_ListsEveryProblemAtOnce(t *testing.T) {
	root, idx := project(t)
	f := boardTemplates()
	f.Members = append(f.Members,
		Member{Ref: "BoardTemplat"},
		Member{Ref: "Template"},
		Member{Ref: "apply"},
		Member{Ref: "app/models/missing.rb"},
	)
	f.Edges = append(f.Edges, Edge{Src: "Board", Dst: "BoardTemplate", Kind: "owns"})

	problems := Resolve(root, "", idx, &f)
	joined := strings.Join(problems, "\n")

	for _, want := range []string{
		`"BoardTemplat": nothing by that name`,
		`"Template": ambiguous; use the full name: Admin::Template, Report::Template`,
		`"apply": is a method`,
		`"app/models/missing.rb": no such file`,
		`kind "owns" is not one of`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
}

func TestResolve_RefusesProtectedFilesAsMembersAndInNotes(t *testing.T) {
	root, idx := project(t)

	f := boardTemplates()
	f.Members = append(f.Members, Member{Ref: "config/master.key"})
	if problems := Resolve(root, "", idx, &f); !strings.Contains(strings.Join(problems, "\n"), "protected credential file") {
		t.Errorf("config/master.key was accepted as a member: %v", problems)
	}

	f = boardTemplates()
	f.Notes = []string{"Signing uses the key in config/master.key."}
	if problems := Resolve(root, "", idx, &f); !strings.Contains(strings.Join(problems, "\n"), `names "config/master.key"`) {
		t.Errorf("a note naming config/master.key was accepted: %v", problems)
	}

	f = boardTemplates()
	f.Notes = []string{"Reads ENV vars for the S3 bucket, e.g. in production."}
	if problems := Resolve(root, "", idx, &f); len(problems) > 0 {
		t.Errorf("an ordinary note was refused: %v", problems)
	}
}

func TestOpenExisting_NeverCreatesAGraph(t *testing.T) {
	root := t.TempDir()

	if _, err := OpenExisting(root); err == nil {
		t.Fatal("OpenExisting opened a graph that was never written")
	}
	if _, err := os.Stat(Path(root)); !os.IsNotExist(err) {
		t.Fatalf("OpenExisting left a graph.db behind: %v", err)
	}
}

func TestTouchpoints_FeatureNameReturnsTheMap(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())

	text, err := Touchpoints(db, root, "", idx, "Board Templates", 1)
	if err != nil {
		t.Fatalf("Touchpoints: %v", err)
	}

	for _, want := range []string{
		"Feature: board templates",
		"BoardTemplate ",
		"app/models/board_template.rb",
		"uses → BoardTemplate",
		"has_many → BoardTemplate",
		"Notes (written by a past session; not verified)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("map is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "[changed]") {
		t.Errorf("nothing has changed, but the map says it has:\n%s", text)
	}
}

func TestTouchpoints_MarksMembersEditedSinceTheMapWasWritten(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())

	write(t, root, "app/models/board_template.rb", "class BoardTemplate\n  def apply; end\n  def preview; end\nend\n")

	text, err := Touchpoints(db, root, "", idx, "board templates", 1)
	if err != nil {
		t.Fatalf("Touchpoints: %v", err)
	}
	if !strings.Contains(text, "(model)  [changed]") || !strings.Contains(text, "1 member(s) marked [changed]") {
		t.Errorf("an edited member was not flagged:\n%s", text)
	}
}

func TestTouchpoints_ClassNameFollowsConnectionsAcrossFeatures(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())
	saved(t, db, root, idx, Feature{
		Name:    "boards",
		Summary: "Boards hold a team's visualizations.",
		Members: []Member{{Ref: "Board", Role: "model"}},
		Edges:   []Edge{{Src: "Board", Dst: "Admin::Template", Kind: "uses"}},
	})

	one, err := Touchpoints(db, root, "", idx, "BoardTemplatesController", 1)
	if err != nil {
		t.Fatalf("Touchpoints: %v", err)
	}
	if !strings.Contains(one, "Part of: board templates") || !strings.Contains(one, "BoardTemplatesController uses → BoardTemplate") {
		t.Errorf("depth 1 should give the feature and the direct edge:\n%s", one)
	}
	if strings.Contains(one, "Admin::Template") {
		t.Errorf("depth 1 went two steps out:\n%s", one)
	}

	two, err := Touchpoints(db, root, "", idx, "BoardTemplate", 2)
	if err != nil {
		t.Fatalf("Touchpoints: %v", err)
	}
	if !strings.Contains(two, "Board uses → Admin::Template") {
		t.Errorf("depth 2 should reach across into the boards map:\n%s", two)
	}
}

func TestTouchpoints_SearchesWhenNothingIsNamed(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())
	saved(t, db, root, idx, Feature{
		Name:    "boards",
		Summary: "Boards hold a team's visualizations.",
		Members: []Member{{Ref: "Board", Role: "model"}},
	})

	hit, err := Touchpoints(db, root, "", idx, "templates", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hit, `No feature is named "templates". Mapped features that match it:`) ||
		!strings.Contains(hit, "board templates (4 files) — Admins save a board layout") {
		t.Errorf("a topic word should find the feature it is about:\n%s", hit)
	}
	if strings.Contains(hit, "boards (") {
		t.Errorf("a feature the word is not about was listed:\n%s", hit)
	}

	miss, err := Touchpoints(db, root, "", idx, "billing", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(miss, `Nothing mapped matches "billing" (searched 2 feature map(s)`) {
		t.Errorf("a miss should say how much was searched, not list every map:\n%s", miss)
	}
}

func TestSearch_RanksAndRequiresEveryWord(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates()) // note: "Templates are copied, not linked…"
	saved(t, db, root, idx, Feature{
		Name:    "template copies",
		Summary: "Copying a board.",
		Members: []Member{{Ref: "Board"}},
	})

	names := func(q string) []string {
		hits, err := Search(db, q, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.Name)
		}
		return out
	}

	if got := names("template"); len(got) != 2 || got[0] != "board templates" && got[0] != "template copies" {
		t.Errorf("both features have the word in their name: %v", got)
	}
	if got := names("linked"); len(got) != 1 || got[0] != "board templates" {
		t.Errorf("a word only in a note should still find its feature: %v", got)
	}
	if got := names("CreateBoardFromTemplateJob"); len(got) != 1 || got[0] != "board templates" {
		t.Errorf("a member's name should find its feature: %v", got)
	}
	if got := names("templates linked"); len(got) != 1 {
		t.Errorf("every word must match: %v", got)
	}
	if got := names("pied"); len(got) != 0 {
		t.Errorf("a word inside another (\"copied\") is not a match: %v", got)
	}
	if got := names("linked templates"); len(got) != 1 || got[0] != "board templates" {
		t.Errorf("word order should not matter: %v", got)
	}
	if got := names("the feature about"); len(got) != 0 {
		t.Errorf("stopwords alone should match nothing: %v", got)
	}
}

func TestRecord_ReplacesTheMapAndKeepsTheOldOne(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())

	// An edge read off the code belongs to the code, and must survive a
	// session rewriting the map.
	if _, err := db.Exec("INSERT INTO edges (feature_id, src, dst, kind, origin) SELECT id, 'Board', 'BoardTemplate', 'has_one', 'code' FROM features"); err != nil {
		t.Fatalf("seeding a code edge: %v", err)
	}

	rewrite := Feature{
		Name:    "board templates",
		Summary: "Templates now only cover the job.",
		Members: []Member{{Ref: "CreateBoardFromTemplateJob", Role: "job"}},
	}
	if problems := Resolve(root, "", idx, &rewrite); len(problems) > 0 {
		t.Fatalf("Resolve: %v", problems)
	}
	replaced, err := Record(db, rewrite)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !replaced {
		t.Error("rewriting an existing feature was not reported as a replacement")
	}

	text, _ := Touchpoints(db, root, "", idx, "board templates", 1)
	if strings.Contains(text, "BoardTemplatesController") || strings.Contains(text, "Templates are copied") {
		t.Errorf("the old map's members or notes survived the replacement:\n%s", text)
	}
	if !strings.Contains(text, "has_one → BoardTemplate  (from the code)") {
		t.Errorf("the code-derived edge was lost in the replacement:\n%s", text)
	}

	var snapshot string
	if err := db.QueryRow("SELECT snapshot FROM feature_history WHERE name = 'board templates'").Scan(&snapshot); err != nil {
		t.Fatalf("no history row: %v", err)
	}
	if !strings.Contains(snapshot, "BoardTemplatesController") {
		t.Errorf("history does not hold the replaced map: %s", snapshot)
	}
}

func TestRecord_ConcurrentWritersBothLand(t *testing.T) {
	// Worktrees share the main checkout's .rts, so two sessions finishing at
	// once is ordinary. Each opens its own connection, as two processes would.
	root, idx := project(t)
	open_(t, root) // create the schema first, as the first session would

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := Open(root)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = db.Close() }()

			f := boardTemplates()
			f.Name = "feature " + string(rune('a'+i))
			if problems := Resolve(root, "", idx, &f); len(problems) > 0 {
				t.Errorf("Resolve: %v", problems)
				return
			}
			if _, err := Record(db, f); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	db := open_(t, root)
	features, err := Summaries(db, root, "")
	if err != nil {
		t.Fatalf("Summaries: %v", err)
	}
	if len(features) != 8 {
		t.Errorf("expected 8 features after 8 concurrent writes, got %d", len(features))
	}
}

func TestPrune_DropsWhatTheCodeNoLongerHas(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())
	saved(t, db, root, idx, Feature{
		Name:    "template jobs",
		Summary: "Background creation of boards.",
		Members: []Member{{Ref: "CreateBoardFromTemplateJob", Role: "job"}},
	})

	// The job is deleted: its member rows and its edge go, the feature made
	// of nothing else goes, and board templates keeps everything else.
	if err := os.Remove(filepath.Join(root, "app/jobs/create_board_from_template_job.rb")); err != nil {
		t.Fatal(err)
	}
	var kept []index.Symbol
	for _, s := range idx.Symbols {
		if s.Name != "CreateBoardFromTemplateJob" {
			kept = append(kept, s)
		}
	}

	res, err := Prune(db, root, &index.IndexFile{Symbols: kept})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Members != 2 || res.Edges != 1 || res.Features != 1 {
		t.Errorf("expected 2 members, 1 edge, 1 feature dropped; got %+v", res)
	}

	features, _ := Summaries(db, root, "")
	if len(features) != 1 || features[0].Name != "board templates" || features[0].Members != 3 {
		t.Errorf("board templates should survive with 3 members; got %+v", features)
	}
}

func TestOpen_UpgradesAVersionOneGraphInPlace(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())

	// Turn it back into what version 1 wrote: no stop_asks table.
	if _, err := db.Exec("DROP TABLE stop_asks; DROP TABLE refs; DROP TABLE nudges; PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	upgraded, err := Open(root)
	if err != nil {
		t.Fatalf("opening a version 1 graph: %v", err)
	}
	defer func() { _ = upgraded.Close() }()

	var version int
	_ = upgraded.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion {
		t.Errorf("user_version = %d after the upgrade, want %d", version, SchemaVersion)
	}
	if err := MarkAsked(upgraded, "s", []string{"board_template"}, time.Now()); err != nil {
		t.Errorf("stop_asks is missing after the upgrade: %v", err)
	}
	if err := MarkNudged(upgraded, "s", []string{"board templates"}, time.Now()); err != nil {
		t.Errorf("nudges is missing after the upgrade: %v", err)
	}
	if features, _ := Summaries(upgraded, root, ""); len(features) != 1 {
		t.Errorf("the upgrade lost the feature maps: %+v", features)
	}
}

// withWorktree adds a linked worktree to a project the way git records one,
// holding a class the main checkout does not have.
func withWorktree(t *testing.T, root string) string {
	t.Helper()

	wt := t.TempDir()
	write(t, wt, "app/models/bar_chart.rb", "class BarChart\nend\n")
	write(t, wt, "app/models/board_template.rb", "class BoardTemplate\n  def bars; end\nend\n")
	write(t, wt, ".git", "gitdir: "+filepath.Join(root, ".git", "worktrees", "wt")+"\n")
	write(t, root, ".git/worktrees/wt/gitdir", filepath.Join(wt, ".git")+"\n")
	return wt
}

func barChart() Feature {
	return Feature{
		Name:    "bar chart",
		Summary: "A bar chart board template.",
		Members: []Member{{Ref: "BarChart", Role: "model"}, {Ref: "BoardTemplate", Role: "model"}},
		Edges:   []Edge{{Src: "BarChart", Dst: "BoardTemplate", Kind: "uses"}},
	}
}

func TestResolve_FindsBranchOnlyCodeInTheSessionsCheckout(t *testing.T) {
	root, idx := project(t)
	wt := withWorktree(t, root)

	// A dispatcher in the main checkout maps what a worker built in its
	// worktree: the name resolves there, and its file is fingerprinted from
	// the worktree that has it.
	f := barChart()
	if problems := Resolve(root, "", idx, &f); len(problems) > 0 {
		t.Fatalf("BarChart exists in one of the project's worktrees and should resolve from the main checkout: %v", problems)
	}
	if f.Members[0].Path != "app/models/bar_chart.rb" || f.Members[0].Fingerprint == "" {
		t.Errorf("a member found in another worktree was not located and fingerprinted: %+v", f.Members[0])
	}

	f = barChart()
	if problems := Resolve(root, wt, idx, &f); len(problems) > 0 {
		t.Fatalf("BarChart exists in the session's worktree and should resolve: %v", problems)
	}
	if f.Members[0].Path != "app/models/bar_chart.rb" || f.Members[0].Fingerprint == "" {
		t.Errorf("branch-only member was not located and fingerprinted: %+v", f.Members[0])
	}
	if f.refPaths["BarChart"] != "app/models/bar_chart.rb" {
		t.Errorf("the edge end's file was not recorded: %v", f.refPaths)
	}
}

func TestResolve_ARemovedWorktreeNoLongerCounts(t *testing.T) {
	root, idx := project(t)
	wt := withWorktree(t, root)
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}

	f := barChart()
	if problems := Resolve(root, "", idx, &f); !strings.Contains(strings.Join(problems, "\n"), `"BarChart": nothing by that name`) {
		t.Errorf("BarChart's only worktree is gone, so it should not resolve: %v", problems)
	}
}

func TestTouchpoints_JudgesChangesAgainstTheReadersCheckout(t *testing.T) {
	root, idx := project(t)
	wt := withWorktree(t, root)
	db := open_(t, root)

	f := barChart()
	if problems := Resolve(root, wt, idx, &f); len(problems) > 0 {
		t.Fatalf("Resolve: %v", problems)
	}
	if _, err := Record(db, f); err != nil {
		t.Fatal(err)
	}

	fromWorktree, _ := Touchpoints(db, root, wt, idx, "bar chart", 1)
	if strings.Contains(fromWorktree, "[changed]") {
		t.Errorf("the session that wrote the map sees its own files as changed:\n%s", fromWorktree)
	}
	fromMain, _ := Touchpoints(db, root, "", idx, "bar chart", 1)
	if !strings.Contains(fromMain, "app/models/board_template.rb  (model)  [changed]") {
		t.Errorf("the main checkout's BoardTemplate differs from the branch's and should be marked:\n%s", fromMain)
	}
}

func TestPrune_KeepsBranchWorkWhileItsWorktreeExists(t *testing.T) {
	root, idx := project(t)
	wt := withWorktree(t, root)
	db := open_(t, root)

	f := barChart()
	if problems := Resolve(root, wt, idx, &f); len(problems) > 0 {
		t.Fatalf("Resolve: %v", problems)
	}
	if _, err := Record(db, f); err != nil {
		t.Fatal(err)
	}

	// The main index never has BarChart. While the worktree holds it, the
	// map keeps it.
	if res, err := Prune(db, root, idx); err != nil || res.Members+res.Edges+res.Features != 0 {
		t.Fatalf("branch-only code was pruned while its worktree exists: %+v, %v", res, err)
	}

	// The worktree is removed unmerged: BarChart is gone everywhere.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	res, err := Prune(db, root, idx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Members != 1 || res.Edges != 1 {
		t.Errorf("expected BarChart's member and edge dropped once its worktree is gone; got %+v", res)
	}
}

func TestResolve_RefusesNotesThatPointAtScratchFiles(t *testing.T) {
	root, idx := project(t)

	for note, refused := range map[string]bool{
		"The sample is generated by tmp/il_sample/build.rb via bin/rails runner.": true,
		"See .claude/plans/board-editor.md for the reasoning.":                    true,
		"Coverage lands in coverage/index.html.":                                  true,
		"Exports are written under tmp/exports before upload.":                    false,
		"Regenerate fixtures with bin/rails prompt_library:fixtures.":             false,
		"Workflows live in .github/workflows/ci.yml.":                             false,
	} {
		f := boardTemplates()
		f.Notes = []string{note}
		problems := strings.Join(Resolve(root, "", idx, &f), "\n")
		if got := strings.Contains(problems, "scratch file"); got != refused {
			t.Errorf("note %q: refused = %v, want %v (%s)", note, got, refused, problems)
		}
	}
}

func TestFeaturesContaining_FileAndDirectoryMembers(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())

	for rel, want := range map[string]int{
		"app/models/board_template.rb":             1, // a class member's file
		"app/views/board_templates/index.html.erb": 1, // inside a directory member
		"app/views/board_templates_old/x.erb":      0, // a sibling with the same prefix is not inside
		"app/models/board.rb":                      0, // an edge end, not a member
	} {
		got, err := FeaturesContaining(db, rel)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Errorf("FeaturesContaining(%q) = %+v, want %d feature(s)", rel, got, want)
		}
	}
}

func TestNudged_RemembersPerSession(t *testing.T) {
	root, _ := project(t)
	db := open_(t, root)

	if err := MarkNudged(db, "s1", []string{"board templates"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, _ := Nudged(db, "s1"); !n["board templates"] {
		t.Error("s1's nudge was not remembered")
	}
	if n, _ := Nudged(db, "s2"); n["board templates"] {
		t.Error("another session inherited s1's nudge")
	}
}

func TestOverlaps_FlagsADuplicateButNotARelatedFeature(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates()) // 4 members

	dup := Feature{Name: "templates", Summary: "s", Members: []Member{
		{Ref: "BoardTemplate"}, {Ref: "BoardTemplatesController"}, {Ref: "CreateBoardFromTemplateJob"}, {Ref: "Board"},
	}}
	if problems := Resolve(root, "", idx, &dup); len(problems) > 0 {
		t.Fatal(problems)
	}
	overlaps, err := Overlaps(db, dup)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlaps) != 1 || overlaps[0].Name != "board templates" || overlaps[0].Shared != 3 || overlaps[0].Of != 4 {
		t.Errorf("3 of 4 files already in board templates should be flagged: %+v", overlaps)
	}
	if left := Distinct(overlaps, []string{"Board Templates"}); len(left) != 0 {
		t.Errorf("distinct_from should clear the overlap it names: %+v", left)
	}

	related := Feature{Name: "boards", Summary: "s", Members: []Member{
		{Ref: "Board"}, {Ref: "BoardTemplate"}, {Ref: "Admin::Template"}, {Ref: "Report::Template"}, {Ref: "app/models/user.rb"},
	}}
	write(t, root, "app/models/user.rb", "class User\nend\n")
	if problems := Resolve(root, "", idx, &related); len(problems) > 0 {
		t.Fatal(problems)
	}
	if overlaps, _ := Overlaps(db, related); len(overlaps) != 0 {
		t.Errorf("a feature sharing 1 of 5 files is related, not a duplicate: %+v", overlaps)
	}

	// Re-saving under its own name is never a duplicate of itself.
	same := boardTemplates()
	if problems := Resolve(root, "", idx, &same); len(problems) > 0 {
		t.Fatal(problems)
	}
	if overlaps, _ := Overlaps(db, same); len(overlaps) != 0 {
		t.Errorf("a feature overlapped itself: %+v", overlaps)
	}
}

func TestTouchpoints_SaysWhenAMapWasSavedOnAnotherBranch(t *testing.T) {
	root, idx := project(t)
	wt := withWorktree(t, root)
	write(t, root, ".git/HEAD", "ref: refs/heads/dev\n")
	write(t, root, ".git/worktrees/wt/HEAD", "ref: refs/heads/huy/bar-chart\n")
	db := open_(t, root)

	f := barChart()
	f.Branch = "huy/bar-chart"
	if problems := Resolve(root, wt, idx, &f); len(problems) > 0 {
		t.Fatal(problems)
	}
	if _, err := Record(db, f); err != nil {
		t.Fatal(err)
	}

	fromDev, _ := Touchpoints(db, root, root, idx, "bar chart", 1)
	if !strings.Contains(fromDev, "matches branch huy/bar-chart") || !strings.Contains(fromDev, "unmerged work, not later edits") {
		t.Errorf("read from dev, the map should say its differences are the branch's work:\n%s", fromDev)
	}
	if fromBranch, _ := Touchpoints(db, root, wt, idx, "bar chart", 1); strings.Contains(fromBranch, "matches branch") {
		t.Errorf("read from its own branch, the map should not mention branches:\n%s", fromBranch)
	}

	summaries, _ := Summaries(db, root, root)
	if len(summaries) != 1 || summaries[0].OtherBranch != "huy/bar-chart" || summaries[0].Changed == 0 {
		t.Errorf("the summary read from dev should carry the other branch: %+v", summaries)
	}
}

func TestOpen_AddsTheBranchColumnToAVersionFourGraph(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())
	if _, err := db.Exec("ALTER TABLE features DROP COLUMN branch; PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	upgraded, err := Open(root)
	if err != nil {
		t.Fatalf("opening a version 4 graph: %v", err)
	}
	defer func() { _ = upgraded.Close() }()

	f := boardTemplates()
	f.Branch = "dev"
	if problems := Resolve(root, "", idx, &f); len(problems) > 0 {
		t.Fatal(problems)
	}
	if _, err := Record(upgraded, f); err != nil {
		t.Errorf("saving after the upgrade: %v", err)
	}
}

func TestInferBranches_FindsWhereAnOlderMapWasSaved(t *testing.T) {
	root, idx := project(t)
	wt := withWorktree(t, root)
	write(t, root, ".git/HEAD", "ref: refs/heads/dev\n")
	write(t, root, ".git/worktrees/wt/HEAD", "ref: refs/heads/huy/bar-chart\n")
	db := open_(t, root)

	f := barChart() // no Branch, as every map saved before it was recorded
	if problems := Resolve(root, wt, idx, &f); len(problems) > 0 {
		t.Fatal(problems)
	}
	if _, err := Record(db, f); err != nil {
		t.Fatal(err)
	}
	saved(t, db, root, idx, boardTemplates()) // saved from dev's files

	n, err := InferBranches(db, root)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected both maps' branches filled in, got %d", n)
	}
	got := map[string]string{}
	rows, _ := db.Query("SELECT name, branch FROM features")
	for rows.Next() {
		var name, branch string
		_ = rows.Scan(&name, &branch)
		got[name] = branch
	}
	_ = rows.Close()
	if got["bar chart"] != "huy/bar-chart" || got["board templates"] != "dev" {
		t.Errorf("branches = %v", got)
	}
	if again, _ := InferBranches(db, root); again != 0 {
		t.Errorf("a settled map should not be revisited, got %d", again)
	}
}

func TestRecord_KeepsOnlyTheLastThreeVersions(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)

	for i := 0; i < 6; i++ {
		f := boardTemplates()
		f.Summary = fmt.Sprintf("version %d", i)
		saved(t, db, root, idx, f)
	}

	rows, err := db.Query("SELECT snapshot FROM feature_history WHERE name = 'board templates' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var kept []string
	for rows.Next() {
		var snap string
		_ = rows.Scan(&snap)
		for v := 0; v < 6; v++ {
			if strings.Contains(snap, fmt.Sprintf(`"Summary":"version %d"`, v)) {
				kept = append(kept, fmt.Sprint(v))
			}
		}
	}
	// Six saves replace five versions (0–4); the last three of those stay.
	if strings.Join(kept, ",") != "2,3,4" {
		t.Errorf("history kept versions %v, want 2,3,4", kept)
	}
}
