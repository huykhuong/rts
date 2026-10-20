package tool

// The TypeScript and JavaScript counterparts of the skeleton and method
// views. Both stand on the index's parser — the same declarations
// find_symbol knows about — so the outline, the method lookup and the index
// can never disagree about what a file defines.

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"rts/index"
)

// maxInlineBody is the longest interface or type body the outline shows in
// full. A shape is often the point of reading a TypeScript file, and a short
// one costs less to show than to fetch separately.
const maxInlineBody = 12

// ProcessScript writes a TypeScript or JavaScript file in full when it is
// small, and as an outline when it is large: its imports, then every
// declaration the index records, each with its line number. path only
// names the file for the parser.
func ProcessScript(path string, r io.Reader, w io.Writer) error {
	lines, err := readAllLines(r)
	if err != nil {
		return err
	}

	writer := bufio.NewWriter(w)
	defer func() { _ = writer.Flush() }()

	if len(lines) < SkeletonThreshold {
		_, _ = fmt.Fprintf(writer, "# rts: %d lines (full)\n", len(lines))
		for _, line := range lines {
			_, _ = writer.WriteString(line + "\n")
		}
		return nil
	}

	_, _ = fmt.Fprintf(writer, "# === rts: skeleton view (%d lines total) — call view_bionic_method for bodies ===\n", len(lines))
	writeScriptOutline(path, lines, writer)
	return nil
}

func writeScriptOutline(path string, lines []string, w *bufio.Writer) {
	numWidth := len(fmt.Sprintf("%d", len(lines)))
	at := func(i int) string { return fmt.Sprintf("%*d\t", numWidth, i+1) }

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "import ") && !strings.HasPrefix(trimmed, "export * ") && !strings.HasPrefix(trimmed, "export {") {
			continue
		}
		end := index.ScriptSpan(lines, i)
		_, _ = w.WriteString(at(i) + joinLines(lines[i:end+1]) + "\n")
		i = end
	}

	// Classes are only tracked at file scope, so at most one is open: its
	// closing brace is written once the outline moves past its last line.
	openClassEnd := -1
	closeClass := func() {
		if openClassEnd >= 0 {
			_, _ = w.WriteString(at(openClassEnd) + "}\n")
			openClassEnd = -1
		}
	}

	for _, sym := range index.ParseTypeScript(path, []byte(strings.Join(lines, "\n"))) {
		start := sym.Line - 1
		if start > openClassEnd {
			closeClass()
		}
		end := index.ScriptSpan(lines, start)
		trimmed := strings.TrimSpace(lines[start])
		indent := ""
		if sym.Kind == "method" {
			indent = "  "
		}

		switch {
		case sym.Kind == "class":
			_, _ = w.WriteString(at(start) + trimmed + "\n")
			if end > start {
				openClassEnd = end
			}
		case (sym.Kind == "interface" || sym.Kind == "type") && end > start && end-start <= maxInlineBody:
			for i := start; i <= end; i++ {
				_, _ = w.WriteString(at(i) + strings.TrimRight(lines[i], " \t") + "\n")
			}
		case end > start:
			_, _ = w.WriteString(at(start) + indent + signature(trimmed) + "\n")
		default:
			_, _ = w.WriteString(at(start) + indent + trimmed + "\n")
		}
	}
	closeClass()
}

// signature cuts a declaration's first line at the brace that opens its
// body, so an outline shows what something takes and returns, not what it
// does.
func signature(trimmed string) string {
	if i := strings.LastIndex(trimmed, "{"); i > 0 {
		return strings.TrimSpace(trimmed[:i]) + " { … }"
	}
	return trimmed + " …"
}

func joinLines(lines []string) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// ExtractScriptDefinitions returns the full text of each requested function,
// method, class, interface, type or constant, keyed by the name asked for.
// A name matches a symbol's own name, or its qualified form ("Store.put",
// "Store#put" and "Store::put" all name put in class Store). A bare name
// defined more than once — the same method in two classes — returns every
// definition, each headed by where it is.
func ExtractScriptDefinitions(path string, r io.Reader, names []string) (map[string]string, error) {
	lines, err := readAllLines(r)
	if err != nil {
		return nil, err
	}
	symbols := index.ParseTypeScript(path, []byte(strings.Join(lines, "\n")))

	results := make(map[string]string, len(names))
	for _, name := range names {
		var matches []index.Symbol
		for _, sym := range symbols {
			if matchesScriptName(sym, name) {
				matches = append(matches, sym)
			}
		}

		var body strings.Builder
		for i, sym := range matches {
			start := sym.Line - 1
			end := index.ScriptSpan(lines, start)
			if len(matches) > 1 {
				if i > 0 {
					body.WriteString("\n")
				}
				fmt.Fprintf(&body, "// %s, line %d\n", qualified(sym), sym.Line)
			}
			body.WriteString(strings.Join(lines[start:end+1], "\n"))
		}
		if len(matches) > 0 {
			results[name] = body.String()
		}
	}
	return results, nil
}

func matchesScriptName(sym index.Symbol, name string) bool {
	if sym.Name == name {
		return true
	}
	if sym.Container == "" {
		return false
	}
	for _, sep := range []string{".", "#", "::"} {
		if sym.Container+sep+sym.Name == name {
			return true
		}
	}
	return false
}

func qualified(sym index.Symbol) string {
	if sym.Container == "" {
		return sym.Name
	}
	return sym.Container + "." + sym.Name
}

// ListScriptDefinitions returns every name ExtractScriptDefinitions can
// match, for "did you mean" suggestions.
func ListScriptDefinitions(path string, r io.Reader) ([]string, error) {
	lines, err := readAllLines(r)
	if err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, sym := range index.ParseTypeScript(path, []byte(strings.Join(lines, "\n"))) {
		for _, n := range []string{sym.Name, qualified(sym)} {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names, nil
}
