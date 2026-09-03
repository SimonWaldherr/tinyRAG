package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseRawEmailKeepsWholeTextAsBody covers handleDraftReply's "Mail"
// tab branch (draftReplyRequest.RawEmail): unlike parseStoredEmail (which
// reverses R3's own generated header format), a pasted customer email has
// no guaranteed structure, so parseRawEmail must not attempt any header
// parsing — the entire (trimmed) pasted text becomes Body, Subject/From
// stay empty.
func TestParseRawEmailKeepsWholeTextAsBody(t *testing.T) {
	raw := "  \nHallo,\n\nkoennen Sie mir sagen, wann meine Bestellung ankommt?\n\nDanke, Max\n  "
	got := parseRawEmail(raw)
	want := "Hallo,\n\nkoennen Sie mir sagen, wann meine Bestellung ankommt?\n\nDanke, Max"
	if got.Body != want {
		t.Errorf("want trimmed body %q, got %q", want, got.Body)
	}
	if got.Subject != "" || got.From != "" {
		t.Errorf("want empty Subject/From for raw pasted text, got Subject=%q From=%q", got.Subject, got.From)
	}
}

// TestParseRawEmailDoesNotMistakeHeaderLikeContentForRealHeaders guards
// against a future "helpful" header-sniffing addition breaking on a
// customer email that happens to start with a line resembling one of
// R3's own header prefixes (e.g. quoting "Subject: ..." from the original
// thread) — the whole text must still land in Body untouched.
func TestParseRawEmailDoesNotMistakeHeaderLikeContentForRealHeaders(t *testing.T) {
	raw := "Subject: RE: Bestellung 12345\nVon: kunde@example.com\n\nBitte um Rueckmeldung."
	got := parseRawEmail(raw)
	if got.Body != raw {
		t.Errorf("want the raw text unchanged as Body, got %q", got.Body)
	}
	if got.Subject != "" || got.From != "" {
		t.Errorf("want Subject/From left empty (no header parsing), got Subject=%q From=%q", got.Subject, got.From)
	}
}

// TestBaselineRankingConfigDefaultUnchanged covers
// rankingConfig.AgentModeMinFinalScore's "0 = disabled" default: with it
// left at 0 (the zero value, so upgrading an existing deployment changes
// nothing), Chat mode and Agent mode must get byte-identical rankingConfig
// values back from baselineRankingConfig for the same input cfg — Agent
// mode's baseline retrieval must behave exactly like Chat's.
func TestBaselineRankingConfigDefaultUnchanged(t *testing.T) {
	cfg := rankingConfig{MinFinalScore: 0.45, RecencyWeight: 0.1, AgentModeMinFinalScore: 0}
	chatCfg := baselineRankingConfig("", cfg)
	agentCfg := baselineRankingConfig("agent", cfg)
	if chatCfg != agentCfg {
		t.Fatalf("AgentModeMinFinalScore=0: want identical baseline configs for chat and agent mode, got chat=%+v agent=%+v", chatCfg, agentCfg)
	}
	if agentCfg.MinFinalScore != 0.45 {
		t.Fatalf("want MinFinalScore left at the configured 0.45, got %v", agentCfg.MinFinalScore)
	}
}

// TestBaselineRankingConfigStricterForAgentModeOnly covers the opt-in case:
// with AgentModeMinFinalScore set above MinFinalScore, only Agent mode's
// resolved config uses the stricter threshold; Chat mode's is untouched.
// A hit scoring between the two thresholds then survives
// filterByMinFinalScore under Chat's config but is dropped under Agent's.
func TestBaselineRankingConfigStricterForAgentModeOnly(t *testing.T) {
	cfg := rankingConfig{MinFinalScore: 0.40, AgentModeMinFinalScore: 0.60, RecencyWeight: 0.1}

	chatCfg := baselineRankingConfig("", cfg)
	if chatCfg.MinFinalScore != 0.40 {
		t.Fatalf("chat mode: want MinFinalScore left at the configured 0.40, got %v", chatCfg.MinFinalScore)
	}
	agentCfg := baselineRankingConfig("agent", cfg)
	if agentCfg.MinFinalScore != 0.60 {
		t.Fatalf("agent mode: want MinFinalScore overridden to AgentModeMinFinalScore 0.60, got %v", agentCfg.MinFinalScore)
	}

	// A hit scoring between the two thresholds (0.40 <= score < 0.60):
	// kept for chat, dropped for agent.
	hits := []rankedHit{{SourceID: "mid", KeywordScore: 0.3, FinalScore: 0.5}}
	chatHits := filterByMinFinalScore(hits, chatCfg.MinFinalScore, chatCfg.RecencyWeight)
	if len(chatHits) != 1 {
		t.Fatalf("chat mode: want the mid-scoring hit kept, got %+v", chatHits)
	}
	agentHits := filterByMinFinalScore(hits, agentCfg.MinFinalScore, agentCfg.RecencyWeight)
	if len(agentHits) != 0 {
		t.Fatalf("agent mode: want the mid-scoring hit dropped under the stricter threshold, got %+v", agentHits)
	}
}

// TestHandleAskRejectsOverLongQuestion proves handleAsk enforces
// uploadConfig.MaxPromptChars (chatimages.go's effectiveMaxPromptChars)
// before doing any retrieval/LLM work — the request 400s outright rather
// than silently truncating the question.
func TestHandleAskRejectsOverLongQuestion(t *testing.T) {
	rag, s := newTestRAG(t)
	// promptMaxCharsMin (2000) is the smallest value effectiveMaxPromptChars
	// won't clamp upward — set right at that floor so the configured limit
	// is exactly what's enforced, not silently widened by the clamp.
	s.Upload.MaxPromptChars = promptMaxCharsMin
	withTestGlobalSettings(t, s)
	handler := handleAsk(rag)

	body, _ := json.Marshal(map[string]any{"question": strings.Repeat("a", promptMaxCharsMin+1)})
	r := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a question over MaxPromptChars, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandleAskAllowsQuestionWithinConfiguredLimit proves a question at or
// under the configured MaxPromptChars is unaffected — same request shape as
// the rejection test above, just under the limit instead of over it.
func TestHandleAskAllowsQuestionWithinConfiguredLimit(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Antwort."}}]}`))
	}))
	defer llm.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", llm.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.Profiles.Local.EmbedModel = "test-embed"
	s.Upload.MaxPromptChars = promptMaxCharsMin
	withTestGlobalSettings(t, s)
	handler := handleAsk(rag)

	body, _ := json.Marshal(map[string]any{"question": strings.Repeat("a", promptMaxCharsMin), "format": "json"})
	r := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for a question at exactly MaxPromptChars, got %d: %s", w.Code, w.Body.String())
	}
}
