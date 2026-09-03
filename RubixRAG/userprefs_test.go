package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newTestUserPrefsStore(t *testing.T) *userPrefsStore {
	t.Helper()
	s, err := newUserPrefsStore(filepath.Join(t.TempDir(), "prefs.db"))
	if err != nil {
		t.Fatalf("newUserPrefsStore: %v", err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

// withTestUserPrefsDB points the package-level userPrefsDB (which
// handleUserPrefsGet/handleUserPrefsSetPersonalContext/personalContextBlock
// all read via the global, same as every other test in this package that
// swaps a global store — see openai_api_test.go's withTestGlobalSettings)
// at a fresh temp store, restoring the previous value on cleanup so this
// test can't leak state into any test that runs after it.
func withTestUserPrefsDB(t *testing.T) *userPrefsStore {
	t.Helper()
	prev := userPrefsDB
	s := newTestUserPrefsStore(t)
	userPrefsDB = s
	t.Cleanup(func() { userPrefsDB = prev })
	return s
}

func TestUserPrefsGetNoRowReturnsZeroValue(t *testing.T) {
	s := newTestUserPrefsStore(t)
	p, err := s.get("nobody")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p != (userPrefs{}) {
		t.Fatalf("want zero-value userPrefs for an unknown owner, got %+v", p)
	}
}

func TestUserPrefsSetLangDoesNotTouchPersonalContext(t *testing.T) {
	s := newTestUserPrefsStore(t)
	// Save personal context first, then set lang — lang must not disturb it,
	// and personal context must not disturb a later lang-only save either.
	if err := s.setPersonalContext("alice", userPrefs{DisplayName: "Alice A.", UsePersonalContext: true}); err != nil {
		t.Fatalf("setPersonalContext: %v", err)
	}
	if err := s.setLang("alice", "en"); err != nil {
		t.Fatalf("setLang: %v", err)
	}
	p, err := s.get("alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.Lang != "en" {
		t.Fatalf("want lang=en, got %q", p.Lang)
	}
	if p.DisplayName != "Alice A." || !p.UsePersonalContext {
		t.Fatalf("want personal context preserved after a lang-only save, got %+v", p)
	}

	// Now the reverse: a personal-context save must not clobber lang.
	if err := s.setPersonalContext("alice", userPrefs{DisplayName: "Alice B.", UsePersonalContext: true}); err != nil {
		t.Fatalf("setPersonalContext (2nd): %v", err)
	}
	p, err = s.get("alice")
	if err != nil {
		t.Fatalf("get (2nd): %v", err)
	}
	if p.Lang != "en" {
		t.Fatalf("want lang=en preserved after a personal-context save, got %q", p.Lang)
	}
	if p.DisplayName != "Alice B." {
		t.Fatalf("want the updated display name, got %q", p.DisplayName)
	}
}

func TestUserPrefsSetPersonalContextRoundTripsAllFields(t *testing.T) {
	s := newTestUserPrefsStore(t)
	want := userPrefs{
		DisplayName:        "Simon W.",
		Position:           "Vertriebsleiter",
		Department:         "Vertrieb Süd",
		Signature:          "Mit freundlichen Grüßen\nSimon W.",
		ContactInfo:        "+49 170 0000000",
		CommunicationStyle: "direkt und knapp",
		TypicalPhrasing:    "Gerne kümmere ich mich darum.",
		AINotes:            "Bitte nie \"Bestellung\" statt \"Auftrag\" schreiben.",
		UsePersonalContext: true,
	}
	if err := s.setPersonalContext("simon", want); err != nil {
		t.Fatalf("setPersonalContext: %v", err)
	}
	got, err := s.get("simon")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want.Lang = "" // setPersonalContext never touches lang; get() returns "" for a fresh row
	if got != want {
		t.Fatalf("round-trip mismatch:\n want %+v\n got  %+v", want, got)
	}
}

func TestUserPrefsSetPersonalContextIsIdempotentPerOwner(t *testing.T) {
	s := newTestUserPrefsStore(t)
	if err := s.setPersonalContext("bob", userPrefs{DisplayName: "Bob"}); err != nil {
		t.Fatalf("1st save: %v", err)
	}
	if err := s.setPersonalContext("bob", userPrefs{DisplayName: "Robert"}); err != nil {
		t.Fatalf("2nd save: %v", err)
	}
	p, err := s.get("bob")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.DisplayName != "Robert" {
		t.Fatalf("want the 2nd save to overwrite, got %q", p.DisplayName)
	}
}

func TestUserPrefsSeparateOwnersDoNotLeak(t *testing.T) {
	s := newTestUserPrefsStore(t)
	if err := s.setPersonalContext("alice", userPrefs{DisplayName: "Alice", UsePersonalContext: true}); err != nil {
		t.Fatalf("alice save: %v", err)
	}
	bob, err := s.get("bob")
	if err != nil {
		t.Fatalf("bob get: %v", err)
	}
	if bob.DisplayName != "" || bob.UsePersonalContext {
		t.Fatalf("want bob to have no data leaked from alice's row, got %+v", bob)
	}
}

// ---- HTTP handlers ---------------------------------------------------------

func TestHandleUserPrefsSetPersonalContextValidation(t *testing.T) {
	withTestUserPrefsDB(t)

	t.Run("over-limit field is rejected", func(t *testing.T) {
		body, _ := json.Marshal(setPersonalContextRequest{DisplayName: strings.Repeat("x", personalContextFieldLimits["display_name"]+1)})
		r := httptest.NewRequest(http.MethodPost, "/api/account/prefs/personal", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleUserPrefsSetPersonalContext(w, r, "carol")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400 for an over-limit field, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("valid fields save and round-trip via GET", func(t *testing.T) {
		body, _ := json.Marshal(setPersonalContextRequest{
			DisplayName:        "Carol C.",
			CommunicationStyle: "freundlich",
			UsePersonalContext: true,
		})
		r := httptest.NewRequest(http.MethodPost, "/api/account/prefs/personal", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleUserPrefsSetPersonalContext(w, r, "carol")
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}

		r2 := httptest.NewRequest(http.MethodGet, "/api/account/prefs", nil)
		w2 := httptest.NewRecorder()
		handleUserPrefsGet(w2, r2, "carol")
		var got userPrefs
		if err := json.Unmarshal(w2.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.DisplayName != "Carol C." || got.CommunicationStyle != "freundlich" || !got.UsePersonalContext {
			t.Fatalf("unexpected round-trip: %+v", got)
		}
	})

	t.Run("whitespace is trimmed before saving", func(t *testing.T) {
		body, _ := json.Marshal(setPersonalContextRequest{DisplayName: "  Dave  "})
		r := httptest.NewRequest(http.MethodPost, "/api/account/prefs/personal", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleUserPrefsSetPersonalContext(w, r, "dave")
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", w.Code)
		}
		p, err := userPrefsDB.get("dave")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if p.DisplayName != "Dave" {
			t.Fatalf("want trimmed value %q, got %q", "Dave", p.DisplayName)
		}
	})
}

// ---- personalContextBlock / userContextBlock -------------------------------

func TestPersonalContextBlockRequiresOptIn(t *testing.T) {
	withTestUserPrefsDB(t)
	if err := userPrefsDB.setPersonalContext("erin", userPrefs{DisplayName: "Erin", UsePersonalContext: false}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := personalContextBlock("erin"); got != "" {
		t.Fatalf("want \"\" when UsePersonalContext is false, got %q", got)
	}
}

func TestPersonalContextBlockRendersOptedInFields(t *testing.T) {
	withTestUserPrefsDB(t)
	if err := userPrefsDB.setPersonalContext("frank", userPrefs{
		DisplayName:        "Frank F.",
		CommunicationStyle: "locker",
		Signature:          "Beste Grüße\nFrank",
		UsePersonalContext: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := personalContextBlock("frank")
	if !strings.Contains(got, "Frank F.") || !strings.Contains(got, "locker") {
		t.Fatalf("want opted-in fields present, got %q", got)
	}
	if !strings.Contains(got, "Beste Grüße / Frank") {
		t.Fatalf("want the signature's newlines flattened to \" / \", got %q", got)
	}
}

func TestPersonalContextBlockEmptyWhenOptedInButNothingFilled(t *testing.T) {
	withTestUserPrefsDB(t)
	if err := userPrefsDB.setPersonalContext("gina", userPrefs{UsePersonalContext: true}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := personalContextBlock("gina"); got != "" {
		t.Fatalf("want \"\" when opted in but no field is actually filled, got %q", got)
	}
}

func TestPersonalContextBlockNilStoreIsSafe(t *testing.T) {
	prevDB := userPrefsDB
	userPrefsDB = nil
	defer func() { userPrefsDB = prevDB }()
	if got := personalContextBlock("anyone"); got != "" {
		t.Fatalf("want \"\" with no store configured, got %q", got)
	}
}

func TestUserContextBlockIncludesPersonalContextWhenOptedIn(t *testing.T) {
	withTestUserPrefsDB(t)
	if err := userPrefsDB.setPersonalContext("Any Admin", userPrefs{
		CommunicationStyle: "sehr formell",
		UsePersonalContext: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	w := httptest.NewRecorder()
	issueSession(w, &ldapUser{CN: "Any Admin", Department: "IT"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	got := userContextBlock(r)
	if !strings.Contains(got, "Any Admin") || !strings.Contains(got, "IT") {
		t.Fatalf("want the AD-derived facts present, got %q", got)
	}
	if !strings.Contains(got, "sehr formell") {
		t.Fatalf("want the opted-in personal context appended, got %q", got)
	}
}

func TestUserContextBlockWithoutPersonalContextOptIn(t *testing.T) {
	withTestUserPrefsDB(t)
	// No row at all for this user — the common "never touched Mein Konto" case.
	w := httptest.NewRecorder()
	issueSession(w, &ldapUser{CN: "Plain User"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	got := userContextBlock(r)
	if !strings.Contains(got, "Plain User") {
		t.Fatalf("want the AD-derived name present, got %q", got)
	}
	if strings.Contains(got, "Persönlicher Kontext") {
		t.Fatalf("want no personal-context section without opt-in, got %q", got)
	}
}
