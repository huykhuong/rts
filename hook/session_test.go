package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rts/index"
)

// indexedProject builds a temp project with a written map, which is the
// precondition for a brief existing at all.
func indexedProject(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeProjectFile(t, root, "Gemfile", "source 'x'\n")
	writeProjectFile(t, root, "config/application.rb", "module App; end\n")
	writeProjectFile(t, root, "app/models/user.rb", "class User\nend\n")

	pm, err := index.BuildMap(root, []index.Symbol{
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
	})
	if err != nil {
		t.Fatalf("BuildMap: %v", err)
	}
	if err := index.WriteMap(root, pm); err != nil {
		t.Fatalf("WriteMap: %v", err)
	}

	return root
}

func writeProjectFile(t *testing.T, root, rel, content string) {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRun_SessionStartEmitsAdditionalContext(t *testing.T) {
	root := indexedProject(t)

	payload := map[string]string{
		"hook_event_name": "SessionStart",
		"source":          "startup",
		"cwd":             root,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out bytes.Buffer
	if err := Run(bytes.NewReader(raw), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var decoded struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("hook output is not valid JSON (%v): %s", err, out.String())
	}

	if decoded.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart",
			decoded.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(decoded.HookSpecificOutput.AdditionalContext, "rts project map") {
		t.Errorf("additionalContext is not the brief:\n%s",
			decoded.HookSpecificOutput.AdditionalContext)
	}

	// A deny decision on a SessionStart payload would block the session.
	if strings.Contains(out.String(), "permissionDecision") {
		t.Errorf("SessionStart must never emit a permission decision: %s", out.String())
	}
}

func TestSessionContext_SkipsResumeOnly(t *testing.T) {
	root := indexedProject(t)

	tests := []struct {
		source string
		want   bool // want a brief
	}{
		{"startup", true},
		{"clear", true},
		// A compact drops the map out of the surviving summary, which is
		// exactly the moment a session starts re-deriving the codebase.
		{"compact", true},
		// A resume brings the transcript back, brief and all — a second copy
		// pays twice and puts a stale map under a fresh one.
		{"resume", false},
		{"something-new", false},
	}

	for _, tc := range tests {
		t.Run(tc.source, func(t *testing.T) {
			got := SessionContext(
				PreToolUseInput{HookEventName: "SessionStart", Source: tc.source, Cwd: root},
				time.Now(),
			)
			if (got != "") != tc.want {
				t.Errorf("source %q: brief emitted = %v, want %v", tc.source, got != "", tc.want)
			}
		})
	}
}

func TestSessionContext_ResumeWithoutABriefGetsOne(t *testing.T) {
	// A session begun before RTS's hooks were wired into the project only
	// gets them by being restarted, which is a resume — and its transcript
	// holds no brief to bring back.
	root := indexedProject(t)
	dir := t.TempDir()

	without := filepath.Join(dir, "old.jsonl")
	if err := os.WriteFile(without, []byte(`{"type":"user","message":{"content":"hello"}}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	with := filepath.Join(dir, "briefed.jsonl")
	if err := os.WriteFile(with, []byte(`{"type":"attachment","content":"=== rts project map: app"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	resume := func(transcript string) string {
		return SessionContext(PreToolUseInput{HookEventName: "SessionStart", Source: "resume", Cwd: root, TranscriptPath: transcript}, time.Now())
	}
	if resume(without) == "" {
		t.Error("a resumed session that never had a brief should get one")
	}
	if got := resume(with); got != "" {
		t.Errorf("a resumed session that already has its brief got a second:\n%s", got)
	}
}

func TestSessionContext_KillSwitchSilencesIt(t *testing.T) {
	// When RTS is disabled every tool refuses to run, so the brief — which
	// tells the model to reach for those tools — must stand down too.
	t.Setenv("RTS_DISABLE", "1")

	got := SessionContext(
		PreToolUseInput{HookEventName: "SessionStart", Source: "startup", Cwd: indexedProject(t)},
		time.Now(),
	)
	if got != "" {
		t.Errorf("RTS_DISABLE=1 still produced a brief:\n%s", got)
	}
}

func TestRun_SessionStartInAnUnindexedProjectIsSilent(t *testing.T) {
	raw := []byte(`{"hook_event_name":"SessionStart","source":"startup","cwd":"` + t.TempDir() + `"}`)

	var out bytes.Buffer
	if err := Run(bytes.NewReader(raw), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("unindexed project produced output: %s", out.String())
	}
}

func TestRun_PreToolUseStillDeniesAfterTheSessionStartBranch(t *testing.T) {
	// Guards the routing: adding SessionStart must not have shadowed the
	// guardrail the hook was installed for in the first place.
	raw := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"/tmp/proj",` +
		`"tool_input":{"command":"cat app/models/user.rb"}}`)

	var out bytes.Buffer
	if err := Run(bytes.NewReader(raw), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out.String(), `"deny"`) {
		t.Errorf("Bash cat was not denied: %s", out.String())
	}
	if !strings.Contains(out.String(), "PreToolUse") {
		t.Errorf("deny output lost its event name: %s", out.String())
	}
}
