package tool

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

// blockOpeners are Ruby keywords that open a new block requiring a matching `end`.
// Keywords like rescue, ensure, else, elsif, when are block *continuations* —
// they live inside an existing block and don't change the nesting depth.
var blockOpeners = []string{
	"class", "module", "def",
	"if", "unless", "while", "until", "for",
	"begin", "case",
}

var testBlockPattern = regexp.MustCompile(`^test\s+["']([^"']+)["']\s+do\b`)

// ExtractMethods reads Ruby source from r once and returns a map of
// method name → full method body (including the def and closing end lines)
// for every method in targetMethods that is found in the source.
//
// Methods that aren't found are simply absent from the returned map; the
// caller decides how to report misses. A single pass handles any number
// of targets, so extracting 5 methods costs the same I/O as extracting 1.
func ExtractMethods(r io.Reader, targetMethods []string) (map[string]string, error) {
	results := make(map[string]string, len(targetMethods))
	if len(targetMethods) == 0 {
		return results, nil
	}

	// Build a set for O(1) lookup while scanning.
	targets := make(map[string]struct{}, len(targetMethods))
	for _, m := range targetMethods {
		targets[m] = struct{}{}
	}

	scanner := bufio.NewScanner(r)

	var (
		recording    bool
		nestingLevel int
		currentName  string
		currentBody  strings.Builder
	)

	finish := func() {
		results[currentName] = currentBody.String()
		currentBody.Reset()
		currentName = ""
		recording = false
		nestingLevel = 0
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Phase 1: not recording — scan for any target method definition.
		if !recording {
			if m := testBlockPattern.FindStringSubmatch(trimmed); m != nil {
				desc := m[1]
				if matchedTarget := matchTestDescription(desc, targets); matchedTarget != "" {
					if _, done := results[matchedTarget]; done {
						continue
					}
					recording = true
					nestingLevel = 1
					currentName = matchedTarget
					currentBody.WriteString(line + "\n")
					continue
				}

				// Still a test block, but not one we're looking for so skip it.
				// but DON'T fall through to the method check
				continue
			}

			name := matchAnyMethodDef(trimmed, targets)
			if name == "" {
				continue
			}
			// Skip if we've already captured this one (duplicate def, unusual but possible).
			if _, done := results[name]; done {
				continue
			}
			recording = true
			nestingLevel = 1
			currentName = name
			currentBody.WriteString(line + "\n")

			// Single-line method: def active?; status == 'active'; end
			if strings.HasSuffix(trimmed, " end") || strings.Contains(trimmed, "; end") {
				finish()
				if len(results) == len(targetMethods) {
					break
				}
			}
			continue
		}

		// Phase 2: recording — track nesting to find the closing end.
		currentBody.WriteString(line + "\n")

		// `end` closes a block — check separately from openers so the same
		// keyword never both opens and closes on one line.
		if trimmed == "end" {
			nestingLevel--
			if nestingLevel == 0 {
				finish()
				if len(results) == len(targetMethods) {
					break
				}
			}
			continue
		}

		// Block openers at the start of the line (postfix if/unless appear
		// mid-line and correctly don't match here).
		for _, opener := range blockOpeners {
			if startsWithKeyword(trimmed, opener) {
				nestingLevel++
				break
			}
		}

		// do...end blocks: "items.each do" or "items.each do |item|"
		if containsDoBlock(trimmed) {
			nestingLevel++
		}
	}

	return results, scanner.Err()
}

// ExtractMethod is a thin single-method wrapper around ExtractMethods,
// kept for backward compatibility with the CLI and any external callers.
func ExtractMethod(r io.Reader, targetMethod string) (string, error) {
	results, err := ExtractMethods(r, []string{targetMethod})
	if err != nil {
		return "", err
	}
	return results[targetMethod], nil
}

// maxSuggestions bounds how many names we offer on a miss. A long list is
// as unhelpful as no list, and this output goes straight into context.
const maxSuggestions = 8

// minSuggestionOverlap is the shortest substring/prefix overlap we'll treat
// as meaningful. Below this, short names collide with unrelated words
// ("id" appears inside "invalid") and the suggestions become noise.
const minSuggestionOverlap = 3

// ListDefinitions returns every method name and Minitest test description
// defined in the source, in the order they appear, without duplicates.
//
// This is the candidate pool for "did you mean" suggestions when a
// requested method isn't found. It deliberately mirrors what
// ExtractMethods can actually match — bare method names (including
// `def self.x` as "x") and test descriptions — so every suggestion is a
// name the caller could successfully re-request.
func ListDefinitions(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var names []string
	seen := make(map[string]struct{})

	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	for scanner.Scan() {
		trimmed := strings.TrimSpace(scanner.Text())

		if m := testBlockPattern.FindStringSubmatch(trimmed); m != nil {
			add(m[1])
			continue
		}
		add(methodDefName(trimmed))
	}

	return names, scanner.Err()
}

// SuggestNames finds names in candidates worth offering when `want` wasn't
// found. It returns the close matches and true when any exist; otherwise it
// returns a bounded sample of everything available and false, so the caller
// can still show what the file does contain rather than a dead end.
//
// "Close" is deliberately cheap — substring in either direction, or a shared
// prefix — mirroring the tiered idea in index.Find without the cost of real
// edit distance. Matching is case-insensitive.
func SuggestNames(want string, candidates []string) ([]string, bool) {
	lowerWant := strings.ToLower(want)

	var close []string
	for _, c := range candidates {
		lc := strings.ToLower(c)
		if lc == lowerWant {
			continue // an exact match isn't a suggestion
		}
		if isCloseName(lowerWant, lc) {
			close = append(close, c)
		}
	}

	if len(close) > 0 {
		return truncate(close), true
	}
	return truncate(candidates), false
}

// isCloseName reports whether candidate is a plausible alternative for want.
// Both arguments must already be lowercased.
func isCloseName(want, candidate string) bool {
	// Requested name appears inside a real name: "dimension" → "dimensions".
	if len(want) >= minSuggestionOverlap && strings.Contains(candidate, want) {
		return true
	}
	// Real name appears inside the requested one: "dimensions" → "dimension".
	if len(candidate) >= minSuggestionOverlap && strings.Contains(want, candidate) {
		return true
	}
	// Shared prefix: "calculate_totals" → "calculate_metrics".
	return sharedPrefixLen(want, candidate) >= minSuggestionOverlap
}

// sharedPrefixLen returns how many leading bytes a and b have in common.
func sharedPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// truncate caps a suggestion list at maxSuggestions.
func truncate(names []string) []string {
	if len(names) > maxSuggestions {
		return names[:maxSuggestions]
	}
	return names
}

// matchAnyMethodDef returns the name from targets that this line defines,
// or "" if the line isn't a def matching any target. Exact word match —
// "def report" does not match target "reportable" and vice versa.
func matchAnyMethodDef(trimmed string, targets map[string]struct{}) string {
	name := methodDefName(trimmed)
	if name == "" {
		return ""
	}
	if _, ok := targets[name]; ok {
		return name
	}
	return ""
}

// methodDefName returns the method name defined on this line, or "" if the
// line isn't a method definition. "def self.publish" yields "publish", so
// class methods are addressable by their bare name.
func methodDefName(trimmed string) string {
	for _, prefix := range []string{"def self.", "def "} {
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		rest := trimmed[len(prefix):]

		// Find the end of the method name (name ends at '(', ';', ' ', or EOL).
		end := len(rest)
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if c == '(' || c == ';' || c == ' ' {
				end = i
				break
			}
		}
		return rest[:end]
	}
	return ""
}

func matchTestDescription(desc string, targets map[string]struct{}) string {
	for target := range targets {
		if strings.Contains(desc, target) {
			return target
		}
	}
	return ""
}

// startsWithKeyword checks that trimmed begins with keyword as a whole word
// (followed by space, '(', ';', or end-of-line).
func startsWithKeyword(trimmed, keyword string) bool {
	if !strings.HasPrefix(trimmed, keyword) {
		return false
	}
	if len(trimmed) == len(keyword) {
		return true
	}
	next := trimmed[len(keyword)]
	return next == ' ' || next == '(' || next == ';'
}

// containsDoBlock detects " do" or " do |" blocks that aren't part of a
// longer word like "todo".
func containsDoBlock(trimmed string) bool {
	return strings.HasSuffix(trimmed, " do") ||
		strings.Contains(trimmed, " do |")
}