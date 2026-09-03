package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ---- migrateLegacyOpenAIAPIConfig -----------------------------------------

func TestMigrateLegacyOpenAIAPIConfigUpgradesOldShape(t *testing.T) {
	old := []byte(`{"openai_api":{"enabled":true,"port":8091,"preset":"partner-a"}}`)
	migrated, err := migrateLegacyOpenAIAPIConfig(old)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var cfg struct {
		OpenAIAPI openAIAPIConfig `json:"openai_api"`
	}
	if err := json.Unmarshal(migrated, &cfg); err != nil {
		t.Fatalf("unmarshal migrated: %v (raw: %s)", err, migrated)
	}
	if !cfg.OpenAIAPI.Enabled || cfg.OpenAIAPI.Port != 8091 {
		t.Fatalf("want enabled/port preserved, got %+v", cfg.OpenAIAPI)
	}
	if len(cfg.OpenAIAPI.Endpoints) != 1 {
		t.Fatalf("want exactly 1 synthesized endpoint, got %+v", cfg.OpenAIAPI.Endpoints)
	}
	ep := cfg.OpenAIAPI.Endpoints[0]
	if ep.Name != "" || !ep.Enabled || !ep.EnableRAG || !ep.EnableTools || ep.Preset != "partner-a" {
		t.Fatalf("want a root ('') endpoint reproducing old always-on RAG+Tools behavior with the old preset, got %+v", ep)
	}
}

func TestMigrateLegacyOpenAIAPIConfigNoOpCases(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"absent", `{"lang":"de"}`},
		{"already migrated", `{"openai_api":{"enabled":true,"port":1,"endpoints":[{"name":"","enabled":true}]}}`},
		{"fresh/no preset", `{"openai_api":{"enabled":false,"port":0}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := migrateLegacyOpenAIAPIConfig([]byte(c.data))
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if string(out) != c.data {
				t.Fatalf("want a no-op, got %s", out)
			}
		})
	}
}

// ---- validateOpenAIEndpoints -----------------------------------------------

func TestValidateOpenAIEndpointsAcceptsGood(t *testing.T) {
	list := []openAIEndpointConfig{
		{Name: "", Enabled: true},
		{Name: "tools-only", Enabled: true, EnableTools: true, MaxToolRounds: 3},
		{Name: "pinned-azure", Enabled: true, Profile: "azure"},
	}
	if err := validateOpenAIEndpoints(list); err != nil {
		t.Fatalf("valid endpoints rejected: %v", err)
	}
}

func TestValidateOpenAIEndpointsRejections(t *testing.T) {
	cases := []struct {
		name string
		list []openAIEndpointConfig
	}{
		{"bad name char", []openAIEndpointConfig{{Name: "bad name!"}}},
		{"name starting with digit", []openAIEndpointConfig{{Name: "1abc"}}},
		{"duplicate empty name", []openAIEndpointConfig{{Name: ""}, {Name: ""}}},
		{"duplicate named (case-insensitive)", []openAIEndpointConfig{{Name: "partner"}, {Name: "Partner"}}},
		{"negative max_tool_rounds", []openAIEndpointConfig{{Name: "x", MaxToolRounds: -1}}},
		{"unknown profile", []openAIEndpointConfig{{Name: "x", Profile: "not-a-real-backend"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateOpenAIEndpoints(c.list); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

// ---- handleOpenAIModels: pinned profile ------------------------------------

func TestHandleOpenAIModelsPinnedProfile(t *testing.T) {
	s := appSettings{API: apiConfig{Keys: []apiKeyRecord{mustAPIKeyRecord(t, "k")}}}
	s.Profiles.Azure = llmProfile{BaseURL: "https://example.openai.azure.com", ChatModel: "gpt-4o"}
	withTestGlobalSettings(t, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", requireOpenAIAPIKey(handleOpenAIModels(openAIEndpointConfig{Profile: "azure"})))
	server := httptest.NewServer(mux)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	var list openAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "azure" {
		t.Fatalf("want exactly the pinned 'azure' model listed, got %+v", list.Data)
	}
}

// ---- handleOpenAIChatCompletions: the RAG × Tools capability matrix -------

// mockLLMCapture is a minimal OpenAI-compatible backend that records the
// last request's tool count and every message's role/content, answering
// either as SSE (tools-less chatStream path) or as one JSON body
// (chatOnce's tool-decision-round path) depending on the request's own
// "stream" field — mirroring exactly what llm.go's chatStreamMessages
// (Stream:true, no Tools field) vs chatOnce (Stream:false, Tools set)
// actually send, so both of chatWithToolsBudget's internal branches are
// exercised correctly regardless of which one a given capability
// combination takes.
type mockLLMCapture struct {
	mu       sync.Mutex
	toolCnt  int
	messages []chatMsg
}

func (m *mockLLMCapture) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolCnt, m.messages = 0, nil
}

func (m *mockLLMCapture) systemPrompt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.messages {
		if msg.Role == "system" {
			return msg.Content
		}
	}
	return ""
}

func (m *mockLLMCapture) toolCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.toolCnt
}

func (m *mockLLMCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool              `json:"stream"`
			Tools    []json.RawMessage `json:"tools"`
			Messages []chatMsg         `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.toolCnt = len(req.Tools)
		m.messages = req.Messages
		m.mu.Unlock()
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}
}

func TestHandleOpenAIChatCompletionsCapabilityMatrix(t *testing.T) {
	mock := &mockLLMCapture{}
	llmServer := httptest.NewServer(mock.handler())
	defer llmServer.Close()

	rag, s := newTestRAG(t)
	if _, err := ingestDocument(rag, s, "test-embed", "file:/geheim.txt", "file", "geheim.txt", "GEHEIMESSTICHWORT im Testdokument.", 0, false); err != nil {
		t.Fatalf("ingestDocument: %v", err)
	}
	chatClient := newLMClientFull("local", llmServer.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.Profiles.Local.EmbedModel = "test-embed"
	// A live tool for the EnableTools branches to actually offer.
	s.MSSQL.Enabled = true
	s.MSSQL.AllowGenericQuery = true
	s.MSSQL.Database = "testdb"
	s.API.Keys = []apiKeyRecord{mustAPIKeyRecord(t, "k")}
	withTestGlobalSettings(t, s)

	run := func(t *testing.T, ep openAIEndpointConfig) (toolCount int, systemPrompt string) {
		t.Helper()
		mock.reset()
		handler := requireOpenAIAPIKey(handleOpenAIChatCompletions(rag, ep))
		body, _ := json.Marshal(openAIChatCompletionRequest{
			Model: "local",
			Messages: []openAIChatMessage{
				{Role: "system", Content: "MEINE-EIGENE-SYSTEMANWEISUNG"},
				{Role: "user", Content: "Was steht im Testdokument?"},
			},
		})
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer k")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", w.Code, w.Body.String())
		}
		return mock.toolCount(), mock.systemPrompt()
	}

	t.Run("llm only: no RAG, no tools — bare passthrough", func(t *testing.T) {
		toolCount, sys := run(t, openAIEndpointConfig{})
		if toolCount != 0 {
			t.Errorf("want 0 tools offered, got %d", toolCount)
		}
		if !strings.Contains(sys, "MEINE-EIGENE-SYSTEMANWEISUNG") {
			t.Errorf("want the caller's own system message honored, got %q", sys)
		}
		if strings.Contains(sys, "GEHEIMESSTICHWORT") || strings.Contains(sys, "Kontext:") {
			t.Errorf("must NOT inject R3's own RAG context, got %q", sys)
		}
	})

	t.Run("RAG only: no tools", func(t *testing.T) {
		toolCount, sys := run(t, openAIEndpointConfig{EnableRAG: true})
		if toolCount != 0 {
			t.Errorf("want 0 tools offered, got %d", toolCount)
		}
		if !strings.Contains(sys, "GEHEIMESSTICHWORT") {
			t.Errorf("want the retrieved chunk in the system prompt, got %q", sys)
		}
		if strings.Contains(sys, "MEINE-EIGENE-SYSTEMANWEISUNG") {
			t.Errorf("RAG mode replaces the caller's own system message, must not leak it through, got %q", sys)
		}
	})

	t.Run("tools only: no RAG", func(t *testing.T) {
		toolCount, sys := run(t, openAIEndpointConfig{EnableTools: true})
		if toolCount == 0 {
			t.Errorf("want at least the live MSSQL tool offered, got 0")
		}
		if strings.Contains(sys, "GEHEIMESSTICHWORT") {
			t.Errorf("must NOT inject R3's own RAG context, got %q", sys)
		}
		if !strings.Contains(sys, "MEINE-EIGENE-SYSTEMANWEISUNG") {
			t.Errorf("no-RAG mode must still honor the caller's own system message, got %q", sys)
		}
	})

	t.Run("RAG and tools together", func(t *testing.T) {
		toolCount, sys := run(t, openAIEndpointConfig{EnableRAG: true, EnableTools: true})
		if toolCount == 0 {
			t.Errorf("want at least the live MSSQL tool offered, got 0")
		}
		if !strings.Contains(sys, "GEHEIMESSTICHWORT") {
			t.Errorf("want the retrieved chunk in the system prompt, got %q", sys)
		}
	})
}

// ---- reconcileOpenAIAPIServer: multi-endpoint routing + live reconfigure ---

func TestReconcileOpenAIAPIServerMultiEndpointRouting(t *testing.T) {
	rag, s := newTestRAG(t)
	s.API.Keys = []apiKeyRecord{mustAPIKeyRecord(t, "multi-key")}
	withTestGlobalSettings(t, s)
	t.Cleanup(stopOpenAIAPIServer)

	port := freeTCPPortForTest(t)
	cfg := openAIAPIConfig{Enabled: true, Port: port, Endpoints: []openAIEndpointConfig{
		{Name: "", Enabled: true, EnableRAG: true},
		{Name: "toolsonly", Enabled: true, EnableTools: true},
		{Name: "disabled-one", Enabled: false, EnableRAG: true},
	}}
	reconcileOpenAIAPIServer(rag, cfg)
	waitForOpenAIServer(t, port, true)

	get := func(path string) int {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp.StatusCode
	}
	// Every enabled endpoint is reachable (401 without a key — reachable,
	// just needs auth — proves the route exists at all).
	if got := get("/v1/models"); got != http.StatusUnauthorized {
		t.Errorf("root endpoint /v1/models: want 401, got %d", got)
	}
	if got := get("/toolsonly/v1/models"); got != http.StatusUnauthorized {
		t.Errorf("named endpoint /toolsonly/v1/models: want 401, got %d", got)
	}
	// The disabled endpoint's path was never registered at all -> 404, not 401.
	if got := get("/disabled-one/v1/models"); got != http.StatusNotFound {
		t.Errorf("disabled endpoint /disabled-one/v1/models: want 404 (not registered), got %d", got)
	}

	// Reconfigure WITHOUT changing the port: disable "toolsonly" and enable
	// "disabled-one" instead. The old (buggy) reconcile logic only rebuilt
	// the mux when the port changed, so this proves the fix — settings
	// changes alone (no port bounce) now take effect immediately.
	cfg2 := openAIAPIConfig{Enabled: true, Port: port, Endpoints: []openAIEndpointConfig{
		{Name: "", Enabled: true, EnableRAG: true},
		{Name: "toolsonly", Enabled: false, EnableTools: true},
		{Name: "disabled-one", Enabled: true, EnableRAG: true},
	}}
	reconcileOpenAIAPIServer(rag, cfg2)
	waitForOpenAIServer(t, port, true)

	if got := get("/toolsonly/v1/models"); got != http.StatusNotFound {
		t.Errorf("after disabling toolsonly (same port, no restart): want 404, got %d", got)
	}
	if got := get("/disabled-one/v1/models"); got != http.StatusUnauthorized {
		t.Errorf("after enabling disabled-one (same port, no restart): want 401 (now reachable), got %d", got)
	}
}
