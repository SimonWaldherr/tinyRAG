package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRewriteQueryForRetrievalDisabledSkipsLLM confirms the disabled
// default never makes an LLM call at all — the fake server fails the test
// if it's ever hit.
func TestRewriteQueryForRetrievalDisabledSkipsLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no LLM call should happen when query rewrite is disabled")
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	history := []askHistoryTurn{{Role: "user", Content: "Wie ist die Lieferantenrichtlinie bei Kistenpfennig?"}}
	got := rewriteQueryForRetrieval(context.Background(), lm, "und bei Schäfer Technik?", history, queryRewriteConfig{Enabled: false}, askHistoryMaxDefault)
	if got != "und bei Schäfer Technik?" {
		t.Fatalf("want the original question unchanged when disabled, got %q", got)
	}
}

// TestRewriteQueryForRetrievalNoHistorySkipsLLM confirms a first question
// in a fresh conversation (no history to rewrite against) never pays for
// the extra LLM round-trip.
func TestRewriteQueryForRetrievalNoHistorySkipsLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no LLM call should happen with no history")
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	got := rewriteQueryForRetrieval(context.Background(), lm, "Wie ist die Lieferantenrichtlinie?", nil, queryRewriteConfig{Enabled: true}, askHistoryMaxDefault)
	if got != "Wie ist die Lieferantenrichtlinie?" {
		t.Fatalf("want the original question unchanged with no history, got %q", got)
	}
}

// TestRewriteQueryForRetrievalLLMErrorFailsOpen confirms a broken rewrite
// call never propagates an error or panics — it must return the original
// question and let the caller's retrieval proceed unaffected.
func TestRewriteQueryForRetrievalLLMErrorFailsOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	history := []askHistoryTurn{{Role: "user", Content: "Wie ist die Lieferantenrichtlinie bei Kistenpfennig?"}}
	got := rewriteQueryForRetrieval(context.Background(), lm, "und bei Schäfer Technik?", history, queryRewriteConfig{Enabled: true}, askHistoryMaxDefault)
	if got != "und bei Schäfer Technik?" {
		t.Fatalf("want the original question on LLM failure (fail-open), got %q", got)
	}
}

// TestRewriteQueryForRetrievalHappyPath confirms the rewritten query
// (and only it — not the model's own preamble) is returned, and that the
// history is actually forwarded to the LLM call as prior turns.
func TestRewriteQueryForRetrievalHappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.Messages) != 3 {
			t.Fatalf("want system + 1 history turn + question = 3 messages, got %d: %+v", len(req.Messages), req.Messages)
		}
		if req.Messages[1].Content != "Wie ist die Lieferantenrichtlinie bei Kistenpfennig?" {
			t.Errorf("want the history turn forwarded, got %q", req.Messages[1].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Lieferantenrichtlinie Schäfer Technik"}}]}`))
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	history := []askHistoryTurn{{Role: "user", Content: "Wie ist die Lieferantenrichtlinie bei Kistenpfennig?"}}
	got := rewriteQueryForRetrieval(context.Background(), lm, "und bei Schäfer Technik?", history, queryRewriteConfig{Enabled: true}, askHistoryMaxDefault)
	if got != "Lieferantenrichtlinie Schäfer Technik" {
		t.Fatalf("want the rewritten query, got %q", got)
	}
}

// TestRewriteQueryForRetrievalOversizedResponseFallsBack confirms a
// suspiciously long "rewrite" (e.g. a confused model echoing back a whole
// paragraph) is rejected in favor of the original question, rather than
// handing rankedSearch something that searches for everything and nothing
// at once.
func TestRewriteQueryForRetrievalOversizedResponseFallsBack(t *testing.T) {
	huge := make([]byte, queryRewriteMaxChars+1)
	for i := range huge {
		huge[i] = 'x'
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": string(huge)}}},
		})
		_, _ = w.Write(body)
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	history := []askHistoryTurn{{Role: "user", Content: "vorherige Frage"}}
	got := rewriteQueryForRetrieval(context.Background(), lm, "und bei Schäfer Technik?", history, queryRewriteConfig{Enabled: true}, askHistoryMaxDefault)
	if got != "und bei Schäfer Technik?" {
		t.Fatalf("want the original question when the rewrite is oversized, got %q", got)
	}
}

// TestResolveQueryRewriteProfile checks the override/fallback logic: an
// explicit Profile wins, otherwise the deployment's own default chat
// profile is used (there is no "main call's own profile" yet at the point
// this runs — see queryRewriteConfig.Profile's doc comment).
func TestResolveQueryRewriteProfile(t *testing.T) {
	if got := resolveQueryRewriteProfile(queryRewriteConfig{}, "local"); got != "local" {
		t.Fatalf("want fallback to defaultChatProfile %q, got %q", "local", got)
	}
	if got := resolveQueryRewriteProfile(queryRewriteConfig{Profile: "azure"}, "local"); got != "azure" {
		t.Fatalf("want the explicit override %q, got %q", "azure", got)
	}
}

// TestValidateQueryRewriteSettings mirrors TestValidateToolRouterSettings
// exactly — same enum, same reasoning.
func TestValidateQueryRewriteSettings(t *testing.T) {
	cases := []struct {
		name    string
		c       queryRewriteConfig
		wantErr bool
	}{
		{"empty profile valid (means: deployment default)", queryRewriteConfig{}, false},
		{"local valid", queryRewriteConfig{Enabled: true, Profile: "local"}, false},
		{"azure valid", queryRewriteConfig{Enabled: true, Profile: "azure"}, false},
		{"case-insensitive", queryRewriteConfig{Profile: "Azure"}, false},
		{"unknown profile", queryRewriteConfig{Profile: "openai"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateQueryRewriteSettings(c.c)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateQueryRewriteSettings(%+v): wantErr=%v, got %v", c.c, c.wantErr, err)
			}
		})
	}
}

// TestHandleSettingsPersistsQueryRewrite confirms QueryRewrite round-trips
// through POST /api/settings, including the lower-casing normalization
// applied on save — same pattern as TestHandleSettingsPersistsToolRouter.
func TestHandleSettingsPersistsQueryRewrite(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"query_rewrite": map[string]any{"enabled": true, "profile": "Azure"},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := settings.get().QueryRewrite
	want := queryRewriteConfig{Enabled: true, Profile: "azure"}
	if got != want {
		t.Fatalf("QueryRewrite after save: want %+v, got %+v", want, got)
	}
}

// TestHandleSettingsRejectsInvalidQueryRewriteSettings guards the 400
// path: an unknown Profile must be rejected, and the previously saved
// QueryRewrite must survive unchanged (the whole save is rejected, not
// partially applied).
func TestHandleSettingsRejectsInvalidQueryRewriteSettings(t *testing.T) {
	rag, s := newTestRAG(t)
	s.QueryRewrite = queryRewriteConfig{Enabled: true, Profile: "local"}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"query_rewrite": map[string]any{"profile": "openai"},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown query_rewrite profile, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := settings.get().QueryRewrite; got != s.QueryRewrite {
		t.Fatalf("a rejected save must not change QueryRewrite: want %+v, got %+v", s.QueryRewrite, got)
	}
}
