package tool

import (
	"fmt"
	"strings"
	"testing"
)

// rawLines builds a plausible rg output block of n matched lines, so the
// measurement assertions below run against realistic byte counts rather
// than a contrived fixture.
func rawLines(n int) string {
	var buf strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&buf, "app/models/user.rb:%d:  has_many :posts, dependent: :destroy\n", i)
	}
	return buf.String()
}

// --- formatResults: measurement ---

// A broad search trips the line cap, and the bytes the cap withheld are
// the whole point of the measurement — saved must be clearly positive.
func TestFormatResults_BroadSearchSavesPositive(t *testing.T) {
	raw := rawLines(120)

	got := formatResults("has_many", raw)

	if got.TotalMatches != 120 {
		t.Errorf("TotalMatches = %d, want 120", got.TotalMatches)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true (120 lines exceeds the cap)")
	}
	if got.NaiveChars != len(raw) {
		t.Errorf("NaiveChars = %d, want %d (len of raw output)", got.NaiveChars, len(raw))
	}
	if saved := got.NaiveChars - len(got.Text); saved <= 0 {
		t.Errorf("saved = %d, want > 0 on a truncated search", saved)
	}
}

// A narrow search returns everything it found plus our header, so it costs
// slightly MORE than the naive grep. That negative is intended — it keeps
// the reported savings honest instead of flattering.
func TestFormatResults_NarrowSearchCostsHeaderOverhead(t *testing.T) {
	raw := rawLines(3)

	got := formatResults("has_many", raw)

	if got.TotalMatches != 3 {
		t.Errorf("TotalMatches = %d, want 3", got.TotalMatches)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false (3 lines is under the cap)")
	}
	if got.NaiveChars != len(raw) {
		t.Errorf("NaiveChars = %d, want %d", got.NaiveChars, len(raw))
	}
	if saved := got.NaiveChars - len(got.Text); saved >= 0 {
		t.Errorf("saved = %d, want < 0 (header is pure overhead when nothing truncates)", saved)
	}
}

// Exactly at the cap must not truncate — off-by-one here would report
// phantom savings on a search that returned everything.
func TestFormatResults_AtCapDoesNotTruncate(t *testing.T) {
	got := formatResults("has_many", rawLines(searchMaxLines))

	if got.Truncated {
		t.Errorf("Truncated = true at exactly %d lines, want false", searchMaxLines)
	}
	if got.TotalMatches != searchMaxLines {
		t.Errorf("TotalMatches = %d, want %d", got.TotalMatches, searchMaxLines)
	}
}

// --- formatEmpty: no baseline ---

// Zero matches means there is no counterfactual to measure, so NaiveChars
// stays 0 and the logger derives no savings at all.
func TestFormatEmpty_ReportsNoBaseline(t *testing.T) {
	got := formatEmpty("nonexistent_thing")

	if got.NaiveChars != 0 {
		t.Errorf("NaiveChars = %d, want 0 on a zero-match search", got.NaiveChars)
	}
	if got.TotalMatches != 0 {
		t.Errorf("TotalMatches = %d, want 0", got.TotalMatches)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}
	if !strings.Contains(got.Text, "0 match(es)") {
		t.Errorf("Text = %q, want it to report 0 matches", got.Text)
	}
}

// --- output shape: unchanged by the measurement work ---

// The measurement must be invisible to Claude Code. These assert the exact
// text shape that shipped before SearchOutcome existed.
func TestFormatResults_OutputShapeUnchanged(t *testing.T) {
	got := formatResults("has_many", rawLines(2)).Text

	wantPrefix := "# search: \"has_many\" — 2 match(es)\n"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("header = %q, want prefix %q", got, wantPrefix)
	}
	if strings.Contains(got, "truncated") {
		t.Errorf("Text = %q, want no truncation note under the cap", got)
	}
}

func TestFormatResults_TruncationNoteUnchanged(t *testing.T) {
	got := formatResults("has_many", rawLines(120)).Text

	wantNote := fmt.Sprintf("# ... truncated to %d of 120 lines", searchMaxLines)
	if !strings.Contains(got, wantNote) {
		t.Errorf("Text missing truncation note %q, got:\n%s", wantNote, got)
	}
	// Header reports the pre-truncation total, not the returned line count.
	if !strings.Contains(got, "120 match(es)") {
		t.Errorf("Text = %q, want header to report the full 120 matches", got)
	}
}
