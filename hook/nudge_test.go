package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rts/graph"
	"rts/index"
)

// mappedProject is a stopProject with a "board templates" map saved.
func mappedProject(t *testing.T) stopProject {
	t.Helper()
	p := newStopProject(t)

	idx, err := index.Load(p.root)
	if err != nil {
		t.Fatal(err)
	}
	f := graph.Feature{
		Name:    "board templates",
		Summary: "Saved board layouts.",
		Members: []graph.Member{{Ref: "BoardTemplate"}, {Ref: "BoardTemplatesController"}, {Ref: "app/views/board_templates"}},
	}
	if problems := graph.Resolve(p.root, "", idx, &f); len(problems) > 0 {
		t.Fatal(problems)
	}
	db, err := graph.Open(p.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Record(db, f); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	p.session(t) // an empty transcript
	return p
}

func (p stopProject) read(session, tool, key, path string) string {
	raw, _ := json.Marshal(map[string]string{key: path})
	return NudgeContext(PreToolUseInput{
		HookEventName:  "PostToolUse",
		ToolName:       tool,
		ToolInput:      raw,
		Cwd:            p.root,
		SessionID:      session,
		TranscriptPath: p.transcript,
	}, time.Now())
}

func TestNudge_PointsAMappedReadAtItsMapOnce(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := mappedProject(t)

	first := p.read("s1", "Read", "file_path", filepath.Join(p.root, "app/models/board_template.rb"))
	if !strings.Contains(first, `app/models/board_template.rb is part of mapped feature(s) "board templates" (3 files)`) ||
		!strings.Contains(first, "call touchpoints") {
		t.Fatalf("expected a nudge naming the feature:\n%s", first)
	}
	if again := p.read("s1", "mcp__rts__view_file", "path", "app/views/board_templates/index.html.erb"); again != "" {
		t.Errorf("the same feature was nudged twice in one session:\n%s", again)
	}
	if other := p.read("s2", "mcp__rts__view_bionic_skeleton", "path", "app/controllers/board_templates_controller.rb"); other == "" {
		t.Error("a different session should get its own nudge")
	}
}

func TestNudge_StaysQuietWhenItShould(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := mappedProject(t)
	member := filepath.Join(p.root, "app/models/board_template.rb")

	if got := p.read("s1", "Read", "file_path", filepath.Join(p.root, "app/models/user.rb")); got != "" {
		t.Errorf("a file in no map was nudged:\n%s", got)
	}
	if got := p.read("s1", "Grep", "path", member); got != "" {
		t.Errorf("a search is not a read and should not nudge:\n%s", got)
	}

	// The session already opened the map.
	p.session(t, toolLine("mcp__rts__touchpoints", "name", "Board Templates"))
	if got := p.read("s2", "Read", "file_path", member); got != "" {
		t.Errorf("a session that already called touchpoints for the feature was nudged:\n%s", got)
	}

	p.session(t)
	t.Setenv("RTS_DISABLE", "1")
	if got := p.read("s3", "Read", "file_path", member); got != "" {
		t.Errorf("RTS_DISABLE should silence the nudge:\n%s", got)
	}
}

func TestNudge_ATaskFileThatNamesAFeature(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := mappedProject(t)
	tasks := t.TempDir()

	for name, tc := range map[string]struct {
		text  string
		nudge bool
	}{
		"by feature name": {"Add a duplicate button to Board Templates.", true},
		"by class name":   {"BoardTemplatesController#create should accept a source board.", true},
		"by path":         {"Change app/models/board_template.rb so copies keep their order.", true},
		"unrelated":       {"Fix the login page footer.", false},
		"common word":     {"The board renders a template preview.", false},
	} {
		t.Run(name, func(t *testing.T) {
			task := filepath.Join(tasks, strings.ReplaceAll(name, " ", "-")+".md")
			writeFile(t, task, "# Task\n\n"+tc.text+"\n")
			got := p.read("s-"+name, "Read", "file_path", task)
			if (got != "") != tc.nudge {
				t.Errorf("nudge = %q, want nudge %v", got, tc.nudge)
			}
			if tc.nudge && !strings.Contains(got, filepath.Base(task)+" is part of mapped feature(s)") {
				t.Errorf("the nudge should name the task file:\n%s", got)
			}
		})
	}
}

func TestRun_PostToolUseAddsContext(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := mappedProject(t)

	in := fmt.Sprintf(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":%q},"cwd":%q,"session_id":"s1","transcript_path":%q}`,
		filepath.Join(p.root, "app/models/board_template.rb"), p.root, p.transcript)
	var out bytes.Buffer
	if err := Run(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}

	var decoded postToolOutput
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("PostToolUse output is not JSON: %v\n%s", err, out.String())
	}
	if decoded.HookSpecificOutput.HookEventName != "PostToolUse" || !strings.Contains(decoded.HookSpecificOutput.AdditionalContext, "board templates") {
		t.Errorf("unexpected output: %+v", decoded)
	}
}

func (p stopProject) prompt(session, text string) string {
	return PromptContext(PreToolUseInput{
		HookEventName:  "UserPromptSubmit",
		Prompt:         text,
		Cwd:            p.root,
		SessionID:      session,
		TranscriptPath: p.transcript,
	}, time.Now())
}

func TestPromptContext_NamesTheMapsATaskTouches(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := mappedProject(t)

	if got := p.prompt("s1", "Can you add a duplicate button to board templates?"); !strings.Contains(got, `this task touches mapped feature(s) "board templates"`) {
		t.Errorf("a message naming the feature should be pointed at it:\n%s", got)
	}
	if got := p.prompt("s2", "Please look at app/models/board_template.rb first."); got == "" {
		t.Error("a message naming a member file should be pointed at its feature")
	}
	if got := p.prompt("s3", "Fix the footer on the login page."); got != "" {
		t.Errorf("an unrelated message was nudged:\n%s", got)
	}
}

func TestPromptContext_FollowsATaskFile(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")
	p := mappedProject(t)

	task := filepath.Join(t.TempDir(), "dispatch", "worker-task.md")
	writeFile(t, task, "## Task\n\nBoardTemplatesController should copy the source board's layout.\n")

	got := p.prompt("w1", "Read "+task+" and follow it exactly.")
	if !strings.Contains(got, `"board templates"`) {
		t.Fatalf("a dispatch worker's first message should be pointed at the map its task file names:\n%s", got)
	}
	// The same session reading a member file later is not told again.
	if again := p.read("w1", "Read", "file_path", filepath.Join(p.root, "app/models/board_template.rb")); again != "" {
		t.Errorf("a feature already named at the prompt was nudged again on read:\n%s", again)
	}
}

func TestPathsIn_FindsPathsInPlainText(t *testing.T) {
	got := pathsIn("Read ~/.virtual-me/dispatch/x/worker-task.md, then app/models/user.rb and ./src/a.ts (not 'v1.2').")
	joined := strings.Join(got, " | ")
	for _, want := range []string{"/.virtual-me/dispatch/x/worker-task.md", "app/models/user.rb", "./src/a.ts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	if strings.Contains(joined, "~/") {
		t.Errorf("~ should be expanded to the home directory: %v", got)
	}
}
