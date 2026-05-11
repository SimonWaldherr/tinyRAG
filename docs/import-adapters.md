# Modular Import Adapters (SharePoint, Dropbox, …)

For large enterprise ecosystems, avoid embedding every vendor SDK/package directly in tinyRAG.

## Problem

Adding many provider packages (SharePoint, Dropbox, Google Drive, Box, etc.) into the main application can:

- Increase binary size and startup footprint
- Expand dependency and vulnerability surface
- Complicate release and patch cycles
- Mix connector-specific concerns into retrieval core

## Recommended Pattern

Keep tinyRAG core focused on governed retrieval and expose ingestion interfaces for **external adapter tools**.

### Adapter responsibilities

An adapter service or CLI should:

1. Authenticate against one external source (e.g., SharePoint)
2. Discover and fetch documents incrementally
3. Normalize payloads into tinyRAG ingestion schema
4. Track external cursor/change token
5. Submit data to tinyRAG import/process APIs
6. Retry/backoff and report telemetry

### tinyRAG responsibilities

tinyRAG should:

- Validate and ingest payloads
- Apply R³ policies (sensitivity, pseudonymization, ACL, ranking metadata)
- Persist import job and audit events
- Serve retrieval/chat/tool orchestration

## Deployment Shapes

- **Sidecar**: adapter runs near tinyRAG in same environment
- **Worker pool**: one worker per provider/source tenant
- **Scheduled batch**: cron-triggered import jobs
- **Event-driven**: webhook-driven delta ingestion where provider supports it

## Contract-first ingestion

Use a stable contract between adapters and tinyRAG:

- Source identifiers and version/hash fields
- ACL/group metadata
- Sensitivity hints (optional)
- Content and provenance metadata
- Cursor/state token for resumable imports

## Practical examples

- `tinyrag-import-sharepoint` (separate module/repo)
- `tinyrag-import-dropbox` (separate module/repo)
- `tinyrag-import-filesystem` (can remain local/simple)

Each adapter can evolve independently without forcing core tinyRAG dependency growth.

## Security Notes

- Treat imported data as untrusted input
- Keep provider credentials out of tinyRAG core config where possible
- Enforce payload limits and timeouts in adapters and tinyRAG
- Keep audit parity: adapter logs + tinyRAG `r3_audit_events`

## Summary

Use tinyRAG as a governed retrieval engine, and implement provider-specific importers as separate tools/adapters.  
This keeps the core application lean while allowing broad source coverage.
