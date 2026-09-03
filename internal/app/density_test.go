package app

import "testing"

func TestNormalizeDensity(t *testing.T) {
	cases := map[string]string{
		"":            "comfortable",
		"comfortable": "comfortable",
		"compact":     "compact",
		"Compact":     "compact",
		" COMPACT ":   "compact",
		"garbage":     "comfortable",
	}
	for in, want := range cases {
		if got := normalizeDensity(in); got != want {
			t.Errorf("normalizeDensity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuiltinDensitiesListsBothModes(t *testing.T) {
	ids := map[string]bool{}
	for _, d := range builtinDensities {
		ids[d["id"]] = true
	}
	if !ids["comfortable"] || !ids["compact"] {
		t.Errorf("expected both comfortable and compact in builtinDensities, got %+v", builtinDensities)
	}
}

func TestLoadOrCreateSettingsNormalizesDensity(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	defaults := defaultSettingsFromFlags("http://x", "c", "e", "de", 500, 3)
	ss, err := loadOrCreateSettings(path, defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings failed: %v", err)
	}
	if got := ss.get().Density; got != "comfortable" {
		t.Errorf("expected default density 'comfortable', got %q", got)
	}
}
