package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// newTestLocalUserStores returns one localUserStore per backend, each
// backed by a fresh temp file/dir, so CRUD behavior is verified against
// both implementations with the same test bodies (table-driven over
// backend, mirroring how vectorstore has two implementations of one
// interface).
func newTestLocalUserStores(t *testing.T) map[string]localUserStore {
	t.Helper()
	dir := t.TempDir()
	sqliteStore, err := newLocalUserStore(storageSettings{Backend: "sqlite", UsersPath: filepath.Join(dir, "users.db")})
	if err != nil {
		t.Fatalf("newLocalUserStore(sqlite): %v", err)
	}
	if err := sqliteStore.init(); err != nil {
		t.Fatalf("init(sqlite): %v", err)
	}
	tinySQLStore, err := newLocalUserStore(storageSettings{Backend: "tinysql", UsersPath: filepath.Join(dir, "users-tinysql")})
	if err != nil {
		t.Fatalf("newLocalUserStore(tinysql): %v", err)
	}
	if err := tinySQLStore.init(); err != nil {
		t.Fatalf("init(tinysql): %v", err)
	}
	// Release file handles before t.TempDir() cleanup runs — Windows can't
	// remove an open file (same reasoning as vectorstore_sqlite_test.go's
	// own Close cleanup).
	t.Cleanup(func() {
		if c, ok := sqliteStore.(interface{ Close() error }); ok {
			_ = c.Close()
		}
		if c, ok := tinySQLStore.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	return map[string]localUserStore{"sqlite": sqliteStore, "tinysql": tinySQLStore}
}

func TestLocalUserStoreCRUD(t *testing.T) {
	for name, store := range newTestLocalUserStores(t) {
		t.Run(name, func(t *testing.T) {
			hash, err := hashLocalPassword("correct horse battery staple", 4) // low cost: fast tests
			if err != nil {
				t.Fatalf("hashLocalPassword: %v", err)
			}
			u := localUser{Username: "Alice", PasswordHash: hash, DisplayName: "Alice Admin", Email: "alice@example.com", Department: "IT", DeptCode: "it", IsAdmin: true}
			if err := store.create(u); err != nil {
				t.Fatalf("create: %v", err)
			}

			// Lookup is case-insensitive.
			got, ok, err := store.getByUsername("alice")
			if err != nil || !ok {
				t.Fatalf("getByUsername(alice) = (%+v, %v, %v), want found", got, ok, err)
			}
			if got.DisplayName != "Alice Admin" || !got.IsAdmin {
				t.Errorf("got %+v, want display_name=Alice Admin is_admin=true", got)
			}
			if got.ID == "" {
				t.Fatal("want a generated ID")
			}

			byID, ok, err := store.getByID(got.ID)
			if err != nil || !ok || byID.Username != "alice" {
				t.Fatalf("getByID(%q) = (%+v, %v, %v), want found alice", got.ID, byID, ok, err)
			}

			// Second create with the same (differently-cased) username fails —
			// handlers_local_users.go relies on getByUsername for its own
			// pre-check, but the store itself must also refuse a duplicate so
			// a race between two concurrent creates can't silently succeed.
			if err := store.create(localUser{Username: "ALICE", PasswordHash: hash}); err == nil {
				t.Fatal("want duplicate username to fail")
			}

			list, err := store.list()
			if err != nil || len(list) != 1 {
				t.Fatalf("list() = (%+v, %v), want exactly one user", list, err)
			}

			got.DisplayName = "Alice A. Admin"
			got.Disabled = true
			if err := store.update(got); err != nil {
				t.Fatalf("update: %v", err)
			}
			updated, _, _ := store.getByID(got.ID)
			if updated.DisplayName != "Alice A. Admin" || !updated.Disabled {
				t.Errorf("update didn't stick: %+v", updated)
			}

			newHash, _ := hashLocalPassword("a different password", 4)
			if err := store.setPassword(got.ID, newHash); err != nil {
				t.Fatalf("setPassword: %v", err)
			}
			afterPW, _, _ := store.getByID(got.ID)
			if afterPW.PasswordHash != newHash {
				t.Error("setPassword didn't stick")
			}
			if afterPW.DisplayName != "Alice A. Admin" {
				t.Error("setPassword must not touch other fields")
			}

			if err := store.delete(got.ID); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, ok, _ := store.getByID(got.ID); ok {
				t.Fatal("want user gone after delete")
			}
			if err := store.delete(got.ID); err == nil {
				t.Fatal("want deleting an already-deleted id to fail")
			}
		})
	}
}

func TestLocalAuthenticate(t *testing.T) {
	store := newTestLocalUserStores(t)["sqlite"]
	hash, _ := hashLocalPassword("correcthorsebatterystaple", 4)
	if err := store.create(localUser{Username: "bob", PasswordHash: hash, DisplayName: "Bob", DeptCode: "sales"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.create(localUser{Username: "carol", PasswordHash: hash, Disabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := localAuthenticate(store, "bob", "wrong password"); err == nil {
		t.Fatal("want wrong password to fail")
	}
	if _, err := localAuthenticate(store, "nobody", "whatever"); err == nil {
		t.Fatal("want unknown username to fail")
	}
	if _, err := localAuthenticate(store, "carol", "correcthorsebatterystaple"); err == nil {
		t.Fatal("want disabled account to fail even with the right password")
	}
	u, err := localAuthenticate(store, "Bob", "correcthorsebatterystaple") // case-insensitive username
	if err != nil {
		t.Fatalf("want correct login to succeed, got %v", err)
	}
	if u.AccountName != "bob" || u.DeptCode != "sales" || u.IsAdmin {
		t.Errorf("localUserToLdapUser mapping wrong: %+v", u)
	}
}

// TestHandleLocalUsersEndToEnd drives the actual HTTP handlers (not the
// store directly), confirming the wire contract: create -> list -> update
// -> set password -> delete, and that PasswordHash never appears in any
// response body.
func TestHandleLocalUsersEndToEnd(t *testing.T) {
	prevUsers := localUsers
	t.Cleanup(func() { localUsers = prevUsers })
	store, err := newLocalUserStore(storageSettings{Backend: "sqlite", UsersPath: filepath.Join(t.TempDir(), "users.db")})
	if err != nil {
		t.Fatalf("newLocalUserStore: %v", err)
	}
	if err := store.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	localUsers = store
	t.Cleanup(func() {
		if c, ok := store.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	withTestGlobalSettings(t, appSettings{LocalAuth: localAuthConfig{Enabled: true, MinPasswordLength: 8, BcryptCost: 4}})

	post := func(path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		switch path {
		case "/api/admin/users/create":
			handleLocalUserCreate(rec, req)
		case "/api/admin/users/update":
			handleLocalUserUpdate(rec, req)
		case "/api/admin/users/password":
			handleLocalUserSetPassword(rec, req)
		case "/api/admin/users/delete":
			handleLocalUserDelete(rec, req)
		}
		return rec
	}

	rec := post("/api/admin/users/create", localUserCreateRequest{Username: "dave", Password: "long enough password", DisplayName: "Dave"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("password_hash")) || bytes.Contains(rec.Body.Bytes(), []byte("$2a$")) {
		t.Fatalf("create response must never include the password hash: %s", rec.Body.String())
	}
	var created struct {
		User localUserView `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.User.ID == "" {
		t.Fatal("want a non-empty id in the create response")
	}

	// Too-short password is rejected (MinPasswordLength: 8).
	rec = post("/api/admin/users/create", localUserCreateRequest{Username: "short", Password: "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for too-short password, got %d", rec.Code)
	}

	// Duplicate username is rejected.
	rec = post("/api/admin/users/create", localUserCreateRequest{Username: "dave", Password: "long enough password"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 for duplicate username, got %d", rec.Code)
	}

	listRec := httptest.NewRecorder()
	handleLocalUsersList(listRec, httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
	var listResp struct {
		Users []localUserView `json:"users"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil || len(listResp.Users) != 1 {
		t.Fatalf("list: got %s, err %v", listRec.Body.String(), err)
	}

	rec = post("/api/admin/users/update", localUserUpdateRequest{ID: created.User.ID, Username: "dave", DisplayName: "David", Disabled: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = post("/api/admin/users/password", localUserSetPasswordRequest{ID: created.User.ID, Password: "a brand new long password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set password: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = post("/api/admin/users/delete", localUserDeleteRequest{ID: created.User.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := localUsers.getByID(created.User.ID); ok {
		t.Fatal("want user gone after delete")
	}
}

// TestRequireAdminSessionWithLocalAuthOnly is the regression test for the
// security fix in handlers.go: with LDAP off but LocalAuth on, a non-admin
// local session must still be rejected by requireAdminSession — before the
// fix, requireAdminSession only checked settings.LDAP.Enabled and would
// have let ANY request through once LDAP was off, regardless of whether a
// local, non-admin session existed.
func TestRequireAdminSessionWithLocalAuthOnly(t *testing.T) {
	withTestGlobalSettings(t, appSettings{LocalAuth: localAuthConfig{Enabled: true}})

	called := false
	protected := requireAdminSession(func(w http.ResponseWriter, r *http.Request) { called = true })

	// No session at all: rejected.
	w := httptest.NewRecorder()
	protected(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if called || w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: want 401 and handler not called, got code=%d called=%v", w.Code, called)
	}

	// Non-admin local session: rejected.
	called = false
	w = httptest.NewRecorder()
	issueSession(w, &ldapUser{CN: "dave", IsAdmin: false})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	w2 := httptest.NewRecorder()
	protected(w2, r)
	if called || w2.Code != http.StatusUnauthorized {
		t.Fatalf("non-admin local session: want 401 and handler not called, got code=%d called=%v", w2.Code, called)
	}

	// Admin local session: allowed.
	called = false
	w = httptest.NewRecorder()
	issueSession(w, &ldapUser{CN: "carol", IsAdmin: true})
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	w3 := httptest.NewRecorder()
	protected(w3, r)
	if !called || w3.Code != http.StatusOK {
		t.Fatalf("admin local session: want handler called, got code=%d called=%v", w3.Code, called)
	}
}
