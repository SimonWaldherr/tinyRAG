package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Per-user, per-service token-usage logging — how many input/output tokens
// each user consumed, broken down by which LLM backend (local/azure/openai/
// openrouter/claude/gemini) handled the request. Chat only (see embedCtx's
// doc comment for why retrieval-query embeddings aren't included) — a
// separate concern from debugTrace (llm.go, one request's raw context/
// messages for the debug panel) and agentProgress (llm.go, live step-by-step
// UI events), but threaded through exactly the same way: a context-carried,
// nil-receiver-safe accumulator, so chatWithToolsBudget/chatStream/chatOnce
// and every existing caller/test are untouched unless a handler explicitly
// opts in via withTokenUsage.
//
// Storage is a dedicated SQLite file (same modernc.org/sqlite driver
// chathistory.go already depends on, same open-string convention) rather
// than the in-memory ring buffers or append-only JSONL logs used elsewhere
// in this codebase (scheduler.go/agent.go, audit.go/settings_history.go) —
// this data is high-volume (every chat/agent/draft call, not just admin
// actions) and the Settings chart needs real SUM/GROUP BY aggregation,
// which SQL gives for free and a growing JSONL file scanned in Go would not.
// ─────────────────────────────────────────────────────────────────────────────

// tokenUsageEvent is one LLM call's token cost, captured at the exact point
// each provider's response is parsed (llm.go's chatOnce/chatStreamMessages/
// embedCtx, llm_claude.go's claudeChatOnce, llm_gemini.go's geminiChatOnce).
type tokenUsageEvent struct {
	Provider         string // c.provider: "local"|"azure"|"openai"|"openrouter"|"claude"|"gemini"
	Model            string // c.chatModel (or c.embedModel for an embed call)
	PromptTokens     int
	CompletionTokens int
	// CacheCreationInputTokens/CacheReadInputTokens are Claude-only (see
	// llm_claude.go's claudeChatOnce, which is the only populator): tokens
	// written to/read from Anthropic's prompt cache on this call, per the
	// response's usage.cache_creation_input_tokens/cache_read_input_tokens
	// fields (see llm_claude.go's system-prompt cache_control marker for
	// why Claude requests ever populate these). Zero for every other
	// provider — none of them report a comparable split today (the
	// OpenAI-compatible path's automatic server-side caching, if any,
	// isn't surfaced in its response shape; Gemini's cache is a distinct,
	// unimplemented mechanism — see llm_gemini.go's package comment).
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	// Estimated is true when PromptTokens/CompletionTokens are a
	// character-count heuristic (chatStreamMessages' OpenAI-compatible SSE
	// path, when the backend never sent a usage-bearing chunk), not a real
	// count the provider reported — see estimateOpenAITokens (openai_api.go).
	Estimated bool
}

// tokenUsageTrace accumulates every tokenUsageEvent made during one request
// — same context-carried, nil-receiver-safe threading pattern as debugTrace/
// agentProgress (llm.go), so no existing call site or test needs to change
// unless it explicitly opts in via withTokenUsage. Actor/Kind are resolved
// once by whichever handler creates the trace (a single HTTP request only
// ever has one of each), not repeated per event.
type tokenUsageTrace struct {
	mu     sync.Mutex
	Actor  string
	Kind   string
	Events []tokenUsageEvent
}

// add records one event. Nil-safe: a call site with no trace in its context
// (i.e. nobody called withTokenUsage) is a silent no-op, not an error.
func (t *tokenUsageTrace) add(e tokenUsageEvent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Events = append(t.Events, e)
}

// snapshot returns a copy of the accumulated events, safe to read after the
// LLM call(s) that populated them have returned.
func (t *tokenUsageTrace) snapshot() []tokenUsageEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]tokenUsageEvent, len(t.Events))
	copy(out, t.Events)
	return out
}

type tokenUsageContextKey struct{}

// withTokenUsage returns ctx augmented with a fresh *tokenUsageTrace, plus
// that same trace for the caller to read back once its LLM call(s)
// complete — actor/kind are resolved once, up front, by the handler (see
// tokenUsageActor below for actor; kind is a short label like "ask"/
// "draft_reply" identifying which feature made the call).
func withTokenUsage(ctx context.Context, actor, kind string) (context.Context, *tokenUsageTrace) {
	t := &tokenUsageTrace{Actor: actor, Kind: kind}
	return context.WithValue(ctx, tokenUsageContextKey{}, t), t
}

// tokenUsageFromContext returns nil if ctx carries none — every capture
// point treats that as "usage logging is off for this call", not an error.
func tokenUsageFromContext(ctx context.Context) *tokenUsageTrace {
	t, _ := ctx.Value(tokenUsageContextKey{}).(*tokenUsageTrace)
	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// API-key identity — requireAPIKey/requireOpenAIAPIKey (handlers.go,
// openai_api.go) resolve the matched key's admin-assigned Name at auth time
// but never hand it to the wrapped handler. Stashing it in context (same
// shape as every accumulator above) lets tokenUsageActor attribute an
// external API caller's usage to that key's Name instead of lumping every
// unauthenticated-by-session caller into "anonym".
// ─────────────────────────────────────────────────────────────────────────────

type apiKeyNameContextKey struct{}

func withAPIKeyName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, apiKeyNameContextKey{}, name)
}

func apiKeyNameFromContext(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(apiKeyNameContextKey{}).(string)
	return name, ok
}

// tokenUsageActor resolves "who's asking" for token-usage attribution:
// session mail/username (same precedence as audit.go's actorFromRequest),
// then an API-key's Name (see withAPIKeyName above — audit.go's resolver
// has no equivalent case, since audit actions are only ever session- or
// request-triggered admin actions, not external API traffic), then
// "anonym" — the same fallback every other identity-aware feature in this
// codebase uses when LDAP login is off or the caller never logged in.
func tokenUsageActor(r *http.Request) string {
	if claims, ok := currentSession(r); ok {
		return sessionActor(claims)
	}
	if name, ok := apiKeyNameFromContext(r.Context()); ok && name != "" {
		return "api:" + name
	}
	return "anonym"
}

// ─────────────────────────────────────────────────────────────────────────────
// Storage — a dedicated SQLite file, independent of whichever vectorStore
// backend stores chunk vectors and of chatHistory's own file (unrelated
// data, no reason to couple any of them) — see newChatHistoryStore
// (chathistory.go) for the identical open/schema pattern this mirrors.
// ─────────────────────────────────────────────────────────────────────────────

// tokenUsage is the process-wide store, opened once in main() from
// settings.TokenUsagePath — nil only in tests that don't call
// newTokenUsageStore.
var tokenUsage *tokenUsageStore

type tokenUsageStore struct {
	db *sql.DB
}

// newTokenUsageStore opens (creating if needed) the SQLite file at path and
// ensures its schema exists.
func newTokenUsageStore(path string) (*tokenUsageStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("token usage: open %s: %w", path, err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS token_usage (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time INTEGER NOT NULL,
			user TEXT NOT NULL,
			kind TEXT NOT NULL,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			prompt_tokens INTEGER NOT NULL,
			completion_tokens INTEGER NOT NULL,
			estimated INTEGER NOT NULL,
			cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_input_tokens INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_token_usage_time ON token_usage(time);
		CREATE INDEX IF NOT EXISTS idx_token_usage_user_provider ON token_usage(user, provider);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("token usage: schema: %w", err)
	}
	// cache_creation_input_tokens/cache_read_input_tokens (Claude prompt
	// caching, see tokenUsageEvent's doc comment) are already in the CREATE
	// TABLE text above for a fresh install, but this file has no migrations
	// system (see the package doc comment), so a token-usage.db from before
	// these columns existed needs them added via ALTER TABLE — CREATE TABLE
	// IF NOT EXISTS is a no-op once the table already exists, columns and
	// all. SQLite has no "ADD COLUMN IF NOT EXISTS", so this always
	// attempts the ALTER and swallows exactly the "duplicate column name"
	// error it returns on a database that already has the column (every
	// fresh install, and every restart after the first) — same pattern as
	// chathistory.go's newChatHistoryStore.
	for _, col := range []string{
		`ALTER TABLE token_usage ADD COLUMN cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE token_usage ADD COLUMN cache_read_input_tokens INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(col); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				db.Close()
				return nil, fmt.Errorf("token usage: add cache columns: %w", err)
			}
		}
	}
	return &tokenUsageStore{db: db}, nil
}

// close releases the underlying database handle — mainly for tests (see
// chathistoryStore.close's doc comment for why: Windows can't delete a
// still-open SQLite file, which t.TempDir's cleanup needs to do).
func (s *tokenUsageStore) close() error { return s.db.Close() }

// record persists every event from one request's trace, all sharing the
// same actor/kind — best-effort from the caller's perspective (see
// handlers.go's call sites: a write failure is logged, never fails the
// response that already succeeded). A request that made no LLM calls (or
// whose trace was never populated) has nothing to insert; that's not an
// error.
func (s *tokenUsageStore) record(actor, kind string, events []tokenUsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, e := range events {
		estimated := 0
		if e.Estimated {
			estimated = 1
		}
		if _, err := tx.Exec(`INSERT INTO token_usage (time, user, kind, provider, model, prompt_tokens, completion_tokens, estimated, cache_creation_input_tokens, cache_read_input_tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			now, actor, kind, e.Provider, e.Model, e.PromptTokens, e.CompletionTokens, estimated, e.CacheCreationInputTokens, e.CacheReadInputTokens); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// tokenUsageProviderRow is one provider's totals — the Settings chart's
// primary breakdown (one stacked bar per row: prompt vs completion tokens).
//
// CacheCreationTokens/CacheReadTokens are always 0 for every provider
// except Claude (see tokenUsageEvent's doc comment) — exposed here so a
// future Settings-chart iteration can render a cached-vs-fresh split, but
// today's chart UI (web/app.js) does not yet read these two fields; that's
// a deliberate, disclosed follow-up, not an oversight.
type tokenUsageProviderRow struct {
	Provider            string `json:"provider"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	Calls               int64  `json:"calls"`
}

// tokenUsageUserRow is one (user, provider) pair's totals — the supporting
// table underneath the chart, answering "which user consumed how much".
// See tokenUsageProviderRow's doc comment re: the two cache fields.
type tokenUsageUserRow struct {
	User                string `json:"user"`
	Provider            string `json:"provider"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	Calls               int64  `json:"calls"`
}

// summary aggregates every row at/after sinceUnix (0 = all-time) into both
// breakdowns the Settings page needs, in one round trip.
func (s *tokenUsageStore) summary(sinceUnix int64) (byProvider []tokenUsageProviderRow, byUserProvider []tokenUsageUserRow, err error) {
	byProvider = []tokenUsageProviderRow{}
	rows, err := s.db.Query(`
		SELECT provider, SUM(prompt_tokens), SUM(completion_tokens), SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens), COUNT(*)
		FROM token_usage WHERE time >= ?
		GROUP BY provider
		ORDER BY SUM(prompt_tokens) + SUM(completion_tokens) DESC`, sinceUnix)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var row tokenUsageProviderRow
		if err := rows.Scan(&row.Provider, &row.PromptTokens, &row.CompletionTokens, &row.CacheCreationTokens, &row.CacheReadTokens, &row.Calls); err != nil {
			rows.Close()
			return nil, nil, err
		}
		byProvider = append(byProvider, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	byUserProvider = []tokenUsageUserRow{}
	rows, err = s.db.Query(`
		SELECT user, provider, SUM(prompt_tokens), SUM(completion_tokens), SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens), COUNT(*)
		FROM token_usage WHERE time >= ?
		GROUP BY user, provider
		ORDER BY SUM(prompt_tokens) + SUM(completion_tokens) DESC`, sinceUnix)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row tokenUsageUserRow
		if err := rows.Scan(&row.User, &row.Provider, &row.PromptTokens, &row.CompletionTokens, &row.CacheCreationTokens, &row.CacheReadTokens, &row.Calls); err != nil {
			return nil, nil, err
		}
		byUserProvider = append(byUserProvider, row)
	}
	return byProvider, byUserProvider, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP handler — registered admin-gated in handlers.go's registerRoutes,
// next to /api/settings/history.
// ─────────────────────────────────────────────────────────────────────────────

type tokenUsageResponse struct {
	ByProvider     []tokenUsageProviderRow `json:"by_provider"`
	ByUserProvider []tokenUsageUserRow     `json:"by_user_provider"`
}

// handleTokenUsage serves GET /api/token-usage?days=N (0 or absent =
// all-time) — one call returns both breakdowns the Settings chart/table
// need.
func handleTokenUsage(w http.ResponseWriter, r *http.Request) {
	var since int64
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		days, err := strconv.Atoi(daysStr)
		if err != nil || days < 0 {
			writeJSONError(w, "invalid days parameter", http.StatusBadRequest)
			return
		}
		if days > 0 {
			since = time.Now().AddDate(0, 0, -days).Unix()
		}
	}
	byProvider, byUserProvider, err := tokenUsage.summary(since)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, tokenUsageResponse{ByProvider: byProvider, ByUserProvider: byUserProvider})
}

// recordTokenUsage persists trace's accumulated events, if any — the common
// tail every wrapped handler calls right after its LLM call(s) return
// (success or error, tokens were spent either way). Best-effort: a write
// failure is logged, never surfaced to the caller, matching logAuditAs'
// own "a log write must never fail the response it's logging" reasoning.
func recordTokenUsage(trace *tokenUsageTrace) {
	// tokenUsage (the package-wide store) is nil in every test that calls a
	// handler like handleAsk directly without going through main()'s
	// bootstrap (newTokenUsageStore is only ever called there) — the same
	// "unconfigured means silently off" contract chatHistory's own
	// nil-in-tests callers already rely on elsewhere, made explicit here
	// since this capture point is unconditional (unlike debugTrace, which
	// only exists in a request's context when debugModeAllowed opted in).
	if trace == nil || tokenUsage == nil {
		return
	}
	events := trace.snapshot()
	if len(events) == 0 {
		return
	}
	if err := tokenUsage.record(trace.Actor, trace.Kind, events); err != nil {
		log.Printf("WARN: token usage log write failed: %v", err)
	}
}
