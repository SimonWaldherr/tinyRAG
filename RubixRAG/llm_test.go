package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatWithToolsNoToolsDelegatesToChatStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Errorf("with no tools configured, chatWithTools should stream directly like chatStream")
		}
		if len(req.Tools) != 0 {
			t.Errorf("expected no tools in request, got %d", len(req.Tools))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi there.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	var out bytes.Buffer
	err := client.chatWithTools(context.Background(), "system", []chatMsg{{Role: "user", Content: "hello"}}, nil, nil, &out)
	if err != nil {
		t.Fatalf("chatWithTools: %v", err)
	}
	if out.String() != "Hi there." {
		t.Fatalf("want %q, got %q", "Hi there.", out.String())
	}
}

func TestChatWithToolsDirectAnswerSkipsToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the non-streaming decision round should ever be hit — the
		// model answers directly without requesting a tool.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "No query needed."}}]}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "query_mssql", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	var out bytes.Buffer
	err := client.chatWithTools(context.Background(), "system", []chatMsg{{Role: "user", Content: "hi"}}, tools, nil, &out)
	if err != nil {
		t.Fatalf("chatWithTools: %v", err)
	}
	if out.String() != "No query needed." {
		t.Fatalf("want %q, got %q", "No query needed.", out.String())
	}
}

func TestChatWithToolsRoundTrip(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request %d: %v", calls, err)
		}
		if calls == 1 {
			if req.Stream {
				t.Errorf("expected the tool-decision round to be non-streaming")
			}
			if len(req.Tools) != 1 || req.Tools[0].Function.Name != "query_mssql" {
				t.Errorf("expected the query_mssql tool in the first request, got %+v", req.Tools)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "query_mssql", "arguments": "{\"query\":\"SELECT 1\"}"}}
			]}}]}`))
			return
		}

		if !req.Stream {
			t.Errorf("expected the final-answer round to be streaming")
		}
		var foundTool bool
		for _, m := range req.Messages {
			if m.Role == "tool" && m.ToolCallID == "call_1" {
				foundTool = true
				if !strings.Contains(m.Content, "42") {
					t.Errorf("expected the tool result message to contain the executor's output, got %q", m.Content)
				}
			}
		}
		if !foundTool {
			t.Errorf("expected a tool-role message with tool_call_id=call_1 in the second request, got %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"The answer is 42.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "query_mssql", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	executors := map[string]toolExecutor{
		"query_mssql": func(ctx context.Context, argsJSON string) (string, error) {
			var args mssqlToolArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return "", err
			}
			if args.Query != "SELECT 1" {
				t.Errorf("want query %q, got %q", "SELECT 1", args.Query)
			}
			return "result: 42", nil
		},
	}

	var out bytes.Buffer
	err := client.chatWithTools(context.Background(), "system prompt", []chatMsg{{Role: "user", Content: "what is the answer?"}}, tools, executors, &out)
	if err != nil {
		t.Fatalf("chatWithTools: %v", err)
	}
	if out.String() != "The answer is 42." {
		t.Fatalf("want %q, got %q", "The answer is 42.", out.String())
	}
	if calls != 2 {
		t.Fatalf("want 2 HTTP calls (decision + final answer), got %d", calls)
	}
}

func TestChatWithToolsUnknownToolName(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "does_not_exist", "arguments": "{}"}}
			]}}]}`))
			return
		}
		var req chatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		var toolMsg chatMsg
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolMsg = m
			}
		}
		if !strings.Contains(toolMsg.Content, "unknown tool") {
			t.Errorf("expected an 'unknown tool' error result fed back to the model, got %q", toolMsg.Content)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "query_mssql", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	var out bytes.Buffer
	if err := client.chatWithTools(context.Background(), "system", []chatMsg{{Role: "user", Content: "hi"}}, tools, map[string]toolExecutor{}, &out); err != nil {
		t.Fatalf("chatWithTools: %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

// TestChatWithToolsBudgetMultiRound covers the agent loop's core promise:
// the model may call tools across SEVERAL rounds (search → refine →
// answer), not just once like the plain-chat wrapper.
func TestChatWithToolsBudgetMultiRound(t *testing.T) {
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		w.Header().Set("Content-Type", "application/json")
		switch chatCalls {
		case 1, 2: // two consecutive tool rounds
			fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_%d", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"q%d\"}"}}
			]}}]}`, chatCalls, chatCalls)
		default: // third round: direct answer, no tools requested
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Fertig nach zwei Suchen."}}]}`))
		}
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	var execCalls int
	executors := map[string]toolExecutor{
		"search": func(ctx context.Context, argsJSON string) (string, error) {
			execCalls++
			return "result " + argsJSON, nil
		},
	}

	var out bytes.Buffer
	if err := client.chatWithToolsBudget(context.Background(), "system", []chatMsg{{Role: "user", Content: "task"}}, tools, executors, &out, 6); err != nil {
		t.Fatalf("chatWithToolsBudget: %v", err)
	}
	if out.String() != "Fertig nach zwei Suchen." {
		t.Fatalf("want the final answer, got %q", out.String())
	}
	if execCalls != 2 {
		t.Fatalf("want the tool executed in both rounds, got %d", execCalls)
	}
	if chatCalls != 3 {
		t.Fatalf("want 3 decision round-trips, got %d", chatCalls)
	}
}

// TestChatWithToolsBudgetExhaustionForcesAnswer: a model that never stops
// asking for tools hits the round budget and is forced to answer — with
// no tools offered on the final call, so it cannot stall again.
func TestChatWithToolsBudgetExhaustionForcesAnswer(t *testing.T) {
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		var req chatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			// The forced final answer arrives as the streaming call —
			// crucially with NO tools in the request.
			if len(req.Tools) != 0 {
				t.Errorf("forced final call must offer no tools, got %d", len(req.Tools))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Erzwungene Antwort.\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Different arguments each round (chatCalls baked into the query)
		// deliberately, not "{}" both times — otherwise this would also
		// exercise chatWithToolsBudget's cross-round result cache (see
		// TestChatWithToolsBudgetCachesRepeatedCallAcrossRounds), which
		// isn't what this test is about.
		fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
			{"id": "call_%d", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"round%d\"}"}}
		]}}]}`, chatCalls, chatCalls)
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	var execCalls int
	executors := map[string]toolExecutor{
		"search": func(ctx context.Context, argsJSON string) (string, error) { execCalls++; return "r", nil },
	}

	var out bytes.Buffer
	if err := client.chatWithToolsBudget(context.Background(), "system", []chatMsg{{Role: "user", Content: "task"}}, tools, executors, &out, 2); err != nil {
		t.Fatalf("chatWithToolsBudget: %v", err)
	}
	if out.String() != "Erzwungene Antwort." {
		t.Fatalf("want the forced answer, got %q", out.String())
	}
	if execCalls != 2 {
		t.Fatalf("want exactly maxRounds tool executions, got %d", execCalls)
	}
}

// TestChatWithToolsBudgetRunsOneRoundConcurrently checks that multiple tool
// calls requested in the same round (a real, common case — see OpenAI's own
// "assume the model may call several functions in one turn" guidance) run
// concurrently, not one after another: two 150ms executors must together
// take well under their sequential sum (300ms).
func TestChatWithToolsBudgetRunsOneRoundConcurrently(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "tool_calls": [
			{"id": "call_a", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"a\"}"}},
			{"id": "call_b", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"b\"}"}}
		]}}]}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	executors := map[string]toolExecutor{
		"search": func(ctx context.Context, argsJSON string) (string, error) {
			time.Sleep(150 * time.Millisecond)
			return "result " + argsJSON, nil
		},
	}

	// maxRounds=1 forces the loop straight to the budget-exhausted path
	// after this one round — irrelevant to what's being measured here
	// (the two calls *within* that round), just keeps the test to a
	// single HTTP round-trip.
	start := time.Now()
	var out bytes.Buffer
	if err := client.chatWithToolsBudget(context.Background(), "system", []chatMsg{{Role: "user", Content: "task"}}, tools, executors, &out, 1); err != nil {
		t.Fatalf("chatWithToolsBudget: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("want the two 150ms tool calls to run concurrently (well under 300ms total), took %v", elapsed)
	}
}

// TestRunToolCallsCapsParallelism keeps the latency benefit of concurrent
// lookups while proving that one model response cannot exhaust an upstream
// connection pool with arbitrary fan-out.
func TestRunToolCallsCapsParallelism(t *testing.T) {
	client := newLMClientFull("local", "http://unused.invalid", "", "embed", "chat", "")
	calls := make([]toolCall, toolCallsPerRoundLimit)
	for i := range calls {
		calls[i] = nativeToolCall(fmt.Sprintf("call_%d", i), "lookup", fmt.Sprintf(`{"id":%d}`, i))
	}
	var inFlight, maxInFlight int32
	exec := func(context.Context, string) (string, error) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			seen := atomic.LoadInt32(&maxInFlight)
			if n <= seen || atomic.CompareAndSwapInt32(&maxInFlight, seen, n) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return "ok", nil
	}

	out := client.runToolCalls(context.Background(), 0, calls, map[string]toolExecutor{"lookup": exec}, map[string]string{})
	if len(out) != len(calls) {
		t.Fatalf("want %d tool results, got %d", len(calls), len(out))
	}
	if got := atomic.LoadInt32(&maxInFlight); got != toolCallsParallelism {
		t.Fatalf("want concurrency cap to be reached at %d, got %d", toolCallsParallelism, got)
	}
}

func TestValidateToolCallsRejectsOversizedOrExcessiveResponses(t *testing.T) {
	tooMany := chatMsg{ToolCalls: make([]toolCall, toolCallsPerRoundLimit+1)}
	if _, err := validateToolCalls(tooMany); err == nil {
		t.Fatal("want too-many-calls response to be rejected")
	}

	oversized := chatMsg{ToolCalls: []toolCall{nativeToolCall("call_1", "lookup", strings.Repeat("x", toolArgumentsMaxBytes+1))}}
	if _, err := validateToolCalls(oversized); err == nil {
		t.Fatal("want oversized tool arguments to be rejected")
	}
}

// TestChatWithToolsBudgetDeadlineStopsGracefully: a model that never stops
// asking for tools, but with a deadline already in the past, must be
// forced to a final answer (no tools offered) on the very first round —
// the same graceful stop chatWithToolsBudget's round-exhaustion path uses,
// not a hard context-cancellation error.
func TestChatWithToolsBudgetDeadlineStopsGracefully(t *testing.T) {
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		var req chatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			if len(req.Tools) != 0 {
				t.Errorf("forced final call must offer no tools, got %d", len(req.Tools))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Zeit abgelaufen.\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
			{"id": "call_%d", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"round%d\"}"}}
		]}}]}`, chatCalls, chatCalls)
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	var execCalls int
	executors := map[string]toolExecutor{
		"search": func(ctx context.Context, argsJSON string) (string, error) { execCalls++; return "r", nil },
	}

	var out bytes.Buffer
	deadline := time.Now().Add(-time.Minute) // already past
	err := client.chatWithToolsBudgetDeadline(context.Background(), "system", []chatMsg{{Role: "user", Content: "task"}}, tools, executors, &out, 6, deadline)
	if err != nil {
		t.Fatalf("chatWithToolsBudgetDeadline: %v", err)
	}
	if out.String() != "Zeit abgelaufen." {
		t.Fatalf("want the forced answer, got %q", out.String())
	}
	if execCalls != 0 {
		t.Fatalf("want zero tool executions (deadline already past before round 0), got %d", execCalls)
	}
}

// TestChatWithToolsBudgetCachesRepeatedCallAcrossRounds checks the
// cross-round result cache: a model asking for the exact same tool+
// arguments a second time (in a later round) gets the cached result
// without the executor running again.
func TestChatWithToolsBudgetCachesRepeatedCallAcrossRounds(t *testing.T) {
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		w.Header().Set("Content-Type", "application/json")
		switch chatCalls {
		case 1, 2: // same call, twice, in two separate rounds
			fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_%d", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"same\"}"}}
			]}}]}`, chatCalls)
		default:
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "fertig"}}]}`))
		}
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	var execCalls int
	executors := map[string]toolExecutor{
		"search": func(ctx context.Context, argsJSON string) (string, error) {
			execCalls++
			return "result", nil
		},
	}

	var out bytes.Buffer
	if err := client.chatWithToolsBudget(context.Background(), "system", []chatMsg{{Role: "user", Content: "task"}}, tools, executors, &out, 6); err != nil {
		t.Fatalf("chatWithToolsBudget: %v", err)
	}
	if out.String() != "fertig" {
		t.Fatalf("want the final answer, got %q", out.String())
	}
	if execCalls != 1 {
		t.Fatalf("want the identical repeated call executed only once (cached the second time), got %d executions", execCalls)
	}
}

func TestEmbedRetriesOnTransientFailureThenSucceeds(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"embedding": [0.1, 0.2]}]}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "test-embed", "test-chat", "")
	vec, err := client.embedSingle("hello")
	if err != nil {
		t.Fatalf("embedSingle: want success after transient failures, got: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("want a 2-dim embedding, got %v", vec)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls (2 failed + 1 success), got %d", calls)
	}
}

func TestEmbedDoesNotRetryOnClientError(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "unknown model"}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "test-embed", "test-chat", "")
	if _, err := client.embedSingle("hello"); err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if calls != 1 {
		t.Fatalf("want exactly 1 call (a 4xx is not transient, should not retry), got %d", calls)
	}
}

// TestChatWithToolsBudgetClarificationShortCircuits covers the core
// promise of the ask_clarifying_question tool: calling it ends the loop
// immediately with *ErrClarificationNeeded, WITHOUT a further HTTP
// round-trip to the model and without executing the tool via the normal
// executor map (there's deliberately no "ask_clarifying_question" entry
// in executors here, proving chatWithToolsBudget itself intercepts it by
// name rather than relying on the caller to register an executor).
func TestChatWithToolsBudgetClarificationShortCircuits(t *testing.T) {
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "tool_calls": [
			{"id": "call_1", "type": "function", "function": {"name": "ask_clarifying_question", "arguments": "{\"question\":\"Welche Bestellung meinst du?\",\"options\":[\"Bestellung A\",\"Bestellung B\"]}"}}
		]}}]}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "ask_clarifying_question", Description: "d", Parameters: map[string]any{"type": "object"}}}}

	var out bytes.Buffer
	err := client.chatWithToolsBudget(context.Background(), "system", []chatMsg{{Role: "user", Content: "Storniere die Bestellung"}}, tools, map[string]toolExecutor{}, &out, 6)
	if err == nil {
		t.Fatal("expected an ErrClarificationNeeded, got nil")
	}
	var clar *ErrClarificationNeeded
	if !errors.As(err, &clar) {
		t.Fatalf("expected *ErrClarificationNeeded, got %T: %v", err, err)
	}
	if clar.Question != "Welche Bestellung meinst du?" {
		t.Fatalf("want the question passed through, got %q", clar.Question)
	}
	if len(clar.Options) != 2 || clar.Options[0] != "Bestellung A" || clar.Options[1] != "Bestellung B" {
		t.Fatalf("want both options passed through, got %+v", clar.Options)
	}
	if out.Len() != 0 {
		t.Fatalf("expected nothing written to w on a clarification, got %q", out.String())
	}
	if chatCalls != 1 {
		t.Fatalf("want exactly 1 decision round-trip (no further round after the clarification), got %d", chatCalls)
	}
}

func TestClarificationFromCallsIgnoresMalformedOrEmptyQuestion(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"invalid JSON", `not json`},
		{"empty question", `{"question":"  "}`},
		{"missing question", `{"options":["a","b"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tc toolCall
			tc.ID = "call_1"
			tc.Type = "function"
			tc.Function.Name = "ask_clarifying_question"
			tc.Function.Arguments = c.args
			if got := clarificationFromCalls([]toolCall{tc}); got != nil {
				t.Fatalf("want nil for %s, got %+v", c.name, got)
			}
		})
	}
}

func TestClarificationFromCallsIgnoresOtherToolNames(t *testing.T) {
	var tc toolCall
	tc.ID = "call_1"
	tc.Type = "function"
	tc.Function.Name = "search_knowledge_base"
	tc.Function.Arguments = `{"query":"x"}`
	if got := clarificationFromCalls([]toolCall{tc}); got != nil {
		t.Fatalf("want nil when no call is ask_clarifying_question, got %+v", got)
	}
}

// TestChatOnceRecordsAzureCachedPromptTokens covers the non-streaming path
// (every Agent-mode tool-decision round): Azure OpenAI / OpenAI report the
// portion of the prompt served from their automatic server-side cache in
// usage.prompt_tokens_details.cached_tokens. That portion must be recorded
// DISJOINT from prompt_tokens (matching the Claude path), so the token-usage
// analytics show the automatic prompt-cache savings instead of billing them
// as full-price prompt tokens.
func TestChatOnceRecordsAzureCachedPromptTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "ok"}}],
			"usage": {"prompt_tokens": 100, "completion_tokens": 20, "prompt_tokens_details": {"cached_tokens": 80}}
		}`))
	}))
	defer server.Close()

	client := newLMClientFull("azure", server.URL, "2024-10-21", "embed", "gpt-4o", "k")
	ctx, trace := withTokenUsage(context.Background(), "alice@rubix.com", "ask")
	if _, err := client.chatOnce(ctx, []chatMsg{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("chatOnce: %v", err)
	}
	events := trace.snapshot()
	if len(events) != 1 {
		t.Fatalf("want 1 usage event, got %d: %+v", len(events), events)
	}
	e := events[0]
	if e.PromptTokens != 20 || e.CompletionTokens != 20 || e.CacheReadInputTokens != 80 {
		t.Fatalf("want prompt=20 (100-80 cached) completion=20 cache_read=80, got %+v", e)
	}
}

// TestChatOnceLocalWithoutCachedTokensUnchanged is the other half: a local
// OpenAI-compatible server (LM Studio, Ollama) that omits prompt_tokens_details
// entirely must record prompt_tokens verbatim, with zero cache-read — nothing
// subtracted, no behavior change from before cached-token capture existed.
func TestChatOnceLocalWithoutCachedTokensUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "ok"}}],
			"usage": {"prompt_tokens": 100, "completion_tokens": 20}
		}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	ctx, trace := withTokenUsage(context.Background(), "anonym", "ask")
	if _, err := client.chatOnce(ctx, []chatMsg{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("chatOnce: %v", err)
	}
	events := trace.snapshot()
	if len(events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(events))
	}
	if e := events[0]; e.PromptTokens != 100 || e.CompletionTokens != 20 || e.CacheReadInputTokens != 0 {
		t.Fatalf("want prompt=100 completion=20 cache_read=0 (no details reported), got %+v", e)
	}
}

// TestChatStreamMessagesRecordsCachedPromptTokens covers the streaming path
// (the Agent tab's forced final answer, and plain streamed chat): the
// trailing stream_options.include_usage chunk carries the same
// prompt_tokens_details.cached_tokens split, recorded disjoint from
// prompt_tokens just like the non-streaming path.
func TestChatStreamMessagesRecordsCachedPromptTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Antwort.\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":40,\"prompt_tokens_details\":{\"cached_tokens\":450}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := newLMClientFull("azure", server.URL, "2024-10-21", "embed", "gpt-4o", "k")
	ctx, trace := withTokenUsage(context.Background(), "alice@rubix.com", "ask")
	var out bytes.Buffer
	if err := client.chatStreamMessages(ctx, []chatMsg{{Role: "system", Content: "s"}, {Role: "user", Content: "hi"}}, &out); err != nil {
		t.Fatalf("chatStreamMessages: %v", err)
	}
	if out.String() != "Antwort." {
		t.Fatalf("want streamed content %q, got %q", "Antwort.", out.String())
	}
	events := trace.snapshot()
	if len(events) != 1 {
		t.Fatalf("want 1 usage event (real usage chunk, not the estimate fallback), got %d: %+v", len(events), events)
	}
	if e := events[0]; e.PromptTokens != 50 || e.CompletionTokens != 40 || e.CacheReadInputTokens != 450 || e.Estimated {
		t.Fatalf("want prompt=50 (500-450) completion=40 cache_read=450 estimated=false, got %+v", e)
	}
}
