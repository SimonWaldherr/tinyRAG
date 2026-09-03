package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Admin CRUD for local user accounts (localusers.go) — settings.LocalAuth
// must be enabled for handleLDAPLogin to actually accept these accounts at
// login time, but the admin API itself only needs requireAdminSession
// (mirrors handleSourceACL's shape: one small, explicit request type per
// action rather than one do-everything endpoint). Every response is a
// localUserView — PasswordHash never leaves the server.
// ─────────────────────────────────────────────────────────────────────────────

// localUserView is the API-facing shape of a localUser — same fields minus
// PasswordHash.
type localUserView struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Department  string `json:"department"`
	DeptCode    string `json:"dept_code"`
	IsAdmin     bool   `json:"is_admin"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

func localUserToView(u localUser) localUserView {
	return localUserView{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
		Department: u.Department, DeptCode: u.DeptCode, IsAdmin: u.IsAdmin, Disabled: u.Disabled,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

// handleLocalUsersList serves GET /api/admin/users: every local account,
// most recently created first isn't guaranteed by localUserStore.list (it
// sorts by username) — that's a deliberate, simpler contract for an admin
// table the browser can also re-sort client-side.
func handleLocalUsersList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	users, err := localUsers.list()
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]localUserView, 0, len(users))
	for _, u := range users {
		views = append(views, localUserToView(u))
	}
	writeJSON(w, map[string]any{"users": views})
}

// localUserCreateRequest is POST /api/admin/users' body. Department is
// admin-typed free text (like ldapUser.Department would be from AD);
// DeptCode defaults to classifyDepartment(Department, "") if left blank, so
// an admin only has to pick a department code explicitly when the
// free-text department name doesn't already match a configured
// department_rules.json pattern.
type localUserCreateRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Department  string `json:"department,omitempty"`
	DeptCode    string `json:"dept_code,omitempty"`
	IsAdmin     bool   `json:"is_admin,omitempty"`
}

// handleLocalUserCreate serves POST /api/admin/users.
func handleLocalUserCreate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req localUserCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeJSONError(w, "missing username", http.StatusBadRequest)
		return
	}
	s := settings.get()
	if minLen := s.LocalAuth.effectiveMinPasswordLength(); len(req.Password) < minLen {
		writeJSONError(w, fmt.Sprintf("password must be at least %d characters", minLen), http.StatusBadRequest)
		return
	}
	if _, exists, err := localUsers.getByUsername(req.Username); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	} else if exists {
		writeJSONError(w, "a user with this username already exists", http.StatusConflict)
		return
	}
	hash, err := hashLocalPassword(req.Password, s.LocalAuth.effectiveBcryptCost())
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	deptCode := strings.TrimSpace(req.DeptCode)
	if deptCode == "" {
		deptCode = classifyDepartment(departmentRulesOrDefault(s.PromptsDir), req.Department, "")
	}
	u := localUser{
		Username: req.Username, PasswordHash: hash, DisplayName: strings.TrimSpace(req.DisplayName),
		Email: strings.TrimSpace(req.Email), Department: strings.TrimSpace(req.Department), DeptCode: deptCode, IsAdmin: req.IsAdmin,
	}
	if err := localUsers.create(u); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	created, _, err := localUsers.getByUsername(req.Username)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logAudit(r, "local_user_create", fmt.Sprintf("username=%q is_admin=%v", created.Username, created.IsAdmin))
	writeJSON(w, map[string]any{"ok": true, "user": localUserToView(created)})
}

// localUserUpdateRequest is POST /api/admin/users/update's body — every
// field except the password (see handleLocalUserSetPassword for why that's
// split into its own endpoint).
type localUserUpdateRequest struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Department  string `json:"department,omitempty"`
	DeptCode    string `json:"dept_code,omitempty"`
	IsAdmin     bool   `json:"is_admin,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

// handleLocalUserUpdate serves POST /api/admin/users/update.
func handleLocalUserUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req localUserUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Username = strings.TrimSpace(req.Username)
	if req.ID == "" || req.Username == "" {
		writeJSONError(w, "missing id or username", http.StatusBadRequest)
		return
	}
	if existing, exists, err := localUsers.getByUsername(req.Username); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	} else if exists && existing.ID != req.ID {
		writeJSONError(w, "a different user with this username already exists", http.StatusConflict)
		return
	}
	u := localUser{
		ID: req.ID, Username: req.Username, DisplayName: strings.TrimSpace(req.DisplayName),
		Email: strings.TrimSpace(req.Email), Department: strings.TrimSpace(req.Department),
		DeptCode: strings.TrimSpace(req.DeptCode), IsAdmin: req.IsAdmin, Disabled: req.Disabled,
	}
	if err := localUsers.update(u); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	updated, _, err := localUsers.getByID(req.ID)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logAudit(r, "local_user_update", fmt.Sprintf("id=%q username=%q is_admin=%v disabled=%v", updated.ID, updated.Username, updated.IsAdmin, updated.Disabled))
	writeJSON(w, map[string]any{"ok": true, "user": localUserToView(updated)})
}

// localUserSetPasswordRequest is POST /api/admin/users/password's body — a
// separate endpoint from update above so a plain "edit display name"
// request can never accidentally blank/overwrite the password hash.
type localUserSetPasswordRequest struct {
	ID       string `json:"id"`
	Password string `json:"password"`
}

// handleLocalUserSetPassword serves POST /api/admin/users/password.
func handleLocalUserSetPassword(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req localUserSetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeJSONError(w, "missing id", http.StatusBadRequest)
		return
	}
	s := settings.get()
	if minLen := s.LocalAuth.effectiveMinPasswordLength(); len(req.Password) < minLen {
		writeJSONError(w, fmt.Sprintf("password must be at least %d characters", minLen), http.StatusBadRequest)
		return
	}
	hash, err := hashLocalPassword(req.Password, s.LocalAuth.effectiveBcryptCost())
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := localUsers.setPassword(req.ID, hash); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logAudit(r, "local_user_set_password", fmt.Sprintf("id=%q", req.ID))
	writeJSON(w, map[string]any{"ok": true})
}

// localUserDeleteRequest is POST /api/admin/users/delete's body.
type localUserDeleteRequest struct {
	ID string `json:"id"`
}

// handleLocalUserDelete serves POST /api/admin/users/delete. Hard delete
// (unlike Disabled, which is the recommended way to revoke access while
// keeping the account's history/audit trail intact) — an admin who wants
// the account gone rather than merely locked out gets that here.
func handleLocalUserDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req localUserDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeJSONError(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := localUsers.delete(req.ID); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logAudit(r, "local_user_delete", fmt.Sprintf("id=%q", req.ID))
	writeJSON(w, map[string]any{"ok": true})
}
