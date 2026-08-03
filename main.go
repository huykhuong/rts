package main

import (
	"fmt"
	"os"
	"rts/doctor"
	"rts/hook"
	"rts/index"
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
		root, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[rts index] error: %v\n", err)
			os.Exit(1)
		}

		symbols, err := index.BuildIndex(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[rts index] error: %v\n", err)
			os.Exit(1)
		}

		if err := index.Write(root, symbols); err != nil {
			fmt.Fprintf(os.Stderr, "[rts index] error: %v\n", err)
			os.Exit(1)
		}

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
		fmt.Println("Usage: rts <path_to_ruby_file> or stream via pipe.")
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