package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDemoRole(t *testing.T) {
	cases := map[string]string{
		"IT":              "it",
		" logistik ":      "logistik",
		"logistics":       "logistik",
		"Sales":           "vertrieb",
		"vertrieb":        "vertrieb",
		"human_resources": "hr",
		"HR":              "hr",
		"":                "it", // unknown/empty falls back to "it"
		"bogus":           "it",
	}
	for in, want := range cases {
		if got := normalizeDemoRole(in); got != want {
			t.Errorf("normalizeDemoRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPermissionsForRole(t *testing.T) {
	it := permissionsForRole("it")
	if !it.CanWebFetch || !it.CanBulkIngest || !it.CanRunModules || !it.CanRunCode {
		t.Errorf("it role should have all permissions, got %+v", it)
	}

	vertrieb := permissionsForRole("vertrieb")
	if !vertrieb.CanWebFetch || vertrieb.CanBulkIngest || vertrieb.CanRunModules || vertrieb.CanRunCode {
		t.Errorf("vertrieb role should only have web-fetch, got %+v", vertrieb)
	}

	hr := permissionsForRole("hr")
	if !hr.CanWebFetch || !hr.CanBulkIngest || hr.CanRunModules || hr.CanRunCode {
		t.Errorf("hr role should have web-fetch+bulk-ingest but not modules/code, got %+v", hr)
	}
}

func TestCanRoleUseTool(t *testing.T) {
	if !canRoleUseTool("it", "shell") {
		t.Error("it role should be able to use shell tool")
	}
	if canRoleUseTool("vertrieb", "shell") {
		t.Error("vertrieb role must not be able to use shell tool")
	}
	if !canRoleUseTool("vertrieb", "wikipedia") {
		t.Error("vertrieb role should be able to use wikipedia (web fetch)")
	}
	if !canRoleUseTool("vertrieb", "local_search") {
		t.Error("local_search should always be allowed")
	}
	if canRoleUseTool("it", "") {
		t.Error("empty tool id must never be allowed")
	}
	if canRoleUseTool("hr", "module:custom-thing") {
		t.Error("hr must not be allowed to run modules")
	}
}

func TestNormalizeRoleScopesAndSerialize(t *testing.T) {
	got := normalizeRoleScopes([]string{"IT", "it", "Sales"}, "hr")
	want := []string{"it", "vertrieb"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("normalizeRoleScopes dedup/sort = %v, want %v", got, want)
	}

	if got := normalizeRoleScopes([]string{"all"}, "hr"); len(got) != len(allDemoRoles()) {
		t.Errorf("normalizeRoleScopes(\"all\") should expand to all roles, got %v", got)
	}

	if got := normalizeRoleScopes(nil, "logistik"); len(got) != 1 || got[0] != "logistik" {
		t.Errorf("normalizeRoleScopes(nil) should fall back to role, got %v", got)
	}

	scope := serializeRoleScope([]string{"it", "hr"})
	if scope != "|it||hr|" {
		t.Errorf("serializeRoleScope = %q, want %q", scope, "|it||hr|")
	}
	if serializeRoleScope(nil) != "|all|" {
		t.Errorf("serializeRoleScope(nil) should be |all|")
	}
}

// TestRoleFilterSQLEscaping locks in that role-derived SQL fragments cannot be
// used to break out of the generated WHERE clause, even though the role value
// itself is normalized before reaching this function. This is a regression
// guard for the raw-SQL-fragment design flagged in the refactor plan.
func TestRoleFilterSQLEscaping(t *testing.T) {
	malicious := "it' OR '1'='1"
	sql := roleScopeFilterSQL(malicious)
	if strings.Contains(sql, "OR '1'='1'") {
		t.Errorf("roleScopeFilterSQL did not neutralize injected quote: %s", sql)
	}
	// normalizeDemoRole should have collapsed the malicious input to "it"
	// before it ever reached the SQL builder.
	if !strings.Contains(sql, roleScopeToken("it")) {
		t.Errorf("expected filter to scope to normalized role 'it', got: %s", sql)
	}

	combined := roleAndACLFilterSQL("hr")
	if !strings.Contains(combined, "role_scope") || !strings.Contains(combined, "acl_groups") {
		t.Errorf("roleAndACLFilterSQL should reference both columns, got: %s", combined)
	}
}

func TestLoadOrCreateSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	defaults := defaultSettingsFromFlags("http://localhost:1234/v1", "chat-model", "embed-model", "en", 800, 4)
	ss, err := loadOrCreateSettings(path, defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings (create) failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected settings file to be created: %v", err)
	}
	got := ss.get()
	if len(got.Personas) == 0 {
		t.Error("expected default personas to be populated on first run")
	}
	if got.ChunkSize != 800 || got.K != 4 {
		t.Errorf("expected chunk/k from defaults, got chunk=%d k=%d", got.ChunkSize, got.K)
	}

	// Reload from disk; normalization must be idempotent and role/lang must persist.
	ss2, err := loadOrCreateSettings(path, defaults)
	if err != nil {
		t.Fatalf("loadOrCreateSettings (reload) failed: %v", err)
	}
	got2 := ss2.get()
	if got2.ActiveRole != normalizeDemoRole(got.ActiveRole) {
		t.Errorf("ActiveRole not normalized consistently across reload: %q vs %q", got2.ActiveRole, got.ActiveRole)
	}
	if got2.BaseURL != "http://localhost:1234" {
		t.Errorf("BaseURL should have /v1 suffix stripped, got %q", got2.BaseURL)
	}
}

func TestSettingsStoreSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	ss := &settingsStore{path: path, s: defaultSettingsFromFlags("http://x", "c", "e", "en", 500, 3)}
	if err := ss.save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not remain after atomic rename")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings file missing after save: %v", err)
	}
}
