package doctor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedNow is a stable "current time" so age strings are deterministic.
var fixedNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// findCheck pulls a single check out of a slice by name.
func findCheck(t *testing.T, checks []check, name string) check {
	t.Helper()
	for _, c := range checks {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, checks)
	return check{}
}

// writeIndex drops a valid .rts/symbols.json under root with n symbols
// and back-dates it to the given mtime.
func writeIndex(t *testing.T, root string, n int, mtime time.Time) {
	t.Helper()

	dir := filepath.Join(root, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir .rts: %v", err)
	}

	symbols := make([]map[string]interface{}, 0, n)
	for i := 0; i < n; i++ {
		symbols = append(symbols, map[string]interface{}{
			"name": "Sym", "kind": "class", "path": "a.rb", "line": 1, "container": "",
		})
	}
	body, err := json.Marshal(map[string]interface{}{
		"version": 1, "generated_at": mtime, "symbols": symbols,
	})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}

	path := filepath.Join(dir, "symbols.json")
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// --- checkBuild ---

func TestCheckBuild(t *testing.T) {
	cases := []struct {
		name      string
		buildTime string
		want      status
		contains  string
	}{
		{"fresh build", fixedNow.Add(-3 * time.Minute).Format(time.RFC3339), statusPass, "3 minutes ago"},
		{"old build", fixedNow.Add(-49 * time.Hour).Format(time.RFC3339), statusPass, "2 days ago"},
		{"unknown", "unknown", statusWarn, "make install"},
		{"empty", "", statusWarn, "make install"},
		{"garbage", "not-a-timestamp", statusWarn, "unparseable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkBuild(tc.buildTime, fixedNow)
			if got.status != tc.want {
				t.Errorf("status = %s, want %s (detail: %s)", got.status, tc.want, got.detail)
			}
			if !strings.Contains(got.detail, tc.contains) {
				t.Errorf("detail %q should contain %q", got.detail, tc.contains)
			}
		})
	}
}

// --- checkKillSwitch ---

func TestCheckKillSwitch_Set(t *testing.T) {
	t.Setenv("RTS_DISABLE", "1")

	got := checkKillSwitch()
	if got.status != statusWarn {
		t.Errorf("RTS_DISABLE=1 should WARN, got %s", got.status)
	}
	if !strings.Contains(got.detail, "RTS_DISABLE=1") {
		t.Errorf("detail should name the flag, got: %s", got.detail)
	}
}

func TestCheckKillSwitch_Unset(t *testing.T) {
	t.Setenv("RTS_DISABLE", "")

	if got := checkKillSwitch(); got.status != statusPass {
		t.Errorf("unset RTS_DISABLE should PASS, got %s (%s)", got.status, got.detail)
	}
}

// --- checkIndex ---

func TestCheckIndex_Missing(t *testing.T) {
	// The headline degradation case: no index at all must WARN with an
	// actionable hint, never fail or panic.
	root := t.TempDir()

	got := checkIndex(root, fixedNow)
	if got.status != statusWarn {
		t.Errorf("missing index should WARN, got %s", got.status)
	}
	if !strings.Contains(got.detail, "rts index") {
		t.Errorf("detail should suggest `rts index`, got: %s", got.detail)
	}
}

func TestCheckIndex_FreshReportsCount(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root, 42, fixedNow.Add(-2*time.Hour))

	got := checkIndex(root, fixedNow)
	if got.status != statusPass {
		t.Errorf("fresh index should PASS, got %s (%s)", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "42 symbols") {
		t.Errorf("detail should report symbol count, got: %s", got.detail)
	}
	if !strings.Contains(got.detail, "2 hours ago") {
		t.Errorf("detail should report age, got: %s", got.detail)
	}
}

func TestCheckIndex_StaleWarns(t *testing.T) {
	root := t.TempDir()
	writeIndex(t, root, 7, fixedNow.Add(-30*24*time.Hour))

	got := checkIndex(root, fixedNow)
	if got.status != statusWarn {
		t.Errorf("30-day-old index should WARN, got %s (%s)", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "re-running") {
		t.Errorf("stale detail should suggest re-indexing, got: %s", got.detail)
	}
}

func TestCheckIndex_CorruptFails(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "symbols.json"), []byte(`{not json`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := checkIndex(root, fixedNow)
	if got.status != statusFail {
		t.Errorf("corrupt index should FAIL, got %s (%s)", got.status, got.detail)
	}
}

// --- checkLog ---

func TestCheckLog_MissingWarns(t *testing.T) {
	t.Setenv("RTS_LOG", "")

	if got := checkLog(t.TempDir()); got.status != statusWarn {
		t.Errorf("absent log should WARN, got %s (%s)", got.status, got.detail)
	}
}

func TestCheckLog_CountsEvents(t *testing.T) {
	t.Setenv("RTS_LOG", "")

	home := t.TempDir()
	dir := filepath.Join(home, ".rts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := strings.Repeat(`{"tool":"view_file","response_chars":10}`+"\n", 3)
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got := checkLog(home)
	if got.status != statusPass {
		t.Errorf("readable log should PASS, got %s (%s)", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "3 event(s)") {
		t.Errorf("detail should report 3 events, got: %s", got.detail)
	}
}

func TestCheckLog_DisabledWarns(t *testing.T) {
	t.Setenv("RTS_LOG", "0")

	got := checkLog(t.TempDir())
	if got.status != statusWarn {
		t.Errorf("RTS_LOG=0 should WARN, got %s", got.status)
	}
	if !strings.Contains(got.detail, "RTS_LOG=0") {
		t.Errorf("detail should name the flag, got: %s", got.detail)
	}
}

func TestCheckLog_NoHomeFails(t *testing.T) {
	t.Setenv("RTS_LOG", "")

	if got := checkLog(""); got.status != statusFail {
		t.Errorf("unknown home should FAIL, got %s", got.status)
	}
}

// --- checkRTSDir ---

func TestCheckRTSDir_CreatesAndProbes(t *testing.T) {
	home := t.TempDir()

	got := checkRTSDir(home)
	if got.status != statusPass {
		t.Errorf("writable home should PASS, got %s (%s)", got.status, got.detail)
	}

	// The probe file must be cleaned up, not left behind.
	if _, err := os.Stat(filepath.Join(home, ".rts", ".doctor-write-probe")); !os.IsNotExist(err) {
		t.Error("write probe should be removed after the check")
	}
}

func TestCheckRTSDir_ReadOnlyFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}

	home := t.TempDir()
	if err := os.Chmod(home, 0500); err != nil { // r-x: can't create .rts
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0700) })

	if got := checkRTSDir(home); got.status != statusFail {
		t.Errorf("read-only home should FAIL, got %s (%s)", got.status, got.detail)
	}
}

func TestCheckRTSDir_NoHomeFails(t *testing.T) {
	if got := checkRTSDir(""); got.status != statusFail {
		t.Errorf("unknown home should FAIL, got %s", got.status)
	}
}

// --- humanAge ---

func TestHumanAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{5 * time.Minute, "5 minutes ago"},
		{time.Hour, "1 hour ago"},
		{3 * time.Hour, "3 hours ago"},
		{24 * time.Hour, "1 day ago"},
		{72 * time.Hour, "3 days ago"},
		{-time.Hour, "in the future"},
	}

	for _, tc := range cases {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// --- report / Run ---

func TestReport_RendersAllChecksAndTally(t *testing.T) {
	checks := []check{
		{"build", statusPass, "built yesterday"},
		{"ripgrep", statusWarn, "not found"},
		{"symbol index", statusFail, "corrupt"},
	}

	var buf bytes.Buffer
	report(&buf, checks)
	out := buf.String()

	for _, want := range []string{"rts doctor", "[PASS]", "[WARN]", "[FAIL]",
		"build", "ripgrep", "symbol index", "1 passed, 1 warning(s), 1 failure(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestCollect_CoversEveryCheck(t *testing.T) {
	// Guards against a check being added to the report but not to collect,
	// or silently dropped.
	got := collect("unknown", t.TempDir(), t.TempDir(), fixedNow)

	want := []string{"build", "kill switch", "ripgrep", "symbol index", "event log", "~/.rts dir"}
	if len(got) != len(want) {
		t.Fatalf("collect returned %d checks, want %d", len(got), len(want))
	}
	for _, name := range want {
		findCheck(t, got, name) // fails the test if absent
	}
}

func TestRun_DegradesGracefullyOnEmptyEnvironment(t *testing.T) {
	// The acceptance criterion: nothing present, nothing panics, and a
	// readable report still comes out.
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var buf bytes.Buffer
	Run(&buf, "unknown")

	out := buf.String()
	if !strings.Contains(out, "rts doctor") {
		t.Errorf("Run should emit a report, got: %s", out)
	}
	if !strings.Contains(out, "passed,") {
		t.Errorf("Run should emit a tally, got: %s", out)
	}
}
