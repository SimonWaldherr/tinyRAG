# R3 — Rubix Ranked RAG

A provenance-first Retrieval-Augmented Generation system for mailbox and
document knowledge bases, built on the same architecture as
[tinyRAG](https://github.com/SimonWaldherr/tinyRAG): a single Go binary,
[tinySQL](https://github.com/SimonWaldherr/tinySQL) as the embedded vector
store, local LM Studio embeddings, selectable chat providers (LM Studio,
Azure OpenAI, OpenAI, OpenRouter, Claude and Gemini), and an embedded browser
frontend.

**Auf Deutsch:** [`ANLEITUNG.md`](ANLEITUNG.md) fasst alles zum Betreiben,
Konfigurieren, Aktualisieren und Erweitern von R3 auf Deutsch zusammen — die
englischsprachigen Dokumente hier und in `docs/` bleiben für tiefere
technische Begründungen die Referenz. Für Management/Nicht-Technik: siehe
[`LIESMICH.md`](LIESMICH.md) (worum es geht, aktueller Stand, offene
Risiken) und [`docs/PROJEKTPLAN.md`](docs/PROJEKTPLAN.md) (Phasenstatus,
nächste Schritte).

What sets R3 apart from a plain vector-search RAG:

- **Ranked retrieval.** Every answer is retrieved with a hybrid score —
  vector similarity, real BM25 keyword match (tinySQL's `FTS_SEARCH`, or —
  since both `vectorStore` backends now implement the same
  `ftsKeywordScorer` capability — a duplicate FTS5 virtual table for the
  SQLite backend, see [`vectorstore_sqlite.go`](vectorstore_sqlite.go))
  and document recency — instead of cosine similarity alone (see
  [`rank.go`](rank.go)). A perfectly-matching but three-year-old email
  should not automatically outrank an exact keyword hit from last week.
  Keyword scoring is a true hybrid **union**, not just a re-rank of the
  vector candidates: a chunk that's an exact BM25 match (an error code, a
  part number, a name) but semantically distant is fetched via the
  backend's `chunkByKey` capability and cosine-scored alongside the vector
  pool, instead of only ever being reachable when it also happened to rank
  in the vector top-N. Every knob in that score is admin-configurable from
  Settings → "Retrieval / Ranking": minimum vector similarity to even
  consider a chunk, max sources per answer, an optional cap on how many of
  the K result slots a single source may occupy (`max_hits_per_source`, a
  diversity guard against one long document crowding out every other
  source), how many chunks before/after a matched chunk get pulled in as
  surrounding context (a source matched at several far-apart positions now
  gets a window around *each* match, joined by a `[…]` gap marker), and
  per-chunk/per-sibling character caps so one huge document can't crowd out
  everything else in the context window — all with backward-compatible
  defaults if left untouched.
- **Conversation-aware retrieval.** Retrieval normally only ever sees the
  current question's own text — a natural follow-up like "und bei
  Kistenpfennig?" carries almost no vocabulary of its own. An optional,
  opt-in pre-flight step ([`query_rewrite.go`](query_rewrite.go), Settings
  → "Konversationsbewusste Suchanfrage") uses the preceding chat turns to
  rewrite such a follow-up into a self-contained search query *before*
  `rankedSearch` runs — one extra, cheap LLM call, fail-open on any error;
  the question shown in the UI and sent to the main answer call is never
  changed, only what's searched for. Skipped automatically (no extra call,
  no added latency) whenever there's no conversation history yet to
  rewrite against — a fresh question's first turn is unaffected.
- **Structure-aware chunking.** [`chunk.go`](chunk.go) splits text on
  paragraph boundaries, keeps markdown tables intact (repeating the header
  row when a table must split), and prefers sentence ends over mid-word
  cuts when a paragraph must be split. Chunks under a markdown heading
  (`##`+) carry a `[Abschnitt: A > B]` breadcrumb line so a chunk from deep
  inside a long document still tells the embedder/BM25/LLM which section it
  belongs to — stripped again when a source's full text is reassembled for
  the citation popup.
- **Provenance on every chunk.** Every stored chunk records *which source*
  it came from, *which import run* (`load_id`) produced it and *when*
  (`loaded_at`), so every answer can cite its source and re-importing a
  changed file/mailbox atomically replaces the old chunks instead of
  duplicating or orphaning them (see [`store.go`](store.go)).
- **A chunk viewer to inspect all of that directly.** The "Chunks" tab
  (see [`chunks.go`](chunks.go)) lists every stored chunk with full-text
  search, filters (source type, source-id substring, embedding model) and
  sortable columns — including a live *freshness* score computed with the
  exact same recency-decay formula `rankedSearch` uses, so you can see why
  a chunk would rank the way it does, not just that it exists.
- **A file-based, web-editable system prompt.** `prompts/index.md` (always
  applied) plus any number of `prompts/skill_*.md` files (domain-specific
  instructions, selected per question by cheap tag-matching — see
  [`skills.go`](skills.go)) are edited from the "Prompts" admin tab and
  take effect on the very next question, no restart. Git-versionable as
  plain Markdown + one small manifest.
- **Citations only for what actually grounded the answer.** A chunk
  reaching the selected chat provider's context doesn't automatically become
  a citation:
  `filterCitations` (see [`rank.go`](rank.go)) only returns sources whose
  `[Qn]` marker actually appears in the model's own answer text, so a
  candidate the vector search retrieved but the model never referenced
  isn't shown as if it had been. On top of that, `settings.
  source_visibility` lets an admin mark whole source types (e.g.
  `pst_email`) as never shown as a citation at all — the content still
  informs the answer, only the citation chip/source name is suppressed,
  for privacy-sensitive sources that should ground answers without being
  named (see "Security / privacy considerations" below).

## Quick start

```bash
go mod tidy && go build .
./R3 -addr :8090 -storage-path r3-data \
     -url http://localhost:1234 -chat mistralai/ministral-3-3b -embed text-embedding-nomic-embed-text-v1.5
```

Then open `http://localhost:8090`. This assumes a local OpenAI-compatible
server (LM Studio, Ollama, vLLM, …) is already running at `-url` with the
given chat and embedding models loaded. Cloud profiles can be configured for
chat in Settings; embeddings remain local so changing chat providers never
invalidates the stored vector space. See "LLM backends" below for details.

### Example: import a PST, ask a cited question

1. Start R3 as above, open `http://localhost:8090`, click "Admin-Login"
   (no password required unless `R3_ADMIN_PASSWORD` is set).
2. Go to the "Import" tab, upload a `.pst` mailbox export. Each message is
   chunked, embedded and stored as its own source
   (`pst:<file>:<folder>:<message-id>`).
3. Back on the main chat view, ask a question about the mailbox content,
   e.g. "What did we agree with Vendor X on delivery dates?".
4. The answer streams in token-by-token and cites the specific message(s)
   it was grounded in; open the "Chunks" tab to inspect exactly which
   chunks were retrieved and how they scored.

See also: [`docs/API.md`](docs/API.md) (using R3 as a JSON API, an
OpenAI-compatible `/v1/chat/completions` endpoint, MCP server or CLI from
outside the browser UI), [`docs/VECTOR_DB.md`](docs/VECTOR_DB.md)
(does tinySQL hold up, or should this be SQLite+sqlite-vec/PostgreSQL+pgvector?),
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) (deployment checklist: packaging,
systemd unit, backup/observability), and
[`docs/HARDENING_PLAN.md`](docs/HARDENING_PLAN.md) (planned next steps:
Sent-folder-only import with request/response pairing, R3Score ranking, a
safety-hardened draft-generation UI — not yet implemented), and
[`docs/ENTERPRISE_READINESS.md`](docs/ENTERPRISE_READINESS.md) (audit
trail, secrets management, observability, HA/backup, data governance —
also not yet implemented).

`docs/` is `.gitignore`d — those files are local working notes that exist
only on machines that already have them, not in a fresh clone. Anything
learned there worth keeping belongs in this README (or `ANLEITUNG.md`)
instead, since those are the only deployment-relevant documents that
actually travel with the repo. `docs/openapi.json` is the one exception
(explicitly un-ignored in `.gitignore`): `main.go` embeds it at build
time to serve `GET /api/openapi.json`, so it has to ship with the repo or
the build breaks on a fresh clone.

A running instance also serves [`GET /llms.txt`](llms.txt) — a short,
agent-oriented summary of what R3 is and how to call its API (the
[llmstxt.org](https://llmstxt.org) convention, analogous to
`robots.txt`), for LLM agents/tools that land on the instance directly
rather than reading this repo.

For humans, `GET /api/docs` is an interactive OpenAPI/Swagger-style
browser for the same spec — endpoints, request/response schemas, and a
working "Try it out" panel that fires real requests against the running
instance (`web/apidocs.html`/`web/apidocs.js`). Deliberately not the
reference swagger-ui bundle: that's a sizeable vendored third-party JS
dependency (or a CDN dependency) for a four-endpoint API, so this is a
small, dependency-free viewer instead, in the same plain-JS/no-build-step
style as the rest of the frontend — the spec itself stays fully standard
OpenAPI 3.0, so it can still be pasted into the real
[Swagger Editor](https://editor.swagger.io/) via the link on that page if
that specific tool is what's wanted.

## Installing dependencies (Debian)

Only Go and a reachable LLM backend are needed to build and run R3 at all
(see "Quick start" above). Everything else below is optional, gated behind
`allow_shell_exec` in Settings, and only ever invoked for the specific
feature that needs it — a missing/disabled binary fails just that one
action (file import, image OCR, voice transcription) with a clear error,
never the rest of R3.

**Go toolchain.** Debian's own `golang-go` package on `stable` is often too
old to parse `go.mod`'s directive (this repo's own `AGENTS.md` documents
hitting exactly this with a stray Go 1.16) — install a current release
directly from [go.dev/dl](https://go.dev/dl/) instead of relying on the
distro package:

```bash
curl -LO https://go.dev/dl/go1.26.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc   # or /etc/profile.d/ for every user
source ~/.bashrc
go version   # should show >= 1.23
```

**A local LLM backend without a GUI.** LM Studio traditionally assumes a
desktop environment; on a headless Debian server,
[Ollama](https://ollama.com) is a common alternative (its own install
script, no Debian package) — it has spoken the same OpenAI-compatible
`/v1/...` API R3 targets via `-url`/`profiles.local.base_url` for some
time now:

```bash
curl -fsSL https://ollama.com/install.sh | sh
ollama pull <chat-model>       # e.g. mistral-nemo, llama3.1
ollama pull <embedding-model>  # e.g. nomic-embed-text
```

**`markitdown`** (Office/PDF/RTF/ODT/EPUB import, optional audio/video via
`markitdown[all]`):

```bash
sudo apt install python3-pip pipx
pipx install markitdown           # or: pipx install 'markitdown[all]'
```

Debian 12/13 refuse a system-wide `pip install <package>` by default since
the PEP 668 "externally managed environment" change — `pipx` (an isolated
environment per program, automatically on `PATH`) sidesteps that cleanly.
`pip install --break-system-packages markitdown` or a dedicated
`python3 -m venv` also work.

**`tesseract`** (OCR for image uploads/attachments — scans, photographed
documents):

```bash
sudo apt install tesseract-ocr tesseract-ocr-deu
```

`tesseract-ocr-eng` ships in the base package already (default language
`deu+eng`); add further language packs as needed, e.g. `tesseract-ocr-fra`.

**`ffmpeg`** (audio normalization for voice input, plus
`markitdown[all]`'s video support):

```bash
sudo apt install ffmpeg
```

**`whisper.cpp`** (local voice-to-text transcription, "voice input" — no
Debian package, build from source):

```bash
sudo apt install build-essential cmake git
git clone https://github.com/ggml-org/whisper.cpp.git
cd whisper.cpp
cmake -B build && cmake --build build -j --config Release
sudo install -m 0755 build/bin/whisper-cli /usr/local/bin/whisper-cli
```

Then download a model (stays entirely local — R3 never fetches one
itself):

```bash
./models/download-ggml-model.sh small   # or tiny/base/medium/large-v3, depending on RAM/accuracy
```

Point Settings → Import at the result: `whisper_bin` (path to
`whisper-cli`, if not on `PATH`), `whisper_model` (path to the downloaded
`.bin` file), and optionally `whisper_language` (e.g. `de`) — see "Data
sources" above for the additional tuning fields (threads, beam size, VAD,
flash attention, max concurrency).

## Deployment

R3 already runs as an MVP in production at
`http://svde-vld-ai01.zitec-intern.de:8090/` (`10.203.0.5`), installed under
`/mnt/application/R3` on that Debian host (see also `ANLEITUNG.md`'s status
note).

- **OS/kernel**: Debian 13 "trixie" (13.4), kernel `6.12.74+deb13+1-amd64`,
  16 cores, 62 GiB RAM, `/mnt/application` has 230G free of 295G.
- **SSH access**: port `4022` (not the default 22).
- **Getting the binary/data onto the server**: no CI/CD pipeline — files
  (binary, `settings.json`, staged data) are uploaded via FileZilla/SFTP.
- **Toolchain already present on the host** — no provisioning needed for
  either: `go1.24.4 linux/amd64` (`/usr/bin/go`, satisfies the `go 1.25`
  `go.mod` directive's ≥1.23 build requirement) and Python 3.13.5 + pip3
  (`markitdown` can be `pip install`ed directly for Office/PDF import).
- **A local OpenAI-compatible server is already running** on
  `127.0.0.1:1234` — the exact default `-url` in "Quick start" above — so
  the local LLM profile works against this host out of the box, without
  standing up a separate LM Studio/Ollama instance first.
- **Port `8090`** (R3's default `-addr`) is free today, but the host runs
  many *other* services in the same range — `8080`-`8082`, `8085`, `8087`-
  `8089` are already `LISTEN`ing (plus `3306`, `443`, `6379`, `9000`/`9001`,
  `9380`-`9384` via Docker) — worth double-checking with `ss -tlnp` before
  every new deployment on this host, not just once.
- **No host firewall and no reverse proxy currently sit in front of R3** —
  `ufw` isn't installed, `iptables`'s `INPUT` chain policy is `ACCEPT` with
  no rules, and no nginx/Apache/Traefik/Caddy binary is present on the
  host either. Combined with R3 having no built-in authentication (see
  "Security / privacy considerations" below), anything reachable to
  `10.203.0.5:8090` today can reach R3 unauthenticated — this is the
  single most important gap to close before real mailbox data goes in, not
  a theoretical one.
- **How the running instance actually gets built/updated today**: the full
  Go source tree is uploaded via FileZilla (not just a pre-built binary
  from a separate machine) and built directly on the host with the
  already-installed `go1.24.4` toolchain — `go build .` there, per
  "Running" above, needs no cross-compilation step.
- **Which OS user R3 should run as long-term is still an open decision** —
  today's instance was last started as `root`; moving to a dedicated
  unprivileged service user (e.g. `r3`, owning its own data directory) is
  planned but not yet done. Update this bullet once that's actually
  decided, since it directly affects the blast radius of anything that
  compromises the R3 process.
- **`/mnt/application`** is a shared, larger-than-root mounted disk on that
  host used for several other locally-run services besides R3, each in its
  own subdirectory: `datadock`, `docker`, `liveCalc`, `llmflow`…`llmflow6`,
  `lmstudio`, `productcrawler`, `promptcron`, `promptcron1`, `ragflow`,
  `sendmail_test`, `tickets`, `tinyRAG` (`lost+found` is a filesystem
  artifact, not an app). Several of these — `llmflow6`, `promptcron`,
  `tinyRAG` — are the sibling projects R3's architecture/conventions were
  explicitly borrowed from (see "Credits" and "LLM backends" above), so this
  is the same host they already run on, not a fresh environment.
- See [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for the full phase-by-phase
  checklist (packaging, systemd unit, backup/observability). That document
  was written before the access details above were confirmed and still
  frames some of them (e.g. "how does the binary get onto the server") as
  open questions — reconcile it with this section and with `ANLEITUNG.md`'s
  "already running" status when next touched.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1 | PST import ([`pst.go`](pst.go), via [go-pst](https://github.com/mooijtech/go-pst)), multi-format file import ([`extract.go`](extract.go), [`ingest.go`](ingest.go)), browser Q&A with ranked, cited retrieval | implemented |
| 2 | Live/external connectors: SharePoint, Exchange Online (Graph) and on-prem/generic IMAP mail (incl. attachments), Microsoft Teams channels, Confluence, Jira, generic websites, plus a live MSSQL query tool | implemented, see "Data sources" and "Live tools" below |
| 3 | HITL draft replies: detect new mail, propose a grounded reply, save to the mailbox's Drafts folder — **never auto-send** | generation logic implemented ([`draft.go`](draft.go)); triggering it automatically off new IMAP/Graph mail (rather than on demand) is not wired up |

## Data sources

Every source below funnels through the same write path
(`ingestDocument` in [`ingest.go`](ingest.go)) — see "Provenance & updates"
below for the shared re-ingest/idempotency guarantee.

Every import request below (upload, folder, PST, SharePoint (+ delta-sync),
Exchange, IMAP, Teams, Confluence, Jira, Freshservice, web) accepts an
optional `"dry_run": true` field in its JSON body (the Admin UI's Import tab
exposes this as a single "🧪 Dry-Run" toggle for the whole page). A dry run
still does everything up to the point of writing: extraction, chunking, and
the content-hash unchanged/changed check all run for real, so the reported
chunk/skip counts are accurate — but nothing is embedded or written to the
vector store, and side effects that normally accompany a real import (a
SharePoint delta-sync's `delta_link`, an IMAP mailbox's last-seen UID, an
uploaded file's optional "keep original" copy) are not persisted either.
Every response's result object gets `"dry_run": true` so the caller can tell
a simulated run from a real one. Combine with `-verbose` (below) to see the
exact per-message ingest decisions a dry run made. The Web and RSS cards
also expose a dedicated **"Probelauf (testen)"** button that forces a dry
run regardless of the page toggle — the closest equivalent to the other
connectors' "Verbindung testen" for these two per-URL sources, which have
no standing connection config of their own.

**Throttling & per-run limits** ([`import_limits.go`](import_limits.go),
Settings → Import → "Drosselung & Limits"). Three global knobs guard every
connector against a single run pulling an unbounded amount or hammering the
upstream:

- **Max items per run** (`max_items_per_run`, default 500, hard ceiling
  100 000) — the cap against "one click ingests 100 000 tickets/mails". A
  run that hits it stops and says so. Resumable sources continue from
  exactly where they stopped on the next run/scheduler tick: IMAP keeps the
  *lowest* new UIDs and advances `last_uid` by that batch; SharePoint's
  delta sync returns the current page's `nextLink` as the resume cursor. So
  a large backlog drains in bounded chunks — nothing is skipped.
- **Request delay** (`request_delay_ms`, default 0) — a deliberate pause
  between an import's per-item outbound requests, i.e. *proactive*
  throttling, on top of the existing *reactive* 429/5xx backoff
  (`doWithRetry`, graph/shop retry loops). e.g. 200 ms ≈ 5 req/s.
- **Preview size** (`preview_limit`, default 50, ceiling 1000) — how many
  candidates the "preview, then select" listings (Confluence/Jira/
  Freshservice/Exchange/Teams) fetch per request; also bounded per
  connector by that API's own page size. Preview stays a single request —
  no unbounded server-side pagination loop, which is exactly the behavior
  the cap exists to prevent.

The pacer (cap + ctx-aware delay) is wired into every import loop; the
Shop live tool additionally caches identical search results and reuses a
pooled HTTP transport (see "Live tools" below).

**Multiple named connections per connector.** SharePoint, Exchange/Graph,
IMAP, Teams, Confluence, Jira and Freshservice each accept any number of
independently-configured connections (e.g. two Exchange mailboxes, two
SharePoint sites) instead of a single fixed one — the Settings UI shows a
card list per connector kind with a "+ Verbindung hinzufügen" button. Every
card carries a "⋮" menu: **Test** (against the card's current, unsaved
values), **Duplicate** (copies all fields including credentials but forces
the new card's `enabled` off, so duplicating never silently doubles a
running import), **Export** (downloads the card as `.json` with secret
fields blanked but `*_env` variable names kept), **Import** (loads a
previously exported `.json` back into that one card; rejects a file from a
different connector kind), and **Remove**. A connection's `name` is
internal only (never sent to the remote system) but must be unique within
its connector kind; an existing single-connection `settings.json` is
migrated automatically to a one-item list (`name: "default"`) on first
load. When more than one connection of a kind exists, an import/preview
request needs an explicit `connection` field naming which one to use (a
single configured connection is still picked implicitly, so the common
case is unaffected); the Import tab itself doesn't yet expose a connection
picker for the multi-connection case, only the API does.

**Discover: recursive structure preview.** SharePoint, the Folder connector
and Exchange each additionally expose a "Discover" action that walks the
source's structure (site/library/folder tree, or mailbox folder tree)
recursively and returns it as a browsable preview before anything is
imported — useful for scoping a large SharePoint site or mailbox down to
the actual subset worth importing instead of guessing paths blind.

**Manual single-source refresh** ([`sourcerefresh.go`](sourcerefresh.go)) —
the Sources tab's "Neu laden" button re-fetches one already-imported source
from its original system and re-runs it through the normal content-hash
compare-and-replace path, without re-running that connector's whole
import/delta-sync. Implemented for the three SharePoint `source_kind`s
(`sharepoint_file`, `sharepoint_page`, `sharepoint_link`) and for `web_page`;
every other kind returns a clear "not supported yet" error rather than
pretending to refresh something it can't actually re-fetch (a PST mailbox's
uploaded file isn't kept server-side after import, for instance, so there's
nothing to re-download from for a `pst_email` row).

**Email family expansion at answer time**: a mail and its attachments are
separate sources, so a query matching only the mail body would otherwise
never surface the offer PDF attached to it (or, matching only the
attachment, lose the mail that explains its context). `assembleContext`
(`rank.go`, `expandEmailFamilies`) therefore pulls the not-yet-cited
siblings of every email-family hit — parent mail ↔ attachments, linked by
their shared `source_id` prefix — into the LLM's context as additional,
individually cited blocks. Siblings pass the same `source_access`
department check as the hits themselves, and are capped (6 siblings, 4000
chars each) so a huge attachment can't flood the context window.

- **PST/OST mailbox export** — upload a `.pst` or `.ost` file; every message becomes its
  own cited source (`pst:<file>:<folder>:<message-id>`). Re-uploading the
  same export only re-embeds messages whose content actually changed.
  Attachments are extracted and ingested too, each as its own source
  (`source_kind` `pst_attachment`, `source_id` suffixed
  `:attachment:<index>:<filename>`) through the same `extractText`
  pipeline file uploads use — so a `.docx`/`.pdf` attachment needs
  `allow_shell_exec` (markitdown) the same way an uploaded one would.
  Deleting the parent PST/email also deletes its attachments, since they
  share its `source_id` prefix. The actual import runs as a detached
  background job (`POST /api/import/pst` returns a `job_id` immediately;
  `pst.go`'s `pstImportJob` registry), not tied to the browser request —
  closing the tab or a network blip no longer kills a half-hour mailbox
  import midway. The Import tab polls `/api/import/pst/status` for
  progress and offers `/api/import/pst/cancel`; reopening the tab
  reattaches via a `job_id` saved in `localStorage`, and a **different**
  browser/machine with no such entry instead calls `/api/import/pst/jobs`
  to discover and attach to whatever's still running.
- **Files** — native extraction for `.txt .md .csv .tsv .json .jsonl .ndjson
  .xml .html .htm .log .yaml .yml .toml .ini .cfg .conf .sql .go .py .js
  .ts .eml .mbox .mht .mhtml .vcf .ics .srt .vtt`.
  Office/PDF formats (`.docx .doc .docm .pptx .ppt .pptm .xlsx .xls .xlsm
  .pdf .rtf .odt .ods .odp .epub .msg`) are converted via Microsoft's
  [markitdown](https://github.com/microsoft/markitdown) CLI, following the
  same "shell out to an existing tool" pattern tinyRAG uses for its SQL
  connectors. Install it with `pip install markitdown` and enable
  `allow_shell_exec` in Settings — it is off by default so a fresh
  checkout never executes an external binary without explicit opt-in.
  `.zip`, `.tar`, `.tar.gz`, `.tgz` and `.7z` archives import supported text
  documents recursively. ZIP/TAR/GZIP are parsed in-process; `.7z` uses the
  configured `7z` binary. Entry count, path traversal, symlinks and expanded
  size are bounded to avoid archive bombs.
  Audio (`.wav .mp3 .m4a .flac .ogg .aac`) uses MarkItDown's optional
  transcription support. Video (`.mp4 .webm .mov .mkv .avi .flv`) is reduced
  to a mono WAV track through the configured `ffmpeg` binary first. Both
  paths require `allow_shell_exec`.
  For scanned PDFs and complex Office tables, an optional HTTPS
  `markitdown_docintel_endpoint` enables MarkItDown's Azure Document
  Intelligence mode; it is off by default because document contents leave
  the server and Azure usage may incur charges.
  Image files (`.png .jpg .jpeg .gif .bmp .tif .tiff .webp`) are OCR'd via
  tesseract on upload and folder import too (same `allow_shell_exec` gate,
  same path a mail attachment's inline screenshot or scanned invoice
  already used) — a folder walk skips images quietly while OCR is
  disabled, rather than erroring once per photo.
- **Local voice input** ([`whisper.go`](whisper.go),
  [`handlers_voice.go`](handlers_voice.go)) — Chat has a push-to-talk
  button (`POST /api/voice/transcribe`). The recording is streamed straight
  into a private temp file (never buffered as a whole `[]byte`), always
  normalized to 16kHz/mono/PCM through the configured `ffmpeg` binary
  regardless of its claimed file extension (a caller-supplied ".wav" isn't
  trusted at face value), then transcribed by the configured
  whisper.cpp-compatible CLI (`import.whisper_bin`, default `whisper-cli`;
  `import.whisper_model`/`import.whisper_language` as usual). Nothing is
  persisted anywhere — not the audio, not the transcript, not in chat
  history or the vector store — and both binaries only ever run with
  `allow_shell_exec` on; R3 never downloads a model itself. A server-wide
  semaphore (`import.whisper_max_concurrent`, default 2) bounds how many
  transcriptions run at once, since each reloads its full model from disk;
  an independent per-anonymous-caller rate limit
  (`api.guest_voice_rate_limit_per_minute`) mirrors `/api/ask`'s own guest
  limit. Performance-tuning flags — `whisper_threads`, `whisper_beam_size`
  (1 = greedy decoding, the standard low-latency trade for a short clip),
  `whisper_flash_attn`, and `whisper_vad`/`whisper_vad_model` (skip silent
  stretches before transcribing) — are all opt-in and off by default,
  leaving whisper.cpp's own defaults in effect until configured. The
  response's `language` field reports what whisper.cpp actually used — the
  configured code, or its own auto-detection result parsed from stderr —
  rather than blindly echoing back the (possibly empty) setting. The
  browser side tries a cross-browser MediaRecorder mimeType candidate list
  (Chrome/Firefox default to WebM/Opus, Safari 14.5+ to MP4/AAC), shows a
  recording timer with a client-side auto-stop ceiling, and supports
  Escape-to-cancel; a successful transcript is inserted into the question
  field for review rather than auto-submitted, so a mis-transcription gets
  the same glance-before-send treatment as anything typed.
- **Server folder** — recursive import of a local directory (admin use).
- **R3's own source code** ([`selfsource.go`](selfsource.go),
  `POST /api/import/self-source`, admin-only) — "yo dawg, I heard you like
  RAG": loads R3's own Go/JS/CSS/HTML/Markdown source into its own vector
  store as `source_kind` `r3_source`, so it can answer questions about its
  own implementation with a citation pointing at the actual file. Always an
  explicit, one-off admin action — never automatic on startup or a
  schedule. Scoped to the server process's own working directory with a
  hard-coded, defense-in-depth exclusion list: `.git`, `external/`
  (reference/scratch projects, not part of R3), `docs/` (locally-sensitive
  working notes), every `r3-data*`/`verify*` storage fixture, and — most
  importantly — any `settings*.json`/`*.env`/`credentials*`/`*.key`/`*.pem`
  file are all excluded regardless of extension, since `settings.json`
  holds live secrets (LDAP bind credentials, API keys) in a real deployment
  that must never end up embedded in a citable knowledge base.
- **SharePoint** ([`sharepoint.go`](sharepoint.go)) — browse a site's
  document library via Microsoft Graph (app-only, client-credentials
  auth) and import selected files. Also imports a site's **Site Pages**
  (`.aspx` pages, extracted to their text content including the sidebar
  "vertical section", not raw markup) and supports importing a single file
  via a pasted **ShareLink** URL, for the common case of "someone shared me
  a link" rather than browsing the whole library. Delta-sync is
  self-healing: a `410 Gone` (Graph's documented response for an
  expired/invalidated resume token) triggers an automatic full re-walk
  instead of failing identically forever, and renamed/moved items are
  reconciled by tracking each item's Graph `id` (not just its path) across
  runs, so a rename doesn't leave the old path's chunks orphaned in the
  vector store. File downloads retry through the same 429/5xx backoff
  every other Graph call gets, falling back to the `/content` endpoint
  when a listing's `downloadUrl` has already expired (it's documented to
  be short-lived) — see `graph.go`/`sharepoint.go`'s
  `spDeltaSync`/`spDownloadItem`. The site-ID lookup itself is cached
  (`spSiteID`), so a multi-page import doesn't pay a repeat Graph round
  trip per page/item just to re-resolve the same site.
- **OneDrive for Business** ([`onedrive.go`](onedrive.go)) — import one
  explicitly configured Microsoft Graph `drive_id` (optionally narrowed to
  a folder) with resumable delta sync. Renames, moves and deletions are
  reconciled by Graph item ID; stored continuation links are host-pinned to
  Graph before use. Use a dedicated, least-privilege app: app-only OneDrive
  permissions can be broader than SharePoint's `Sites.Selected`.
- **Outlook / Exchange Online** ([`graphmail.go`](graphmail.go)) — list and
  import messages from a shared/service mailbox via Microsoft Graph,
  including `fileAttachment`s (`source_kind` `outlook_attachment`) fetched
  from each message's `/attachments` endpoint and base64-decoded (skipped
  entirely, no extra Graph call, for a message whose `hasAttachments` is
  false); `itemAttachment`/`referenceAttachment` (forwarded messages,
  cloud-file links) have no retrievable bytes via this API and are
  skipped. The scheduled sync (see "Background scheduler" below) is
  incremental via a per-connection `receivedDateTime` watermark, so a
  backlog larger than one run drains across consecutive runs instead of
  only ever seeing the newest preview page.
- **IMAP** ([`imapmail.go`](imapmail.go)) — on-prem Exchange or any
  IMAP-reachable mailbox; "Import jetzt" fetches everything newer than the
  last imported UID (`imap:<account>:<mailbox>:<uid>`), no preview/select
  step needed since UIDs are already deduplicated. Attachments are parsed
  out of the message's MIME structure (`source_kind` `imap_attachment`)
  and ingested the same way PST attachments are.
- **Microsoft Teams** ([`teams.go`](teams.go)) — import a channel's posts
  via Microsoft Graph, following pagination beyond the newest page. Each
  imported post's **thread replies** ride along in the same document (in
  conversation order, deleted replies excluded), so an answer buried in
  reply #7 is as searchable as the opening post, and the document's date
  reflects the thread's most recent activity rather than only its opening
  post. How many replies ride along per thread is capped by
  `import.teams_max_replies_per_thread` (default 200) — bounds both a very
  active thread's document size and the Graph request count (each 50-reply
  page costs one more call).
- **Confluence** ([`confluence.go`](confluence.go)) — import pages from an
  Atlassian Confluence Cloud space via the REST API (Basic auth, email +
  API token).
- **Jira** ([`jira.go`](jira.go)) — import issues from an Atlassian Jira
  Cloud project via the REST API v2 (same Basic auth, email + API token —
  often the same Atlassian account/token Confluence uses). REST v2 rather
  than v3 deliberately, so `description` comes back as a plain
  wiki-markup string instead of Atlassian Document Format's nested JSON.
- **Freshservice** ([`freshservice.go`](freshservice.go)) — import tickets
  from a Freshservice instance's REST API v2, HTTP Basic-auth'd with an
  account API key as the username and the literal string `X` as the
  password (Freshservice's documented convention). The only connector that
  can *also* run unattended: `sync_interval_minutes > 0` has
  [`scheduler.go`](scheduler.go) pull and ingest every ticket on a timer in
  addition to the manual "Import jetzt" — see "Background scheduler" below.
- **GitHub** ([`github.go`](github.go)) — import the README plus Issues and
  Pull Requests from one GitHub.com or GitHub Enterprise repository using a
  fine-grained read-only token. Large first imports resume by page; later
  cycles use GitHub's updated-since listing. Requests are HTTPS-only,
  response-bounded and do not follow redirects.
- **SAP S/4HANA OData** ([`sap_s4.go`](sap_s4.go)) — import one explicitly
  configured, read-only OData entity set (V2 or V4). The admin allow-lists
  ID, title and content fields, which R3 sends as `$select`; continuation
  and delta links are accepted only on the configured HTTPS SAP host.
- **Generic websites** ([`webimport.go`](webimport.go)) — paste one or more
  URLs; no settings/auth needed, but see that file's SSRF-considerations
  doc comment before exposing this to anyone other than a trusted admin.
  Supports recursive crawling (follow same-origin links to a configured
  depth) instead of only the pasted URL itself.
- **RSS/Atom feeds** — paste a feed URL; each entry is imported as its own
  cited source. Shares the same "🧪 Dry-Run" toggle and per-URL "Probelauf
  (testen)" button as the Web import card, since neither has a standing
  connection config of its own to run "Verbindung testen" against.

## Live tools (function calling)

- **Shop** ([`shop.go`](shop.go)) — not an import source: when enabled, the
  chat model can call `search_shop_items` to look up live product data from
  Rubix's own shop (`shopConfig.BaseURL`, defaults to
  `https://de.rubix.com`). Auth normally exchanges the configured
  username/password for a bearer token via `POST /rest-api/v1/tokens`
  (`shopTokenCache`), but the shop's own login endpoint sometimes answers
  HTTP 200 with no JSON token field and a `Set-Cookie` instead —
  `shopAccessToken` detects that shape and falls back to a **cookie-session**
  login, caching the resulting cookie jar the same way it caches a bearer
  token, and re-authenticating (once) on a `401` mid-request
  (`reauthed` in shop.go). Results are cached per identical query and reuse
  a pooled HTTP transport (see "Throttling & per-run limits" above).
- **MSSQL** ([`mssql.go`](mssql.go)) — not an import source: when enabled,
  the chat model can call a `query_mssql` tool at answer time to run a
  live, read-only `SELECT` against a configured SQL Server database
  (see [`llm.go`](llm.go)'s `chatWithTools` for the generic tool-calling
  round-trip this is built on). Defense in depth, weakest to strongest:
  an app-layer SELECT-only statement check (best-effort blocklist, not a
  SQL parser) → row cap → query timeout → **the database login itself
  should only have SELECT permission**, which is the real boundary. On
  top of that, `mssql.mask_columns` names result columns (case-insensitive,
  e.g. `email`, `phone`, `iban`) that get replaced with a fixed `•••`
  placeholder in *every* result — both the generic `query_mssql` tool and
  every `QueryTemplates` entry — before it ever reaches the model, so a
  table can stay queryable for counts/aggregates without the model seeing
  PII columns verbatim (`maskedColumnSet` in `mssql.go`). Tool calls are
  translated for OpenAI-compatible backends as well as Claude's Messages API
  and Gemini's Function Calling API; a local model that does not support tool
  calling simply answers without invoking the tool.
  Query templates are defined from Settings → MSSQL with a **structured
  editor** (name, model-facing description, SQL, per-parameter
  type/required/description/**example**, and a **result hint**) instead of
  hand-written JSON — a collapsible "Als JSON bearbeiten" escape hatch
  stays for power users. What the model is handed for each template
  (`queryTemplateToolSchema` in connector.go) folds the description, a
  per-parameter reference (`name (type, required/optional): desc [Beispiel:
  …]`) and the result hint into one tool description, and puts each example
  into the JSON-schema property — so the model can decide *whether* and
  *how* to call a template without ever seeing the SQL itself. The same
  structured editor + description enrichment applies to the HTTP templates
  below. Both template kinds share one placeholder syntax, `{name}`
  (`migrateLegacySQLTemplateParamSyntax` in `mssql.go` rewrites an
  existing saved SQL template's older `@name` syntax on load, so nothing
  needs re-entering by hand); `{name}` is rewritten to a real driver-bound
  `@name` parameter before execution, so values are still never
  string-concatenated into SQL.
- **HTTP query templates** ([`http_tool.go`](http_tool.go)) — the generic,
  REST analogue of MSSQL's query templates above (see
  [`docs/MCP_CONNECTORS_PLAN.md`](docs/MCP_CONNECTORS_PLAN.md) section
  (A)): instead of a new Go type/tool for every additional live lookup
  (a Freshservice ticket's current status, a Jira issue's status, a
  Confluence live search), an admin configures a named GET request with
  `{placeholder}`-style parameters directly from Settings ->
  "HTTP-Abfrage-Vorlagen" — a placeholder substituted into the query
  string is `url.QueryEscape`'d (not merely path-escaped), so a value like
  `"4711&extra=1"` can't inject an extra query parameter that wasn't in
  the admin's template. Each template borrows an already-configured
  connector's own credentials via `auth_source` (`confluence`, `jira`,
  `freshservice`, or `none` for an unauthenticated public endpoint) —
  SharePoint/Exchange aren't supported as an `auth_source` yet since their
  OAuth2 client-credentials flow needs its own token-acquisition wiring,
  left for when a concrete use case needs it. Validated at save time
  (unique names, GET-only, every declared parameter actually referenced,
  and the URL's host must match the referenced connector's own configured
  base URL — a SSRF guard, since the model only ever fills in placeholder
  values, never the domain) — an invalid template is rejected outright
  rather than silently exposing a broken/dangerous tool. An optional
  `response_json_path` (e.g. `"tickets"`) narrows a verbose API response
  down to just the relevant field before it reaches the model. Gated the
  same way as MSSQL/Shop above via the `http` tool category in Presets. An
  optional per-template **"TLS-Zertifikat ungeprüft akzeptieren"**
  (`insecure_skip_verify`, off by default) switches that one template's
  requests onto a separate HTTP client with certificate validation
  disabled — for an internal-only endpoint with a self-signed or
  internally-issued certificate (e.g. an on-prem SAP se16-style gateway)
  that a fresh Go process doesn't already trust, same scoped-opt-in
  posture as `mssqlConfig.TrustServerCertificate`; every other connector
  (Confluence/Jira/Freshservice — external SaaS with real, publicly-
  trusted certificates) keeps using the normal, verifying client
  unconditionally (see `insecureConnectorHTTPClient` in
  [`connector.go`](connector.go)).
- **SharePoint live search** (`search_sharepoint`, [`sharepoint.go`](sharepoint.go))
  — not an import source: queries a SharePoint site's content live via
  Microsoft Graph's Search API at answer time, instead of relying on
  content already imported into the vector store — useful for a file that
  changed recently or was never imported. Opt-in per SharePoint connection
  (`live_search_enabled`, off by default), scoped to that connection's
  site via a KQL `path:` filter so a live search can't reach outside the
  consented site. The `site` parameter only appears (and is required) once
  more than one connection has opted in; with exactly one, it's implied.
  Returns short hit summaries (filename, snippet, link), not full text —
  `search_knowledge_base` stays the right tool once a file is actually
  imported. Gated by the `sharepoint_search` tool category in Presets.

## External integrations: OpenAI-compatible API

Besides `/api/ask`/`/api/search` (R3's own JSON shape, see `docs/API.md`),
R3 can also expose an **OpenAI-compatible** `/v1/chat/completions` +
`/v1/models` server (`openai_api.go`) for tools that already speak that
wire format — Open WebUI, IDE assistants, other agents, anything with a
plain "OpenAI-compatible base URL" field. Off by default; turned on from
Settings → "Externe Schnittstellen" with:

- its **own TCP port**, separate from the main UI/API port, so a firewall
  rule can expose only this port to a partner tool — started/stopped/
  rebound live the moment settings are saved, no restart;
- an **always-required** API key (same key list as `/api/ask`'s
  `X-API-Key`, managed from Settings → "Externe Schnittstellen" →
  "API-Zugriff") — unlike `/api/ask`/`/api/search`, which are only
  key-gated when `api.require_api_key` is explicitly turned on, this
  server never has an unauthenticated mode: it's a distinct, deliberately-
  exposed integration surface, not the bundled browser UI's same-origin
  trust level, so it defaults secure rather than defaulting open (see
  `requireOpenAIAPIKey` in [`openai_api.go`](openai_api.go)). Enabling it
  with `openai_api.enabled: true` and a `port` but **no key created yet**
  starts the listener anyway — every request 401s until a key exists — so
  `reconcileOpenAIAPIServer` logs a `WARN` right when the server starts if
  `api.keys` is still empty, instead of that being a silent, hard-to-
  diagnose "it's on but nothing works";
- an admin-fixed **preset** (the same Presets mechanism Chat's Agent tier
  `default_preset` uses) restricting which `source_kind`s are retrievable
  and which tools (Shop, MSSQL) are offered to that external caller.

Every request still runs through R3's normal ranked retrieval first — the
caller gets a grounded answer with a trailing `Quellen:` footer, not a raw
passthrough to the underlying LLM. See `docs/API.md`'s "OpenAI-compatible
chat completions API" section for the full request/response shape.

### MCP: authenticated, read-only knowledge access

`POST /mcp` exposes a small [Model Context Protocol](https://modelcontextprotocol.io/)
Streamable-HTTP surface for external AI hosts. It supports `initialize`,
`ping`, `tools/list`, and `tools/call`, returning ordinary JSON responses
(no SSE session is needed). A valid R3 API key is always required, even if
`api.require_api_key` is off for the bundled browser UI; browser requests
with a foreign `Origin` are rejected. The intentionally narrow tool set is:

- `search_knowledge_base` — ranked, ACL-filtered snippets;
- `get_source_content` — ACL-filtered full extracted source text; and
- `get_r3_status` — non-sensitive chunk/storage status.

It deliberately does **not** expose imports, settings, mailbox drafting,
live SQL/HTTP tools, or any write action. Those need separate confirmation
and authorization design before becoming machine-callable.

## Chat, Agent & Mail: how they differ

**Agent is not a separate tab anymore.** It used to be; commit "Merge Agent
tier into Chat" folded it into Chat as the third option of a reasoning-tier
selector (`#askTier`: Instant · Standard · Agent) — `tab-agent.html` is gone,
`web/app.js`'s former `currentAgentConversation`/`mode:"agent"` state is now
just Chat's own `mode` field. Only two tabs remain: **Chat** (with its
Instant/Standard/Agent tier dropdown) and **Mail**. "Instant" skips retrieval
entirely for a bare LLM answer, "Standard" is the old single-retrieval-pass
Chat behavior, and "Agent" is the tool-loop behavior described below —
switching tiers mid-conversation is allowed and re-tags the conversation's
`mode`.

All three (Instant/Standard tiers, Agent tier, Mail) run through the same
`/api/ask` pipeline and the same ranked retrieval (`rankedSearch`/
`assembleContext`, see "Ranked retrieval" above), so the difference isn't
*what* they can ground an answer in — it's how much room each one has to
work before answering.

| | Chat | Agent | Mail |
|---|---|---|---|
| Tool loop | none — a single retrieval pass, then one answer (`maxRounds=1`) | full multi-round loop, up to `agent.max_tool_rounds` (default 6) | its own multi-round loop (`composeDraftReply`/`composeNewMail`), scoped to one draft |
| Tools available | none | `search_knowledge_base`, `get_source_content`, `list_sources`, `ask_clarifying_question`, plus Shop/MSSQL/HTTP-template/mail/`fetch_url`/`web_research`/`web_search`/`azure_bing_search` tools per setting | read-only KB tools + Shop/MSSQL/HTTP templates allowed by `draft_preset` — never `ask_clarifying_question`, `save_draft_to_mailbox`, `fetch_url`/`web_research`/`web_search`/`azure_bing_search`, or recursion |
| Can ask a clarifying question instead of guessing | no | yes | no (see "Human-in-the-loop by design") |
| Sub-agent delegation for broad, multi-part questions | no | yes (`delegate_subtasks`) | no |
| Multi-turn conversation memory | yes, optionally persisted (browser-local or per-account server-side, see "Chat history" below) | yes, same persistence | no — each draft request is its own one-shot task |
| Output | an answer with citations | an answer with citations, plus a live "Arbeitsschritte" step timeline | an editable draft text field, never a sent email |

**Chat** is the fastest path for a question the knowledge base can likely
answer directly from one retrieval pass — no back-and-forth, no live
lookups, just "search once, answer."

**Agent** is for tasks that need more than one step: refining a search
after seeing the first results, pulling a live number from SAP/a database/
the shop, comparing several independent things at once (via sub-agents),
or stopping to ask when a question has more than one reasonable reading
(e.g. "storniere die Bestellung" with several open orders on file) instead
of silently picking one. See "Agent mode & tools" below for the full tool
set and how the step timeline works.

**Mail** isn't general Q&A at all — it's a focused workflow that turns a
pasted incoming email (or a short brief) into a reviewable reply/new-mail
draft, agentic enough to look things up before writing but deliberately
without the loop-control or side-effecting tools that don't fit a one-shot
draft. **R3 never sends a mail on its own**: every draft needs a human to
review it and explicitly send/save it (copy, download as `.eml`, "send to
self", or file to the configured mailbox's Drafts folder via IMAP) — see
"Human-in-the-loop by design" below for the full reasoning.

### Rendered output formats (Chat & Agent)

The Chat/Agent answer area renders the assistant's Markdown richly, so the
model can reach for whatever format communicates best. The system prompt is
told which formats are available (see `outputFormattingGuidance` in
[`skills.go`](skills.go)); the client (`renderMarkdown` +
`enhanceRenderedContent` in [`web/app.js`](web/app.js)) renders them. All
rendering is **XSS-safe by construction** — the raw text is HTML-escaped
first, and every enhancement works on that escaped output — and every
library-backed format **degrades gracefully to a labeled, copyable code
block** if its library can't load.

| Fenced block / markup | Rendered as | Needs a library? |
|---|---|---|
| Markdown (headings, **bold**, lists, task lists, `>` quotes, `---`, `~~strike~~`) | formatted HTML | no |
| Markdown tables (with `:--`/`:-:`/`--:` alignment) | scrollable `<table>` | no |
| ` ```json ` / ` ```xml ` | pretty-printed (JSON) + syntax-highlighted | no (pure JS) |
| any other ` ```lang ` | code block with language label + copy button | no |
| ` ```mermaid ` | rendered SVG diagram (flowchart, sequence, gantt, …) | Mermaid |
| ` ```d3 ` | chart drawn by the snippet into `#viz` | d3 |

**Diagram/chart libraries — configurable, air-gap-friendly.** Mermaid and
d3 are too large to bundle into the binary, so they load lazily (only when
an answer actually contains such a block) from URLs in `settings.render`
([`renderConfig`](settings.go)), exposed to the browser via
`/api/auth/status`:

- `render.mermaid_url` — default `https://cdn.jsdelivr.net/npm/mermaid@11/+esm`
- `render.d3_url` — default `https://cdn.jsdelivr.net/npm/d3@7/dist/d3.min.js`

Point either at a **self-hosted copy** for an offline/air-gapped install,
or set it to `"-"` to **disable** that renderer (its source then just shows
as a code block). Versions are pinned to a major release so an upstream
change can't silently alter rendering.

**d3 runs sandboxed.** A ` ```d3 ` block is AI-authored JavaScript, so it
executes inside an `<iframe sandbox="allow-scripts">` **without**
`allow-same-origin`: the snippet can draw a chart but has an opaque origin —
no access to cookies, the session, the parent DOM, top-navigation or forms.
The only channel back out is a `postMessage` carrying its content height, so
the frame can be auto-sized. Mermaid is initialised with
`securityLevel:"strict"` (diagram text is treated as data, never markup).

### Mail draft controls & auto-draft rules

- **Length & format** selectors on the Mail tab steer only the *shape* of a
  draft — "kurz"/"normal"/"ausführlich" and "Fließtext"/"Stichpunkte". They
  resolve server-side against a closed set (`draftFormatInstruction`,
  [`draft.go`](draft.go)) so a client can't inject prompt text, and they
  explicitly leave the grounded facts untouched — only tone/structure change.
- **Auto-draft rules** (Exchange/Graph, [`autodraft.go`](autodraft.go)) have
  an editor in each Exchange connection card under Settings: the two opt-in
  gates (`enable_draft_replies`, `enable_auto_draft_rules`) plus a rule list
  (field = sender/subject, contains-pattern, negate, active). Both gates are
  **off by default**; with them on, a rule match on a newly-synced message
  files a grounded reply **draft** — never a send. See "Human-in-the-loop by
  design".

### Image uploads: hybrid vision + OCR

Chat and Agent both accept image attachments (paperclip icon, up to 4 images
per question, 8 MB each, `settings.upload.max_attachment_mb`) — never
persisted server-side, never saved into chat history. `settings.upload`
(`uploadConfig` in [`settings.go`](settings.go)) decides, admin-side, how an
uploaded image is turned into something the model can use:

- `image_mode: "ocr"` (default) — the image is run through the local
  `tesseract` binary (needs `allow_shell_exec` + `import.tesseract_bin`/
  `tesseract_lang`, same opt-in as markitdown) and only the extracted text
  reaches the model; no extra LLM call, no vision-capable backend required.
- `image_mode: "vision"` — the **whole question** (text + image) is routed
  to `upload.vision_profile` regardless of which chat profile is selected
  in the Chat/Agent dropdown, since a vision-capable backend (e.g. Azure
  `gpt-4o`) is often not the same one used for plain text chat. If
  `vision_profile` is unset, or the caller's session doesn't satisfy
  `ldap.guest_azure_profile_policy`'s login requirement, the image is
  silently dropped (with a note to the model) rather than failing the
  request or quietly falling back to OCR.
- `vision_max_dim` (default 1600px, configurable 800–1600) and
  `vision_jpeg_quality` (default 85, configurable 50–95) downscale/re-encode
  an oversized image before it's sent on the vision path — a phone photo
  upload is often 3000–4000px wide, which doesn't improve model legibility,
  only inflates request size and (for some providers) cost. The OCR path
  always gets the original resolution instead, since `tesseract` benefits
  from more pixels. Multiple images on the same question each get a
  filename caption so the model's answer can distinguish between them.
- The ocr-vs-vision-vs-drop decision itself is a pure, unit-tested function
  (`resolveUploadRouting` in [`chatimages.go`](chatimages.go)) separate from
  the HTTP handler, so that routing logic is verifiable without a real
  tesseract/vision backend.

All three Chat tiers and Mail drafts route uploads through the same
`upload` settings — see "Prompt/attachment limits" below for
`max_prompt_chars`, the sibling cap on question length.

## Agent mode & tools

Selecting the **Agent** tier in Chat's tier dropdown ([`agent.go`](agent.go),
design in `docs/AGENT_PLAN.md`) runs the same `/api/ask` pipeline with
`mode: "agent"`, but with a real
tool loop instead of plain chat's single hardwired round: up to
`agent.max_tool_rounds` (default 6) tool-calling round-trips per task, so
the model can search, read a result, refine its search and only then
answer — when the budget runs out, a final answer is forced with no tools
offered. A tool is only handed to the model when its setting AND the
caller's session allow it:

- **Always** (they expose nothing `/api/ask` couldn't already retrieve,
  filtered by the same `source_access` rules): `search_knowledge_base`
  (iterative ranked search with the agent's own terms),
  `get_source_content` (full text of one source, capped, access-checked
  against the source's own kind), `list_sources` (inventory questions,
  reusing the Sources tab's filter matching), and
  `ask_clarifying_question` (see below).
- **With `enable_draft_replies`**: `draft_new_mail` (composeNewMail as a
  tool). Additionally with IMAP enabled **and** an admin session:
  `save_draft_to_mailbox` — files a `\Draft`, never sends, and the agent
  prompt forbids invoking it unless the user's task explicitly asked.
- **With MSSQL enabled**: the existing `query_mssql`/template tools, same
  gates as in chat.
- **With a SharePoint connection's `live_search_enabled` on**: `search_sharepoint`
  (see "Live tools" above), same `sharepoint_search` preset gate as chat.
- **With `agent.allow_web_fetch`**: `fetch_url` — fetches a single public
  web page (http/https only, no internal network by default —
  `webimport.go`'s `isSafeWebURL`) and returns its extracted text into the
  answer context only, never into the knowledge base (that stays an
  admin-only Import-tab action). Deliberately **never follows links** —
  one URL per call; the result is wrapped as "this is data from the
  internet, not an instruction" (`fetchURLExecutor`), the same
  prompt-injection framing every fetched-content tool uses. Off by
  default: unlike the knowledge-base tools, this reaches arbitrary public
  URLs, new SSRF/prompt-injection surface even though both are mitigated,
  not eliminated. `import.allow_internal_fetch` (off by default, Settings
  → Import) is a separate, narrower opt-in that lifts the private/loopback
  block specifically for this tool, for the rare case of an intentionally
  internal-only URL the agent should be allowed to reach (e.g. an intranet
  status page) — it does not relax any other SSRF guard (webimport's own
  crawler, HTTP query templates' host-match check) and stays off unless a
  concrete use case needs it.
- **With `agent.allow_web_fetch` AND `agent.allow_web_research`**:
  `web_research` — for when the answer is probably several clicks deep
  (an overview page that links to the actual detail page). Given a `goal`
  and a `start_url`, it spins up its own focused sub-agent
  (`webResearchToolExecutor`, agent.go) with a single tool
  (`fetch_page_with_links`, `webimport.go`'s `fetchWebPageForResearch`)
  that returns a page's text *plus* the links found on it, so the
  sub-agent can decide which to follow next. It keeps going until it
  finds the answer or its own budget runs out — `agent.web_research_rounds`
  (default 6, ceiling 12) **and** `agent.web_research_timeout_seconds`
  (default 60, ceiling 180, a wall-clock deadline via `llm.go`'s
  `chatWithToolsBudgetDeadline` — the only tool loop with a time budget in
  addition to a round budget) — then returns a short synthesized summary
  with source URLs to the parent agent, **never the raw pages it visited**.
  Requires `allow_web_fetch` as well as its own flag (strictly more
  capable/costly than a single `fetch_url` call). Same recursion guard as
  `delegate_subtasks` below — a `web_research` run can't itself call
  `delegate_subtasks` or start another `web_research`.
- **With `agent.allow_web_search`**: `web_search` (`websearch.go`) — the
  query→URL discovery step neither `fetch_url` nor `web_research` has
  (both require an already-known start URL). Given a plain-text query,
  calls [Tavily](https://tavily.com)'s `POST /search` API (Bearer-token
  auth) and returns a short numbered list of results (title, URL,
  truncated content excerpt) as plain text, up to `agent.web_search_max_results`
  (default 5, ceiling 20) within `agent.web_search_timeout_seconds`
  (default 15, ceiling 60). The agent typically calls this first, then
  hands a promising result's URL to `fetch_url`/`web_research` for the
  actual full-text read. Independently gated from `allow_web_fetch` —
  unlike that flag, R3 itself never fetches an arbitrary caller-influenced
  URL here (Tavily does its own crawling on its own infrastructure), so
  this carries a different risk profile (one specific, admin-configured
  third-party API call, like Shop/MSSQL) rather than an open-web-fetch
  SSRF surface. Requires `agent.web_search_api_key`/`web_search_api_key_env`
  (same inline-value-vs-env-var-name pattern as every other credential in
  this app) — with neither set, the tool call fails with a clear
  "kein API-Key konfiguriert" error rather than silently doing nothing.
  Tavily was chosen over Google/Azure-Bing search-grounding options
  specifically because those return only a synthesized answer from their
  own vendor's model, never raw results a *different* backend could use —
  incompatible with this app's six interchangeable chat backends
  (Local/Azure/OpenAI/OpenRouter/Claude/Gemini).
- **With `agent.allow_azure_bing_search`**: `azure_bing_search`
  (`azurebingsearch.go`) — the black-box counterpart to `web_search`,
  answering a question via Azure OpenAI's Responses API `web_search` tool
  (built on Grounding with Bing Search;
  `POST {resource}/openai/v1/responses`, a *different* API surface than
  the Chat Completions endpoint this app's normal chat/embedding calls
  use). Unlike Tavily, Azure never hands back raw search results — search
  and answer synthesis happen together in one Azure-hosted call — so this
  tool returns the model's already-grounded answer text plus the
  `url_citation` annotations Azure does expose, formatted as a "Quellen:"
  list. Reuses the `azure` LLM profile's existing `base_url`/`api_key`/
  `chat_model` (no separate credential set) via `ragSystem.getChatLM
  ("azure")`, and is offered to the agent only when that profile actually
  has a `base_url` and `chat_model` configured — otherwise every call
  would just fail with a "not configured" error, so it isn't offered at
  all. Bounded by `agent.azure_bing_search_timeout_seconds` (default 30,
  ceiling 120). Works regardless of which profile answers the *overall*
  chat/agent request — Azure is used purely as a grounded-search
  microservice here, the same role Tavily plays for `web_search`.
- **`run_code`** exists only as a seam so far: the `codeSandbox`
  interface (agent.go) documents the acceptance criteria any
  implementation must meet (no fs/net/env, an in-interpreter operation
  budget — a wall clock can't stop a busy loop in Go —, memory budget,
  output cap; plan: [nanoGo](https://github.com/SimonWaldherr/nanoGo)
  hosted inside a [wazero](https://github.com/tetratelabs/wazero) WASM
  wall). The tool is offered only when `agent.allow_code_execution`
  (default off) AND a compiled-in sandbox are both present — operator
  opt-in and build opt-in, defense in depth. No sandbox ships today.

**Clarifying questions instead of guessing**: `ask_clarifying_question`
(always offered, no setting) lets the agent stop and ask the user when a
task has more than one reasonable reading, instead of silently picking an
interpretation — e.g. "Storniere die Bestellung" with several open orders
on file. Calling this tool doesn't behave like a normal tool result fed
back to the model: `llm.go`'s `chatWithToolsBudget` recognizes the tool
call by name and ends the loop immediately with a distinguished
`*ErrClarificationNeeded` (question + up to a handful of short answer
options), which `handleAsk` turns into a `"clarify"` NDJSON message (or
the `format:"json"` response's `clarify` field) instead of a normal
answer. The UI renders the question as the assistant's message and
any options as clickable buttons (`web/app.js`'s
`renderAgentClarification`) — clicking one submits its exact text as the
next question, continuing the same conversation; without options, the
user just types a free-text reply as usual. Scoped to the Agent tier only
(the tool is only added by `agent.go`'s `buildAgentTools`), since the
Instant/Standard tiers' single-round semantics have no loop to
short-circuit.

Within a single tool-calling round, independent tool calls the model
requests together run **concurrently** rather than one-by-one
(`runToolCalls` in `llm.go`), and an identical repeat call (same tool,
same arguments) within the same request is served from a small
**cross-round result cache** instead of re-executing it — a model that
re-issues the same `search_knowledge_base` query after refining its
answer doesn't pay for a second real search. Both are internal
performance improvements with no setting to configure; see `llm_test.go`
for the tests proving both properties.

**Sub-agent orchestration.** The top-level agent can call
`delegate_subtasks` (agent.go) to fan a broad question out to several
focused sub-agents that run **in parallel**, each with its own bounded tool
loop (knowledge-base read tools + the allowed live tools), then synthesize
their findings — for a question with independent parts ("compare A, B and
C"; "status, history and open points on X") this beats one long linear
loop. Sub-agents deliberately do *not* get `delegate_subtasks` themselves
(`buildSubAgentTools`), plus a context depth-guard — so recursion is
impossible. The fan-out is admin-managed (Settings → Agent): how many
sub-agents one delegate call may spawn (`agent.max_subtasks`, default 4),
each sub-agent's own tool-round budget (`agent.subagent_rounds`, default
4), and — the key throttle — how many run **concurrently**
(`agent.max_concurrency`, default 4, a weighted semaphore over the parallel
fan-out so a broad question can't flood the chat backend, same "pace
ourselves" reasoning as the import throttle). All three fall back to a
default when 0 and are clamped to a hard ceiling (8) so no config turns the
fan-out into a runaway. On by default in Agent mode; opt out entirely with
`agent.subagents_disabled`.

A long, multi-round run — especially a broad question that fanned out into
several sub-agents — can accumulate a lot of tool-result text across
rounds even though each individual result is already capped. **Context
compaction** (`compactOldToolRounds` in [`llm.go`](llm.go), Settings →
Agent) bounds that: once the accumulated size crosses a threshold (default
24 000 characters), every tool-result message belonging to a round older
than the most recent two is replaced with a short placeholder — no second
LLM call, no summarization, purely a deterministic size cap. The model's
own tool calls are never touched, only the (already-processed) raw result
bodies from rounds it has, in practice, already moved past. On by default
with sane built-in thresholds for every caller (drafts, sub-agents,
web-research included); tunable or disable-able from Settings for the main
Chat/Agent entry point specifically.

Chat's Agent tier shows this working live: a **"Arbeitsschritte" timeline**
renders above each answer as it streams, with one row per tool call
(pending → done/error, with duration) and each sub-agent (`delegate_subtasks`,
`web_research`) as an indented, labelled group, its own nested tool calls
grouped underneath it. It's driven by NDJSON `"step"` events the server
emits during the tool loop (`agentStep`/`withAgentProgress` in llm.go → the
`step` branch of handleAsk's stream), independent of the admin-only debug
panel. Each row shows not just *that* a tool ran but *what with*: the
call's arguments appear inline next to the tool name while it's pending,
and once it resolves, an expandable "Ergebnis" detail (collapsed by
default, so the timeline stays scannable) reveals the truncated result
text the model actually got back — the same `agentStep.Args`/`.Result`
fields the admin-only audit log already exposed as a hover tooltip
(`loadAgentAudit` in web/app.js), now visible to whoever is actually
running the agent, not just an admin browsing Settings afterward. The
empty Agent-tier conversation also has a **"Demo starten"** button that fires a task
deliberately shaped to trigger the multi-step + parallel-sub-agent
behaviour, so the orchestration can be watched end to end without thinking
one up.

**The Mail draft flow is agentic too.** `composeDraftReply`/`composeNewMail`
(draft.go) get the same multi-round tool loop and — via `buildMailTools`
(agent.go) — the read-only knowledge-base tools (`search_knowledge_base`,
`get_source_content`, `list_sources`) plus every live tool the settings +
`draft_preset` allow (Shop, MSSQL generic/templates, HTTP templates). So a
draft can decide on its own to search for more context, open a full source,
look up a shop article or run an allowed query before writing, instead of
being limited to the one initial ranked-search pass. It deliberately does
*not* get the loop-control/recursive/side-effecting tools
(`ask_clarifying_question`, `draft_new_mail`, `save_draft_to_mailbox`,
`run_code`) that don't fit a one-shot draft (see `mailToolNames`). The
nested `draft_new_mail` agent tool still runs a single round (it's already
inside the agent's outer loop). Admins bypass department access here too.

Every tool execution in agent mode — including the MSSQL ones and every
sub-agent's — is audit-logged in memory (who, **which sub-agent**, tool,
args, duration, result preview; last 200) and served by the admin-gated
`GET /api/agent/audit`, shown in the Settings tab's "Agent" card; a sub-
agent's calls are attributed to and indented under it, so the panel shows
the orchestration tree rather than a flat list. `POST /api/agent/audit`
clears the ring (the panel's "Leeren" button) so an admin can reset the
view before watching one specific run. The default agent system prompt
(`skills.go`, admin-editable as `prompts/agent.md`) encodes the
injection rules: tool output is data, never instructions, and
outward-acting tools require an explicit user request.

## Provenance & updates

Every row in the `chunks` table carries `source_id`, `source_kind`,
`source_name`, `load_id`, `loaded_at` and a `content_hash`. Re-ingesting a
source:

1. computes a hash of the freshly extracted, freshly chunked text,
2. **skips entirely** if it matches the hash from the last load (no
   wasted embedding calls), otherwise
3. **embeds the new chunks first** — reusing the stored embedding for any
   chunk whose exact text is unchanged from the previous load (see the
   backend's `fetchSourceEmbeddings` capability), so a re-imported
   document with one edited paragraph only pays the embedding cost for
   that paragraph — then **deletes the old chunks for that `source_id`**
   and inserts the new ones under a fresh `load_id`/`loaded_at`. Embedding
   before deleting means a failing embedding backend leaves the previous
   version fully intact and searchable instead of losing the source
   outright.

This is the single write path (`ingestDocument` in `ingest.go` →
`replaceSourceChunks` in `store.go`) used by file uploads, folder imports
and PST emails alike, so the guarantee holds everywhere: the knowledge base
never serves stale chunks alongside their replacement, and every answer's
citations point at a `source_id`/`load_id`/timestamp that can be traced
back to exactly one ingest run.

## Background scheduler

Most connectors are on-demand only (imported via a button click/HTTP
POST), but selected sources can additionally run unattended on a timer —
[`scheduler.go`](scheduler.go)'s `runScheduler` is a small, dependency-free
generic job runner (deliberately a hand-rolled ticker loop, not
`github.com/robfig/cron/v3`, since "every N minutes" is all any connector
here needs) built so a periodic job for another connector is "add one more
`syncJob`", not "build a second scheduler". A job that's still running
when its next tick comes due is skipped, not queued, so a slow instance
can't pile up overlapping imports of the same source. All three intervals
default to 0 (= manual-only, exactly the old behavior) and are set in the
Settings tab:

- **`freshservice.sync_interval_minutes`** — pulls and ingests every
  ticket on the interval.
- **`imap.poll_interval_seconds`** — fetches everything above the
  mailbox's persisted `last_uid` (the same incremental fetch "Import
  jetzt" does) and advances that high-water mark. This finally activates
  a field that had existed unread in `mailboxConfig` since the connector
  was written; its documented meaning was always exactly this.
- **`sharepoint.sync_interval_minutes`** — runs the Graph delta-sync
  (ingest changed files, delete removed ones) and advances the persisted
  `delta_link` cursor, same as the Import tab's "Delta-Sync" button.
- **`onedrive.sync_interval_minutes`** — runs the drive's Graph delta sync
  with the same rename/move/delete reconciliation as SharePoint.
- **`github.sync_interval_minutes`** — resumes the repository page cursor
  or fetches work items updated since the completed cycle.
- **`sap_s4.sync_interval_minutes`** — reads the selected OData window and
  advances an OData next/delta link when the SAP service exposes one.

The last 50 runs across all jobs are retained across restarts in
`r3-scheduler-history.jsonl` (`GET /api/scheduler/history`,
`requireAdminSession`-gated). Failed jobs create a durable, acknowledgeable
operational alert; the next successful run resolves it automatically. For
monitoring systems, `GET /metrics` exposes Prometheus-style per-job run,
failure, duration, running and active-alert gauges when the
`R3_METRICS_TOKEN` environment variable is set (call it with that Bearer
token; without one the endpoint returns 404).

**Jobs tab.** The scheduler has its own top-level, admin-only **"Jobs"**
tab (`web/templates/tab-jobs.html`, between Prompts and Settings in the
sidebar) rather than living only inside a Settings card: one row per
enabled connection (including connections with no interval configured,
shown as "manual only"), each row showing its configured interval, live
status (running since when / paused / waiting, with the next run's
timestamp), and its last run (✓/✗, timestamp, duration, trigger `auto` or
`manual`, details on hover). Self-refreshes every 10 seconds while the tab
is active. Three per-job actions, all `requireAdminSession`-gated
(`/api/scheduler/run`, `/api/scheduler/cancel`, `/api/scheduler/pause`):
**run now** (ad-hoc, ignores the interval and any pause, skips if that job
is already running), **cancel** (stops a running job at its next
per-item boundary via context cancellation, never mid-request; already-
ingested items are kept and the run is recorded as "cancelled"), and
**pause/resume** (suspends only the automatic cadence without forgetting
the configured interval — unlike setting it to 0 — persisted per
connection and deliberately **not** overwritten by a plain "save settings"
from a stale-open tab; manual runs and "run now" still work while paused).
History below the per-job rows lists every run (auto or manual) under
`<connector>-sync:<connection-name>`, so which specific connection ran is
identifiable even with several connections of the same kind.

SharePoint and IMAP jobs specifically — the two with a server-managed
resume cursor (`delta_link` / `last_uid`) — get a fourth action, **reset
cursor** (`POST /api/scheduler/reset-cursor`, also `requireAdminSession`-
gated): clears that connection's stored cursor so the next run (auto or
manual) does a full walk from scratch instead of resuming. Exists because
a cursor can end up stuck past content the connector never actually
finished ingesting — e.g. SharePoint's delta feed advancing past items
whose *individual* download failed (each item's own error is recorded, but
the run as a whole still completes and its final `delta_link` still gets
persisted, since re-reading the same window on the next tick would
duplicate everything that *did* succeed rather than recover what didn't).
Since then, SharePoint's own delta-sync additionally self-heals
automatically on Graph's documented `410 Gone` (an invalidated cursor) —
see "Data sources" above — so this button is now mainly for the rarer case
where items were skipped without Graph ever invalidating the cursor
outright.

The Jobs tab also carries a **"Feedback-Auswertung"** card, independent of
the scheduler — see "Security / privacy considerations" below for what it
shows.

## LLM backends: local embeddings + selectable chat providers

R3 keeps the vector space local and lets chat use the provider that fits the
question or deployment. The named profiles in [`settings.go`](settings.go)
are:

- `local` — an OpenAI-compatible server such as LM Studio, Ollama or vLLM;
- `azure` — Azure OpenAI deployments;
- `openai` — the original OpenAI Chat Completions API;
- `openrouter` — OpenRouter's OpenAI-compatible gateway;
- `claude` — Anthropic's Messages API; and
- `gemini` — Google's Gemini GenerateContent API.

The shared `lmClient` handles OpenAI-compatible and Azure URL/auth shapes,
while [`llm_native.go`](llm_native.go) translates Claude and Gemini's native
message and tool-call formats into RubixRAG's internal representation.

- **Embeddings are local-only.** `embed_profile` is normalized to `local` and
  all chunk/query embeddings use `profiles.local.embed_model`. Changing the
  chat provider therefore never invalidates the stored vectors or requires a
  re-import.
- **Chat** defaults to `chat_profile` (default `local`), and each `/api/ask`
  request can override it, for example `{"question":"...","profile":"claude"}`.
  The Settings, Chat and Agent UIs expose the same profile names.
- **Tool execution stays in RubixRAG.** Claude tool-use blocks and Gemini
  function calls are translated back to the common `toolCall` type; SQL,
  HTTP, shop and other tools are then executed by RubixRAG's existing
  authorization/audit-aware executors. The provider never receives direct
  database access.
- Profiles are rebuilt live after Settings are saved (`ragSystem.setLLM`),
  without a restart. API keys can be entered for local development or read
  from provider-specific environment variables (`OPENAI_API_KEY`,
  `OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY` and
  `AZURE_OPENAI_API_KEY`).
- Non-streaming provider calls retry network errors and 429/5xx responses
  with the existing backoff. Native Claude/Gemini calls currently return one
  completed provider response to the surrounding RubixRAG stream; OpenAI-
  compatible backends retain token streaming.

Configure cloud chat profiles in Settings or `settings.json`. The CLI still
initializes the local embedding/chat profile:

```bash
./R3 -addr :8090 -storage-path r3-data \
     -url http://localhost:1234 -chat mistralai/ministral-3-3b -embed text-embedding-nomic-embed-text-v1.5 \
     -azure-url https://MY-RESOURCE.openai.azure.com \
     -azure-chat-deployment gpt-4o \
     -azure-api-version 2024-10-21
```

See [`CREDENTIALS.md`](CREDENTIALS.md) for the full picture: every
credential field's `*_env` counterpart, how to actually set an
environment variable on Windows vs. the production Linux host, and
credential-handling best practices for this repo (including a known
priority inconsistency in the Azure profile specifically, see below).

Azure credentials should come from an environment variable, not
`settings.json` — set `api_key_env` (default `AZURE_OPENAI_API_KEY`) to the
variable name and export it before starting R3:

```bash
export AZURE_OPENAI_API_KEY=...
```

For the other chat profiles use the corresponding environment variable, for
example `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY` or
`GEMINI_API_KEY`; the variable names are configurable through each profile's
`api_key_env` setting.

An inline `api_key` field exists for local dev convenience but is masked
(`***set***`) on every `GET /api/settings` response and should not be used
in a shared deployment.

### Embedding model: under consideration for a change

The default local embedding model, `text-embedding-nomic-embed-text-v1.5`
(`profiles.local.embed_model`), is a de-facto industry standard for
self-hosted RAG right now — it's the default embedding model bundled with
LM Studio, widely used in local/offline RAG setups, Apache-2.0 licensed,
and supports an 8192-token context (long documents/emails embed without
truncation) plus Matryoshka-style dimension truncation (768 → 256/128/64)
for cheaper storage/search if ever needed. Switching away from it is not
a given — it's a reasonable default — but a change is currently being
evaluated, mainly because it's English-optimized and R3's actual corpus
(mailboxes, tickets, SharePoint/Confluence content) is substantially
German. A multilingual alternative (e.g. `multilingual-e5-large`) may
retrieve German-language chunks more accurately at the cost of a larger
model and no built-in dimension truncation.

**What matters before changing it, given how R3 stores embeddings today:**

- **Every existing chunk becomes unsearchable against a new-model query
  embedding.** Vector similarity only makes sense between embeddings from
  the *same* model — R3 has no mixed-model search. Changing
  `profiles.local.embed_model` therefore requires re-embedding the entire
  corpus (delete `r3-data`'s chunk table or re-run every import), not a
  live/rolling migration. Budget for that: full PST/SharePoint/Confluence/
  Jira/Freshservice re-import time, not just a config edit.
- **Embedding dimension changes what's stored per chunk.** A larger
  dimension (e.g. `multilingual-e5-large`'s 1024 vs. nomic's 768, no
  truncation option) means more storage and slightly slower vector search
  per chunk at scale — check against `storage.max_memory_mb` in
  `settings.json`.
- **Model file size / local resource footprint** — a larger embedding
  model needs to fit alongside the local chat model in whatever's serving
  `-url` (LM Studio/Ollama/vLLM); confirm the target host (see
  "Deployment" above) has the RAM/VRAM headroom for both loaded at once,
  not just the embedding model in isolation.
- **Task-prefix / calling convention differences.** Some embedding models
  (nomic included) expect a `search_query:`/`search_document:` prefix
  before the text for best accuracy; a replacement model may need a
  different (or no) prefix — check wherever R3 builds the embedding
  request (`llm.go`) and adjust if the new model's documented usage
  differs.
- **Cloud chat profiles stay unaffected.** Per "LLM backends" above,
  embeddings are always local (`embed_profile` is normalized to `local`),
  so this decision is independent of which chat provider (`azure`,
  `openai`, `claude`, …) is configured — only `profiles.local.embed_model`
  and the resulting re-embed are in scope.
- **Benchmark before switching in production.** Compare retrieval quality
  (ranked results, not just raw cosine similarity) on a representative
  sample of real German-language questions against the current corpus
  before committing to a full re-embed — nomic's English-language
  performance is strong enough that a naive multilingual swap isn't
  automatically a win if most retrieval is still keyword/BM25-assisted
  (see "Ranked retrieval" above).

No decision has been made yet; this section documents the tradeoffs so
the choice (if made) is deliberate rather than a config edit that quietly
invalidates the whole knowledge base.

## Running

```bash
go mod tidy   # resolves the exact indirect dependency set — see note below
go build .
./R3 -addr :8090 -storage-path r3-data -url http://localhost:1234 \
     -chat mistralai/ministral-3-3b -embed text-embedding-nomic-embed-text-v1.5
```

Then open `http://localhost:8090`.

Configuration is persisted to `settings.json` after first run, with
`profiles.local`, `profiles.azure`, `profiles.openai`,
`profiles.openrouter`, `profiles.claude` and `profiles.gemini` blocks for
LLM backends (see above), a
`ranking` block controlling the hybrid retrieval weights, and an `import`
block for the markitdown binary path and max file size — all editable from
the Settings tab in the UI. A `storage` block (`backend`/`mode`/`path`/
`max_memory_mb`, see `docs/VECTOR_DB.md`) configures the vector store —
this one is startup-only (CLI flags on first run, or hand-edit
`settings.json` and restart), not exposed in the UI, since changing it
means opening a differently-shaped store rather than tweaking a live
config value.

The Settings tab itself is organized into five groups rather than one
long list of accordions: "LLM & Antworten", "Datenquellen (Connectors)",
"Werkzeuge (Live-Tools)", "Zugriff & Sicherheit" and "Externe
Schnittstellen" (the last one covering both API keys and the
OpenAI-compatible server above) — purely a UI grouping, no change to the
underlying settings shape or `settings.json` keys.

The theme switcher lives in a popover menu off the sidebar footer (not a
Settings field) with a live preview swatch per theme, so a theme can be
tried and compared without leaving the current page.

### Prompt/attachment limits

`settings.upload.max_attachment_mb` (default 8, admin-configurable 1–50)
and `settings.upload.max_prompt_chars` (default 20000, admin-configurable
2000–100000) cap, respectively, how large a single Chat/Agent attachment
may be before base64 inflation and how long a single question may be —
guarding against an accidental (or deliberate) huge paste/upload inflating
retrieval or LLM cost, not against a genuinely long but reasonable
question. Both fall back to their built-in default for a `settings.json`
predating these fields (`effectiveMaxAttachmentMB`/
`effectiveMaxPromptChars` in `settings.go`), so no existing deployment's
behavior changes until an admin deliberately adjusts them.

### UI language (`lang`)

The browser UI is available in **German, English, French and Italian**
(`supportedUILangs` in [`settings.go`](settings.go); dictionaries in
[`web/i18n.js`](web/i18n.js)). `lang` is a server-wide setting (Settings →
General), not a per-browser preference, and takes effect immediately after
saving, no restart. Translation coverage now extends across the admin tabs
too — Import's connector cards, connection-test results, the Chunks filter
bar and the Jobs scheduler dashboard are all `data-i18n`-tagged, not just
the end-user-facing surfaces (sidebar, Chat/Mail, Help, history, "Mein
Konto"/My Account, theme/font-size switcher, login). Adding a language
means populating `MESSAGES.<lang>` in `web/i18n.js` with the same keys
already used elsewhere; any string still found hard-coded is a gap to close
there, not a deliberately untranslated area.

Loading an existing `settings.json` never requires a matching manual
migration step, even when a release adds a brand-new field or an entire
new connector struct: `loadOrCreateSettings` ([`settings.go`](settings.go))
starts from the current defaults and unmarshals the file's JSON on top of
that, so any key the file doesn't mention — at any nesting depth — simply
keeps its default instead of becoming a zero value, while every key the
file *does* define is left exactly as-is. The now-complete result is
immediately written back to `settings.json`, so the file itself grows to
include newly-introduced fields (with their defaults) the moment it's
next loaded, not just in memory.

`/api/ask` always answers as newline-delimited JSON so the browser can
render the reply token-by-token as it's generated (`handlers.go`'s
`flushingTokenWriter`; the "Streaming" checkbox under Settings → Chat, or
`disable_streaming` in `settings.json`, turns the live flush off in favor
of one buffered line per answer — the wire format is identical either
way, so nothing else needs to change when it's toggled).

### Running as a service (`/etc/init.d/r3`)

`build-linux-deploy.bat` cross-compiles `R3` for linux/amd64 on Windows and
packages it together with `deploy/r3.init` (an LSB init script) and
`deploy/r3.default` (its config, read from `/etc/default/r3`) into
`upload/`. On the target server, one-time setup:

```bash
cp deploy/r3.init /etc/init.d/r3 && chmod +x /etc/init.d/r3
cp deploy/r3.default /etc/default/r3   # then edit paths/flags in it
chown root:root /etc/default/r3 && chmod 600 /etc/default/r3   # may hold secrets
update-rc.d r3 defaults
service r3 start
```

`/etc/default/r3` is where every start flag lives (`R3_ADDR`,
`R3_STORAGE_BACKEND`/`R3_STORAGE_PATH`, `R3_USER`, …) — see the comments in
`deploy/r3.default`. `R3_VERBOSE=1` there turns on `-verbose` (every
request, every embed/chat call's provider/model/base URL, every
import/connector step — see "`make` targets" below) without touching the
binary or the start command; `service r3 restart` picks it up. The init
script wraps R3 so its stdout/stderr — R3 only logs via the standard `log`
package, no file logging of its own — end up in `R3_LOGFILE` (default
`/mnt/application/R3/r3.log`).

Two extra actions beyond the standard `start`/`stop`/`restart`/`status`:

- `service r3 logs` — `tail -F` on the current `R3_LOGFILE`, whether or not
  `R3_VERBOSE` is on and whether or not R3 is currently running (useful to
  see what happened right before a crash).
- `service r3 debug` — stops R3 if running, restarts it with `-verbose`
  forced on for just this run (doesn't touch `R3_VERBOSE` in
  `/etc/default/r3`, so a later plain `restart` reverts to normal), then
  attaches to the log. Ctrl-C only stops watching, not the service itself —
  `service r3 restart` afterwards goes back to quiet.

Re-running `build-linux-deploy.bat` after every source change and copying
just the new `R3` binary onto the server (`service r3 stop`, replace the
binary, `service r3 start`) does **not** need `deploy/r3.init`/
`r3.default` re-copied — only pull those again if you change the init
script/config template itself.

### `make` targets

```bash
make build   # go build ./...
make run     # go run . — quiet, no per-request/model logging
make dev     # gofmt + go vet, then go run . -verbose — logs every
             # request (method/path/duration) and every embed/chat call
             # (provider/model/base) to stdout; use this while developing
make check   # fmt + vet + test
make release # go build -trimpath -ldflags="-s -w" -o R3 . — same binary as
             # `build`, minus DWARF debug info and symbol table (biggest
             # size lever short of dropping a dependency); no source-path
             # embedding either. Smaller artifact for shipping to the
             # deployment host, at the cost of a less debuggable binary if
             # it ever needs to be inspected with a debugger/profiler there.
```

`-verbose` (and therefore `make dev`) is meant for interactive development
— once R3 runs unattended against Exchange in the background (Phase 2/3,
see the roadmap above), nobody's watching stdout, so `make run`/a plain
`./R3` without the flag is the right mode for that.

`-verbose` also covers every importer/connector (PST, SharePoint, Exchange,
IMAP, Teams, Confluence, Jira, Freshservice, web, folder): each logs a line
per fetched item (counts, dry-run flag) and `ingestDocument` logs a line per
document (chunk/redaction counts, skip decision, dry-run flag) — useful for
diagnosing "why didn't this get re-embedded"/"why is this chunk count 0"
without adding a separate debug flag. Same single global switch, no extra
setting needed.

### Toolchain note

This code was originally written where only Go 1.16 was installed
(too old to parse the `go 1.25` directive in `go.mod`) — it has since been
**built and run successfully on the real Go ≥1.23 target server**,
confirming the dependency set and API usage (tinySQL, go-pst) are correct.
If building somewhere new: `go mod tidy` first — `go.sum` was generated on
the first real build, not written by hand.

`go-pst` depends on `github.com/godzie44/go-uring`, a Linux `io_uring`
wrapper — this is fine on Windows too, since go-pst gates that file with
`//go:build linux` and the async/io_uring code path (`pst.NewAsync`) is
simply never called by `pst.go`; the standard `pst.New(io.Reader)` path used
here is pure Go and cross-platform.

### Troubleshooting

- **`go build` fails parsing `go.mod`** — the installed `go` is older than
  the `go 1.25` directive; install a current Go release (≥1.23 is
  confirmed working, see above).
- **Chat/embedding requests hang or fail with connection errors** — embeddings
  always require the configured local backend (`-url`, default
  `http://localhost:1234`) and its `-embed` model. Chat can use that backend
  or any configured `azure`, `openai`, `openrouter`, `claude` or `gemini`
  profile; check the provider-specific model/key/base URL in Settings. Run
  with `-verbose` (or `make dev`) to log every embed/chat call's
  provider/model/base and see exactly which backend is being hit.
- **Office/PDF/audio files fail to import** — `allow_shell_exec` is off by
  default, or the required converter is missing from `PATH`; install
  `markitdown[all]` for Office/PDF/audio support, `ffmpeg` for video, and
  `7z` for `.7z` archives, then enable `allow_shell_exec` in Settings.
- **Settings change doesn't seem to apply** — most settings (LLM profiles,
  ranking weights, import options) take effect on the next request with no
  restart; the `storage` block is the exception and requires a restart
  (see "Running" above).

## Prompts & skills

The chat system prompt is not hardcoded — it lives in `prompts/index.md`
(applied to every question) plus any number of `prompts/skill_*.md` files,
each with tags in `prompts/manifest.json`. A skill is only injected into
the prompt when one of its tags appears as a token in the question
(`skills.go:selectSkills` — cheap keyword matching, no extra embedding or
LLM call). Both are managed from the "Prompts" admin tab: edit, enable/
disable, create, delete — writes go straight to disk and are picked up on
the very next question, no restart. The checked-in `prompts/` starts with
two disabled example skills (`skill_mechatronic.md`, `skill_ppe.md`) as a
template for the actual domain content Fachbereiche add via that tab.

## Admin access

Import/Sources/Chunks/Prompts/Users/Settings are hidden behind an
"Admin-Login" button in the UI. Whether that's *just* a UI convenience or
actual access control depends on whether **any** login system is enabled —
`settings.ldap.enabled` or `settings.local_auth.enabled` (the shared check
is `authTierActive` in `handlers.go`):

- **Neither enabled (default)** — the old MVP behavior: `/api/admin/check`
  compares a submitted password against `AdminPasswordEnv` (default env
  var name `R3_ADMIN_PASSWORD`; unset = check always passes), but none of
  the admin-only *API endpoints themselves* are protected — anyone who can
  reach the server can still call `/api/upload`, `/api/settings`, etc.
  directly.
- **LDAP enabled** ([`ldapauth.go`](ldapauth.go), [`session.go`](session.go)) —
  real per-user AD bind authentication plus an HMAC-signed session cookie;
  `requireAdminSession` (`handlers.go`) then actually enforces a valid
  session on every admin route, optionally restricted to members of one AD
  group (`ldap.required_group_dn`). This is the real access-control path;
  flip it on once `ldap.url`/`base_dn`/`required_group_dn` are configured
  for the target AD. **Sessions now persist across a restart**: both the
  HMAC signing secret and the session store are written to disk
  (`initSessionPersistence`/`loadOrCreateSessionSecret` in `session.go`)
  instead of being regenerated in memory on every process start, so a
  planned restart (deploy, config reload) no longer force-logs-out every
  logged-in user.
- **Local accounts enabled** (`settings.local_auth.enabled`,
  [`localusers.go`](localusers.go)) — individually provisioned local
  accounts as an alternative *and* complement to LDAP: useful for people
  without an AD account (external partners, contractors, test logins) or
  for deployments that don't run AD/LDAP at all. Accounts are managed in
  the admin "Benutzer" tab (`/api/admin/users*`,
  [`handlers_local_users.go`](handlers_local_users.go)) and stored in
  their own file using the **same storage backend as the chunk store**
  (`storage.backend`: tinySQL or SQLite; path via `storage.users_path`,
  defaults `r3-users-tinysql`/`r3-users.db`) — never in `settings.json`.
  Passwords are **bcrypt-hashed** (per-account salt embedded, configurable
  cost via `local_auth.bcrypt_cost`, default 12; minimum length via
  `local_auth.min_password_length`, default 12) — deliberately *not* the
  unsalted SHA-256 used for API keys, which is only acceptable for
  high-entropy random tokens. Login order in `handleLDAPLogin`:
  break-glass env password → local account (by username, **no fallthrough
  to LDAP on a wrong local password**, to avoid username-collision
  confusion) → LDAP bind. A local account carries its own
  department/`dept_code` (auto-classified from the free-text department
  via `department_rules.json`, or set explicitly), so `source_access`
  department filtering and personalization work exactly as for AD users.
  Enabling local auth alone — without LDAP — turns on the same real
  server-side enforcement of every admin route and "Registriert"-tier
  gate that enabling LDAP does.

Either way, `/api/sources/content`, `/api/sources/original` and
`/api/draft/reply` stay intentionally ungated — they're reachable from a
chat citation popup for any user who can already see that citation, not
just admins. They still enforce `source_access` themselves though
(`sourceAccessAllowedForRequest` in handlers.go): a department-restricted
`source_kind` isn't fetchable by `source_id` alone just because these
routes carry no session requirement.

**Admins bypass department restrictions everywhere.** An admin session
resolves to a sentinel department code (`adminDeptCode`, see
`resolveDeptCode` in [`settings.go`](settings.go)) that
`sourceAccessAllowed` treats as "allowed for every `source_kind`" — so an
admin's chat/agent retrieval, direct source/chunk fetch, and draft-from-
source never 404 on a department-restricted source, matching the Chunks
tab which already shows admins every chunk unfiltered. This is deliberate:
an admin is already trusted with Settings and the full audit log, so
scoping their *read* access below what the Chunks viewer already grants
would be inconsistent, not safer.

**Debug-Modus** is likewise tied to admin status, not a hardcoded user:
`debugModeAllowed` (handlers.go) returns true for any `IsAdmin` session,
so every admin gets the per-request debug panel (retrieved chunks with
per-signal scores, the exact messages sent to the model, every tool call
with timings, the raw answer, plus the resolved profile/preset/dept-code
and total request duration) on Chat, Agent and Mail — governed by the
same `ldap.admin_users`/`ldap.required_group_dn` mechanism as every other
admin capability.

## Admin notifications: pushed live via SSE

A small in-memory admin notification feed (`notifications.go`) surfaces
toast-worthy events — e.g. "Import X fertig" — to every logged-in admin's
browser as a `novapop.js` toast. It's pushed **live** over Server-Sent
Events (`handleAdminNotificationsStream`, one long-lived connection per open
admin tab via `web/app.js`'s `EventSource`) rather than the original design
of an 8-second poll from every tab: harmless at small scale, but needlessly
chatty in the access log, and each poll paid for a full request/response
round trip for what's almost always "nothing new." The original one-shot
`GET /api/admin/notifications` poll endpoint (`handleAdminNotifications`)
stays registered too, for any caller that genuinely wants point-in-time
polling (a script, a future non-browser integration) — both share the same
in-memory history ring, they only differ in whether the server pushes
updates on top of the initial catch-up. Modeled on `scheduler.go`'s history
ring buffer: capped size, no disk persistence, a restart drops any pending
notifications (independent of the session-persistence change above — this
is transient UI feedback, not something that needs to survive a restart).

## Testing connectors from Settings

Every external interface configured in the Settings tab — all six LLM chat
profiles, LDAP, SharePoint, Exchange, IMAP, Teams, Confluence, Jira,
Freshservice, SMTP, MSSQL — has a "Verbindung testen" button right in its
card (see [`conntest.go`](conntest.go), `POST /api/settings/test/<name>`,
`requireAdminSession`-gated same as `/api/settings` itself). Clicking it
tests the form's *current, not-yet-saved* values, not what's already
persisted in `settings.json`, so a credential can be validated before
committing it with "Einstellungen speichern". Each test reuses the
connector's real code path — the same `previewXPages`/`previewXIssues`
function the Import tab's preview step calls, `sendMail` for SMTP,
`ldapAuthenticate` for LDAP, a `sql.Open`+`PingContext` for MSSQL, an IMAP
dial+login for IMAP, and a real chat completion for every configured LLM
profile. The local profile additionally performs an embedding call; cloud
profiles are intentionally chat-only because RubixRAG keeps embeddings in
LM Studio. The test therefore exercises the actual provider wire format —
including Claude/Gemini native chat — rather than a separate, lighter probe.

Two connectors need throwaway input beyond their saved config, since
neither has a testable credential of its own: LDAP's card has a
"Testbenutzer"/"Testpasswort" pair that runs one real AD bind + attribute
lookup exactly like a normal login (never persisted), and SMTP's card has
a "Test-Empfänger" address the test mail is sent to (also never persisted
— unlike the chat "send to me" feature, which can only ever mail the
logged-in user's own AD address, see `smtpConfig.From`'s doc comment in
`settings.go`, this admin-only debug action can target any address typed
into the field).

Every response is the same `{ok, detail}` shape regardless of connector —
a failed test is a normal 200 response reporting a negative result, not an
HTTP error; only a malformed request body is a real 4xx.

**Full request/response on click.** For the HTTP-based connectors (all
configured LLM profiles, Shop, Confluence, Jira, Freshservice, SharePoint, Exchange,
Teams) the test result carries the raw, secret-redacted request and
response of every HTTP call it made, behind a per-result "Details
anzeigen" toggle (never shown inline). A context-carried tracing
`http.RoundTripper` ([`conntrace.go`](conntrace.go)) captures method, URL,
headers (Authorization/api-key values masked) and bodies; the panel offers
a text and a hex view, so a garbled/binary/empty-body response (the class
of failure behind the Shop "HTTP 200 but no token field" case) is
inspectable without a rebuild. The socket/driver-based tests (LDAP, IMAP,
SMTP, MSSQL) don't carry raw dumps — their transports live inside third-
party libraries that don't expose the wire bytes — so those still report
just the `{ok, detail}` line.

**Settings change history.** Every successful "Einstellungen speichern"
appends a field-level diff (old → new, JSON-path-precise) to an append-
only log ([`settings_history.go`](settings_history.go),
`GET /api/settings/history`), surfaced in a "Änderungshistorie" card so an
admin can see who changed what and when. Secret-bearing paths
(passwords/tokens/client-secrets, but not the `*_env` variable *names*)
are recorded as "changed" with **no values** — the history never becomes a
new credential store. The audit log's `settings_update` entry carries the
same changed-path summary (paths only, never values).

The local LLM-Backend card additionally has a "Modelle laden" button
(`POST /api/settings/test/llm-models`, `lmClient.listModels` in `llm.go`)
that queries the backend's standard `GET /v1/models` listing and fills a
`<datalist>` both the Chat-Modell and Embedding-Modell fields point at —
lets an admin pick an exact, currently-loaded model id instead of typing
one blind. This listing is available for local/OpenAI-compatible endpoints;
Azure deployments and native Claude/Gemini model names are entered directly.

## Security / privacy considerations

- **Audit trail** (`audit.go`, `docs/ENTERPRISE_READINESS.md` Phase A) —
  every security-relevant action is appended as one JSON line to
  `r3-audit.jsonl` (next to whichever `-settings` file the instance uses):
  who asked what (question only, never the generated answer), every
  import (connector, chunk/skip/error counts, `dry_run` — never content),
  settings saves, source deletions, admin/LDAP login attempts (success
  **and** failure), mail draft generation, filing a draft into a mailbox,
  and API key create/revoke. `Actor` is the session's AD mail (falling
  back to CN), or `"anonym"` with no session — never a password. No admin
  UI yet, deliberately: read the file directly (`tail`/`grep`) until real
  usage patterns justify building a viewer.
- `redact_pii` (off by default) strips emails/phone numbers/IBANs/card
  numbers from extracted text before chunking — useful when a mailbox
  export will be shared more broadly than the original inbox access.
- `source_visibility` (empty by default, i.e. everything visible) maps a
  `source_kind` (e.g. `pst_email`, `pst_attachment`) to `false` to stop
  that type's chunks from ever being shown as a citation — the content
  still grounds answers exactly as before (`rankedSearch`/
  `assembleContext` don't consult this setting at all), only the citation
  chip/source name presented to the user is suppressed after generation
  (`filterCitations` in [`rank.go`](rank.go)). Configurable per type from
  Settings → "Zitate (Sichtbarkeit je Quelltyp)", intended for sources
  like a PST mailbox import that should inform an answer without exposing
  the underlying mailbox content as a named, clickable source.
- `source_access` (empty by default, i.e. unrestricted) is the real
  counterpart to `source_visibility` above: it maps a `source_kind` to a
  list of department codes and is enforced *before* retrieval even
  starts (`filterByDeptAccess` in [`rank.go`](rank.go)) — a chunk whose
  kind is restricted to departments the current requester isn't in is
  dropped from the candidate set entirely and never reaches the LLM's
  context, unlike `source_visibility`, which only hides an already-used
  source from the citation list after the fact. The same check also gates
  direct `source_id` lookups — `/api/sources/content`, `/api/sources/
  original` and `/api/draft/reply` (`sourceAccessAllowedForRequest` in
  handlers.go) — so a restricted source can't be fetched by ID even
  though those three routes carry no session requirement of their own. A
  requester's department is classified from their AD `department`/`title`
  attribute at LDAP
  login ([`department.go`](department.go)'s `classifyDepartment`, a
  regex table ported from Rubix's existing AD-attribute conventions) onto
  one of a fixed set of codes (`Vertrieb`, `IT`, `Einkauf`, `Logistik`,
  ... — Settings → "Zugriffskontrolle je Quelltyp (Abteilungen)" lists
  all of them). The rule table itself is admin-overridable via a
  `department_rules.json` file in `prompts_dir` (same directory/pattern as
  `index.md`/`skill_*.md`, re-read on every login) — invalid JSON or a
  bad regex logs a warning and falls back to the built-in defaults rather
  than breaking every login's classification. An anonymous (not logged
  in) requester is always treated
  as department `""`, so only kinds with no restriction configured are
  ever visible to them. Requires `ldap.enabled` — without a session,
  every caller is anonymous. `personalize_answers` (off by default) is
  the softer, non-security-relevant use of the same login: when on,
  `handleAsk` prepends the logged-in user's name/department/title/office
  to the system prompt purely for tone/register — this does send those AD
  attributes to whichever chat backend answers the question, including
  Azure OpenAI if that profile is selected, worth knowing before enabling
  it on a deployment with the Azure profile active.
- **Per-document ACLs** add a second, narrower allow-list to
  `source_access`: in Sources, an admin can choose which department codes
  and/or authenticated user IDs may retrieve one specific document. The
  rule is stored owner-only in `r3-source-acl.json`; an empty rule means
  “inherit the source-kind policy”. It is checked for ranked retrieval,
  citation/source-content lookups, attachment context, Agent tools and MCP
  tools, so knowing a `source_id` does not bypass it. These are local,
  administrator-maintained rules today — upstream SharePoint/Teams item ACL
  synchronization remains a separate future integration.
- `allow_shell_exec` (off by default) gates every call to the external
  `markitdown` binary.
- `enable_chat_history` (off by default) lets the browser remember past
  conversations so they can be reopened later (sidebar "Verlauf" button).
  Stored entirely in that browser's `localStorage` — never sent to or
  kept by the R3 server — since regular chat has no per-user login for a
  server-side history to belong to. Clearing browser data removes it;
  there's no server-side copy to also delete.
- Login endpoints (`/api/auth/login`, `/api/admin/check`) are rate-limited
  per client IP — 10 failed attempts within 5 minutes locks that IP out
  for the rest of the window (`ratelimit.go`). In-memory, no persistence
  (a restart clears it), and it doesn't help against an attack spread
  across many IPs — that still needs a reverse proxy/WAF in front, same
  as every other rate-limiting gap noted in `docs/API.md`.
- `/api/ask` itself additionally rate-limits **guest** (not-logged-in)
  callers per client IP (`settings.api.guest_ask_rate_limit_per_minute`,
  `ratelimit.go`'s `requestLimiter`) — a logged-in/admin session is never
  throttled by this; it exists purely to stop an anonymous caller from
  hammering the LLM backend, not as a real abuse-prevention system (same
  in-memory, restart-clears, single-IP-only caveats as above).
  `/api/voice/transcribe` has its own independent counterpart
  (`settings.api.guest_voice_rate_limit_per_minute`, a second
  `requestLimiter` instance) — a voice request is heavier (spawns an
  external ffmpeg/whisper.cpp process) than a plain `/api/ask` call, so an
  unbounded anonymous caller could exhaust host CPU faster. A separate,
  server-wide concurrency cap (`import.whisper_max_concurrent`) bounds
  total simultaneous transcriptions regardless of caller.
- Every answer gets a 👍/👎 feedback control (`feedback.go`) that appends a
  small JSONL record (question, answer, citations, thumbs up/down,
  timestamp, user if known) to a local log. The Jobs tab's
  **"Feedback-Auswertung"** card (`readFeedbackStats`, admin-only
  `GET /api/feedback/stats`) turns that log into an at-a-glance aggregate —
  overall 👍/👎 rate, which cited sources accumulate the most 👎 relative
  to 👍 (candidates for re-import, cleanup or exclusion), and the most
  recent downvoted questions — recomputed fresh from the log on every
  request rather than kept in a database, since it's a small admin panel,
  not a hot path.
- Access comes in three tiers depending on how a caller identifies: an
  anonymous **guest** (subject to the rate limit above and to
  `source_access`'s empty-department default), a **registered** user
  (LDAP-authenticated but not an admin — sees a "Mein Konto" panel with
  their AD identity), and an **admin** session as described above. This is
  additive to, not a replacement for, the `source_access`/
  `source_visibility` department controls already described.
- No built-in *server-side* authentication — same as tinyRAG, run behind a
  reverse proxy with auth, or restrict network access, before pointing
  this at a real mailbox export. The admin-login button (see above) does
  not change this.
- PST files and mailbox exports are sensitive. The vector store directory
  (`storage.path`) and any staged temp files contain full message bodies
  — treat them with the same handling requirements as the original
  mailbox.

## Human-in-the-loop by design

The draft feature (`draft.go`) generates proposals grounded in retrieved
context but has **no send path** — R3 is not designed to send mail
autonomously at any phase. The "Mail" tab (gated by
`enable_draft_replies`) covers two modes against the same
`/api/draft/reply` endpoint: paste an incoming mail for a grounded reply
draft, or describe recipient/topic/key points (`brief`) and
`composeNewMail` drafts a brand-new mail, including a proposed subject
line. Either way the result lands in editable subject/body fields, and the
human then copies it, downloads it as an `.eml` (opens in Outlook & Co.
for final review/send), or — admin-gated, IMAP configured — files it via
`POST /api/draft/save-imap` into the mailbox's Drafts folder
(`AppendDraft`, `\Draft` flag, folder per `imap.drafts_mailbox`, default
"Drafts"). Appending a draft for human review is the only write R3 ever
performs against a mailbox; what's still open from the original plan is
only the *auto*-trigger (new incoming mail → draft proposed without a
click) — see `draft.go`'s package comment for the current status map.

## Technology choices

A few "does X make sense here" questions come up naturally for a project
like this — short answers, longer reasoning in commit history/PRs as it
comes up:

- **RAG** — yes, obviously the core of the project.
- **Vector DB** — tinySQL (embedded, in the same binary, currently
  v0.21.1 — see `docs/DEPENDENCIES.md`) is the right choice at mailbox
  scale (thousands to low millions of chunks): zero extra infra,
  `VEC_COSINE_SIMILARITY` built into SQL, and it's what tinyRAG itself
  uses. Keyword scoring uses real BM25 on both backends — tinySQL's
  `FTS_SEARCH`, or a duplicate FTS5 virtual table for the SQLite backend
  (`vectorstore_sqlite.go`'s `keywordCandidatesFTS`) — replacing an
  earlier hand-rolled keyword-overlap heuristic that's now only a
  defensive fallback for a hypothetical future backend implementing
  neither. Vector candidate lookups go through tinySQL's own version-aware
  `VEC_SEARCH` result cache (`tinysql.ConfigureVectorCache` in
  `vectorstore_tinysql.go`) — adopted from upstream tinySQL rather than
  reimplemented in R3.
  `store.go` doesn't abstract this behind an
  interface yet; if/when scale or a managed-service requirement forces a
  swap to something like Qdrant or pgvector, do that abstraction *then*
  (see "What's worth adopting from tinyRAG-f18c4fa" below — the upstream
  project already built exactly that seam).
- **MQTT** — no clear fit. R3 is request/response (browser Q&A) and batch
  (PST/file import), not an event stream; MQTT would earn its place only
  if another Rubix system needs to be pushed "a document just changed"
  notifications in real time, which nothing here currently requires.
- **gRPC** — not yet. Useful once R3 needs a typed machine-to-machine API
  (e.g. another internal Rubix service calling into ingestion or
  retrieval directly), or once it splits into multiple processes. For a
  single binary with a browser client, plain JSON/HTTP is simpler and
  sufficient.
- **Git** — recommended regardless of the RAG-specific questions above:
  this directory isn't a git repository yet. Basic hygiene, unrelated to
  any of the above trade-offs.

### What's worth adopting from tinyRAG-f18c4fa

The upstream tinyRAG project has grown a much larger, more mature version
(`tinyRAG-f18c4fa` — main.go is ~6x bigger) with new subsystems worth
knowing about for R3's own evolution. Notably, it has a file literally
named `r3.go` implementing exactly the "ranked RAG" idea this project is
named after — a `RankingPolicy` scoring semantic similarity together with
source quality, trust level, freshness decay, feedback signal and
sensitivity/conflict penalties, plus role/ACL-aware filtering. That
validates the direction `rank.go` is already going in; the natural next
step is widening R3's scoring beyond vector+keyword+recency toward those
same signals as provenance metadata accumulates (e.g. a manually-curated
FAQ answer should outrank an ambiguous old email even at equal semantic
similarity).

Other pieces worth borrowing later, roughly in order of value for R3's
mailbox-RAG use case:

1. **`vector_store.go`'s `vectorChunkStore` interface** — an abstraction
   over the persistence layer (tinySQL today, something else later)
   instead of `ragSystem` calling tinySQL directly. Cheap insurance for
   the "swap the vector DB" question above.
2. **`telemetry.go`'s structured per-request JSON logging** — request id,
   question length, chosen profile, chunk counts, timing, success/error —
   genuinely useful for understanding retrieval quality and Azure-vs-local
   cost/latency trade-offs in practice, and cheap to add.
3. **`ingest.go`'s declarative `ragPullSource`** (id/kind/path/recursive/
   `interval_seconds`) — a config-driven periodic re-sync source, which is
   exactly the shape Phase 2's IMAP polling will want, and also generalizes
   R3's current folder import into "re-scan this folder every N seconds"
   without new code.
4. **`router.go`'s heuristic direct/retrieval/agentic query router** —
   relevant if R3 later adds tool-calling (e.g. "look up this order
   number") and shouldn't always pay for a full RAG retrieval on
   conceptual questions the model can answer directly.

Not relevant to R3 as scoped: `connectors.go`'s generic HTTP/SQL connector
framework and `okf.go`'s CKAN open-data importer solve problems R3 doesn't
have (mailbox/document RAG, not arbitrary API/open-data ingestion).

## What still needs real-environment verification

All connectors above were built and tested against fake in-process
servers (`httptest`) standing in for Microsoft Graph, Confluence's REST
API and a generic web server — the sandbox this code was written in has
no real Active Directory server, Azure AD tenant, IMAP mailbox,
Confluence space or SQL Server reachable from it. Test coverage per
connector:

| Connector | Tested here | Still needs a real target |
|---|---|---|
| LDAP/AD login | filter-injection escaping, group-membership logic | an actual AD bind against a real domain controller |
| SharePoint / Exchange Graph / Teams | OAuth2 token-cache logic, request shaping, response parsing, ingest wiring — all against a fake Graph server | a real Azure AD app registration with the stated Graph permissions, admin-consented |
| IMAP | `mailboxConfig.resolvedPassword`, `importIMAPMessages`'s ingestion/LastUID logic (fake `imapClient`) | `realIMAPClient`'s actual wire protocol (`go-imap/v2`) against a real/staging mailbox |
| Confluence | REST client auth header, pagination-free listing, page-fetch-and-ingest | a real Confluence Cloud space + API token |
| Jira | REST client auth header, JQL search, issue-fetch-and-ingest | a real Jira Cloud project + API token |
| Generic website import | SSRF guard (loopback/private/link-local rejection), fetch-and-ingest, `<title>` extraction | nothing sandbox-specific — should work as-is against any public URL |
| MSSQL tool | `validateSelectOnly`'s statement blocklist, DSN construction, JSON tool-argument decoding, the generic `chatWithTools` round-trip (fake OpenAI-compatible server) | a real SQL Server connection (open/query/row-formatting path is unexercised), and a genuinely tool-calling-capable chat model/backend |

None of this is a substitute for a first real run against Rubix's actual
AD, Azure tenant, mailboxes, Confluence instance and database — treat that
as required validation before relying on any of these connectors in
production, not optional follow-up.

## Credits

R3 stands on the shoulders of giants — a stack of open-source projects,
plus tools and lessons from the author's own earlier career that shaped
how it's built.

### Open-source projects this depends on

- **Go libraries** (see `go.mod` for exact versions): [go-imap](https://github.com/emersion/go-imap)
  by emersion (the IMAP connector, `imapmail.go`/`imap.go`), [go-ldap](https://github.com/go-ldap/ldap)
  (Active Directory login, `ldapauth.go`), [go-mssqldb](https://github.com/microsoft/go-mssqldb)
  by Microsoft (the live MSSQL query tool, `shop.go`), [go-pst](https://github.com/mooijtech/go-pst)
  by Marten Mooij (PST mailbox export parsing, `pst.go`), [eris](https://github.com/rotisserie/eris)
  (stack-trace-aware error wrapping in the PST import path), [golang.org/x/text](https://pkg.go.dev/golang.org/x/text)
  (the Go team — Windows-1252/mojibake charset repair, `encoding.go`), and
  [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) by Jan Mercl (`cznic`)
  and contributors — a pure-Go, no-CGO SQLite build (riding on the same
  team's `modernc.org/libc`/`mathutil`/`memory`) that backs R3's optional
  SQLite storage backend.
- **Office/PDF extraction** via [markitdown](https://github.com/microsoft/markitdown)
  by Microsoft.
- **Local LLM backends.** R3 talks to any OpenAI-compatible server, but
  [llama.cpp](https://github.com/ggml-org/llama.cpp) (Georgi Gerganov and
  the ggml-org community) is the open-source inference engine most local
  setups are quietly built on, and [LM Studio](https://lmstudio.ai) — a
  free desktop app built on top of it, though not itself open source — is
  what this project defaults to for local/offline use. The same ggml
  lineage's [whisper.cpp](https://github.com/ggml-org/whisper.cpp) powers
  R3's local voice-input transcription (`whisper.go`) — see "Installing
  dependencies (Debian)" above for building it.
- **Diagrams and charts** in rendered answers come from [Mermaid](https://mermaid.js.org)
  and [d3.js](https://d3js.org) (CDN-loaded by default, self-hostable —
  see "Rendered output formats" above).
- **Audio/OCR tooling**: `ffmpeg` normalizes voice-input audio before
  transcription; `tesseract` OCRs image uploads/attachments — both
  long-established open-source projects in their own right.

### Simon Waldherr's own tools

**Open source, reused directly:** [tinyRAG](https://github.com/SimonWaldherr/tinyRAG) —
R3's architectural starting point (the provider-abstracted LLM client in
`llm.go`/`llm_native.go` follows the local/Azure URL/auth pattern used by
the sibling `llmflow6` and `promptcron` projects, and adds native
Claude/Gemini adapters while keeping embeddings local). [tinySQL](https://github.com/SimonWaldherr/tinySQL) —
the default embedded vector/SQL store. [novapop](https://github.com/SimonWaldherr/novapop)
(MIT) — vendored for the toast/notification UI (`web/novapop.js`).
[nanoGo](https://github.com/SimonWaldherr/nanoGo) — planned (not yet
integrated) as the sandboxed Go interpreter for the `run_code` agent tool
seam, hosted inside a [wazero](https://github.com/tetratelabs/wazero) WASM
wall (see "Live tools" above).

**Closed-source tools from earlier roles, at ZITEC GmbH and RUBIX GmbH**
(ZITEC is RUBIX's legal predecessor company, so reusing/relicensing that
code here is above-board, not a gray area) — never published externally,
but a real, direct ancestor of R3, not just an inspiration: parts of it
started as recycled code from these tools, since substantially rewritten
and improved. Most directly **ZNDZ**, an internal MSSQL/SAP-SE16
article-master query and reporting tool whose live-database-lookup logic
is the direct predecessor of both R3's MSSQL live-query tool (`shop.go`)
and its SAP SE16 REST connector template (`connector.go`,
`docs/CONNECTORS.md`); also **ARCA** and other internal tooling from the
same era.
