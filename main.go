package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"rts/brief"
	"rts/doctor"
	"rts/graph"
	"rts/hook"
	"rts/index"
	"rts/insight"
	"rts/mcp"
	"rts/secrets"
	"rts/tool"
)

var buildTime = "unknown"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		mcp.Start(buildTime)
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "hook" {
		if err := hook.Run(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Hook error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `rts doctor` — health check. Always exits 0; it reports problems
	// rather than gating on them.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		doctor.Run(os.Stdout, buildTime)
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("rts built at: %s\n", buildTime)
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "index" {
		runIndex()
		return
	}

	// `rts brief` prints what a session is handed at startup. The hook emits
	// the same text as JSON, which is unreadable when you are trying to work
	// out why the brief says what it says — so the plain-text path exists for
	// a human to look at.
	if len(os.Args) > 1 && os.Args[1] == "brief" {
		runBrief()
		return
	}

	// `rts insights` — list what this project has recorded, and remove a note
	// that turned out to be wrong. Removal has a CLI path as well as the
	// tool's `supersedes` because deleting a bad memory is something the
	// person should be able to do without asking a model to do it for them.
	if len(os.Args) > 1 && os.Args[1] == "insights" {
		runInsights(os.Args[2:])
		return
	}

	// `rts graph` — the feature maps sessions have saved. graph.db is SQLite
	// and cannot be read in an editor, so this is how a person looks at it.
	if len(os.Args) > 1 && os.Args[1] == "graph" {
		runGraph(os.Args[2:])
		return
	}

	// If no arguments, try reading from Stdin (allows piping like: cat user.rb | rts)
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		if err := tool.Process(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing stream: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) < 2 {
		fmt.Println("RTS Context Optimizer")
		fmt.Println()
		fmt.Println("  rts <file.rb>      Skeletonize a Ruby file (also works as a pipe: cat x.rb | rts)")
		fmt.Println("  rts index          Parse the project: symbol index + project map")
		fmt.Println("  rts brief          Print what a new session is handed at startup")
		fmt.Println("  rts insights       List the findings recorded for this project")
		fmt.Println("  rts insights rm ID Remove a finding that turned out to be wrong")
		fmt.Println("  rts graph [NAME]   List the mapped features, or print one")
		fmt.Println("  rts doctor         Health check")
		fmt.Println("  rts mcp            Run the MCP server (invoked by Claude Code)")
		fmt.Println("  rts hook           Run the hook (invoked by Claude Code)")
		fmt.Println("  rts version        Show the build timestamp")
		os.Exit(1)
	}

	// An agent can run this through Bash as easily as a person can, so it is
	// held to the same rules as view_file: refused by name, scrubbed by value.
	filePath := os.Args[1]
	if secrets.IsProtected(filePath) {
		fmt.Fprintln(os.Stderr, secrets.Refusal(filePath))
		os.Exit(1)
	}
	file, err := os.Open(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	var out bytes.Buffer
	if err := tool.Process(file, &out); err != nil {
		fmt.Fprintf(os.Stderr, "Error skeletonizing file: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(cliRedactor().Redact(out.String()))
}

// cliRedactor scrubs key values from a CLI command's output, reading keys
// from both the working directory and the main checkout it resolves to.
func cliRedactor() secrets.Redactor {
	cwd, _ := os.Getwd()
	return secrets.NewRedactor(cwd, index.ResolveRoot(cwd))
}

// runIndex parses the project and writes all three derived artifacts.
//
// The map is built from the symbols that were just parsed rather than from a
// second parse, so adding it costs one filesystem walk and no extra parsing.
// A map failure is reported but not fatal: the symbol index is the part
// find_symbol depends on, and losing the map should never cost you that too.
func runIndex() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] error: %v\n", err)
		os.Exit(1)
	}
	// A linked git worktree has no .rts/ of its own — see index.ResolveRoot
	// — so `rts index` run inside one reindexes the main checkout instead,
	// consistent with every other RTS command.
	root = index.ResolveRoot(root)

	// The SessionStart hook starts this in the background whenever the code
	// has drifted, so two sessions opening together would otherwise run two
	// indexes writing the same files. The second one skips: the run already
	// in flight is producing the index it would have.
	release, ok, err := index.AcquireLock(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] error: %v\n", err)
		os.Exit(1)
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "[rts index] another index run is in progress here; skipping")
		return
	}
	defer release()

	// Failures past this point release the lock before exiting: os.Exit
	// skips deferred calls, and a lock left behind blocks every automatic
	// reindex in this project until it goes stale.
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "[rts index] error: %v\n", err)
		release()
		os.Exit(1)
	}

	symbols, err := index.BuildIndex(root)
	if err != nil {
		fail(err)
	}

	if err := index.Write(root, symbols); err != nil {
		fail(err)
	}

	pruneGraph(root, symbols)

	// Fingerprints before the map: the map's own staleness warning is read
	// against them, so an index that wrote one without the other would
	// report itself permanently undetectable.
	fingerprints, err := index.BuildFingerprints(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] could not fingerprint files: %v\n", err)
	} else if err := index.WriteFingerprints(root, fingerprints); err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] could not write fingerprints: %v\n", err)
	}

	projectMap, err := index.BuildMap(root, symbols)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] could not build project map: %v\n", err)
		return
	}
	if err := index.WriteMap(root, projectMap); err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] could not write project map: %v\n", err)
	}
}

// pruneGraph drops what the new index no longer contains from the feature
// maps. A failure is reported and nothing more: the index the run exists to
// write has already been written.
func pruneGraph(root string, symbols []index.Symbol) {
	db, err := graph.OpenExisting(root)
	if err != nil {
		return // nothing mapped here
	}
	defer func() { _ = db.Close() }()

	res, err := graph.Prune(db, root, &index.IndexFile{Symbols: symbols})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] could not prune the feature graph: %v\n", err)
		return
	}
	if res.Members+res.Edges+res.Features > 0 {
		fmt.Fprintf(os.Stderr, "[rts index] feature graph: dropped %d member(s), %d edge(s) and %d feature(s) for code that no longer exists\n",
			res.Members, res.Edges, res.Features)
	}
	if n, err := graph.InferBranches(db, root); err != nil {
		fmt.Fprintf(os.Stderr, "[rts index] could not work out which branch older maps were saved on: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "[rts index] feature graph: recorded the branch of %d map(s) saved before branches were\n", n)
	}
}

func runGraph(args []string) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts graph] error: %v\n", err)
		os.Exit(1)
	}
	root := index.ResolveRoot(cwd)
	checkout := index.CheckoutRoot(cwd)

	db, err := graph.OpenExisting(root)
	if err != nil {
		fmt.Println("No features are mapped in this project yet.")
		return
	}
	defer func() { _ = db.Close() }()

	if len(args) > 0 {
		idx, _ := index.Load(root)
		text, err := graph.Touchpoints(db, root, checkout, idx, strings.Join(args, " "), 2)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[rts graph] error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(cliRedactor().Redact(text))
		return
	}

	features, err := graph.Summaries(db, root, checkout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts graph] error: %v\n", err)
		os.Exit(1)
	}
	if len(features) == 0 {
		fmt.Println("No features are mapped in this project yet.")
		return
	}
	for _, f := range features {
		line := fmt.Sprintf("%-40s %3d members  mapped %s", f.Name, f.Members, f.UpdatedAt.Local().Format("2006-01-02"))
		switch {
		case f.Changed > 0 && f.OtherBranch != "":
			line += fmt.Sprintf("  (%d differ here; matches branch %s)", f.Changed, f.OtherBranch)
		case f.Changed > 0:
			line += fmt.Sprintf("  (%d changed since)", f.Changed)
		}
		fmt.Println(cliRedactor().Redact(line))
	}
}

func runBrief() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts brief] error: %v\n", err)
		os.Exit(1)
	}
	root = index.ResolveRoot(root)

	text := cliRedactor().Redact(brief.Render(root, brief.Options{}))
	if text == "" {
		fmt.Println("Nothing to brief: no project map and no recorded findings.")
		fmt.Println("Run `rts index` in the project root to build the map.")
		return
	}

	fmt.Print(text)
}

func runInsights(args []string) {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts insights] error: %v\n", err)
		os.Exit(1)
	}
	root = index.ResolveRoot(root)

	if len(args) >= 1 && args[0] == "rm" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: rts insights rm <id>")
			os.Exit(1)
		}

		removed, err := insight.Remove(root, args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "[rts insights] error: %v\n", err)
			os.Exit(1)
		}
		if !removed {
			fmt.Printf("No finding with id %q.\n", args[1])
			return
		}

		fmt.Printf("Removed %s.\n", args[1])
		return
	}

	store, err := insight.Load(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts insights] error: %v\n", err)
		os.Exit(1)
	}

	live := insight.Live(root, store)
	if len(live) == 0 {
		fmt.Println("No findings recorded for this project yet.")
		return
	}

	// Dropped findings are reported rather than hidden: a note vanishing
	// because its files were deleted is correct, but silently correct is
	// indistinguishable from a bug when you are looking for a note you know
	// you recorded.
	if dropped := len(store.Insights) - len(live); dropped > 0 {
		fmt.Printf("%d finding(s) hidden — every file they named is gone.\n\n", dropped)
	}

	for _, in := range live {
		fmt.Printf("[%s] %s\n", in.ID, in.Title)
		fmt.Printf("     %s\n", in.Body)
		if len(in.Paths) > 0 {
			fmt.Printf("     -> %s\n", strings.Join(in.Paths, ", "))
		}
		if len(in.Symbols) > 0 {
			fmt.Printf("     ~  %s\n", strings.Join(in.Symbols, ", "))
		}
		fmt.Printf("     recorded %s\n\n", in.RecordedAt.Local().Format("2006-01-02 15:04"))
	}
}
