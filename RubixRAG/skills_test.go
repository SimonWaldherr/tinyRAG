package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadIndexPromptFallsBackWhenMissing covers the "no index.md yet"
// case shared by readIndexPrompt/readDraftPrompt/readAgentPrompt: a fresh
// PromptsDir (or a dir that doesn't exist at all) must never error, just
// fall back to the built-in default.
func TestReadIndexPromptFallsBackWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if got := readIndexPrompt(dir); got != defaultSystemPrompt {
		t.Errorf("want defaultSystemPrompt fallback, got %q", got)
	}
	if got := readIndexPrompt(filepath.Join(dir, "does-not-exist")); got != defaultSystemPrompt {
		t.Errorf("want defaultSystemPrompt fallback for a nonexistent dir, got %q", got)
	}
}

// TestReadIndexPromptUsesFileContent covers the populated-file case.
func TestReadIndexPromptUsesFileContent(t *testing.T) {
	dir := t.TempDir()
	custom := "Du bist ein Test-Assistent."
	if err := os.WriteFile(filepath.Join(dir, promptsIndexFile), []byte(custom), 0o644); err != nil {
		t.Fatalf("write index.md: %v", err)
	}
	if got := readIndexPrompt(dir); got != custom {
		t.Errorf("want custom index.md content, got %q", got)
	}
}

// TestReadIndexPromptFallsBackOnWhitespaceOnlyFile guards against an
// admin accidentally saving an empty/whitespace-only prompt and silently
// losing the system prompt entirely.
func TestReadIndexPromptFallsBackOnWhitespaceOnlyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, promptsIndexFile), []byte("   \n\t\n"), 0o644); err != nil {
		t.Fatalf("write index.md: %v", err)
	}
	if got := readIndexPrompt(dir); got != defaultSystemPrompt {
		t.Errorf("want defaultSystemPrompt fallback for a whitespace-only file, got %q", got)
	}
}

// TestReadDraftPromptFallsBackWhenMissing/TestReadDraftPromptUsesFileContent
// mirror the index.md cases above for draft.md — same read-with-fallback
// shape (skills.go), just a different file/fallback constant.
func TestReadDraftPromptFallsBackWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if got := readDraftPrompt(dir); got != defaultDraftSystemPrompt {
		t.Errorf("want defaultDraftSystemPrompt fallback, got %q", got)
	}
}

func TestReadDraftPromptUsesFileContent(t *testing.T) {
	dir := t.TempDir()
	custom := "Formuliere Antwortentwuerfe kurz und foermlich."
	if err := os.WriteFile(filepath.Join(dir, draftPromptFile), []byte(custom), 0o644); err != nil {
		t.Fatalf("write draft.md: %v", err)
	}
	if got := readDraftPrompt(dir); got != custom {
		t.Errorf("want custom draft.md content, got %q", got)
	}
}

// TestReadAgentPromptFallsBackWhenMissing/TestReadAgentPromptUsesFileContent
// mirror the same cases for agent.md.
func TestReadAgentPromptFallsBackWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if got := readAgentPrompt(dir); got != defaultAgentSystemPrompt {
		t.Errorf("want defaultAgentSystemPrompt fallback, got %q", got)
	}
}

func TestReadAgentPromptUsesFileContent(t *testing.T) {
	dir := t.TempDir()
	custom := "Du bist ein Aufgaben-Agent fuer Tests."
	if err := os.WriteFile(filepath.Join(dir, agentPromptFile), []byte(custom), 0o644); err != nil {
		t.Fatalf("write agent.md: %v", err)
	}
	if got := readAgentPrompt(dir); got != custom {
		t.Errorf("want custom agent.md content, got %q", got)
	}
}

// TestBuildSystemPromptForModeSelectsBaseFileByMode is the key regression
// guard for the Agent tab: mode="" must keep reading index.md as its base,
// while mode="agent" must read agent.md instead. Both modes then get the
// shared outputFormattingGuidance appended (so the model knows which rich
// formats the UI renders) — asserted here so a future change can't silently
// drop it, and can't leak the wrong mode's base file.
func TestBuildSystemPromptForModeSelectsBaseFileByMode(t *testing.T) {
	dir := t.TempDir()
	indexContent := "INDEX-PROMPT-MARKER"
	agentContent := "AGENT-PROMPT-MARKER"
	if err := os.WriteFile(filepath.Join(dir, promptsIndexFile), []byte(indexContent), 0o644); err != nil {
		t.Fatalf("write index.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentPromptFile), []byte(agentContent), 0o644); err != nil {
		t.Fatalf("write agent.md: %v", err)
	}

	got, _ := buildSystemPromptForMode(dir, "irrelevant question", "")
	if !strings.HasPrefix(got, indexContent) {
		t.Errorf("mode=\"\": want prompt to start with index.md content %q, got %q", indexContent, got)
	}
	if strings.Contains(got, agentContent) {
		t.Errorf("mode=\"\": prompt must not contain agent.md content, got %q", got)
	}
	if !strings.Contains(got, outputFormattingGuidance) {
		t.Errorf("mode=\"\": prompt must include the output-formatting guidance")
	}

	got, _ = buildSystemPromptForMode(dir, "irrelevant question", "agent")
	if !strings.HasPrefix(got, agentContent) {
		t.Errorf("mode=\"agent\": want prompt to start with agent.md content %q, got %q", agentContent, got)
	}
	if strings.Contains(got, indexContent) {
		t.Errorf("mode=\"agent\": prompt must not contain index.md content, got %q", got)
	}
	if !strings.Contains(got, outputFormattingGuidance) {
		t.Errorf("mode=\"agent\": prompt must include the output-formatting guidance")
	}

	// buildSystemPrompt (no mode param) must still behave exactly like
	// mode="" — existing Chat callers shouldn't need to change.
	got, _ = buildSystemPrompt(dir, "irrelevant question")
	if !strings.HasPrefix(got, indexContent) {
		t.Errorf("buildSystemPrompt: want prompt to start with index.md content %q, got %q", indexContent, got)
	}
}

// TestSelectSkillsMatchesExactTag confirms the baseline case still works:
// a tag appearing verbatim (as a whole token) in the question selects
// that skill.
func TestSelectSkillsMatchesExactTag(t *testing.T) {
	entries := []skillEntry{
		{Filename: "skill_a.md", DisplayName: "A", Enabled: true, Tags: []string{"mechatronik", "sensor"}},
		{Filename: "skill_b.md", DisplayName: "B", Enabled: true, Tags: []string{"einkauf"}},
	}
	got := selectSkills("Frage zur Mechatronik-Wartung", entries)
	if len(got) != 1 || got[0].Filename != "skill_a.md" {
		t.Fatalf("want only skill_a selected, got %+v", got)
	}
}

// TestSelectSkillsMatchesPluralOfSingularTag is the regression test for
// the shipped skill_ppe.md bug the audit flagged: its tags are singular
// stems ("handschuh", "gehoerschutz", …) but a real question naturally
// uses the German plural ("Handschuhe", "Gehörschutz" already singular in
// this case, so use "Handschuhe" and "Schutzbrillen"). Before
// tagTermMatches' prefix fallback, exact-token matching would have missed
// this entirely — this must now match.
func TestSelectSkillsMatchesPluralOfSingularTag(t *testing.T) {
	entries := []skillEntry{
		{Filename: "skill_ppe.md", DisplayName: "PSA", Enabled: true, Tags: []string{"handschuh", "schutzbrille"}},
	}
	got := selectSkills("Welche Handschuhe und Schutzbrillen brauche ich beim Schweißen?", entries)
	if len(got) != 1 || got[0].Filename != "skill_ppe.md" {
		t.Fatalf("want skill_ppe selected via plural-matching fallback, got %+v", got)
	}
}

// TestSelectSkillsShortTagRequiresExactMatch confirms skillTagStemMinLen
// actually gates the fuzzy fallback — a short tag ("qm", below the
// threshold) must NOT prefix-match an unrelated longer word that happens
// to start with the same two letters, only an exact token.
func TestSelectSkillsShortTagRequiresExactMatch(t *testing.T) {
	entries := []skillEntry{
		{Filename: "skill_q.md", DisplayName: "QM", Enabled: true, Tags: []string{"qm"}},
	}
	// "qualitaetsmanagement" starts with "qm"? No — starts with "qu", so
	// this specific word was never a risk; use a word that actually shares
	// the "qm" prefix to prove short tags don't fuzzy-match at all.
	got := selectSkills("qmanagement-frage ohne den exakten begriff", entries)
	if len(got) != 0 {
		t.Fatalf("want no match for a short tag against a merely-prefixed word, got %+v", got)
	}
	got = selectSkills("Frage zum qm-Handbuch", entries)
	if len(got) != 1 {
		t.Fatalf("want the exact token \"qm\" to still match, got %+v", got)
	}
}

// TestSelectSkillsDisabledEntryNeverSelected confirms a disabled skill is
// excluded even when its tags match perfectly — the one authorization gate
// this function must never bypass.
func TestSelectSkillsDisabledEntryNeverSelected(t *testing.T) {
	entries := []skillEntry{
		{Filename: "skill_a.md", DisplayName: "A", Enabled: false, Tags: []string{"mechatronik"}},
	}
	got := selectSkills("Frage zur Mechatronik", entries)
	if len(got) != 0 {
		t.Fatalf("want a disabled skill never selected, got %+v", got)
	}
}
