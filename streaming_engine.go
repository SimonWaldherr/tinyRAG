package main

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
	"sync"
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
	Call      XMLToolCall
	Text      string
	Source    string
	Error     error
	StartedAt time.Time
	Duration  time.Duration
}

// EngineRequest captures all inputs for one /api/ask execution.
type EngineRequest struct {
	RequestID    string
	Question     string
	SystemPrompt string
	Messages     []chatMsg
	AutoSearch   bool
	Debug        bool
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

			// Forward visible text to client
			if result.Visible != "" {
				tel.VisibleChars += len(result.Visible)
				sw.data(result.Visible)
				roundAnswer.WriteString(result.Visible)
			}

			// Launch tool calls concurrently as they complete
			for _, call := range result.Calls {
				dedupKey := toolDedupKey(call.Name, call.Query)
				if seen[dedupKey] {
					log.Printf("ENGINE[%s] dedup skip: %s(%s)", req.RequestID, call.Name, truncate(call.Query, 60))
					tel.recordTool(ToolInvocationRecord{
						ID: call.ID, Tool: call.Name, Query: call.Query,
						Deduplicated: true, StartTime: time.Now(), EndTime: time.Now(),
					})
					continue
				}
				if totalTools >= e.cfg.MaxToolsTotal {
					log.Printf("ENGINE[%s] tool cap reached (%d), skipping %s", req.RequestID, e.cfg.MaxToolsTotal, call.Name)
					continue
				}
				if len(pendingTools) >= e.cfg.MaxToolsPerRound {
					log.Printf("ENGINE[%s] per-round tool cap (%d) reached", req.RequestID, e.cfg.MaxToolsPerRound)
					continue
				}
				if !e.toolAllowed(s, call.Name, req.AutoSearch) {
					log.Printf("ENGINE[%s] tool not allowed: %s (role=%s auto_search=%t)", req.RequestID, call.Name, s.ActiveRole, req.AutoSearch)
					continue
				}

				seen[dedupKey] = true
				totalTools++

				// Notify frontend that tool is starting
				startPayload, _ := json.Marshal(map[string]string{
					"id": call.ID, "tool": call.Name, "query": call.Query,
				})
				sw.event("tool_start", string(startPayload))

				// Launch tool goroutine
				resCh := make(chan ToolResult, 1)
				go e.execTool(ctx, call, s, resCh)
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

		// Collect all tool results (with individual timeouts already applied
		// inside execTool, so this Wait should return quickly).
		var toolResults []ToolResult
		var wg sync.WaitGroup
		resultsMu := sync.Mutex{}
		for _, pt := range pendingTools {
			wg.Add(1)
			go func(ch chan ToolResult, call XMLToolCall) {
				defer wg.Done()
				res := <-ch
				resultsMu.Lock()
				toolResults = append(toolResults, res)
				resultsMu.Unlock()

				// Record telemetry
				rec := ToolInvocationRecord{
					ID:         call.ID,
					Tool:       call.Name,
					Query:      call.Query,
					StartTime:  res.StartedAt,
					EndTime:    res.StartedAt.Add(res.Duration),
					DurationMS: res.Duration.Milliseconds(),
				}
				if res.Error != nil {
					rec.Error = res.Error.Error()
				} else {
					rec.ResultBytes = len(res.Text)
				}
				tel.recordTool(rec)

				// Notify frontend of result
				payload, _ := json.Marshal(map[string]any{
					"id": call.ID, "tool": call.Name, "query": call.Query,
					"source": res.Source, "error": errStr(res.Error),
					"result_bytes": len(res.Text),
				})
				sw.event("tool_result", string(payload))
			}(pt.res, pt.call)
		}
		wg.Wait()

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

		contMsg := buildContinuationMessage(toolResults)
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
func (e *StreamingEngine) execTool(ctx context.Context, call XMLToolCall, s appSettings, resCh chan<- ToolResult) {
	startedAt := time.Now()
	tctx, cancel := context.WithTimeout(ctx, e.cfg.ToolTimeout)
	defer cancel()

	tr := toolRequest{Tool: call.Name, Query: call.Query}
	rag := e.rag
	text, source, err := executeToolRequestCtx(tctx, tr, s, rag, e.customAPIs, e.modules, e.connectors, e.connExec)
	duration := time.Since(startedAt)

	if err != nil {
		log.Printf("ENGINE tool error: id=%s name=%s: %v", call.ID, call.Name, err)
	} else {
		log.Printf("ENGINE tool done: id=%s name=%s bytes=%d duration=%s", call.ID, call.Name, len(text), duration)
	}

	resCh <- ToolResult{
		Call: call, Text: text, Source: source,
		Error: err, StartedAt: startedAt, Duration: duration,
	}
}

// toolAllowed checks whether a tool may be executed in the current context.
func (e *StreamingEngine) toolAllowed(s appSettings, toolName string, autoSearch bool) bool {
	tr := toolRequest{Tool: toolName}
	return shouldAutoExecuteTool(s, tr, autoSearch)
}

// ─────────────────────────────────────────────────────────────────────────────
// Continuation message builder
// ─────────────────────────────────────────────────────────────────────────────

// buildContinuationMessage builds the user message that injects tool results
// back into the conversation for the next LLM round.
func buildContinuationMessage(results []ToolResult) string {
	var sb strings.Builder
	sb.WriteString("Die folgenden Tools wurden ausgeführt. Verarbeite die Ergebnisse und erstelle eine präzise, vollständige Antwort.\n\n")

	for _, r := range results {
		sb.WriteString(fmt.Sprintf("### Tool: %s\nQuery: %s\n", r.Call.Name, r.Call.Query))
		if r.Error != nil {
			sb.WriteString(fmt.Sprintf("Fehler: %s\n", r.Error.Error()))
			sb.WriteString("Hinweis: Erkläre dem Nutzer ehrlich, dass dieses Tool fehlgeschlagen ist. Erfinde keine Daten.\n")
		} else {
			sb.WriteString(fmt.Sprintf("Ergebnis (%d Zeichen):\n%s\n", len(r.Text), r.Text))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\nRegeln für die Antwort:\n")
	sb.WriteString("- Verwende alle verfügbaren Tool-Ergebnisse.\n")
	sb.WriteString("- Trenne lokales Wissen und Tool-Ergebnisse klar.\n")
	sb.WriteString("- Wenn ein Tool fehlschlug, sage das offen.\n")
	sb.WriteString("- Keine neuen <tool>-Blöcke emittieren, außer wenn unbedingt nötig.\n")
	sb.WriteString("- Kompakte, faktische Antwort ohne Marketing-Sprache.\n")
	return sb.String()
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
	fmt.Fprintf(h, "%s\x00%s", name, query)
	return fmt.Sprintf("%x", h.Sum(nil))
}
