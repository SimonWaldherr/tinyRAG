package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// testDefaults mirrors what main.go builds via defaultSettings/CLI flags,
// with easy-to-recognize sentinel values so a test can tell "came from
// defaults" apart from "came from the on-disk file".
func testDefaults() appSettings {
	d := defaultSettings("http://localhost:1234", "test-chat", "test-embed", "de", 800, 5)
	return d
}

// TestLoadOrCreateSettingsBackfillsMissingFields simulates upgrading an
// R3 install: an old settings.json written before several fields (and
// even whole connector structs) existed. loadOrCreateSettings must fill
// every missing field from defaults — at any nesting depth — without
// requiring a hand-written backfill line per field, and without
// clobbering the values the old file actually set.
func TestLoadOrCreateSettingsBackfillsMissingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// An "old" file: only a handful of top-level fields, no jira/mssql/
	// source_visibility/source_access blocks at all, and Storage present
	// but missing max_memory_mb specifically (a field added to an
	// existing struct after this file was written).
	old := `{
		"chat_profile": "azure",
		"k": 9,
		"storage": {"backend": "sqlite", "path": "custom.db"}
	}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("write old settings.json: %v", err)
	}

	defaults := testDefaults()
	ss, err := loadOrCreateSettings(path, defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings: %v", err)
	}
	got := ss.get()

	// Explicit values from the old file must survive untouched.
	if got.ChatProfile != "azure" {
		t.Errorf("want ChatProfile preserved as %q, got %q", "azure", got.ChatProfile)
	}
	if got.K != 9 {
		t.Errorf("want K preserved as 9, got %d", got.K)
	}
	if got.Storage.Backend != "sqlite" || got.Storage.Path != "custom.db" {
		t.Errorf("want Storage.Backend/Path preserved, got %+v", got.Storage)
	}

	// Fields entirely missing from the old file must be backfilled from
	// defaults, including a field nested inside a struct that WAS
	// partially present in the file (Storage.MaxMemoryMB) and whole
	// connector structs never mentioned at all (Jira, MSSQL).
	if got.Storage.MaxMemoryMB != defaults.Storage.MaxMemoryMB {
		t.Errorf("want Storage.MaxMemoryMB backfilled to %d, got %d", defaults.Storage.MaxMemoryMB, got.Storage.MaxMemoryMB)
	}
	if got.ChunkSize != defaults.ChunkSize {
		t.Errorf("want ChunkSize backfilled to %d, got %d", defaults.ChunkSize, got.ChunkSize)
	}
	if got.EmbedProfile != defaults.EmbedProfile {
		t.Errorf("want EmbedProfile backfilled to %q, got %q", defaults.EmbedProfile, got.EmbedProfile)
	}
	if got.MSSQL.Port != defaults.MSSQL.Port || got.MSSQL.MaxRows != defaults.MSSQL.MaxRows {
		t.Errorf("want MSSQL backfilled from defaults, got %+v", got.MSSQL)
	}
	if len(got.Jira) != len(defaults.Jira) {
		t.Errorf("want Jira backfilled from defaults, got %+v", got.Jira)
	}
	if got.LDAP.URL != defaults.LDAP.URL {
		t.Errorf("want LDAP.URL backfilled to %q, got %q", defaults.LDAP.URL, got.LDAP.URL)
	}
	if got.PromptsDir != defaults.PromptsDir {
		t.Errorf("want PromptsDir backfilled to %q, got %q", defaults.PromptsDir, got.PromptsDir)
	}

	// The rewritten file on disk (not just the in-memory copy) must now
	// contain the backfilled fields too, so a second load elsewhere sees
	// the same complete settings without depending on defaults again.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back settings.json: %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse rewritten settings.json: %v", err)
	}
	if _, ok := onDisk["jira"]; !ok {
		t.Errorf("want 'jira' key present in rewritten settings.json, got keys %v", mapKeys(onDisk))
	}
	if _, ok := onDisk["mssql"]; !ok {
		t.Errorf("want 'mssql' key present in rewritten settings.json, got keys %v", mapKeys(onDisk))
	}
}

// TestLoadOrCreateSettingsSimulatedFutureField models what happens when a
// future release adds a brand-new nested config struct that the currently
// running binary's backfill logic (there isn't any anymore — see
// loadOrCreateSettings' doc comment) never explicitly mentions: an old
// file missing it entirely must still load cleanly and end up with that
// struct's defaults, proving new settings don't need a dedicated
// migration line to work.
func TestLoadOrCreateSettingsSimulatedFutureField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	defaults := testDefaults()
	// Simulate "a future release added a new connector" by mutating the
	// defaults this particular test run is given, the same way a new
	// field's Go-level default would just show up in defaultSettings()
	// without loadOrCreateSettings needing to know about it.
	defaults.Confluence = []confluenceConfig{{SpaceKey: "FUTURE-DEFAULT"}}

	ss, err := loadOrCreateSettings(path, defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings: %v", err)
	}
	got := ss.get()
	if len(got.Confluence) != 1 || got.Confluence[0].SpaceKey != "FUTURE-DEFAULT" {
		t.Fatalf("want the not-yet-seen default to be picked up automatically, got %+v", got.Confluence)
	}
}

// TestLoadOrCreateSettingsFirstRun keeps the pre-existing "no file yet"
// behavior working: defaults are used as-is and written out verbatim.
func TestLoadOrCreateSettingsFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	defaults := testDefaults()
	ss, err := loadOrCreateSettings(path, defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings: %v", err)
	}
	got := ss.get()
	if got.ChunkSize != defaults.ChunkSize || got.K != defaults.K {
		t.Fatalf("want defaults on first run, got %+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("want settings.json created on first run: %v", err)
	}
}

// TestSettingsStoreSeparatesOperationalAndFormRevisions makes sure that
// background updates (for example a connector cursor or an API key's
// last-used timestamp) do not turn an otherwise unchanged Settings form into
// a false optimistic-concurrency conflict. Only whole-form saves advance the
// revision presented by /api/settings.
func TestSettingsStoreSeparatesOperationalAndFormRevisions(t *testing.T) {
	dir := t.TempDir()
	defaults := testDefaults()
	defaults.API.Keys = []apiKeyRecord{{ID: "operational-key", LastUsedAt: 1, Enabled: true}}
	ss, err := loadOrCreateSettings(filepath.Join(dir, "settings.json"), defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings: %v", err)
	}

	_, initialRevision := ss.getWithRevision()
	if err := ss.update(func(s *appSettings) { s.API.Keys[0].LastUsedAt = 2 }); err != nil {
		t.Fatalf("operational update: %v", err)
	}
	if _, got := ss.getWithRevision(); got != initialRevision {
		t.Fatalf("operational update revision=%d, want %d", got, initialRevision)
	}

	_, after, revision, err := ss.updateIfRevision(initialRevision, true, func(s *appSettings) { s.ChunkSize = 901 })
	if err != nil {
		t.Fatalf("form update after operational update: %v", err)
	}
	if after.ChunkSize != 901 || revision != initialRevision+1 {
		t.Fatalf("form update settings=%+v revision=%d, want ChunkSize=901 revision=%d", after, revision, initialRevision+1)
	}
}

// TestConfiguredSourceKinds confirms only connectors with at least one
// ENABLED entry are reported — a configured-but-disabled connection (a very
// common admin action: pause a connector without deleting its credentials)
// must not show up, since it isn't actually contributing to the knowledge
// base right now. Drives the Help tab's dynamic "available sources" list.
func TestConfiguredSourceKinds(t *testing.T) {
	var s appSettings
	if got := configuredSourceKinds(s); len(got) != 0 {
		t.Fatalf("want no sources with everything unconfigured, got %v", got)
	}

	s.Jira = []jiraConfig{{connRuntime: connRuntime{Name: "ops"}, Enabled: false}}
	s.SharePoint = []sharePointConfig{{connRuntime: connRuntime{Name: "vertrieb"}, Enabled: true}}
	s.IMAP = []mailboxConfig{{Enabled: false}}
	got := configuredSourceKinds(s)
	if len(got) != 1 || got[0] != "sharepoint" {
		t.Fatalf("want only sharepoint (Jira/IMAP entries disabled), got %v", got)
	}

	s.Jira[0].Enabled = true
	got = configuredSourceKinds(s)
	if len(got) != 2 {
		t.Fatalf("want sharepoint+jira once jira is enabled, got %v", got)
	}
}

// TestConfiguredToolKinds confirms each tool's real enabling condition is
// checked (not just presence of the config struct), including the two
// external-search tools' extra requirements (azure_bing_search additionally
// needs a usable Azure profile) and the "at least one enabled template/
// connector" checks for HTTP templates and generic REST connectors.
func TestConfiguredToolKinds(t *testing.T) {
	var s appSettings
	s.Agent.SubagentsDisabled = true // isolate the other checks — subagents defaults ON
	if got := configuredToolKinds(s); len(got) != 0 {
		t.Fatalf("want no tools with everything else unconfigured, got %v", got)
	}

	s.Agent.AllowAzureBingSearch = true
	if got := configuredToolKinds(s); len(got) != 0 {
		t.Fatalf("want azure_bing_search excluded without a configured Azure profile, got %v", got)
	}
	s.Profiles.Azure.BaseURL = "https://x.openai.azure.com"
	s.Profiles.Azure.ChatModel = "gpt-deployment"
	got := configuredToolKinds(s)
	if len(got) != 1 || got[0] != "azure_bing_search" {
		t.Fatalf("want azure_bing_search once the Azure profile is usable, got %v", got)
	}

	s.MSSQL.Enabled = true
	s.HTTPTemplates = []httpQueryTemplate{{Name: "t1", Enabled: false}, {Name: "t2", Enabled: true}}
	got = configuredToolKinds(s)
	want := map[string]bool{"azure_bing_search": true, "mssql": true, "http": true}
	if len(got) != len(want) {
		t.Fatalf("want exactly %v, got %v", want, got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected tool kind %q in %v", k, got)
		}
	}
}

// TestHandleAuthStatusExposesAvailableSourcesAndTools confirms the
// /api/auth/status wiring actually surfaces both new fields end to end.
func TestHandleAuthStatusExposesAvailableSourcesAndTools(t *testing.T) {
	s := appSettings{Jira: []jiraConfig{{connRuntime: connRuntime{Name: "ops"}, Enabled: true}}}
	s.Agent.AllowWebFetch = true
	s.Agent.SubagentsDisabled = true // isolate: subagents defaults ON otherwise
	withTestGlobalSettings(t, s)

	r := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	w := httptest.NewRecorder()
	handleAuthStatus(w, r)

	var out struct {
		AvailableSources []string `json:"available_sources"`
		AvailableTools   []string `json:"available_tools"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.AvailableSources) != 1 || out.AvailableSources[0] != "jira" {
		t.Fatalf("want available_sources=[jira], got %v", out.AvailableSources)
	}
	if len(out.AvailableTools) != 1 || out.AvailableTools[0] != "fetch_url" {
		t.Fatalf("want available_tools=[fetch_url], got %v", out.AvailableTools)
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
