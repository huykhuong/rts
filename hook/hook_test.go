package hook

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// --- isUnderCwd: path resolution ---

func TestIsUnderCwd_RelativePathInsideProject(t *testing.T) {
	if !isUnderCwd("app/models/user.rb", "/proj") {
		t.Error("relative path under cwd should be flagged as inside")
	}
}

func TestIsUnderCwd_AbsolutePathInsideProject(t *testing.T) {
	if !isUnderCwd("/proj/app/models/user.rb", "/proj") {
		t.Error("absolute path under cwd should be flagged as inside")
	}
}

func TestIsUnderCwd_AbsolutePathOutsideProject(t *testing.T) {
	if isUnderCwd("/etc/hosts", "/proj") {
		t.Error("system file should not be flagged as inside project")
	}
}

func TestIsUnderCwd_SimilarPrefixNotMatched(t *testing.T) {
	// Classic bug: naive HasPrefix would match "/proj-other" against "/proj".
	// The trailing-separator check should catch this.
	if isUnderCwd("/proj-other/file.rb", "/proj") {
		t.Error("directory with shared prefix should NOT be flagged as inside")
	}
}

func TestIsUnderCwd_EmptyInputs(t *testing.T) {
	if isUnderCwd("", "/proj") {
		t.Error("empty path should return false")
	}
	if isUnderCwd("foo", "") {
		t.Error("empty cwd should return false")
	}
}

// --- Decide: Read tool cases ---

// preToolInput is a small helper that builds a PreToolUseInput with the
// given tool_input already JSON-encoded, matching how Claude Code sends it.
func preToolInput(t *testing.T, toolName string, toolInput interface{}, cwd string) PreToolUseInput {
	t.Helper()
	raw, err := json.Marshal(toolInput)
	if err != nil {
		t.Fatalf("marshal tool_input: %v", err)
	}
	return PreToolUseInput{
		ToolName:  toolName,
		Cwd:       cwd,
		ToolInput: raw,
	}
}

func TestDecide_ReadIsAlwaysAllowed(t *testing.T) {
	cases := []struct {
			name     string
			filePath string
	}{
			{"project file", "app/models/user.rb"},
			{"system file", "/etc/hosts"},
			{"home dotfile", "/home/user/.zshrc"},
	}
	for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
					input := preToolInput(t, "Read",
							map[string]string{"file_path": tc.filePath},
							"/proj",
					)
					if reason := Decide(input); reason != "" {
							t.Errorf("Read on %s should be allowed, got deny: %s", tc.filePath, reason)
					}
			})
	}
}

// --- Decide: Bash tool cases ---

func TestDecide_BashBlocksCatOnProjectFile(t *testing.T) {
	input := preToolInput(t, "Bash",
		map[string]string{"command": "cat app/models/user.rb"},
		"/proj",
	)
	if reason := Decide(input); reason == "" {
		t.Error("bash cat on project file should be blocked")
	}
}

func TestDecide_BashBlocksHeadWithFlag(t *testing.T) {
	// Flag arguments (-5) must not be mistaken for paths.
	input := preToolInput(t, "Bash",
		map[string]string{"command": "head -5 app/models/user.rb"},
		"/proj",
	)
	if reason := Decide(input); reason == "" {
		t.Error("head -5 on project file should still be blocked")
	}
}

func TestDecide_BashBlocksSedRange(t *testing.T) {
	input := preToolInput(t, "Bash",
		map[string]string{"command": "sed -n '127,131p' app/models/content_element.rb"},
		"/proj",
	)
	if reason := Decide(input); reason == "" {
		t.Error("sed range read on project file should be blocked")
	}
}

func TestDecide_BashBlocksAliasBypass(t *testing.T) {
	// Leading backslash bypasses shell aliases; we still catch it.
	input := preToolInput(t, "Bash",
		map[string]string{"command": `\cat app/models/user.rb`},
		"/proj",
	)
	if reason := Decide(input); reason == "" {
		t.Error("backslash-prefixed cat should still be blocked")
	}
}

func TestDecide_BashBlocksInPipeline(t *testing.T) {
	// Second segment of pipeline reads a project file.
	input := preToolInput(t, "Bash",
		map[string]string{"command": "echo hi && cat app/models/user.rb"},
		"/proj",
	)
	if reason := Decide(input); reason == "" {
		t.Error("cat in pipeline should be blocked")
	}
}

func TestDecide_BashAllowsCatOnSystemFile(t *testing.T) {
	input := preToolInput(t, "Bash",
		map[string]string{"command": "cat /etc/hosts"},
		"/proj",
	)
	if reason := Decide(input); reason != "" {
		t.Errorf("cat /etc/hosts should be allowed, got deny: %s", reason)
	}
}

func TestDecide_BashAllowsNonReadingCommands(t *testing.T) {
	// Any command that isn't a reading verb should pass through untouched.
	// These are exactly the kinds of Bash commands Claude Code legitimately
	// needs — running tests, git, bundle, etc.
	commands := []string{
		"bundle install",
		"rspec spec/models/user_spec.rb",
		"git status",
		"rails db:migrate",
		"echo hello",
		"ls app/models",
		"grep -r foo app/",
		"find . -name '*.rb'",
	}
	for _, cmd := range commands {
		input := preToolInput(t, "Bash",
			map[string]string{"command": cmd},
			"/proj",
		)
		if reason := Decide(input); reason != "" {
			t.Errorf("command %q should be allowed, got deny: %s", cmd, reason)
		}
	}
}

func TestDecide_BashAllowsCatWithNoPathArgs(t *testing.T) {
	// `cat` on its own (or with only flags) reads stdin — no file to protect.
	input := preToolInput(t, "Bash",
		map[string]string{"command": "cat -n"},
		"/proj",
	)
	if reason := Decide(input); reason != "" {
		t.Errorf("cat with no path args should be allowed, got deny: %s", reason)
	}
}

func TestDecide_BashAllowsHomeRelativePath(t *testing.T) {
	// Reading ~/.zshrc is outside the project — allow.
	input := preToolInput(t, "Bash",
		map[string]string{"command": "cat ~/.zshrc"},
		"/proj",
	)
	if reason := Decide(input); reason != "" {
		t.Errorf("cat ~/.zshrc should be allowed, got deny: %s", reason)
	}
}

// --- Decide: other tools untouched ---

func TestDecide_OtherToolsAlwaysAllowed(t *testing.T) {
	// The hook only intercepts Read and Bash. Everything else — Edit, Grep,
	// Glob, the RTS MCP tools themselves — passes through untouched.
	tools := []string{"Edit", "Write", "Grep", "Glob", "mcp__rts__view_file"}
	for _, name := range tools {
		input := preToolInput(t, name,
			map[string]string{"file_path": "app/models/user.rb"},
			"/proj",
		)
		if reason := Decide(input); reason != "" {
			t.Errorf("tool %q should not be intercepted, got deny: %s", name, reason)
		}
	}
}

// --- Decide: kill switch ---

func TestDecide_KillSwitchAllowsEverything(t *testing.T) {
	// With RTS_DISABLE=1 the MCP tools all refuse to run, so the hook must
	// stop blocking too — otherwise there's no way left to read a file.
	t.Setenv("RTS_DISABLE", "1")

	input := preToolInput(t, "Bash",
		map[string]string{"command": "cat app/models/user.rb"},
		"/proj",
	)
	if reason := Decide(input); reason != "" {
		t.Errorf("with RTS_DISABLE=1 everything should be allowed, got deny: %s", reason)
	}
}

func TestDecide_KillSwitchOffStillBlocks(t *testing.T) {
	// Any value other than "1" leaves normal behavior intact.
	t.Setenv("RTS_DISABLE", "0")

	input := preToolInput(t, "Bash",
		map[string]string{"command": "cat app/models/user.rb"},
		"/proj",
	)
	if reason := Decide(input); reason == "" {
		t.Error("RTS_DISABLE=0 should not disable blocking")
	}
}

// --- Decide: fail-open on malformed input ---

func TestDecide_MalformedToolInputFailsOpen(t *testing.T) {
	// If tool_input can't be parsed, we allow rather than block.
	// Broken hooks must not break Claude Code.
	input := PreToolUseInput{
		ToolName:  "Read",
		Cwd:       "/proj",
		ToolInput: json.RawMessage(`{"file_path": 123}`), // wrong type
	}
	if reason := Decide(input); reason != "" {
		t.Errorf("malformed tool_input should fail open, got deny: %s", reason)
	}
}

// --- Run: end-to-end stdin→stdout ---

func TestRun_BlockedProducesDenyJSON(t *testing.T) {
	// Bash reads are the blocked case; native Read is deliberately allowed
	// (Edit/Write require a prior native Read as a safety precondition).
	stdin := bytes.NewBufferString(`{
		"tool_name": "Bash",
		"cwd": "/proj",
		"tool_input": {"command": "cat app/models/user.rb"}
	}`)
	var stdout bytes.Buffer

	if err := Run(stdin, &stdout); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("blocked run should emit deny JSON, got: %s", out)
	}
	if !strings.Contains(out, `"hookEventName":"PreToolUse"`) {
		t.Errorf("output should carry hookEventName, got: %s", out)
	}
	if !strings.Contains(out, "view_file") {
		t.Errorf("deny reason should mention view_file, got: %s", out)
	}
}

func TestRun_AllowedProducesEmptyOutput(t *testing.T) {
	stdin := bytes.NewBufferString(`{
		"tool_name": "Read",
		"cwd": "/proj",
		"tool_input": {"file_path": "/etc/hosts"}
	}`)
	var stdout bytes.Buffer

	if err := Run(stdin, &stdout); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if stdout.Len() != 0 {
		t.Errorf("allowed run should emit no output, got: %s", stdout.String())
	}
}

func TestRun_MalformedStdinFailsOpen(t *testing.T) {
	stdin := bytes.NewBufferString(`{not valid json`)
	var stdout bytes.Buffer

	if err := Run(stdin, &stdout); err != nil {
		t.Fatalf("Run should fail open on malformed input, got error: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("malformed input should produce no output (fail open), got: %s", stdout.String())
	}
}