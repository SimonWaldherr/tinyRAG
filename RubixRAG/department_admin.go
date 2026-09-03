package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// ─────────────────────────────────────────────────────────────────────────────
// Admin CRUD for department_rules.json (department.go) — same directory
// and "no caching, re-read on every use, missing file falls back to a
// built-in default" shape as prompts_admin.go/skills.go, just for one
// file instead of index.md + skill_*.md.
// ─────────────────────────────────────────────────────────────────────────────

// departmentRulesResponse is what GET /api/department-rules returns — the
// effective ruleset plus whether it came from an on-disk override or the
// built-in default, so the admin UI can show which one is actually active.
type departmentRulesResponse struct {
	Rules      []departmentRule `json:"rules"`
	Overridden bool             `json:"overridden"`
}

// handleDepartmentRules returns the currently effective ruleset — the
// override file's content if present and valid, else defaultDepartmentRules
// — for the admin UI's editor to load into its textarea.
func handleDepartmentRules(w http.ResponseWriter, r *http.Request) {
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	rules, ok := loadDepartmentRulesFromFile(filepath.Join(dir, departmentRulesFilename))
	if !ok {
		rules = defaultDepartmentRules
	}
	writeJSON(w, departmentRulesResponse{Rules: rules, Overridden: ok})
}

type saveDepartmentRulesRequest struct {
	Rules []departmentRule `json:"rules"`
}

// handleDepartmentRulesSave validates every rule (non-empty code, regex
// compiles) before writing anything — a bad rule here would silently break
// department classification for every login afterward (see
// ldapauth.go/department.go), so this fails loudly at save time instead
// of at the next person's login.
func handleDepartmentRulesSave(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req saveDepartmentRulesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	if err := validateDepartmentRules(req.Rules); err != nil {
		writeJSONError(w, err.Error(), 400)
		return
	}
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	b, err := json.MarshalIndent(req.Rules, "", "  ")
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(dir, departmentRulesFilename), b, 0o644); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleDepartmentRulesReset deletes the override file, reverting to
// defaultDepartmentRules — distinct from saving an empty rule list (which
// would mean "classify nothing ever", a valid but very different choice
// an admin might not intend).
func handleDepartmentRulesReset(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	_ = os.Remove(filepath.Join(dir, departmentRulesFilename)) // best-effort; a missing file is already the reset state
	writeJSON(w, map[string]bool{"ok": true})
}
