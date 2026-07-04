package main

// ─────────────────────────────────────────────────────────────────────────────
// Ready-made themes & scenario templates
//
// tinyRAG ships a small library of CSS-variable themes (defaultCustomThemes)
// and full deployment scenario templates (scenarioTemplates) that bundle a
// theme with a matching uiConfig — the "blueprint" presets for common RAG
// product shapes: a support widget, a public kiosk, an internal helpdesk,
// a developer console, a research assistant, a legal archive, an education
// portal, and an accessibility-first kiosk.
//
// Ready-made themes are seeded into settings.CustomThemes on first run (see
// loadOrCreateSettings) so they show up immediately in the theme grid; users
// remain free to edit or delete them like any custom theme. Applying a
// scenario template (POST /api/ui/templates/apply) sets both the active
// theme and the uiConfig in one step — a starting point, not a lock-in.
// ─────────────────────────────────────────────────────────────────────────────

// defaultCustomThemes returns the built-in theme library, layered on top of
// the "dark" or "light" base theme via CSS-variable overrides.
func defaultCustomThemes() []uiThemeDef {
	return []uiThemeDef{
		{
			ID: "corporate", Label: "Corporate", Base: "light",
			Vars: map[string]string{
				"--bg": "#f3f5f9", "--panel": "#ffffff", "--panel2": "#eaeef5",
				"--text": "#1c2536", "--muted": "#5b6478", "--border": "#dfe4ee",
				"--accent": "#1d4ed8", "--accent-subtle": "rgba(29,78,216,.10)", "--accent-subtle2": "rgba(29,78,216,.20)",
				"--gradient-secondary": "#334876", "--ok": "#0e8a5f", "--warn": "#b7791f", "--danger": "#c0392b",
				"--sidebar-bg": "#ffffff", "--input-bg": "#ffffff", "--header-bg": "rgba(255,255,255,.94)",
			},
		},
		{
			ID: "healthcare", Label: "Healthcare", Base: "light",
			Vars: map[string]string{
				"--bg": "#f2f8f7", "--panel": "#ffffff", "--panel2": "#e7f3f1",
				"--text": "#173430", "--muted": "#5c7a75", "--border": "#d9ece8",
				"--accent": "#0f9488", "--accent-subtle": "rgba(15,148,136,.10)", "--accent-subtle2": "rgba(15,148,136,.20)",
				"--gradient-secondary": "#3fb8a8", "--ok": "#1a9e6b", "--warn": "#c68a1f", "--danger": "#c0392b",
				"--sidebar-bg": "#ffffff", "--input-bg": "#ffffff", "--header-bg": "rgba(255,255,255,.94)",
			},
		},
		{
			ID: "legal", Label: "Legal", Base: "dark",
			Vars: map[string]string{
				"--bg": "#11141c", "--panel": "#181c27", "--panel2": "#0e1119",
				"--text": "#e6e2d3", "--muted": "#8a8778", "--border": "#2a2f3d",
				"--accent": "#c8a24d", "--accent-subtle": "rgba(200,162,77,.12)", "--accent-subtle2": "rgba(200,162,77,.22)",
				"--gradient-secondary": "#7d8ba8", "--ok": "#7bab6e", "--warn": "#c8a24d", "--danger": "#b5544f",
				"--sidebar-bg": "#0e1119", "--input-bg": "#181c27", "--header-bg": "rgba(14,17,25,.94)", "--code-bg": "#0e1119",
			},
		},
		{
			ID: "education", Label: "Education", Base: "light",
			Vars: map[string]string{
				"--bg": "#faf7fd", "--panel": "#ffffff", "--panel2": "#f1e9fb",
				"--text": "#2c2438", "--muted": "#6f6480", "--border": "#e6dbf5",
				"--accent": "#7c3aed", "--accent-subtle": "rgba(124,58,237,.10)", "--accent-subtle2": "rgba(124,58,237,.20)",
				"--gradient-secondary": "#f97316", "--ok": "#16a34a", "--warn": "#d97706", "--danger": "#dc2626",
				"--sidebar-bg": "#ffffff", "--input-bg": "#ffffff", "--header-bg": "rgba(255,255,255,.94)",
			},
		},
		{
			ID: "high-contrast", Label: "High Contrast", Base: "dark",
			Vars: map[string]string{
				"--bg": "#000000", "--panel": "#0a0a0a", "--panel2": "#000000",
				"--text": "#ffffff", "--muted": "#d0d0d0", "--border": "#ffffff",
				"--accent": "#ffd60a", "--accent-subtle": "rgba(255,214,10,.18)", "--accent-subtle2": "rgba(255,214,10,.30)",
				"--gradient-secondary": "#ffd60a", "--ok": "#5cff5c", "--warn": "#ffd60a", "--danger": "#ff6b6b",
				"--sidebar-bg": "#000000", "--input-bg": "#0a0a0a", "--header-bg": "rgba(0,0,0,.98)", "--code-bg": "#000000",
			},
		},
		{
			ID: "terminal", Label: "Terminal", Base: "dark",
			Vars: map[string]string{
				"--bg": "#0a0f0a", "--panel": "#0e150e", "--panel2": "#080c08",
				"--text": "#c8ffc8", "--muted": "#5f8f5f", "--border": "#1e3a1e",
				"--accent": "#39ff6a", "--accent-subtle": "rgba(57,255,106,.10)", "--accent-subtle2": "rgba(57,255,106,.20)",
				"--gradient-secondary": "#2fd8c8", "--ok": "#39ff6a", "--warn": "#e8e04a", "--danger": "#ff5c5c",
				"--sidebar-bg": "#080c08", "--input-bg": "#0e150e", "--header-bg": "rgba(8,12,8,.94)", "--code-bg": "#050805",
				"--font": "\"JetBrains Mono\",\"Fira Code\",ui-monospace,monospace",
			},
		},
		{
			ID: "sunset", Label: "Sunset", Base: "light",
			Vars: map[string]string{
				"--bg": "#fff4ec", "--panel": "#ffffff", "--panel2": "#ffe7d6",
				"--text": "#3a2418", "--muted": "#8a6a58", "--border": "#ffd9bd",
				"--accent": "#ea580c", "--accent-subtle": "rgba(234,88,12,.10)", "--accent-subtle2": "rgba(234,88,12,.20)",
				"--gradient-secondary": "#db2777", "--ok": "#16a34a", "--warn": "#d97706", "--danger": "#dc2626",
				"--sidebar-bg": "#ffffff", "--input-bg": "#ffffff", "--header-bg": "rgba(255,255,255,.94)",
			},
		},
		{
			ID: "print", Label: "Print / Grayscale", Base: "light",
			Vars: map[string]string{
				"--bg": "#ffffff", "--panel": "#ffffff", "--panel2": "#f2f2f2",
				"--text": "#111111", "--muted": "#5a5a5a", "--border": "#cccccc",
				"--accent": "#333333", "--accent-subtle": "rgba(0,0,0,.06)", "--accent-subtle2": "rgba(0,0,0,.12)",
				"--gradient-secondary": "#555555", "--ok": "#2f6f2f", "--warn": "#8a6d1f", "--danger": "#7a2323",
				"--sidebar-bg": "#ffffff", "--input-bg": "#ffffff", "--header-bg": "rgba(255,255,255,.98)", "--shadow": "none", "--shadow-sm": "none",
			},
		},
	}
}

// uiScenarioTemplate bundles a theme with a matching uiConfig — a one-click
// starting point for a specific deployment shape.
type uiScenarioTemplate struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Theme       string   `json:"theme"`
	Config      uiConfig `json:"config"`
}

// scenarioTemplates returns the built-in library of deployment scenarios.
func scenarioTemplates() []uiScenarioTemplate {
	allOn := func() map[string]bool {
		return map[string]bool{"chat": true, "search": true, "ingest": true, "stats": true}
	}
	allModesOn := func() map[string]bool {
		return map[string]bool{"auto_search": true, "deep": true, "offline": true, "agent": true, "debug": true}
	}
	boolPtr := func(b bool) *bool { return &b }

	return []uiScenarioTemplate{
		{
			ID: "support-widget", Label: "Support-Widget", Theme: "corporate",
			Description: "Schlankes Kunden-Support-Fenster: nur Chat, keine internen Admin-Panels sichtbar.",
			Config: uiConfig{
				DefaultPanel:      "chat",
				Panels:            map[string]bool{"chat": true, "search": false, "ingest": false, "stats": false},
				Modes:             map[string]bool{"auto_search": true, "deep": false, "offline": false, "agent": false, "debug": false},
				ShowPersonaPicker: boolPtr(false), ShowRolePicker: boolPtr(false), ShowLLMSwitcher: boolPtr(false),
				FooterText: "Antworten können Fehler enthalten. Bei dringenden Anliegen kontaktiere den Support direkt.",
				Suggestions: []uiSuggestion{
					{Label: "Rückgabe", Prompt: "Wie kann ich eine Bestellung zurückgeben?"},
					{Label: "Lieferstatus", Prompt: "Wo ist meine Lieferung?"},
				},
			},
		},
		{
			ID: "knowledge-kiosk", Label: "Wissens-Kiosk", Theme: "high-contrast",
			Description: "Öffentliches Terminal mit Fokus auf Suche, hoher Kontrast für Zugänglichkeit.",
			Config: uiConfig{
				DefaultPanel:      "search",
				Panels:            map[string]bool{"chat": true, "search": true, "ingest": false, "stats": false},
				Modes:             map[string]bool{"auto_search": true, "deep": false, "offline": false, "agent": false, "debug": false},
				ShowPersonaPicker: boolPtr(false), ShowRolePicker: boolPtr(false), ShowLLMSwitcher: boolPtr(false),
				FooterText: "Öffentlicher Zugang – bitte keine persönlichen Daten eingeben.",
			},
		},
		{
			ID: "internal-helpdesk", Label: "Interner Helpdesk", Theme: "corporate",
			Description: "Vollausgestatteter IT-Helpdesk: alle Panels, Rollenwechsel, Agent-Modus verfügbar.",
			Config: uiConfig{
				DefaultPanel: "chat", Panels: allOn(),
				Modes:             map[string]bool{"auto_search": true, "deep": true, "offline": false, "agent": true, "debug": true},
				ShowPersonaPicker: boolPtr(true), ShowRolePicker: boolPtr(true), ShowLLMSwitcher: boolPtr(false),
				Suggestions: []uiSuggestion{
					{Label: "VPN-Problem", Prompt: "Wie behebe ich VPN-Verbindungsprobleme?"},
					{Label: "Passwort-Reset", Prompt: "Wie setze ich mein Passwort zurück?"},
				},
			},
		},
		{
			ID: "developer-console", Label: "Entwickler-Konsole", Theme: "terminal",
			Description: "Terminal-Optik, Agent- und Debug-Modus aktiv, Modellwechsel sichtbar.",
			Config: uiConfig{
				DefaultPanel: "chat", Panels: allOn(),
				Modes:             map[string]bool{"auto_search": true, "deep": true, "offline": true, "agent": true, "debug": true},
				ShowPersonaPicker: boolPtr(false), ShowRolePicker: boolPtr(false), ShowLLMSwitcher: boolPtr(true),
			},
		},
		{
			ID: "research-assistant", Label: "Recherche-Assistent", Theme: "nord",
			Description: "Deep-Research- und Agent-Modus aktiv, für ausführliche mehrstufige Recherchen.",
			Config: uiConfig{
				DefaultPanel: "chat", Panels: allOn(), Modes: allModesOn(),
				ShowPersonaPicker: boolPtr(true), ShowRolePicker: boolPtr(false), ShowLLMSwitcher: boolPtr(false),
			},
		},
		{
			ID: "legal-archive", Label: "Rechtsarchiv", Theme: "legal",
			Description: "Chat + Suche für Aktenrecherche; Ingest bleibt Admins vorbehalten (ausgeblendet).",
			Config: uiConfig{
				DefaultPanel:      "search",
				Panels:            map[string]bool{"chat": true, "search": true, "ingest": false, "stats": false},
				Modes:             map[string]bool{"auto_search": true, "deep": true, "offline": false, "agent": false, "debug": false},
				ShowPersonaPicker: boolPtr(false), ShowRolePicker: boolPtr(true), ShowLLMSwitcher: boolPtr(false),
				FooterText: "Keine Rechtsberatung. Antworten sind Recherchehilfen und ersetzen keine juristische Prüfung.",
			},
		},
		{
			ID: "education-portal", Label: "Bildungsportal", Theme: "education",
			Description: "Chat, Suche und eigener Upload für Lernmaterialien; bewusst ohne Agent/Debug.",
			Config: uiConfig{
				DefaultPanel:      "chat",
				Panels:            map[string]bool{"chat": true, "search": true, "ingest": true, "stats": false},
				Modes:             map[string]bool{"auto_search": true, "deep": false, "offline": false, "agent": false, "debug": false},
				ShowPersonaPicker: boolPtr(true), ShowRolePicker: boolPtr(false), ShowLLMSwitcher: boolPtr(false),
				Suggestions: []uiSuggestion{
					{Label: "Zusammenfassung", Prompt: "Fasse das aktuelle Kapitel zusammen"},
					{Label: "Übungsfragen", Prompt: "Erstelle drei Übungsfragen zum Thema"},
				},
			},
		},
		{
			ID: "accessibility-kiosk", Label: "Barrierearmer Kiosk", Theme: "high-contrast",
			Description: "Maximaler Kontrast, minimale Bedienoberfläche für öffentliche Terminals.",
			Config: uiConfig{
				DefaultPanel:      "chat",
				Panels:            map[string]bool{"chat": true, "search": false, "ingest": false, "stats": false},
				Modes:             map[string]bool{"auto_search": true, "deep": false, "offline": false, "agent": false, "debug": false},
				ShowPersonaPicker: boolPtr(false), ShowRolePicker: boolPtr(false), ShowLLMSwitcher: boolPtr(false),
				FooterText: "Öffentliches Terminal – bitte keine persönlichen Daten eingeben.",
			},
		},
	}
}

// findScenarioTemplate looks up a scenario template by id.
func findScenarioTemplate(id string) (uiScenarioTemplate, bool) {
	for _, t := range scenarioTemplates() {
		if t.ID == id {
			return t, true
		}
	}
	return uiScenarioTemplate{}, false
}
