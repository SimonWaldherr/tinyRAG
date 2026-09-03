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

// azureResponsesTestClient builds an lmClient pointed at a fake Azure
// Responses API server — mirrors newLMClientFull, just always "azure".
func azureResponsesTestClient(serverURL string) *lmClient {
	return newLMClientFull("azure", serverURL, "", "embed-deployment", "gpt-deployment", "test-key")
}

// TestLMClientAzureResponsesURL confirms the Responses API URL has no
// /openai/deployments/{name}/ segment and no api-version query parameter
// — the key structural difference from chatCompletionsURL/azureDeploymentURL.
func TestLMClientAzureResponsesURL(t *testing.T) {
	c := newLMClientFull("azure", "https://myresource.openai.azure.com", "2024-10-21", "e", "gpt-deployment", "k")
	got := c.azureResponsesURL()
	want := "https://myresource.openai.azure.com/openai/v1/responses"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// TestLMClientAzureBingSearchRejectsNonAzure confirms azureBingSearch
// refuses to run against a non-Azure client — this tool only exists on
// Azure's Responses API.
func TestLMClientAzureBingSearchRejectsNonAzure(t *testing.T) {
	c := newLMClientFull("local", "http://localhost:1234", "", "e", "m", "")
	_, _, err := c.azureBingSearch(context.Background(), "q")
	if err == nil {
		t.Fatal("want an error for a non-Azure client")
	}
}

// TestLMClientAzureBingSearchHappyPath confirms the request shape (POST,
// api-key header, model=chatModel, tools=[{"type":"web_search"}]) and that
// the message text + de-duplicated url_citation annotations are extracted
// from the Responses API's "output" array shape.
func TestLMClientAzureBingSearchHappyPath(t *testing.T) {
	var gotAPIKey string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != azureResponsesAPIPath {
			t.Errorf("want path %q, got %q", azureResponsesAPIPath, r.URL.Path)
		}
		gotAPIKey = r.Header.Get("api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output": [
				{"type": "web_search_call", "status": "completed", "action": {"type": "search", "query": "renewable energy trends"}},
				{"type": "message", "role": "assistant", "content": [
					{"type": "output_text", "text": "Renewable energy grew significantly in 2026.", "annotations": [
						{"type": "url_citation", "url": "https://a.example/", "title": "Source A"},
						{"type": "url_citation", "url": "https://b.example/", "title": "Source B"},
						{"type": "url_citation", "url": "https://a.example/", "title": "Source A dup"}
					]}
				]}
			]
		}`))
	}))
	defer server.Close()

	c := azureResponsesTestClient(server.URL)
	text, citations, err := c.azureBingSearch(context.Background(), "renewable energy trends")
	if err != nil {
		t.Fatalf("azureBingSearch: %v", err)
	}
	if gotAPIKey != "test-key" {
		t.Fatalf("want api-key header set, got %q", gotAPIKey)
	}
	if gotBody["model"] != "gpt-deployment" {
		t.Fatalf("want model=gpt-deployment in request body, got %+v", gotBody)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want exactly one tool in request body, got %+v", gotBody["tools"])
	}
	toolMap, _ := tools[0].(map[string]any)
	if toolMap["type"] != "web_search" {
		t.Fatalf(`want tool type "web_search", got %+v`, toolMap)
	}
	if text != "Renewable energy grew significantly in 2026." {
		t.Fatalf("want the message text extracted, got %q", text)
	}
	if len(citations) != 2 {
		t.Fatalf("want 2 de-duplicated citations, got %+v", citations)
	}
	if citations[0].URL != "https://a.example/" || citations[1].URL != "https://b.example/" {
		t.Fatalf("want citations in first-seen order, got %+v", citations)
	}
}

// TestLMClientAzureBingSearchNonOKStatus confirms an error status
// surfaces as a Go error via llmPostJSONRetry's own error formatting.
func TestLMClientAzureBingSearchNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"web_search tool blocked for this subscription"}`))
	}))
	defer server.Close()

	c := azureResponsesTestClient(server.URL)
	_, _, err := c.azureBingSearch(context.Background(), "q")
	if err == nil {
		t.Fatal("want an error for a 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("want the status code in the error, got %v", err)
	}
}

// TestAgentAzureBingSearchTimeoutClamp covers the 0=default,
// >ceiling=ceiling clamping.
func TestAgentAzureBingSearchTimeoutClamp(t *testing.T) {
	if got := agentAzureBingSearchTimeout(agentConfig{}); got.Seconds() != azureBingSearchDefaultTimeoutSeconds {
		t.Fatalf("want default %ds for unset, got %v", azureBingSearchDefaultTimeoutSeconds, got)
	}
	if got := agentAzureBingSearchTimeout(agentConfig{AzureBingSearchTimeoutSeconds: 999}); got.Seconds() != azureBingSearchTimeoutCeiling {
		t.Fatalf("want clamped to ceiling %ds, got %v", azureBingSearchTimeoutCeiling, got)
	}
}

// TestAzureBingSearchToolExecutorRejectsEmptyQuery / RequiresAzureProfile
// confirm the executor's own input guards.
func TestAzureBingSearchToolExecutorRejectsEmptyQuery(t *testing.T) {
	rag, s := newTestRAG(t)
	exec := azureBingSearchToolExecutor(rag, s)
	if _, err := exec(context.Background(), `{"query":"  "}`); err == nil {
		t.Fatal("want an error for an empty/whitespace query")
	}
}

func TestAzureBingSearchToolExecutorRequiresAzureProfile(t *testing.T) {
	rag, s := newTestRAG(t)
	// newTestRAG's default settings have no Azure profile configured, and
	// its chat client map only ever registers "local" — getChatLM("azure")
	// falls back to the default ("local") profile, which isAzure() must
	// reject.
	exec := azureBingSearchToolExecutor(rag, s)
	_, err := exec(context.Background(), `{"query":"widgets"}`)
	if err == nil {
		t.Fatal("want an error when no Azure profile is configured")
	}
	if !strings.Contains(err.Error(), "Azure") {
		t.Fatalf("want a clear 'no Azure profile configured' message, got %v", err)
	}
}

// TestAzureBingSearchToolExecutorFormatsResults is the end-to-end happy
// path through the tool executor: registers a real Azure chat client
// pointed at a fake server, then confirms the formatted output includes
// both the grounded text and a "Quellen:" citation list.
func TestAzureBingSearchToolExecutorFormatsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output": [
				{"type": "message", "role": "assistant", "content": [
					{"type": "output_text", "text": "Grounded answer text.", "annotations": [
						{"type": "url_citation", "url": "https://a.example/", "title": "Source A"}
					]}
				]}
			]
		}`))
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	azureClient := newLMClientFull("azure", server.URL, "", "embed", "gpt-deployment", "k")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": rag.getChatLM("local"), "azure": azureClient}, "local")

	exec := azureBingSearchToolExecutor(rag, s)
	out, err := exec(context.Background(), `{"query":"widgets"}`)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "Grounded answer text.") {
		t.Fatalf("want the grounded text in the output, got %q", out)
	}
	if !strings.Contains(out, "Quellen:") || !strings.Contains(out, "https://a.example/") {
		t.Fatalf("want a citation list in the output, got %q", out)
	}
}

// TestHandleSettingsPersistsAzureBingSearch guards the failure mode this
// codebase has hit before (see AGENTS.md/memory): a new agentConfig field
// added but never wired into handleSettings' merge closure silently fails
// to persist.
func TestHandleSettingsPersistsAzureBingSearch(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"agent": map[string]any{
			"allow_azure_bing_search":           true,
			"azure_bing_search_timeout_seconds": 45,
		},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := settings.get().Agent
	if !got.AllowAzureBingSearch {
		t.Fatalf("want AllowAzureBingSearch=true after save, got false")
	}
	if got.AzureBingSearchTimeoutSeconds != 45 {
		t.Fatalf("want AzureBingSearchTimeoutSeconds=45, got %d", got.AzureBingSearchTimeoutSeconds)
	}
}

// TestBuildAgentToolsOffersAzureBingSearchOnlyWhenConfigured confirms the
// gate in buildAgentTools: the tool must never be offered when the Azure
// profile isn't actually usable, even if AllowAzureBingSearch is on —
// otherwise every call would just fail with a "not configured" error.
func TestBuildAgentToolsOffersAzureBingSearchOnlyWhenConfigured(t *testing.T) {
	rag, s := newTestRAG(t)
	s.Agent.AllowAzureBingSearch = true
	sess := agentSession{User: "tester", IsAdmin: true}

	tools, _ := buildAgentTools(rag, s, sess)
	for _, tl := range tools {
		if tl.Function.Name == azureBingSearchToolName {
			t.Fatalf("want azure_bing_search NOT offered without a configured Azure profile, got it in %+v", tools)
		}
	}

	s.Profiles.Azure.BaseURL = "https://myresource.openai.azure.com"
	s.Profiles.Azure.ChatModel = "gpt-deployment"
	tools, _ = buildAgentTools(rag, s, sess)
	found := false
	for _, tl := range tools {
		if tl.Function.Name == azureBingSearchToolName {
			found = true
		}
	}
	if !found {
		t.Fatalf("want azure_bing_search offered once the Azure profile is configured, got %+v", tools)
	}
}

// TestBuildAgentToolsRespectsPresetToolsForSearchTools confirms web_search
// and azure_bing_search — like the older MSSQL/Shop/HTTP-template live
// tools — are excluded when the caller's resolved preset doesn't list them
// under "tools", even though their own settings.Agent checkbox is on. Before
// this, a preset built to keep a use case off the open internet had no
// effect on either tool: only their global on/off flag mattered.
func TestBuildAgentToolsRespectsPresetToolsForSearchTools(t *testing.T) {
	rag, s := newTestRAG(t)
	s.Agent.AllowWebSearch = true
	s.Agent.AllowAzureBingSearch = true
	s.Agent.WebSearchAPIKey = "test-key"
	s.Profiles.Azure.BaseURL = "https://myresource.openai.azure.com"
	s.Profiles.Azure.ChatModel = "gpt-deployment"

	restricted := agentSession{User: "tester", IsAdmin: true, PresetTools: []string{"shop"}}
	tools, _ := buildAgentTools(rag, s, restricted)
	for _, tl := range tools {
		if tl.Function.Name == webSearchToolName || tl.Function.Name == azureBingSearchToolName {
			t.Fatalf("want %s excluded by a preset that only lists 'shop', got it in %+v", tl.Function.Name, tools)
		}
	}

	unrestricted := agentSession{User: "tester", IsAdmin: true}
	tools, _ = buildAgentTools(rag, s, unrestricted)
	var sawWebSearch, sawAzureBing bool
	for _, tl := range tools {
		if tl.Function.Name == webSearchToolName {
			sawWebSearch = true
		}
		if tl.Function.Name == azureBingSearchToolName {
			sawAzureBing = true
		}
	}
	if !sawWebSearch || !sawAzureBing {
		t.Fatalf("want both tools offered with no preset restriction (nil PresetTools = unrestricted), got web_search=%v azure_bing_search=%v", sawWebSearch, sawAzureBing)
	}

	allowed := agentSession{User: "tester", IsAdmin: true, PresetTools: []string{"web_search", "azure_bing_search"}}
	tools, _ = buildAgentTools(rag, s, allowed)
	sawWebSearch, sawAzureBing = false, false
	for _, tl := range tools {
		if tl.Function.Name == webSearchToolName {
			sawWebSearch = true
		}
		if tl.Function.Name == azureBingSearchToolName {
			sawAzureBing = true
		}
	}
	if !sawWebSearch || !sawAzureBing {
		t.Fatalf("want both tools offered when the preset explicitly lists them, got web_search=%v azure_bing_search=%v", sawWebSearch, sawAzureBing)
	}
}
