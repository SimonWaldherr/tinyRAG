package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestSubAgentOrchestration drives delegate_subtasks end to end against a
// fake chat backend: two sub-tasks fan out, each sub-agent answers, and the
// combined result carries both labelled answers. Also asserts the live
// progress stream reports a subagent_start/subagent_end pair per task.
func TestSubAgentOrchestration(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		// Each sub-agent answers directly (no tools) — the first
		// (non-streaming) chatOnce returns content, ending its loop.
		_, _ = w.Write([]byte(fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":"Teilantwort %d"}}]}`, n)))
	}))
	defer srv.Close()

	rag, s := newTestRAG(t)
	chat := newLMClientFull("local", srv.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chat}, "local")
	s.ChatProfile = "local"
	withTestGlobalSettings(t, s)

	// Capture the live progress steps.
	var mu sync.Mutex
	var steps []agentStep
	ctx := withAgentProgress(context.Background(), func(st agentStep) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, st)
	})

	exec := subAgentToolExecutor(rag, s, agentSession{User: "t", IsAdmin: true})
	args, _ := json.Marshal(map[string]any{
		"subtasks": []map[string]string{
			{"label": "Status", "task": "Wie ist der Status?"},
			{"label": "Historie", "task": "Was ist die Historie?"},
		},
	})
	out, err := exec(ctx, string(args))
	if err != nil {
		t.Fatalf("delegate_subtasks: %v", err)
	}
	if !strings.Contains(out, "### Status") || !strings.Contains(out, "### Historie") {
		t.Fatalf("combined result missing a labelled section:\n%s", out)
	}
	if !strings.Contains(out, "Teilantwort") {
		t.Fatalf("combined result missing a sub-agent answer:\n%s", out)
	}

	mu.Lock()
	defer mu.Unlock()
	starts, ends := 0, 0
	startIDByAgent := map[string]string{}
	seenIDs := map[string]bool{}
	for _, st := range steps {
		if st.ID == "" {
			t.Errorf("every step must carry a non-empty ID, got %+v", st)
		}
		if seenIDs[st.ID] && st.Type != "subagent_end" {
			t.Errorf("step ID %q reused unexpectedly by a non-matching-end step: %+v", st.ID, st)
		}
		switch st.Type {
		case "subagent_start":
			starts++
			if st.Agent == "" {
				t.Error("subagent_start step must carry a label")
			}
			startIDByAgent[st.Agent] = st.ID
			seenIDs[st.ID] = true
		case "subagent_end":
			ends++
			// A subagent_end must reuse its subagent_start's own ID (so the
			// frontend/graph view can match the pair directly by ID) rather
			// than minting a fresh one.
			if want := startIDByAgent[st.Agent]; want != "" && st.ID != want {
				t.Errorf("subagent_end for %q: want ID %q (matching its subagent_start), got %q", st.Agent, want, st.ID)
			}
		}
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("want 2 subagent_start + 2 subagent_end steps, got %d/%d", starts, ends)
	}
	if startIDByAgent["Status"] == "" || startIDByAgent["Historie"] == "" {
		t.Fatalf("want both sub-agents to have their own recorded start ID, got %+v", startIDByAgent)
	}
	if startIDByAgent["Status"] == startIDByAgent["Historie"] {
		t.Fatalf("want distinct IDs for concurrently-running sub-agents, both got %q", startIDByAgent["Status"])
	}
}

// TestAgentStepParentIDNesting is a focused unit test of the ID/ParentID
// propagation mechanism itself (agentProgress.send/withSubAgentLabel,
// llm.go) — the foundation both the Debug panel's per-call attribution and
// a graphical agent/sub-agent/tool-call hierarchy view rely on. Verifies:
// a start/end pair sharing one ID, a nested sub-agent scope's own steps
// carrying that scope's start ID as ParentID, and sibling top-level steps
// never acquiring a ParentID they weren't given.
func TestAgentStepParentIDNesting(t *testing.T) {
	var steps []agentStep
	ctx := withAgentProgress(context.Background(), func(st agentStep) {
		steps = append(steps, st)
	})
	prog := agentProgressFromContext(ctx)

	rootID := prog.send(agentStep{Type: "tool_start", Tool: "search_knowledge_base"})
	prog.send(agentStep{ID: rootID, Type: "tool_end", Tool: "search_knowledge_base"})

	subStartID := prog.send(agentStep{Type: "subagent_start", Agent: "Teilaufgabe 1"})
	subCtx := withSubAgentLabel(ctx, "Teilaufgabe 1", subStartID)
	subProg := agentProgressFromContext(subCtx)
	nestedID := subProg.send(agentStep{Type: "tool_start", Tool: "search_knowledge_base"})
	subProg.send(agentStep{ID: nestedID, Type: "tool_end", Tool: "search_knowledge_base"})
	prog.send(agentStep{ID: subStartID, Type: "subagent_end", Agent: "Teilaufgabe 1"})

	if len(steps) != 6 {
		t.Fatalf("want 6 recorded steps (root start+end, subagent start, nested start+end, subagent end), got %d: %+v", len(steps), steps)
	}
	byID := map[string][]agentStep{}
	for _, s := range steps {
		if s.ID == "" {
			t.Errorf("every step must have a non-empty ID: %+v", s)
		}
		if s.StartedAt == 0 {
			t.Errorf("every step must have a non-zero StartedAt: %+v", s)
		}
		byID[s.ID] = append(byID[s.ID], s)
	}
	if len(byID) != 3 {
		t.Fatalf("want 3 distinct IDs (root call, sub-agent scope, nested call), got %d: %v", len(byID), byID)
	}
	if rootID == subStartID || rootID == nestedID || subStartID == nestedID {
		t.Fatalf("want three distinct IDs, got rootID=%q subStartID=%q nestedID=%q", rootID, subStartID, nestedID)
	}
	for _, s := range byID[rootID] {
		if s.ParentID != "" {
			t.Errorf("root-level tool call must have no ParentID, got %q on %+v", s.ParentID, s)
		}
	}
	for _, s := range byID[subStartID] {
		if s.ParentID != "" {
			t.Errorf("top-level subagent_start/end must have no ParentID, got %q on %+v", s.ParentID, s)
		}
	}
	for _, s := range byID[nestedID] {
		if s.ParentID != subStartID {
			t.Errorf("nested tool call must have ParentID %q (its enclosing sub-agent scope), got %q on %+v", subStartID, s.ParentID, s)
		}
	}
}

// TestAgentLimitAccessors covers the default/clamp behaviour of the
// configurable sub-agent management knobs.
func TestAgentLimitAccessors(t *testing.T) {
	// Unset → defaults.
	if got := agentMaxSubtasks(agentConfig{}); got != subAgentDefaultMaxTasks {
		t.Errorf("max subtasks default: want %d, got %d", subAgentDefaultMaxTasks, got)
	}
	if got := agentSubagentRounds(agentConfig{}); got != subAgentDefaultRounds {
		t.Errorf("subagent rounds default: want %d, got %d", subAgentDefaultRounds, got)
	}
	if got := agentConcurrency(agentConfig{}); got != agentDefaultConcurrency {
		t.Errorf("concurrency default: want %d, got %d", agentDefaultConcurrency, got)
	}
	// Explicit honored, over-ceiling clamped.
	if got := agentMaxSubtasks(agentConfig{MaxSubtasks: 2}); got != 2 {
		t.Errorf("explicit max subtasks: want 2, got %d", got)
	}
	if got := agentMaxSubtasks(agentConfig{MaxSubtasks: 999}); got != subAgentMaxTasksCeiling {
		t.Errorf("max subtasks ceiling: want %d, got %d", subAgentMaxTasksCeiling, got)
	}
	if got := agentConcurrency(agentConfig{MaxConcurrency: 999}); got != agentConcurrencyCeiling {
		t.Errorf("concurrency ceiling: want %d, got %d", agentConcurrencyCeiling, got)
	}
}

// TestSubAgentRespectsMaxSubtasks confirms a configured MaxSubtasks caps
// how many sub-agents actually run even if the model asks for more.
func TestSubAgentRespectsMaxSubtasks(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	rag, s := newTestRAG(t)
	chat := newLMClientFull("local", srv.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chat}, "local")
	s.ChatProfile = "local"
	s.Agent.MaxSubtasks = 2
	withTestGlobalSettings(t, s)

	exec := subAgentToolExecutor(rag, s, agentSession{User: "t", IsAdmin: true})
	args, _ := json.Marshal(map[string]any{"subtasks": []map[string]string{
		{"task": "a"}, {"task": "b"}, {"task": "c"}, {"task": "d"},
	}})
	out, err := exec(context.Background(), string(args))
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	// Exactly 2 sub-agents → exactly 2 backend calls (each answers in one round).
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("want 2 sub-agent runs (capped from 4), got %d", got)
	}
	if strings.Count(out, "### ") != 2 {
		t.Fatalf("want 2 labelled result sections, got:\n%s", out)
	}
}

// TestSubAgentRejectsNesting proves the recursion guard: a delegate call
// made from within a sub-agent context is refused, so sub-agents can't
// spawn sub-agents.
func TestSubAgentRejectsNesting(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)
	exec := subAgentToolExecutor(rag, s, agentSession{User: "t"})
	nested := context.WithValue(context.Background(), subAgentDepthKey{}, true)
	args, _ := json.Marshal(map[string]any{"subtasks": []map[string]string{{"task": "x"}}})
	if _, err := exec(nested, string(args)); err == nil {
		t.Fatal("want an error when delegate_subtasks is called from within a sub-agent")
	}
}

// TestSubAgentToolGatedBySetting confirms the orchestration tool is offered
// by default and withheld when SubagentsDisabled is set.
func TestSubAgentToolGatedBySetting(t *testing.T) {
	rag, s := newTestRAG(t)
	sess := agentSession{User: "t", IsAdmin: true}

	has := func(tools []toolDef) bool {
		for _, td := range tools {
			if td.Function.Name == subAgentToolName {
				return true
			}
		}
		return false
	}

	tools, _ := buildAgentTools(rag, s, sess)
	if !has(tools) {
		t.Fatal("delegate_subtasks should be offered by default")
	}

	s.Agent.SubagentsDisabled = true
	tools, _ = buildAgentTools(rag, s, sess)
	if has(tools) {
		t.Fatal("delegate_subtasks must be withheld when SubagentsDisabled is set")
	}

	// A sub-agent's own tool set must never include the delegate tool.
	subTools, _ := buildSubAgentTools(rag, s, sess, sourcePreset{})
	if has(subTools) {
		t.Fatal("sub-agents must never get the delegate tool (recursion guard)")
	}
}
