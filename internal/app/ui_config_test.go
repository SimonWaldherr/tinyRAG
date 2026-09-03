package app

import (
	"strings"
	"testing"
)

func validTestTheme() uiThemeDef {
	return uiThemeDef{
		ID:    "corporate",
		Label: "Corporate",
		Base:  "light",
		Vars:  map[string]string{"--accent": "#ff6600", "--bg": "#fafafa"},
	}
}

func TestSanitizeCustomThemeValid(t *testing.T) {
	clean, err := sanitizeCustomTheme(validTestTheme())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clean.ID != "corporate" || clean.Base != "light" || len(clean.Vars) != 2 {
		t.Fatalf("unexpected sanitized theme: %+v", clean)
	}
}

func TestSanitizeCustomThemeRejectsBadIDs(t *testing.T) {
	for _, id := range []string{"", "UPPER CASE", "a b", "ä", strings.Repeat("x", 40), "-leading"} {
		th := validTestTheme()
		th.ID = id
		if _, err := sanitizeCustomTheme(th); err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
	// Built-in collision
	th := validTestTheme()
	th.ID = "dark"
	if _, err := sanitizeCustomTheme(th); err == nil {
		t.Error("built-in theme id must be rejected")
	}
}

func TestSanitizeCustomThemeRejectsBadVars(t *testing.T) {
	cases := []map[string]string{
		{"accent": "#fff"},                     // missing -- prefix
		{"--accent": "red; } body { color: x"}, // css injection attempt
		{"--accent": "url(https://evil)"},      // external load
		{"--accent": "expression(alert(1))"},   // legacy IE expression
		{"--accent": "<script>"},               // markup
		{"--accent": ""},                       // empty
	}
	for _, vars := range cases {
		th := validTestTheme()
		th.Vars = vars
		if _, err := sanitizeCustomTheme(th); err == nil {
			t.Errorf("vars %v should be rejected", vars)
		}
	}
}

func TestSanitizeCustomThemeDefaults(t *testing.T) {
	th := validTestTheme()
	th.Label = ""
	th.Base = "no-such-base"
	clean, err := sanitizeCustomTheme(th)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clean.Label != clean.ID {
		t.Errorf("empty label should fall back to id, got %q", clean.Label)
	}
	if clean.Base != "dark" {
		t.Errorf("unknown base should fall back to dark, got %q", clean.Base)
	}
}

// TestNormalizeUIConfigDefaults locks in the minimalist-by-default policy:
// a blank uiConfig{} shows only what's needed to use tinyRAG (chat, search,
// ingest, auto-search) and hides every power-user/admin control.
func TestNormalizeUIConfigDefaults(t *testing.T) {
	c := normalizeUIConfig(uiConfig{})
	if c.DefaultPanel != "chat" {
		t.Errorf("default panel should be chat, got %q", c.DefaultPanel)
	}
	wantPanels := map[string]bool{"chat": true, "search": true, "ingest": true, "stats": false}
	for p, want := range wantPanels {
		if c.Panels[p] != want {
			t.Errorf("panel %q default = %v, want %v", p, c.Panels[p], want)
		}
	}
	wantModes := map[string]bool{"auto_search": true, "deep": false, "offline": false, "agent": false, "debug": false}
	for m, want := range wantModes {
		if c.Modes[m] != want {
			t.Errorf("mode %q default = %v, want %v", m, c.Modes[m], want)
		}
	}
	if c.ShowPersonaPicker || c.ShowRolePicker || c.ShowLLMSwitcher {
		t.Errorf("toolbar pickers should be hidden by default, got persona=%v role=%v llm=%v",
			c.ShowPersonaPicker, c.ShowRolePicker, c.ShowLLMSwitcher)
	}
	if c.ShowWorkspaceStrip {
		t.Error("workspace strip should be hidden by default")
	}
}

func TestNormalizeUIConfigKeepsDefaultPanelReachable(t *testing.T) {
	c := normalizeUIConfig(uiConfig{
		DefaultPanel: "stats",
		Panels:       map[string]bool{"stats": false, "search": false},
	})
	if !c.Panels["stats"] {
		t.Error("the default panel must stay enabled")
	}
	if c.Panels["search"] {
		t.Error("explicitly disabled panel should stay disabled")
	}
}

func TestNormalizeUIConfigSuggestions(t *testing.T) {
	sugg := make([]uiSuggestion, 0, 12)
	for i := 0; i < 12; i++ {
		sugg = append(sugg, uiSuggestion{Label: "L", Prompt: "P"})
	}
	sugg = append(sugg, uiSuggestion{Label: " ", Prompt: "x"}) // invalid, dropped
	c := normalizeUIConfig(uiConfig{Suggestions: sugg})
	if len(c.Suggestions) != 8 {
		t.Errorf("suggestions should be capped at 8, got %d", len(c.Suggestions))
	}
	long := uiSuggestion{Label: strings.Repeat("a", 60), Prompt: strings.Repeat("b", 400)}
	c = normalizeUIConfig(uiConfig{Suggestions: []uiSuggestion{long}})
	if len(c.Suggestions[0].Label) != 40 || len(c.Suggestions[0].Prompt) != 300 {
		t.Errorf("suggestion label/prompt should be truncated, got %d/%d",
			len(c.Suggestions[0].Label), len(c.Suggestions[0].Prompt))
	}
}

func TestUpsertAndRemoveCustomTheme(t *testing.T) {
	var themes []uiThemeDef
	a, _ := sanitizeCustomTheme(validTestTheme())
	themes = upsertCustomTheme(themes, a)
	if len(themes) != 1 {
		t.Fatalf("expected 1 theme, got %d", len(themes))
	}
	a.Label = "Renamed"
	themes = upsertCustomTheme(themes, a)
	if len(themes) != 1 || themes[0].Label != "Renamed" {
		t.Fatalf("upsert should replace by id, got %+v", themes)
	}
	themes, found := removeCustomTheme(themes, "corporate")
	if !found || len(themes) != 0 {
		t.Fatalf("remove failed: found=%v len=%d", found, len(themes))
	}
	if _, found := removeCustomTheme(themes, "nope"); found {
		t.Error("removing unknown theme must report found=false")
	}
}

func TestCLIPalette(t *testing.T) {
	on := cliPalette{on: true}
	off := cliPalette{on: false}
	if got := off.accent("x"); got != "x" {
		t.Errorf("disabled palette must pass through, got %q", got)
	}
	if got := on.accent("x"); !strings.Contains(got, "\x1b[36m") || !strings.Contains(got, "\x1b[0m") {
		t.Errorf("enabled palette must wrap with ANSI codes, got %q", got)
	}
}
