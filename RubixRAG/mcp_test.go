package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestMCPOriginValidation(t *testing.T) {
	r := httptest.NewRequest("POST", "http://r3.example.test/mcp", nil)
	r.Host = "r3.example.test"
	if !mcpOriginAllowed(r) {
		t.Fatal("non-browser request without Origin must be accepted")
	}
	r.Header.Set("Origin", "https://r3.example.test")
	if !mcpOriginAllowed(r) {
		t.Fatal("same-host Origin must be accepted")
	}
	r.Header.Set("Origin", "https://attacker.example.test")
	if mcpOriginAllowed(r) {
		t.Fatal("foreign Origin must be rejected")
	}
}

func TestMCPToolsAreReadOnlyRAGTools(t *testing.T) {
	tools := mcpTools()
	got := map[string]bool{}
	for _, tool := range tools {
		got[tool["name"].(string)] = true
	}
	for _, want := range []string{"search_knowledge_base", "get_source_content", "get_r3_status"} {
		if !got[want] {
			t.Fatalf("missing MCP tool %q: %#v", want, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("MCP surface must stay read-only and minimal, got %#v", got)
	}
}

func TestValidMCPRequestIDAndArguments(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`"request-1"`), json.RawMessage(`42`)} {
		if !validMCPRequestID(raw) {
			t.Fatalf("want valid MCP request id %q", raw)
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`{}`), json.RawMessage(`true`)} {
		if validMCPRequestID(raw) {
			t.Fatalf("want invalid MCP request id %q", raw)
		}
	}
	if err := mcpOnlyArguments(map[string]any{"query": "open orders"}, "query", "k"); err != nil {
		t.Fatalf("allowed arguments rejected: %v", err)
	}
	if err := mcpOnlyArguments(map[string]any{"unexpected": true}, "query"); err == nil {
		t.Fatal("want unexpected MCP arguments to be rejected")
	}
}
