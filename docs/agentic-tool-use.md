# Agentic Tool Use

tinyRAG uses a bounded agent loop for questions that benefit from retrieval,
calculation, or external read operations. The goal is dependable evidence
collection, not unrestricted autonomous execution.

## Execution model

1. An optional planner proposes a small set of upfront tool calls.
2. The server validates every proposed call against the same policy as inline
   calls from the answer stream.
3. Independent approved calls run concurrently, within per-round and
   per-request limits.
4. Results are converted to a bounded evidence packet and supplied to the
   next answer round.
5. Inline calls can fill a genuine remaining evidence gap, but the loop has
   fixed continuation and tool budgets.

The planning budget is the smallest of the configured planner budget,
`MaxToolsPerRound`, and the remaining request-wide tool budget. Planning uses
the request's `auto_search` choice; it never grants a separate approval path.
Planner questions and responses also have fixed input and output bounds, so a
malformed planning response fails open to the normal answer path.

## Tool contract

Alongside retrieval and approved external read access, three stateless local
data tools are available:

- `json_query` reads JSON through a dot path such as `items.0.name`.
- `text_diff` produces a bounded line-oriented comparison of two texts.
- `regex_extract` returns bounded matches and capture groups from an RE2 pattern.

All three use structured `arguments`, process only the supplied data, and make
no network or write operations.

The established single-input XML form remains supported:

```xml
<tool name="websearch"><query>retrieval evaluation methods</query></tool>
```

Tools with several typed fields can use structured arguments:

```xml
<tool name="lookup_record">
  <arguments>{"record_id":"42","region":"eu"}</arguments>
</tool>
```

Connector inputs are schema-validated before execution. Legacy `<query>`,
`<url>`, `<source>`, and `<input>` calls are adapted to the same internal
contract. Calls have bounded XML, query, and argument sizes; oversized or
malformed blocks are visible as rejected text and cannot execute.

## Autonomous execution policy

Only registered capabilities that are safe to read are advertised to the
answer model and planner:

| Capability class | Autonomous behavior |
| --- | --- |
| Local retrieval and calculation | Allowed within the request budget |
| External read operations | Allowed only when `auto_search` is enabled |
| HTTP connector reads | GET/HEAD only, subject to the same consent |
| Read-only SQL connector capabilities | Allowed when their configured query is read-only |
| Read-only JSON-RPC connector capabilities | Allowed only with an explicit `read_only` declaration and the same consent |
| Code, shell, raw database access | Not autonomous |
| Ingestion, modules, and mutating connector calls | Not autonomous; require an explicit user-initiated workflow |

The policy also enforces role permissions and prevents unknown or hidden tool
identifiers from becoming callable. Tool aliases and whitespace variants share
a canonical deduplication key.

## Evidence boundary and provenance

Tool output is treated as untrusted evidence, not as instructions. Every
continuation packet contains a call ID, tool name, source label, phase, and
content hash, wrapped in explicit `BEGIN/END UNTRUSTED TOOL OUTPUT` delimiters.
The system instruction tells the model never to obey content found inside that
packet.

Evidence has both per-result and total request budgets. If data is shortened,
the continuation, SSE event, and telemetry record carry a truncation signal.
Connector output is rendered from its mapped result rather than duplicating a
raw response into the model context.

## Traceability

The SSE stream includes `plan`, `tool_start`, `tool_result`, and
`tool_skipped` events. Request telemetry records each outcome with its phase,
policy decision, duration, result size, content hash, and evidence-truncation
status. Results are collected in tool-call order so traces and evidence are
reproducible even when calls complete at different times.
