package graph

import (
	"database/sql"
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

	if problems := Resolve(root, idx, &f); len(problems) > 0 {
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

	if problems := Resolve(root, idx, &f); len(problems) > 0 {
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

	problems := Resolve(root, idx, &f)
	joined := strings.Join(problems, "\n")

	for _, want := range []string{
		`"BoardTemplat": no class or module`,
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
	if problems := Resolve(root, idx, &f); !strings.Contains(strings.Join(problems, "\n"), "protected credential file") {
		t.Errorf("config/master.key was accepted as a member: %v", problems)
	}

	f = boardTemplates()
	f.Notes = []string{"Signing uses the key in config/master.key."}
	if problems := Resolve(root, idx, &f); !strings.Contains(strings.Join(problems, "\n"), `names "config/master.key"`) {
		t.Errorf("a note naming config/master.key was accepted: %v", problems)
	}

	f = boardTemplates()
	f.Notes = []string{"Reads ENV vars for the S3 bucket, e.g. in production."}
	if problems := Resolve(root, idx, &f); len(problems) > 0 {
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

	text, err := Touchpoints(db, root, idx, "Board Templates", 1)
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

	text, err := Touchpoints(db, root, idx, "board templates", 1)
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

	one, err := Touchpoints(db, root, idx, "BoardTemplatesController", 1)
	if err != nil {
		t.Fatalf("Touchpoints: %v", err)
	}
	if !strings.Contains(one, "Part of: board templates") || !strings.Contains(one, "BoardTemplatesController uses → BoardTemplate") {
		t.Errorf("depth 1 should give the feature and the direct edge:\n%s", one)
	}
	if strings.Contains(one, "Admin::Template") {
		t.Errorf("depth 1 went two steps out:\n%s", one)
	}

	two, err := Touchpoints(db, root, idx, "BoardTemplate", 2)
	if err != nil {
		t.Fatalf("Touchpoints: %v", err)
	}
	if !strings.Contains(two, "Board uses → Admin::Template") {
		t.Errorf("depth 2 should reach across into the boards map:\n%s", two)
	}
}

func TestTouchpoints_MissNamesWhatIsMapped(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())

	text, err := Touchpoints(db, root, idx, "billing", 1)
	if err != nil {
		t.Fatalf("Touchpoints: %v", err)
	}
	if !strings.Contains(text, `Nothing mapped for "billing"`) || !strings.Contains(text, "Mapped features: board templates") {
		t.Errorf("a miss should list what is mapped:\n%s", text)
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
	if problems := Resolve(root, idx, &rewrite); len(problems) > 0 {
		t.Fatalf("Resolve: %v", problems)
	}
	replaced, err := Record(db, rewrite)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !replaced {
		t.Error("rewriting an existing feature was not reported as a replacement")
	}

	text, _ := Touchpoints(db, root, idx, "board templates", 1)
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
			if problems := Resolve(root, idx, &f); len(problems) > 0 {
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
	features, err := Summaries(db, root)
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

	features, _ := Summaries(db, root)
	if len(features) != 1 || features[0].Name != "board templates" || features[0].Members != 3 {
		t.Errorf("board templates should survive with 3 members; got %+v", features)
	}
}

func TestOpen_UpgradesAVersionOneGraphInPlace(t *testing.T) {
	root, idx := project(t)
	db := open_(t, root)
	saved(t, db, root, idx, boardTemplates())

	// Turn it back into what version 1 wrote: no stop_asks table.
	if _, err := db.Exec("DROP TABLE stop_asks; PRAGMA user_version = 1"); err != nil {
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
	if features, _ := Summaries(upgraded, root); len(features) != 1 {
		t.Errorf("the upgrade lost the feature maps: %+v", features)
	}
}
