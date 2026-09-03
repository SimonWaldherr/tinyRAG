# tinyRAG

[![DOI](https://zenodo.org/badge/1158167759.svg)](https://doi.org/10.5281/zenodo.18652377)
[![Go Version](https://img.shields.io/github/go-mod/go-version/SimonWaldherr/tinyRAG)](https://golang.org)
[![Release](https://img.shields.io/github/v/release/SimonWaldherr/tinyRAG?label=release)](https://github.com/SimonWaldherr/tinyRAG/releases)
[![Stars](https://img.shields.io/github/stars/SimonWaldherr/tinyRAG?style=social)](https://github.com/SimonWaldherr/tinyRAG/stargazers)

A lightweight Retrieval-Augmented Generation (RAG) system with a modern web interface, built in Go.

## Features

- **R³ Governed Retrieval**: Ranked, Responsible, Retrieval with policy-driven scoring and citations
- **Semantic Search**: Store and search documents using vector embeddings
- **RAG Chat**: Ask questions and get answers based on your knowledge base
- **Multiple Data Sources**:
  - Wikipedia articles
  - Web scraping
  - Text input
  - File upload (.txt, .md, .csv, .json, .xml, .html, .log)
  - Folder import (recursive)
- **OpenAI-Compatible API**: Works with any OpenAI-compatible LLM backend (LM Studio, Ollama, etc.)
- **Custom APIs**: Add external API integrations
- **Personas**: Configure different conversation styles with pre-prompts
- **Persistent Agent Memory**: Save explicit preferences and stable context for future chats; entries are opt-in and never inferred from transcripts
- **Themes**: Multiple built-in themes (Dark, Light, Nord, Solarized, Monokai, Dracula)
- **Code Execution**: Optional support for nanoGo (interpreted Go) execution
- **Embedded Frontend**: No separate build required - all assets embedded in the binary

## Requirements

- Go 1.26.5 or later
- An OpenAI-compatible LLM backend (e.g., [LM Studio](https://lmstudio.ai/), [Ollama](https://ollama.ai/))

## Installation

### From Source

```bash
# Clone the repository
git clone https://github.com/SimonWaldherr/tinyRAG.git
cd tinyRAG

# Build the application
go build

# Run
./tinyRAG -web -addr :8080 -db tinyrag.gob
```

### Using Make

```bash
# Format, vet, and run
make dev

# Or build only
make build

# Run all checks
make check
```

## Usage

### Starting the Server

```bash
./tinyRAG -web -addr :8080 -db tinyrag.gob
```

Options:
- `-web`: Enable web interface (default: true)
- `-addr`: Server address (default: :8080)
- `-db`: Database file path (default: tinyrag.gob)
- `-url`: LLM API base URL (default: http://localhost:1234)
- `-inference-api`: inference wire protocol (`auto`, `openai`, or `ollama`)
- `-chat`: Chat model name
- `-embed`: Embedding model name
- `-lang`: Language code (default: de)
- `-chunk`: Chunk size for text splitting (default: 800)
- `-k`: Number of chunks to retrieve for RAG (default: 5)

### Configuration

The application stores its configuration in `settings.json`. You can modify this file or use the web interface settings panel.

Example configuration:
```json
{
  "version": 3,
  "base_url": "http://localhost:1234",
  "chat_model": "mistralai/ministral-3-14b-reasoning",
  "embed_model": "text-embedding-nomic-embed-text-v1.5",
  "lang": "de",
  "theme": "monokai",
  "chunk_size": 800,
  "k": 5,
  "custom_apis": [],
  "personas": [
    {
      "id": "persona-default",
      "name": "Standard",
      "prompt": ""
    }
  ],
  "agent_memory_enabled": false,
  "agent_memory": [],
  "allow_code_exec": false,
  "allow_nanogo": false
}
```

### Persistent agent memory

Under **Settings → Memory**, users can save up to 32 explicit, short pieces
of durable context, such as language preferences, project conventions, or a
default timezone. Memory is disabled by default and is only included in future
answers after it has been enabled.

tinyRAG never creates memory entries from conversations, model output, or tool
results. Entries can be added, removed, or disabled in the UI; the protected
API offers the same operations through `GET`/`POST /api/memory` and
`POST /api/memory/delete`.

### tinySQL v0.49.0 features

tinyRAG uses tinySQL v0.49.0. The settings panel exposes the following
optional database features:

- **Native vector retrieval** uses `VEC_SEARCH` for faster candidate lookup.
  The existing scalar retrieval mode remains available when maximum recall is
  preferred.
- **Hybrid retrieval (new)** uses tinySQL's `HYBRID_SEARCH` to fuse the vector
  candidate list with a real BM25 full-text pass over chunk content via
  reciprocal rank fusion, recovering exact identifiers and rare terms that
  cosine similarity alone can miss. The R³ ranking pipeline still scores every
  candidate on plain cosine similarity, not the raw fusion score, so
  downstream thresholds/reranking behave exactly as before.
- **Configurable vector index (new)** picks the ANN strategy `VEC_SEARCH`/
  `HYBRID_SEARCH` use: `flat` (exact, default), `ivf`, or `hnsw`. Benchmark
  before switching away from `flat` — corpus size/shape determines the winner.
- **Startup index warm-up (new)** runs tinySQL's `VEC_WARM` once at startup
  when `vector`/`hybrid` retrieval is enabled, so the first real query never
  pays the one-time index-build cost. A no-op under the default `scalar` mode.
- **Vector result cache** stores only deterministic result IDs, never source
  text or embedding vectors. New and migrated installations enable a bounded
  cache with 128 entries and a 30-second TTL. Set
  `tinysql_vector_cache_entries` to `0` to disable it.
- **Vector analytics** records only query shape and timing. Administrators can
  inspect cache and analytics state at `GET /api/debug/vector-cache`.
- **Portable snapshots** are available to administrators at
  `GET /api/debug/database-snapshot`. They are consistent tinySQL GOB exports
  and work independently of the selected storage backend.
- **Tamper-evident audit logging** writes a hash-chained JSONL audit trail.
  It can be enabled with `tinysql_audit_enabled`; changing it requires a
  restart.
- **Encryption at rest** for disk, index, and hybrid storage modes reads a
  32-byte hexadecimal or Base64 key from `TINYRAG_STORAGE_KEY`. The key is
  never written to `settings.json`; enabling encryption requires a restart.
- **Geodata import** accepts GeoJSON, KML, and OpenStreetMap XML through the
  Open Data panel when `geo_import_enabled` is enabled.

See [docs/tinysql-optional-features.md](docs/tinysql-optional-features.md) for
operational details and constraints.

The universal retrieval design and its staged relation-aware roadmap are in
[docs/retrieval-architecture.md](docs/retrieval-architecture.md).

Configured REST, JSON-RPC 2.0, and SQL capabilities are documented in
[docs/connectors.md](docs/connectors.md). Agent execution admits only the
explicitly read-only subset of those capabilities.

### Setting up LLM Backend

tinyRAG talks to any OpenAI-compatible chat/embeddings endpoint, local or
cloud, and can also use the native Ollama `/api` protocol. The **provider switcher** in the top toolbar lists common presets
(grouped Local / Cloud) and pre-fills the default base URL for each; picking
one probes the endpoint, lists available models, and lets you apply a chat
model in a couple of clicks. "Custom..." opens Settings for anything not in
the list — any OpenAI-compatible server works even if it isn't listed.

| Provider | Type | Default base URL |
|---|---|---|
| LM Studio | Local | `http://localhost:1234` |
| Ollama | Local | `http://localhost:11434` |
| llama.cpp (`llama-server`) | Local | `http://localhost:8080` |
| vLLM | Local | `http://localhost:8000` |
| text-generation-webui | Local | `http://localhost:5000` |
| KoboldCpp | Local | `http://localhost:5001` |
| Jan | Local | `http://localhost:1337` |
| LocalAI | Local | `http://localhost:8080` |
| GopherLLM | Local | `http://localhost:8091` (embedded demo default) |
| RustyLLM | Local | `http://localhost:8091` (change if your server differs) |
| OpenAI | Cloud | `https://api.openai.com` |
| Anthropic | Cloud | `https://api.anthropic.com` |
| Google Gemini | Cloud | `https://generativelanguage.googleapis.com` |
| Mistral AI | Cloud | `https://api.mistral.ai` |
| Groq | Cloud | `https://api.groq.com/openai` |
| DeepSeek | Cloud | `https://api.deepseek.com` |
| Together AI | Cloud | `https://api.together.xyz` |
| xAI (Grok) | Cloud | `https://api.x.ai` |
| Cohere | Cloud | `https://api.cohere.ai` |
| Perplexity | Cloud | `https://api.perplexity.ai` |
| OpenRouter | Cloud | `https://openrouter.ai/api` |

Full setup instructions (install commands, default models, quirks per
provider) are in **[docs/llm-providers.md](docs/llm-providers.md)**.

**Zero-install demo option**: build with `-tags demo_llm` and run
`./tinyRAG -demo-llm-model auto` to run a tiny pure-Go model
([GopherLLM](https://github.com/SimonWaldherr/GopherLLM)) in-process — no
LM Studio/Ollama/llama.cpp needed. Demo quality only; see
[docs/llm-providers.md](docs/llm-providers.md#demo-mode-embedded-gopherllm-no-external-tool-at-all).

Quick start with the two most common local runners:

1. **LM Studio**:
   - Download and install [LM Studio](https://lmstudio.ai/)
   - Load a chat model (e.g., Mistral, Llama)
   - Load an embedding model (e.g., nomic-embed-text)
   - Start the local server (usually runs on port 1234)

2. **Ollama**:
   ```bash
   # Install Ollama
   curl -fsSL https://ollama.ai/install.sh | sh
   
   # Pull models
   ollama pull llama2
   ollama pull nomic-embed-text
   ```

3. Configure tinyRAG:
   - Open the web interface
   - Click the provider switcher in the toolbar and pick your backend, **or**
     click the settings (⚙) button → "LLM Backend" tab for manual entry
   - Enter your API endpoint (if not using the switcher)
   - Click "Test & Load Models"
   - Select your chat and embedding models
   - Click "Save"

On startup, if the configured endpoint is unreachable, tinyRAG automatically
probes the common local ports above (LM Studio, Ollama, llama.cpp, vLLM,
text-generation-webui, KoboldCpp, Jan) and switches to the first one it finds
— see `maybePreferOfflineLLM` in [llm_discovery.go](llm_discovery.go).

## Web Interface

Access the web interface at `http://localhost:8080` (or your configured address).

### Main Panels

1. **Chat**: Ask questions about your knowledge base
2. **Search**: Perform semantic search on stored chunks
3. **Data Import**: Add documents to your knowledge base
   - Wikipedia: Load articles directly
   - URL: Scrape web pages
   - Text: Paste text content
   - Upload: Upload text files
   - Folder: Import entire directories

### Sidebar

- **Chats**: View and manage conversation history
- **Sources**: Browse imported documents

### Settings

- **General**: Theme selection and general options
- **LLM Backend**: Configure API endpoint and models
- **Custom APIs**: Add external API integrations
- **Personas**: Create conversation personas with custom prompts
- **Memory**: Explicitly save, review, enable, and delete durable assistant context

## Development

### Project Structure

```
.
├── main.go        # Main application code (4067 lines)
├── index.html     # Frontend HTML
├── app.js         # Frontend JavaScript (1270 lines)
├── style.css      # Frontend CSS (991 lines)
├── settings.json  # Application configuration
├── go.mod         # Go module definition
├── go.sum         # Go module checksums
└── Makefile       # Build automation
```

### Make Targets

```bash
make fmt          # Format Go code
make vet          # Run go vet
make lint         # Run golangci-lint
make tidy         # Tidy Go modules
make build        # Build the application
make test         # Run tests
make check        # Run all checks (fmt, vet, lint, test)
make run          # Run the application
make dev          # Format, vet, and run
make help         # Show available targets
```

### Code Style

The project follows standard Go conventions:
- Use `gofmt` for formatting
- Run `go vet` to catch common issues
- Use `golangci-lint` for comprehensive linting

## Architecture

### Storage

- Uses [tinySQL](https://github.com/SimonWaldherr/tinySQL) for embedded database
- Data persisted in `.gob` format
- Three main stores:
  - **Chunks**: Vector embeddings and text content
  - **Chats**: Conversation history
  - **Sources**: Document metadata

### Vector Search

- Cosine similarity for semantic search
- Configurable chunk size and retrieval count (k)
- Efficient in-memory vector operations
- Native tinySQL `VEC_SEARCH` with optional bounded result caching and
  privacy-preserving query analytics
- Portable database snapshots for backend-independent administrative backups
- Optional hybrid retrieval (`HYBRID_SEARCH`: vector + BM25 via reciprocal
  rank fusion) and configurable ANN index (`flat`/`ivf`/`hnsw`) with startup
  warm-up (`VEC_WARM`)
- Metadata-aware R³ ranking with trust, quality, freshness, feedback, and sensitivity penalties

### Chunking & Ingestion

- **Structure-aware chunking**: text is split into atomic blocks first — a
  run of numbered/bulleted list items, a table's rows, or a fenced code
  block — so a character-budget cut lands between blocks, not in the middle
  of a step-by-step procedure or a table row. Plain prose chunking behavior
  is unchanged. An oversized block/line that still exceeds the budget is now
  split by its own rows/lines (falling back to word-boundary slicing only
  for a genuinely unbreakable line), instead of passing through unbounded.
- **Office document structure preservation**: DOCX/PPTX/XLSX/ODT/ODP/ODS
  text extraction now emits a line break at paragraph, table row, and table
  cell boundaries (instead of collapsing an entire document/sheet into one
  line), so the structure-aware chunker above can recognize spreadsheet rows
  and table structure in these formats too.
- **Source-type quality tiers** (`r3.go`) include a generic `structured_item`
  tier for small, well-formed structured records — a glossary entry, a
  reference-table row, a course module or Q/A pair — imported one record at
  a time (CSV/JSON row import uses this tier automatically); the dedicated
  `wiki` tier is now actually applied by the Wikipedia import endpoint
  instead of silently falling back to the generic default.
- **Operator-configurable terminology** (`terminology` setting, managed via
  `GET`/`POST /api/settings/terminology`): a table of term ↔ expansion
  pairs (e.g. an abbreviation and its full form, or a domain synonym pair).
  When a query matches one side, query expansion adds the other side(s) as
  additional retrieval/search variants — bridging vocabulary the embedding
  model alone might not know is equivalent. Empty by default.
- **Revision-aware re-ingestion**: `/api/add-wiki`, `/api/add-url`, and
  `/api/add-text` now accept an optional `metadata` object (same shape as
  the folder-import path), including `update_mode` (`skip` (default) /
  `upsert` / `replace`), so a previously imported page/URL/text can be
  refreshed by content hash instead of being silently skipped forever.

### R³ Governance

tinyRAG includes a governed retrieval layer (R³: **Ranked, Responsible, Retrieval**):

- Retrieval units with source/ACL/sensitivity/provenance metadata
- Weighted deterministic ranking (`R3Score`) beyond pure semantic similarity
- ACL and role filtering before context assembly
- Citation-first context and answer constraints
- Policy-driven tool persistence (`transient_only`, `persistable_after_policy`, `never_persist`)
- Import job and audit-event telemetry tables for traceability

See:

- [`docs/r3-architecture.md`](docs/r3-architecture.md)
- [`docs/import-adapters.md`](docs/import-adapters.md)
- [`docs/request-lifecycle.md`](docs/request-lifecycle.md)
- [`docs/xml-tool-protocol.md`](docs/xml-tool-protocol.md)
- [`docs/agentic-tool-use.md`](docs/agentic-tool-use.md)

### LLM Integration

- Protocol-aware inference client (`auto`, OpenAI-compatible `/v1`, or native Ollama `/api`)
- OpenAI-compatible inference servers, including GopherLLM, RustyLLM, llama.cpp, LM Studio, vLLM and gateways
- Native Ollama model discovery, chat streaming, batched embeddings and legacy embedding fallback
- Streaming responses
- Support for custom system prompts (personas)
- Context injection from retrieved chunks

## Structured Processing API

For machine-to-machine jobs you can use `POST /api/process`.
This endpoint is intended for workflows where a Python or PHP tool:

- reads rows from MSSQL
- sends each row or a grouped payload as JSON
- passes `system_prompt`, `pre_prompt`, and `post_prompt`
- requests a strict JSON response with a provided schema
- validates the JSON on the server
- writes the result to JSONL

Example request:

```json
{
  "request_id": "row-4711",
  "mode": "direct",
  "system_prompt": "Du bist ein Extraktionssystem.",
  "pre_prompt": "Analysiere die Eingabedaten.",
  "input": {
    "id": 4711,
    "company": "Example GmbH",
    "text": "..."
  },
  "post_prompt": "Gib nur JSON zurueck.",
  "response_schema": {
    "type": "object",
    "required": ["status", "summary"],
    "additionalProperties": false,
    "properties": {
      "status": {"type": "string"},
      "summary": {"type": "string"},
      "score": {"type": "number"}
    }
  },
  "options": {
    "validate_json": true,
    "repair_json": true,
    "max_retries": 2
  }
}
```

Example response:

```json
{
  "request_id": "row-4711",
  "ok": true,
  "mode": "direct",
  "valid_json": true,
  "attempts": 1,
  "duration_ms": 812,
  "raw": "{\"status\":\"ok\",\"summary\":\"...\",\"score\":0.94}",
  "result": {
    "status": "ok",
    "summary": "...",
    "score": 0.94
  }
}
```

If you want retrieval from the local knowledge base before processing, set `mode` to `"rag"` or `rag.enabled` to `true`.

Included examples:

- `examples/mssql_to_jsonl.py`
- `examples/mssql_to_jsonl.php`
- `examples/jsonl_viewer.php`

## Security Considerations

### Code Execution

By default, code execution features are **disabled** for security:
- `allow_code_exec`: Allows running user-provided code
- `allow_nanogo`: Enables nanoGo interpreter

⚠️ **Only enable these features in trusted environments!**

### API Access

- No built-in authentication
- Recommended to run behind a reverse proxy with auth
- Consider network isolation for production use

## Dependencies

- [github.com/SimonWaldherr/tinySQL](https://github.com/SimonWaldherr/tinySQL) - Embedded SQL database
- [simonwaldherr.de/go/nanogo](https://simonwaldherr.de/go/nanogo) - Go interpreter
- [simonwaldherr.de/go/smallr](https://simonwaldherr.de/go/smallr) - Small templating engine

## License

See the repository for license information.

## Contributing

Contributions are welcome! Please feel free to submit issues and pull requests.

## Author

Simon Waldherr - [GitHub](https://github.com/SimonWaldherr)

## Related Projects

- [tinySQL](https://github.com/SimonWaldherr/tinySQL) - Lightweight embedded SQL database for Go
