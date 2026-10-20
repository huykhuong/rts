package index

// The TypeScript and JavaScript half of the index. Same approach as the Ruby
// parser: a line-by-line pattern match with the nesting tracked alongside,
// not a real parser. It records what a session asks "where is X?" about —
// top-level classes, interfaces, types, enums, functions and constants, and
// the methods inside a class — and accepts missing an exotic declaration as
// the price of staying dependency-free.
//
// Ruby nests with `end`; these nest with braces, so the tracking counts
// braces outside strings, template literals and comments.

import (
	"bufio"
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
)

// scriptExtensions are the files ParseTypeScript reads. Declaration files
// (.d.ts) are left out: they describe code that lives elsewhere, usually in
// node_modules, and indexing them points find_symbol at a signature instead
// of a body.
var scriptExtensions = map[string]bool{
	".ts": true, ".tsx": true, ".mts": true, ".cts": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
}

// IsScript reports whether path is a TypeScript or JavaScript source file the
// index reads.
func IsScript(path string) bool {
	return scriptExtensions[filepath.Ext(path)] && !strings.HasSuffix(path, ".d.ts")
}

const tsIdent = `[A-Za-z_$][\w$]*`

var (
	// Everything a top-level declaration may be prefixed with.
	tsPrefix = `^(?:export\s+)?(?:default\s+)?(?:declare\s+)?`

	tsClassPattern     = regexp.MustCompile(tsPrefix + `(?:abstract\s+)?class\s+(` + tsIdent + `)`)
	tsAnonClassPattern = regexp.MustCompile(tsPrefix + `(?:abstract\s+)?class(?:\s+extends\b|\s*\{)`)
	tsInterfacePattern = regexp.MustCompile(tsPrefix + `interface\s+(` + tsIdent + `)`)
	tsTypePattern      = regexp.MustCompile(tsPrefix + `type\s+(` + tsIdent + `)\s*[<=]`)
	tsEnumPattern      = regexp.MustCompile(tsPrefix + `(?:const\s+)?enum\s+(` + tsIdent + `)`)
	tsFunctionPattern  = regexp.MustCompile(tsPrefix + `(?:async\s+)?function\s*\*?\s*(` + tsIdent + `)`)
	tsVariablePattern  = regexp.MustCompile(tsPrefix + `(?:const|let|var)\s+(` + tsIdent + `)\s*(?::[^=]*)?=\s*(.*)`)

	// Inside a class body: optional modifiers, a name, optional generics,
	// then the parameter list. `name = (...) =>` and `name = async (...)`
	// are methods too.
	tsModifiers         = `^(?:(?:public|private|protected|static|readonly|async|abstract|override|declare|get|set)\s+)*`
	tsMethodPattern     = regexp.MustCompile(tsModifiers + `(#?` + tsIdent + `)\s*[?!]?\s*(?:<[^>]*>)?\s*\(`)
	tsArrowFieldPattern = regexp.MustCompile(tsModifiers + `(#?` + tsIdent + `)\s*(?::[^=]*)?=\s*(?:async\s+)?(?:\(|` + tsIdent + `\s*=>|function)`)
)

// tsNotMethods are words that look like a method call at the start of a line
// inside a class body but are statements.
var tsNotMethods = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "super": true, "new": true, "function": true, "await": true,
	"typeof": true, "do": true, "else": true, "throw": true, "delete": true,
}

// ParseTypeScript extracts the symbols defined in TypeScript or JavaScript
// source. The path only populates each Symbol's Path.
func ParseTypeScript(path string, source []byte) []Symbol {
	var symbols []Symbol
	var lex tsLexer

	type openClass struct {
		name string
		body int // brace depth of the class body
	}
	var classes []openClass

	depth := 0
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		startsInCode := !lex.inBlockComment && lex.inString == 0

		if startsInCode && trimmed != "" && !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "/*") {
			switch {
			case depth == 0 && tsAnonClassPattern.MatchString(trimmed):
				// `export default class extends Controller` — every
				// Stimulus controller. The class has no name to index, but
				// its methods do, filed under no container; the path says
				// which controller they belong to.
				classes = append(classes, openClass{body: 1})
			case depth == 0:
				if sym, opensClass := topLevelSymbol(trimmed); sym != nil {
					sym.Path, sym.Line = path, lineNum
					symbols = append(symbols, *sym)
					if opensClass {
						classes = append(classes, openClass{name: sym.Name, body: 1})
					}
				}
			case len(classes) > 0 && depth == classes[len(classes)-1].body:
				if name := methodName(trimmed); name != "" {
					symbols = append(symbols, Symbol{
						Name: name, Kind: "method", Path: path, Line: lineNum,
						Container: classes[len(classes)-1].name,
					})
				}
			}
		}

		depth += lex.braces(line)
		if depth < 0 {
			depth = 0 // a stray brace in something the lexer misread; recover rather than drift
		}
		for len(classes) > 0 && depth < classes[len(classes)-1].body {
			classes = classes[:len(classes)-1]
		}
	}

	return symbols
}

// topLevelSymbol matches a declaration at file scope.
func topLevelSymbol(trimmed string) (sym *Symbol, opensClass bool) {
	if m := tsClassPattern.FindStringSubmatch(trimmed); m != nil {
		return &Symbol{Name: m[1], Kind: "class"}, true
	}
	if m := tsInterfacePattern.FindStringSubmatch(trimmed); m != nil {
		return &Symbol{Name: m[1], Kind: "interface"}, false
	}
	if m := tsTypePattern.FindStringSubmatch(trimmed); m != nil {
		return &Symbol{Name: m[1], Kind: "type"}, false
	}
	if m := tsEnumPattern.FindStringSubmatch(trimmed); m != nil {
		return &Symbol{Name: m[1], Kind: "enum"}, false
	}
	if m := tsFunctionPattern.FindStringSubmatch(trimmed); m != nil {
		return &Symbol{Name: m[1], Kind: "function"}, false
	}
	if m := tsVariablePattern.FindStringSubmatch(trimmed); m != nil {
		kind := "const"
		rhs := m[2]
		if strings.HasPrefix(rhs, "function") || strings.HasPrefix(rhs, "async") || strings.Contains(rhs, "=>") {
			kind = "function"
		}
		return &Symbol{Name: m[1], Kind: kind}, false
	}
	return nil, false
}

// methodName matches a method or arrow-function field at class-body level.
func methodName(trimmed string) string {
	if m := tsArrowFieldPattern.FindStringSubmatch(trimmed); m != nil {
		return m[1]
	}
	if m := tsMethodPattern.FindStringSubmatch(trimmed); m != nil && !tsNotMethods[m[1]] {
		return m[1]
	}
	return ""
}

// tsLexer follows strings and comments across a file so braces inside them
// are not counted. Template literals and block comments can span lines,
// which is why it carries state from one line to the next.
type tsLexer struct {
	inBlockComment bool
	inString       byte // the quote character, or 0
	sawOpen        bool // a { outside strings and comments has been seen
}

// braces returns the net change in brace depth over one line.
func (l *tsLexer) braces(line string) int {
	delta := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case l.inBlockComment:
			if c == '*' && i+1 < len(line) && line[i+1] == '/' {
				l.inBlockComment = false
				i++
			}
		case l.inString != 0:
			switch c {
			case '\\':
				i++
			case l.inString:
				l.inString = 0
			}
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return delta
		case c == '/' && i+1 < len(line) && line[i+1] == '*':
			l.inBlockComment = true
			i++
		case c == '"' || c == '\'' || c == '`':
			l.inString = c
		case c == '{':
			delta++
			l.sawOpen = true
		case c == '}':
			delta--
		}
	}
	// A quote does not run past the end of its line; a template literal does.
	if l.inString == '"' || l.inString == '\'' {
		l.inString = 0
	}
	return delta
}

// ScriptSpan returns the index of the last line of the declaration that
// starts at lines[start]: the line where its braces close again, or, for a
// declaration with no body, the line that ends it with a semicolon. A
// declaration whose end cannot be found runs to the end of the file.
func ScriptSpan(lines []string, start int) int {
	var lex tsLexer
	depth := 0
	for i := start; i < len(lines); i++ {
		depth += lex.braces(lines[i])
		if lex.sawOpen && depth <= 0 {
			return i
		}
		if !lex.sawOpen && depth == 0 && lex.inString == 0 && !lex.inBlockComment && endsStatement(lines, i) {
			return i
		}
	}
	return len(lines) - 1
}

// endsStatement reports whether a bodiless declaration ends on lines[i]:
// with a semicolon, or — in code written without them — on a line that does
// not leave an expression open and is not continued by the next.
func endsStatement(lines []string, i int) bool {
	line := strings.TrimSpace(lines[i])
	if strings.HasSuffix(line, ";") {
		return true
	}
	for _, open := range []string{"=", "(", "[", ",", "=>", "&&", "||", "?", ":", "+", "-", "*", "/", "|", "&", "<"} {
		if strings.HasSuffix(line, open) {
			return false
		}
	}
	for j := i + 1; j < len(lines); j++ {
		next := strings.TrimSpace(lines[j])
		if next == "" {
			continue
		}
		for _, cont := range []string{".", "?", ":", "+", "-", "*", "/", "|", "&", ")", "]", ">", "=>"} {
			if strings.HasPrefix(next, cont) {
				return false
			}
		}
		break
	}
	return true
}
