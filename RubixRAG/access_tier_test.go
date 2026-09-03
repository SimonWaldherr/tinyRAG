package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Covers docs/UI_HARDENING_PLAN.md's "Registriert" tier: three server-side
// enforcement points (resolveAskProfile, mssqlToolAllowed,
// requireSessionIfLDAP) that only kick in once LDAP login exists at all —
// with LDAP off, every case must behave exactly as before this tier
// existed (guest = the only tier).

func TestResolveAskProfile(t *testing.T) {
	cases := []struct {
		name        string
		profile     string
		ldap        ldapConfig
		authActive  bool
		hasSession  bool
		wantProfile string
		wantDeny    bool
	}{
		{"no auth tier, no session, azure requested: unchanged", "azure", ldapConfig{Enabled: false}, false, false, "azure", false},
		{"ldap on, has session, azure requested: unchanged", "azure", ldapConfig{Enabled: true}, true, true, "azure", false},
		{"ldap on, no session, non-azure profile: unchanged", "local", ldapConfig{Enabled: true}, true, false, "local", false},
		{"ldap on, no session, azure, default policy: falls back", "azure", ldapConfig{Enabled: true}, true, false, "local", false},
		{"ldap on, no session, azure, fallback policy: falls back", "azure", ldapConfig{Enabled: true, GuestAzureProfilePolicy: "fallback"}, true, false, "local", false},
		{"ldap on, no session, azure, deny policy: rejected", "azure", ldapConfig{Enabled: true, GuestAzureProfilePolicy: "deny"}, true, false, "azure", true},
		{"local auth only (ldap off), no session, azure, deny policy: rejected", "azure", ldapConfig{GuestAzureProfilePolicy: "deny"}, true, false, "azure", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profile, deny := resolveAskProfile(c.profile, c.ldap, c.authActive, c.hasSession, "local")
			if profile != c.wantProfile || deny != c.wantDeny {
				t.Errorf("resolveAskProfile(%q, %+v, %v, %v) = (%q, %v), want (%q, %v)",
					c.profile, c.ldap, c.authActive, c.hasSession, profile, deny, c.wantProfile, c.wantDeny)
			}
		})
	}
}

func TestMSSQLToolAllowed(t *testing.T) {
	cases := []struct {
		name       string
		authActive bool
		hasSession bool
		want       bool
	}{
		{"no auth tier, no session: allowed", false, false, true},
		{"auth tier active, no session: denied", true, false, false},
		{"auth tier active, has session: allowed", true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mssqlToolAllowed(c.authActive, c.hasSession); got != c.want {
				t.Errorf("mssqlToolAllowed(%v, %v) = %v, want %v", c.authActive, c.hasSession, got, c.want)
			}
		})
	}
}

func TestRequireSessionIfLDAP(t *testing.T) {
	t.Run("ldap off: always allowed, even with no session", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if !requireSessionIfLDAP(w, r, appSettings{}) {
			t.Fatal("want allowed with LDAP off")
		}
		if w.Code != 200 {
			t.Fatalf("want no response written, got status %d", w.Code)
		}
	})

	t.Run("ldap on, no session cookie: rejected with 401", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		s := appSettings{LDAP: ldapConfig{Enabled: true}}
		if requireSessionIfLDAP(w, r, s) {
			t.Fatal("want rejected with LDAP on and no session")
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", w.Code)
		}
	})

	t.Run("ldap on, valid session cookie: allowed", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "test.user", IsAdmin: false})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		s := appSettings{LDAP: ldapConfig{Enabled: true}}
		w2 := httptest.NewRecorder()
		if !requireSessionIfLDAP(w2, r, s) {
			t.Fatal("want allowed with a valid session")
		}
	})
}
