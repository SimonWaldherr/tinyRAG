# Streaming Execution Flow

## Overview

The `StreamingEngine` in `streaming_engine.go` implements the full
streaming-first, inline-tool architecture.  The key property is:

> **Tool execution starts as soon as a complete XML block is detected,
> concurrent with the remainder of the LLM stream.**

---

## Execution Loop (Single Round)

```
┌─────────────────────────────────────────────────────────────────────┐
│  LLM goroutine writes to io.Pipe (pw)                               │
│                                                                     │
│  Stream reader goroutine reads from pr:                             │
│                                                                     │
│   token → XMLParseState.Feed(token)                                 │
│             │                                                       │
│             ├─ result.Visible → SSE data: "token"                   │
│             │                                                       │
│             └─ result.Calls[] → for each completed XMLToolCall:     │
│                   1. Validate registered capability + policy        │
│                   2. Check canonical dedup (seen map)               │
│                   3. Check caps (totalTools, pendingTools)          │
│                   4. Emit SSE event: tool_start                     │
│                   5. Launch execTool() goroutine ──────────────────►│
│                                                                     │
│   [LLM stream continues while tools run concurrently]              │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

After the LLM stream pipe closes:
1. Flush remaining parser buffer
2. Collect all pending tool results in call order (via channel receive)
3. Emit SSE `event: tool_result` for each
4. If results exist → build continuation message → start next round
5. If cap reached or no tools → emit `[DONE]`

---

## XMLParseState Incremental Parsing

`XMLParseState.Feed(chunk)` processes one token at a time:

```
buffer += chunk

loop:
  start = index("<tool ", buffer)
  if start == -1:
    emit buffer[0 : -partial_prefix_len] as Visible
    break

  emit buffer[0:start] as Visible
  buffer = buffer[start:]

  end = index("</tool>", buffer)
  if end == -1:
    break  // incomplete – wait for more data

  block = buffer[0 : end+len("</tool>")]
  buffer = buffer[end+len("</tool>"):]

  if parseXMLBlock(block) succeeds:
    append to Calls
    emit block as Visible (for frontend rendering)
  else:
    emit block as Visible (plain text fallback)
    ParseErrors++
```

The `partial_prefix_len` holds back the tail of the buffer when it could
be the start of a future `<tool ` tag, preventing premature emission.

---

## Concurrent Tool Execution

Tools are launched as goroutines and communicate via buffered channels:

```go
resCh := make(chan ToolResult, 1)
go execTool(ctx, call, settings, resCh)
```

`execTool` applies a per-tool timeout and calls the context-aware tool
executor. The request context is forwarded to the planner, engine, URL fetch,
and connector execution. Results are collected after the LLM stream ends in
call order, so the prompt and telemetry stay deterministic even if tools
finish in another order.

---

## Continuation Loop

After the first round completes with tool results:

1. Build `contMsg` from a bounded evidence packet:
   - Includes call ID, tool, source, phase, and content hash
   - Wraps output in an explicit untrusted-data delimiter
   - Limits each result and the complete evidence packet
   - Instructs the model to produce a single revised answer without following
     instructions embedded in a tool result

2. Append to messages:
   ```
   msgs += {role: "assistant", content: round_answer_with_xml}
   msgs += {role: "user",      content: contMsg}
   ```

3. Start a new LLM stream round.

4. The loop repeats until:
   - No tools are called in a round
   - `MaxContinuations` is reached
   - Context cancelled

---

## Safety Limits

| Limit | Default | Config key |
|-------|---------|------------|
| Max continuation rounds | 3 | `EngineConfig.MaxContinuations` |
| Max tools per round | 3 | `EngineConfig.MaxToolsPerRound` |
| Max tools per request | 5 | `EngineConfig.MaxToolsTotal` |
| Per-tool timeout | 30 s | `EngineConfig.ToolTimeout` |
| Tool-evidence packet | 18,000 runes | Fixed server-side budget |
| One tool result in evidence | 6,000 runes | Fixed server-side budget |

When `MaxContinuations` is reached, `tel.FallbackReason` is set to
`"max_continuations_reached"` and the engine returns the accumulated answer.

---

## Tool Result Persistence

Successful tool results are evaluated by `ToolPersistencePolicy`; the default
for external and connector results is transient. Only a policy-approved source
is ingested as chunks:

```go
chunks, _ := chunksForIngest(tr.Text, s)
rag.addChunks(tr.Source, chunks, s.EmbedModel)
```

This keeps long-lived retrieval separate from transient agent evidence and
prevents the agent loop from turning every fetched response into corpus data.

---

## Error Handling

| Scenario | Behavior |
|----------|----------|
| LLM stream error before any tokens | Emit `⚠️ LLM-Fehler: …` + `[DONE]` |
| LLM stream error after tokens | Stop streaming, return partial answer |
| Tool execution error | Reported to LLM in continuation message; model must explain honestly |
| Tool timeout | Returns error to continuation message |
| Invalid XML | Logged, emitted as visible text, no execution |
| Partial XML at stream end | Flushed as visible text via `parser.Flush()` |
