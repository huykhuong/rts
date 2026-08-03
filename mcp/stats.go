package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// SessionStats is the aggregated summary of a single MCP session
type SessionStats struct{
	SessionID	string
	Calls			int
	ResponseChars int
	NaiveChars	int
	SavedChars	int
	ByTool			map[string]ToolStats
}

// ToolStats is the per-tool slice of a SessionStats — how many times
// each tool was called and its cumulative savings.
type ToolStats struct {
	Calls		int
	Saved		int
}

func ComputeStats(sessionID string) (*SessionStats, error) {
	stats := &SessionStats{
		SessionID: sessionID,
		ByTool: 	 make(map[string]ToolStats),
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	logPath := filepath.Join(home, ".rts", "events.jsonl")
	f, err := os.Open(logPath)
	if os.IsNotExist(err) {
		return stats, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Bump buffer for potentially long event lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		var ev Event
		// Skip lines that fail to parse - old format, truncated writes
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}

		// Filter to just the current session.
		if ev.Session != sessionID {
			continue
		}

		stats.Calls++
		stats.ResponseChars += ev.ResponseChars
		stats.NaiveChars += ev.NaiveChars
		stats.SavedChars += ev.SavedChars

		// Update per-tool breakdown.
		ts := stats.ByTool[ev.Tool]
		ts.Calls++
		ts.Saved += ev.SavedChars
		stats.ByTool[ev.Tool] = ts
	}

	return stats, scanner.Err()
}