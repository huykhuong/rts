package mcp

import (
	"strings"
	"testing"

	"rts/index"
)

// graphProject is knowledgeProject with a symbol index, which record_feature
// checks every name against.
func graphProject(t *testing.T) string {
	t.Helper()

	root := knowledgeProject(t)
	writeInto(t, root, "app/models/account.rb", "class Account\nend\n")
	if err := index.Write(root, []index.Symbol{
		{Name: "User", Kind: "class", Path: "app/models/user.rb", Line: 1},
		{Name: "Account", Kind: "class", Path: "app/models/account.rb", Line: 1},
	}); err != nil {
		t.Fatalf("index.Write: %v", err)
	}
	return root
}

func TestRecordFeatureThenTouchpoints(t *testing.T) {
	root := graphProject(t)

	saved := resultText(t, callTool(t, root, "record_feature", map[string]interface{}{
		"feature": "accounts",
		"summary": "Users belong to an account.",
		"members": []map[string]string{{"ref": "Account", "role": "model"}, {"ref": "User", "role": "model"}},
		"edges":   []map[string]string{{"src": "Account", "dst": "User", "kind": "has_many"}},
		"notes":   []string{"An account is never deleted while it has users."},
	}))
	if !strings.Contains(saved, `Saved feature "accounts": 2 members, 1 edges, 1 notes`) {
		t.Errorf("record_feature did not confirm the save:\n%s", saved)
	}

	text := resultText(t, callTool(t, root, "touchpoints", map[string]interface{}{"name": "accounts"}))
	for _, want := range []string{"Feature: accounts", "has_many → User", "app/models/account.rb", "never deleted"} {
		if !strings.Contains(text, want) {
			t.Errorf("touchpoints is missing %q:\n%s", want, text)
		}
	}

	brief := resultText(t, callTool(t, root, "project_map", map[string]interface{}{}))
	if !strings.Contains(brief, "--- Mapped features (1) ---") || !strings.Contains(brief, "accounts — 2 members") {
		t.Errorf("project_map does not list the mapped feature:\n%s", brief)
	}
}

func TestRecordFeatureRefusesAMapWithAnUnknownName(t *testing.T) {
	root := graphProject(t)

	out := callTool(t, root, "record_feature", map[string]interface{}{
		"feature": "accounts",
		"summary": "Users belong to an account.",
		"members": []map[string]string{{"ref": "Acount"}},
	})
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "Nothing was saved") || !strings.Contains(out, `Acount`) {
		t.Fatalf("expected a refusal naming the bad ref, got: %s", out)
	}

	text := resultText(t, callTool(t, root, "touchpoints", map[string]interface{}{"name": "accounts"}))
	if !strings.Contains(text, "No features are mapped") {
		t.Errorf("a refused map left something behind:\n%s", text)
	}
}

func TestRecordFeatureRefusesANearDuplicateUnlessDeclaredDistinct(t *testing.T) {
	root := graphProject(t)
	writeInto(t, root, "app/models/membership.rb", "class Membership\nend\n")

	accounts := map[string]interface{}{
		"feature": "accounts",
		"summary": "Users belong to an account.",
		"members": []map[string]string{{"ref": "Account"}, {"ref": "User"}, {"ref": "app/models/membership.rb"}},
	}
	resultText(t, callTool(t, root, "record_feature", accounts))

	dup := map[string]interface{}{
		"feature": "account users",
		"summary": "The same thing under another name.",
		"members": []map[string]string{{"ref": "Account"}, {"ref": "User"}, {"ref": "app/models/membership.rb"}},
	}
	out := callTool(t, root, "record_feature", dup)
	if !strings.Contains(out, `\"accounts\" already has 3 of this map's 3 files`) || !strings.Contains(out, "Nothing was saved") {
		t.Fatalf("a near-duplicate should be refused naming the feature it repeats: %s", out)
	}

	dup["distinct_from"] = []string{"accounts"}
	if text := resultText(t, callTool(t, root, "record_feature", dup)); !strings.Contains(text, `Saved feature "account users"`) {
		t.Errorf("distinct_from should let a declared-different feature save: %s", text)
	}
}

func TestFindSymbolReindexesOnAMissWhenTheCodeHasMoved(t *testing.T) {
	root := graphProject(t)
	fp, err := index.BuildFingerprints(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.WriteFingerprints(root, fp); err != nil {
		t.Fatal(err)
	}

	calls := 0
	previous := reindexNow
	reindexNow = func(dir string) error {
		calls++
		syms, err := index.BuildIndex(dir)
		if err != nil {
			return err
		}
		if err := index.Write(dir, syms); err != nil {
			return err
		}
		fp, err := index.BuildFingerprints(dir)
		if err != nil {
			return err
		}
		return index.WriteFingerprints(dir, fp)
	}
	t.Cleanup(func() { reindexNow = previous })

	// A miss with nothing changed costs the check and nothing more.
	if text := resultText(t, callTool(t, root, "find_symbol", map[string]interface{}{"name": "Invoice"})); strings.Contains(text, "re-indexed") || calls != 0 {
		t.Fatalf("an up-to-date index should not be rebuilt on a miss (calls=%d):\n%s", calls, text)
	}

	// A pull brings a class the index has never seen.
	writeInto(t, root, "app/models/invoice.rb", "class Invoice\nend\n")
	text := resultText(t, callTool(t, root, "find_symbol", map[string]interface{}{"name": "Invoice"}))
	if calls != 1 || !strings.Contains(text, "re-indexed before answering") || !strings.Contains(text, "app/models/invoice.rb") {
		t.Errorf("a miss on a stale index should reindex and then find the class (calls=%d):\n%s", calls, text)
	}
}
