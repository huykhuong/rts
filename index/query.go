package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads and parses .rts/symbols.json from projectRoot. Returns a friendly error if the file doesn't exist.
func Load(projectRoot string) (*IndexFile, error) {
	path := filepath.Join(projectRoot, ".rts", "symbols.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no symbol index found at %s - run `rts index` in your project root first.", path)
	}
	if err != nil {
		return nil, err
	}

	var idx IndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parsing %s (index may be corrupted, try re-running `rts index`): %w", path, err)
	}

	return &idx, nil
}

type FindResult struct {
	Symbols   []Symbol
	MatchKind string // "Exact, "segment" or "suffix"
}

// Find looks up symbols by name using progressively looser matching.
// It returns the FIRST tier that produces any results, so exact matches
// never get diluted by fuzzy ones:
//
//  1. exact   — sym.Name == query (normal case: "calculate_metrics", "User")
//  2. segment — the last "::" segment of the symbol's qualified name == query
//     (handles both `class ReportPage::PageFinder` [Case B] and
//     nested `class PageFinder` inside `ReportPage` [Case A] when
//     searching for "PageFinder")
//  3. suffix  — qualified name ends with "::query" (handles multi-segment
//     queries like "Report::PageFinder")
//
// If nothing matches at any tier, returns an empty result.
func Find(idx *IndexFile, name string) FindResult {
	var exact []Symbol
	for _, sym := range idx.Symbols {
		// Either spelling counts as exact: the bare name a lookup usually
		// uses, or the fully-qualified one.
		//
		// The qualified case was missing, and it made the most confident
		// possible query fail. Searching "Projects::Reports::PdfDownloads\
		// Controller" — the class's real, complete name — matched nothing:
		// tier 1 compared it against the bare name, tier 2 against the last
		// segment, and tier 3 wanted the qualified name to end with
		// "::" + the query, which a name that IS the query does not. So
		// knowing exactly what you were looking for was the one way to be
		// told it did not exist.
		if sym.Name == name || qualifiedName(sym) == name {
			exact = append(exact, sym)
		}
	}
	if len(exact) > 0 {
		return FindResult{Symbols: exact, MatchKind: "exact"}
	}

	// Tier 2: last-segment match on the qualified name
	var segment []Symbol
	for _, sym := range idx.Symbols {
		if lastSegment(qualifiedName(sym)) == name {
			segment = append(segment, sym)
		}
	}
	if len(segment) > 0 {
		return FindResult{Symbols: segment, MatchKind: "segment"}
	}

	// Tier 3: suffix match ("::name") on the qualified name.
	var suffix []Symbol
	needle := "::" + name
	for _, sym := range idx.Symbols {
		if strings.HasSuffix(qualifiedName(sym), needle) {
			suffix = append(suffix, sym)
		}
	}
	if len(suffix) > 0 {
		return FindResult{Symbols: suffix, MatchKind: "suffix"}
	}

	return FindResult{Symbols: nil, MatchKind: "exact"}
}

func qualifiedName(sym Symbol) string {
	if strings.Contains(sym.Name, "::") {
		return sym.Name
	}

	if sym.Container != "" {
		return sym.Container + "::" + sym.Name
	}
	return sym.Name
}

func lastSegment(qualified string) string {
	if idx := strings.LastIndex(qualified, "::"); idx >= 0 {
		return qualified[idx+2:]
	}
	return qualified
}

// QualifiedName is a symbol's full "A::B::C" name, however the definition
// spelled it.
func QualifiedName(sym Symbol) string {
	return qualifiedName(sym)
}
