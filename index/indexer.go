package index

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type IndexFile struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Symbols     []Symbol  `json:"symbols"`
}

var skippedDirs = map[string]bool{
	".git":         true,
	".rts":         true, // don't recurse into our own output
	"vendor":       true,
	"tmp":          true,
	"log":          true,
	"node_modules": true,
	"coverage":     true,
	"public":       true,
	"dist":         true, // build output of a JS/TS project
	"builds":       true, // app/assets/builds: Rails' bundled JS and CSS
}

var skippedPathSuffixes = []string{
	string(filepath.Separator) + "db" + string(filepath.Separator) + "migrate" + string(filepath.Separator),
}

func BuildIndex(projectRoot string) ([]Symbol, error) {
	var all []Symbol
	filesIndexed := 0

	err := filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Walk encounters error - log and skip.
			fmt.Fprintf(os.Stderr, "[rts index] skipping %s: %v\n", path, err)
			return nil
		}

		// Skip dirs we don't care about
		if d.IsDir() {
			if skipDir(path, d.Name(), projectRoot) {
				return filepath.SkipDir
			}
			return nil
		}

		if !shouldIndex(path) {
			return nil
		}

		// Skip + parse, any error is logged and skipped
		source, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[rts index] skipping %s: %v\n", path, err)
			return nil
		}

		// Store relative paths in the index so can be accessed across different machines.
		relPath, err := filepath.Rel(projectRoot, path)
		if err != nil {
			relPath = path
		}

		symbols := ParseFile(relPath, source)
		if IsScript(path) {
			symbols = ParseTypeScript(relPath, source)
		}
		all = append(all, symbols...)
		filesIndexed++
		return nil
	})

	if err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "[rts index] indexed %d symbols across %d files\n", len(all), filesIndexed)

	return all, nil
}

// Write serializes the symbol list to .rts/symbols.json
func Write(projectRoot string, symbols []Symbol) error {
	dir := filepath.Join(projectRoot, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating .rts directory %w", err)
	}

	indexFile := IndexFile{
		Version:     1,
		GeneratedAt: time.Now().UTC(),
		Symbols:     symbols,
	}

	data, err := json.MarshalIndent(indexFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling index: %w", err)
	}

	outPath := filepath.Join(dir, "symbols.json")
	if err := writeFileAtomic(outPath, data); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	fmt.Fprintf(os.Stderr, "[rts index] wrote %s\n", outPath)
	return nil
}
