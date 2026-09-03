package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDebugModeAllowed(t *testing.T) {
	t.Run("no session: false", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if debugModeAllowed(r) {
			t.Fatal("want false with no session cookie")
		}
	})

	t.Run("session for a non-admin user: false", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "someone.else", IsAdmin: false})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		if debugModeAllowed(r) {
			t.Fatal("want false for a non-admin session")
		}
	})

	t.Run("session for an admin: true", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "Any Admin", IsAdmin: true})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		if !debugModeAllowed(r) {
			t.Fatal("want true for any admin session, not just one hardcoded user")
		}
	})
}

// TestChatWithToolsBudgetPopulatesDebugTraceViaContext is the core
// guarantee behind "Debug-Modus": wrapping ctx with withDebugTrace makes
// chatWithToolsBudget/runToolCalls record the exact message sequence sent
// to the model and every tool call made, with no change to the function's
// normal behavior or signature.
func TestChatWithToolsBudgetPopulatesDebugTraceViaContext(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"q1\"}"}}
			]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Fertig."}}]}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	executors := map[string]toolExecutor{
		"search": func(ctx context.Context, argsJSON string) (string, error) {
			return "result for " + argsJSON, nil
		},
	}

	ctx, dt := withDebugTrace(context.Background())
	dt.RetrievedChunks = []rankedHit{{SourceID: "doc-1", Content: "chunk text"}}

	var out bytes.Buffer
	if err := client.chatWithToolsBudget(ctx, "system prompt", []chatMsg{{Role: "user", Content: "task"}}, tools, executors, &out, 6); err != nil {
		t.Fatalf("chatWithToolsBudget: %v", err)
	}
	if out.String() != "Fertig." {
		t.Fatalf("want the final answer written to w, got %q", out.String())
	}

	if len(dt.RetrievedChunks) != 1 || dt.RetrievedChunks[0].SourceID != "doc-1" {
		t.Fatalf("want RetrievedChunks set by the caller to survive untouched, got %+v", dt.RetrievedChunks)
	}
	if len(dt.ToolCalls) != 1 {
		t.Fatalf("want exactly 1 recorded tool call, got %+v", dt.ToolCalls)
	}
	tc := dt.ToolCalls[0]
	if tc.Name != "search" || tc.Arguments != `{"query":"q1"}` || tc.Result != `result for {"query":"q1"}` || tc.Error != "" {
		t.Fatalf("unexpected recorded tool call: %+v", tc)
	}
	if tc.Round != 1 {
		t.Fatalf("want round 1, got %d", tc.Round)
	}

	// The final message sequence must include the system prompt, the
	// user turn, the assistant's tool-call message and the tool result —
	// i.e. exactly what was actually sent to the model on the last round.
	var foundSystem, foundTool bool
	for _, m := range dt.Messages {
		if m.Role == "system" && m.Content == "system prompt" {
			foundSystem = true
		}
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			foundTool = true
		}
	}
	if !foundSystem {
		t.Fatalf("want the system prompt present in dt.Messages, got %+v", dt.Messages)
	}
	if !foundTool {
		t.Fatalf("want the tool result message present in dt.Messages, got %+v", dt.Messages)
	}
}

// TestChatWithToolsBudgetWithoutDebugTraceIsANoOp guards that every debug*
// call is nil-receiver-safe: a plain context.Background() call site (every
// existing caller before this feature, and every caller other than the one
// recognized debug session) must behave exactly as before — no panic, no
// behavior change.
func TestChatWithToolsBudgetWithoutDebugTraceIsANoOp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}]}`))
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}

	var out bytes.Buffer
	if err := client.chatWithToolsBudget(context.Background(), "system", []chatMsg{{Role: "user", Content: "hi"}}, tools, map[string]toolExecutor{}, &out, 6); err != nil {
		t.Fatalf("chatWithToolsBudget without a debug trace in context: %v", err)
	}
	if out.String() != "ok" {
		t.Fatalf("want %q, got %q", "ok", out.String())
	}
}

// TestHandleAskIncludesDebugForRecognizedSession is an end-to-end check
// through handleAsk itself (format:"json", no tools, so chatOnce is never
// reached and the model always answers directly via chatStream) — the
// debug field must be present and populated for the recognized session,
// and absent for everyone else, using the exact same request otherwise.
func TestHandleAskIncludesDebugForRecognizedSession(t *testing.T) {
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Antwort.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer llmServer.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", llmServer.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.Profiles.Local.EmbedModel = "test-embed"
	withTestGlobalSettings(t, s)

	handler := handleAsk(rag)

	doRequest := func(cookies []*http.Cookie) map[string]any {
		body, _ := json.Marshal(map[string]any{"question": "hallo?", "format": "json"})
		r := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handler(w, r)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
		}
		return out
	}

	t.Run("no session: no debug field", func(t *testing.T) {
		out := doRequest(nil)
		if _, ok := out["debug"]; ok {
			t.Fatalf("want no debug field for an anonymous caller, got %+v", out)
		}
	})

	t.Run("admin session: debug field present and populated", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "Any Admin", IsAdmin: true})
		out := doRequest(w.Result().Cookies())
		dbg, ok := out["debug"].(map[string]any)
		if !ok {
			t.Fatalf("want a debug object in the response, got %+v", out)
		}
		if dbg["raw_answer"] != "Antwort." {
			t.Fatalf("want raw_answer %q, got %+v", "Antwort.", dbg["raw_answer"])
		}
		if _, ok := dbg["messages"]; !ok {
			t.Fatalf("want messages present in debug, got %+v", dbg)
		}
	})
}

// TestHandleAskDebugIncludesSelectedSkills confirms buildSystemPromptForMode's
// selected-skills result — previously only ever written to the server's
// verbose log, invisible anywhere in the UI — now reaches the Debug-Modus
// trace an admin session gets back from /api/ask.
func TestHandleAskDebugIncludesSelectedSkills(t *testing.T) {
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Antwort.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer llmServer.Close()

	promptsDir := t.TempDir()
	skillContent := "---\nname: PSA\ntags: [handschuh]\nenabled: true\n---\n\nSkill body.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "skill_ppe.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", llmServer.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.Profiles.Local.EmbedModel = "test-embed"
	s.PromptsDir = promptsDir
	withTestGlobalSettings(t, s)

	handler := handleAsk(rag)
	w := httptest.NewRecorder()
	issueSession(w, &ldapUser{CN: "Any Admin", IsAdmin: true})

	body, _ := json.Marshal(map[string]any{"question": "Welche Handschuhe brauche ich?", "format": "json"})
	r := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler(rec, r)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	dbg, ok := out["debug"].(map[string]any)
	if !ok {
		t.Fatalf("want a debug object in the response, got %+v", out)
	}
	skills, ok := dbg["selected_skills"].([]any)
	if !ok || len(skills) != 1 || skills[0] != "PSA" {
		t.Fatalf(`want selected_skills=["PSA"], got %+v`, dbg["selected_skills"])
	}
}
