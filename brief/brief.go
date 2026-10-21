// Package brief renders the orientation text a session opens with: the
// project map, the findings earlier sessions recorded, and — only when it is
// true — how far both have drifted from the code.
//
// It is the answer to the cost RTS did not previously touch. Every other tool
// in RTS makes one read cheaper; this makes a read unnecessary, because the
// session already knows what it would have gone looking for.
//
// Two rules shape everything here, and both are load-bearing:
//
//  1. It is injected as context at SessionStart, not written into a system
//     prompt. The static instructions stay byte-identical across sessions so
//     they keep being cached; only this, which genuinely differs per project
//     and per day, is paid for.
//
//  2. It says what kind of thing each line is. The map is derived and true of
//     the code; the findings are somebody's conclusions and may be wrong. A
//     brief that blurred the two would be a faster way to be confidently
//     mistaken, which is worse than reading the files.
package brief

import (
	"fmt"
	"strings"
	"time"

	"rts/graph"
	"rts/index"
	"rts/insight"
)

// maxRenderedInsights bounds what one brief carries. The store holds more;
// this is what fits in an opening turn without the reader skimming past it.
const maxRenderedInsights = 12

// bodyPreviewChars is how much of a finding's body the brief shows. Enough to
// know whether it bears on the task, short enough that twelve of them do not
// crowd out the map. record_insight returns the whole body when asked.
const bodyPreviewChars = 220

// The "Mapped features" list shows every map while a project has up to
// listAllFeaturesUpTo of them, one line each, so a session can tell at a
// glance whether its area has one. Past that it shows the listRecentFeatures
// most recently updated and says how to search the rest: the startup cost
// stays flat however many maps a project grows, and the hooks and
// touchpoints' search find the others when a task needs them.
const (
	listAllFeaturesUpTo = 30
	listRecentFeatures  = 10
)

// Options carry what the caller knows that the stores do not.
type Options struct {
	// Now is the clock, injected so the staleness line is testable.
	Now time.Time

	// SkipStaleness suppresses the filesystem walk behind the drift check.
	// The walk is cheap but not free, and a caller rendering the brief on a
	// hot path can decline to pay for it.
	SkipStaleness bool

	// Drift is a drift check the caller already made. When set, the brief
	// uses it instead of walking the tree a second time — the SessionStart
	// hook checks drift itself, to decide whether to start a reindex.
	Drift *index.Drift

	// Checkout is the top of the session's own checkout, which a feature
	// map's "changed since" is judged against. Empty means the root.
	Checkout string

	// Refreshing says a reindex was started in the background for this
	// session. The map below it is then the previous run's, and the brief
	// says so instead of telling the reader to do what is already happening.
	Refreshing bool
}

// Render composes the brief for a project. It returns an empty string when
// there is nothing worth saying — a project with no map and no findings gets
// silence rather than a header announcing two empty sections.
//
// A missing map is not an error here. Most projects on this machine have
// never had `rts index` run in them, and a hook that shouted about that on
// every session in every repo would be turned off within a day. The one place
// it is worth saying is a project that HAS findings but no map, because there
// the index genuinely used to exist.
func Render(projectRoot string, opts Options) string {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	pm, mapErr := index.LoadMap(projectRoot)

	store, err := insight.Load(projectRoot)
	if err != nil {
		store = &insight.Store{}
	}
	findings := insight.Live(projectRoot, store)
	features := loadFeatures(projectRoot, opts.Checkout)

	if mapErr != nil && len(findings) == 0 && features.total == 0 {
		return ""
	}

	var buf strings.Builder

	if pm != nil {
		renderMap(&buf, pm)
	}
	renderFeatures(&buf, features)
	renderFindings(&buf, findings)

	if pm != nil {
		renderStaleness(&buf, projectRoot, pm, opts)
	}
	if pm == nil {
		buf.WriteString("\nNo project map for this repo — run `rts index` to build one.\n")
	}

	renderFooter(&buf, pm)
	return buf.String()
}

func renderMap(buf *strings.Builder, pm *index.ProjectMap) {
	fmt.Fprintf(buf, "=== rts project map: %s", pm.Name)
	if pm.Kind != "" && pm.Kind != "unknown" {
		fmt.Fprintf(buf, " (%s)", pm.Kind)
	}
	fmt.Fprintf(buf, " — %s symbols across %s files, indexed %s ===\n",
		formatInt(pm.Counts.Symbols), formatInt(pm.Counts.Files),
		pm.GeneratedAt.Local().Format("2006-01-02"))

	buf.WriteString("Derived from the code by `rts index`. True as of that run.\n")

	if len(pm.EntryPoints) > 0 {
		fmt.Fprintf(buf, "\nEntry points: %s\n", strings.Join(pm.EntryPoints, ", "))
	}

	if len(pm.Dirs) > 0 {
		buf.WriteString("\nWhere the code lives (symbols / files):\n")
		width := 0
		for _, d := range pm.Dirs {
			if len(d.Path) > width {
				width = len(d.Path)
			}
		}
		for _, d := range pm.Dirs {
			fmt.Fprintf(buf, "  %-*s  %5s / %s\n", width, d.Path, formatInt(d.Symbols), formatInt(d.Files))
		}
	}

	// Read frequency first, size second. "Which file does the work keep
	// landing in" is a better opening answer than "which file is biggest",
	// and it is the one thing here that no amount of static analysis could
	// have produced.
	if len(pm.Hot) > 0 {
		buf.WriteString("\nFiles past sessions reached for most (RTS read counts):\n")
		for _, h := range pm.Hot {
			fmt.Fprintf(buf, "  %-52s %s read(s)\n", h.Path, formatInt(h.Reads))
		}
	}

	if len(pm.Largest) > 0 {
		buf.WriteString("\nLargest files — reach for view_bionic_skeleton, never read one whole:\n")
		for _, f := range pm.Largest {
			fmt.Fprintf(buf, "  %-52s %s lines\n", f.Path, formatInt(f.Lines))
		}
	}
}

// featureList is what the brief shows of the feature maps.
type featureList struct {
	shown []graph.Summary
	total int
}

// loadFeatures reads the feature list without ever creating a graph: the brief
// runs in every project, and most have never mapped anything.
func loadFeatures(projectRoot, checkout string) featureList {
	db, err := graph.OpenExisting(projectRoot)
	if err != nil {
		return featureList{}
	}
	defer func() { _ = db.Close() }()

	shown, total, err := graph.Recent(db, projectRoot, checkout, listAllFeaturesUpTo+1)
	if err != nil {
		return featureList{}
	}
	if total > listAllFeaturesUpTo {
		shown = shown[:listRecentFeatures]
	}
	return featureList{shown: shown, total: total}
}

// renderFeatures lists what touchpoints can answer, so a session about board
// templates learns in its first turn that it need not send an Explore agent.
func renderFeatures(buf *strings.Builder, features featureList) {
	if features.total == 0 {
		return
	}

	fmt.Fprintf(buf, "\n--- Mapped features (%d) ---\n", features.total)
	if len(features.shown) < features.total {
		fmt.Fprintf(buf, "The %d most recently updated are below. To find any of the others, call touchpoints\n"+
			"with a word or two about your task (e.g. \"capsule\", \"pdf sizing\"): it searches every map\n"+
			"and lists the ones that match. Then call it with the name of the one you need.\n\n", len(features.shown))
	} else {
		buf.WriteString("Call touchpoints with a name below before exploring that area: it returns\n" +
			"the feature's classes, files and connections in one call.\n\n")
	}

	for _, f := range features.shown {
		line := fmt.Sprintf("  %s — %s, mapped %s", f.Name, plural(f.Members, "member"), f.UpdatedAt.Local().Format("2006-01-02"))
		if f.Changed > 0 {
			line += fmt.Sprintf(" (%d changed since)", f.Changed)
		}
		buf.WriteString(line + "\n")
	}

	if more := features.total - len(features.shown); more > 0 {
		fmt.Fprintf(buf, "  … and %d more, found by touchpoints' search\n", more)
	}
}

// renderFindings prints the recorded notes under a label that cannot be
// misread. The caveat is attached to the section rather than repeated per
// line: a warning printed on every row is one nobody reads.
func renderFindings(buf *strings.Builder, findings []insight.Insight) {
	if len(findings) == 0 {
		return
	}

	shown := findings
	if len(shown) > maxRenderedInsights {
		shown = shown[:maxRenderedInsights]
	}

	fmt.Fprintf(buf, "\n--- Recorded findings (%d) ---\n", len(findings))
	buf.WriteString("Conclusions earlier sessions wrote down. These are notes, NOT verified\n" +
		"facts about the code: they can be stale or wrong. Treat one as a lead to\n" +
		"check, never as evidence to cite. Correct a wrong one with record_insight\n" +
		"(supersedes), don't argue with it.\n\n")

	for _, in := range shown {
		fmt.Fprintf(buf, "  [%s] %s\n", in.ID, in.Title)
		if body := preview(in.Body); body != "" {
			fmt.Fprintf(buf, "       %s\n", body)
		}
		if len(in.Paths) > 0 {
			fmt.Fprintf(buf, "       → %s\n", strings.Join(in.Paths, ", "))
		}
		if len(in.Symbols) > 0 {
			fmt.Fprintf(buf, "       ↳ %s\n", strings.Join(in.Symbols, ", "))
		}
	}

	if len(findings) > len(shown) {
		fmt.Fprintf(buf, "\n  (%d more — `rts insights` lists them all)\n", len(findings)-len(shown))
	}
}

// renderStaleness says how far the code has moved since the index was built —
// and says nothing at all when it has not.
//
// Printing "0 files changed" on every session would train the reader to skip
// the line, which is exactly the line that matters on the day it is not zero.
// The drift check earns that silence by comparing content: an earlier version
// compared mtimes and reported 493 changed files an hour after a rebase, 492
// of them byte-identical, which is precisely the crying-wolf this guards
// against.
func renderStaleness(buf *strings.Builder, projectRoot string, pm *index.ProjectMap, opts Options) {
	age := humanAge(opts.Now.Sub(pm.GeneratedAt))

	if opts.Refreshing {
		if opts.Drift != nil && opts.Drift.Stale() {
			fmt.Fprintf(buf, "\n! Index is behind the code: %s since it was built (%s). Refreshing it in the background now.\n",
				opts.Drift.Describe(), age)
		} else {
			// No drift to report means the index predates content
			// fingerprints, so nothing could say whether it was current.
			fmt.Fprintf(buf, "\n! Index was built (%s) without content fingerprints, so its freshness can't be checked. Rebuilding it in the background now.\n", age)
		}
		buf.WriteString("  This map is from the previous run; find_symbol picks up the new index by itself within a few seconds.\n")
		return
	}

	drift := opts.Drift
	if drift == nil {
		if opts.SkipStaleness {
			return
		}
		checked, err := index.CheckDrift(projectRoot)
		if err != nil {
			return
		}
		drift = &checked
	}
	if !drift.Stale() {
		return
	}

	fmt.Fprintf(buf, "\n! Index is behind the code: %s since it was built (%s).\n", drift.Describe(), age)
	buf.WriteString("  find_symbol may miss or point at stale lines. Run `rts index` to refresh.\n")
}

// renderFooter names the tools worth reaching for, and names only the ones
// that will actually work here.
//
// The skeletonizer, the method extractor and the symbol index are all Ruby
// parsers. Advertising them in a Go or Node repo is advice that fails on
// first use, which costs the reader a wasted call and costs the rest of the
// brief its credibility.
func renderFooter(buf *strings.Builder, pm *index.ProjectMap) {
	buf.WriteString("\nTo go deeper, use the rts tools rather than reading files whole:\n")

	if pm != nil && pm.Counts.Symbols > 0 {
		// Symbols come from Ruby, TypeScript and JavaScript, and the skeleton
		// and method views read all three.
		buf.WriteString("  find_symbol — where a class/method/constant is defined\n" +
			"  view_bionic_skeleton — a big Ruby/TypeScript/JavaScript file's structure\n" +
			"  view_bionic_method — one method or function body, several at a time\n")
	}

	buf.WriteString("  view_file — read any file (routes large Ruby files to a skeleton)\n" +
		"  search_text / list_directory — bounded grep and tree\n")

	if pm != nil {
		buf.WriteString("  project_map — this map again, in full\n")
	}

	buf.WriteString("  touchpoints — a mapped feature's classes, files and connections, in one call\n" +
		"  record_feature — save the map of a feature you explored, so the next session skips the exploring\n" +
		"  record_insight — write a finding back here, so the next session starts with it\n")
}

func preview(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if len(body) <= bodyPreviewChars {
		return body
	}
	return body[:bodyPreviewChars-1] + "…"
}

// humanAge renders a duration the way the doctor report does — coarse, and
// only as precise as a reader needs.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "under an hour ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// formatInt adds thousands separators: 124164 → "124,164".
func formatInt(n int) string {
	if n < 0 {
		return "-" + formatInt(-n)
	}

	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var out strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		out.WriteString(s[:lead])
		out.WriteString(",")
	}
	for i := lead; i < len(s); i += 3 {
		out.WriteString(s[i : i+3])
		if i+3 < len(s) {
			out.WriteString(",")
		}
	}
	return out.String()
}
