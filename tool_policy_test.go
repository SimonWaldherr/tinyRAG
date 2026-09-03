package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func toolPolicyTestEngine(t *testing.T, connectors *connectorStore) *StreamingEngine {
	t.Helper()
	settings := &settingsStore{}
	settings.s.ActiveRole = "it"
	return newStreamingEngine(
		&mockLMProvider{}, nil, settings,
		&apiStore{settings: settings}, &moduleStore{settings: settings}, connectors, nil,
	)
}

func TestAutonomousToolPolicyRejectsUnknownAndManualTools(t *testing.T) {
	eng := toolPolicyTestEngine(t, nil)
	s := eng.settings.get()

	for _, name := range []string{"llm", "does_not_exist", "sql_query", "shell", "nanogo"} {
		decision := eng.evaluateToolPolicy(s, XMLToolCall{Name: name, Query: "x"}, true)
		if decision.Allowed {
			t.Fatalf("%s must not be available for autonomous execution: %+v", name, decision)
		}
	}
	if !eng.evaluateToolPolicy(s, XMLToolCall{Name: "calculate", Query: "2+2"}, false).Allowed {
		t.Fatal("local calculation should remain autonomously available")
	}
	_, decision := eng.admitToolCall(s, XMLToolCall{Name: "calculate", Arguments: map[string]any{"unexpected": "x"}}, true)
	if decision.Allowed || decision.Reason != "missing_scalar_input" {
		t.Fatalf("built-in tools must not accept arbitrary argument objects: %+v", decision)
	}
}

func TestAutonomousConnectorPolicyAllowsOnlyReadOperations(t *testing.T) {
	store, err := newConnectorStore(filepath.Join(t.TempDir(), "connectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	connector := Connector{
		ID: "policy-test", Name: "Policy test", Type: ConnectorTypeHTTP, Enabled: true,
		BaseURL: "https://example.com",
		Capabilities: []Capability{
			{Name: "read_record", Type: CapabilityTypeTool, Method: http.MethodGet, InputSchema: JSONSchema{Type: "object"}},
			{Name: "read_with_body", Type: CapabilityTypeTool, Method: http.MethodGet, BodyTemplate: `{"filter":"{query}"}`, InputSchema: JSONSchema{Type: "object"}},
			{Name: "change_record", Type: CapabilityTypeTool, Method: http.MethodPost, InputSchema: JSONSchema{Type: "object"}},
			{Name: "ingest_record", Type: CapabilityTypeIngest, Method: http.MethodGet, InputSchema: JSONSchema{Type: "object"}},
		},
	}
	if _, err := store.upsert(connector); err != nil {
		t.Fatal(err)
	}
	if _, err := store.upsert(Connector{
		ID: "rpc-policy-test", Name: "RPC policy test", Type: ConnectorTypeRPC, Enabled: true,
		BaseURL: "https://example.com/rpc",
		Capabilities: []Capability{
			{Name: "rpc_read_record", Type: CapabilityTypeTool, RPCMethod: "records.get", ReadOnly: true, InputSchema: JSONSchema{Type: "object"}},
			{Name: "rpc_change_record", Type: CapabilityTypeTool, RPCMethod: "records.update", InputSchema: JSONSchema{Type: "object"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	eng := toolPolicyTestEngine(t, store)
	s := eng.settings.get()

	if !eng.evaluateToolPolicy(s, XMLToolCall{Name: "read_record", Query: "x"}, true).Allowed {
		t.Fatal("GET connector capability should be allowed after auto-search consent")
	}
	for _, name := range []string{"read_with_body", "change_record", "ingest_record"} {
		decision := eng.evaluateToolPolicy(s, XMLToolCall{Name: name, Query: "x"}, true)
		if decision.Allowed || decision.Mode != "confirmation_required" {
			t.Fatalf("%s must require explicit action, got %+v", name, decision)
		}
	}
	if decision := eng.evaluateToolPolicy(s, XMLToolCall{Name: "read_record", Query: "x"}, false); decision.Allowed || decision.Reason != "auto_search_disabled" {
		t.Fatalf("external GET needs auto-search consent, got %+v", decision)
	}
	if !eng.evaluateToolPolicy(s, XMLToolCall{Name: "rpc_read_record", Query: "x"}, true).Allowed {
		t.Fatal("explicit read-only RPC capability should be allowed after auto-search consent")
	}
	if decision := eng.evaluateToolPolicy(s, XMLToolCall{Name: "rpc_change_record", Query: "x"}, true); decision.Allowed || decision.Mode != "confirmation_required" {
		t.Fatalf("state-changing RPC capability must require explicit action, got %+v", decision)
	}
}

func TestAutonomousReadOnlySQLIsStrict(t *testing.T) {
	for _, query := range []string{
		"SELECT id, title FROM records WHERE active = 1",
		" select count(*) from records ",
	} {
		if !autonomousReadOnlySQL(query) {
			t.Fatalf("expected autonomous read-only SQL to accept %q", query)
		}
	}
	for _, query := range []string{
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT * FROM records; DELETE FROM records",
		"SELECT * FROM records INTO archive",
		"SELECT * FROM records WHERE note = 'DROP TABLE'",
	} {
		if autonomousReadOnlySQL(query) {
			t.Fatalf("expected autonomous read-only SQL to reject %q", query)
		}
	}
}

func TestPlannerHonorsAutoSearchAndRoundBudget(t *testing.T) {
	// An external planned lookup is not even part of the planner catalog while
	// auto-search is disabled.
	eng, _ := buildTestEngine(`[{"tool":"wikipedia","query":"Go"}]`)
	rec := httptest.NewRecorder()
	sw := &sseWriter{w: rec, flusher: &recordingFlusher{rec}}
	tel := newRequestTelemetry("plan-no-web", "chat", "Go")
	_, err := eng.Run(context.Background(), EngineRequest{
		RequestID: "plan-no-web", Question: "Go", PlanFirst: true,
		Messages: []chatMsg{{Role: "user", Content: "Go"}}, AutoSearch: false,
	}, sw, tel)
	if err != nil {
		t.Fatal(err)
	}
	events := collectSSEFromString(rec.Body.String())
	if len(events.events["tool_start"]) != 0 || len(events.events["plan"]) != 0 {
		t.Fatalf("planner must not execute or emit a web plan without consent: %#v", events.events)
	}

	// The planner's configured max is still bounded by the per-round engine cap.
	eng, _ = buildTestEngine(`[{"tool":"calculate","query":"1+1"},{"tool":"calculate","query":"2+2"},{"tool":"calculate","query":"3+3"}]`)
	eng.settings.s.AgentMaxPlanSteps = 5
	eng.cfg.MaxToolsPerRound = 1
	rec = httptest.NewRecorder()
	sw = &sseWriter{w: rec, flusher: &recordingFlusher{rec}}
	tel = newRequestTelemetry("plan-cap", "chat", "calculate")
	_, err = eng.Run(context.Background(), EngineRequest{
		RequestID: "plan-cap", Question: "calculate", PlanFirst: true,
		Messages: []chatMsg{{Role: "user", Content: "calculate"}},
	}, sw, tel)
	if err != nil {
		t.Fatal(err)
	}
	events = collectSSEFromString(rec.Body.String())
	if got := len(events.events["tool_start"]); got != 1 {
		t.Fatalf("planner started %d tools, want per-round cap of 1", got)
	}
}

func TestEngineCanonicalToolDedupAndUnknownTrace(t *testing.T) {
	response := `<tool name="CALCULATE"><query>2 + 2</query></tool><tool name="calculate"><query>2   +   2</query></tool><tool name="llm"><query>hidden</query></tool>`
	eng, _ := buildTestEngine(response)
	rec := httptest.NewRecorder()
	sw := &sseWriter{w: rec, flusher: &recordingFlusher{rec}}
	tel := newRequestTelemetry("canonical", "chat", "2+2")
	if _, err := eng.Run(context.Background(), EngineRequest{
		RequestID: "canonical", Question: "2+2", AutoSearch: true,
		Messages: []chatMsg{{Role: "user", Content: "2+2"}},
	}, sw, tel); err != nil {
		t.Fatal(err)
	}

	var executed, duplicate, unknown bool
	for _, record := range tel.ToolInvocations {
		if record.Tool == "calculate" && record.PolicyDecision == "allow" {
			executed = true
		}
		if strings.Contains(record.PolicyDecision, "duplicate_call") {
			duplicate = true
		}
		if strings.Contains(record.PolicyDecision, "unknown_tool") {
			unknown = true
		}
	}
	if !executed || !duplicate || !unknown {
		t.Fatalf("expected executed, duplicate and unknown traces, got %+v", tel.ToolInvocations)
	}
}

func TestFetchURLCtxHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchURLCtx(ctx, "https://example.com"); err == nil {
		t.Fatal("cancelled context must stop URL fetch before it performs work")
	}
}
