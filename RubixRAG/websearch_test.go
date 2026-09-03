package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withTestTavilyServer points tavilyBaseURL at a fake server for the
// duration of one test and restores the original afterward.
func withTestTavilyServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	orig := tavilyBaseURL
	tavilyBaseURL = server.URL
	t.Cleanup(func() { tavilyBaseURL = orig })
}

// TestTavilySearchHappyPath confirms the request shape (POST, Bearer auth,
// JSON body with query/max_results) and that results decode correctly.
func TestTavilySearchHappyPath(t *testing.T) {
	var gotAuth, gotMethod string
	var gotBody map[string]any
	withTestTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Result A","url":"https://a.example/","content":"Snippet A"}]}`))
	})

	res, err := tavilySearch(context.Background(), "tvly-test-key", "widget pricing", 5)
	if err != nil {
		t.Fatalf("tavilySearch: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("want POST, got %s", gotMethod)
	}
	if gotAuth != "Bearer tvly-test-key" {
		t.Fatalf("want Bearer auth header, got %q", gotAuth)
	}
	if gotBody["query"] != "widget pricing" {
		t.Fatalf("want query in request body, got %+v", gotBody)
	}
	if gotBody["max_results"] != float64(5) {
		t.Fatalf("want max_results=5 in request body, got %+v", gotBody)
	}
	if len(res.Results) != 1 || res.Results[0].Title != "Result A" || res.Results[0].URL != "https://a.example/" {
		t.Fatalf("want the decoded result, got %+v", res.Results)
	}
}

// TestTavilySearchNonOKStatus confirms an error status becomes a Go error
// carrying the response body, not a silently-empty result.
func TestTavilySearchNonOKStatus(t *testing.T) {
	withTestTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid api key"}`))
	})

	_, err := tavilySearch(context.Background(), "bad-key", "q", 5)
	if err == nil {
		t.Fatal("want an error for a 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("want the status code in the error, got %v", err)
	}
}

// TestAgentWebSearchMaxResultsClamp / TestAgentWebSearchTimeoutClamp cover
// the 0=default, >ceiling=ceiling clamping (clampInt, agent.go).
func TestAgentWebSearchMaxResultsClamp(t *testing.T) {
	if got := agentWebSearchMaxResults(agentConfig{}); got != webSearchDefaultMaxResults {
		t.Fatalf("want default %d for unset, got %d", webSearchDefaultMaxResults, got)
	}
	if got := agentWebSearchMaxResults(agentConfig{WebSearchMaxResults: 3}); got != 3 {
		t.Fatalf("want 3, got %d", got)
	}
	if got := agentWebSearchMaxResults(agentConfig{WebSearchMaxResults: 999}); got != webSearchMaxResultsCeiling {
		t.Fatalf("want clamped to ceiling %d, got %d", webSearchMaxResultsCeiling, got)
	}
}

func TestAgentWebSearchTimeoutClamp(t *testing.T) {
	if got := agentWebSearchTimeout(agentConfig{}); got.Seconds() != webSearchDefaultTimeoutSeconds {
		t.Fatalf("want default %ds for unset, got %v", webSearchDefaultTimeoutSeconds, got)
	}
	if got := agentWebSearchTimeout(agentConfig{WebSearchTimeoutSeconds: 999}); got.Seconds() != webSearchTimeoutCeiling {
		t.Fatalf("want clamped to ceiling %ds, got %v", webSearchTimeoutCeiling, got)
	}
}

// TestResolveWebSearchAPIKeyPrefersEnv confirms WebSearchAPIKeyEnv takes
// precedence over the inline WebSearchAPIKey, same convention as every
// other credential pair (resolveSecret).
func TestResolveWebSearchAPIKeyPrefersEnv(t *testing.T) {
	t.Setenv("R3_TEST_TAVILY_KEY", "from-env")
	got := resolveWebSearchAPIKey(agentConfig{WebSearchAPIKey: "inline", WebSearchAPIKeyEnv: "R3_TEST_TAVILY_KEY"})
	if got != "from-env" {
		t.Fatalf("want the env-resolved key to win, got %q", got)
	}
	got = resolveWebSearchAPIKey(agentConfig{WebSearchAPIKey: "inline"})
	if got != "inline" {
		t.Fatalf("want the inline key when no env var is set, got %q", got)
	}
}

// TestWebSearchToolExecutorRejectsEmptyQuery / MissingAPIKey confirm the
// executor's own input guards, before any HTTP call is attempted.
func TestWebSearchToolExecutorRejectsEmptyQuery(t *testing.T) {
	exec := webSearchToolExecutor(appSettings{Agent: agentConfig{WebSearchAPIKey: "key"}})
	if _, err := exec(context.Background(), `{"query":"  "}`); err == nil {
		t.Fatal("want an error for an empty/whitespace query")
	}
}

func TestWebSearchToolExecutorRequiresAPIKey(t *testing.T) {
	exec := webSearchToolExecutor(appSettings{Agent: agentConfig{}})
	_, err := exec(context.Background(), `{"query":"widgets"}`)
	if err == nil {
		t.Fatal("want an error when no API key is configured")
	}
	if !strings.Contains(err.Error(), "API-Key") {
		t.Fatalf("want a clear 'no API key configured' message, got %v", err)
	}
}

// TestWebSearchToolExecutorFormatsResults is the end-to-end happy path:
// the executor calls Tavily and formats results as a numbered
// title/URL/snippet list, not raw JSON.
func TestWebSearchToolExecutorFormatsResults(t *testing.T) {
	withTestTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"First Result","url":"https://a.example/","content":"First snippet"},
			{"title":"Second Result","url":"https://b.example/","content":"Second snippet"}
		]}`))
	})
	exec := webSearchToolExecutor(appSettings{Agent: agentConfig{WebSearchAPIKey: "key"}})
	out, err := exec(context.Background(), `{"query":"widgets"}`)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "1. First Result") || !strings.Contains(out, "https://a.example/") || !strings.Contains(out, "First snippet") {
		t.Fatalf("want the first result formatted, got %q", out)
	}
	if !strings.Contains(out, "2. Second Result") {
		t.Fatalf("want the second result formatted, got %q", out)
	}
}

// TestWebSearchToolExecutorNoResults confirms an empty result set returns
// a clear "no hits" message rather than an empty string or an error.
func TestWebSearchToolExecutorNoResults(t *testing.T) {
	withTestTavilyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	exec := webSearchToolExecutor(appSettings{Agent: agentConfig{WebSearchAPIKey: "key"}})
	out, err := exec(context.Background(), `{"query":"nonexistent widgets xyz"}`)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "Keine Treffer") {
		t.Fatalf("want a clear no-results message, got %q", out)
	}
}

// TestHandleSettingsPersistsWebSearch guards the failure mode this
// codebase has hit before (see AGENTS.md/memory): a new agentConfig field
// added but never wired into handleSettings' merge closure silently fails
// to persist. Confirms all five new AllowWebSearch/WebSearchAPIKey* /
// WebSearchMaxResults/WebSearchTimeoutSeconds fields round-trip through
// POST /api/settings, same pattern as TestHandleSettingsPersistsContextCompaction.
func TestHandleSettingsPersistsWebSearch(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"agent": map[string]any{
			"allow_web_search":           true,
			"web_search_api_key":         "tvly-real-key",
			"web_search_api_key_env":     "TAVILY_API_KEY",
			"web_search_max_results":     8,
			"web_search_timeout_seconds": 20,
		},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := settings.get().Agent
	if !got.AllowWebSearch {
		t.Fatalf("want AllowWebSearch=true after save, got false")
	}
	if got.WebSearchAPIKey != "tvly-real-key" {
		t.Fatalf("want WebSearchAPIKey persisted, got %q", got.WebSearchAPIKey)
	}
	if got.WebSearchAPIKeyEnv != "TAVILY_API_KEY" {
		t.Fatalf("want WebSearchAPIKeyEnv persisted, got %q", got.WebSearchAPIKeyEnv)
	}
	if got.WebSearchMaxResults != 8 {
		t.Fatalf("want WebSearchMaxResults=8, got %d", got.WebSearchMaxResults)
	}
	if got.WebSearchTimeoutSeconds != 20 {
		t.Fatalf("want WebSearchTimeoutSeconds=20, got %d", got.WebSearchTimeoutSeconds)
	}
}

// TestHandleSettingsRedactsWebSearchAPIKey confirms GET /api/settings never
// round-trips the real Tavily key to the browser (maskedSettings), and
// that re-POSTing the redaction placeholder back (as an unedited form
// naturally would) does NOT overwrite the real stored key with the
// placeholder text — same guard as Shop.Password/ClientSecret.
func TestHandleSettingsRedactsWebSearchAPIKey(t *testing.T) {
	rag, s := newTestRAG(t)
	s.Agent.WebSearchAPIKey = "tvly-super-secret"
	withTestGlobalSettings(t, s)

	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	agentOut, _ := got["agent"].(map[string]any)
	if agentOut["web_search_api_key"] == "tvly-super-secret" {
		t.Fatalf("want the real API key never returned to the browser, got %+v", agentOut["web_search_api_key"])
	}

	// Re-POST exactly what the browser would have (the masked placeholder,
	// unedited) plus an unrelated field change — the real key must survive.
	body, _ := json.Marshal(map[string]any{
		"agent": map[string]any{
			"allow_web_search":   true,
			"web_search_api_key": agentOut["web_search_api_key"],
		},
	})
	rec = httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if settings.get().Agent.WebSearchAPIKey != "tvly-super-secret" {
		t.Fatalf("want the real key preserved when the placeholder is re-submitted, got %q", settings.get().Agent.WebSearchAPIKey)
	}
}
