package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsSecretSettingsPath(t *testing.T) {
	cases := map[string]bool{
		"shop.password":            true,
		"shop.password_env":        false, // env-var NAME, not a value
		"sharepoint.client_secret": true,
		"confluence.api_token":     true,
		"freshservice.api_key":     true,
		"freshservice.api_key_env": false,
		"confluence.space_key":     false, // "key" alone must not match
		"jira.project_key":         false,
		"k":                        false,
		"profiles.azure.api_key":   true,
		"ldap.admin_users[0]":      false,
	}
	for path, want := range cases {
		if got := isSecretSettingsPath(path); got != want {
			t.Errorf("isSecretSettingsPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestDiffSettingsMasksSecrets is the core guarantee of the whole
// feature: a changed password shows up as "changed" but NEVER with its
// old or new value; a non-secret change carries both values.
func TestDiffSettingsMasksSecrets(t *testing.T) {
	before := defaultSettings("http://localhost:1234", "chat-m", "embed-m", "de", 800, 5)
	after := defaultSettings("http://localhost:1234", "chat-m", "embed-m", "de", 800, 5)
	after.Shop.Password = "super-geheim-123"
	after.K = before.K + 3

	changes := diffSettings(before, after)
	var sawSecret, sawK bool
	for _, c := range changes {
		if c.Path == "shop.password" {
			sawSecret = true
			if !c.Secret {
				t.Error("shop.password change must be flagged Secret")
			}
			if c.Old != "" || c.New != "" {
				t.Errorf("shop.password change must carry no values, got old=%q new=%q", c.Old, c.New)
			}
		}
		if c.Path == "k" {
			sawK = true
			if c.Secret || c.New == "" {
				t.Errorf("k change should carry values, got %+v", c)
			}
		}
	}
	if !sawSecret || !sawK {
		t.Fatalf("want both shop.password and k in the diff, got %+v", changes)
	}
	// And the raw value must not appear anywhere in the serialized entry.
	raw, _ := json.Marshal(changes)
	if containsStr(string(raw), "super-geheim-123") {
		t.Fatal("the secret value leaked into the serialized diff")
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestDiffSettingsNoChanges(t *testing.T) {
	s := defaultSettings("http://localhost:1234", "chat-m", "embed-m", "de", 800, 5)
	if changes := diffSettings(s, s); len(changes) != 0 {
		t.Fatalf("identical settings must produce an empty diff, got %+v", changes)
	}
}

func TestSettingsHistoryEndpointRoundtrip(t *testing.T) {
	dir := t.TempDir()
	orig := settingsHistoryPath
	settingsHistoryPath = filepath.Join(dir, "history.jsonl")
	defer func() { settingsHistoryPath = orig }()

	// Empty file/absent file → empty list, not an error.
	r := httptest.NewRequest(http.MethodGet, "/api/settings/history", nil)
	w := httptest.NewRecorder()
	handleSettingsHistory(w, r)
	if w.Code != http.StatusOK || w.Body.String() == "" {
		t.Fatalf("want 200 with an empty list, got %d: %s", w.Code, w.Body.String())
	}

	appendSettingsHistory(settingsHistoryEntry{Time: time.Now().Unix(), Actor: "admin@rubix.com", Changes: []settingsChange{{Path: "k", Old: "5", New: "8"}}})
	appendSettingsHistory(settingsHistoryEntry{Time: time.Now().Unix() + 1, Actor: "admin@rubix.com", Changes: []settingsChange{{Path: "shop.password", Secret: true}}})

	w = httptest.NewRecorder()
	handleSettingsHistory(w, httptest.NewRequest(http.MethodGet, "/api/settings/history", nil))
	var entries []settingsHistoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	// newest first
	if entries[0].Changes[0].Path != "shop.password" || !entries[0].Changes[0].Secret {
		t.Fatalf("want the newest (secret) entry first, got %+v", entries[0])
	}

	// A corrupt line must not break the endpoint.
	f, _ := os.OpenFile(settingsHistoryPath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("{kaputt\n")
	f.Close()
	w = httptest.NewRecorder()
	handleSettingsHistory(w, httptest.NewRequest(http.MethodGet, "/api/settings/history", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || len(entries) != 2 {
		t.Fatalf("corrupt line must be skipped, got err=%v n=%d", err, len(entries))
	}
}
