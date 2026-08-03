// Package index builds and queries a symbol index for a Rails codebase.
//
// The parser walks Ruby source line by line, tracking the enclosing
// class/module stack so methods can be reported as User#calculate_metrics
// rather than a bare "calculate_metrics". This is a pattern-based parser
// (same style as the skeletonizer), not a full Ruby parser — it handles
// the common Rails cases and intentionally trades edge-case correctness
// for simplicity.
package index

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

// Symbol describes one indexed definition. Fields are shaped to be
// serialized directly to JSON as .rts/symbols.json.
type Symbol struct {
	Name      string `json:"name"`      // "calculate_metrics"
	Kind      string `json:"kind"`      // "class" | "module" | "method" | "class_method" | "scope" | "constant"
	Path      string `json:"path"`      // "app/services/metrics.rb"
	Line      int    `json:"line"`      // 1-indexed
	Container string `json:"container"` // "MetricsService" ("" for top-level)
}

// Regexes for symbol detection. All are anchored to the trimmed line
// (leading whitespace already stripped), so they only match definitions,
// not references to a name inside a method body.
var (
	// class Foo, class Foo < Bar, class Foo::Bar
	classPattern = regexp.MustCompile(`^class\s+([A-Z][A-Za-z0-9_:]*)`)

	// module Foo, module Foo::Bar
	modulePattern = regexp.MustCompile(`^module\s+([A-Z][A-Za-z0-9_:]*)`)

	// def foo, def foo(args), def foo?, def foo!
	methodPattern = regexp.MustCompile(`^def\s+([a-z_][A-Za-z0-9_]*[?!=]?)`)

	// def self.foo, def self.foo(args)
	classMethodPattern = regexp.MustCompile(`^def\s+self\.([a-z_][A-Za-z0-9_]*[?!=]?)`)

	// scope :active, -> { ... }
	scopePattern = regexp.MustCompile(`^scope\s+:([a-z_][A-Za-z0-9_]*[?!=]?)`)

	// SOME_CONST = ..., MY_LIST = [...]. Only fully-uppercase identifiers
	// to avoid catching regular variable assignments.
	constantPattern = regexp.MustCompile(`^([A-Z][A-Z0-9_]+)\s*=`)
)

// ==================== NEW ====================
// ParseFile extracts every symbol defined in the given Ruby source.
// The path is used only to populate the Path field on each Symbol.
func ParseFile(path string, source []byte) []Symbol {
	var symbols []Symbol
	var containerStack []string // stack of enclosing class/module names

	// nestingDepth tracks how deep we are in ANY block (class, def, if, do…).
	// containerDepth tracks the depth at which each container was opened,
	// so we know when a container's own `end` fires.
	nestingDepth := 0
	containerDepths := []int{}

	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		trimmed := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments — they never define symbols and
		// they never change nesting depth.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// currentContainer is the innermost enclosing class/module name.
		currentContainer := ""
		if len(containerStack) > 0 {
			currentContainer = containerStack[len(containerStack)-1]
		}

		// ---- Symbol detection (order matters: check class_method BEFORE method) ----

		if m := classPattern.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, Symbol{
				Name: m[1], Kind: "class", Path: path, Line: lineNum,
				Container: currentContainer,
			})
			containerStack = append(containerStack, m[1])
			containerDepths = append(containerDepths, nestingDepth)
			nestingDepth++
			continue
		}

		if m := modulePattern.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, Symbol{
				Name: m[1], Kind: "module", Path: path, Line: lineNum,
				Container: currentContainer,
			})
			containerStack = append(containerStack, m[1])
			containerDepths = append(containerDepths, nestingDepth)
			nestingDepth++
			continue
		}

		if m := classMethodPattern.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, Symbol{
				Name: m[1], Kind: "class_method", Path: path, Line: lineNum,
				Container: currentContainer,
			})
			nestingDepth++
			continue
		}

		if m := methodPattern.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, Symbol{
				Name: m[1], Kind: "method", Path: path, Line: lineNum,
				Container: currentContainer,
			})
			nestingDepth++
			continue
		}

		if m := scopePattern.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, Symbol{
				Name: m[1], Kind: "scope", Path: path, Line: lineNum,
				Container: currentContainer,
			})
			// Scopes don't open a block — they end on the same line.
			continue
		}

		if m := constantPattern.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, Symbol{
				Name: m[1], Kind: "constant", Path: path, Line: lineNum,
				Container: currentContainer,
			})
			continue
		}

		// ---- Nesting tracking for non-symbol lines ----

		// Block openers increase depth. Same list as the extractor uses.
		if opensBlock(trimmed) {
			nestingDepth++
		}

		// `end` closes a block. If it closes a container's own block,
		// pop the container stack.
		if trimmed == "end" {
			nestingDepth--
			if len(containerDepths) > 0 &&
				nestingDepth == containerDepths[len(containerDepths)-1] {
				containerStack = containerStack[:len(containerStack)-1]
				containerDepths = containerDepths[:len(containerDepths)-1]
			}
		}
	}

	return symbols
}
// =============================================

// ==================== NEW ====================
// opensBlock reports whether a line starts a block that will need a
// matching `end`. Kept in sync with the extractor's blockOpeners list
// (rescue/else/when/ensure are continuations, not new blocks).
func opensBlock(trimmed string) bool {
	openers := []string{"if", "unless", "while", "until", "for", "begin", "case", "do"}
	for _, kw := range openers {
		if trimmed == kw || strings.HasPrefix(trimmed, kw+" ") ||
			strings.HasPrefix(trimmed, kw+"(") {
			return true
		}
	}
	// do-block at end of line: "items.each do |x|" or "3.times do"
	if strings.HasSuffix(trimmed, " do") || strings.Contains(trimmed, " do |") {
		return true
	}
	return false
}
// =============================================