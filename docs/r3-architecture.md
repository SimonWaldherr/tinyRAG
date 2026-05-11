# R³ Architecture (Ranked, Responsible, Retrieval)

This document summarizes how tinyRAG applies governance and provenance to retrieval and ingestion.

## Goals

- Improve retrieval quality beyond cosine-only ranking
- Enforce access and sensitivity constraints before answer generation
- Make citations mandatory for RAG-grounded answers
- Keep ingestion auditable and resumable
- Keep integration-heavy imports out of the core binary

## Core Domain Objects

R³ introduces explicit domain concepts:

- `RetrievalUnit` — retrieval-ready chunk with source, ACL, sensitivity, trust/quality/freshness metadata
- `SourceRegistryRecord` — canonical source metadata (`r3_sources`)
- `RankingPolicy` — configurable weighted score model
- `ACLPolicy` — role/group access checks
- `SensitivityPolicy` — sensitivity classification and pseudonymization rules
- `Citation` — structured provenance emitted to context and UI
- `ImportJob` — idempotent, resumable ingestion telemetry (`r3_import_jobs`)
- `ToolPersistencePolicy` — per-capability persistence class
- `AuditEvent` — policy decisions and execution audit trail (`r3_audit_events`)

## Retrieval Flow

1. Retrieve semantic candidates
2. Apply ACL/role filtering
3. Compute weighted `R3Score` (semantic + trust + quality + freshness + feedback − penalties)
4. Deterministically rank and select context units
5. Build provenance-first context blocks (citations attached)
6. Enforce citation presence in final RAG-grounded answer

## Ranking Signals

`R3Score` is a weighted combination of:

- Semantic similarity
- Source quality defaults by type (official docs > wiki > tickets > chat)
- Trust level
- Freshness decay
- Feedback and content quality
- Penalties (e.g., sensitivity/conflict/noise ticket heuristics)

## Provenance and Safety

- Every selected chunk should carry structured citation metadata
- Restricted sources should only contribute policy-approved (masked) context
- No uncited RAG answers
- No hallucinated or unauthorized source references

## Ingestion Governance

The governed pipeline is modeled as stages:

`discover → extract → classify → sensitivity detect → pseudonymize → normalize → deduplicate → chunk → embed → index → audit`

Import jobs are designed to be:

- Cursor-aware
- Idempotent
- Resumable
- Telemetry-rich (processed/imported/skipped/errors/hash/version)

## tinySQL Tables

- Extended `chunks` with R³ metadata (backward-compatible evolution)
- `r3_sources` for canonical source registry
- `r3_import_jobs` for ingestion orchestration telemetry
- `r3_audit_events` for policy/audit traces

## Why this stays in core

R³ policy execution, retrieval ranking, citation enforcement, and audit controls are core behavior and remain inside tinyRAG.
