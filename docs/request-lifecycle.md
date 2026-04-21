# Request Lifecycle

## End-to-End Flow for `/api/ask`

```
Client                    Backend                           LLM
  │                          │                               │
  ├─ POST /api/ask ──────────►│                               │
  │                          │ normalizeQuery()               │
  │                          │ routeQuery() → mode           │
  │                          │ prepareContext() / RAG         │
  │                          │ buildToolSystemPrompt()        │
  │◄── event: meta ──────────┤                               │
  │◄── event: debug ─────────┤ (if debug=true)               │
  │◄── event: route ─────────┤ (if debug=true)               │
  │                          │                               │
  │                          │ StreamingEngine.Run()          │
  │                          ├─ chatStreamDetailed() ────────►│
  │                          │                               │
  │◄── data: "token" ────────┤◄── stream token ──────────────┤
  │◄── data: "token" ────────┤◄── stream token ──────────────┤
  │◄── data: "<tool…" ───────┤◄── XML block starts ──────────┤
  │◄── data: "…/tool>" ──────┤◄── XML block completes ───────┤
  │◄── event: tool_start ────┤                               │
  │                          │  [tool goroutine launched]     │
  │◄── data: "token" ────────┤◄── more tokens ───────────────┤
  │                          │◄── LLM stream ends ────────────┤
  │                          │  [collect tool results]        │
  │◄── event: tool_result ───┤                               │
  │                          │ buildContinuationMessage()     │
  │                          ├─ chatStreamDetailed() ────────►│
  │◄── data: "token" ────────┤◄── continuation tokens ────────┤
  │◄── data: [DONE] ─────────┤◄── done ───────────────────────┤
  │                          │ tel.finalize() → TELEMETRY log │
  │                          │ chats.addMessageWithMeta()     │
```

---

## Phases

### 1. Input / Request Context

`POST /api/ask` receives:
- `question` — user message
- `chat_id` — conversation ID (created if missing)
- `auto_search` — whether web/external tools are auto-approved
- `debug` — emit detailed debug events
- `deep` — use larger K for RAG retrieval
- `offline` — skip LLM, return raw context
- `persona_id` — assistant persona override
- `image_base64` / `image_type` — optional vision attachment

### 2. Query Normalizer

`normalizeQuery(question)` — lowercase, trimmed, word-split.
Used internally for routing and telemetry.

### 3. Router

`routeQuery(nq, hasRAGContext)` — deterministic heuristic:
- `direct_answer` — conceptual / no external data
- `retrieval_answer` — docs, specs, manuals, FAQs
- `agentic_answer` — orders, URLs, calculations, multi-source

The mode is emitted in `event: route` (debug mode) and in telemetry.

### 4. RAG Context Preparation

`rag.prepareContext(question, debug)` retrieves the top-K most similar
chunks from the vector store.  In deep mode, K is multiplied.

### 5. System Prompt Construction

`buildToolSystemPrompt(ctxText, tools, deep, s)` builds a compact system
prompt that includes:
- Policy instructions (language, accuracy, anti-hallucination)
- RAG context (or empty-context message)
- XML tool protocol instructions
- Available tool manifest

### 6. Streaming Answer Engine

`StreamingEngine.Run()` handles the full streaming loop.
See [streaming-execution-flow.md](streaming-execution-flow.md).

### 7. Chat History

After the engine returns, `chats.addMessageWithMeta()` stores the
clean answer (XML blocks stripped) to the conversation.

### 8. Telemetry

`tel.finalize()` emits a JSON telemetry log line at the end of the request.
See [telemetry fields](#telemetry-fields) below.

---

## Telemetry Fields

| Field | Type | Description |
|-------|------|-------------|
| `request_id` | string | Unique request identifier |
| `chat_id` | string | Conversation ID |
| `question` | string | Original question |
| `normalized_query` | string | Lowercase normalized query |
| `question_len` | int | Length of question |
| `selected_mode` | string | direct/retrieval/agentic |
| `route_reason` | string | Why this mode was chosen |
| `route_hints` | []string | Detected signal keywords |
| `context_chars` | int | RAG context size |
| `tokens_streamed` | int | Total LLM tokens received |
| `visible_chars` | int | Visible answer characters |
| `xml_blocks_emitted` | int | Successful XML tool calls |
| `xml_parse_errors` | int | Invalid XML blocks detected |
| `tool_invocations` | []ToolInvocationRecord | Per-tool timing and results |
| `continuation_count` | int | Number of continuation rounds |
| `fallback_reason` | string | Why fallback occurred (if any) |
| `start_time` | time | Request start |
| `total_ms` | int | Total request duration |
| `success` | bool | Whether the request succeeded |
| `error` | string | Error message if any |
