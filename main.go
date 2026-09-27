package main

import (
	"fmt"
	"os"
	"strings"

	"rts/brief"
	"rts/doctor"
	"rts/hook"
	"rts/index"
	"rts/insight"
	"rts/mcp"
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
		fmt.Println("  rts doctor         Health check")
		fmt.Println("  rts mcp            Run the MCP server (invoked by Claude Code)")
		fmt.Println("  rts hook           Run the hook (invoked by Claude Code)")
		fmt.Println("  rts version        Show the build timestamp")
		os.Exit(1)
	}

	filePath := os.Args[1]
	file, err := os.Open(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	if err := tool.Process(file, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "Error skeletonizing file: %v\n", err)
		os.Exit(1)
	}
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

func runBrief() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rts brief] error: %v\n", err)
		os.Exit(1)
	}

	text := brief.Render(root, brief.Options{})
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
