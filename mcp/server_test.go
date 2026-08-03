package mcp

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout swaps os.Stdout for a pipe while fn runs and returns
// everything fn printed. The handlers write JSON-RPC responses straight to
// stdout via fmt.Println, so this is the only way to observe them.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

// toolCall builds a tools/call JSON-RPC request for the given tool.
func toolCall(t *testing.T, name string, args map[string]interface{}) []byte {
	t.Helper()

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      name,
			"arguments": args,
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

// --- kill switch ---

func TestHandleToolsCall_KillSwitchDisablesEveryTool(t *testing.T) {
	// RTS_DISABLE=1 must short-circuit before any dispatch, for every tool —
	// including the ones (get_stats, find_symbol) that are routed early.
	t.Setenv("RTS_DISABLE", "1")

	tools := []string{
		"view_file",
		"view_bionic_skeleton",
		"view_bionic_method",
		"find_symbol",
		"search_text",
		"list_directory",
		"get_stats",
	}

	for _, name := range tools {
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, func() {
				handleToolsCall(
					toolCall(t, name, map[string]interface{}{"path": "app/models/user.rb"}),
					json.RawMessage("1"),
					NewFileCache(),
					NewIndexCache("."),
					NewEventLogger("test-session"),
				)
			})

			if !strings.Contains(out, `"error"`) {
				t.Errorf("disabled %s should return a JSON-RPC error, got: %s", name, out)
			}
			if !strings.Contains(out, "RTS is disabled") {
				t.Errorf("error should say RTS is disabled, got: %s", out)
			}
			if !strings.Contains(out, "Read") || !strings.Contains(out, "Grep") {
				t.Errorf("error should name the native alternatives, got: %s", out)
			}
			if !strings.Contains(out, name) {
				t.Errorf("error should name the tool that was called, got: %s", out)
			}
		})
	}
}

func TestHandleToolsCall_KillSwitchOffDispatchesNormally(t *testing.T) {
	// Any value other than "1" leaves RTS fully operational. We use an
	// unknown tool name so no real file I/O happens — reaching the
	// "Unknown tool" error proves dispatch ran instead of the kill switch.
	t.Setenv("RTS_DISABLE", "0")

	out := captureStdout(t, func() {
		handleToolsCall(
			toolCall(t, "definitely_not_a_tool", map[string]interface{}{}),
			json.RawMessage("1"),
			NewFileCache(),
			NewIndexCache("."),
			NewEventLogger("test-session"),
		)
	})

	if strings.Contains(out, "RTS is disabled") {
		t.Errorf("RTS_DISABLE=0 should not disable RTS, got: %s", out)
	}
}

// --- formatMethodResults: fuzzy suggestions on miss ---

// suggestionSource is a small Ruby file used across the suggestion tests.
var suggestionSource = []byte(`class Report
  def height
    @height
  end

  def width
    @width
  end

  def calculate_metrics
    {}
  end
end
`)

func TestFormatMethodResults_NoMissesHasNoSuggestions(t *testing.T) {
	out := formatMethodResults(
		[]string{"height"},
		map[string]string{"height": "def height\n  @height\nend\n"},
		suggestionSource,
	)

	if strings.Contains(out, "not found") {
		t.Errorf("all-found result should not report misses, got:\n%s", out)
	}
	if strings.Contains(out, "did you mean") {
		t.Errorf("all-found result should not suggest anything, got:\n%s", out)
	}
	if !strings.Contains(out, "# === method: height ===") {
		t.Errorf("result should include the method header, got:\n%s", out)
	}
}

func TestFormatMethodResults_MissSuggestsCloseName(t *testing.T) {
	// "calculate" is a prefix of the real "calculate_metrics".
	out := formatMethodResults([]string{"calculate"}, map[string]string{}, suggestionSource)

	if !strings.Contains(out, "# === not found: calculate ===") {
		t.Errorf("should report the miss, got:\n%s", out)
	}
	if !strings.Contains(out, "did you mean: calculate_metrics?") {
		t.Errorf("should suggest the close name, got:\n%s", out)
	}
}

func TestFormatMethodResults_MissFallsBackToAvailableNames(t *testing.T) {
	// The roadmap's acceptance case: request "dimensions" against a file
	// whose real methods are height/width and get the real names back.
	out := formatMethodResults([]string{"dimensions"}, map[string]string{}, suggestionSource)

	if !strings.Contains(out, "# === not found: dimensions ===") {
		t.Errorf("should report the miss, got:\n%s", out)
	}
	if !strings.Contains(out, "did you mean one of:") {
		t.Errorf("should offer the available names, got:\n%s", out)
	}
	for _, name := range []string{"height", "width", "calculate_metrics"} {
		if !strings.Contains(out, name) {
			t.Errorf("fallback should list %q, got:\n%s", name, out)
		}
	}
}

func TestFormatMethodResults_PartialMissKeepsFoundBodies(t *testing.T) {
	out := formatMethodResults(
		[]string{"height", "widht"}, // second is a typo
		map[string]string{"height": "def height\n  @height\nend\n"},
		suggestionSource,
	)

	if !strings.Contains(out, "# === method: height ===") {
		t.Errorf("found method should still be rendered, got:\n%s", out)
	}
	if !strings.Contains(out, "# === not found: widht ===") {
		t.Errorf("missing method should be reported, got:\n%s", out)
	}
	if !strings.Contains(out, "width") {
		t.Errorf("typo should surface the real name, got:\n%s", out)
	}
}

func TestFormatMethodResults_EmptySourceStillReportsMiss(t *testing.T) {
	// No candidates to suggest — the not-found report must survive intact
	// rather than being dropped along with the suggestions.
	out := formatMethodResults([]string{"whatever"}, map[string]string{}, []byte(""))

	if !strings.Contains(out, "# === not found: whatever ===") {
		t.Errorf("miss must be reported even with no candidates, got:\n%s", out)
	}
	if strings.Contains(out, "did you mean") {
		t.Errorf("no candidates should mean no suggestion line, got:\n%s", out)
	}
}

func TestFormatMethodResults_SuggestsPerMissingName(t *testing.T) {
	out := formatMethodResults(
		[]string{"heigth", "widht"},
		map[string]string{},
		suggestionSource,
	)

	if !strings.Contains(out, "# === not found: heigth, widht ===") {
		t.Errorf("both misses should be listed, got:\n%s", out)
	}
	// Each miss gets its own suggestion line.
	if strings.Count(out, "did you mean") != 2 {
		t.Errorf("expected one suggestion line per miss, got:\n%s", out)
	}
}
