package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Streaming Engine Tests
// ─────────────────────────────────────────────────────────────────────────────

// ── Mock LM Provider ─────────────────────────────────────────────────────────

// mockLMProvider is a fake lmProvider that streams a predefined response.
type mockLMProvider struct {
	response string // the streamed text to emit
	delay    time.Duration
	err      error
}

func (m *mockLMProvider) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	return m.chatStreamDetailed(ctx, system, msgs, w, nil)
}

func (m *mockLMProvider) chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error {
	if m.err != nil {
		return m.err
	}
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	_, err := io.WriteString(w, m.response)
	return err
}

func (m *mockLMProvider) chatStreamVision(ctx context.Context, system string, msgs []visionMsg, w io.Writer, thinkW io.Writer) error {
	return m.chatStreamDetailed(ctx, system, nil, w, thinkW)
}

func (m *mockLMProvider) embed(texts []string) ([][]float64, error) {
	result := make([][]float64, len(texts))
	for i := range texts {
		result[i] = make([]float64, 4) // tiny fake embedding
	}
	return result, nil
}

func (m *mockLMProvider) embedSingle(text string) ([]float64, error) {
	return make([]float64, 4), nil
}

func (m *mockLMProvider) ping() error { return nil }

// ── SSE helper ───────────────────────────────────────────────────────────────

// collectSSE reads all SSE data tokens from a response body and returns them
// concatenated, plus named events.
type sseCollector struct {
	tokens []string
	events map[string][]string // eventName → []dataStr
}

func collectSSEFromString(body string) sseCollector {
	c := sseCollector{events: make(map[string][]string)}
	lines := strings.Split(body, "\n")
	currentEvent := "message"
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(line[5:])
			if data == "[DONE]" {
				currentEvent = "message"
				continue
			}
			c.events[currentEvent] = append(c.events[currentEvent], data)
			if currentEvent == "message" {
				var tok string
				if json.Unmarshal([]byte(data), &tok) == nil {
					c.tokens = append(c.tokens, tok)
				}
			}
			currentEvent = "message"
		}
	}
	return c
}

// ── recordingFlusher ─────────────────────────────────────────────────────────

type recordingFlusher struct {
	*httptest.ResponseRecorder
}

func (r *recordingFlusher) Flush() {}

// ── Engine construction helper ───────────────────────────────────────────────

// buildTestEngine builds a StreamingEngine with a mock LM that returns resp.
func buildTestEngine(resp string) (*StreamingEngine, *mockLMProvider) {
	lm := &mockLMProvider{response: resp}
	s := &settingsStore{}
	s.s.K = 3
	store := &apiStore{}
	mstore := &moduleStore{settings: s}
	// We can't easily instantiate ragSystem in unit tests without a DB,
	// so we pass nil and rely on the engine not calling RAG tools in these tests.
	eng := newStreamingEngine(lm, nil, s, store, mstore)
	return eng, lm
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestEngine_DirectStreamedAnswer(t *testing.T) {
	// A plain answer with no tool calls should stream directly.
	response := "This is a direct answer without any tool calls."
	eng, _ := buildTestEngine(response)

	rec := httptest.NewRecorder()
	rf := &recordingFlusher{rec}
	sw := &sseWriter{w: rec, flusher: rf}
	tel := newRequestTelemetry("test-1", "chat-1", "What is 2+2?")

	answer, err := eng.Run(context.Background(), EngineRequest{
		RequestID:    "test-1",
		Question:     "What is 2+2?",
		SystemPrompt: "You are a helpful assistant.",
		Messages:     []chatMsg{{Role: "user", Content: "What is 2+2?"}},
	}, sw, tel)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(answer, "direct answer") {
		t.Errorf("expected direct answer in output, got %q", answer)
	}
	if tel.ContinuationCount != 0 {
		t.Errorf("expected 0 continuations, got %d", tel.ContinuationCount)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Error("expected [DONE] in SSE output")
	}
}

func TestEngine_PartialXML_NotExecuted(t *testing.T) {
	// Partial XML block should not trigger tool execution.
	response := `I will search. <tool name="websearch"><query>incomplete`
	eng, _ := buildTestEngine(response)

	rec := httptest.NewRecorder()
	rf := &recordingFlusher{rec}
	sw := &sseWriter{w: rec, flusher: rf}
	tel := newRequestTelemetry("test-2", "chat-1", "test")

	_, err := eng.Run(context.Background(), EngineRequest{
		RequestID:    "test-2",
		Question:     "test",
		SystemPrompt: "sys",
		Messages:     []chatMsg{{Role: "user", Content: "test"}},
	}, sw, tel)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No tool invocations should have happened
	if tel.XMLBlocksEmitted != 0 {
		t.Errorf("expected 0 XML blocks, got %d", tel.XMLBlocksEmitted)
	}
}

func TestEngine_InvalidXMLHandledSafely(t *testing.T) {
	// Invalid XML (empty name attribute) should not crash and not trigger a tool.
	// Note: <tool name=""> starts with "<tool " so the parser detects it,
	// attempts to parse the block, and increments ParseErrors.
	response := `Here: <tool name=""><query>test</query></tool> done.`
	eng, _ := buildTestEngine(response)

	rec := httptest.NewRecorder()
	rf := &recordingFlusher{rec}
	sw := &sseWriter{w: rec, flusher: rf}
	tel := newRequestTelemetry("test-3", "chat-1", "test")

	_, err := eng.Run(context.Background(), EngineRequest{
		RequestID: "test-3", Question: "test",
		SystemPrompt: "sys",
		Messages:  []chatMsg{{Role: "user", Content: "test"}},
	}, sw, tel)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tel.XMLParseErrors == 0 {
		t.Error("expected at least 1 XML parse error recorded")
	}
	if tel.XMLBlocksEmitted != 0 {
		t.Errorf("expected 0 successful XML blocks, got %d", tel.XMLBlocksEmitted)
	}
}

func TestEngine_MaxContinuationsConfig(t *testing.T) {
	cfg := defaultEngineConfig()
	if cfg.MaxContinuations <= 0 {
		t.Errorf("expected positive MaxContinuations, got %d", cfg.MaxContinuations)
	}
	if cfg.MaxToolsTotal <= 0 {
		t.Errorf("expected positive MaxToolsTotal, got %d", cfg.MaxToolsTotal)
	}
	if cfg.ToolTimeout <= 0 {
		t.Errorf("expected positive ToolTimeout, got %v", cfg.ToolTimeout)
	}
}

func TestEngine_Deduplication(t *testing.T) {
	// Two identical tool blocks in one response should only execute once.
	block := `<tool name="calculate"><query>2+2</query></tool>`
	response := fmt.Sprintf("First: %s Second: %s", block, block)
	eng, _ := buildTestEngine(response)

	// Override cfg to allow calculate tool
	eng.cfg.MaxToolsTotal = 5

	rec := httptest.NewRecorder()
	rf := &recordingFlusher{rec}
	sw := &sseWriter{w: rec, flusher: rf}
	tel := newRequestTelemetry("test-dedup", "chat-1", "2+2")

	s := eng.settings.get()
	// Mark calculate as allowed via autoSearch=true
	_ = s

	_, _ = eng.Run(context.Background(), EngineRequest{
		RequestID:    "test-dedup",
		Question:     "2+2",
		SystemPrompt: "sys",
		Messages:     []chatMsg{{Role: "user", Content: "2+2"}},
		AutoSearch:   true,
	}, sw, tel)

	// Because the rag is nil and calculate doesn't use rag, it should attempt
	// the call. The second identical call should be deduped.
	// We check that not more than 1 invocation was recorded.
	nonDedup := 0
	for _, rec := range tel.ToolInvocations {
		if !rec.Deduplicated {
			nonDedup++
		}
	}
	if nonDedup > 1 {
		t.Errorf("expected at most 1 non-deduped invocation, got %d", nonDedup)
	}
}

func TestSSEWriter_EmitsEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	rf := &recordingFlusher{rec}
	sw := &sseWriter{w: rec, flusher: rf}

	sw.data("hello")
	sw.event("tool_start", `{"id":"tc-1","tool":"rag","query":"test"}`)
	sw.done()

	body := rec.Body.String()
	if !strings.Contains(body, `"hello"`) {
		t.Error("expected data hello in body")
	}
	if !strings.Contains(body, "event: tool_start") {
		t.Error("expected tool_start event")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("expected [DONE]")
	}
}

func TestBuildContinuationMessage_Success(t *testing.T) {
	results := []ToolResult{
		{Call: XMLToolCall{Name: "websearch", Query: "tinyRAG"}, Text: "tinyRAG is a tiny RAG system.", Source: "websearch:tinyRAG"},
	}
	msg := buildContinuationMessage(results)
	if !strings.Contains(msg, "websearch") {
		t.Error("expected tool name in continuation message")
	}
	if !strings.Contains(msg, "tinyRAG is a tiny") {
		t.Error("expected tool result in continuation message")
	}
}

func TestBuildContinuationMessage_ToolFailure(t *testing.T) {
	results := []ToolResult{
		{Call: XMLToolCall{Name: "websearch", Query: "test"}, Error: fmt.Errorf("connection refused")},
	}
	msg := buildContinuationMessage(results)
	if !strings.Contains(msg, "Fehler") {
		t.Error("expected error mention in continuation message")
	}
	if !strings.Contains(msg, "Erfinde keine Daten") {
		t.Error("expected no-hallucination instruction")
	}
}

// ── HTTP flusher check ───────────────────────────────────────────────────────

func TestSSEWriter_IsHttpFlusher(t *testing.T) {
	// Ensure sseWriter works with any http.ResponseWriter that implements Flusher.
	var _ http.ResponseWriter = httptest.NewRecorder()
	var _ http.Flusher = &recordingFlusher{}
}

// ── ToolAllowed ───────────────────────────────────────────────────────────────

func TestEngineToolAllowed(t *testing.T) {
	s := &settingsStore{}
	s.s.ActiveRole = "it"
	s.s.AllowNanoGo = false
	s.s.AllowShellExec = false

	eng := &StreamingEngine{settings: s, cfg: defaultEngineConfig()}

	settings := s.get()

	// calculate should always be allowed
	if !eng.toolAllowed(settings, "calculate", false) {
		t.Error("expected calculate to be allowed")
	}
	// websearch should require autoSearch
	if eng.toolAllowed(settings, "websearch", false) {
		t.Error("expected websearch blocked when autoSearch=false")
	}
	if !eng.toolAllowed(settings, "websearch", true) {
		t.Error("expected websearch allowed when autoSearch=true")
	}
	// shell requires AllowShellExec
	if eng.toolAllowed(settings, "shell", true) {
		t.Error("expected shell blocked when AllowShellExec=false")
	}
}
