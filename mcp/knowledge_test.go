package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rts/index"
	"rts/insight"
)

// knowledgeProject builds an indexed temp project for the knowledge tools.
func knowledgeProject(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeInto(t, root, "Gemfile", "source 'x'\n")
	writeInto(t, root, "config/application.rb", "module App; end\n")
	writeInto(t, root, "app/models/user.rb", "class User\nend\n")

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

func writeInto(t *testing.T, root, rel, content string) {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// resultText pulls the text payload out of a JSON-RPC tool response, failing
// the test if the response is an error instead.
func resultText(t *testing.T, out string) string {
	t.Helper()

	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, out)
	}
	if decoded.Error != nil {
		t.Fatalf("tool returned an error: %s", decoded.Error.Message)
	}
	if len(decoded.Result.Content) == 0 {
		t.Fatalf("response carried no content: %s", out)
	}
	return decoded.Result.Content[0].Text
}

func args(t *testing.T, kv map[string]interface{}) map[string]json.RawMessage {
	t.Helper()

	out := map[string]json.RawMessage{}
	for k, v := range kv {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", k, err)
		}
		out[k] = raw
	}
	return out
}

func TestHandleProjectMap_ReturnsTheBrief(t *testing.T) {
	root := knowledgeProject(t)

	out := captureStdout(t, func() {
		handleProjectMap(json.RawMessage("1"), root, NewEventLogger("test-session", root))
	})

	text := resultText(t, out)
	if !strings.Contains(text, "rts project map") {
		t.Errorf("project_map did not return the map:\n%s", text)
	}
	if !strings.Contains(text, "app/models") {
		t.Errorf("map is missing the layout:\n%s", text)
	}
}

func TestHandleProjectMap_UnindexedProjectExplainsTheFix(t *testing.T) {
	out := captureStdout(t, func() {
		root := t.TempDir()
		handleProjectMap(json.RawMessage("1"), root, NewEventLogger("test-session", root))
	})

	text := resultText(t, out)
	if !strings.Contains(text, "rts index") {
		t.Errorf("an unindexed project should name the fix:\n%s", text)
	}
}

func TestHandleRecordInsight_StoresAndConfirms(t *testing.T) {
	root := knowledgeProject(t)

	out := captureStdout(t, func() {
		handleRecordInsight(args(t, map[string]interface{}{
			"title":   "Users are soft-deleted, never destroyed",
			"body":    "Every destroy path sets deleted_at instead.",
			"paths":   []string{"app/models/user.rb"},
			"symbols": []string{"User"},
		}), json.RawMessage("1"), root, "test-session", NewEventLogger("test-session", root))
	})

	text := resultText(t, out)
	if !strings.Contains(text, "[i1]") {
		t.Errorf("confirmation must carry the id:\n%s", text)
	}
	if !strings.Contains(text, "next session") {
		t.Errorf("confirmation must say what recording achieved:\n%s", text)
	}

	store, err := insight.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(store.Insights) != 1 {
		t.Fatalf("store holds %d findings, want 1", len(store.Insights))
	}
	if store.Insights[0].Session != "test-session" {
		t.Errorf("session = %q, want the recording session", store.Insights[0].Session)
	}
	if len(store.Insights[0].Symbols) != 1 || store.Insights[0].Symbols[0] != "User" {
		t.Errorf("symbols = %v", store.Insights[0].Symbols)
	}
}

func TestHandleRecordInsight_LinksAreOptional(t *testing.T) {
	// A finding about the project as a whole names no file, and demanding one
	// would push the model into inventing a plausible path.
	root := knowledgeProject(t)

	out := captureStdout(t, func() {
		handleRecordInsight(args(t, map[string]interface{}{
			"title": "Migrations run through bin/deploy",
			"body":  "Never rails db:migrate directly.",
		}), json.RawMessage("1"), root, "test-session", NewEventLogger("test-session", root))
	})

	if text := resultText(t, out); !strings.Contains(text, "Recorded") {
		t.Errorf("a finding with no links should still record:\n%s", text)
	}
}

func TestHandleRecordInsight_SupersedeReplacesTheOldNote(t *testing.T) {
	root := knowledgeProject(t)
	logger := NewEventLogger("test-session", root)

	captureStdout(t, func() {
		handleRecordInsight(args(t, map[string]interface{}{
			"title": "Wrong", "body": "Not true.",
		}), json.RawMessage("1"), root, "test-session", logger)
	})

	out := captureStdout(t, func() {
		handleRecordInsight(args(t, map[string]interface{}{
			"title": "Right", "body": "Corrected.", "supersedes": "i1",
		}), json.RawMessage("2"), root, "test-session", logger)
	})

	if text := resultText(t, out); !strings.Contains(text, "Superseded and removed [i1]") {
		t.Errorf("supersede was not reported:\n%s", text)
	}

	store, _ := insight.Load(root)
	if len(store.Insights) != 1 || store.Insights[0].Title != "Right" {
		t.Errorf("store after supersede: %+v", store.Insights)
	}
}

func TestHandleRecordInsight_MissingRequiredArgsError(t *testing.T) {
	root := knowledgeProject(t)

	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"no title", map[string]interface{}{"body": "b"}},
		{"no body", map[string]interface{}{"title": "t"}},
		{"blank title", map[string]interface{}{"title": "  ", "body": "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				handleRecordInsight(args(t, tc.args), json.RawMessage("1"),
					root, "test-session", NewEventLogger("test-session", root))
			})

			if !strings.Contains(out, `"error"`) {
				t.Errorf("expected a JSON-RPC error, got: %s", out)
			}
		})
	}
}

func TestKnowledgeToolsAreAdvertised(t *testing.T) {
	// A handler nothing lists is a handler nothing calls.
	out := captureStdout(t, func() { handleToolsList(json.RawMessage("1")) })

	for _, name := range []string{"project_map", "record_insight", "touchpoints", "record_feature"} {
		if !strings.Contains(out, `"`+name+`"`) {
			t.Errorf("tools/list does not advertise %q", name)
		}
	}
}

func TestKnowledgeToolsRouteWithoutAPathArgument(t *testing.T) {
	// Both take no path. The path-taking branch treats a missing "path" as an
	// error, so a routing regression shows up here as "missing required
	// argument" rather than as a map.
	root := knowledgeProject(t)
	t.Setenv("RTS_DISABLE", "0")

	out := captureStdout(t, func() {
		handleToolsCall(
			toolCall(t, "project_map", map[string]interface{}{}),
			json.RawMessage("1"),
			root,
			"test-session",
			NewFileCache(),
			NewIndexCache(root),
			NewEventLogger("test-session", root),
		)
	})

	if strings.Contains(out, "missing required argument") {
		t.Fatalf("project_map fell through to the path-taking branch: %s", out)
	}
	if text := resultText(t, out); !strings.Contains(text, "rts project map") {
		t.Errorf("project_map did not return the map:\n%s", text)
	}
}

func TestKnowledgeToolsRespectTheKillSwitch(t *testing.T) {
	// RTS_DISABLE exists to give a clean "no RTS" baseline. A tool that kept
	// answering through it would quietly contaminate that measurement.
	root := knowledgeProject(t)
	t.Setenv("RTS_DISABLE", "1")

	for _, name := range []string{"project_map", "record_insight", "touchpoints", "record_feature"} {
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, func() {
				handleToolsCall(
					toolCall(t, name, map[string]interface{}{"title": "t", "body": "b"}),
					json.RawMessage("1"),
					root,
					"test-session",
					NewFileCache(),
					NewIndexCache(root),
					NewEventLogger("test-session", root),
				)
			})

			if !strings.Contains(out, "RTS is disabled") {
				t.Errorf("%s answered through the kill switch: %s", name, out)
			}
		})
	}
}

func TestStringListArg_AcceptsArraysAndDelimitedStrings(t *testing.T) {
	// A model that sends "a.rb, b.rb" instead of ["a.rb","b.rb"] used to get
	// every link dropped in silence: the finding recorded, apparently fine,
	// linked to nothing.
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"array", `["a.rb","b.rb"]`, []string{"a.rb", "b.rb"}},
		{"comma string", `"a.rb, b.rb"`, []string{"a.rb", "b.rb"}},
		{"semicolons and newlines", `"a.rb;b.rb\nc.rb"`, []string{"a.rb", "b.rb", "c.rb"}},
		{"single value", `"a.rb"`, []string{"a.rb"}},
		{"empty string", `""`, nil},
		{"wrong type entirely", `42`, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stringListArg(map[string]json.RawMessage{"paths": json.RawMessage(tc.raw)}, "paths")

			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}

	if got := stringListArg(map[string]json.RawMessage{}, "paths"); got != nil {
		t.Errorf("a missing argument should yield nil, got %v", got)
	}
}

func TestHandleRecordInsight_SupersedingAnUnknownIDSaysSo(t *testing.T) {
	root := knowledgeProject(t)

	out := captureStdout(t, func() {
		handleRecordInsight(args(t, map[string]interface{}{
			"title": "New", "body": "b", "supersedes": "i99",
		}), json.RawMessage("1"), root, "test-session", NewEventLogger("test-session", root))
	})

	text := resultText(t, out)
	if strings.Contains(text, "Superseded and removed") {
		t.Errorf("confirmed a supersede that never happened:\n%s", text)
	}
	if !strings.Contains(text, "No finding [i99] to supersede") {
		t.Errorf("did not report the failed supersede:\n%s", text)
	}
}

func TestHandleRecordInsight_AcceptsCommaSeparatedLinks(t *testing.T) {
	root := knowledgeProject(t)

	captureStdout(t, func() {
		handleRecordInsight(args(t, map[string]interface{}{
			"title": "t", "body": "b",
			"paths": "app/models/user.rb, config/application.rb",
		}), json.RawMessage("1"), root, "test-session", NewEventLogger("test-session", root))
	})

	store, err := insight.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(store.Insights) != 1 {
		t.Fatalf("store holds %d findings, want 1", len(store.Insights))
	}
	if len(store.Insights[0].Paths) != 2 {
		t.Errorf("paths = %v, want both files", store.Insights[0].Paths)
	}
}

// --- find_symbol result framing -----------------------------------------

func symbolIndex(symbols ...index.Symbol) *index.IndexFile {
	return &index.IndexFile{Version: 1, Symbols: symbols}
}

func TestFormatSymbolMatches_NamespaceMatchLeadsWithTheFind(t *testing.T) {
	// This read "No exact match for %q; found 4 symbol(s)..." and a real
	// session took four correct answers for a failure, then paid for
	// follow-up calls it did not need.
	matches := []index.Symbol{
		{Name: "PdfRequester", Kind: "class", Path: "app/models/concerns/report/pdf_requester.rb", Line: 1, Container: "Report"},
		{Name: "PdfRequester", Kind: "class", Path: "app/models/concerns/custom_report/pdf_requester.rb", Line: 1, Container: "CustomReport"},
	}

	got := formatSymbolMatches("PdfRequester", matches, "segment", symbolIndex(matches...))

	if strings.HasPrefix(got, "No exact match") {
		t.Errorf("a successful namespace match still leads with a negative:\n%s", got)
	}
	if !strings.Contains(got, "Found 2 symbol(s)") {
		t.Errorf("result does not lead with what it found:\n%s", got)
	}
	if !strings.Contains(got, "pdf_requester.rb") {
		t.Errorf("result lost the matches:\n%s", got)
	}
}

func TestFormatSymbolMatches_ExactMatchIsUnchanged(t *testing.T) {
	matches := []index.Symbol{
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
	}

	got := formatSymbolMatches("User", matches, "exact", symbolIndex(matches...))
	if !strings.Contains(got, `Found 1 symbol(s) named "User"`) {
		t.Errorf("exact-match wording changed:\n%s", got)
	}
}

func TestFormatSymbolMiss_OffersNearMissesInsteadOfBlamingStaleness(t *testing.T) {
	// A real session searched Projects::Reports::PdfDownloadsController,
	// which never existed under that name — the class is
	// ClientPortal::PdfDownloadsController. It was told to reindex, which
	// would not have helped, instead of shown the near-miss that would.
	idx := symbolIndex(
		index.Symbol{Name: "PdfDownloadsController", Kind: "class",
			Path: "app/controllers/client_portal/pdf_downloads_controller.rb", Line: 2, Container: "ClientPortal"},
		index.Symbol{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
	)

	got := formatSymbolMatches("Projects::Reports::PdfDownloadsController", nil, "", idx)

	if !strings.Contains(got, "Did you mean") {
		t.Errorf("no near-miss offered:\n%s", got)
	}
	if !strings.Contains(got, "ClientPortal::PdfDownloadsController") {
		t.Errorf("suggestion must be fully qualified so the correction is visible:\n%s", got)
	}
	if strings.Contains(got, "run `rts index`") {
		t.Errorf("a wrong-namespace guess should not be blamed on staleness:\n%s", got)
	}
}

func TestFormatSymbolMiss_TrueUnknownStillMentionsReindexing(t *testing.T) {
	idx := symbolIndex(index.Symbol{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1})

	got := formatSymbolMatches("CompletelyUnrelatedThing", nil, "", idx)

	if strings.Contains(got, "Did you mean") {
		t.Errorf("offered a suggestion for something with no near match:\n%s", got)
	}
	if !strings.Contains(got, "rts index") {
		t.Errorf("a genuine unknown should still offer reindexing as a possibility:\n%s", got)
	}
}

func TestSuggestSymbols_MatchesOnTheLastSegment(t *testing.T) {
	// A wrong guess is usually a wrong namespace around a right class name.
	idx := symbolIndex(
		index.Symbol{Name: "PdfRequester", Kind: "class", Path: "a.rb", Line: 1, Container: "Report"},
		index.Symbol{Name: "PdfRequester", Kind: "class", Path: "b.rb", Line: 1, Container: "CustomReport"},
	)

	got, close := suggestSymbols("Wrong::Namespace::PdfRequester", idx)
	if !close {
		t.Error("a right name under a wrong namespace should count as close")
	}
	if len(got) == 0 {
		t.Fatal("no suggestions for a right name under a wrong namespace")
	}
	for _, s := range got {
		if !strings.Contains(s, "PdfRequester") {
			t.Errorf("unrelated suggestion %q", s)
		}
	}
}

func TestSuggestSymbols_BoundedAndNilSafe(t *testing.T) {
	if got, _ := suggestSymbols("Anything", nil); got != nil {
		t.Errorf("a nil index should yield no suggestions, got %v", got)
	}

	var symbols []index.Symbol
	for i := 0; i < 40; i++ {
		symbols = append(symbols, index.Symbol{
			Name: "PdfRequester", Kind: "class", Path: "a.rb", Line: 1,
			Container: fmt.Sprintf("Ns%d", i),
		})
	}

	if got, _ := suggestSymbols("PdfRequestor", symbolIndex(symbols...)); len(got) > maxSymbolSuggestions {
		t.Errorf("returned %d suggestions, cap is %d", len(got), maxSymbolSuggestions)
	}
}

func TestFormatSymbolMatches_UsesRubySeparators(t *testing.T) {
	// This output gets read and copied. A nested class is
	// Projects::Reports::Controller, never Projects::Reports.Controller.
	tests := []struct {
		kind string
		want string
	}{
		{"class", "Projects::Reports::Thing"},
		{"module", "Projects::Reports::Thing"},
		{"constant", "Projects::Reports::Thing"},
		{"method", "Projects::Reports#Thing"},
		{"class_method", "Projects::Reports.Thing"},
		{"scope", "Projects::Reports.Thing"},
	}

	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			m := []index.Symbol{{Name: "Thing", Kind: tc.kind, Path: "a.rb", Line: 1, Container: "Projects::Reports"}}

			if got := formatSymbolMatches("Thing", m, "exact", symbolIndex(m...)); !strings.Contains(got, tc.want) {
				t.Errorf("%s rendered wrong, want %q:\n%s", tc.kind, tc.want, got)
			}
		})
	}
}

func TestReadToolsAreMarkedReadOnly(t *testing.T) {
	// Claude Code refuses an MCP tool in plan mode unless it declares
	// readOnlyHint. Without the marks every RTS tool failed there with
	// "Cannot call ... while in plan mode".
	out := captureStdout(t, func() { handleToolsList(json.RawMessage("1")) })

	var decoded struct {
		Result struct {
			Tools []Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("tools/list is not valid JSON: %v", err)
	}

	for _, tool := range decoded.Result.Tools {
		writes := tool.Name == "record_insight" || tool.Name == "record_feature"
		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		if readOnly == writes {
			t.Errorf("%s: readOnlyHint = %v, want %v", tool.Name, readOnly, !writes)
		}
	}
}

func TestInitializeGivesStandingInstructions(t *testing.T) {
	// Claude Code loads MCP tools on demand, so the instructions are what a
	// session sees while it plans.
	out := captureStdout(t, func() { handleInitialize(json.RawMessage("1")) })

	var decoded struct {
		Result struct {
			Instructions string `json:"instructions"`
			ServerInfo   struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("initialize is not valid JSON: %v\n%s", err, out)
	}
	if decoded.Result.ServerInfo.Name != "rts" || !strings.Contains(decoded.Result.Instructions, "call touchpoints") {
		t.Errorf("initialize should name the server and tell sessions to call touchpoints: %s", out)
	}
}
