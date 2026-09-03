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

// TestRunToolRouterDisabledSkipsLLM confirms the disabled default never
// makes an LLM call at all — the fake server fails the test if it's ever
// hit.
func TestRunToolRouterDisabledSkipsLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no LLM call should happen when the tool router is disabled")
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	s := appSettings{ToolRouter: toolRouterConfig{Enabled: false}, MSSQL: mssqlConfig{Enabled: true, AllowGenericQuery: true}}
	got := runToolRouter(context.Background(), lm, "wie viel Bestand hat Artikel X?", s, agentSession{}, sourcePreset{}, true)
	if got != "" {
		t.Fatalf("want empty string when disabled, got %q", got)
	}
}

// TestRunToolRouterNoLiveToolsSkipsLLM confirms that with no MSSQL/Shop/HTTP
// tool configured, buildLiveTools returns an empty set and runToolRouter
// short-circuits before ever calling the LLM.
func TestRunToolRouterNoLiveToolsSkipsLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no LLM call should happen when there are no live tools to offer")
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	s := appSettings{ToolRouter: toolRouterConfig{Enabled: true}}
	got := runToolRouter(context.Background(), lm, "irgendeine Frage", s, agentSession{}, sourcePreset{}, true)
	if got != "" {
		t.Fatalf("want empty string with no live tools, got %q", got)
	}
}

// TestRunToolRouterNoToolNeededReturnsEmpty confirms that when the model
// answers directly (no tool_calls), runToolRouter discards that text and
// returns "" — its own prose is never used, only a tool call would be.
func TestRunToolRouterNoToolNeededReturnsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "nein"}}]}`))
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	// MSSQL enabled+AllowGenericQuery so buildLiveTools offers a tool, but
	// the fake model never calls it — no real DB connection is attempted.
	s := appSettings{ToolRouter: toolRouterConfig{Enabled: true}, MSSQL: mssqlConfig{Enabled: true, AllowGenericQuery: true}}
	got := runToolRouter(context.Background(), lm, "hallo", s, agentSession{}, sourcePreset{}, true)
	if got != "" {
		t.Fatalf("want empty string when the router decides no tool is needed, got %q", got)
	}
}

// TestRunToolRouterLLMErrorFailsOpen confirms a broken router call never
// propagates an error or panics — it must return "" and let the caller's
// main answer proceed unaffected.
func TestRunToolRouterLLMErrorFailsOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")

	s := appSettings{ToolRouter: toolRouterConfig{Enabled: true}, MSSQL: mssqlConfig{Enabled: true, AllowGenericQuery: true}}
	got := runToolRouter(context.Background(), lm, "hallo", s, agentSession{}, sourcePreset{}, true)
	if got != "" {
		t.Fatalf("want empty string on LLM failure (fail-open), got %q", got)
	}
}

// TestRunToolRouterExecutesToolAndReturnsContext is the happy path: the
// router decides a tool is needed, calls it, and returns a context block
// containing the tool's name and result — using an HTTP query template
// (not MSSQL) so the "external system" is a second httptest server this
// test fully controls, no real network dependency. Also confirms the
// step this causes reaches a wrapped progress emitter tagged Phase:"router".
func TestRunToolRouterExecutesToolAndReturnsContext(t *testing.T) {
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "widgets" {
			t.Errorf("want q=widgets, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stock": 42}`))
	}))
	defer toolServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream {
			t.Errorf("the router's decision call must be non-streaming")
		}
		if len(req.Tools) != 1 || req.Tools[0].Function.Name != "lookup_stock" {
			t.Errorf("expected only the lookup_stock tool to be offered, got %+v", req.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "tool_calls": [
			{"id": "call_1", "type": "function", "function": {"name": "lookup_stock", "arguments": "{\"q\":\"widgets\"}"}}
		]}}]}`))
	}))
	defer llmServer.Close()

	lm := newLMClientFull("local", llmServer.URL, "", "embed", "chat", "")
	s := appSettings{
		ToolRouter: toolRouterConfig{Enabled: true},
		HTTPTemplates: []httpQueryTemplate{{
			Name:        "lookup_stock",
			Description: "Looks up stock for an item",
			Method:      "GET",
			URLTemplate: toolServer.URL + "/items?q={q}",
			AuthSource:  "none",
			Parameters:  []sqlQueryParam{{Name: "q", Type: "string", Required: true}},
			Enabled:     true,
		}},
	}

	var steps []agentStep
	ctx := withAgentProgress(context.Background(), func(st agentStep) {
		steps = append(steps, st)
	})

	got := runToolRouter(ctx, lm, "wie viel Bestand hat widgets?", s, agentSession{}, sourcePreset{}, false)
	if !strings.Contains(got, "lookup_stock") {
		t.Fatalf("want the context block to mention the tool name, got %q", got)
	}
	if !strings.Contains(got, "42") {
		t.Fatalf("want the context block to contain the tool's result, got %q", got)
	}

	var sawRouterToolEnd bool
	for _, st := range steps {
		if st.Type == "tool_end" && st.Tool == "lookup_stock" {
			sawRouterToolEnd = true
			if st.Phase != "router" {
				t.Errorf("want the router's own tool step tagged Phase=%q, got %q", "router", st.Phase)
			}
		}
	}
	if !sawRouterToolEnd {
		t.Fatalf("want a tool_end step for lookup_stock forwarded to the parent emitter, got %+v", steps)
	}
}

// TestResolveRouterProfile checks the override/fallback logic: an explicit
// Profile wins, otherwise the main call's own profile is reused.
func TestResolveRouterProfile(t *testing.T) {
	if got := resolveRouterProfile(toolRouterConfig{}, "local"); got != "local" {
		t.Fatalf("want fallback to mainProfile %q, got %q", "local", got)
	}
	if got := resolveRouterProfile(toolRouterConfig{Profile: "azure"}, "local"); got != "azure" {
		t.Fatalf("want the explicit override %q, got %q", "azure", got)
	}
}

// TestValidateToolRouterSettings exercises the enum guard handleSettings
// runs before persisting ToolRouter — same reasoning/shape as
// validateStorageSettings (vectorstore.go).
func TestValidateToolRouterSettings(t *testing.T) {
	cases := []struct {
		name    string
		c       toolRouterConfig
		wantErr bool
	}{
		{"empty profile valid (means: same as main call)", toolRouterConfig{}, false},
		{"local valid", toolRouterConfig{Enabled: true, Profile: "local"}, false},
		{"azure valid", toolRouterConfig{Enabled: true, Profile: "azure"}, false},
		{"case-insensitive", toolRouterConfig{Profile: "Azure"}, false},
		{"unknown profile", toolRouterConfig{Profile: "openai"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateToolRouterSettings(c.c)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateToolRouterSettings(%+v): wantErr=%v, got %v", c.c, c.wantErr, err)
			}
		})
	}
}

// TestHandleSettingsPersistsToolRouter confirms ToolRouter round-trips
// through POST /api/settings (same pattern as
// TestHandleSettingsPersistsStorage, handlers_storage_test.go), including
// the lower-casing normalization applied on save.
func TestHandleSettingsPersistsToolRouter(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"tool_router": map[string]any{"enabled": true, "profile": "Azure"},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := settings.get().ToolRouter
	want := toolRouterConfig{Enabled: true, Profile: "azure"}
	if got != want {
		t.Fatalf("ToolRouter after save: want %+v, got %+v", want, got)
	}
}

// TestHandleSettingsRejectsInvalidToolRouterSettings guards the 400 path:
// an unknown Profile must be rejected, and the previously saved ToolRouter
// must survive unchanged (the whole save is rejected, not partially
// applied).
func TestHandleSettingsRejectsInvalidToolRouterSettings(t *testing.T) {
	rag, s := newTestRAG(t)
	s.ToolRouter = toolRouterConfig{Enabled: true, Profile: "local"}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"tool_router": map[string]any{"profile": "openai"},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown tool_router profile, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := settings.get().ToolRouter; got != s.ToolRouter {
		t.Fatalf("a rejected save must not change ToolRouter: want %+v, got %+v", s.ToolRouter, got)
	}
}
