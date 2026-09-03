package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTestGlobalSettings points the package-level `settings` store (which
// handleOpenAIChatCompletions/requireOpenAIAPIKey read via settings.get(),
// same as every other handler in this codebase) at a fresh, temp-file-backed
// store seeded with s, and restores the previous global on cleanup — no
// other test in this package touches the global store, so this only needs
// to not leak across tests, not coordinate with concurrent ones.
func withTestGlobalSettings(t *testing.T, s appSettings) {
	t.Helper()
	prev := settings
	ss, err := loadOrCreateSettings(filepath.Join(t.TempDir(), "settings.json"), s)
	if err != nil {
		t.Fatalf("loadOrCreateSettings: %v", err)
	}
	settings = ss
	t.Cleanup(func() { settings = prev })
}

func TestAppendOpenAICitationsFooter(t *testing.T) {
	if got := appendOpenAICitationsFooter("answer", nil); got != "answer" {
		t.Fatalf("no citations: want the answer unchanged, got %q", got)
	}
	citations := []sourceInfo{
		{Marker: 1, SourceName: "Angebot.pdf", SourceURL: "https://example.com/a.pdf"},
		{Marker: 2, SourceName: "Mail von Kunde X"},
	}
	got := appendOpenAICitationsFooter("Die Antwort ist 42.", citations)
	if !strings.HasPrefix(got, "Die Antwort ist 42.\n\nQuellen:\n") {
		t.Fatalf("want the original answer followed by a Quellen: footer, got %q", got)
	}
	if !strings.Contains(got, "[Q1] Angebot.pdf (https://example.com/a.pdf)") {
		t.Fatalf("want citation 1 with its URL, got %q", got)
	}
	if !strings.Contains(got, "[Q2] Mail von Kunde X") || strings.Contains(got, "[Q2] Mail von Kunde X (") {
		t.Fatalf("want citation 2 without a URL (none set), got %q", got)
	}
}

func TestOpenAIChatMessageAcceptsTextContentParts(t *testing.T) {
	var message openAIChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`), &message); err != nil {
		t.Fatalf("unmarshal text content parts: %v", err)
	}
	if message.Content != "first\nsecond" {
		t.Fatalf("want joined text parts, got %q", message.Content)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}`), &message); err == nil {
		t.Fatal("want unsupported image content to be rejected")
	}
}

func TestWriteOpenAIErrorUsesSDKCompatibleEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, "bad model", "invalid_request_error", "model", "model_not_found", http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	var response openAIErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error.Message != "bad model" || response.Error.Type != "invalid_request_error" || response.Error.Param == nil || *response.Error.Param != "model" {
		t.Fatalf("unexpected OpenAI error envelope: %+v", response)
	}
}

func TestOpenAIModelsRequiresAPIKey(t *testing.T) {
	// Azure needs both a ChatModel (the same "is this profile configured"
	// check every non-local profile gets) and a BaseURL (azure-specific,
	// handleOpenAIModels) to be listed — "local" is always listed
	// regardless, so this fixture only needs to configure the one other
	// profile the assertion below expects to see.
	s := appSettings{API: apiConfig{Keys: []apiKeyRecord{mustAPIKeyRecord(t, "valid-key")}}}
	s.Profiles.Azure = llmProfile{BaseURL: "https://example.openai.azure.com", ChatModel: "gpt-4o"}
	withTestGlobalSettings(t, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", requireOpenAIAPIKey(handleOpenAIModels(openAIEndpointConfig{})))
	server := httptest.NewServer(mux)
	defer server.Close()

	// No key at all.
	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", resp.StatusCode)
	}

	// Wrong key.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models (wrong key): %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key: want 401, got %d", resp.StatusCode)
	}

	// Valid key, presented OpenAI-style as a bearer token.
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer valid-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models (valid key): %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid key: want 200, got %d", resp.StatusCode)
	}
	var list openAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("want 2 models (local, azure), got %+v", list.Data)
	}
}

// mustAPIKeyRecord builds an enabled apiKeyRecord whose hash matches
// plaintext, via the same hashAPIKey (apikey.go) findAPIKey compares
// against — so this test-fixed key value actually authenticates, unlike
// generateAPIKey's own random plaintext.
func mustAPIKeyRecord(t *testing.T, plaintext string) apiKeyRecord {
	t.Helper()
	return apiKeyRecord{ID: "test-key-id", Name: "test key", Hash: hashAPIKey(plaintext), Enabled: true, CreatedAt: time.Now().Unix()}
}

func TestHandleOpenAIChatCompletionsEndToEnd(t *testing.T) {
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode chat request: %v", err)
		}
		var sawContext bool
		for _, m := range req.Messages {
			if m.Role == "system" && strings.Contains(m.Content, "RUBIXRAG-TESTDOKUMENT") {
				sawContext = true
			}
		}
		if !sawContext {
			t.Errorf("want the retrieved chunk in the system prompt, got messages %+v", req.Messages)
		}
		// No tools are configured in this test (MSSQL/Shop both off), so
		// chatWithToolsBudget's zero-tools branch delegates straight to
		// chatStream (llm.go) regardless of the OpenAI request's own
		// "stream" field — it always buffers an SSE response internally,
		// even for handleOpenAIChatCompletions' non-streaming JSON path.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Die Antwort steht in [Q1].\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer chatServer.Close()

	rag, s := newTestRAG(t)
	if _, err := ingestDocument(rag, s, "test-embed", "file:/docs/Test.txt", "file", "Test.txt", "RUBIXRAG-TESTDOKUMENT: der Inhalt, den die Suche finden soll.", 0, false); err != nil {
		t.Fatalf("ingestDocument: %v", err)
	}
	chatClient := newLMClientFull("local", chatServer.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")

	// activeEmbedModel() (settings.go) resolves from Profiles.Local.EmbedModel
	// when EmbedProfile isn't "azure" — must match the "test-embed" model
	// name ingestDocument tagged the chunk with above, or vectorCandidates'
	// embed_model filter matches nothing and retrieval silently finds
	// zero hits.
	s.Profiles.Local.EmbedModel = "test-embed"
	s.API.Keys = []apiKeyRecord{mustAPIKeyRecord(t, "test-secret-key")}
	withTestGlobalSettings(t, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", requireOpenAIAPIKey(handleOpenAIChatCompletions(rag, openAIEndpointConfig{EnableRAG: true, EnableTools: true})))
	server := httptest.NewServer(mux)
	defer server.Close()

	body, _ := json.Marshal(openAIChatCompletionRequest{
		Model:    "local",
		Messages: []openAIChatMessage{{Role: "user", Content: "Was steht im Testdokument?"}},
	})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer test-secret-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out openAIChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("want exactly 1 choice, got %+v", out.Choices)
	}
	if !strings.Contains(out.Choices[0].Message.Content, "Die Antwort steht in [Q1].") {
		t.Fatalf("want the model's answer text present, got %q", out.Choices[0].Message.Content)
	}
	if !strings.Contains(out.Choices[0].Message.Content, "Quellen:") || !strings.Contains(out.Choices[0].Message.Content, "Test.txt") {
		t.Fatalf("want a Quellen: footer citing Test.txt, got %q", out.Choices[0].Message.Content)
	}
	if out.Object != "chat.completion" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("want OpenAI-shaped response fields, got %+v", out)
	}
	if out.Usage.PromptTokens == 0 || out.Usage.CompletionTokens == 0 || out.Usage.TotalTokens != out.Usage.PromptTokens+out.Usage.CompletionTokens {
		t.Fatalf("want useful estimated usage, got %+v", out.Usage)
	}
}

// TestReconcileOpenAIAPIServerLifecycle exercises the actual hot-reload
// server lifecycle (start on save, rebind on port change, stop when
// disabled) — the risky, concurrency-bearing part of openai_api.go that a
// handler-level test alone wouldn't cover.
func TestReconcileOpenAIAPIServerLifecycle(t *testing.T) {
	rag, s := newTestRAG(t)
	s.API.Keys = []apiKeyRecord{mustAPIKeyRecord(t, "lifecycle-key")}
	withTestGlobalSettings(t, s)
	t.Cleanup(stopOpenAIAPIServer)

	port := freeTCPPortForTest(t)
	endpoints := []openAIEndpointConfig{{Name: "", Enabled: true, EnableRAG: true, EnableTools: true}}
	reconcileOpenAIAPIServer(rag, openAIAPIConfig{Enabled: true, Port: port, Endpoints: endpoints})
	waitForOpenAIServer(t, port, true)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		t.Fatalf("GET /v1/models on started server: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without a key (server IS reachable, just needs auth), got %d", resp.StatusCode)
	}

	// Disabling stops the listener.
	reconcileOpenAIAPIServer(rag, openAIAPIConfig{Enabled: false, Port: port, Endpoints: endpoints})
	waitForOpenAIServer(t, port, false)
}

func freeTCPPortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func waitForOpenAIServer(t *testing.T, port int, wantUp bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			if wantUp {
				return
			}
		} else if !wantUp {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wantUp {
		t.Fatalf("server on port %d never came up", port)
	}
	t.Fatalf("server on port %d never shut down", port)
}
