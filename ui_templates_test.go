package main

import "testing"

func TestDefaultCustomThemesAllSanitize(t *testing.T) {
	themes := defaultCustomThemes()
	if len(themes) == 0 {
		t.Fatal("expected at least one default theme")
	}
	seen := map[string]bool{}
	for _, th := range themes {
		clean, err := sanitizeCustomTheme(th)
		if err != nil {
			t.Errorf("default theme %q failed sanitization: %v", th.ID, err)
			continue
		}
		if seen[clean.ID] {
			t.Errorf("duplicate default theme id %q", clean.ID)
		}
		seen[clean.ID] = true
		if clean.ID != th.ID {
			t.Errorf("theme id changed during sanitize: %q -> %q", th.ID, clean.ID)
		}
	}
}

func TestScenarioTemplatesReferenceValidThemes(t *testing.T) {
	themes := defaultCustomThemes()
	known := map[string]bool{}
	for _, th := range themes {
		known[th.ID] = true
	}
	seenID := map[string]bool{}
	for _, tmpl := range scenarioTemplates() {
		if tmpl.ID == "" || tmpl.Label == "" || tmpl.Description == "" {
			t.Errorf("template %+v missing required fields", tmpl)
		}
		if seenID[tmpl.ID] {
			t.Errorf("duplicate scenario template id %q", tmpl.ID)
		}
		seenID[tmpl.ID] = true
		if !isBuiltinTheme(tmpl.Theme) && !known[tmpl.Theme] {
			t.Errorf("template %q references unknown theme %q", tmpl.ID, tmpl.Theme)
		}
		norm := normalizeUIConfig(tmpl.Config)
		if !norm.Panels[norm.DefaultPanel] {
			t.Errorf("template %q: default panel %q ends up disabled after normalization", tmpl.ID, norm.DefaultPanel)
		}
	}
}

func TestFindScenarioTemplate(t *testing.T) {
	tmpl, ok := findScenarioTemplate("support-widget")
	if !ok || tmpl.Theme == "" {
		t.Fatalf("expected to find support-widget template, got %+v ok=%v", tmpl, ok)
	}
	if _, ok := findScenarioTemplate("does-not-exist"); ok {
		t.Error("unknown template id should not be found")
	}
}

func TestLoadOrCreateSettingsSeedsDefaultThemes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	defaults := defaultSettingsFromFlags("http://x", "c", "e", "de", 500, 3)
	ss, err := loadOrCreateSettings(path, defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings failed: %v", err)
	}
	got := ss.get()
	if len(got.CustomThemes) == 0 {
		t.Fatal("expected default custom themes to be seeded on first run")
	}
	found := false
	for _, th := range got.CustomThemes {
		if th.ID == "corporate" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'corporate' among seeded default themes")
	}
}
