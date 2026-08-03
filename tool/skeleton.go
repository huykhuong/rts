package tool

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// SkeletonThreshold is the line-count cutoff for skeletonization.
// Files strictly under this many lines are returned in full with a
// header note, because the token savings from skeletonizing a small
// file are marginal and Claude would likely need the bodies anyway.
const SkeletonThreshold = 100

// Rails macros we want to preserve in the skeleton because they define behavior.
var railsMacros = []string{
	"before_action", "after_action", "around_action",
	"has_many", "belongs_to", "has_one", "has_and_belongs_to_many",
	"scope", "validate", "validates", "include", "extend",
}

// Process reads Ruby source from r and writes either the full file
// (for small files under SkeletonThreshold lines) or a stripped-down
// skeleton (classes, modules, method signatures, Rails macros) to w.
//
// The output always begins with a header comment explaining which mode
// was used, so Claude Code knows whether it received a skeleton or the
// full file.
func Process(r io.Reader, w io.Writer) error {
	// Step 1: read the whole file into memory as a slice of lines.
	// Ruby source files are always small enough that this is fine, and
	// buffering lets us decide passthrough-vs-skeleton after we know the
	// total line count.
	lines, err := readAllLines(r)
	if err != nil {
		return err
	}

	writer := bufio.NewWriter(w)
	defer writer.Flush()

	// Step 2: small-file passthrough. Return the raw content with a header
	// that tells Claude Code why it got the full file instead of a skeleton.
	if len(lines) < SkeletonThreshold {
		fmt.Fprintf(writer, "# rts: %d lines (full)\n", len(lines))
		for _, line := range lines {
			writer.WriteString(line + "\n")
		}
		return nil
	}

	// Step 3: large file — write the skeleton header, then skeletonize.
	fmt.Fprintf(writer, "# === rts: skeleton view (%d lines total) — call view_bionic_method for bodies ===\n", len(lines))
	return skeletonize(lines, writer)
}

// readAllLines pulls every line from r into a slice. Scanner strips the
// trailing newline from each line; we re-add it when we write out, so
// blank lines are preserved correctly.
func readAllLines(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	// Bump the buffer size for very long lines (rare in Ruby but possible
	// with fixture data or big embedded strings). Default is 64KB per line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// skeletonize walks through the previously-read lines and writes only the
// structural parts (class/module/def declarations, Rails macros, visibility
// keywords, `end`) to w. Method bodies, comments, and blank lines are
// dropped. This is the original Process logic, unchanged in behavior,
// just extracted so Process can decide passthrough vs skeleton first.
func skeletonize(lines []string, w *bufio.Writer) error {
	inDoc := false

	// Calculate the width needed for line numbers so they right-align
	// cleanly. A 50-line file needs 2 digits, a 500-line file needs 3, etc.
	numWidth := len(fmt.Sprintf("%d", len(lines)))

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// 1. Handle multi-line comments (=begin ... =end)
		if strings.HasPrefix(trimmed, "=begin") {
			inDoc = true
			continue
		}
		if strings.HasPrefix(trimmed, "=end") {
			inDoc = false
			continue
		}
		if inDoc || strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}

		// 2. Identify structural keywords
		isStructure := false
		if strings.HasPrefix(trimmed, "class ") ||
			strings.HasPrefix(trimmed, "module ") ||
			strings.HasPrefix(trimmed, "def ") ||
			trimmed == "private" ||
			trimmed == "protected" ||
			trimmed == "public" ||
			trimmed == "end" {
			isStructure = true
		}

		isTestBlock := false
		var testDesc string
		if !isStructure {
			if m := testBlockPattern.FindStringSubmatch(trimmed); m != nil {
				isTestBlock = true
				isStructure = true
				testDesc = m[1]
			}
		}

		// 3. Identify Rails Macros
		if !isStructure {
			for _, macro := range railsMacros {
				if strings.HasPrefix(trimmed, macro+" ") || strings.HasPrefix(trimmed, macro+"(") {
					isStructure = true
					break
				}
			}
		}

		// 4. Output structural lines. If it's an inline/single-line def, keep it.
		if isStructure {
			lineNum := fmt.Sprintf("%*d\t", numWidth, i+1)

			if isTestBlock {
				fmt.Fprintf(w, "%stest %q ; end\n", lineNum, testDesc)
			} else if strings.HasPrefix(trimmed, "def ") && strings.HasSuffix(trimmed, " end") {
				w.WriteString(lineNum + trimmed + "\n")
			} else if strings.HasPrefix(trimmed, "def ") {
				w.WriteString(lineNum + trimmed + " ; end\n")
			} else {
				w.WriteString(lineNum + trimmed + "\n")
			}
		}
	}

	return nil
}