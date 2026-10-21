package brief

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rts/graph"
	"rts/index"
	"rts/insight"
)

// project builds a minimal indexed project and returns its root.
func project(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	write(t, root, "Gemfile", "source 'x'\n")
	write(t, root, "config/application.rb", "module App; end\n")
	write(t, root, "app/models/user.rb", "class User\n  def name; end\nend\n")

	pm, err := index.BuildMap(root, []index.Symbol{
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
		{Name: "name", Kind: "method", Path: "app/models/user.rb", Line: 2, Container: "User"},
	})
	if err != nil {
		t.Fatalf("BuildMap: %v", err)
	}
	if err := index.WriteMap(root, pm); err != nil {
		t.Fatalf("WriteMap: %v", err)
	}

	fp, err := index.BuildFingerprints(root)
	if err != nil {
		t.Fatalf("BuildFingerprints: %v", err)
	}
	if err := index.WriteFingerprints(root, fp); err != nil {
		t.Fatalf("WriteFingerprints: %v", err)
	}

	return root
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

func TestRender_SaysNothingWhenThereIsNothingToSay(t *testing.T) {
	// Most repos on this machine have never had `rts index` run in them. A
	// hook that shouted about that on every session in every repo would be
	// switched off within a day.
	if got := Render(t.TempDir(), Options{}); got != "" {
		t.Errorf("unindexed project produced a brief:\n%s", got)
	}
}

func TestRender_DescribesTheProject(t *testing.T) {
	got := Render(project(t), Options{SkipStaleness: true})

	for _, want := range []string{
		"rts project map",
		"(rails)",
		"Derived from the code",
		"app/models",
		"find_symbol",
		"record_insight",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief missing %q:\n%s", want, got)
		}
	}
}

func TestRender_LabelsFindingsAsNotesRatherThanFacts(t *testing.T) {
	// The load-bearing property of the whole feature: a past session's
	// conclusion must never be readable as a verified fact about the code.
	root := project(t)
	if _, _, err := insight.Record(root, insight.Insight{
		Title: "Sync is convergent, not batched",
		Body:  "Each stage asks whether it is satisfied against project-wide state.",
		Paths: []string{"app/models/user.rb"},
	}, ""); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := Render(root, Options{SkipStaleness: true})

	if !strings.Contains(got, "Sync is convergent, not batched") {
		t.Errorf("brief omitted the recorded finding:\n%s", got)
	}
	if !strings.Contains(got, "NOT verified") {
		t.Errorf("findings section must mark itself unverified:\n%s", got)
	}
	if !strings.Contains(got, "[i1]") {
		t.Errorf("brief must show the id so a wrong note can be superseded:\n%s", got)
	}
	if !strings.Contains(got, "app/models/user.rb") {
		t.Errorf("brief must show what the finding rests on:\n%s", got)
	}
}

func TestRender_WorksWithFindingsButNoMap(t *testing.T) {
	// The one case where a missing map is worth mentioning: this project has
	// findings, so the index genuinely used to exist.
	root := t.TempDir()
	write(t, root, "app/models/user.rb", "class User; end\n")
	if _, _, err := insight.Record(root, insight.Insight{
		Title: "A finding",
		Body:  "Body.",
		Paths: []string{"app/models/user.rb"},
	}, ""); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := Render(root, Options{})

	if !strings.Contains(got, "A finding") {
		t.Errorf("brief lost the finding:\n%s", got)
	}
	if !strings.Contains(got, "run `rts index`") {
		t.Errorf("a project with findings but no map should say so:\n%s", got)
	}
}

func TestRender_StalenessOnlyAppearsWhenStale(t *testing.T) {
	root := project(t)

	t.Run("fresh index says nothing", func(t *testing.T) {
		// Printing "0 files changed" every session trains the reader to skip
		// the one line that matters on the day it is not zero.
		if got := Render(root, Options{Now: time.Now()}); strings.Contains(got, "changed since") {
			t.Errorf("fresh index produced a staleness warning:\n%s", got)
		}
	})

	t.Run("a git restamp is not drift", func(t *testing.T) {
		// The regression: an mtime-based check reported 493 changed files an
		// hour after a rebase, 492 byte-identical. A warning that loud on a
		// quiet day is one nobody reads on a noisy one.
		future := time.Now().Add(48 * time.Hour)
		target := filepath.Join(root, "app/models/user.rb")
		if err := os.Chtimes(target, future, future); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		if got := Render(root, Options{Now: time.Now()}); strings.Contains(got, "Index is behind") {
			t.Errorf("a restamp with no content change produced a warning:\n%s", got)
		}
	})

	t.Run("a real edit warns", func(t *testing.T) {
		write(t, root, "app/models/user.rb", "class User\n  def added_since_indexing; end\nend\n")
		future := time.Now().Add(48 * time.Hour)
		if err := os.Chtimes(filepath.Join(root, "app/models/user.rb"), future, future); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		got := Render(root, Options{Now: time.Now()})
		if !strings.Contains(got, "Index is behind the code") {
			t.Errorf("a real edit produced no warning:\n%s", got)
		}
		if !strings.Contains(got, "1 changed") {
			t.Errorf("warning should say what changed:\n%s", got)
		}
		if !strings.Contains(got, "rts index") {
			t.Errorf("warning must name the fix:\n%s", got)
		}
	})
}

func TestRender_BoundsTheNumberOfFindings(t *testing.T) {
	// The brief is paid for on every session, so nothing in it is unbounded.
	root := project(t)
	for i := 0; i < maxRenderedInsights+5; i++ {
		if _, _, err := insight.Record(root, insight.Insight{Title: "finding", Body: "b"}, ""); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got := Render(root, Options{SkipStaleness: true})

	if strings.Count(got, "] finding") > maxRenderedInsights {
		t.Errorf("rendered more than %d findings", maxRenderedInsights)
	}
	if !strings.Contains(got, "more — `rts insights` lists them all") {
		t.Errorf("brief must say findings were withheld:\n%s", got)
	}
}

func TestPreview_CollapsesWhitespaceAndTruncates(t *testing.T) {
	got := preview("line one\n\n  line   two  ")
	if got != "line one line two" {
		t.Errorf("preview = %q", got)
	}

	long := strings.Repeat("x", bodyPreviewChars*2)
	if len(preview(long)) > bodyPreviewChars+3 {
		t.Errorf("preview did not truncate: %d chars", len(preview(long)))
	}
}

func TestFormatInt(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{{0, "0"}, {999, "999"}, {1000, "1,000"}, {124164, "124,164"}, {-1500, "-1,500"}} {
		if got := formatInt(tc.in); got != tc.want {
			t.Errorf("formatInt(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderFooter_OnlyAdvertisesToolsThatWorkHere(t *testing.T) {
	// The Ruby parsers are advertised on a Ruby project and nowhere else:
	// advice that fails on first use costs the rest of the brief its
	// credibility.
	t.Run("ruby project", func(t *testing.T) {
		var buf strings.Builder
		renderFooter(&buf, &index.ProjectMap{Kind: "rails", Counts: index.Counts{Symbols: 40}})

		if !strings.Contains(buf.String(), "view_bionic_skeleton") {
			t.Errorf("a project with Ruby symbols should be offered the skeletonizer:\n%s", buf.String())
		}
	})

	t.Run("typescript project", func(t *testing.T) {
		// TypeScript is indexed and the code views read it, so it is
		// offered all three.
		var buf strings.Builder
		renderFooter(&buf, &index.ProjectMap{Kind: "node", Counts: index.Counts{Symbols: 7996}})

		for _, want := range []string{"find_symbol", "view_bionic_skeleton", "view_bionic_method"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("an indexed TypeScript project should be offered %s:\n%s", want, buf.String())
			}
		}
	})

	t.Run("project with no ruby", func(t *testing.T) {
		var buf strings.Builder
		renderFooter(&buf, &index.ProjectMap{Counts: index.Counts{Symbols: 0}})

		for _, unwanted := range []string{"view_bionic_skeleton", "view_bionic_method", "find_symbol"} {
			if strings.Contains(buf.String(), unwanted) {
				t.Errorf("offered %q on a project with no Ruby symbols:\n%s", unwanted, buf.String())
			}
		}
		if !strings.Contains(buf.String(), "search_text") {
			t.Errorf("language-agnostic tools should still be offered:\n%s", buf.String())
		}
	})
}

// withFeatures saves n single-file feature maps into a project.
func withFeatures(t *testing.T, root string, n int) {
	t.Helper()
	db, err := graph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for i := 0; i < n; i++ {
		f := graph.Feature{
			Name:    fmt.Sprintf("feature %02d", i),
			Summary: "s",
			Members: []graph.Member{{Ref: "app/models/user.rb"}},
		}
		if problems := graph.Resolve(root, "", &index.IndexFile{}, &f); len(problems) > 0 {
			t.Fatal(problems)
		}
		if _, err := graph.Record(db, f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRender_ListsEveryMapUpToThirty(t *testing.T) {
	root := project(t)
	withFeatures(t, root, 30)

	text := Render(root, Options{SkipStaleness: true})
	if !strings.Contains(text, "--- Mapped features (30) ---") || strings.Count(text, "  feature ") != 30 {
		t.Errorf("thirty maps should all be listed:\n%s", text)
	}
	if strings.Contains(text, "and 0 more") || strings.Contains(text, "most recently updated") {
		t.Errorf("a complete list should not point at the search:\n%s", text)
	}
}

func TestRender_PastThirtyListsTenAndPointsAtTheSearch(t *testing.T) {
	root := project(t)
	withFeatures(t, root, 31)

	text := Render(root, Options{SkipStaleness: true})
	if !strings.Contains(text, "--- Mapped features (31) ---") || strings.Count(text, "  feature ") != 10 {
		t.Errorf("past thirty, the ten most recent should be listed:\n%s", text)
	}
	if !strings.Contains(text, "call touchpoints\nwith a word or two about your task") || !strings.Contains(text, "… and 21 more, found by touchpoints' search") {
		t.Errorf("the brief should say how to find the rest:\n%s", text)
	}
}
