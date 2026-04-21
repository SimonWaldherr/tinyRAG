# XML Tool Protocol

## Overview

tinyRAG uses an XML-based inline tool-call protocol.  The LLM emits
self-contained XML blocks **inside its streamed text** to request tool
execution.  The backend detects, parses, and executes these blocks
incrementally during streaming.

---

## Block Format

All tool invocations use the same outer element:

```xml
<tool name="TOOL_NAME">CONTENT</tool>
```

The `name` attribute identifies the tool.  The inner element depends on the
tool type:

| Tool type       | Inner element | Description                  |
|-----------------|---------------|------------------------------|
| RAG / search    | `<query>`     | Free-text search term         |
| URL fetch       | `<url>`       | Full HTTP/HTTPS URL           |
| Code execution  | `<source>`    | Go source snippet             |

### Examples

```xml
<!-- Internal knowledge base search -->
<tool name="rag_knowledge"><query>product return policy</query></tool>

<!-- Fetch a URL and return plain text -->
<tool name="url_fetch"><url>https://example.com/docs/api</url></tool>

<!-- Execute Go code in the nanoGo sandbox -->
<tool name="nanogo"><source>fmt.Println(2 + 2)</source></tool>

<!-- Web search -->
<tool name="websearch"><query>tinyRAG streaming architecture</query></tool>

<!-- Arithmetic calculation -->
<tool name="calculate"><query>3 * (17 + 5) / 2</query></tool>

<!-- Wikipedia article -->
<tool name="wikipedia"><query>Retrieval-augmented generation</query></tool>
```

---

## Parsing Rules

1. **Detection threshold** — parsing starts when `<tool ` (with trailing
   space) is found in the buffer.
2. **Completion threshold** — execution starts only after `</tool>` is seen.
   Partial blocks are **never** executed.
3. **Strict validation** — the block must have a non-empty `name` attribute
   and a non-empty content element.  Invalid blocks are emitted as visible
   text and logged; execution is skipped.
4. **No nested tool tags** — nested `<tool>` elements are not supported and
   result in a parse error.
5. **No markdown wrapping** — the XML must not be wrapped in code fences or
   any Markdown construct.

---

## Safety Rules

| Rule | Enforcement |
|------|-------------|
| Partial XML must not trigger execution | `XMLParseState.Feed` holds the buffer until `</tool>` |
| Invalid XML is logged and ignored | `parseXMLBlock` returns `(_, false)` |
| Duplicate calls are skipped | Engine maintains a `seen` map per request |
| Max calls per request | `EngineConfig.MaxToolsTotal` (default 5) |
| Max calls per round | `EngineConfig.MaxToolsPerRound` (default 3) |
| Max continuation rounds | `EngineConfig.MaxContinuations` (default 3) |
| Per-tool timeout | `EngineConfig.ToolTimeout` (default 30 s) |
| SQL exposed to model | ❌ Never — only SELECT on `chunks` table |
| Generic HTTP tool | ❌ Not exposed — `url_fetch` returns plain text only |
| Shell commands | Disabled unless `allow_shell_exec: true` |
| nanoGo execution | Disabled unless `allow_nanogo: true` |

---

## Frontend Rendering

The raw XML is passed through in the streamed text so the frontend can
render it as a status card.  The `app.js` `replaceXMLToolBlocksWithCards`
function replaces `<tool>` blocks with:

```html
<div class="xml-tool-card" data-tool="TOOL_NAME" data-status="running">
  <span class="tool-icon">🔍</span>
  <strong>TOOL_NAME</strong>
  <span class="tool-query">CONTENT_PREVIEW</span>
  <span class="tool-status-badge">⟳</span>
</div>
```

The badge updates to `✓` (done) or `✗` (error) via the `tool_result` SSE event.

---

## SSE Events Related to Tool Calls

| Event        | Direction | Payload                                            |
|--------------|-----------|-----------------------------------------------------|
| `tool_start` | server→client | `{id, tool, query}` — tool execution started    |
| `tool_result`| server→client | `{id, tool, query, source, error, result_bytes}` |
| `route`      | server→client | `{mode, reason, hints}` — routing decision (debug) |
