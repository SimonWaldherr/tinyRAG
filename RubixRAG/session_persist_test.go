package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestSessionSurvivesRestart proves a session issued before a simulated
// process restart (secret + store reloaded from disk, same as main() does
// via initSessionPersistence) is still valid afterward — the scenario that
// motivated persisting both files in the first place (session.go's package
// doc comment).
func TestSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	sessionSecretPath = filepath.Join(dir, "r3-session-secret.bin")
	sessionStorePath = filepath.Join(dir, "r3-sessions.gob")
	sessionSecret = nil
	sessionStoreMu.Lock()
	sessionStore = map[string]sessionClaims{}
	sessionStoreMu.Unlock()

	initSessionPersistence()

	w := httptest.NewRecorder()
	issueSession(w, &ldapUser{CN: "bob", IsAdmin: true, MemberOf: []string{"CN=A,DC=rubix,DC=com"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}

	// Simulate a restart: wipe in-memory state, reload from disk exactly
	// like main() does on startup.
	sessionSecret = nil
	sessionStoreMu.Lock()
	sessionStore = map[string]sessionClaims{}
	sessionStoreMu.Unlock()
	initSessionPersistence()

	claims, ok := currentSession(req)
	if !ok {
		t.Fatal("want session to still be valid after simulated restart")
	}
	if claims.User != "bob" || !claims.IsAdmin || len(claims.Groups) != 1 {
		t.Errorf("want restored claims for bob/admin/1 group, got %+v", claims)
	}
}
