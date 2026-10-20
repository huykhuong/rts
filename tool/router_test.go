package tool

import (
	"strings"
	"testing"
)

// --- classify: extension & basename detection ---

func TestClassify_RubyByExtension(t *testing.T) {
	cases := []string{
		"app/models/user.rb",
		"lib/tasks/deploy.rake",
		"my_gem.gemspec",
		"USER.RB", // uppercase should still classify as Ruby
	}
	for _, path := range cases {
		if got := classify(path); got != kindRuby {
			t.Errorf("classify(%q) = %v, want kindRuby", path, got)
		}
	}
}

func TestClassify_RubyByBasename(t *testing.T) {
	cases := []string{
		"Rakefile",
		"project/Rakefile",
		"Gemfile",
		"vendor/some/Gemfile",
	}
	for _, path := range cases {
		if got := classify(path); got != kindRuby {
			t.Errorf("classify(%q) = %v, want kindRuby", path, got)
		}
	}
}

func TestClassify_KnownPassthroughTypes(t *testing.T) {
	cases := []string{
		"README.md",
		"notes.txt",
		"app/views/home/index.html.erb", // .erb is what filepath.Ext returns
		"app/views/api/show.json.erb",
		"app/assets/stylesheets/app.css",
		"config/database.yml",
		"package.json",
		"tsconfig.json",
		"log/development.log",
	}
	for _, path := range cases {
		if got := classify(path); got != kindPassthrough {
			t.Errorf("classify(%q) = %v, want kindPassthrough", path, got)
		}
	}
}

func TestClassify_SpecialBasenames(t *testing.T) {
	cases := []string{
		"Dockerfile",
		"Gemfile.lock",
		".gitignore",
		".env.example",
		".ruby-version",
	}
	for _, path := range cases {
		if got := classify(path); got != kindPassthrough {
			t.Errorf("classify(%q) = %v, want kindPassthrough", path, got)
		}
	}
}

func TestClassify_UnknownExtension(t *testing.T) {
	cases := []string{
		"some/binary.exe",
		"weird.xyz",
		"no_extension_at_all",
	}
	for _, path := range cases {
		if got := classify(path); got != kindUnknown {
			t.Errorf("classify(%q) = %v, want kindUnknown", path, got)
		}
	}
}

// --- ViewFile: routing behavior ---

func viewFile(t *testing.T, path, source string) string {
	t.Helper()
	var buf strings.Builder
	err := ViewFile(path, strings.NewReader(source), &buf)
	if err != nil {
		t.Fatalf("ViewFile returned error: %v", err)
	}
	return buf.String()
}

func TestViewFile_RubyDelegatesToProcess_SmallFile(t *testing.T) {
	// Small .rb file — should go through Process and hit its passthrough branch.
	source := `class Foo
  def bar
    "hello"
  end
end
`
	got := viewFile(t, "app/models/foo.rb", source)

	// Should have Process's small-file header, NOT the router's own header.
	// The two differ by the type label: the router tags passthrough output
	// with the extension, Process doesn't.
	if !strings.Contains(got, "(full)") {
		t.Errorf("small .rb should trigger Process's small-file header:\n%s", got)
	}
	// Should NOT have the router's passthrough header (that's for non-Ruby files).
	if strings.Contains(got, ".rb") {
		t.Errorf("small .rb should not use router's passthrough header:\n%s", got)
	}
	if !strings.Contains(got, `"hello"`) {
		t.Error("small .rb should include the method body")
	}
}

func TestViewFile_RubyDelegatesToProcess_LargeFile(t *testing.T) {
	// Large .rb file — should go through Process and hit its skeleton branch.
	source := padToLarge(`class BigClass
  def method_one
    "body 1"
  end

  def method_two
    "body 2"
  end
end
`)
	got := viewFile(t, "app/models/big_class.rb", source)

	if !strings.Contains(got, "skeleton view") {
		t.Errorf("large .rb should trigger Process's skeleton header:\n%s", got)
	}
	// Bodies should be stripped in skeleton mode
	if strings.Contains(got, `"body 1"`) {
		t.Errorf("skeleton should not include method bodies:\n%s", got)
	}
}

func TestViewFile_MarkdownPassthrough(t *testing.T) {
	source := `# Project README

Some **markdown** content.

- bullet
- another bullet
`
	got := viewFile(t, "README.md", source)

	// Router's passthrough header — labeled with the extension
	if !strings.Contains(got, "(full)") {
		t.Errorf(".md should trigger router's passthrough header:\n%s", got)
	}
	if !strings.Contains(got, ".md") {
		t.Errorf("passthrough header should mention .md extension:\n%s", got)
	}
	// Full content preserved
	if !strings.Contains(got, "Some **markdown** content.") {
		t.Error(".md content should be preserved verbatim")
	}
	if !strings.Contains(got, "- bullet") {
		t.Error(".md bullet list should be preserved")
	}
}

func TestViewFile_JSONPassthrough(t *testing.T) {
	source := `{
  "name": "my-app",
  "version": "1.0.0"
}
`
	got := viewFile(t, "package.json", source)

	if !strings.Contains(got, "(full)") {
		t.Errorf(".json should trigger passthrough header:\n%s", got)
	}
	if !strings.Contains(got, ".json") {
		t.Errorf("header should mention .json:\n%s", got)
	}
	// JSON content preserved exactly — note it's fine that the header
	// is a # comment even though # isn't valid JSON, since Claude reads
	// the response as text, not as parsed JSON.
	if !strings.Contains(got, `"name": "my-app"`) {
		t.Error(".json content should be preserved verbatim")
	}
}

func TestViewFile_ErbPassthrough(t *testing.T) {
	// filepath.Ext returns the LAST extension, so .html.erb → .erb
	source := `<h1>Hello, <%= @user.name %></h1>
`
	got := viewFile(t, "app/views/home/index.html.erb", source)

	if !strings.Contains(got, "(full)") {
		t.Errorf(".erb should trigger passthrough header:\n%s", got)
	}
	if !strings.Contains(got, "@user.name") {
		t.Error(".erb content should be preserved verbatim")
	}
}

func TestViewFile_UnknownExtensionStillPassesThrough(t *testing.T) {
	// Permissive default: unknown extension → still return content
	source := "some mystery content\nline two\n"
	got := viewFile(t, "weird.xyz", source)

	if !strings.Contains(got, "(full)") {
		t.Errorf("unknown ext should still passthrough:\n%s", got)
	}
	if !strings.Contains(got, "unknown type") {
		t.Errorf("unknown ext header should say 'unknown type':\n%s", got)
	}
	if !strings.Contains(got, "some mystery content") {
		t.Error("unknown ext content should be preserved verbatim")
	}
}

func TestViewFile_DockerfilePassthrough(t *testing.T) {
	// Extension-less special file
	source := `FROM ruby:3.2
WORKDIR /app
`
	got := viewFile(t, "Dockerfile", source)

	if !strings.Contains(got, "(full)") {
		t.Errorf("Dockerfile should passthrough:\n%s", got)
	}
	// describeKind falls back to basename for extension-less files
	if !strings.Contains(got, "Dockerfile") {
		t.Errorf("header should mention Dockerfile basename:\n%s", got)
	}
	if !strings.Contains(got, "FROM ruby:3.2") {
		t.Error("Dockerfile content should be preserved")
	}
}

func TestViewFile_RakefileTreatedAsRuby(t *testing.T) {
	// Rakefile has no .rb extension but is Ruby source — should go through Process.
	source := `task :hello do
  puts "hi"
end
`
	got := viewFile(t, "Rakefile", source)

	// Rakefile is small so Process will passthrough — but with Process's
	// small-file header, not the router's (which would tag it "Rakefile").
	if !strings.Contains(got, "(full)") {
		t.Errorf("Rakefile should route through Process (small file):\n%s", got)
	}
	if strings.Contains(got, "# rts: 3 lines Rakefile") {
		t.Errorf("Rakefile should not use the router's passthrough header:\n%s", got)
	}
	if !strings.Contains(got, `puts "hi"`) {
		t.Error("Rakefile content should be preserved")
	}
}

func TestViewFile_LineCountInHeader(t *testing.T) {
	// The passthrough header advertises line count — verify it's correct.
	source := "line 1\nline 2\nline 3\n"
	got := viewFile(t, "notes.txt", source)

	if !strings.Contains(got, "3 lines") {
		t.Errorf("passthrough header should report 3 lines:\n%s", got)
	}
}
func TestClassify_Scripts(t *testing.T) {
	for _, path := range []string{"app/javascript/controllers/foo.js", "src/server/blobs.ts", "src/client/ui/panel.tsx"} {
		if got := classify(path); got != kindScript {
			t.Errorf("classify(%q) = %v, want kindScript", path, got)
		}
	}
	if got := classify("src/types/vendor.d.ts"); got == kindScript {
		t.Error("a .d.ts declaration file should not get the script outline")
	}
}
