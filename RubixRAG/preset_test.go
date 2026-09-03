package main

import "testing"

func TestFindPreset(t *testing.T) {
	presets := []sourcePreset{
		{Name: "intern", DisplayName: "Intern (alles)"},
		{Name: "kunde", DisplayName: "Kundenkommunikation", Kinds: []string{"file", "shop"}, Tools: []string{"shop"}},
	}

	got, ok := findPreset(presets, "Kunde") // case-insensitive, matching sourceAccessAllowed's style
	if !ok || got.Name != "kunde" {
		t.Fatalf("want case-insensitive match for 'kunde', got %+v (ok=%v)", got, ok)
	}

	_, ok = findPreset(presets, "unbekannt")
	if ok {
		t.Fatalf("want no match for an undefined preset name")
	}

	got, ok = findPreset(presets, "")
	if ok || got.Name != "" || len(got.Kinds) != 0 {
		t.Fatalf("want zero-value, not-ok for an empty name, got %+v (ok=%v)", got, ok)
	}
}

func TestPresetAllowsKind(t *testing.T) {
	if !presetAllowsKind(nil, "pst_email") {
		t.Fatalf("empty kinds list must mean unrestricted")
	}
	kinds := []string{"file", "shop"}
	if !presetAllowsKind(kinds, "Shop") { // case-insensitive
		t.Fatalf("want case-insensitive match for an allowed kind")
	}
	if presetAllowsKind(kinds, "pst_email") {
		t.Fatalf("want a kind outside the list to be disallowed")
	}
}

func TestPresetAllowsTool(t *testing.T) {
	if !presetAllowsTool(nil, "mssql") {
		t.Fatalf("empty tools list must mean unrestricted")
	}
	tools := []string{"shop"}
	if !presetAllowsTool(tools, "SHOP") { // case-insensitive
		t.Fatalf("want case-insensitive match for an allowed tool")
	}
	if presetAllowsTool(tools, "mssql") {
		t.Fatalf("want a tool outside the list to be disallowed")
	}
}

// TestValidatePresets confirms the two structural failures that previously
// saved silently: a blank Name (unreferenceable by DefaultPreset/DraftPreset/
// askRequest.Preset) and a duplicate Name (findPreset's lookup would only
// ever see whichever entry comes first, silently discarding the other).
func TestValidatePresets(t *testing.T) {
	cases := []struct {
		name    string
		presets []sourcePreset
		wantErr bool
	}{
		{"nil is valid", nil, false},
		{"one well-formed preset", []sourcePreset{{Name: "kunde"}}, false},
		{"distinct names valid", []sourcePreset{{Name: "kunde"}, {Name: "intern"}}, false},
		{"blank name", []sourcePreset{{Name: "  "}}, true},
		{"duplicate name", []sourcePreset{{Name: "kunde"}, {Name: "kunde"}}, true},
		{"duplicate name case-insensitive", []sourcePreset{{Name: "Kunde"}, {Name: "kunde"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePresets(c.presets)
			if (err != nil) != c.wantErr {
				t.Fatalf("validatePresets(%+v): wantErr=%v, got %v", c.presets, c.wantErr, err)
			}
		})
	}
}

func TestFilterByPresetKinds(t *testing.T) {
	hits := []rankedHit{
		{SourceID: "wiki-1", SourceKind: "confluence_page"},
		{SourceID: "mail-1", SourceKind: "pst_email"},
	}

	// No preset kinds configured at all: nothing filtered — same
	// opt-out-not-opt-in default as filterByDeptAccess.
	got := filterByPresetKinds(hits, nil)
	if len(got) != 2 {
		t.Fatalf("no preset kinds: want both hits unfiltered, got %+v", got)
	}

	// Preset restricted to confluence_page: pst_email is dropped.
	got = filterByPresetKinds(hits, []string{"confluence_page"})
	if len(got) != 1 || got[0].SourceID != "wiki-1" {
		t.Fatalf("want only wiki-1 to survive, got %+v", got)
	}
}
