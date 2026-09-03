package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClaudeChatOnceSendsCacheControlOnSystemPrompt asserts the actual
// request body sent to Claude's Messages API — not just that claudeChatOnce
// returns successfully — carries the cache_control marker in the one place
// that matters: the system prompt's content block. A regression here (e.g.
// falling back to the plain-string "system" form, which cannot carry
// cache_control at all) would silently defeat prompt caching without
// breaking any existing test, since claudeMessages/claudeChatOnce's other
// behavior is unaffected either way.
func TestClaudeChatOnceSendsCacheControlOnSystemPrompt(t *testing.T) {
	const systemPrompt = "You are a helpful assistant.\n\nKontext:\nsome retrieved context"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		rawSystem, ok := body["system"]
		if !ok {
			t.Fatalf("expected a \"system\" field in the request body, got none: %+v", body)
		}
		blocks, ok := rawSystem.([]any)
		if !ok {
			t.Fatalf("expected \"system\" to be an array of content blocks (required to carry cache_control), got %T: %v", rawSystem, rawSystem)
		}
		if len(blocks) != 1 {
			t.Fatalf("expected exactly one system content block, got %d: %+v", len(blocks), blocks)
		}
		block, ok := blocks[0].(map[string]any)
		if !ok {
			t.Fatalf("expected the system block to be an object, got %T", blocks[0])
		}
		if block["type"] != "text" {
			t.Errorf("want system block type %q, got %v", "text", block["type"])
		}
		if block["text"] != systemPrompt {
			t.Errorf("want system block text %q, got %v", systemPrompt, block["text"])
		}
		cc, ok := block["cache_control"].(map[string]any)
		if !ok {
			t.Fatalf("expected a cache_control object on the system block, got %T: %v", block["cache_control"], block["cache_control"])
		}
		if cc["type"] != "ephemeral" {
			t.Errorf("want cache_control.type %q, got %v", "ephemeral", cc["type"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "ok"}],
			"usage": {"input_tokens": 100, "output_tokens": 10, "cache_creation_input_tokens": 40, "cache_read_input_tokens": 0}
		}`))
	}))
	defer server.Close()

	client := newLMClientFull("claude", server.URL, "", "", "claude-sonnet-5", "test-key")
	ctx, trace := withTokenUsage(context.Background(), "alice@rubix.com", "ask")
	msg, err := client.claudeChatOnce(ctx, []chatMsg{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "hello"},
	}, nil)
	if err != nil {
		t.Fatalf("claudeChatOnce: %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("want assistant content %q, got %q", "ok", msg.Content)
	}

	events := trace.snapshot()
	if len(events) != 1 {
		t.Fatalf("want 1 token usage event, got %d: %+v", len(events), events)
	}
	if got := events[0]; got.PromptTokens != 100 || got.CompletionTokens != 10 || got.CacheCreationInputTokens != 40 || got.CacheReadInputTokens != 0 {
		t.Fatalf("token usage event not threaded through correctly: %+v", got)
	}
}

// TestClaudeChatOnceRollingCacheOnToolTranscript asserts the second,
// ROLLING cache breakpoint markLastMessageForCache adds: in a multi-round
// agent loop the request ends on the accumulated tool-result transcript, and
// its last content block must carry cache_control so round N reads rounds
// 1..N-1 from cache instead of reprocessing every prior tool result at full
// input price. The static system breakpoint must still be present too — the
// two together are what make the tool loop cheap.
func TestClaudeChatOnceRollingCacheOnToolTranscript(t *testing.T) {
	var gotMessages []any
	var gotSystem []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		gotMessages, _ = body["messages"].([]any)
		gotSystem, _ = body["system"].([]any)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer server.Close()

	// The shape chatWithToolsBudgetDeadline hands claudeChatOnce on round 2+:
	// system + user question + the assistant's tool call + that call's result.
	assistant := chatMsg{Role: "assistant"}
	assistant.ToolCalls = []toolCall{{ID: "call_1", Type: "function"}}
	assistant.ToolCalls[0].Function.Name = "search_knowledge_base"
	assistant.ToolCalls[0].Function.Arguments = `{"query":"x"}`
	all := []chatMsg{
		{Role: "system", Content: "big stable system prompt\n\nKontext:\n..."},
		{Role: "user", Content: "die Frage"},
		assistant,
		{Role: "tool", ToolCallID: "call_1", Name: "search_knowledge_base", Content: "1. [file] ... a large tool result ..."},
	}

	client := newLMClientFull("claude", server.URL, "", "", "claude-sonnet-5", "test-key")
	if _, err := client.claudeChatOnce(context.Background(), all, nil); err != nil {
		t.Fatalf("claudeChatOnce: %v", err)
	}

	// System breakpoint still present.
	if len(gotSystem) != 1 {
		t.Fatalf("want 1 system block, got %d: %+v", len(gotSystem), gotSystem)
	}
	if sys, _ := gotSystem[0].(map[string]any); sys["cache_control"] == nil {
		t.Errorf("system block lost its cache_control marker: %+v", gotSystem[0])
	}

	// Rolling breakpoint: the last message (the tool_result-carrying user
	// message) must have cache_control on its last content block.
	if len(gotMessages) == 0 {
		t.Fatalf("no messages in request body")
	}
	lastMsg, ok := gotMessages[len(gotMessages)-1].(map[string]any)
	if !ok {
		t.Fatalf("last message not an object: %T", gotMessages[len(gotMessages)-1])
	}
	if role := lastMsg["role"]; role != "user" {
		t.Fatalf("expected the tool-result transcript to end on a user message, got role %v", role)
	}
	blocks, ok := lastMsg["content"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("last message content not a non-empty block array: %T %v", lastMsg["content"], lastMsg["content"])
	}
	lastBlock, _ := blocks[len(blocks)-1].(map[string]any)
	if lastBlock["cache_control"] == nil {
		t.Errorf("rolling cache_control missing on the last tool-result block: %+v", lastBlock)
	}
}

// TestClaudeChatOnceSingleTurnLeavesMessagesUnmarked is the other half of the
// rolling-cache contract: markLastMessageForCache must NOT touch a trailing
// plain-string user message (the single-round Chat path, or round 1 before
// any tool call). There is no accumulated transcript to cache there, and
// leaving that common path byte-identical is the whole reason the marker is
// scoped to array-form content only.
func TestClaudeChatOnceSingleTurnLeavesMessagesUnmarked(t *testing.T) {
	var gotMessages []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		gotMessages, _ = body["messages"].([]any)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":1}}`))
	}))
	defer server.Close()

	client := newLMClientFull("claude", server.URL, "", "", "claude-sonnet-5", "test-key")
	if _, err := client.claudeChatOnce(context.Background(), []chatMsg{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "hello"},
	}, nil); err != nil {
		t.Fatalf("claudeChatOnce: %v", err)
	}
	if len(gotMessages) != 1 {
		t.Fatalf("want 1 message, got %d: %+v", len(gotMessages), gotMessages)
	}
	msg, _ := gotMessages[0].(map[string]any)
	// A plain-string user message: content is a JSON string, not a block
	// array, and must carry no cache_control anywhere.
	if _, isString := msg["content"].(string); !isString {
		t.Errorf("expected the single-turn user message to stay plain-string content, got %T: %v", msg["content"], msg["content"])
	}
}

// TestClaudeChatOnceNoSystemPromptOmitsSystemField covers the "nothing worth
// caching" case: chatWithToolsBudget's forced final round with no tools, or
// any caller that never supplies a system-role message, must not send a
// "system" field at all (empty-string cache_control on an absent prompt
// would be nonsensical) and must not error or panic building the request.
func TestClaudeChatOnceNoSystemPromptOmitsSystemField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if _, ok := body["system"]; ok {
			t.Errorf("expected no \"system\" field when no system message was supplied, got %+v", body["system"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "hi there"}],
			"usage": {"input_tokens": 5, "output_tokens": 2}
		}`))
	}))
	defer server.Close()

	client := newLMClientFull("claude", server.URL, "", "", "claude-sonnet-5", "test-key")
	msg, err := client.claudeChatOnce(context.Background(), []chatMsg{
		{Role: "user", Content: "hello, no system prompt here"},
	}, nil)
	if err != nil {
		t.Fatalf("claudeChatOnce with no system prompt must not error: %v", err)
	}
	if msg.Content != "hi there" {
		t.Fatalf("want assistant content %q, got %q", "hi there", msg.Content)
	}
}

// TestClaudeChatOnceCacheReadTokensThreadThrough covers the cache-HIT shape
// (cache_read_input_tokens > 0, cache_creation_input_tokens == 0) — the
// opposite half of the cache-WRITE case already covered above — landing in
// the recorded tokenUsageEvent unchanged.
func TestClaudeChatOnceCacheReadTokensThreadThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "ok"}],
			"usage": {"input_tokens": 12, "output_tokens": 3, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 900}
		}`))
	}))
	defer server.Close()

	client := newLMClientFull("claude", server.URL, "", "", "claude-sonnet-5", "test-key")
	ctx, trace := withTokenUsage(context.Background(), "bob@rubix.com", "ask")
	if _, err := client.claudeChatOnce(ctx, []chatMsg{
		{Role: "system", Content: "a large, stable, cached system prompt"},
		{Role: "user", Content: "hello again"},
	}, nil); err != nil {
		t.Fatalf("claudeChatOnce: %v", err)
	}

	events := trace.snapshot()
	if len(events) != 1 {
		t.Fatalf("want 1 token usage event, got %d: %+v", len(events), events)
	}
	if got := events[0]; got.CacheReadInputTokens != 900 || got.CacheCreationInputTokens != 0 {
		t.Fatalf("want cache_read_input_tokens=900 cache_creation_input_tokens=0, got %+v", got)
	}
}
