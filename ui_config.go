package main

// ─────────────────────────────────────────────────────────────────────────────
// UI configuration & theming
//
// tinyRAG is meant to be a base/blueprint for arbitrary RAG frontends, so the
// web UI is data-driven: which panels and chat modes exist, which suggestion
// buttons are shown, and how the app looks (themes) all come from settings and
// are served via GET /api/ui. Operators can reshape the UI without touching
// HTML/CSS/JS:
//
//   - Custom themes are CSS-variable maps applied client-side via
//     style.setProperty (never injected as CSS text). Values are additionally
//     sanitized server-side (no braces, semicolons, angle brackets or url()).
//   - uiConfig toggles panels (chat/search/ingest/stats), chat modes
//     (auto_search/deep/offline/agent/debug), pickers, custom suggestion
//     buttons and the footer text.
//
// Built-in themes live in style.css as [data-theme="…"] blocks; the server
// only knows their IDs so it can validate selections and list them alongside
// custom themes.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"regexp"
	"strings"
)

// builtinThemes lists the themes shipped in style.css.
var builtinThemes = []map[string]string{
	{"id": "dark", "label": "Dark"},
	{"id": "light", "label": "Light"},
	{"id": "nord", "label": "Nord"},
	{"id": "solarized", "label": "Solarized"},
	{"id": "monokai", "label": "Monokai"},
	{"id": "dracula", "label": "Dracula"},
}

// uiThemeDef is a custom theme: a named set of CSS-variable overrides that is
// applied on top of a built-in base theme.
type uiThemeDef struct {
	ID    string            `json:"id"`
	Label string            `json:"label"`
	Base  string            `json:"base"` // built-in theme the vars build upon ("dark"/"light"/…)
	Vars  map[string]string `json:"vars"` // e.g. "--accent": "#ff6600"
}

// builtinDensities lists the layout density modes the frontend understands.
// Unlike themes these are pure CSS-variable presets baked into style.css
// ([data-density="compact"]); there is no custom-density mechanism.
var builtinDensities = []map[string]string{
	{"id": "comfortable", "label": "Comfortable"},
	{"id": "compact", "label": "Compact"},
}

// normalizeDensity maps arbitrary input to a valid density mode. Empty or
// unrecognized input defaults to "comfortable".
func normalizeDensity(raw string) string {
	if strings.ToLower(strings.TrimSpace(raw)) == "compact" {
		return "compact"
	}
	return "comfortable"
}

// uiSuggestion is one suggestion button on the empty chat screen.
type uiSuggestion struct {
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
}

// uiConfig controls which parts of the web UI are visible.
//
// Defaults are minimalist by design: only what's needed to actually use
// tinyRAG out of the box (chat, search, ingest, auto-search) is shown; every
// power-user/admin control (deep research, offline mode, agent planning,
// debug, the persona/role/LLM pickers, the workspace status strip) is
// hidden until explicitly enabled — via Settings, a scenario template, or by
// setting the corresponding field to true here.
type uiConfig struct {
	// DefaultPanel is the panel activated on load: chat|search|ingest|stats.
	DefaultPanel string `json:"default_panel"`
	// Panels toggles main panels. Missing keys default to true, except
	// "stats" which defaults to false (see uiDefaultPanelVisible).
	Panels map[string]bool `json:"panels"`
	// Modes toggles the chat mode checkboxes. Missing keys default to false,
	// except "auto_search" which defaults to true (see uiDefaultModeVisible).
	Modes map[string]bool `json:"modes"`
	// ShowPersonaPicker / ShowRolePicker / ShowLLMSwitcher toggle toolbar
	// pickers. Default false (hidden) — accessible via Settings regardless.
	ShowPersonaPicker bool `json:"show_persona_picker"`
	ShowRolePicker    bool `json:"show_role_picker"`
	ShowLLMSwitcher   bool `json:"show_llm_switcher"`
	// ShowWorkspaceStrip toggles the status pill row above the chat
	// (provider/persona/role/mode). Default false (hidden) — it's status
	// display, not needed to use the chat.
	ShowWorkspaceStrip bool `json:"show_workspace_strip"`
	// Suggestions replaces the default empty-state suggestion buttons.
	Suggestions []uiSuggestion `json:"suggestions,omitempty"`
	// FooterText replaces the chat disclaimer line. Empty = keep default.
	FooterText string `json:"footer_text,omitempty"`
}

var uiKnownPanels = []string{"chat", "search", "ingest", "stats"}
var uiKnownModes = []string{"auto_search", "deep", "offline", "agent", "debug"}

// uiDefaultPanelVisible / uiDefaultModeVisible give the minimalist defaults
// applied to any panel/mode key missing from a uiConfig — only the
// essentials (chat, search, ingest, auto-search) are visible out of the box.
var uiDefaultPanelVisible = map[string]bool{"chat": true, "search": true, "ingest": true, "stats": false}
var uiDefaultModeVisible = map[string]bool{"auto_search": true, "deep": false, "offline": false, "agent": false, "debug": false}

var themeIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var themeVarNameRe = regexp.MustCompile(`^--[a-z0-9][a-z0-9-]{0,39}$`)

// themeVarValueOK reports whether a CSS variable value is safe to store.
// Values are applied via style.setProperty (not CSS text), but we still
// reject anything that could smuggle CSS constructs or external loads.
func themeVarValueOK(v string) bool {
	if v == "" || len(v) > 96 {
		return false
	}
	if strings.ContainsAny(v, "{};<>\\") {
		return false
	}
	low := strings.ToLower(v)
	if strings.Contains(low, "url(") || strings.Contains(low, "expression") || strings.Contains(low, "@import") || strings.Contains(low, "javascript:") {
		return false
	}
	return true
}

// isBuiltinTheme reports whether id names a theme shipped in style.css.
func isBuiltinTheme(id string) bool {
	for _, t := range builtinThemes {
		if t["id"] == id {
			return true
		}
	}
	return false
}

// sanitizeCustomTheme validates and normalizes a custom theme definition.
func sanitizeCustomTheme(t uiThemeDef) (uiThemeDef, error) {
	t.ID = strings.ToLower(strings.TrimSpace(t.ID))
	if !themeIDRe.MatchString(t.ID) {
		return t, fmt.Errorf("invalid theme id (allowed: a-z 0-9 - _, max 32 chars)")
	}
	if isBuiltinTheme(t.ID) {
		return t, fmt.Errorf("theme id %q collides with a built-in theme", t.ID)
	}
	t.Label = strings.TrimSpace(t.Label)
	if t.Label == "" {
		t.Label = t.ID
	}
	if len(t.Label) > 40 {
		t.Label = t.Label[:40]
	}
	if !isBuiltinTheme(t.Base) {
		t.Base = "dark"
	}
	if len(t.Vars) == 0 {
		return t, fmt.Errorf("theme needs at least one CSS variable")
	}
	if len(t.Vars) > 48 {
		return t, fmt.Errorf("too many CSS variables (max 48)")
	}
	clean := make(map[string]string, len(t.Vars))
	for k, v := range t.Vars {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !themeVarNameRe.MatchString(k) {
			return t, fmt.Errorf("invalid CSS variable name %q", k)
		}
		if !themeVarValueOK(v) {
			return t, fmt.Errorf("invalid CSS variable value for %q", k)
		}
		clean[k] = v
	}
	t.Vars = clean
	return t, nil
}

// normalizeUIConfig fills defaults so the frontend can rely on complete maps.
func normalizeUIConfig(c uiConfig) uiConfig {
	switch c.DefaultPanel {
	case "chat", "search", "ingest", "stats":
	default:
		c.DefaultPanel = "chat"
	}
	panels := make(map[string]bool, len(uiKnownPanels))
	for _, p := range uiKnownPanels {
		if v, ok := c.Panels[p]; ok {
			panels[p] = v
		} else {
			panels[p] = uiDefaultPanelVisible[p]
		}
	}
	// The default panel must stay reachable.
	panels[c.DefaultPanel] = true
	c.Panels = panels

	modes := make(map[string]bool, len(uiKnownModes))
	for _, m := range uiKnownModes {
		if v, ok := c.Modes[m]; ok {
			modes[m] = v
		} else {
			modes[m] = uiDefaultModeVisible[m]
		}
	}
	c.Modes = modes

	if len(c.Suggestions) > 8 {
		c.Suggestions = c.Suggestions[:8]
	}
	valid := make([]uiSuggestion, 0, len(c.Suggestions))
	for _, s := range c.Suggestions {
		s.Label = strings.TrimSpace(s.Label)
		s.Prompt = strings.TrimSpace(s.Prompt)
		if s.Label == "" || s.Prompt == "" {
			continue
		}
		if len(s.Label) > 40 {
			s.Label = s.Label[:40]
		}
		if len(s.Prompt) > 300 {
			s.Prompt = s.Prompt[:300]
		}
		valid = append(valid, s)
	}
	c.Suggestions = valid

	if len(c.FooterText) > 200 {
		c.FooterText = c.FooterText[:200]
	}
	return c
}

// upsertCustomTheme inserts or replaces a custom theme by ID.
func upsertCustomTheme(themes []uiThemeDef, t uiThemeDef) []uiThemeDef {
	for i := range themes {
		if themes[i].ID == t.ID {
			themes[i] = t
			return themes
		}
	}
	return append(themes, t)
}

// removeCustomTheme deletes a custom theme by ID; reports whether it existed.
func removeCustomTheme(themes []uiThemeDef, id string) ([]uiThemeDef, bool) {
	for i := range themes {
		if themes[i].ID == id {
			return append(themes[:i], themes[i+1:]...), true
		}
	}
	return themes, false
}
