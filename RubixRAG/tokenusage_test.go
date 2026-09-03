package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
)

func newTestTokenUsageStore(t *testing.T) *tokenUsageStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokenusage_test.db")
	s, err := newTokenUsageStore(path)
	if err != nil {
		t.Fatalf("newTokenUsageStore: %v", err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

func TestTokenUsageRecordAndSummaryAggregates(t *testing.T) {
	s := newTestTokenUsageStore(t)
	if err := s.record("alice@rubix.com", "ask", []tokenUsageEvent{
		{Provider: "azure", Model: "gpt-4o", PromptTokens: 100, CompletionTokens: 50},
		{Provider: "azure", Model: "gpt-4o", PromptTokens: 20, CompletionTokens: 10}, // a second round in the same request
	}); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	if err := s.record("bob@rubix.com", "draft_reply", []tokenUsageEvent{
		{Provider: "claude", Model: "claude-3-5-sonnet", PromptTokens: 200, CompletionTokens: 80},
	}); err != nil {
		t.Fatalf("record bob: %v", err)
	}
	if err := s.record("alice@rubix.com", "ask", []tokenUsageEvent{
		{Provider: "claude", Model: "claude-3-5-sonnet", PromptTokens: 5, CompletionTokens: 5, Estimated: true},
	}); err != nil {
		t.Fatalf("record alice claude: %v", err)
	}

	byProvider, byUserProvider, err := s.summary(0)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	sort.Slice(byProvider, func(i, j int) bool { return byProvider[i].Provider < byProvider[j].Provider })
	if len(byProvider) != 2 {
		t.Fatalf("want 2 providers, got %+v", byProvider)
	}
	azure := byProvider[0]
	if azure.Provider != "azure" || azure.PromptTokens != 120 || azure.CompletionTokens != 60 || azure.Calls != 2 {
		t.Fatalf("azure totals wrong: %+v", azure)
	}
	claude := byProvider[1]
	if claude.Provider != "claude" || claude.PromptTokens != 205 || claude.CompletionTokens != 85 || claude.Calls != 2 {
		t.Fatalf("claude totals wrong (should sum bob's + alice's estimated call): %+v", claude)
	}

	if len(byUserProvider) != 3 {
		t.Fatalf("want 3 (user,provider) rows, got %+v", byUserProvider)
	}
	var aliceAzure, aliceClaude, bobClaude *tokenUsageUserRow
	for i := range byUserProvider {
		r := &byUserProvider[i]
		switch {
		case r.User == "alice@rubix.com" && r.Provider == "azure":
			aliceAzure = r
		case r.User == "alice@rubix.com" && r.Provider == "claude":
			aliceClaude = r
		case r.User == "bob@rubix.com" && r.Provider == "claude":
			bobClaude = r
		}
	}
	if aliceAzure == nil || aliceAzure.PromptTokens != 120 || aliceAzure.CompletionTokens != 60 {
		t.Fatalf("alice/azure row wrong: %+v", aliceAzure)
	}
	if aliceClaude == nil || aliceClaude.PromptTokens != 5 || aliceClaude.Calls != 1 {
		t.Fatalf("alice/claude row wrong: %+v", aliceClaude)
	}
	if bobClaude == nil || bobClaude.PromptTokens != 200 || bobClaude.CompletionTokens != 80 {
		t.Fatalf("bob/claude row wrong: %+v", bobClaude)
	}
	// Cross-user isolation of the aggregation itself: alice's claude usage
	// and bob's claude usage must never be folded into one row just
	// because they share a provider.
	if aliceClaude == bobClaude {
		t.Fatal("alice's and bob's claude usage must be separate rows, not merged")
	}
}

// TestTokenUsageRecordAndSummaryIncludeCacheTokens covers Claude's
// cache_creation_input_tokens/cache_read_input_tokens (llm_claude.go)
// round-tripping through record/summary alongside the pre-existing
// prompt/completion totals — added when cache_control support landed, so a
// future regression in the ALTER-TABLE column migration or the SUM
// aggregation doesn't silently zero these out.
func TestTokenUsageRecordAndSummaryIncludeCacheTokens(t *testing.T) {
	s := newTestTokenUsageStore(t)
	if err := s.record("alice@rubix.com", "ask", []tokenUsageEvent{
		{Provider: "claude", Model: "claude-sonnet-5", PromptTokens: 50, CompletionTokens: 20, CacheCreationInputTokens: 1000, CacheReadInputTokens: 0},
		{Provider: "claude", Model: "claude-sonnet-5", PromptTokens: 30, CompletionTokens: 15, CacheCreationInputTokens: 0, CacheReadInputTokens: 1000}, // second round in the same answer: cache hit
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// A non-Claude provider's events never populate the cache fields — must
	// come back as 0, not NULL/error.
	if err := s.record("bob@rubix.com", "ask", []tokenUsageEvent{
		{Provider: "azure", Model: "gpt-4o", PromptTokens: 10, CompletionTokens: 5},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	byProvider, byUserProvider, err := s.summary(0)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	var claudeRow, azureRow *tokenUsageProviderRow
	for i := range byProvider {
		switch byProvider[i].Provider {
		case "claude":
			claudeRow = &byProvider[i]
		case "azure":
			azureRow = &byProvider[i]
		}
	}
	if claudeRow == nil {
		t.Fatalf("no claude row in byProvider: %+v", byProvider)
	}
	if claudeRow.CacheCreationTokens != 1000 || claudeRow.CacheReadTokens != 1000 {
		t.Fatalf("want claude cache totals 1000/1000, got %+v", claudeRow)
	}
	if azureRow == nil {
		t.Fatalf("no azure row in byProvider: %+v", byProvider)
	}
	if azureRow.CacheCreationTokens != 0 || azureRow.CacheReadTokens != 0 {
		t.Fatalf("want azure cache totals to be zero (never populated), got %+v", azureRow)
	}

	var aliceClaude *tokenUsageUserRow
	for i := range byUserProvider {
		if byUserProvider[i].User == "alice@rubix.com" && byUserProvider[i].Provider == "claude" {
			aliceClaude = &byUserProvider[i]
		}
	}
	if aliceClaude == nil || aliceClaude.CacheCreationTokens != 1000 || aliceClaude.CacheReadTokens != 1000 {
		t.Fatalf("alice/claude cache totals wrong: %+v", aliceClaude)
	}
}

func TestTokenUsageSummarySinceFiltersOlderRows(t *testing.T) {
	s := newTestTokenUsageStore(t)
	if err := s.record("alice@rubix.com", "ask", []tokenUsageEvent{
		{Provider: "local", Model: "m", PromptTokens: 10, CompletionTokens: 10},
	}); err != nil {
		t.Fatal(err)
	}
	// A future "since" cutoff: every row recorded above (at time.Now()) is
	// necessarily older than it, so the summary must come back empty.
	byProvider, byUserProvider, err := s.summary(1 << 61)
	if err != nil {
		t.Fatal(err)
	}
	if len(byProvider) != 0 || len(byUserProvider) != 0 {
		t.Fatalf("want no rows past a future cutoff, got provider=%+v user=%+v", byProvider, byUserProvider)
	}
}

func TestTokenUsageRecordNoEventsIsNoop(t *testing.T) {
	s := newTestTokenUsageStore(t)
	if err := s.record("alice@rubix.com", "ask", nil); err != nil {
		t.Fatalf("record with no events must not error: %v", err)
	}
	byProvider, _, err := s.summary(0)
	if err != nil || len(byProvider) != 0 {
		t.Fatalf("want no rows, got %+v (err=%v)", byProvider, err)
	}
}

// TestTokenUsageActorPrecedence covers tokenUsageActor's fallback chain:
// session mail, then session user (no mail on the claims), then an
// API-key name stashed in context (requireAPIKey/requireOpenAIAPIKey),
// then "anonym" — the same fallback every other identity-aware feature in
// this codebase uses.
func TestTokenUsageActorPrecedence(t *testing.T) {
	newReq := func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) }
	withSession := func(r *http.Request, claims sessionClaims) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, sessionCacheEntry{claims: claims, ok: true}))
	}

	t.Run("no session, no API key -> anonym", func(t *testing.T) {
		if got := tokenUsageActor(newReq()); got != "anonym" {
			t.Fatalf("want anonym, got %q", got)
		}
	})

	t.Run("API key name used when there is no session", func(t *testing.T) {
		r := newReq()
		r = r.WithContext(withAPIKeyName(r.Context(), "integration-x"))
		if got := tokenUsageActor(r); got != "api:integration-x" {
			t.Fatalf("want api:integration-x, got %q", got)
		}
	})

	t.Run("session mail takes precedence over an API key (session wins)", func(t *testing.T) {
		r := newReq()
		r = r.WithContext(withAPIKeyName(r.Context(), "integration-x"))
		r = withSession(r, sessionClaims{User: "alice", Mail: "alice@rubix.com"})
		if got := tokenUsageActor(r); got != "alice@rubix.com" {
			t.Fatalf("want alice@rubix.com, got %q", got)
		}
	})

	t.Run("session user used when claims have no mail", func(t *testing.T) {
		r := withSession(newReq(), sessionClaims{User: "alice"})
		if got := tokenUsageActor(r); got != "alice" {
			t.Fatalf("want alice, got %q", got)
		}
	})

	t.Run("empty API key name falls through to anonym, not an empty actor", func(t *testing.T) {
		r := newReq()
		r = r.WithContext(withAPIKeyName(r.Context(), ""))
		if got := tokenUsageActor(r); got != "anonym" {
			t.Fatalf("want anonym for an empty key name, got %q", got)
		}
	})
}

// TestTokenUsageTraceNilSafe confirms every method on a nil *tokenUsageTrace
// (the "nobody called withTokenUsage for this request" case, e.g.
// conntest.go's connection tests) is a no-op rather than a panic — the same
// guarantee debugTrace/agentProgress already document and rely on.
func TestTokenUsageTraceNilSafe(t *testing.T) {
	var trace *tokenUsageTrace
	trace.add(tokenUsageEvent{Provider: "local", PromptTokens: 1})
	if got := trace.snapshot(); got != nil {
		t.Fatalf("nil trace snapshot must be nil, got %+v", got)
	}
	recordTokenUsage(trace) // must not panic even though tokenUsage (the package var) is unset in this test binary
}

// TestRecordTokenUsageNilStoreDoesNotPanic is a regression test: a real
// (non-nil) trace with real events, but the package-level tokenUsage store
// itself unset — exactly the situation any handler test that calls
// handleAsk/handleDraftReply/etc. directly is in, since only main()'s
// bootstrap ever calls newTokenUsageStore. Caught a genuine nil-pointer
// panic in handleAsk's own existing test suite before this guard existed.
func TestRecordTokenUsageNilStoreDoesNotPanic(t *testing.T) {
	saved := tokenUsage
	tokenUsage = nil
	defer func() { tokenUsage = saved }()

	_, trace := withTokenUsage(context.Background(), "alice@rubix.com", "ask")
	trace.add(tokenUsageEvent{Provider: "azure", Model: "gpt-4o", PromptTokens: 10, CompletionTokens: 5})
	recordTokenUsage(trace) // must not panic
}
