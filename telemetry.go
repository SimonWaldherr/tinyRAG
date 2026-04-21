package main

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry
//
// Structured telemetry for each /api/ask request.  All fields are populated
// incrementally during request processing and emitted at the end as a single
// structured log line.
//
// The design is intentionally simple: a plain struct, populated by value,
// and emitted via the standard log package.  No external dependencies.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"encoding/json"
	"log"
	"time"
)

// RequestTelemetry captures observable events and timings for a single
// /api/ask request.  It is safe to copy (no pointer fields that are mutated).
type RequestTelemetry struct {
	// ── Identity ─────────────────────────────────────────────────────────────
	RequestID string `json:"request_id"`
	ChatID    string `json:"chat_id"`

	// ── Input ────────────────────────────────────────────────────────────────
	Question         string `json:"question"`
	NormalizedQuery  string `json:"normalized_query"`
	QuestionLen      int    `json:"question_len"`

	// ── Routing ──────────────────────────────────────────────────────────────
	SelectedMode string   `json:"selected_mode"`
	RouteReason  string   `json:"route_reason"`
	RouteHints   []string `json:"route_hints,omitempty"`

	// ── RAG context ──────────────────────────────────────────────────────────
	ContextChars int `json:"context_chars"`
	RAGChunks    int `json:"rag_chunks"`

	// ── Streaming ─────────────────────────────────────────────────────────────
	TokensStreamed    int `json:"tokens_streamed"`
	VisibleChars      int `json:"visible_chars"`

	// ── XML tool calls ────────────────────────────────────────────────────────
	XMLBlocksEmitted  int `json:"xml_blocks_emitted"`
	XMLParseErrors    int `json:"xml_parse_errors"`

	// ── Tool execution ────────────────────────────────────────────────────────
	ToolInvocations []ToolInvocationRecord `json:"tool_invocations,omitempty"`

	// ── Continuation ─────────────────────────────────────────────────────────
	ContinuationCount int    `json:"continuation_count"`
	FallbackReason    string `json:"fallback_reason,omitempty"`

	// ── Timing ────────────────────────────────────────────────────────────────
	StartTime  time.Time     `json:"start_time"`
	TotalMS    int64         `json:"total_ms"`

	// ── Final state ───────────────────────────────────────────────────────────
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// ToolInvocationRecord captures one tool call's lifecycle.
type ToolInvocationRecord struct {
	ID          string    `json:"id"`
	Tool        string    `json:"tool"`
	Query       string    `json:"query"`
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	DurationMS  int64     `json:"duration_ms"`
	ResultBytes int       `json:"result_bytes"`
	Error       string    `json:"error,omitempty"`
	Deduplicated bool     `json:"deduplicated,omitempty"`
}

// newRequestTelemetry initializes a RequestTelemetry for a new request.
func newRequestTelemetry(reqID, chatID, question string) *RequestTelemetry {
	return &RequestTelemetry{
		RequestID: reqID,
		ChatID:    chatID,
		Question:  question,
		StartTime: time.Now(),
	}
}

// recordTool adds a completed tool invocation record.
func (t *RequestTelemetry) recordTool(rec ToolInvocationRecord) {
	t.ToolInvocations = append(t.ToolInvocations, rec)
	t.XMLBlocksEmitted++
}

// finalize sets timing and emits the telemetry log line.
func (t *RequestTelemetry) finalize(success bool, errMsg string) {
	t.Success = success
	t.Error = errMsg
	t.TotalMS = time.Since(t.StartTime).Milliseconds()
	b, _ := json.Marshal(t)
	log.Printf("TELEMETRY %s", string(b))
}
