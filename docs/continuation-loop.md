# Continuation Loop

## Purpose

The continuation loop allows the LLM to use tool results to produce a
better final answer.  After the first streaming round ends, if any tools
were called and returned results, the engine starts a new LLM round with
the results injected into the conversation.

---

## Flow Diagram

```
Round 0 (initial answer):
  LLM streams → emits XML tool calls
  Tools execute concurrently
  Stream ends

Round 1 (continuation):
  msgs += {role:"assistant", content: round0_answer}
  msgs += {role:"user",      content: buildContinuationMessage(results)}
  LLM streams → usually produces final answer
  More tools may be called (if needed)

Round 2 (if needed):
  ... same pattern ...

Round N: MaxContinuations reached → stop
```

---

## Continuation Message Structure

```
Die folgenden Tools wurden ausgeführt. Verarbeite die Ergebnisse und erstelle eine präzise, vollständige Antwort.

### Tool: rag_knowledge
Query: product return policy
Ergebnis (1234 Zeichen):
[tool result text…]

### Tool: url_fetch
Query: https://example.com/docs
Fehler: connection refused
Hinweis: Erkläre dem Nutzer ehrlich, dass dieses Tool fehlgeschlagen ist. Erfinde keine Daten.

Regeln für die Antwort:
- Verwende alle verfügbaren Tool-Ergebnisse.
- Trenne lokales Wissen und Tool-Ergebnisse klar.
- Wenn ein Tool fehlschlug, sage das offen.
- Keine neuen <tool>-Blöcke emittieren, außer wenn unbedingt nötig.
- Kompakte, faktische Antwort ohne Marketing-Sprache.
```

---

## Deduplication

Within a single request, each unique `(tool, query)` pair is tracked in
a `seen` map.  If the model emits the same XML block twice (in the same
or different rounds), the second emission is skipped and logged as
`deduplicated: true` in telemetry.

---

## Cap Enforcement

```
per round:    len(pendingTools) >= MaxToolsPerRound → skip
per request:  totalTools >= MaxToolsTotal → skip
rounds:       round > MaxContinuations → break and emit [DONE]
```

When the cap is reached, `tel.FallbackReason = "max_continuations_reached"`.

---

## Honest Failure Handling

If a tool fails, the error is passed verbatim into the continuation message
with the instruction:

> "Erkläre dem Nutzer ehrlich, dass dieses Tool fehlgeschlagen ist. Erfinde keine Daten."

The model must **not** hallucinate data when a tool fails.  If it does,
the system prompt's anti-hallucination instructions apply.

---

## Feature Flag

The continuation loop is always active.  To disable it for a specific
deployment, set `MaxContinuations: 0` in `EngineConfig`.  In that case,
tool results are still collected and emitted as SSE events, but no second
LLM round is started.

Future work: expose `max_continuations` as a per-request override and as
a settings field in `appSettings`.
