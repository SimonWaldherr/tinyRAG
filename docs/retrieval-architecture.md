# Retrieval architecture and relation-aware roadmap

tinyRAG keeps vector retrieval as its fast, general-purpose baseline. Relation
awareness is an additive capability: it is only used when the question needs
connections between entities or evidence from more than one source.

## Current retrieval pipeline

1. A concise follow-up can be rewritten into a standalone retrieval query; the
   original wording remains the visible user question.
2. Dense retrieval supplies broad semantic recall.
3. For technical identifiers, a bounded full-text candidate source contributes
   literal matches. Candidates are deduplicated and receive only a small
   reciprocal-rank fusion bonus.
4. A configured semantic similarity floor admits primary evidence before
   quality signals are applied. A full-text candidate can bypass that floor
   only when it contains the complete queried technical identifier.
5. Role and ACL constraints are applied to every candidate source before a
   chunk can be used as evidence.
6. Quality signals and optional deterministic reranking order admitted
   evidence; they never turn an irrelevant source into answer context.
7. Document IDs, rather than display titles, scope deduplication, neighbor
   expansion, source listing, and source deletion. An ambiguous exact-title
   lookup falls back to ranked retrieval instead of combining documents by
   title alone.
8. Primary evidence is packed ahead of neighboring chunks into a bounded,
   citation-preserving context. A single omission marker makes any truncation
   visible in the debug payload and to the answer model.

The full-text path is deliberately fail-open: unsupported storage backends,
tokenizer limitations, or search errors leave vector retrieval unchanged.

The optional retrieval planner is also fail-open. Its action, query, count,
and threshold are bounded; a direct-answer decision still receives the
admitted cited evidence rather than bypassing grounding. Request cancellation
is propagated through retrieval follow-up checks, planning, and optional LLM
re-ranking so stale work does not continue after the client has gone away.

## Safe source refresh

For an update, tinyRAG prepares all replacement embeddings before touching the
currently visible source. The storage layer then writes the complete new chunk
set and removes the previous chunk IDs only after the write succeeds. A failed
embedding or write therefore keeps the prior source available instead of
leaving a partial or empty document behind.

## Relation-aware retrieval: staged design

### 1. Provenance-first relation records

Store a lightweight relation record rather than introducing a graph database:

```text
relation_id, subject_id, predicate, object_id,
document_id, chunk_id, source_version, confidence, observed_at
```

Every relation must point to the chunk that supports it. Canonical entity IDs
and aliases are kept separately, so spelling variations do not create a new
entity. Access control is inherited from the source chunk; a relation is never
returned when its evidence is not visible.

Phase one should only ingest relations that are explicit in structured imports
or document metadata. Extraction from free text can follow as an opt-in,
reviewable job with confidence thresholds and an audit trail.

### 2. Bounded subgraph expansion

At query time, vector and full-text retrieval provide the seed chunks. Entity
matching then expands only their immediate neighborhood (one hop by default,
two hops only for a clear multi-step question). Apply strict limits for nodes,
edges, source documents, and context characters.

The expansion returns short evidence statements such as
`entity A — relation — entity B`, each carrying its originating citation. It
does not expose an ungrounded path as a fact.

### 3. One bounded ranking pass

Merge chunk candidates and relation statements, then rank them once using the
existing quality, freshness, trust, feedback, and relevance signals. A single
optional model-based grading pass may reorder the small candidate set, but
retrieval must not depend on unbounded agent loops.

### 4. Answer-time safeguards

Only cite relations whose supporting chunks are in the selected context. If a
required path is incomplete or contradictory, the answer should say so rather
than infer a missing connection. The debug payload should expose the selected
evidence IDs and hop count, not hidden prompt text.

## Evaluation gates

Relation-aware retrieval should be enabled only after an evaluation set shows
an improvement over the current pipeline. Measure at minimum:

- evidence recall at `k` for ordinary, identifier, and multi-step questions;
- citation precision and the share of answers with fully supported paths;
- p50/p95 retrieval latency and context size;
- access-control tests across all candidate and expansion paths;
- degradation behavior when no relation records exist.

This sequence keeps the system portable: it works with local files, web
documents, APIs, and structured datasets without relying on a vendor-specific
directory, messaging system, or graph service.
