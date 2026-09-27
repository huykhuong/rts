package index

import "testing"

func TestFind_MatchesAFullyQualifiedName(t *testing.T) {
	// Searching a class by its real, complete name used to return nothing:
	// tier 1 compared against the bare name, tier 2 against the last
	// segment, and tier 3 required a strict prefix before "::"+query. So the
	// most confident possible query was the one guaranteed to fail.
	idx := &IndexFile{Symbols: []Symbol{
		{Name: "PdfDownloadsController", Kind: "class", Path: "a.rb", Line: 1, Container: "Projects::Reports"},
		{Name: "PdfDownloadsController", Kind: "class", Path: "b.rb", Line: 1, Container: "ClientPortal"},
	}}

	got := Find(idx, "Projects::Reports::PdfDownloadsController")

	if len(got.Symbols) != 1 {
		t.Fatalf("got %d matches, want exactly the qualified one", len(got.Symbols))
	}
	if got.Symbols[0].Path != "a.rb" {
		t.Errorf("matched %q, want a.rb", got.Symbols[0].Path)
	}
	if got.MatchKind != "exact" {
		t.Errorf("MatchKind = %q, want exact", got.MatchKind)
	}
}

func TestFind_BareNameStillMatchesEveryNamespace(t *testing.T) {
	idx := &IndexFile{Symbols: []Symbol{
		{Name: "PdfRequester", Kind: "class", Path: "a.rb", Line: 1, Container: "Report"},
		{Name: "PdfRequester", Kind: "class", Path: "b.rb", Line: 1, Container: "CustomReport"},
	}}

	if got := Find(idx, "PdfRequester"); len(got.Symbols) != 2 {
		t.Errorf("got %d matches, want both namespaces", len(got.Symbols))
	}
}
