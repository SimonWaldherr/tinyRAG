package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Per-user preference overrides — keyed by the session's AD username
// (session.go's currentSession claims.User), the same pattern
// chathistory.go already established for server-side, per-user,
// cross-device data. settings.go's Lang remains the admin-configured
// *default*; a row here (when non-empty) overrides it for that one user,
// resolved in handlers.go's handleAuthStatus. Anonymous visitors and
// deployments with LDAP off have no user identity to key this by at all —
// they fall back to the browser-local mechanism web/app.js's Theme/Font-
// size switchers already use (localStorage), never touching this store.
//
// Phase 4 addition: personal context fields (DisplayName..AINotes) — see
// their own doc comments below and handlers.go's personalContextBlock for
// how they flow into the system prompt, ONLY once UsePersonalContext is
// explicitly on. These are the user's OWN words about themselves, kept
// entirely separate from AD-derived facts (sessionClaims), which R3 already
// folds into prompts unconditionally once settings.PersonalizeAnswers is on.
// ─────────────────────────────────────────────────────────────────────────────

// userPrefsDB is the process-wide store, opened once in main() from
// settings.UserPrefsPath — nil only in tests that don't call
// newUserPrefsStore.
var userPrefsDB *userPrefsStore

type userPrefsStore struct {
	db *sql.DB
}

// userPrefs is one user's stored overrides/preferences. An empty field
// means "no override, use the admin default" (Lang) or "not filled in yet"
// (every personal-context field) rather than "unset row missing" — get()
// returns a zero-value userPrefs for an owner with no row at all, same
// meaning either way.
type userPrefs struct {
	Lang string `json:"lang"`

	// DisplayName is how this person wants to be addressed/signed —
	// independent of AD's cn, which may be a login-style name rather than
	// how they'd actually sign an email.
	DisplayName string `json:"display_name,omitempty"`
	// Position is a free-text role/job title, e.g. "Vertriebsleiter Süd" —
	// independent of (and may be more specific/current than) AD's title.
	Position string `json:"position,omitempty"`
	// Department is a free-text department/team label — independent of AD's
	// department attribute (which department.go also classifies into
	// DeptCode for access control; this field never feeds that, purely
	// prompt-facing).
	Department string `json:"department,omitempty"`
	// Signature is a free-text, possibly multi-line email signature block —
	// the Mail tab's "Signatur einfügen" prefers this over the generic
	// AD-derived placeholder once set (see web/app.js's buildMailSignature).
	Signature string `json:"signature,omitempty"`
	// ContactInfo is free-text contact details (phone/mobile/…) — commonly
	// part of a signature but kept as its own field so it can inform a
	// drafted answer even when the signature itself isn't inserted.
	ContactInfo string `json:"contact_info,omitempty"`
	// CommunicationStyle is a free-text note on how this person prefers to
	// communicate (e.g. "direkt und knapp", "eher formell") — folded into
	// the system context as a tone hint, never as a fact about the current
	// question/mail.
	CommunicationStyle string `json:"communication_style,omitempty"`
	// TypicalPhrasing is free-text example phrasing/wording this person
	// tends to use — a style reference for the model, not a template to
	// quote verbatim.
	TypicalPhrasing string `json:"typical_phrasing,omitempty"`
	// AINotes is a free-text catch-all for anything else this person wants
	// the model to keep in mind about them specifically.
	AINotes string `json:"ai_notes,omitempty"`
	// UsePersonalContext is the per-user opt-in gate: even with
	// appSettings.PersonalizeAnswers on deployment-wide, none of the fields
	// above are folded into a prompt until the user themselves switches
	// this on (handlers.go's personalContextBlock) — a second, personal
	// consent layered on top of the admin's own toggle, since this is the
	// user's own words about themselves, not something R3 infers.
	UsePersonalContext bool `json:"use_personal_context"`
}

// personalContextFieldLimits bounds each personal-context field's length —
// these flow directly into every future prompt this user's answers/drafts
// generate, so an unbounded paste would silently inflate token cost (and,
// for the free-text fields, is also simply not what a "note about
// yourself" needs to be). Enforced by handleUserPrefsSetPersonalContext,
// which rejects an over-limit request outright rather than silently
// truncating it.
var personalContextFieldLimits = map[string]int{
	"display_name":        200,
	"position":            200,
	"department":          200,
	"contact_info":        200,
	"communication_style": 300,
	"signature":           1000,
	"typical_phrasing":    1000,
	"ai_notes":            1000,
}

// newUserPrefsStore opens (creating if needed) the SQLite file at path and
// ensures its schema exists.
func newUserPrefsStore(path string) (*userPrefsStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("user prefs: open %s: %w", path, err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS user_prefs (
			owner TEXT PRIMARY KEY,
			lang TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("user prefs: schema: %w", err)
	}
	// Personal-context columns (Phase 4) — CREATE TABLE IF NOT EXISTS is a
	// no-op once the table already exists, columns and all, so a
	// user_prefs.db from before these existed needs them added via ALTER
	// TABLE. SQLite has no "ADD COLUMN IF NOT EXISTS", so this always
	// attempts the ALTER and swallows exactly the "duplicate column name"
	// error a database that already has the column returns — same pattern
	// as chathistory.go's newChatHistoryStore / tokenusage.go's
	// newTokenUsageStore.
	for _, col := range []string{
		`ALTER TABLE user_prefs ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN position TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN department TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN signature TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN contact_info TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN communication_style TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN typical_phrasing TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN ai_notes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_prefs ADD COLUMN use_personal_context INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(col); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				db.Close()
				return nil, fmt.Errorf("user prefs: add personal-context columns: %w", err)
			}
		}
	}
	return &userPrefsStore{db: db}, nil
}

// close releases the underlying database handle — mainly for tests, same
// reasoning as chathistory.go's close().
func (s *userPrefsStore) close() error { return s.db.Close() }

// get returns owner's stored overrides, or a zero-value userPrefs{} (no
// error) if owner has no row yet — that's the ordinary "hasn't set a
// personal override" case, not a failure.
func (s *userPrefsStore) get(owner string) (userPrefs, error) {
	var p userPrefs
	err := s.db.QueryRow(`
		SELECT lang, display_name, position, department, signature, contact_info,
		       communication_style, typical_phrasing, ai_notes, use_personal_context
		FROM user_prefs WHERE owner = ?
	`, owner).Scan(&p.Lang, &p.DisplayName, &p.Position, &p.Department, &p.Signature,
		&p.ContactInfo, &p.CommunicationStyle, &p.TypicalPhrasing, &p.AINotes, &p.UsePersonalContext)
	if err == sql.ErrNoRows {
		return userPrefs{}, nil
	}
	return p, err
}

// setLang upserts owner's language override — lang == "" is a valid,
// explicit "clear the override, use the admin default" value, not
// rejected. Touches only the lang column: a first-ever row gets every
// personal-context column's table-level default (” / 0), and an existing
// row's personal-context fields are left exactly as they were — this is
// deliberately NOT the same upsert as setPersonalContext below, so saving
// the language (the sidebar switcher) can never clobber personal-context
// fields saved separately (the account modal), or vice versa.
func (s *userPrefsStore) setLang(owner, lang string) error {
	_, err := s.db.Exec(`
		INSERT INTO user_prefs (owner, lang) VALUES (?, ?)
		ON CONFLICT(owner) DO UPDATE SET lang = excluded.lang
	`, owner, lang)
	return err
}

// setPersonalContext upserts owner's personal-context fields — mirrors
// setLang's "touch only my own columns" shape: a first-ever row gets lang's
// table default (”), and an existing row's lang is left untouched, since
// ON CONFLICT's SET clause never mentions it.
func (s *userPrefsStore) setPersonalContext(owner string, p userPrefs) error {
	_, err := s.db.Exec(`
		INSERT INTO user_prefs (
			owner, lang, display_name, position, department, signature, contact_info,
			communication_style, typical_phrasing, ai_notes, use_personal_context
		) VALUES (?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner) DO UPDATE SET
			display_name = excluded.display_name,
			position = excluded.position,
			department = excluded.department,
			signature = excluded.signature,
			contact_info = excluded.contact_info,
			communication_style = excluded.communication_style,
			typical_phrasing = excluded.typical_phrasing,
			ai_notes = excluded.ai_notes,
			use_personal_context = excluded.use_personal_context
	`, owner, p.DisplayName, p.Position, p.Department, p.Signature, p.ContactInfo,
		p.CommunicationStyle, p.TypicalPhrasing, p.AINotes, p.UsePersonalContext)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP handlers — registered in handlers.go's registerRoutes, wrapped in
// chathistory.go's existing requireSession (any logged-in account, not
// admin-only): a personal preference override needs to know who's asking,
// same requirement chat history already has, so no new middleware.
// ─────────────────────────────────────────────────────────────────────────────

// handleUserPrefsGet serves GET /api/account/prefs: the caller's own
// overrides/preferences (lang override plus every personal-context field),
// empty string/false meaning "none set, using the admin default".
func handleUserPrefsGet(w http.ResponseWriter, r *http.Request, user string) {
	p, err := userPrefsDB.get(user)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, p)
}

type setUserPrefsRequest struct {
	Lang string `json:"lang"`
}

// handleUserPrefsSet serves POST /api/account/prefs/set: sets or clears
// (empty string) the caller's own language override. Deliberately narrow
// (lang only) — see setPersonalContext's doc comment for why personal
// context has its own separate save endpoint below.
func handleUserPrefsSet(w http.ResponseWriter, r *http.Request, user string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req setUserPrefsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body", 400)
		return
	}
	if req.Lang != "" && !isSupportedUILang(req.Lang) {
		writeJSONError(w, "lang: unbekannte Sprache", 400)
		return
	}
	if err := userPrefsDB.setLang(user, req.Lang); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// setPersonalContextRequest carries every personal-context field from the
// "Mein Konto" modal's save action — the same shape as userPrefs minus Lang
// (that field's own save path, handleUserPrefsSet above, is untouched).
type setPersonalContextRequest struct {
	DisplayName        string `json:"display_name"`
	Position           string `json:"position"`
	Department         string `json:"department"`
	Signature          string `json:"signature"`
	ContactInfo        string `json:"contact_info"`
	CommunicationStyle string `json:"communication_style"`
	TypicalPhrasing    string `json:"typical_phrasing"`
	AINotes            string `json:"ai_notes"`
	UsePersonalContext bool   `json:"use_personal_context"`
}

// handleUserPrefsSetPersonalContext serves POST /api/account/prefs/personal:
// saves the caller's own personal-context fields (userprefs.go's userPrefs,
// Phase 4) — separate from handleUserPrefsSet (lang only) so the two save
// actions (sidebar language switcher vs. the account modal's own save
// button) can never clobber each other's fields. Rejects (400) rather than
// silently truncating any field over personalContextFieldLimits.
func handleUserPrefsSetPersonalContext(w http.ResponseWriter, r *http.Request, user string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req setPersonalContextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body", 400)
		return
	}
	fields := map[string]string{
		"display_name":        req.DisplayName,
		"position":            req.Position,
		"department":          req.Department,
		"signature":           req.Signature,
		"contact_info":        req.ContactInfo,
		"communication_style": req.CommunicationStyle,
		"typical_phrasing":    req.TypicalPhrasing,
		"ai_notes":            req.AINotes,
	}
	for name, value := range fields {
		if limit := personalContextFieldLimits[name]; limit > 0 && len([]rune(value)) > limit {
			writeJSONError(w, fmt.Sprintf("%s: darf höchstens %d Zeichen lang sein", name, limit), 400)
			return
		}
	}
	p := userPrefs{
		DisplayName:        strings.TrimSpace(req.DisplayName),
		Position:           strings.TrimSpace(req.Position),
		Department:         strings.TrimSpace(req.Department),
		Signature:          strings.TrimSpace(req.Signature),
		ContactInfo:        strings.TrimSpace(req.ContactInfo),
		CommunicationStyle: strings.TrimSpace(req.CommunicationStyle),
		TypicalPhrasing:    strings.TrimSpace(req.TypicalPhrasing),
		AINotes:            strings.TrimSpace(req.AINotes),
		UsePersonalContext: req.UsePersonalContext,
	}
	if err := userPrefsDB.setPersonalContext(user, p); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
