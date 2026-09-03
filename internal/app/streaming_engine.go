package app

// ─────────────────────────────────────────────────────────────────────────────
// Streaming Answer Engine
//
// The StreamingEngine replaces the inline tool-execution code in /api/ask.
// It implements the full streaming-first, inline-tool architecture:
//
//  1. The LLM starts streaming immediately.
//  2. Tokens are fed to the XMLParseState incrementally.
//  3. Visible text is forwarded to the SSE client as it arrives.
//  4. As soon as a complete <tool>…</tool> block is recognized, the
//     corresponding tool is launched in a background goroutine (concurrent
//     with the remainder of the LLM stream).
//  5. After the LLM stream ends, all pending tool results are collected.
//  6. Tool results are injected back into the conversation as a new message
//     and the LLM is called again (continuation).
//  7. Steps 1-6 repeat up to maxContinuations times.
//  8. A hard cap on tool calls per request prevents runaway loops.
//  9. Duplicate tool calls (same name+query within one request) are skipped.
// 10. On tool failure, the error is reported to the LLM so it can answer
//     honestly without hallucinating data.
//
// Safety limits (all configurable via EngineConfig):
//   maxContinuations  — max continuation rounds (default 3)
//   maxToolsPerRound  — max tool calls per round (default 3)
//   maxToolsTotal     — total tool calls per request (default 5)
//   toolTimeout       — per-tool execution timeout (default 30 s)
// ─────────────────────────────────────────────────────────────────────────────

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

// EngineConfig holds all tunable limits for the streaming engine.
type EngineConfig struct {
	MaxContinuations int
	MaxToolsPerRound int
	MaxToolsTotal    int
	ToolTimeout      time.Duration
}

// defaultEngineConfig returns safe default limits.
func defaultEngineConfig() EngineConfig {
	return EngineConfig{
		MaxContinuations: 3,
		MaxToolsPerRound: 3,
		MaxToolsTotal:    5,
		ToolTimeout:      30 * time.Second,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Data structures
// ─────────────────────────────────────────────────────────────────────────────

// ToolResult holds the outcome of a single tool execution.
type ToolResult struct {
	Call              XMLToolCall
	Text              string
	Source            string
	Error             error
	Phase             string
	ContentHash       string
	EvidenceTruncated bool
	StartedAt         time.Time
	Duration          time.Duration
}

// EngineRequest captures all inputs for one /api/ask execution.
type EngineRequest struct {
	RequestID    string
	Question     string
	SystemPrompt string
	Messages     []chatMsg
	AutoSearch   bool
	Debug        bool
	// PlanFirst enables the agent planner: tools are planned and executed
	// upfront before the first answering round (see agent_planner.go).
	PlanFirst bool
}

// sseWriter wraps an http.ResponseWriter + http.Flusher to emit SSE events.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (sw *sseWriter) event(name, data string) {
	fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", name, data)
	sw.flusher.Flush()
}

func (sw *sseWriter) data(tok string) {
	d, _ := json.Marshal(tok)
	fmt.Fprintf(sw.w, "data: %s\n\n", d)
	sw.flusher.Flush()
}

func (sw *sseWriter) done() {
	fmt.Fprintf(sw.w, "data: [DONE]\n\n")
	sw.flusher.Flush()
}

// ─────────────────────────────────────────────────────────────────────────────
// StreamingEngine
// ─────────────────────────────────────────────────────────────────────────────

// StreamingEngine orchestrates the streaming answer loop with concurrent
// inline tool execution and LLM continuation.
type StreamingEngine struct {
	lm         lmProvider
	rag        *ragSystem
	settings   *settingsStore
	customAPIs *apiStore
	modules    *moduleStore
	connectors *connectorStore
	connExec   *connectorExecutor
	cfg        EngineConfig
}

// newStreamingEngine creates a StreamingEngine with default config.
func newStreamingEngine(lm lmProvider, rag *ragSystem, settings *settingsStore, customAPIs *apiStore, modules *moduleStore, connectors *connectorStore, connExec *connectorExecutor) *StreamingEngine {
	return &StreamingEngine{
		lm:         lm,
		rag:        rag,
		settings:   settings,
		customAPIs: customAPIs,
		modules:    modules,
		connectors: connectors,
		connExec:   connExec,
		cfg:        defaultEngineConfig(),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Main entry point
// ─────────────────────────────────────────────────────────────────────────────

// Run executes the full streaming answer loop and writes SSE events to w.
// It returns after [DONE] has been written.
func (e *StreamingEngine) Run(
	ctx context.Context,
	req EngineRequest,
	sw *sseWriter,
	tel *RequestTelemetry,
) (finalAnswer string, err error) {
	s := e.settings.get()

	msgs := req.Messages
	systemPrompt := req.SystemPrompt

	// dedup set: tool-name+query pairs seen this request
	seen := make(map[string]bool)
	totalTools := 0

	// Agent planner: plan + execute tools upfront, inject results as an
	// additional user message so the answering pass sees all evidence.
	if req.PlanFirst {
		if planMsg, ok := e.runPlannerPhase(ctx, req, sw, tel, s, seen, &totalTools); ok {
			msgs = append(msgs, planMsg)
		}
	}

	var fullAnswer strings.Builder

	for round := 0; round <= e.cfg.MaxContinuations; round++ {
		if round > 0 {
			tel.ContinuationCount = round
			log.Printf("ENGINE[%s] continuation round %d/%d", req.RequestID, round, e.cfg.MaxContinuations)
		}

		// ── Start LLM streaming goroutine ─────────────────────────────────────
		pr, pw := io.Pipe()
		var thinkBuf bytes.Buffer
		streamErrCh := make(chan error, 1)
		go func() {
			streamErr := e.lm.chatStreamDetailed(ctx, systemPrompt, msgs, pw, &thinkBuf)
			if streamErr != nil {
				pw.CloseWithError(streamErr)
			} else {
				pw.Close()
			}
			streamErrCh <- streamErr
		}()

		// ── Read stream, parse XML tool calls ─────────────────────────────────
		scanner := bufio.NewScanner(pr)
		scanner.Split(bufio.ScanRunes)

		parser := &XMLParseState{}

		// Collect tool calls found this round
		type pendingTool struct {
			call XMLToolCall
			res  chan ToolResult
		}
		var pendingTools []pendingTool

		var roundAnswer strings.Builder
		tokenCount := 0

		for scanner.Scan() {
			tok := scanner.Text()
			tokenCount++
			tel.TokensStreamed++

			result := parser.Feed(tok)
			tel.XMLParseErrors += result.ParseErrors
			tel.XMLBlocksEmitted += len(result.Calls)

			// Forward visible text to client
			if result.Visible != "" {
				tel.VisibleChars += len(result.Visible)
				sw.data(result.Visible)
				roundAnswer.WriteString(result.Visible)
			}

			// Launch tool calls concurrently as they complete
			for _, call := range result.Calls {
				var decision toolPolicyDecision
				call, decision = e.admitToolCall(s, call, req.AutoSearch)
				if !decision.Allowed {
					log.Printf("ENGINE[%s] tool not allowed: %s (%s)", req.RequestID, call.Name, decision.Reason)
					e.recordToolSkip(sw, tel, call, "inline", decision)
					continue
				}
				dedupKey := canonicalToolCallKey(call)
				if seen[dedupKey] {
					log.Printf("ENGINE[%s] dedup skip: %s(%s)", req.RequestID, call.Name, truncate(call.Query, 60))
					e.recordToolSkip(sw, tel, call, "inline", denyTool("deny", "duplicate_call", decision.Risk))
					continue
				}
				if totalTools >= e.cfg.MaxToolsTotal {
					log.Printf("ENGINE[%s] tool cap reached (%d), skipping %s", req.RequestID, e.cfg.MaxToolsTotal, call.Name)
					e.recordToolSkip(sw, tel, call, "inline", denyTool("deny", "request_tool_cap_reached", decision.Risk))
					continue
				}
				if len(pendingTools) >= e.cfg.MaxToolsPerRound {
					log.Printf("ENGINE[%s] per-round tool cap (%d) reached", req.RequestID, e.cfg.MaxToolsPerRound)
					e.recordToolSkip(sw, tel, call, "inline", denyTool("deny", "round_tool_cap_reached", decision.Risk))
					continue
				}

				seen[dedupKey] = true
				totalTools++

				// Notify frontend that tool is starting
				startPayload, _ := json.Marshal(map[string]any{
					"id": call.ID, "tool": call.Name, "query": call.Query,
					"arguments": call.Arguments, "phase": "inline",
				})
				sw.event("tool_start", string(startPayload))

				// Launch tool goroutine
				resCh := make(chan ToolResult, 1)
				go e.execTool(ctx, call, s, "inline", resCh)
				pendingTools = append(pendingTools, pendingTool{call: call, res: resCh})

				log.Printf("ENGINE[%s] tool started: id=%s name=%s query=%q", req.RequestID, call.ID, call.Name, truncate(call.Query, 80))
			}
		}

		// Flush any remaining buffered text from parser
		if tail := parser.Flush(); tail != "" {
			sw.data(tail)
			roundAnswer.WriteString(tail)
			tel.VisibleChars += len(tail)
		}

		// Check stream errors
		streamErr := <-streamErrCh
		if streamErr != nil {
			log.Printf("ENGINE[%s] LM stream error (round %d): %v", req.RequestID, round, streamErr)
			if tokenCount == 0 {
				sw.data("⚠️ LLM-Fehler: " + streamErr.Error())
			}
			if len(pendingTools) == 0 {
				sw.done()
				return fullAnswer.String(), streamErr
			}
			// Fall through to collect any completed tools
		}

		// Strip <tool> XML from the answer text before saving/using
		visibleAnswer := stripXMLToolCalls(roundAnswer.String())
		// Emit thinking/reasoning tokens as a dedicated SSE event so the
		// frontend can display them in a collapsible "reasoning" section.
		if thinkingStr := strings.TrimSpace(thinkBuf.String()); thinkingStr != "" {
			payload, _ := json.Marshal(thinkingStr)
			sw.event("reasoning", string(payload))
		}

		fullAnswer.WriteString(visibleAnswer)

		// ── Collect tool results ──────────────────────────────────────────────
		if len(pendingTools) == 0 {
			// No tools this round → we're done
			break
		}

		// Tools run concurrently, but collect their buffered results in call
		// order. This makes evidence, telemetry and SSE traces reproducible and
		// avoids concurrent writes to the streaming response.
		toolResults := make([]ToolResult, 0, len(pendingTools))
		for _, pt := range pendingTools {
			toolResults = append(toolResults, <-pt.res)
		}
		evidence := buildToolEvidenceMessage(toolResults, "inline")
		for i, res := range toolResults {
			toolResults[i].EvidenceTruncated = evidence.TruncatedCallIDs[res.Call.ID]
			rec := toolInvocationRecordFromResult(toolResults[i])
			tel.recordTool(rec)

			payload, _ := json.Marshal(map[string]any{
				"id": res.Call.ID, "tool": res.Call.Name, "query": res.Call.Query,
				"arguments": res.Call.Arguments, "source": res.Source, "error": errStr(res.Error),
				"result_bytes": len(res.Text), "content_hash": res.ContentHash,
				"evidence_truncated": toolResults[i].EvidenceTruncated, "phase": res.Phase,
			})
			sw.event("tool_result", string(payload))
		}

		if round >= e.cfg.MaxContinuations {
			log.Printf("ENGINE[%s] max continuations reached, stopping", req.RequestID)
			tel.FallbackReason = "max_continuations_reached"
			break
		}

		// ── Build continuation message ────────────────────────────────────────
		// Ingest tool results into RAG for future retrieval
		persistPolicy := ToolPersistencePolicy{}
		for _, tr := range toolResults {
			pclass := persistPolicy.Classify(tr.Call.Name, tr.Source)
			polPayload, _ := json.Marshal(map[string]any{
				"id": tr.Call.ID, "tool": tr.Call.Name, "source": tr.Source, "class": pclass,
			})
			sw.event("persistence_policy", string(polPayload))
			if tr.Error == nil && tr.Text != "" && e.rag != nil && pclass == ToolPersistableAfterPolicy {
				s = e.settings.get()
				chunks, _ := chunksForIngestWithDoc(tr.Text, s, stableContentHash(tr.Source), false)
				if ingestErr := e.rag.addChunks(tr.Source, chunks, s.EmbedModel); ingestErr != nil {
					log.Printf("ENGINE[%s] failed to ingest tool %s: %v", req.RequestID, tr.Call.Name, ingestErr)
					e.rag.logR3Audit(AuditEvent{
						EventType:   "tool_ingest_failed",
						Actor:       "engine",
						EntityType:  "tool_call",
						EntityID:    tr.Call.ID,
						Decision:    "deny",
						PolicyClass: string(pclass),
						Details:     ingestErr.Error(),
					})
				} else {
					e.rag.logR3Audit(AuditEvent{
						EventType:   "tool_ingest",
						Actor:       "engine",
						EntityType:  "tool_call",
						EntityID:    tr.Call.ID,
						Decision:    "allow",
						PolicyClass: string(pclass),
						Details:     tr.Call.Name,
					})
				}
			} else if e.rag != nil {
				e.rag.logR3Audit(AuditEvent{
					EventType:   "tool_persistence_skipped",
					Actor:       "engine",
					EntityType:  "tool_call",
					EntityID:    tr.Call.ID,
					Decision:    "deny",
					PolicyClass: string(pclass),
					Details:     tr.Call.Name,
				})
			}
		}

		contMsg := evidence.Content
		msgs = append(msgs, chatMsg{Role: "assistant", Content: roundAnswer.String()})
		msgs = append(msgs, chatMsg{Role: "user", Content: contMsg})

		log.Printf("ENGINE[%s] continuation: %d tool results, cont_msg_len=%d", req.RequestID, len(toolResults), len(contMsg))

		// Reset round answer for the next round
		roundAnswer.Reset()
		thinkBuf.Reset()
	}

	sw.done()
	return stripXMLToolCalls(fullAnswer.String()), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool execution
// ─────────────────────────────────────────────────────────────────────────────

// execTool runs the named tool and sends the result on resCh.
// It applies the configured ToolTimeout via a derived context.
// The context is passed to executeToolRequest to support cancellation.
func (e *StreamingEngine) execTool(ctx context.Context, call XMLToolCall, s appSettings, phase string, resCh chan<- ToolResult) {
	startedAt := time.Now()
	tctx, cancel := context.WithTimeout(ctx, e.cfg.ToolTimeout)
	defer cancel()

	tr := toolRequest{Tool: call.Name, Query: call.Query, Arguments: call.Arguments}
	rag := e.rag
	text, source, err := executeToolRequestCtx(tctx, tr, s, rag, e.customAPIs, e.modules, e.connectors, e.connExec)
	duration := time.Since(startedAt)

	if err != nil {
		log.Printf("ENGINE tool error: id=%s name=%s: %v", call.ID, call.Name, err)
	} else {
		log.Printf("ENGINE tool done: id=%s name=%s bytes=%d duration=%s", call.ID, call.Name, len(text), duration)
	}

	contentHash := ""
	if err == nil {
		contentHash = toolContentHash(text)
	}
	resCh <- ToolResult{
		Call: call, Text: text, Source: source, Phase: phase,
		ContentHash: contentHash, Error: err, StartedAt: startedAt, Duration: duration,
	}
}

// toolAllowed checks whether a tool may be executed in the current context.
func (e *StreamingEngine) toolAllowed(s appSettings, toolName string, autoSearch bool) bool {
	return e.evaluateToolPolicy(s, XMLToolCall{Name: toolName, Query: "_"}, autoSearch).Allowed
}

func toolInvocationRecordFromResult(res ToolResult) ToolInvocationRecord {
	rec := ToolInvocationRecord{
		ID:                res.Call.ID,
		Tool:              res.Call.Name,
		Query:             res.Call.Query,
		Phase:             res.Phase,
		StartTime:         res.StartedAt,
		EndTime:           res.StartedAt.Add(res.Duration),
		DurationMS:        res.Duration.Milliseconds(),
		ContentHash:       res.ContentHash,
		EvidenceTruncated: res.EvidenceTruncated,
		PolicyDecision:    "allow",
	}
	if res.Error != nil {
		rec.Error = res.Error.Error()
	} else {
		rec.ResultBytes = len(res.Text)
	}
	return rec
}

// recordToolSkip records policy and budget decisions in one place so the UI,
// logs and request telemetry all observe the same agent trace.
func (e *StreamingEngine) recordToolSkip(sw *sseWriter, tel *RequestTelemetry, call XMLToolCall, phase string, decision toolPolicyDecision) {
	now := time.Now()
	rec := ToolInvocationRecord{
		ID:             call.ID,
		Tool:           call.Name,
		Query:          call.Query,
		Phase:          phase,
		StartTime:      now,
		EndTime:        now,
		PolicyDecision: decision.Mode + ":" + decision.Reason,
		Deduplicated:   decision.Reason == "duplicate_call",
	}
	tel.recordTool(rec)
	if e.rag != nil {
		e.rag.logR3Audit(AuditEvent{
			EventType:   "tool_policy",
			Actor:       "engine",
			EntityType:  "tool_call",
			EntityID:    call.ID,
			Decision:    decision.Mode,
			PolicyClass: string(decision.Risk),
			Details:     phase + ":" + decision.Reason,
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"id": call.ID, "tool": call.Name, "query": call.Query,
		"arguments": call.Arguments, "phase": phase,
		"reason": decision.Reason, "policy": decision.Mode,
	})
	sw.event("tool_skipped", string(payload))
}

// ─────────────────────────────────────────────────────────────────────────────
// Continuation message builder
// ─────────────────────────────────────────────────────────────────────────────

// buildContinuationMessage builds the user message that injects tool results
// back into the conversation for the next LLM round.
func buildContinuationMessage(results []ToolResult) string {
	return buildToolEvidenceMessage(results, "inline").Content
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// toolDedupKey returns a stable, collision-resistant key for a (name, query) pair.
// Using a hash avoids false positives when a query legitimately contains the separator.
func toolDedupKey(name, query string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s", strings.ToLower(strings.TrimSpace(name)), strings.Join(strings.Fields(query), " "))
	return fmt.Sprintf("%x", h.Sum(nil))
}
