package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestResolveExecutionTier(t *testing.T) {
	cases := []struct {
		name       string
		tier       string
		mode       string
		wantName   string
		wantPrompt string
		wantLive   bool
		wantAgent  bool
		wantRounds int
	}{
		{"instant", "instant", "", "instant", "", false, false, 0},
		{"standard explicit", "standard", "", "standard", "", true, false, 1},
		{"balanced alias", "balanced", "", "standard", "", true, false, 1},
		{"auto alias", "auto", "", "standard", "", true, false, 1},
		{"agent explicit", "agent", "", "agent", "agent", true, true, agentRoundsSentinel},
		{"tier wins over mode", "instant", "agent", "instant", "", false, false, 0},
		{"empty tier falls back to mode agent", "", "agent", "agent", "agent", true, true, agentRoundsSentinel},
		{"empty tier empty mode = standard", "", "", "standard", "", true, false, 1},
		{"unknown tier falls back to mode", "turbo", "agent", "agent", "agent", true, true, agentRoundsSentinel},
		{"unknown tier no mode = standard", "turbo", "", "standard", "", true, false, 1},
		{"case/space insensitive", "  Instant ", "", "instant", "", false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveExecutionTier(c.tier, c.mode)
			if got.Name != c.wantName {
				t.Errorf("Name: got %q, want %q", got.Name, c.wantName)
			}
			if got.PromptMode != c.wantPrompt {
				t.Errorf("PromptMode: got %q, want %q", got.PromptMode, c.wantPrompt)
			}
			if got.LiveTools != c.wantLive {
				t.Errorf("LiveTools: got %v, want %v", got.LiveTools, c.wantLive)
			}
			if got.AgentTools != c.wantAgent {
				t.Errorf("AgentTools: got %v, want %v", got.AgentTools, c.wantAgent)
			}
			if got.Rounds != c.wantRounds {
				t.Errorf("Rounds: got %d, want %d", got.Rounds, c.wantRounds)
			}
		})
	}
}

// TestExecutionTierInvariants pins the two safety-relevant properties the
// rest of handleAsk relies on: instant never offers tools or rounds (pure
// RAG, max speed), and only the agent tier pulls in the agentic tool set.
func TestExecutionTierInvariants(t *testing.T) {
	if tierInstant.LiveTools || tierInstant.AgentTools || tierInstant.Rounds != 0 {
		t.Fatalf("instant tier must be pure RAG (no tools, 0 rounds): %+v", tierInstant)
	}
	if !tierStandard.LiveTools || tierStandard.AgentTools {
		t.Fatalf("standard tier must offer live tools but not agentic tools: %+v", tierStandard)
	}
	if !tierAgent.AgentTools || tierAgent.PromptMode != "agent" {
		t.Fatalf("agent tier must use the agent prompt and agentic tools: %+v", tierAgent)
	}
}

// TestHandleAskTierControlsToolExposure drives handleAsk end-to-end with a
// mock LLM that records how many tools each request offered, proving the
// tier actually gates tool exposure: instant offers none (pure RAG stream),
// standard offers the live tool(s), and agent offers strictly more (the
// agentic knowledge-base tools on top).
func TestHandleAskTierControlsToolExposure(t *testing.T) {
	var mu sync.Mutex
	var sawStream bool
	var maxTools int
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool              `json:"stream"`
			Tools  []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		if req.Stream {
			sawStream = true
		}
		if len(req.Tools) > maxTools {
			maxTools = len(req.Tools)
		}
		mu.Unlock()
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Antwort.\"}}]}\n\ndata: [DONE]\n\n"))
			return
		}
		// Non-streaming tool-decision round: answer directly (no tool_calls)
		// so the loop ends after one round without executing anything.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Antwort."}}]}`))
	}))
	defer llm.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", llm.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.Profiles.Local.EmbedModel = "test-embed"
	// A live tool must be configured so "standard"/"agent" have something to
	// offer and "instant" has something to withhold.
	s.MSSQL.Enabled = true
	s.MSSQL.AllowGenericQuery = true
	s.MSSQL.Database = "testdb"
	withTestGlobalSettings(t, s)

	handler := handleAsk(rag)

	run := func(tier string) (int, bool) {
		mu.Lock()
		sawStream, maxTools = false, 0
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"question": "hallo?", "format": "json", "tier": tier})
		r := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("tier %q: status %d, body %s", tier, w.Code, w.Body.String())
		}
		mu.Lock()
		defer mu.Unlock()
		return maxTools, sawStream
	}

	instantTools, instantStream := run("instant")
	if instantTools != 0 {
		t.Errorf("instant tier must offer no tools, got %d", instantTools)
	}
	if !instantStream {
		t.Errorf("instant tier must stream a pure-RAG answer (no tool-decision round)")
	}

	standardTools, _ := run("standard")
	if standardTools < 1 {
		t.Errorf("standard tier must offer at least the live MSSQL tool, got %d", standardTools)
	}

	agentTools, _ := run("agent")
	if agentTools <= standardTools {
		t.Errorf("agent tier must offer strictly more tools than standard (adds knowledge-base tools); agent=%d standard=%d", agentTools, standardTools)
	}
}
