package main

import (
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// vectorStore: the seam between R3 and whatever actually stores/searches
// chunk vectors.
//
// ragSystem (store.go) and rankedSearch (rank.go) talk to this interface,
// never to tinySQL or SQLite directly. Two implementations exist —
// tinySQLStore (vectorstore_tinysql.go, the default) and sqliteStore
// (vectorstore_sqlite.go, modernc.org/sqlite, no CGO) — selected via
// storageSettings.Backend / the -storage-backend flag (see main.go,
// Makefile's STORAGE_BACKEND). Adding a third backend later is the same
// shape: a new file implementing this interface plus one more case in
// newVectorStore below, not touching store.go/rank.go/handlers.go/
// ingest.go/pst.go/draft.go. See docs/VECTOR_DB.md for why either backend
// is a reasonable choice at R3's current scale.
// ─────────────────────────────────────────────────────────────────────────────

type vectorStore interface {
	// init creates whatever schema/tables the backend needs.
	init() error

	// lastContentHash returns the content_hash recorded for sourceID's most
	// recent load, or "" if the source has never been ingested.
	lastContentHash(sourceID string) string

	// deleteSource removes every chunk previously stored for sourceID.
	deleteSource(sourceID string) error

	// insertChunks stores sc.Chunks (already embedded into vectors, one per
	// chunk, same order) under a fresh load, tagged with embedModel/loadID/
	// loadedAt/hash. Returns the number of chunks actually inserted.
	insertChunks(sc sourceChunks, embedModel string, vectors [][]float64, loadID string, loadedAt int64, hash string) (int, error)

	// vectorCandidates returns the top `limit` chunks by similarity to
	// queryVec among rows matching embedModel, with provenance attached.
	vectorCandidates(queryVec []float64, embedModel string, limit int) ([]rankedHit, error)

	// fetchSourceChunks returns every stored chunk of sourceID, ordered by
	// chunk_idx ascending, in a single query — used to pull in the rest of
	// a cited source's content around a hit (rank.go's assembleContext) and
	// to reassemble a source's full text (store.go's fetchSourceContent).
	fetchSourceChunks(sourceID string) ([]sourceChunk, error)

	// listSources returns one row per distinct source with its latest load
	// metadata and chunk count, most recently loaded first.
	listSources() ([]sourceInfo, error)

	// docCount returns the total number of stored chunks.
	docCount() int

	// listChunks returns raw chunk rows matching filter's structured
	// (SQL-cheap, exact-match) fields, up to an internal safety cap — free-
	// text search, substring filters, sorting and pagination for the chunk-
	// viewer UI all happen in Go over this result set (see chunks.go), so
	// this only needs equality filters and a hard row cap. capped is true
	// if the cap was hit, meaning more rows exist than were returned.
	listChunks(filter chunkFilter) (rows []chunkRow, capped bool, err error)

	// save persists any pending writes. Cheap/incremental for disk-backed
	// modes, a full snapshot for memory-resident ones — see
	// vectorstore_tinysql.go and docs/VECTOR_DB.md for why that distinction
	// matters for import performance.
	save() error

	// exportAll and importRaw exist only for one-shot backend-to-backend
	// migration (see migrate.go, `-migrate-from-backend`) — not used on
	// R3's normal request path. exportAll returns every stored chunk with
	// its raw embedding vector; importRaw inserts rows verbatim (original
	// provenance and embeddings preserved byte-for-byte) so migrating
	// doesn't need a working embedding endpoint or re-pay embedding cost.
	exportAll() ([]exportedChunk, error)
	importRaw(rows []exportedChunk) error
}

// exportedChunk is one fully self-contained chunk row used only for
// migration — unlike chunkRow (the UI-facing shape in chunks.go), it
// carries the raw embedding vector and content_hash needed to recreate
// the row exactly in a different backend.
type exportedChunk struct {
	SourceID    string
	SourceKind  string
	SourceName  string
	LoadID      string
	LoadedAt    int64
	ContentHash string
	DocDate     int64
	ChunkIdx    int
	Content     string
	Embedding   []float64
	EmbedModel  string
}

// sourceChunk is one (chunk_idx, content, source_kind) row as returned by
// fetchSourceChunks, ordered by chunk_idx ascending. SourceKind is the same
// on every row of one sourceID; callers that need it (e.g. enforcing
// source_access before disclosing content) just read chunks[0].SourceKind.
type sourceChunk struct {
	ChunkIdx   int
	Content    string
	SourceKind string
}

// chunkRow is one raw stored chunk with full metadata — the unit the
// chunk-viewer UI (chunks.go) inspects to show exactly what's searchable,
// where it came from and why it would rank the way it does.
type chunkRow struct {
	ID         int
	SourceID   string
	SourceKind string
	SourceName string
	LoadID     string
	LoadedAt   int64
	DocDate    int64
	ChunkIdx   int
	Content    string
	EmbedModel string
}

// chunkFilter narrows listChunks to a structured subset before any
// free-text search/substring filtering/sorting/pagination (all handled in
// Go, see chunks.go). Deliberately exact-match only, not substring — see
// vectorstore_tinysql.go's listChunks for why (tinySQL's LIKE support is
// unconfirmed; substring filters are applied in Go instead).
type chunkFilter struct {
	SourceKind string // exact match, "" = any
	EmbedModel string // exact match, "" = any
}

// storageSettings configures whichever vectorStore backend is selected.
// Settable from the CLI (-storage-backend/-storage-mode/-storage-path/
// -storage-max-mem-mb, first run only — main.go), and, since the Speicher
// section was added to the settings UI, editable there too and persisted to
// settings.json. Either way it is a restart-only setting, never a live one:
// main.go opens exactly one vectorStore at startup (newVectorStore) and
// ragSystem never swaps it out afterward (unlike embedLM/chatLMs, which
// setLLM does swap live) — so a value saved through handleSettings only
// takes effect the *next* time the process starts, same as the CLI flags. On
// every startup, loadOrCreateSettings applies these CLI-flag-built defaults
// first and then overlays whatever settings.json already has, so once a
// settings.json exists (which it does after the very first run) its Storage
// block — CLI-flag-seeded or since edited via the UI — is what actually wins,
// not the flags. See handleSettings' validateStorageSettings call for the
// accepted enum values and handlers_storage.go for the live-status endpoint
// (/api/admin/storage) the UI shows alongside these editable fields so an
// admin can compare "pending for next restart" against "actually running".
type storageSettings struct {
	// Backend selects the implementation: "tinysql" (default) or "sqlite"
	// (modernc.org/sqlite, see vectorstore_sqlite.go).
	Backend string `json:"backend"`
	// Mode is backend-specific. For "tinysql": "memory" | "wal" | "disk" |
	// "index" | "hybrid" (see vectorstore_tinysql.go). Ignored by "sqlite",
	// which always uses SQLite's own WAL mode. "disk" is the default since
	// tinySQL v0.20.0 — see vectorstore_tinysql.go's file header for why:
	// "hybrid"/"index" bound memory via MaxMemoryMB below, but once the
	// (single, ever-growing) chunks table outgrows that budget tinySQL can
	// no longer keep it resident, so its HNSW/vector/FTS caches — keyed by
	// *Table pointer identity — are rebuilt on every single search. "disk"
	// keeps the table resident (pointer-stable) regardless of size, at the
	// cost of unbounded memory growth with the archive.
	Mode string `json:"mode"`
	// Path is a file path (memory/wal) or directory path (disk/index/
	// hybrid) depending on Mode, for "tinysql". For "sqlite" it's always a
	// single database file path.
	Path string `json:"path"`
	// MaxMemoryMB bounds the in-memory cache/index size for "tinysql"
	// modes that use one ("index", "hybrid"). Ignored otherwise, including
	// by "sqlite" and by "disk"/"memory"/"wal". Must exceed the on-disk
	// footprint of the chunks table or hybrid/index search performance
	// degrades badly (see Mode's doc comment and warnIfOversized) — the
	// admin storage-status box in the settings UI reports the current
	// on-disk size precisely so this can be sized correctly.
	MaxMemoryMB int64 `json:"max_memory_mb"`
	// UsersPath is where local user accounts (localusers.go) are stored,
	// using the SAME Backend as the chunk store above (tinySQL or SQLite,
	// per the admin's choice) but in its own separate file/directory — not
	// shared with Path, which is unrelated chunk/vector data. A deliberate
	// deviation from the "own always-SQLite side-store" convention used by
	// chathistory.go/userprefs.go/tokenusage.go: local accounts follow
	// Backend so an admin who already chose tinySQL (no CGO, no SQLite file)
	// doesn't end up with a SQLite file anyway just for user accounts.
	// Empty defaults to "r3-users.db" (sqlite) or "r3-users-tinysql"
	// (tinysql) — see newLocalUserStore.
	UsersPath string `json:"users_path,omitempty"`
}

// validStorageBackends/validStorageModes are the only values
// validateStorageSettings accepts. Unlike parseStorageMode (vectorstore_
// tinysql.go), which only warns and falls back to a safe default for an
// unknown Mode at runtime (because a running server must stay up), this
// rejects an unknown value outright — a config destined for settings.json
// that will only be consulted again after a restart gets exactly one chance
// to be validated now, since there's no later "live apply" step that could
// surface a typo more visibly.
var (
	validStorageBackends = map[string]bool{"": true, "tinysql": true, "sqlite": true}
	validStorageModes    = map[string]bool{"": true, "memory": true, "wal": true, "disk": true, "index": true, "hybrid": true}
)

// validateStorageSettings rejects an unknown Backend/Mode or a negative
// MaxMemoryMB before handleSettings persists it to settings.json. Backend/
// Mode/Path/MaxMemoryMB only take effect on the next server restart (see
// storageSettings' doc comment), so this validation is the only safety net
// against saving a config that breaks — or silently falls back on — the
// next startup.
func validateStorageSettings(s storageSettings) error {
	backend := strings.ToLower(strings.TrimSpace(s.Backend))
	if !validStorageBackends[backend] {
		return fmt.Errorf("unknown backend %q (valid: tinysql, sqlite)", s.Backend)
	}
	mode := strings.ToLower(strings.TrimSpace(s.Mode))
	if !validStorageModes[mode] {
		return fmt.Errorf("unknown mode %q (valid: memory, wal, disk, index, hybrid)", s.Mode)
	}
	if s.MaxMemoryMB < 0 {
		return fmt.Errorf("max_memory_mb must be >= 0, got %d", s.MaxMemoryMB)
	}
	if s.UsersPath != "" && s.UsersPath == s.Path {
		return fmt.Errorf("users_path must not be the same as path (chunk store and local user accounts need separate files)")
	}
	return nil
}

// newVectorStore constructs the configured backend — "tinysql" (default)
// or "sqlite" (modernc.org/sqlite, no CGO; see vectorstore_sqlite.go).
// Adding a third backend later is the same shape: a new file plus one
// more case here, per the package comment above.
func newVectorStore(cfg storageSettings) (vectorStore, error) {
	switch cfg.Backend {
	case "", "tinysql":
		return newTinySQLStore(cfg)
	case "sqlite":
		return newSQLiteStore(cfg)
	default:
		return nil, fmt.Errorf("unknown vector store backend %q", cfg.Backend)
	}
}
