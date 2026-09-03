package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Admin CRUD for the prompt/skill framework (skills.go). Small enough a
// directory (index.md + a handful of skill_*.md files) that returning
// everything in one GET and re-reading fresh on every request (see
// skills.go's buildSystemPrompt) is simpler than paginating or caching.
// ─────────────────────────────────────────────────────────────────────────────

type skillWithContent struct {
	Filename    string   `json:"filename"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description,omitempty"`
	Enabled     bool     `json:"enabled"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content"`
}

type promptsResponse struct {
	IndexContent string `json:"index_content"`
	// IndexIsDefault/DraftIsDefault/AgentIsDefault report whether the
	// corresponding *Content field is actually a saved file's content, or
	// the built-in fallback (defaultSystemPrompt/defaultDraftSystemPrompt/
	// defaultAgentSystemPrompt, skills.go/draft.go) because no file exists
	// yet — the Prompts tab otherwise has no way to tell the two apart:
	// IndexContent is populated either way, so an admin looking at a
	// non-empty text box can't tell "this is what's actually configured"
	// from "this is just what the deployment falls back to when nothing's
	// been saved".
	IndexIsDefault bool               `json:"index_is_default"`
	DraftContent   string             `json:"draft_content"`
	DraftIsDefault bool               `json:"draft_is_default"`
	AgentContent   string             `json:"agent_content"`
	AgentIsDefault bool               `json:"agent_is_default"`
	Skills         []skillWithContent `json:"skills"`
}

// handlePrompts returns the index/draft/agent prompt files plus every
// skill's manifest metadata and full file content in one response — the
// admin UI's single data load for rendering the whole Prompts tab.
func handlePrompts(w http.ResponseWriter, r *http.Request) {
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	entries, err := loadSkillsFromDir(dir)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	skills := make([]skillWithContent, 0, len(entries))
	for _, e := range entries {
		content, _ := readSkillContent(dir, e.Filename) // best-effort: missing file shows empty, still editable
		skills = append(skills, skillWithContent{
			Filename: e.Filename, DisplayName: e.DisplayName, Description: e.Description,
			Enabled: e.Enabled, Tags: e.Tags, Content: content,
		})
	}

	// readIndexPrompt/readDraftPrompt/readAgentPrompt (skills.go/draft.go)
	// are the SAME read-with-fallback functions buildSystemPrompt/
	// composeDraftReply actually use at request time — reusing them here
	// (rather than a separate os.ReadFile) guarantees the Prompts tab
	// shows exactly the prompt text actually in effect, including the
	// built-in default when no file has been saved yet (or the saved file
	// is empty/whitespace-only, which those functions also treat as "no
	// real content" — see their own doc comments). IsDefault compares
	// against the known constant rather than re-checking file existence
	// separately, so it can never disagree with what IndexContent/
	// DraftContent/AgentContent actually shows.
	indexContent := readIndexPrompt(dir)
	draftContent := readDraftPrompt(dir)
	agentContent := readAgentPrompt(dir)

	writeJSON(w, promptsResponse{
		IndexContent:   indexContent,
		IndexIsDefault: indexContent == defaultSystemPrompt,
		DraftContent:   draftContent,
		DraftIsDefault: draftContent == defaultDraftSystemPrompt,
		AgentContent:   agentContent,
		AgentIsDefault: agentContent == defaultAgentSystemPrompt,
		Skills:         skills,
	})
}

type saveIndexRequest struct {
	Content string `json:"content"`
}

// handlePromptsSaveIndex overwrites index.md verbatim with the submitted
// content — no validation beyond decoding the request body, since the index
// is free-form prose consumed only by buildSystemPrompt.
func handlePromptsSaveIndex(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req saveIndexRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, promptsIndexFile), []byte(req.Content), 0o644); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handlePromptsSaveDraftReply overwrites draft.md verbatim — the system
// prompt composeDraftReply (draft.go) uses for both the Mail tab and the
// PST-source draft-reply button. Mirrors handlePromptsSaveIndex exactly,
// just a different target file.
func handlePromptsSaveDraftReply(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req saveIndexRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, draftPromptFile), []byte(req.Content), 0o644); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handlePromptsSaveAgent overwrites agent.md verbatim — the base system
// prompt buildSystemPromptForMode uses for the Agent tab. Mirrors
// handlePromptsSaveIndex exactly, just a different target file.
func handlePromptsSaveAgent(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req saveIndexRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, agentPromptFile), []byte(req.Content), 0o644); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type saveSkillRequest struct {
	Filename    string   `json:"filename"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content"`
}

// handlePromptsSaveSkill creates or updates one skill_*.md file and its
// manifest entry in a single call — the admin UI has one "Save" per skill
// covering both content and metadata.
func handlePromptsSaveSkill(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req saveSkillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	filename := strings.TrimSpace(req.Filename)
	if !isValidSkillFilename(filename) {
		writeJSONError(w, `filename must match skill_<name>.md (letters, digits, "_", "-" only)`, 400)
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = filename
	}

	dir := promptsDirOrDefault(settings.get().PromptsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	fm := skillFrontmatter{
		Name:        displayName,
		Description: strings.TrimSpace(req.Description),
		Enabled:     req.Enabled,
		Tags:        req.Tags,
	}
	fileContent := marshalFrontmatter(fm, req.Content)
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(fileContent), 0o644); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}

	entries, err := loadManifest(dir)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	updated := skillEntry{Filename: filename, DisplayName: displayName, Description: strings.TrimSpace(req.Description), Enabled: req.Enabled, Tags: req.Tags}
	found := false
	for i, e := range entries {
		if e.Filename == filename {
			entries[i] = updated
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, updated)
	}
	if err := saveManifest(dir, entries); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type skillTestRequest struct {
	Question string `json:"question"`
}

type skillTestResponse struct {
	// Selected is the display names of every currently-saved, enabled
	// skill that would apply to Question — in the same order
	// buildSystemPromptForMode would concatenate them, most relevant
	// (highest tag-match count) first.
	Selected []string `json:"selected"`
}

// handlePromptsSkillTest lets an admin try a sample question against the
// skills actually saved on disk right now, from the Prompts tab itself —
// without this, the only way to see whether a skill's tags would fire was
// to go ask a real question in Chat/Agent with an admin session and
// inspect Debug-Modus (and even that showed nothing before SelectedSkills
// was added to debugTrace, see llm.go). Calls the exact same
// loadSkillsFromDir + selectSkills (skills.go) buildSystemPromptForMode
// uses, so the result can never drift from real runtime behavior — this
// is not a reimplementation of the matching logic, just a read-only,
// no-LLM-call preview of it.
func handlePromptsSkillTest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req skillTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	dir := promptsDirOrDefault(settings.get().PromptsDir)
	entries, err := loadSkillsFromDir(dir)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	selected := selectSkills(req.Question, entries)
	names := make([]string, len(selected))
	for i, s := range selected {
		names[i] = s.DisplayName
	}
	writeJSON(w, skillTestResponse{Selected: names})
}

type deleteSkillRequest struct {
	Filename string `json:"filename"`
}

// handlePromptsDeleteSkill removes a skill's manifest entry and best-effort
// deletes its file; the manifest write is what actually takes it out of
// rotation, so a failed/partial file removal afterward doesn't leave a
// still-active skill behind.
func handlePromptsDeleteSkill(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req deleteSkillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !isValidSkillFilename(req.Filename) {
		writeJSONError(w, "invalid or missing filename", 400)
		return
	}
	dir := promptsDirOrDefault(settings.get().PromptsDir)

	entries, err := loadManifest(dir)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.Filename != req.Filename {
			kept = append(kept, e)
		}
	}
	if err := saveManifest(dir, kept); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	_ = os.Remove(filepath.Join(dir, req.Filename)) // best-effort; manifest entry removal is what actually stops it being used
	writeJSON(w, map[string]bool{"ok": true})
}
