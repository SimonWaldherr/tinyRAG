package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Real server-side sessions for LDAP-backed login (see ldapauth.go).
//
// The pre-existing admin gate (handlers.go's handleAdminCheck) only ever
// hid UI tabs — no session, no server-side enforcement, by design (a
// "convenience", per its own doc comment). Once LDAP login is enabled
// (settings.LDAP.Enabled), admin routes need to actually check who's
// asking, which needs a real session.
//
// Sessions live in an in-memory store (sessionStore below), keyed by a
// random ID; the cookie only ever carries that ID (HMAC-signed so a
// tampered/guessed ID is rejected before it even reaches a map lookup).
// This used to be a fully self-contained signed cookie (claims + expiry
// encoded directly into the cookie value, no server-side state at all) —
// that broke for AD users with enough group memberships: sessionClaims.Groups
// (a user's full memberOf list) pushed the encoded payload past browsers'
// ~4096-byte cookie size limit, so Set-Cookie was silently dropped and the
// user appeared logged in (the login POST succeeded) while every subsequent
// request 401'd. Server-side storage has no such size limit.
//
// Both the signing secret and the store persist to disk now (initSessionPersistence,
// called from main() once *settingsPath is known), so a restart no longer
// logs everyone out — same "next to settings.json, never committed"
// convention as feedback.go/audit.go/settings_history.go's runtime logs.
// This is a deliberate step away from the previous "no secret-at-rest"
// tradeoff: sessionSecretPath now holds a standing credential — anyone who
// can read that file can forge a session (any user, any IsAdmin/Groups)
// until it's rotated (delete the file and restart). Treat it like
// settings.json's credential fields: 0o600, host-local, not for backup
// media that isn't already trusted with the rest of R3's secrets.
// ─────────────────────────────────────────────────────────────────────────────

const sessionCookieName = "r3_session"
const sessionTTL = 8 * time.Hour

// sessionActiveWindow is intentionally shorter than sessionTTL: a browser
// cookie can remain valid for hours after its tab was closed, while the admin
// operations view needs to distinguish "signed in" from "active now".
const sessionActiveWindow = 15 * time.Minute

// sessionActivityWriteInterval bounds the map mutation made by request
// middleware. Activity is live-process telemetry, not audit evidence, so it
// is kept in memory and never forces a disk write for every browser request.
const sessionActivityWriteInterval = time.Minute

// sessionClaims carries everything a later request needs about who's
// asking, resolved once at login (ldapauth.go) rather than re-queried
// from AD on every request:
//   - IsAdmin: the actual admin/no-admin decision (RequiredGroupDN
//     membership) — requireAdminSession checks this field, not merely
//     "has a valid session", since login now also serves non-admin
//     employees (see ldapauth.go's package comment).
//   - Department/Title/Office/DeptCode: used to filter retrieval against
//     settings.SourceAccess and, if settings.PersonalizeAnswers is on, to
//     build the "who's asking" block handlers.go's userContextBlock adds
//     to the system prompt.
type sessionClaims struct {
	User              string
	DisplayName       string
	AccountName       string
	UserPrincipalName string
	Mail              string
	Department        string
	Title             string
	Office            string
	Company           string
	// DirectoryID is objectGUID in base64url form (ldapauth.go). It remains
	// server-only and is used exclusively to group multiple browser sessions
	// belonging to the same AD account in operational statistics.
	DirectoryID string
	IsAdmin     bool
	DeptCode    string
	// Groups is the AD memberOf list (ldapUser.MemberOf) resolved once at
	// login, same reasoning as Department/Title/Office above — lets
	// per-connector accessControl.allows/exchangeGraphConfig.AllowedGroups
	// checks run against a cached group list instead of a repeat LDAP
	// round trip on every request. Unbounded in principle, which is exactly
	// why claims now live server-side (sessionStore) instead of in the
	// cookie itself — see the package doc comment above.
	Groups     []string
	IssuedAt   int64
	LastSeenAt int64
	Expires    int64
}

// sessionSecretPath/sessionStorePath default to the working directory but
// are overridden in main() to sit next to whichever -settings file this
// instance uses (same per-instance convention as feedbackLogPath etc.),
// before initSessionPersistence reads them.
var (
	sessionSecretPath = "r3-session-secret.bin"
	sessionStorePath  = "r3-sessions.gob"
)

var sessionSecret []byte

// initSessionPersistence loads (or, on first run, creates) the HMAC signing
// secret and restores any not-yet-expired sessions saved by a previous run.
// Must run once, after sessionSecretPath/sessionStorePath are set, before
// any request touches signSession/sessionStore — main() calls it right
// after loadOrCreateSettings.
func initSessionPersistence() {
	secret, err := loadOrCreateSessionSecret(sessionSecretPath)
	if err != nil {
		log.Fatalf("session: failed to load/create signing secret at %s: %v", sessionSecretPath, err)
	}
	sessionSecret = secret

	loaded, err := loadSessionStore(sessionStorePath)
	if err != nil {
		// A corrupt/unreadable store file shouldn't stop the server from
		// starting — everyone just has to log in again, same as before
		// this feature existed.
		log.Printf("session: could not restore saved sessions from %s: %v", sessionStorePath, err)
		return
	}
	now := time.Now().Unix()
	sessionStoreMu.Lock()
	for id, claims := range loaded {
		if now <= claims.Expires {
			sessionStore[id] = claims
		}
	}
	sessionStoreMu.Unlock()
}

// loadOrCreateSessionSecret reads the persisted signing secret, generating
// and saving a fresh one on first run. Kept separate from crypto/rand's
// direct use in newSessionID so only this one value needs disk I/O.
func loadOrCreateSessionSecret(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

// signSession HMAC-signs a session ID with sessionSecret; issueSession
// appends the result to the cookie value and parseSessionCookie recomputes
// it to detect a tampered or guessed ID before ever touching sessionStore.
func signSession(id string) string {
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// sessionStore holds every live session's claims server-side, keyed by the
// random ID handed out in the cookie. A plain mutex-guarded map is enough:
// R3 is a small internal tool, sessions are capped at sessionTTL, and
// expired entries are swept lazily (on lookup, in parseSessionCookie) plus
// opportunistically on each new login — no background goroutine needed.
var (
	sessionStoreMu sync.Mutex
	sessionStore   = map[string]sessionClaims{}
)

// newSessionID generates a random, unguessable session-store key.
func newSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("session: failed to generate session id: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// issueSession sets a signed session cookie from a successfully
// authenticated ldapUser (ldapauth.go) — called once, right after
// ldapAuthenticate returns, regardless of whether that account turned out
// to be an admin (see ldapUser.IsAdmin's doc comment).
func issueSession(w http.ResponseWriter, user *ldapUser) {
	now := time.Now()
	claims := sessionClaims{
		User:              user.CN,
		DisplayName:       user.DisplayName,
		AccountName:       user.AccountName,
		UserPrincipalName: user.UserPrincipalName,
		Mail:              user.Mail,
		Department:        user.Department,
		Title:             user.Title,
		Office:            user.Office,
		Company:           user.Company,
		DirectoryID:       user.DirectoryID,
		IsAdmin:           user.IsAdmin,
		DeptCode:          user.DeptCode,
		Groups:            user.MemberOf,
		IssuedAt:          now.Unix(),
		LastSeenAt:        now.Unix(),
		Expires:           now.Add(sessionTTL).Unix(),
	}
	id := newSessionID()

	sessionStoreMu.Lock()
	sessionStore[id] = claims
	sweepExpiredSessionsLocked()
	saveSessionStoreLocked()
	sessionStoreMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id + "." + signSession(id),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is deliberately not set: R3 is commonly deployed behind
		// plain HTTP on an internal network (see docs/DEPLOYMENT.md) —
		// forcing Secure would silently break the cookie there. Terminate
		// TLS at a reverse proxy in front of R3 if serving over HTTPS.
	})
}

// sweepExpiredSessionsLocked removes expired entries from sessionStore.
// Callers must hold sessionStoreMu. Piggybacked on issueSession rather than
// run on a timer, so an idle server just keeps a few expired entries around
// a little longer instead of needing a background goroutine to shut down.
func sweepExpiredSessionsLocked() {
	now := time.Now().Unix()
	for id, claims := range sessionStore {
		if now > claims.Expires {
			delete(sessionStore, id)
		}
	}
}

// loadSessionStore reads a gob-encoded sessionStore snapshot from a
// previous run. A missing file (first run, or persistence just enabled)
// is not an error — it returns an empty map.
func loadSessionStore(path string) (map[string]sessionClaims, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]sessionClaims{}, nil
		}
		return nil, err
	}
	defer f.Close()
	var m map[string]sessionClaims
	if err := gob.NewDecoder(f).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// saveSessionStoreLocked writes sessionStore to sessionStorePath so it
// survives a restart. Callers must hold sessionStoreMu. Same
// write-to-tmp-then-rename pattern as settingsStore.saveLocked (settings.go),
// for the same reason: a crash mid-write leaves the previous, still-valid
// file in place instead of a truncated one. Runs synchronously on every
// login/logout — sessions change rarely enough (compared to per-request
// traffic) that this never sits on a hot path.
func saveSessionStoreLocked() {
	tmp := sessionStorePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("session: failed to save session store: %v", err)
		return
	}
	err = gob.NewEncoder(f).Encode(sessionStore)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		log.Printf("session: failed to save session store: %v", err)
		return
	}
	if err := os.Rename(tmp, sessionStorePath); err != nil {
		log.Printf("session: failed to save session store: %v", err)
	}
}

// sessionCtxKey is the context key sessionCacheMiddleware (main.go) stores a
// pre-parsed sessionCacheEntry under, so a request touching currentSession
// from several handlers/helpers (requireAdminSession, userContextBlock,
// handleAsk, ...) only pays for cookie parsing/HMAC verification/JSON
// unmarshaling once per request instead of once per call site.
type sessionCtxKey struct{}

type sessionCacheEntry struct {
	claims sessionClaims
	ok     bool
}

// withSessionCache parses r's session cookie once and returns a copy of r
// carrying the result in its context, for currentSession to reuse.
func withSessionCache(r *http.Request) *http.Request {
	claims, ok := parseSessionCookie(r)
	entry := sessionCacheEntry{claims: claims, ok: ok}
	return r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, entry))
}

// currentSession returns the authenticated caller's full claims and true
// if r carries a valid, unexpired session cookie — use this wherever a
// handler needs more than just the username (department-based retrieval
// filtering, personalization, the real IsAdmin check).
//
// If r was passed through withSessionCache (every request reaching
// registerRoutes' mux does, via main.go's sessionCacheMiddleware), the
// cached result is reused; otherwise (e.g. a test calling this directly
// against a bare httptest.NewRequest) it falls back to parsing the cookie
// on the spot, just without caching.
func currentSession(r *http.Request) (sessionClaims, bool) {
	if entry, ok := r.Context().Value(sessionCtxKey{}).(sessionCacheEntry); ok {
		return entry.claims, entry.ok
	}
	return parseSessionCookie(r)
}

// parseSessionCookie does the actual cookie decode/HMAC-verify/store-lookup
// work — split out from currentSession so withSessionCache can run it once
// per request and cache the result.
func parseSessionCookie(r *http.Request) (sessionClaims, bool) {
	id, ok := verifiedSessionID(r)
	if !ok {
		return sessionClaims{}, false
	}

	now := time.Now().Unix()
	sessionStoreMu.Lock()
	claims, ok := sessionStore[id]
	if !ok {
		sessionStoreMu.Unlock()
		return sessionClaims{}, false
	}
	if now > claims.Expires {
		delete(sessionStore, id)
		sessionStoreMu.Unlock()
		return sessionClaims{}, false
	}
	if claims.LastSeenAt == 0 || now-claims.LastSeenAt >= int64(sessionActivityWriteInterval.Seconds()) {
		claims.LastSeenAt = now
		sessionStore[id] = claims
	}
	sessionStoreMu.Unlock()
	return claims, true
}

// verifiedSessionID extracts r's session ID and confirms its HMAC before
// any sessionStore lookup, so a tampered or made-up cookie value is
// rejected outright instead of just missing the map.
func verifiedSessionID(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	id, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return "", false
	}
	if !hmac.Equal([]byte(sig), []byte(signSession(id))) {
		return "", false
	}
	return id, true
}

// currentSessionUser is currentSession's thin username-only counterpart,
// for call sites that only need to know who's asking, not their
// admin/department claims.
func currentSessionUser(r *http.Request) (string, bool) {
	claims, ok := currentSession(r)
	return claims.User, ok
}

// sessionActor returns the most useful stable, human-readable identity for
// audit logs and agent attribution. AD installations do not guarantee mail,
// but an UPN or sAMAccountName is commonly present; only old/minimal entries
// fall back to CN. This is deliberately separate from claims.User, whose CN
// semantics remain the backwards-compatible owner key for saved data.
func sessionActor(claims sessionClaims) string {
	for _, value := range []string{claims.Mail, claims.UserPrincipalName, claims.AccountName, claims.User} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "anonym"
}

// sessionDisplayName is presentation-only. It keeps user-facing surfaces
// friendly without changing the durable CN-based owner key above.
func sessionDisplayName(claims sessionClaims) string {
	for _, value := range []string{claims.DisplayName, claims.User, claims.AccountName, claims.Mail} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "Unbekannt"
}

// clearSession logs the current visitor out: drops their entry from
// sessionStore (if r still carries a valid session cookie) and expires the
// cookie itself so the browser stops sending it.
func clearSession(w http.ResponseWriter, r *http.Request) {
	if id, ok := verifiedSessionID(r); ok {
		sessionStoreMu.Lock()
		delete(sessionStore, id)
		saveSessionStoreLocked()
		sessionStoreMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:   sessionCookieName,
		Path:   "/",
		MaxAge: -1,
	})
}
