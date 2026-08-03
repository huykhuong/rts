package tool

import (
	"fmt"
	"strings"
	"testing"
)

func extract(t *testing.T, source, method string) string {
	t.Helper()
	result, err := ExtractMethod(strings.NewReader(source), method)
	if err != nil {
		t.Fatalf("ExtractMethod returned error: %v", err)
	}
	return result
}

// --- Bug #1: The original extractor dumped from the method to EOF ---

func TestExtractMethod_StopsAtEnd(t *testing.T) {
	source := `class User
  def greet
    "hello"
  end

  def farewell
    "goodbye"
  end
end
`
	got := extract(t, source, "greet")
	if strings.Contains(got, "farewell") {
		t.Errorf("extracted method leaked into next method:\n%s", got)
	}
	if !strings.Contains(got, "def greet") {
		t.Error("missing def line")
	}
	if !strings.Contains(got, `"hello"`) {
		t.Error("missing method body")
	}
	if !strings.Contains(got, "end") {
		t.Error("missing closing end")
	}
}

func TestExtractMethod_DoesNotReturnFileTail(t *testing.T) {
	source := `class BigModel
  def first_method
    x = 1
  end

  def second_method
    y = 2
  end

  def third_method
    z = 3
  end

  def fourth_method
    w = 4
  end
end
`
	got := extract(t, source, "first_method")

	for _, leaked := range []string{"second_method", "third_method", "fourth_method"} {
		if strings.Contains(got, leaked) {
			t.Errorf("file tail leaked — found %q in output:\n%s", leaked, got)
		}
	}
}

// --- Bug #2: Nesting tracker must handle if/unless/while/case/begin correctly ---

func TestExtractMethod_NestedIf(t *testing.T) {
	source := `class Foo
  def check(x)
    if x > 0
      if x > 10
        "big"
      else
        "small"
      end
    end
  end

  def unrelated
    "should not appear"
  end
end
`
	got := extract(t, source, "check")
	if strings.Contains(got, "unrelated") {
		t.Errorf("nested if caused over-read:\n%s", got)
	}
	if !strings.Contains(got, `"big"`) || !strings.Contains(got, `"small"`) {
		t.Error("missing nested if body content")
	}
}

func TestExtractMethod_NestedCase(t *testing.T) {
	source := `class Foo
  def classify(val)
    case val
    when :a
      "alpha"
    when :b
      "beta"
    else
      "other"
    end
  end

  def after
    "leaked"
  end
end
`
	got := extract(t, source, "classify")
	if strings.Contains(got, "leaked") {
		t.Errorf("case/when caused over-read:\n%s", got)
	}
	if !strings.Contains(got, `"alpha"`) || !strings.Contains(got, `"beta"`) {
		t.Error("missing case body")
	}
}

func TestExtractMethod_BeginRescueEnsure(t *testing.T) {
	source := `class Foo
  def risky
    begin
      do_something
    rescue StandardError => e
      handle(e)
    ensure
      cleanup
    end
  end

  def safe
    "should not appear"
  end
end
`
	got := extract(t, source, "risky")
	if strings.Contains(got, "should not appear") {
		t.Errorf("begin/rescue/ensure caused over-read:\n%s", got)
	}
	if !strings.Contains(got, "do_something") || !strings.Contains(got, "cleanup") {
		t.Error("missing begin/rescue/ensure body")
	}
}

func TestExtractMethod_DoBlock(t *testing.T) {
	source := `class Foo
  def process
    items.each do |item|
      transform(item)
    end
  end

  def other
    "leaked"
  end
end
`
	got := extract(t, source, "process")
	if strings.Contains(got, "leaked") {
		t.Errorf("do block caused over-read:\n%s", got)
	}
	if !strings.Contains(got, "transform(item)") {
		t.Error("missing do block body")
	}
}

func TestExtractMethod_DoBlockWithoutParams(t *testing.T) {
	source := `class Foo
  def run
    3.times do
      puts "hi"
    end
  end

  def next_method
    "nope"
  end
end
`
	got := extract(t, source, "run")
	if strings.Contains(got, "nope") {
		t.Errorf("do block (no params) caused over-read:\n%s", got)
	}
}

// --- Method name precision ---

func TestExtractMethod_ExactNameMatch(t *testing.T) {
	source := `class Foo
  def report
    "the report"
  end

  def reportable
    "reportable flag"
  end

  def report_topic
    "the topic"
  end
end
`
	got := extract(t, source, "report")
	if strings.Contains(got, "reportable") {
		t.Errorf("matched prefix 'reportable' instead of exact 'report':\n%s", got)
	}
	if strings.Contains(got, "report_topic") {
		t.Errorf("matched prefix 'report_topic' instead of exact 'report':\n%s", got)
	}
	if !strings.Contains(got, `"the report"`) {
		t.Error("missing correct method body")
	}
}

func TestExtractMethod_MethodWithParens(t *testing.T) {
	source := `class Foo
  def calculate(x, y)
    x + y
  end

  def calculate_total
    "wrong"
  end
end
`
	got := extract(t, source, "calculate")
	if strings.Contains(got, "calculate_total") || strings.Contains(got, "wrong") {
		t.Errorf("matched prefix instead of exact name with parens:\n%s", got)
	}
}

func TestExtractMethod_SelfMethod(t *testing.T) {
	source := `class Foo
  def self.build(attrs)
    new(attrs)
  end

  def instance_method
    "not this"
  end
end
`
	got := extract(t, source, "build")
	if strings.Contains(got, "instance_method") {
		t.Errorf("self method leaked:\n%s", got)
	}
	if !strings.Contains(got, "def self.build") {
		t.Error("missing def self.build line")
	}
}

// --- Single-line methods ---

func TestExtractMethod_SingleLine(t *testing.T) {
	source := `class Foo
  def active?; status == 'active'; end

  def other
    "not this"
  end
end
`
	got := extract(t, source, "active?")
	if strings.Contains(got, "other") {
		t.Errorf("single-line method leaked:\n%s", got)
	}
	if !strings.Contains(got, "active?") {
		t.Error("missing single-line method")
	}
}

// --- Edge: method not found ---

func TestExtractMethod_NotFound(t *testing.T) {
	source := `class Foo
  def existing
    true
  end
end
`
	got := extract(t, source, "nonexistent")
	if got != "" {
		t.Errorf("expected empty string for missing method, got:\n%s", got)
	}
}

// --- Edge: deeply nested ---

func TestExtractMethod_DeeplyNested(t *testing.T) {
	source := `class Foo
  def deep
    if a
      unless b
        while c
          begin
            items.each do |i|
              case i
              when 1
                "one"
              when 2
                "two"
              end
            end
          rescue
            retry
          end
        end
      end
    end
  end

  def shallow
    "must not appear"
  end
end
`
	got := extract(t, source, "deep")
	if strings.Contains(got, "must not appear") {
		t.Errorf("deeply nested method caused over-read:\n%s", got)
	}
	if !strings.Contains(got, `"one"`) || !strings.Contains(got, `"two"`) {
		t.Error("missing deep body content")
	}
}

// --- Edge: postfix if/unless should NOT open a block ---

func TestExtractMethod_PostfixConditional(t *testing.T) {
	source := `class Foo
  def simple
    return nil if invalid?
    return "ok" unless blocked?
    "done"
  end

  def after
    "leaked"
  end
end
`
	got := extract(t, source, "simple")
	if strings.Contains(got, "leaked") {
		t.Errorf("postfix conditional caused over-read:\n%s", got)
	}
	if !strings.Contains(got, `"done"`) {
		t.Error("missing method body")
	}
}

// --- Skeleton tool tests ---

func skeleton(t *testing.T, source string) string {
	t.Helper()
	var buf strings.Builder
	err := Process(strings.NewReader(source), &buf)
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	return buf.String()
}

// padToLarge appends enough blank lines to push source past SkeletonThreshold,
// forcing Process into skeleton mode. Blank lines count toward line total
// but get stripped by skeletonize, so they don't pollute assertions.
func padToLarge(source string) string {
	current := strings.Count(source, "\n")
	needed := SkeletonThreshold - current + 5 // +5 for safety margin
	if needed <= 0 {
		return source
	}
	return source + strings.Repeat("\n", needed)
}

func TestSkeleton_StripsMethodBodies(t *testing.T) {
	source := padToLarge(`class User < ApplicationRecord
  has_many :posts
  belongs_to :team
  validates :email, presence: true

  before_action :authenticate

  def full_name
    "#{first_name} #{last_name}"
  end

  def active?
    status == 'active'
  end

  private

  def normalize_email
    self.email = email.downcase.strip
  end
end
`)
	got := skeleton(t, source)

	// Structure should be preserved
	for _, want := range []string{
		"class User < ApplicationRecord",
		"has_many :posts",
		"belongs_to :team",
		"validates :email",
		"before_action :authenticate",
		"def full_name ; end",
		"def active? ; end",
		"private",
		"def normalize_email ; end",
		"end",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skeleton missing %q\ngot:\n%s", want, got)
		}
	}

	// Bodies should be stripped
	for _, gone := range []string{"first_name", "last_name", "downcase", "strip"} {
		if strings.Contains(got, gone) {
			t.Errorf("skeleton leaked body content %q\ngot:\n%s", gone, got)
		}
	}

	// Skeleton mode should announce itself in the header
	if !strings.Contains(got, "skeleton view") {
		t.Errorf("large-file output missing skeleton header:\n%s", got)
	}
}

func TestSkeleton_StripsComments(t *testing.T) {
	source := padToLarge(`# This is a comment
class Foo
  # Another comment
  def bar
    "body"
  end
end
`)
	got := skeleton(t, source)
	if strings.Contains(got, "This is a comment") || strings.Contains(got, "Another comment") {
		t.Errorf("skeleton should strip comments:\n%s", got)
	}
}

func TestSkeleton_IncludesLineNumbers(t *testing.T) {
	// Build a source where we know the exact line numbers of structural
	// elements. The padding pushes us past SkeletonThreshold so we
	// actually get skeleton mode (not passthrough).
	source := "class Foo\n"          // line 1
	source += "  has_many :bars\n"   // line 2
	source += "\n"                   // line 3 (blank, skipped)
	source += "  def hello\n"        // line 4
	source += "    \"world\"\n"      // line 5 (body, skipped)
	source += "  end\n"              // line 6
	source += "end\n"                // line 7

	source = padToLarge(source)
	got := skeleton(t, source)

	// ← NEW: Line numbers appear tab-separated, content is trimmed of
	// leading indent (compact format from step #9). Assertions updated
	// to match the compact output — no more "  has_many" with leading
	// spaces; it's just "has_many".
	if !strings.Contains(got, "1\tclass Foo") {
		t.Errorf("skeleton should show line 1 for class declaration:\n%s", got)
	}
	if !strings.Contains(got, "2\thas_many :bars") { // ← NEW: no indent
		t.Errorf("skeleton should show line 2 for has_many macro:\n%s", got)
	}
	if !strings.Contains(got, "4\tdef hello ; end") { // ← NEW: no indent
		t.Errorf("skeleton should show line 4 for method signature:\n%s", got)
	}
	if !strings.Contains(got, "6\tend") { // ← NEW: no indent
		t.Errorf("skeleton should show line 6 for inner end:\n%s", got)
	}
	if !strings.Contains(got, "7\tend") {
		t.Errorf("skeleton should show line 7 for class end:\n%s", got)
	}
}

// ← NEW: dedicated test for compact output behavior from step #9.
func TestSkeleton_CompactStripsLeadingIndent(t *testing.T) {
	// Deeply nested source with multiple levels of indentation.
	// Compact output should strip ALL leading whitespace regardless
	// of nesting depth.
	source := padToLarge(`module Outer
  class Inner
    def deep_method
      "body"
    end
  end
end
`)
	got := skeleton(t, source)

	// The compact skeleton should NOT contain leading spaces before
	// structural keywords. Every structural line starts with the line
	// number, a tab, then the keyword immediately — no indentation.
	for _, unwanted := range []string{
		"\t  class",         // "  class Inner" would have this
		"\t    def",         // "    def deep_method" would have this
		"\t  end",           // "  end" (inner) would have this
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("compact skeleton should not contain leading indent %q:\n%s", unwanted, got)
		}
	}

	// The structural content itself is still present, just without indent.
	for _, wanted := range []string{
		"class Inner",
		"def deep_method ; end",
	} {
		if !strings.Contains(got, wanted) {
			t.Errorf("compact skeleton should still contain %q:\n%s", wanted, got)
		}
	}
}

func TestSkeleton_LineNumbersNotInPassthrough(t *testing.T) {
	// Small files use passthrough — line numbers should NOT appear,
	// because passthrough returns raw content.
	source := `class Tiny
  def hello
    "world"
  end
end
`
	got := skeleton(t, source)

	// Passthrough should have the raw line without a tab-prefixed number
	if strings.Contains(got, "\tclass Tiny") {
		t.Errorf("passthrough should not have line number prefixes:\n%s", got)
	}
}

// --- Small-file passthrough tests ---

func TestProcess_SmallFilePassthrough(t *testing.T) {
	// A tiny file — well under SkeletonThreshold.
	source := `class Tiny
  def one
    "hello"
  end

  def two
    "world"
  end
end
`
	got := skeleton(t, source)

	// Header should announce passthrough mode
	if !strings.Contains(got, "# rts:") {
		t.Errorf("small-file output missing passthrough header:\n%s", got)
	}
	if !strings.Contains(got, "(full)") {
		t.Errorf("passthrough header should explain the mode:\n%s", got)
	}

	// Method bodies should be present (raw content, not skeleton)
	if !strings.Contains(got, `"hello"`) {
		t.Errorf("passthrough should include body 'hello':\n%s", got)
	}
	if !strings.Contains(got, `"world"`) {
		t.Errorf("passthrough should include body 'world':\n%s", got)
	}

	// Should NOT be skeletonized (no "; end" appended to method signatures)
	if strings.Contains(got, "def one ; end") {
		t.Errorf("passthrough should not skeletonize:\n%s", got)
	}
}

func TestProcess_SmallFilePreservesComments(t *testing.T) {
	source := `# Important comment
class Foo
  # Another important comment
  def bar
    1
  end
end
`
	got := skeleton(t, source)

	// In passthrough mode, comments must be preserved
	if !strings.Contains(got, "Important comment") {
		t.Error("passthrough should preserve top-level comment")
	}
	if !strings.Contains(got, "Another important comment") {
		t.Error("passthrough should preserve inline comment")
	}
}

func TestProcess_ThresholdBoundary(t *testing.T) {
	// A file with exactly SkeletonThreshold lines should skeletonize
	// (the check is `< threshold`, so `== threshold` skeletonizes).
	// A file with threshold-1 lines should passthrough.
	classHeader := "class Boundary\n"

	// Case 1: exactly at threshold → skeleton mode
	source := classHeader + strings.Repeat("\n", SkeletonThreshold-1)
	got := skeleton(t, source)
	if !strings.Contains(got, "skeleton view") {
		t.Errorf("at-threshold file should use skeleton mode:\n%s", got)
	}

	// Case 2: one line under threshold → passthrough mode
	source = classHeader + strings.Repeat("\n", SkeletonThreshold-2)
	got = skeleton(t, source)
	if !strings.Contains(got, "(full)") {
		t.Errorf("under-threshold file should use passthrough mode:\n%s", got)
	}
}

func TestProcess_EmptyFile(t *testing.T) {
	// Edge case: empty input. 0 lines is under threshold → passthrough with
	// zero content lines. Should not error.
	got := skeleton(t, "")
	if !strings.Contains(got, "# rts: 0 lines (full)") {
		t.Errorf("empty file should report 0 lines:\n%s", got)
	}
}
// --- ExtractMethods: multi-method extraction in one pass ---

func extractMulti(t *testing.T, source string, names []string) map[string]string {
	t.Helper()
	result, err := ExtractMethods(strings.NewReader(source), names)
	if err != nil {
		t.Fatalf("ExtractMethods returned error: %v", err)
	}
	return result
}

func TestExtractMethods_MultipleInOnePass(t *testing.T) {
	source := `class ContentElement
  def reportable
    report_topic.present?
  end

  def grid
    layout.grid
  end

  def report_topic
    topics.reports.first
  end

  def tech_stack
    "ruby"
  end
end
`
	got := extractMulti(t, source, []string{"reportable", "report_topic"})

	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %v", len(got), got)
	}

	if !strings.Contains(got["reportable"], "report_topic.present?") {
		t.Errorf("reportable missing body:\n%s", got["reportable"])
	}
	if !strings.Contains(got["report_topic"], "topics.reports.first") {
		t.Errorf("report_topic missing body:\n%s", got["report_topic"])
	}

	// Crucially: neither result should leak content from methods we didn't ask for
	for _, body := range got {
		if strings.Contains(body, "layout.grid") {
			t.Errorf("leaked 'grid' method into results:\n%s", body)
		}
		if strings.Contains(body, `"ruby"`) {
			t.Errorf("leaked 'tech_stack' method into results:\n%s", body)
		}
	}
}

func TestExtractMethods_MissingMethodsAbsentFromResults(t *testing.T) {
	source := `class Foo
  def known
    1
  end
end
`
	got := extractMulti(t, source, []string{"known", "unknown"})

	if _, ok := got["known"]; !ok {
		t.Error("known method should be present")
	}
	if _, ok := got["unknown"]; ok {
		t.Error("unknown method should NOT be present (caller reports misses)")
	}
	if len(got) != 1 {
		t.Errorf("expected exactly 1 result, got %d", len(got))
	}
}

func TestExtractMethods_EmptyTargetList(t *testing.T) {
	source := `class Foo
  def bar
    1
  end
end
`
	got := extractMulti(t, source, []string{})
	if len(got) != 0 {
		t.Errorf("expected empty results for empty target list, got %v", got)
	}
}

func TestExtractMethods_StopsEarlyWhenAllFound(t *testing.T) {
	// This test doesn't directly verify early termination (that's an
	// optimization detail), but it does verify that finding all requested
	// methods in the middle of a large file returns correct results.
	source := `class Big
  def early_one
    "e1"
  end

  def early_two
    "e2"
  end

  def much_later_unused
    "should not affect results"
  end

  def another_unused
    "also irrelevant"
  end
end
`
	got := extractMulti(t, source, []string{"early_one", "early_two"})

	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	for name, body := range got {
		if strings.Contains(body, "much_later_unused") || strings.Contains(body, "another_unused") {
			t.Errorf("%s result contains later method content:\n%s", name, body)
		}
	}
}

func TestExtractMethods_OrderIndependent(t *testing.T) {
	// The order of names in the request should not matter — results are
	// keyed by name, and each is bounded correctly.
	source := `class Foo
  def first
    1
  end

  def second
    2
  end

  def third
    3
  end
end
`
	// Request in reverse order.
	got := extractMulti(t, source, []string{"third", "first", "second"})

	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	if !strings.Contains(got["first"], "1") {
		t.Error("first method wrong")
	}
	if !strings.Contains(got["second"], "2") {
		t.Error("second method wrong")
	}
	if !strings.Contains(got["third"], "3") {
		t.Error("third method wrong")
	}
}

func TestExtractMethods_MixedSingleAndMultiLine(t *testing.T) {
	source := `class Foo
  def active?; status == 'active'; end

  def process(items)
    items.each do |i|
      transform(i)
    end
  end

  def other
    "leaked"
  end
end
`
	got := extractMulti(t, source, []string{"active?", "process"})

	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if !strings.Contains(got["active?"], "status == 'active'") {
		t.Error("single-line method wrong")
	}
	if !strings.Contains(got["process"], "transform(i)") {
		t.Error("multi-line method wrong")
	}
	for _, body := range got {
		if strings.Contains(body, "leaked") {
			t.Errorf("leaked content:\n%s", body)
		}
	}
}

// --- ExtractMethod (single-method wrapper) still works ---

func TestExtractMethod_WrapperStillWorks(t *testing.T) {
	source := `class Foo
  def greet
    "hi"
  end
end
`
	got, err := ExtractMethod(strings.NewReader(source), "greet")
	if err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}
	if !strings.Contains(got, `"hi"`) {
		t.Errorf("wrapper lost content:\n%s", got)
	}
}

func TestExtractMethod_WrapperReturnsEmptyWhenNotFound(t *testing.T) {
	source := `class Foo
  def existing
    1
  end
end
`
	got, err := ExtractMethod(strings.NewReader(source), "missing")
	if err != nil {
		t.Fatalf("wrapper returned error: %v", err)
	}
	if got != "" {
		t.Errorf("wrapper should return empty string when method missing, got:\n%s", got)
	}
}
// --- ListDefinitions ---

func TestListDefinitions_CollectsMethodsAndTests(t *testing.T) {
	src := `class Report
  def height
    @height
  end

  def self.build(attrs)
    new(attrs)
  end

  def width; @width; end
end

class ReportTest < ActiveSupport::TestCase
  test "reports the correct height" do
    assert_equal 10, report.height
  end
end
`
	got, err := ListDefinitions(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ListDefinitions: %v", err)
	}

	want := []string{"height", "build", "width", "reports the correct height"}
	if len(got) != len(want) {
		t.Fatalf("got %d definitions %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("definition %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestListDefinitions_DedupesAndHandlesEmpty(t *testing.T) {
	got, err := ListDefinitions(strings.NewReader("def dup\nend\ndef dup\nend\n"))
	if err != nil {
		t.Fatalf("ListDefinitions: %v", err)
	}
	if len(got) != 1 || got[0] != "dup" {
		t.Errorf("duplicate defs should collapse to one, got %v", got)
	}

	empty, err := ListDefinitions(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ListDefinitions on empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty source should yield no definitions, got %v", empty)
	}
}

// --- SuggestNames ---

func TestSuggestNames_CloseMatches(t *testing.T) {
	cases := []struct {
		name       string
		want       string
		candidates []string
		expect     string
	}{
		{"requested is substring of real", "dimension", []string{"dimensions", "width"}, "dimensions"},
		{"real is substring of requested", "dimensions_hash", []string{"dimensions", "width"}, "dimensions"},
		{"shared prefix", "calculate_totals", []string{"calculate_metrics", "width"}, "calculate_metrics"},
		{"case insensitive", "HEIGHT", []string{"height_in_cm"}, "height_in_cm"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, isClose := SuggestNames(tc.want, tc.candidates)
			if !isClose {
				t.Fatalf("expected a close match for %q in %v, got fallback %v", tc.want, tc.candidates, got)
			}
			if len(got) != 1 || got[0] != tc.expect {
				t.Errorf("SuggestNames(%q) = %v, want [%s]", tc.want, got, tc.expect)
			}
		})
	}
}

func TestSuggestNames_FallsBackToAllNames(t *testing.T) {
	// The acceptance case: "dimensions" shares nothing with height/width,
	// so there's no close match — but we still show what IS available
	// rather than a dead end.
	got, isClose := SuggestNames("dimensions", []string{"height", "width"})
	if isClose {
		t.Errorf("height/width are not close to 'dimensions', got isClose=true")
	}
	if len(got) != 2 || got[0] != "height" || got[1] != "width" {
		t.Errorf("fallback should list all names, got %v", got)
	}
}

func TestSuggestNames_ExcludesExactMatch(t *testing.T) {
	// An exact match is never a suggestion — it would have been found.
	got, isClose := SuggestNames("height", []string{"height"})
	if isClose {
		t.Errorf("exact match should not count as a close suggestion, got %v", got)
	}
}

func TestSuggestNames_IgnoresShortAccidentalOverlap(t *testing.T) {
	// "id" appears inside "invalid" — a 2-char overlap is noise, not a hint.
	if _, isClose := SuggestNames("invalid", []string{"id"}); isClose {
		t.Error("2-char overlap should not qualify as close")
	}
}

func TestSuggestNames_CapsSuggestionCount(t *testing.T) {
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, fmt.Sprintf("method_%d", i))
	}

	got, isClose := SuggestNames("method_", many)
	if !isClose {
		t.Fatal("shared prefix should qualify as close")
	}
	if len(got) != maxSuggestions {
		t.Errorf("suggestions = %d, want capped at %d", len(got), maxSuggestions)
	}
}

func TestSuggestNames_NoCandidates(t *testing.T) {
	got, isClose := SuggestNames("anything", nil)
	if isClose || len(got) != 0 {
		t.Errorf("no candidates should yield no suggestions, got %v (close=%v)", got, isClose)
	}
}
