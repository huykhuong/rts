package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rts/graph"
	"rts/index"
)

func TestAreaOf_GroupsAFeatureAcrossRailsFolders(t *testing.T) {
	for path, want := range map[string]string{
		"app/models/board_template.rb":                  "board_template",
		"app/controllers/board_templates_controller.rb": "board_template",
		"app/views/board_templates/index.html.erb":      "board_template",
		"app/views/board_templates/_form.html.erb":      "board_template",
		"test/models/board_template_test.rb":            "board_template",
		"spec/requests/board_templates_spec.rb":         "board_template",
		"app/jobs/create_board_from_template_job.rb":    "create_board_from_template",
		"app/prompts/user/html/radar.md":                "radar",
		"app/services/html_audit.rb":                    "html_audit",
		"app/models/category.rb":                        "category",
		"app/models/categories/address.rb":              "address",
		"app/views/companies/show.html.erb":             "company",
		"app/services/status.rb":                        "status",
		"app/views/shared/index.html.erb":               "",
	} {
		if got := areaOf(path); got != want {
			t.Errorf("areaOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestProjectRelative_KeepsOnlyMappableProjectFiles(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
		ok   bool
	}{
		{"/proj/app/models/user.rb", "app/models/user.rb", true},
		{"app/models/user.rb", "app/models/user.rb", true},
		{"/proj/.claude/plans/x.md", "", false},
		{"/proj/node_modules/x/index.js", "", false},
		{"/elsewhere/user.rb", "", false},
		{"/proj", "", false},
	} {
		got, ok := projectRelative(tc.path, "/proj", "/proj")
		if got != tc.want || ok != tc.ok {
			t.Errorf("projectRelative(%q) = %q, %v; want %q, %v", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

// stopProject is an indexed project and a transcript of a session in it.
type stopProject struct {
	root       string
	transcript string
}

func newStopProject(t *testing.T) stopProject {
	t.Helper()

	root := t.TempDir()
	files := []string{
		"app/models/board_template.rb",
		"app/controllers/board_templates_controller.rb",
		"app/views/board_templates/index.html.erb",
		"app/views/board_templates/show.html.erb",
		"test/models/board_template_test.rb",
		"app/models/user.rb",
		"app/models/account.rb",
	}
	for _, f := range files {
		writeFile(t, filepath.Join(root, f), "class X\nend\n")
	}
	if err := index.Write(root, []index.Symbol{
		{Name: "BoardTemplate", Kind: "class", Path: "app/models/board_template.rb", Line: 1},
		{Name: "BoardTemplatesController", Kind: "class", Path: "app/controllers/board_templates_controller.rb", Line: 1},
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
	}); err != nil {
		t.Fatalf("index.Write: %v", err)
	}

	return stopProject{root: root, transcript: filepath.Join(t.TempDir(), "session.jsonl")}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// toolLine is one assistant transcript line calling a tool on a file.
func toolLine(tool, key, path string) string {
	line := map[string]interface{}{
		"type": "assistant",
		"message": map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "tool_use", "name": tool, "input": map[string]string{key: path}},
			},
		},
	}
	b, _ := json.Marshal(line)
	return string(b) + "\n"
}

func (p stopProject) session(t *testing.T, lines ...string) {
	t.Helper()
	writeFile(t, p.transcript, strings.Join(lines, ""))
}

func (p stopProject) stop(session string) string {
	return StopReason(PreToolUseInput{
		HookEventName:  "Stop",
		Cwd:            p.root,
		SessionID:      session,
		TranscriptPath: p.transcript,
	}, time.Now())
}

func (p stopProject) readsOfBoardTemplates() []string {
	return []string{
		toolLine("Read", "file_path", filepath.Join(p.root, "app/models/board_template.rb")),
		toolLine("mcp__rts__view_file", "path", "app/controllers/board_templates_controller.rb"),
		toolLine("Read", "file_path", filepath.Join(p.root, "app/views/board_templates/index.html.erb")),
		toolLine("mcp__rts__view_bionic_skeleton", "path", "app/views/board_templates/show.html.erb"),
		toolLine("Read", "file_path", filepath.Join(p.root, "test/models/board_template_test.rb")),
	}
}

func TestStopReason_AsksOnceForAnExploredArea(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := newStopProject(t)
	p.session(t, p.readsOfBoardTemplates()...)

	reason := p.stop("s1")
	if !strings.Contains(reason, "board_template — 5 file(s) read") || !strings.Contains(reason, "record_feature") {
		t.Fatalf("expected an ask naming the board_template area:\n%s", reason)
	}
	if again := p.stop("s1"); again != "" {
		t.Errorf("the same session was asked about the same area twice:\n%s", again)
	}
	if other := p.stop("s2"); other == "" {
		t.Error("a different session exploring the same area should be asked")
	}
}

func TestStopReason_AGlanceIsNotAnExploration(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := newStopProject(t)
	p.session(t, p.readsOfBoardTemplates()[:4]...)

	if reason := p.stop("s1"); reason != "" {
		t.Errorf("four files read should not prompt a map:\n%s", reason)
	}
}

func TestStopReason_AnyEditOutsideEveryMapAsks(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := newStopProject(t)
	p.session(t, toolLine("Edit", "file_path", filepath.Join(p.root, "app/models/account.rb")))

	if reason := p.stop("s1"); !strings.Contains(reason, "account — 1 file(s), some edited") {
		t.Errorf("an edit outside every map should prompt a map:\n%s", reason)
	}
}

func TestStopReason_CountsAnExploreAgentsReads(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := newStopProject(t)
	p.session(t, toolLine("Read", "file_path", filepath.Join(p.root, "app/models/board_template.rb")))

	sub := filepath.Join(strings.TrimSuffix(p.transcript, ".jsonl"), "subagents", "agent-1.jsonl")
	writeFile(t, sub, strings.Join(p.readsOfBoardTemplates()[1:], ""))

	if reason := p.stop("s1"); !strings.Contains(reason, "board_template — 5 file(s) read") {
		t.Errorf("reads made by a subagent should count toward the session's:\n%s", reason)
	}
}

func TestStopReason_StaysQuietWhenItShould(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")

	for _, tc := range []struct {
		name  string
		input func(p stopProject) PreToolUseInput
	}{
		{"stop_hook_active", func(p stopProject) PreToolUseInput {
			return PreToolUseInput{Cwd: p.root, SessionID: "s", TranscriptPath: p.transcript, StopHookActive: true}
		}},
		{"plan mode", func(p stopProject) PreToolUseInput {
			return PreToolUseInput{Cwd: p.root, SessionID: "s", TranscriptPath: p.transcript, PermissionMode: "plan"}
		}},
		{"no transcript", func(p stopProject) PreToolUseInput {
			return PreToolUseInput{Cwd: p.root, SessionID: "s"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newStopProject(t)
			p.session(t, p.readsOfBoardTemplates()...)
			if reason := StopReason(tc.input(p), time.Now()); reason != "" {
				t.Errorf("expected no ask, got:\n%s", reason)
			}
		})
	}

	t.Run("plan mode does not use up the ask", func(t *testing.T) {
		p := newStopProject(t)
		p.session(t, p.readsOfBoardTemplates()...)
		StopReason(PreToolUseInput{Cwd: p.root, SessionID: "s", TranscriptPath: p.transcript, PermissionMode: "plan"}, time.Now())
		if reason := p.stop("s"); reason == "" {
			t.Error("leaving plan mode should still get the ask")
		}
	})

	t.Run("unindexed project", func(t *testing.T) {
		p := newStopProject(t)
		p.session(t, p.readsOfBoardTemplates()...)
		_ = os.Remove(filepath.Join(p.root, ".rts", "symbols.json"))
		if reason := p.stop("s"); reason != "" {
			t.Errorf("record_feature cannot work without an index, so nothing should ask for it:\n%s", reason)
		}
	})

	t.Run("RTS_DISABLE", func(t *testing.T) {
		p := newStopProject(t)
		p.session(t, p.readsOfBoardTemplates()...)
		t.Setenv("RTS_DISABLE", "1")
		if reason := p.stop("s"); reason != "" {
			t.Errorf("RTS_DISABLE should silence the ask:\n%s", reason)
		}
	})
}

func TestStopReason_AnUpToDateMapCoversItsFiles(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := newStopProject(t)
	p.session(t, p.readsOfBoardTemplates()...)

	idx, err := index.Load(p.root)
	if err != nil {
		t.Fatal(err)
	}
	f := graph.Feature{
		Name:    "board templates",
		Summary: "Saved board layouts.",
		Members: []graph.Member{
			{Ref: "BoardTemplate"},
			{Ref: "BoardTemplatesController"},
			{Ref: "app/views/board_templates"},
			{Ref: "test/models/board_template_test.rb"},
		},
	}
	if problems := graph.Resolve(p.root, idx, &f); len(problems) > 0 {
		t.Fatalf("Resolve: %v", problems)
	}
	db, err := graph.Open(p.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Record(db, f); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if reason := p.stop("s1"); reason != "" {
		t.Errorf("a mapped, unchanged area should not be asked about:\n%s", reason)
	}

	// A session that edits a mapped file is asked even though the map still
	// matches the main checkout: in a worktree it changed its own copy.
	p.session(t, toolLine("Edit", "file_path", filepath.Join(p.root, "app/models/board_template.rb")))
	if reason := p.stop("s-edit"); !strings.Contains(reason, "board_template — 1 file(s), some edited") {
		t.Errorf("an edit to a mapped file should ask for the map again:\n%s", reason)
	}

	// Once a member changes, its map is the one that needs saving again.
	writeFile(t, filepath.Join(p.root, "app/models/board_template.rb"), "class BoardTemplate\n  def apply; end\nend\n")
	p.session(t, append(p.readsOfBoardTemplates(), toolLine("Edit", "file_path", filepath.Join(p.root, "app/models/board_template.rb")))...)
	if reason := p.stop("s2"); !strings.Contains(reason, "board_template") {
		t.Errorf("an edited member should bring the ask back:\n%s", reason)
	}
}

func TestRun_StopWritesABlockDecision(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := newStopProject(t)
	p.session(t, p.readsOfBoardTemplates()...)

	in := fmt.Sprintf(`{"hook_event_name":"Stop","cwd":%q,"session_id":"s1","transcript_path":%q}`, p.root, p.transcript)
	var out bytes.Buffer
	if err := Run(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}

	var decoded stopOutput
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("Stop output is not JSON: %v\n%s", err, out.String())
	}
	if decoded.Decision != "block" || !strings.Contains(decoded.Reason, "board_template") {
		t.Errorf("expected a block naming the area, got %+v", decoded)
	}
}

func TestAskText_SummarisesAreasPastTheLimit(t *testing.T) {
	var areas []area
	for i := 0; i < maxListedAreas+3; i++ {
		areas = append(areas, area{name: fmt.Sprintf("area%02d", i), files: []string{"f.rb"}, edited: true})
	}

	text := askText(areas)
	if strings.Count(text, "some edited") != maxListedAreas {
		t.Errorf("expected %d areas listed in full:\n%s", maxListedAreas, text)
	}
	if !strings.Contains(text, "and 3 smaller area(s): area08, area09, area10") {
		t.Errorf("the areas past the limit should be named in one line:\n%s", text)
	}
}
