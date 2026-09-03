package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ─────────────────────────────────────────────────────────────────────────────
// localUserStore: individually-provisioned local accounts (settings.
// LocalAuth), an alternative and complement to LDAP/AD login — see
// appSettings.LocalAuth's doc comment (settings.go) for the "why".
//
// Deliberately its OWN small abstraction, not folded into vectorStore
// (vectorstore.go): that interface is bluntly chunk/vector-centric by
// design (see its own header comment), and a users table has nothing to do
// with embeddings/search. Two implementations exist — sqliteLocalUserStore
// (localusers_sqlite.go) and tinySQLLocalUserStore (localusers_tinysql.go) —
// selected the same way as vectorStore, via storageSettings.Backend, but
// each opens its OWN file (storageSettings.UsersPath), not the chunk
// store's file: sqliteStore deliberately caps its connection pool at 1
// (vectorstore_sqlite.go) to avoid SQLITE_BUSY, so a second independent
// *sql.DB on the very same file would reintroduce exactly that problem.
// ─────────────────────────────────────────────────────────────────────────────

// localUser is one local account row. PasswordHash is a bcrypt hash (salt
// and cost embedded in the string itself — see hashLocalPassword) and never
// leaves the server in any API response.
type localUser struct {
	ID           string
	Username     string
	PasswordHash string
	DisplayName  string
	Email        string
	// Department/DeptCode mirror ldapUser's fields of the same name, so a
	// local account is filtered by settings.SourceAccess/sourceACL exactly
	// like an AD one — DeptCode is normally classifyDepartment(Department,
	// "") at creation time, but an admin can also just type a known
	// department code directly (see handlers_local_users.go).
	Department string
	DeptCode   string
	IsAdmin    bool
	// Disabled accounts fail login (localAuthenticate) but are kept, not
	// deleted, so an admin can see/re-enable them later and existing
	// chat-history/preferences rows (keyed by CN — see ldapUser.CN's own
	// doc comment) aren't orphaned by a name that could otherwise be
	// reused.
	Disabled  bool
	CreatedAt int64
	UpdatedAt int64
}

// localUsers is the process-wide store, opened once in main() right after
// newVectorStore — same "global, opened once at startup" convention as
// chatHistory/tokenUsage/userPrefsDB.
var localUsers localUserStore

// localUserStore is the storage seam every admin-API handler
// (handlers_local_users.go) and the login flow (handleLDAPLogin) talk to.
type localUserStore interface {
	init() error
	create(u localUser) error
	getByUsername(username string) (localUser, bool, error)
	getByID(id string) (localUser, bool, error)
	list() ([]localUser, error)
	// update changes every field except PasswordHash — see setPassword for
	// why that's split out.
	update(u localUser) error
	setPassword(id, passwordHash string) error
	delete(id string) error
}

// newLocalUserStore constructs the configured backend — mirrors
// newVectorStore (vectorstore.go) exactly, just for a different table.
func newLocalUserStore(cfg storageSettings) (localUserStore, error) {
	switch cfg.Backend {
	case "", "tinysql":
		return newTinySQLLocalUserStore(cfg)
	case "sqlite":
		return newSQLiteLocalUserStore(cfg)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Backend)
	}
}

// newLocalUserID generates a random, unguessable account id — same
// construction as session.go's newSessionID, just hex-encoded (shorter,
// friendlier to show/copy in the admin UI) and prefixed so it's visibly
// distinct from an AD DirectoryID at a glance in logs/exports.
func newLocalUserID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("localusers: failed to generate account id: " + err.Error())
	}
	return "local:" + hex.EncodeToString(b)
}

// normalizeUsername is applied before every lookup/uniqueness check so
// "Alice" and "alice" are always the same account.
func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// sortLocalUsersByUsername orders list() results — tinySQLLocalUserStore
// needs this explicitly (a raw "SELECT *" has no guaranteed row order);
// sqliteLocalUserStore already gets the same order via "ORDER BY username
// ASC" in SQL, so calling this there too would just be a harmless no-op
// re-sort, but isn't needed.
func sortLocalUsersByUsername(users []localUser) {
	sort.Slice(users, func(i, j int) bool {
		return users[i].Username < users[j].Username
	})
}

// hashLocalPassword bcrypt-hashes password at cost (localAuthConfig.
// effectiveBcryptCost) — bcrypt generates and embeds its own random salt,
// so no separate salt column/parameter is needed (unlike apikey.go's
// hashAPIKey, which uses unsalted SHA-256 — acceptable there only because
// API keys are high-entropy random tokens, never true for a user-chosen
// password).
func hashLocalPassword(password string, cost int) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(b), nil
}

// verifyLocalPassword reports whether password matches hash. bcrypt's own
// comparison is constant-time with respect to the password bytes.
func verifyLocalPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// localUserToLdapUser adapts a localUser into the same *ldapUser shape
// ldapAuthenticate returns, so it flows into issueSession (session.go)
// completely unchanged — see handleLDAPLogin's local-auth branch. MemberOf
// stays nil (no AD groups); DirectoryID reuses the account's own local id
// since that already serves the same "stable, never reused, server-only"
// role DirectoryID documents for AD accounts.
func localUserToLdapUser(u localUser) *ldapUser {
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	return &ldapUser{
		CN:          u.Username,
		DisplayName: name,
		AccountName: u.Username,
		Mail:        u.Email,
		Department:  u.Department,
		DirectoryID: u.ID,
		IsAdmin:     u.IsAdmin,
		DeptCode:    u.DeptCode,
	}
}

// localAuthenticate looks up username in store and verifies password,
// returning the same (*ldapUser, error) shape ldapAuthenticate uses so
// handleLDAPLogin's local-auth branch reads identically to its LDAP one. A
// disabled account or a lookup miss both fail closed with the same generic
// error message — deliberately not distinguishing "unknown user" from
// "wrong password" or "disabled", the same anti-enumeration posture
// ldapAuthenticate already has for a failed bind.
func localAuthenticate(store localUserStore, username, password string) (*ldapUser, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, fmt.Errorf("username and password required")
	}
	u, ok, err := store.getByUsername(username)
	if err != nil {
		return nil, fmt.Errorf("look up local user: %w", err)
	}
	if !ok || u.Disabled || !verifyLocalPassword(u.PasswordHash, password) {
		return nil, fmt.Errorf("invalid username or password")
	}
	return localUserToLdapUser(u), nil
}
