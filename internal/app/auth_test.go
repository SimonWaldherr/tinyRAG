package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionStoreLifecycle(t *testing.T) {
	ss := newSessionStore()
	sess := ss.create("u1", "alice", "admin", time.Hour)
	if sess.Token == "" {
		t.Fatal("expected non-empty session token")
	}
	got, ok := ss.get(sess.Token)
	if !ok || got.Username != "alice" || got.Role != "admin" {
		t.Fatalf("expected to retrieve created session, got %+v ok=%v", got, ok)
	}
	ss.delete(sess.Token)
	if _, ok := ss.get(sess.Token); ok {
		t.Fatal("session should be gone after delete")
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	ss := newSessionStore()
	sess := ss.create("u1", "bob", "viewer", -time.Second) // already expired
	if _, ok := ss.get(sess.Token); ok {
		t.Fatal("expired session must not be returned")
	}
}

func TestHashAndVerifyWebUIPassword(t *testing.T) {
	hash, err := hashWebUIPassword("s3cret!")
	if err != nil {
		t.Fatalf("hashWebUIPassword failed: %v", err)
	}
	u := webUIUser{PasswordHash: hash}
	if !verifyWebUIPassword(u, "s3cret!") {
		t.Error("correct password should verify")
	}
	if verifyWebUIPassword(u, "wrong") {
		t.Error("incorrect password must not verify")
	}
	if verifyWebUIPassword(webUIUser{}, "anything") {
		t.Error("user with no password hash must never verify")
	}
}

func TestGenerateAndHashAPIToken(t *testing.T) {
	tok1, err := generateAPIToken()
	if err != nil {
		t.Fatalf("generateAPIToken failed: %v", err)
	}
	tok2, _ := generateAPIToken()
	if tok1 == tok2 {
		t.Error("two generated tokens should not collide")
	}
	h1 := hashAPIToken(tok1)
	h2 := hashAPIToken(tok1)
	if h1 != h2 {
		t.Error("hashAPIToken must be deterministic")
	}
	if h1 == tok1 {
		t.Error("hash must differ from the raw token")
	}
}

func TestAuthenticateAPIUser(t *testing.T) {
	token, _ := generateAPIToken()
	users := []adminAPIUser{
		{ID: "1", Enabled: true, APIKeyHash: hashAPIToken(token)},
		{ID: "2", Enabled: false, APIKeyHash: hashAPIToken("other")},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/ask", nil)
	req.Header.Set("X-API-Key", token)
	u, ok := authenticateAPIUser(req, users)
	if !ok || u.ID != "1" {
		t.Fatalf("expected to authenticate as user 1, got %+v ok=%v", u, ok)
	}

	reqBad := httptest.NewRequest(http.MethodGet, "/api/ask", nil)
	reqBad.Header.Set("X-API-Key", "totally-wrong")
	if _, ok := authenticateAPIUser(reqBad, users); ok {
		t.Error("wrong API key must not authenticate")
	}

	reqBearer := httptest.NewRequest(http.MethodGet, "/api/ask", nil)
	reqBearer.Header.Set("Authorization", "Bearer "+token)
	if _, ok := authenticateAPIUser(reqBearer, users); !ok {
		t.Error("Authorization: Bearer header should also authenticate")
	}
}

func TestFindAPIRouteRule(t *testing.T) {
	rules := []apiRouteRule{
		{Path: "/api/admin", MatchType: "prefix", Enabled: true},
		{Path: "/api/admin/users", MatchType: "prefix", Enabled: false},
		{Path: "/api/ask", MatchType: "exact", Enabled: true, Public: true},
	}
	if rule, ok := findAPIRouteRule("/api/ask", rules); !ok || !rule.Public {
		t.Fatalf("expected exact match for /api/ask, got %+v ok=%v", rule, ok)
	}
	// Longest matching prefix should win.
	rule, ok := findAPIRouteRule("/api/admin/users/42", rules)
	if !ok || rule.Path != "/api/admin/users" {
		t.Fatalf("expected longest-prefix match /api/admin/users, got %+v ok=%v", rule, ok)
	}
	if _, ok := findAPIRouteRule("/unmatched", rules); ok {
		t.Error("unmatched path should return ok=false")
	}
}

func TestNormalizeAPIRouteRulesMergesDefaults(t *testing.T) {
	custom := []apiRouteRule{
		{Path: "/api/ask", Enabled: false, Public: false}, // override a default
	}
	out := normalizeAPIRouteRules(custom)
	found := false
	for _, r := range out {
		if r.Path == "/api/ask" {
			found = true
			if r.Enabled || r.Public {
				t.Errorf("custom override should be preserved, got %+v", r)
			}
		}
		if r.Path == "/api/process" && !r.Enabled {
			t.Error("unmentioned defaults should remain enabled")
		}
	}
	if !found {
		t.Fatal("/api/ask rule missing from normalized output")
	}
}

func TestIsLoopbackRequest(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:5000", true},
		{"[::1]:5000", true},
		{"localhost:5000", true},
		{"203.0.113.5:5000", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.addr
		if got := isLoopbackRequest(r); got != c.want {
			t.Errorf("isLoopbackRequest(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestIsHTTPS(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if isHTTPS(r) {
		t.Error("plain request should not be considered HTTPS")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !isHTTPS(r) {
		t.Error("X-Forwarded-Proto: https should be honored")
	}
}
