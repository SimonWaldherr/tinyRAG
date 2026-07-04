package main

import "testing"

func TestBuildEngineQuery(t *testing.T) {
	if got := buildEngineQuery("", "ecosia"); got != "" {
		t.Errorf("empty query should pass through, got %q", got)
	}
	if got := buildEngineQuery(`news "bitcoin price"`, "brave"); got != "bitcoin price" {
		t.Errorf("brave should strip news-prefix wrapper, got %q", got)
	}
	if got := buildEngineQuery("was ist die Hauptstadt von Frankreich", "ecosia"); got == "was ist die Hauptstadt von Frankreich" {
		t.Error("ecosia should strip question words")
	}
	if got := buildEngineQuery("golang timeout", "unknown-engine"); got != "golang timeout" {
		t.Errorf("unknown engine should return the trimmed query unchanged, got %q", got)
	}
}

func TestHasAnyWord(t *testing.T) {
	if !hasAnyWord("Das ist ein Test.", []string{"ist"}) {
		t.Error("expected to find 'ist' as a whole word")
	}
	if hasAnyWord("Testistwort", []string{"ist"}) {
		t.Error("'ist' must not match inside a larger word")
	}
	if !hasAnyWord("Word, with-punctuation!", []string{"word"}) {
		t.Error("trailing punctuation should be stripped before comparison")
	}
	if hasAnyWord("", []string{"x"}) {
		t.Error("empty string should never match")
	}
}

func TestStripQuestionWords(t *testing.T) {
	cases := map[string]string{
		"was ist die Hauptstadt von Frankreich": "die Hauptstadt von Frankreich",
		"what is the capital of France":         "the capital of France",
		"golang timeout handling":               "golang timeout handling",
	}
	for in, want := range cases {
		if got := stripQuestionWords(in); got != want {
			t.Errorf("stripQuestionWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClampUnitInterval(t *testing.T) {
	cases := map[float64]float64{-1: 0, 0: 0, 0.5: 0.5, 1: 1, 2: 1}
	for in, want := range cases {
		if got := clampUnitInterval(in); got != want {
			t.Errorf("clampUnitInterval(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestCandidateLimitForK(t *testing.T) {
	if got := candidateLimitForK(1); got != 100 {
		t.Errorf("small k should use the 100-row floor, got %d", got)
	}
	if got := candidateLimitForK(100); got != 300 {
		t.Errorf("candidateLimitForK(100) = %d, want 300", got)
	}
	if got := candidateLimitForK(10000); got != 1000 {
		t.Errorf("candidateLimitForK should cap at 1000, got %d", got)
	}
}

func TestFormatContextChunk(t *testing.T) {
	got := formatContextChunk("Golang", 3, "some content")
	if got != "[Quelle: Golang | Chunk: 3]\nsome content" {
		t.Errorf("unexpected format: %q", got)
	}
	got = formatContextChunk("  ", 0, "x")
	if got != "[Quelle: unknown | Chunk: 0]\nx" {
		t.Errorf("blank article should fall back to 'unknown', got %q", got)
	}
}

func TestFormatContextChunkWithCitation(t *testing.T) {
	cit := Citation{Title: "My Doc", SourceSystem: "wiki", SourceType: "article", UpdatedAt: "2026-01-01", TrustLevel: 0.8, R3Score: 0.91}
	got := formatContextChunkWithCitation("article", 1, "body text", cit)
	for _, want := range []string{"My Doc", "wiki", "article", "2026-01-01", "0.80", "0.910", "body text"} {
		if !contains(got, want) {
			t.Errorf("expected citation format to contain %q, got:\n%s", want, got)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (needle == "" || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestExtractAndStripToolRequest(t *testing.T) {
	text := `Vorher [TOOL_REQUEST]{"tool":"wikipedia","query":"Go (Sprache)"}[/TOOL_REQUEST] Nachher`
	tr, ok := extractToolRequest(text)
	if !ok || tr.Tool != "wikipedia" || tr.Query != "Go (Sprache)" {
		t.Fatalf("unexpected extraction: %+v ok=%v", tr, ok)
	}
	stripped := stripToolRequest(text)
	if stripped != "Vorher  Nachher" && stripped != "Vorher Nachher" {
		t.Errorf("unexpected stripped text: %q", stripped)
	}

	if _, ok := extractToolRequest("no tool request here"); ok {
		t.Error("text without a tool request block should not extract")
	}
	if _, ok := extractToolRequest(`[TOOL_REQUEST]{"tool":"","query":"x"}[/TOOL_REQUEST]`); ok {
		t.Error("empty tool name should be rejected")
	}
	if _, ok := extractToolRequest(`[TOOL_REQUEST]not json[/TOOL_REQUEST]`); ok {
		t.Error("invalid JSON body should be rejected")
	}
}

func TestFilterToolsForRole(t *testing.T) {
	tools := []toolDef{{Name: "shell"}, {Name: "wikipedia"}, {Name: "local_search"}}
	out := filterToolsForRole(tools, "vertrieb")
	names := map[string]bool{}
	for _, tl := range out {
		names[tl.Name] = true
	}
	if names["shell"] {
		t.Error("vertrieb role must not see the shell tool")
	}
	if !names["wikipedia"] || !names["local_search"] {
		t.Error("vertrieb role should see web-fetch and local_search tools")
	}
}

func TestShouldAutoExecuteTool(t *testing.T) {
	s := appSettings{ActiveRole: "it", AllowShellExec: false}
	if shouldAutoExecuteTool(s, toolRequest{Tool: "shell"}, true) {
		t.Error("shell must require AllowShellExec even when auto-search is on")
	}
	s.AllowShellExec = true
	if !shouldAutoExecuteTool(s, toolRequest{Tool: "shell"}, true) {
		t.Error("shell should auto-execute once AllowShellExec is set")
	}
	if !shouldAutoExecuteTool(s, toolRequest{Tool: "calculate"}, false) {
		t.Error("calculate should always auto-execute regardless of auto-search")
	}
	if shouldAutoExecuteTool(s, toolRequest{Tool: "wikipedia"}, false) {
		t.Error("wikipedia should require auto-search")
	}
	commercial := appSettings{ActiveRole: "it", UsageProfile: "commercial", AllowShellExec: true}
	if shouldAutoExecuteTool(commercial, toolRequest{Tool: "shell"}, true) {
		t.Error("commercial usage profile must disable shell tool regardless of AllowShellExec")
	}
}
