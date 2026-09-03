package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCompactOldToolRoundsNoOpWithFewRounds confirms nothing is touched
// until MORE than keepRounds have completed — a short 1-2 round answer
// (the overwhelming common case) never pays for the scan at all.
func TestCompactOldToolRoundsNoOpWithFewRounds(t *testing.T) {
	all := []chatMsg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "call"},
		{Role: "tool", Name: "search", Content: strings.Repeat("x", 100000)},
	}
	roundStarts := []int{1}
	got, upTo := compactOldToolRounds(all, roundStarts, 0, 2, 100)
	if got[2].Content != strings.Repeat("x", 100000) {
		t.Fatalf("want the single round left untouched (only keepRounds=2 rounds exist), got %q", got[2].Content)
	}
	if upTo != 0 {
		t.Fatalf("want compactedUpTo unchanged at 0, got %d", upTo)
	}
}

// TestCompactOldToolRoundsNoOpBelowThreshold confirms nothing is touched
// while the accumulated size stays under thresholdChars, even with more
// than keepRounds completed.
func TestCompactOldToolRoundsNoOpBelowThreshold(t *testing.T) {
	all := []chatMsg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "call1"},
		{Role: "tool", Name: "search", Content: "short result 1"},
		{Role: "assistant", Content: "call2"},
		{Role: "tool", Name: "search", Content: "short result 2"},
		{Role: "assistant", Content: "call3"},
		{Role: "tool", Name: "search", Content: "short result 3"},
	}
	roundStarts := []int{1, 3, 5}
	got, upTo := compactOldToolRounds(all, roundStarts, 0, 2, 100000)
	for i, m := range all {
		if got[i].Content != m.Content {
			t.Fatalf("want message %d untouched below threshold, got %q want %q", i, got[i].Content, m.Content)
		}
	}
	if upTo != 0 {
		t.Fatalf("want compactedUpTo unchanged at 0, got %d", upTo)
	}
}

// TestCompactOldToolRoundsCompactsOlderRoundsOnly is the core behavior:
// once the threshold is crossed, round 1's tool result is compacted but
// the most recent keepRounds (2 and 3) are left completely untouched —
// and assistant messages (carrying the model's own tool_calls) are never
// touched, even in a compacted round.
func TestCompactOldToolRoundsCompactsOlderRoundsOnly(t *testing.T) {
	big := strings.Repeat("y", 500)
	all := []chatMsg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "call round 1"},
		{Role: "tool", Name: "search", Content: big},
		{Role: "assistant", Content: "call round 2"},
		{Role: "tool", Name: "search", Content: big},
		{Role: "assistant", Content: "call round 3"},
		{Role: "tool", Name: "search", Content: big},
	}
	roundStarts := []int{1, 3, 5}
	got, upTo := compactOldToolRounds(all, roundStarts, 0, 2, 1000)

	if got[1].Content != "call round 1" {
		t.Fatalf("assistant message must never be touched, got %q", got[1].Content)
	}
	if got[2].Content == big {
		t.Fatalf("want round 1's tool result compacted, got it unchanged")
	}
	if !strings.Contains(got[2].Content, "gekürzt") {
		t.Fatalf("want the placeholder marker in the compacted content, got %q", got[2].Content)
	}
	if got[4].Content != big || got[6].Content != big {
		t.Fatalf("want the most recent 2 rounds left verbatim, got round2=%q round3=%q", got[4].Content, got[6].Content)
	}
	if upTo != 3 {
		t.Fatalf("want compactedUpTo advanced to round 2's start (3) — the boundary of the last keepRounds=2 rounds — got %d", upTo)
	}
}

// TestCompactOldToolRoundsNeverRecompacts confirms a second call with the
// same compactedUpTo watermark doesn't re-touch (or double-shrink) an
// already-compacted range — the watermark must actually gate re-scanning.
func TestCompactOldToolRoundsNeverRecompacts(t *testing.T) {
	big := strings.Repeat("y", 500)
	all := []chatMsg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "call round 1"},
		{Role: "tool", Name: "search", Content: big},
		{Role: "assistant", Content: "call round 2"},
		{Role: "tool", Name: "search", Content: big},
		{Role: "assistant", Content: "call round 3"},
		{Role: "tool", Name: "search", Content: big},
	}
	roundStarts := []int{1, 3, 5}
	first, upTo1 := compactOldToolRounds(all, roundStarts, 0, 2, 1000)
	firstCompacted := first[2].Content

	second, upTo2 := compactOldToolRounds(first, roundStarts, upTo1, 2, 1000)
	if second[2].Content != firstCompacted {
		t.Fatalf("want the already-compacted message left alone on a second pass, got %q want %q", second[2].Content, firstCompacted)
	}
	if upTo2 != upTo1 {
		t.Fatalf("want compactedUpTo stable across a no-op second pass, got %d then %d", upTo1, upTo2)
	}
}

// TestContextCompactionFromContextDefaults confirms a context with no
// carried config (every caller except handleAsk) resolves to the built-in
// defaults, not zero-value ("thresholdChars=0" would wrongly mean
// "compact immediately", which contextCompactionFromContext must never
// produce for an unconfigured caller).
func TestContextCompactionFromContextDefaults(t *testing.T) {
	got := contextCompactionFromContext(context.Background())
	if got.disabled {
		t.Fatalf("want compaction enabled by default, got disabled=true")
	}
	if got.thresholdChars != contextCompactionDefaultThresholdChars {
		t.Fatalf("want default thresholdChars %d, got %d", contextCompactionDefaultThresholdChars, got.thresholdChars)
	}
	if got.keepRounds != contextCompactionDefaultKeepRounds {
		t.Fatalf("want default keepRounds %d, got %d", contextCompactionDefaultKeepRounds, got.keepRounds)
	}
}

// TestContextCompactionFromContextPartialOverride confirms an explicitly
// carried config with only some fields set (0 = "use default" per
// settings.go's convention) still fills in defaults for the rest.
func TestContextCompactionFromContextPartialOverride(t *testing.T) {
	ctx := withContextCompaction(context.Background(), contextCompactionConfig{thresholdChars: 5000})
	got := contextCompactionFromContext(ctx)
	if got.thresholdChars != 5000 {
		t.Fatalf("want the explicit override 5000, got %d", got.thresholdChars)
	}
	if got.keepRounds != contextCompactionDefaultKeepRounds {
		t.Fatalf("want keepRounds still defaulted, got %d", got.keepRounds)
	}

	ctxDisabled := withContextCompaction(context.Background(), contextCompactionConfig{disabled: true})
	if got := contextCompactionFromContext(ctxDisabled); !got.disabled {
		t.Fatalf("want disabled=true to survive through the resolver")
	}
}

// TestChatWithToolsBudgetCompactsOldRoundsAcrossManyRounds is the
// end-to-end proof: a model that keeps calling a tool for many rounds
// (each returning a large result) gets its OLDER rounds' tool results
// compacted, while the most recent ones stay full — verified via the
// debug trace's recorded message sequence, which is exactly the "all"
// slice these tests operate on directly above.
func TestChatWithToolsBudgetCompactsOldRoundsAcrossManyRounds(t *testing.T) {
	const rounds = 5
	bigResult := strings.Repeat("z", 3000) // 5 rounds * 3000 chars comfortably crosses a small test threshold
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		w.Header().Set("Content-Type", "application/json")
		if chatCalls > rounds {
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Fertig."}}]}`))
			return
		}
		fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
			{"id": "call_%d", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"q%d\"}"}}
		]}}]}`, chatCalls, chatCalls)
	}))
	defer server.Close()

	client := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "search", Description: "d", Parameters: map[string]any{"type": "object"}}}}
	executors := map[string]toolExecutor{
		"search": func(ctx context.Context, argsJSON string) (string, error) { return bigResult, nil },
	}

	// A small threshold (well under one round's own result size) forces
	// compaction to kick in from the very first eligible round, so this
	// test doesn't need dozens of rounds to observe the effect.
	ctx := withContextCompaction(context.Background(), contextCompactionConfig{thresholdChars: 1000, keepRounds: 2})
	ctx, dbg := withDebugTrace(ctx)

	var out bytes.Buffer
	if err := client.chatWithToolsBudget(ctx, "system", []chatMsg{{Role: "user", Content: "task"}}, tools, executors, &out, rounds+1); err != nil {
		t.Fatalf("chatWithToolsBudget: %v", err)
	}
	if out.String() != "Fertig." {
		t.Fatalf("want the final answer, got %q", out.String())
	}

	var toolMsgs []chatMsg
	for _, m := range dbg.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != rounds {
		t.Fatalf("want %d tool-result messages recorded, got %d", rounds, len(toolMsgs))
	}
	// The oldest rounds must be compacted (short placeholder, not the
	// original 3000-char result); the most recent keepRounds=2 must stay
	// full.
	for i, m := range toolMsgs {
		isRecent := i >= rounds-2
		if isRecent && m.Content != bigResult {
			t.Errorf("round %d (recent): want the full result preserved, got %q", i+1, m.Content)
		}
		if !isRecent && m.Content == bigResult {
			t.Errorf("round %d (old): want the result compacted, got it unchanged", i+1)
		}
		if !isRecent && !strings.Contains(m.Content, "gekürzt") {
			t.Errorf("round %d (old): want a compaction placeholder, got %q", i+1, m.Content)
		}
	}
}

// TestHandleSettingsPersistsContextCompaction guards exactly the failure
// mode this codebase has hit before (see AGENTS.md/memory): a new
// per-connection or per-feature settings field added to the struct but
// never wired into handleSettings' merge closure silently fails to
// persist on save, even though the UI sends it. Confirms all three new
// agentConfig.ContextCompaction* fields actually round-trip through
// POST /api/settings.
func TestHandleSettingsPersistsContextCompaction(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"agent": map[string]any{
			"context_compaction_disabled":        true,
			"context_compaction_threshold_chars": 5000,
			"context_compaction_keep_rounds":     3,
		},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := settings.get().Agent
	if !got.ContextCompactionDisabled {
		t.Fatalf("want ContextCompactionDisabled=true after save, got false")
	}
	if got.ContextCompactionThresholdChars != 5000 {
		t.Fatalf("want ContextCompactionThresholdChars=5000 after save, got %d", got.ContextCompactionThresholdChars)
	}
	if got.ContextCompactionKeepRounds != 3 {
		t.Fatalf("want ContextCompactionKeepRounds=3 after save, got %d", got.ContextCompactionKeepRounds)
	}
}
