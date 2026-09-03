package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTestPromptsDir points settings.PromptsDir at a fresh temp directory
// for the duration of one test, so skill files/manifest.json/index.md
// writes never leak between tests or touch the real "prompts" directory.
func withTestPromptsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, s := newTestRAG(t)
	s.PromptsDir = dir
	withTestGlobalSettings(t, s)
	return dir
}

// TestHandlePromptsReportsIsDefaultFlags is the regression guard for the
// Prompts tab's "using built-in default" indicator: with no index.md/
// draft.md/agent.md saved yet, all three *IsDefault flags must be true
// (and the *Content fields must show the actual fallback text, not a
// blank box) — then false once a real file exists, even an unrelated one.
func TestHandlePromptsReportsIsDefaultFlags(t *testing.T) {
	dir := withTestPromptsDir(t)

	rec := httptest.NewRecorder()
	handlePrompts(rec, httptest.NewRequest(http.MethodGet, "/api/prompts", nil))
	var res promptsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if !res.IndexIsDefault || !res.DraftIsDefault || !res.AgentIsDefault {
		t.Fatalf("want all three *IsDefault=true with no files saved, got %+v", res)
	}
	if res.IndexContent != defaultSystemPrompt {
		t.Fatalf("want IndexContent to show the actual fallback text, got %q", res.IndexContent)
	}

	if err := os.WriteFile(filepath.Join(dir, promptsIndexFile), []byte("Ein eigener System-Prompt."), 0o644); err != nil {
		t.Fatalf("write index.md: %v", err)
	}

	rec = httptest.NewRecorder()
	handlePrompts(rec, httptest.NewRequest(http.MethodGet, "/api/prompts", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if res.IndexIsDefault {
		t.Fatalf("want IndexIsDefault=false once index.md is saved, got %+v", res)
	}
	if !res.DraftIsDefault || !res.AgentIsDefault {
		t.Fatalf("want draft/agent still default (untouched), got %+v", res)
	}
}

// TestHandlePromptsSkillTestMatchesSavedSkill confirms the tester endpoint
// reflects exactly the skills saved on disk (via selectSkills, skills.go)
// — including the plural/stemming fallback (tagTermMatches), since this
// preview must never drift from real runtime behavior.
func TestHandlePromptsSkillTestMatchesSavedSkill(t *testing.T) {
	dir := withTestPromptsDir(t)
	skillContent := "---\nname: PSA\ntags: [handschuh]\nenabled: true\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "skill_ppe.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}

	body, _ := json.Marshal(skillTestRequest{Question: "Welche Handschuhe brauche ich?"})
	rec := httptest.NewRecorder()
	handlePromptsSkillTest(rec, httptest.NewRequest(http.MethodPost, "/api/prompts/skill-test", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var res skillTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(res.Selected) != 1 || res.Selected[0] != "PSA" {
		t.Fatalf(`want selected=["PSA"], got %+v`, res.Selected)
	}
}

// TestHandlePromptsSkillTestNoMatch confirms a question matching nothing
// returns an empty (not nil-panicking, not erroring) selection.
func TestHandlePromptsSkillTestNoMatch(t *testing.T) {
	dir := withTestPromptsDir(t)
	skillContent := "---\nname: PSA\ntags: [handschuh]\nenabled: true\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "skill_ppe.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}

	body, _ := json.Marshal(skillTestRequest{Question: "Wie beantrage ich Urlaub?"})
	rec := httptest.NewRecorder()
	handlePromptsSkillTest(rec, httptest.NewRequest(http.MethodPost, "/api/prompts/skill-test", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"selected":null`) && !strings.Contains(rec.Body.String(), `"selected":[]`) {
		t.Fatalf("want an empty selection for a non-matching question, got %s", rec.Body.String())
	}
}

// TestHandlePromptsSkillTestRequiresAdminSession confirms the endpoint is
// wired through requireAdminSession, matching every other prompts/skill
// admin endpoint — a skill's tags aren't secret, but the underlying
// question is arbitrary caller-supplied text evaluated against the
// server's live skill config, same trust boundary as everything else here.
func TestHandlePromptsSkillTestRequiresAdminSession(t *testing.T) {
	_, s := newTestRAG(t)
	s.LDAP.Enabled = true
	withTestGlobalSettings(t, s)

	rec := httptest.NewRecorder()
	requireAdminSession(handlePromptsSkillTest)(rec, httptest.NewRequest(http.MethodPost, "/api/prompts/skill-test", bytes.NewReader([]byte(`{}`))))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without an admin session when LDAP is enabled, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}
