package app

import (
	"path/filepath"
	"testing"
)

// withTestSettings points the package-level `settings` global (read directly
// by runListThemes/runListTemplates/runApplyTemplate, mirroring the HTTP
// handlers in main.go) at a fresh temp-file-backed store, and restores the
// previous global on test cleanup so other tests are unaffected.
func withTestSettings(t *testing.T) *settingsStore {
	t.Helper()
	prev := settings
	t.Cleanup(func() { settings = prev })

	path := filepath.Join(t.TempDir(), "settings.json")
	ss, err := loadOrCreateSettings(path, defaultSettingsFromFlags("http://x", "c", "e", "de", 500, 3))
	if err != nil {
		t.Fatalf("loadOrCreateSettings failed: %v", err)
	}
	settings = ss
	return ss
}

func testPersonaStore(t *testing.T, ss *settingsStore) *personaStore {
	t.Helper()
	return newPersonaStore(ss)
}

func TestCLISessionSystemPromptWithoutPersona(t *testing.T) {
	cs := &cliSession{}
	if got := cs.systemPrompt(nil); got != cliAskSystemPrompt {
		t.Errorf("expected base prompt without persona, got %q", got)
	}
}

func TestCLISessionSystemPromptWithPersona(t *testing.T) {
	ss := withTestSettings(t)
	personas := testPersonaStore(t, ss)
	all := personas.list()
	if len(all) == 0 {
		t.Fatal("expected default personas to be seeded")
	}
	cs := &cliSession{personaID: all[0].ID}
	got := cs.systemPrompt(personas)
	if got == cliAskSystemPrompt {
		t.Error("expected persona prompt to be prefixed")
	}
	if !contains(got, all[0].Prompt) {
		t.Errorf("expected persona prompt %q to appear in %q", all[0].Prompt, got)
	}
}

func TestCLISessionSystemPromptUnknownPersonaFallsBack(t *testing.T) {
	ss := withTestSettings(t)
	personas := testPersonaStore(t, ss)
	cs := &cliSession{personaID: "does-not-exist"}
	if got := cs.systemPrompt(personas); got != cliAskSystemPrompt {
		t.Errorf("unknown persona should fall back to base prompt, got %q", got)
	}
}

func TestCLISessionRecentHistoryCapsAtLimit(t *testing.T) {
	cs := &cliSession{}
	for i := 0; i < cliHistoryLimit+5; i++ {
		cs.history = append(cs.history, chatMessage{Role: "user", Content: "msg"})
	}
	hist := cs.recentHistory()
	if len(hist) != cliHistoryLimit {
		t.Fatalf("expected history capped at %d, got %d", cliHistoryLimit, len(hist))
	}
}

func TestCLISessionRecentHistoryBelowLimit(t *testing.T) {
	cs := &cliSession{}
	cs.history = append(cs.history, chatMessage{Role: "user", Content: "a"}, chatMessage{Role: "assistant", Content: "b"})
	hist := cs.recentHistory()
	if len(hist) != 2 || hist[0].Content != "a" || hist[1].Content != "b" {
		t.Fatalf("unexpected history: %+v", hist)
	}
}

func TestCLISessionAsConversation(t *testing.T) {
	cs := &cliSession{history: []chatMessage{{Role: "user", Content: "hallo"}}}
	conv := cs.asConversation()
	if conv.ID == "" || len(conv.Messages) != 1 {
		t.Fatalf("unexpected conversation: %+v", conv)
	}
}

func TestRunListThemes(t *testing.T) {
	withTestSettings(t)
	if code := runListThemes(); code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
}

func TestRunListTemplates(t *testing.T) {
	withTestSettings(t)
	if code := runListTemplates(); code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
}

func TestRunApplyTemplateValid(t *testing.T) {
	ss := withTestSettings(t)
	if code := runApplyTemplate("support-widget"); code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	got := ss.get()
	if got.Theme != "corporate" {
		t.Errorf("expected theme 'corporate' after applying support-widget, got %q", got.Theme)
	}
	if got.UI.Panels["ingest"] {
		t.Error("expected support-widget template to disable the ingest panel")
	}
}

func TestRunApplyTemplateWithDensity(t *testing.T) {
	ss := withTestSettings(t)
	if code := runApplyTemplate("finance-dashboard"); code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if got := ss.get().Density; got != "compact" {
		t.Errorf("expected density 'compact' after applying finance-dashboard, got %q", got)
	}
}

func TestRunApplyTemplateUnknownID(t *testing.T) {
	withTestSettings(t)
	if code := runApplyTemplate("does-not-exist"); code == 0 {
		t.Error("expected non-zero exit code for an unknown template id")
	}
}
