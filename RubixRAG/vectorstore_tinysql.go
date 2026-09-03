package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// ─────────────────────────────────────────────────────────────────────────────
// tinySQL-backed vectorStore
//
// tinySQL supports several storage modes (mirroring tinyRAG's own usage):
//
//   - ModeMemory: everything in RAM, saved as one GOB snapshot on save().
//   - ModeWAL:    also saved as one GOB snapshot on save() — despite the
//                 name, tinySQL's WAL mode does NOT give incremental saves
//                 the way ModeDisk/ModeIndex/ModeHybrid do (confirmed
//                 against tinyRAG's own save() switch, which treats WAL
//                 the same as the legacy/memory full-snapshot path).
//   - ModeDisk:   each table lives as files under Path; save() calls
//                 db.Sync(), which flushes only dirty tables.
//   - ModeIndex:  schemas in RAM, rows on disk under Path, bounded by
//                 MaxMemoryBytes; save() is also an incremental Sync().
//   - ModeHybrid: an LRU cache (MaxMemoryBytes) over disk-backed storage
//                 under Path; save() is also an incremental Sync().
//
// R3 ingests one source (file/email) at a time and calls save() after each
// one. With ModeMemory/ModeWAL that means re-serializing the *entire*
// database on every single ingested email — O(n²) total import time for n
// emails. ModeDisk (the default here) avoids that: save() is an incremental
// Sync() that flushes only dirty tables, and disk-loaded tables stay resident
// in the catalog — so R3's one hot `chunks` table keeps a stable *Table
// pointer and tinySQL's HNSW/vector/FTS caches stay warm across queries,
// with no memory-budget cliff (see the v0.20.0 residency caveat below).
// ModeHybrid/ModeIndex also write incrementally but bound residency to
// MaxMemoryBytes — preferable on a RAM-constrained host, at the cost of the
// per-query cache rebuild described below once the table outgrows the budget.
// (Before v0.20.0, Hybrid was the default here; the residency change flipped
// the tradeoff, since Disk now gives the same warm-cache read latency without
// the cliff for R3's single-always-hot-table shape.)
//
// tinySQL v0.20.0 residency caveat (matters a lot for R3's single big
// `chunks` table): in ModeHybrid/ModeIndex a table larger than
// MaxMemoryBytes is no longer retained across queries. It's handed to the
// current statement as a freshly decoded *Table each time and never admitted
// to the bounded buffer pool. Because tinySQL's HNSW, vector-column and FTS
// caches are all keyed by *Table *pointer identity* (not just table name),
// that fresh pointer misses every cache — so an oversized chunks table
// rebuilds the HNSW graph, re-scans every row for vector norms, and
// re-tokenizes the whole FTS corpus on *every single search*. VEC_WARM below
// can't prevent it either (it warms a lease pointer that's released when the
// warm statement ends). v0.19.1 kept every loaded table in the catalog
// forever — a memory leak v0.20.0 deliberately fixed — which as a side effect
// kept the pointer stable and the caches warm regardless of size.
//
// Consequence: in hybrid/index mode MaxMemoryMB must be large enough to hold
// the whole chunks table, or search degrades badly on large archives. init()
// warns at startup when the on-disk footprint already exceeds the budget (see
// warnIfOversized). ModeDisk is exempt — it keeps loaded tables resident in
// the catalog, so its *Table pointer stays stable and the caches stay warm —
// which makes ModeDisk the safer choice for an archive too large to fit a RAM
// budget. (ModePagedIndex/ModeAdvancedWAL are not exported by the tinySQL
// package, so they aren't selectable here.)
// ─────────────────────────────────────────────────────────────────────────────

type tinySQLStore struct {
	db   *tinysql.DB
	mode tinysql.StorageMode
	path string

	dbMu sync.Mutex

	idMu   sync.Mutex
	nextID int

	// embedModelsMu/embedModels track which embed_model values have ever
	// been written this session (seeded from disk in init, appended to in
	// insertChunks), so vectorCandidates can tell — without an O(n) scan —
	// whether the chunks table currently holds exactly one embedding model.
	// See isSoleEmbedModel.
	embedModelsMu sync.Mutex
	embedModels   map[string]bool
}

// parseStorageMode maps the -storage-backend Mode string to a
// tinysql.StorageMode, defaulting to ModeHybrid (and warning) for any
// empty, unknown, or "hybrid" value.
func parseStorageMode(raw string) tinysql.StorageMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "memory":
		return tinysql.ModeMemory
	case "wal":
		return tinysql.ModeWAL
	case "disk":
		return tinysql.ModeDisk
	case "index":
		return tinysql.ModeIndex
	case "hybrid", "":
		return tinysql.ModeHybrid
	default:
		log.Printf("WARN: unknown storage mode %q, falling back to hybrid", raw)
		return tinysql.ModeHybrid
	}
}

// storageModeLabel is parseStorageMode's inverse, used for logging/startup
// messages rather than round-tripping config.
func storageModeLabel(mode tinysql.StorageMode) string {
	switch mode {
	case tinysql.ModeMemory:
		return "memory"
	case tinysql.ModeWAL:
		return "wal"
	case tinysql.ModeDisk:
		return "disk"
	case tinysql.ModeIndex:
		return "index"
	case tinysql.ModeHybrid:
		return "hybrid"
	default:
		return "unknown"
	}
}

// newTinySQLStore opens the configured tinySQL database, defaulting Path
// to "r3-data" and MaxMemoryMB to 256MB when unset.
func newTinySQLStore(cfg storageSettings) (*tinySQLStore, error) {
	mode := parseStorageMode(cfg.Mode)
	path := cfg.Path
	if path == "" {
		path = "r3-data"
	}
	maxMem := cfg.MaxMemoryMB * 1024 * 1024
	if maxMem <= 0 {
		maxMem = 256 * 1024 * 1024
	}

	db, err := tinysql.OpenDB(tinysql.StorageConfig{
		Mode:           mode,
		Path:           path,
		MaxMemoryBytes: maxMem,
	})
	if err != nil {
		return nil, fmt.Errorf("open tinySQL store (mode=%s, path=%s): %w", storageModeLabel(mode), path, err)
	}
	fmt.Printf("Storage mode: %s (%s)\n", storageModeLabel(mode), path)

	// VEC_SEARCH result cache (tinySQL v0.19.0+, see internal/engine/
	// vector_query_cache.go upstream): process-wide, keyed by the exact
	// query vector + table/column/metric/index/k *and* the table's own
	// version counter, so an insert (a new import) invalidates it
	// automatically — this only ever serves a cached result for a table
	// state that's still current, never a stale one, so the TTL below is
	// just a memory bound, not a correctness guard. It only helps
	// byte-identical repeat searches, but that's a real, common case here:
	// the chat "Neu generieren" button re-asks the same question, and a
	// popular FAQ-style question gets asked verbatim by more than one
	// person. Bounded (256 entries, 30s TTL) rather than defaulting to
	// tinysql.DefaultVectorCacheConfig() as-is, which leaves
	// ResultCacheEntries at 0 (disabled) until a caller opts in — this is
	// that opt-in. Analytics (a small in-memory recent-query ring) is on
	// too, for admin-visible cache-hit-rate reporting via
	// tinysql.VectorCacheAnalytics() (see storageStats() below) — cheap, and
	// not worth a second config knob just to gate it separately from the
	// cache itself. AnalyticsWindow/AnalyticsMaxEvents widen tinySQL's own
	// defaults (1 minute / 128 events, engine.defaultVectorAnalyticsWindow)
	// to 5 minutes / 200 events — the admin storage tab is polled
	// periodically, not continuously, so the default 1-minute window could
	// otherwise show an empty trace between page loads on a quiet instance.
	tinysql.ConfigureVectorCache(tinysql.VectorCacheConfig{
		ResultCacheEntries: 256,
		ResultCacheTTL:     30 * time.Second,
		Analytics:          true,
		AnalyticsWindow:    5 * time.Minute,
		AnalyticsMaxEvents: 200,
	})

	return &tinySQLStore{db: db, mode: mode, path: path, embedModels: make(map[string]bool)}, nil
}

// exec parses and runs a raw SQL string against the "default" database
// under dbMu, serializing all access since tinySQL is single-writer — every
// other method funnels through here rather than calling tinysql.Execute
// directly.
func (s *tinySQLStore) exec(q string) (*tinysql.ResultSet, error) {
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", q, err)
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	return tinysql.Execute(context.Background(), s.db, "default", stmt)
}

// init creates the chunks and loads tables if missing, then primes nextID
// from the current max chunk id so allocIDs continues from where a
// previous run left off instead of restarting at 0.
func (s *tinySQLStore) init() error {
	if _, err := s.exec(`CREATE TABLE IF NOT EXISTS chunks (
		id INT, source_id TEXT, source_kind TEXT, source_name TEXT,
		load_id TEXT, loaded_at INT, content_hash TEXT, doc_date INT,
		chunk_idx INT, content TEXT, embedding VECTOR, embed_model TEXT
	)`); err != nil {
		return err
	}
	if _, err := s.exec(`CREATE TABLE IF NOT EXISTS loads (
		load_id TEXT, source_id TEXT, source_kind TEXT, source_name TEXT,
		started_at INT, finished_at INT, chunks INT, status TEXT, err TEXT
	)`); err != nil {
		return err
	}
	s.idMu.Lock()
	s.nextID = s.maxChunkIDLocked() + 1
	s.idMu.Unlock()

	if rs, err := s.exec("SELECT DISTINCT embed_model FROM chunks"); err == nil {
		s.embedModelsMu.Lock()
		for _, row := range rs.Rows {
			if v, ok := tinysql.GetVal(row, "embed_model"); ok {
				s.embedModels[toStr(v)] = true
			}
		}
		s.embedModelsMu.Unlock()
	}

	// Warm the vector search structures once here, at startup (the flat scan's
	// column-norm cache, or the HNSW graph for a large table — vecSearchIndex
	// picks which), rather than paying tinySQL's lazy first-query build cost on
	// whichever user happens to ask the first question after a restart (see
	// vectorstore_tinysql.go's vecJSON/vectorCandidatesIndexed doc comments and
	// tinySQL's rag-guide.md#6).
	// Only worth it when isSoleEmbedModel would actually pick the indexed
	// path (0 or 1 distinct embed_model) — with 2+, vectorCandidates always
	// falls back to the brute-force scan, so a warmed index would just sit
	// unused. This only covers the cold-start-after-restart case: writes
	// during normal operation invalidate the index again, and re-warming
	// after every single ingested source would be far more expensive than
	// the one lazy rebuild it's meant to avoid, so this intentionally does
	// NOT re-warm on every save() — only once, here.
	//
	// v0.20.0 caveat: warming only sticks while the chunks table stays
	// pointer-stable (fits MaxMemoryBytes, or ModeDisk keeps it resident). If
	// the table is oversized in hybrid/index mode it's re-decoded per query
	// and this warmed index is discarded with the lease — see the file header
	// and warnIfOversized. Warming stays correct (harmless when it doesn't
	// stick), it's just no substitute for a large-enough budget.
	s.embedModelsMu.Lock()
	soleModel := len(s.embedModels) <= 1
	s.embedModelsMu.Unlock()
	if soleModel {
		warmIdx := s.vecSearchIndex()
		if _, err := s.exec(fmt.Sprintf(`SELECT * FROM VEC_WARM('chunks', 'embedding', 'cosine', '%s')`, warmIdx)); err != nil {
			log.Printf("WARN: VEC_WARM(%s) failed, first vector search will pay the index-build cost instead: %v", warmIdx, err)
		}
	}
	s.warnIfOversized()
	return nil
}

// warnIfOversized logs a startup warning when R3 is running in a pool-bounded
// mode (hybrid/index) whose on-disk footprint already meets or exceeds the
// in-memory budget. Past that point tinySQL v0.20.0 can't keep the chunks
// table resident, and its HNSW/vector/FTS caches — keyed by *Table pointer
// identity — are rebuilt on every search (see this file's header). The remedy
// is operational, not code: raise -storage-max-mem-mb past the reported
// footprint, or switch to -storage-mode=disk (which keeps loaded tables
// resident). Modes without a pool bound (disk/memory/wal) can't hit this and
// are skipped. The check is deliberately conservative: it compares the
// (possibly compressed) on-disk size against the budget, and the decoded
// in-memory size is always larger, so it under-warns rather than false-alarms.
// BackendStats().LoadCount climbing across queries at runtime is the
// definitive thrash signal.
func (s *tinySQLStore) warnIfOversized() {
	if s.mode != tinysql.ModeHybrid && s.mode != tinysql.ModeIndex {
		return
	}
	st := s.db.BackendStats()
	if st.MemoryLimitBytes <= 0 || st.DiskUsedBytes < st.MemoryLimitBytes {
		return
	}
	const mib = 1024 * 1024
	log.Printf("WARN: tinySQL %s store on disk (%d MiB) meets/exceeds the in-memory budget (%d MiB); "+
		"in v0.20.0 an oversized table is re-decoded and its HNSW/FTS caches rebuilt on every search. "+
		"Raise -storage-max-mem-mb to >= %d MiB, or use -storage-mode=disk (keeps tables resident).",
		storageModeLabel(s.mode), st.DiskUsedBytes/mib, st.MemoryLimitBytes/mib, st.DiskUsedBytes/mib+1)
}

// storageStats implements the optional storageStatser capability
// (handlers_storage.go) so the admin storage endpoint can surface tinySQL's
// runtime health — most importantly whether the chunks table is thrashing
// (Oversized, plus a climbing LoadCount / falling CacheHitRate), the very
// condition warnIfOversized only estimates once at startup. Read-only and
// cheap: one BackendStats() snapshot, one COUNT(*), and the process-wide
// VEC_SEARCH result-cache analytics (enabled in newTinySQLStore).
func (s *tinySQLStore) storageStats() storageStats {
	const mib = 1024 * 1024
	st := s.db.BackendStats()
	vc := tinysql.VectorCacheAnalytics()
	pooled := s.mode == tinysql.ModeHybrid || s.mode == tinysql.ModeIndex
	return storageStats{
		Backend:        "tinysql",
		Mode:           storageModeLabel(s.mode),
		Path:           s.path,
		Chunks:         s.docCount(),
		TablesInMemory: st.TablesInMemory,
		TablesOnDisk:   st.TablesOnDisk,
		MemoryUsedMB:   st.MemoryUsedBytes / mib,
		MemoryLimitMB:  st.MemoryLimitBytes / mib,
		DiskUsedMB:     st.DiskUsedBytes / mib,
		CacheHitRate:   st.CacheHitRate,
		LoadCount:      st.LoadCount,
		EvictionCount:  st.EvictionCount,
		SyncCount:      st.SyncCount,
		Oversized:      pooled && st.MemoryLimitBytes > 0 && st.DiskUsedBytes >= st.MemoryLimitBytes,
		VectorResultCache: vectorCacheStats{
			Enabled:       vc.Enabled,
			Entries:       vc.Entries,
			Hits:          vc.Hits,
			Misses:        vc.Misses,
			Evictions:     vc.Evictions,
			ApproxBytes:   vc.ApproxBytes,
			HeapAllocMB:   int64(vc.HeapAlloc) / mib,
			RecentQueries: toVectorQueryEvents(vc.RecentQueries),
		},
	}
}

// toVectorQueryEvents converts tinySQL's own VectorQueryEvent trace (newest
// last) into the admin-facing mirror type, newest first — most recent
// queries are what an operator glancing at the storage tab cares about.
func toVectorQueryEvents(events []tinysql.VectorQueryEvent) []vectorQueryEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]vectorQueryEvent, len(events))
	for i, e := range events {
		out[len(events)-1-i] = vectorQueryEvent{
			At:         e.At.Unix(),
			Table:      e.Table,
			Column:     e.Column,
			Metric:     e.Metric,
			Index:      e.Index,
			K:          e.K,
			CacheHit:   e.CacheHit,
			DurationMS: e.Duration.Milliseconds(),
		}
	}
	return out
}

// noteEmbedModel records that embedModel has been written to the chunks
// table, so isSoleEmbedModel can later tell whether the table might mix
// more than one embedding model (e.g. after switching the active model).
func (s *tinySQLStore) noteEmbedModel(embedModel string) {
	s.embedModelsMu.Lock()
	s.embedModels[embedModel] = true
	s.embedModelsMu.Unlock()
}

// isSoleEmbedModel reports whether embedModel is the only embedding model
// ever seen in the chunks table this session (or the table is empty/new).
// A false positive (claiming "sole" when a stale model actually still has
// rows, e.g. after deleteSource) only costs a wasted VEC_SEARCH attempt —
// vectorCandidates always re-filters by embed_model before returning, so
// correctness never depends on this being exact.
func (s *tinySQLStore) isSoleEmbedModel(embedModel string) bool {
	s.embedModelsMu.Lock()
	defer s.embedModelsMu.Unlock()
	switch len(s.embedModels) {
	case 0:
		return true
	case 1:
		return s.embedModels[embedModel]
	default:
		return false
	}
}

// maxChunkIDLocked returns the highest existing chunk id, or -1 if the
// table is empty or the query fails. Caller must hold idMu.
func (s *tinySQLStore) maxChunkIDLocked() int {
	rs, err := s.exec("SELECT MAX(id) AS mid FROM chunks")
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return -1
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "mid")
	if !ok || v == nil {
		return -1
	}
	return toInt(v)
}

// allocIDs reserves a contiguous block of n chunk ids and returns the first
// one, letting callers assign sequential ids across a batch insert without
// re-querying MAX(id) (and racing another insert) for every row.
func (s *tinySQLStore) allocIDs(n int) int {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	start := s.nextID
	s.nextID += n
	return start
}

// toInt coerces a tinysql.GetVal result (int, int64, or float64) to int,
// returning 0 for any other type including nil.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// toFloat coerces a tinysql.GetVal result (int, int64, or float64) to
// float64, returning 0 for any other type including nil.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

// toStr coerces a tinysql.GetVal result to string, returning "" if v isn't
// actually a string (including nil).
func toStr(v any) string {
	s, _ := v.(string)
	return s
}

// sqlStr returns a complete, safe-to-embed SQL string-literal expression
// for s, e.g. sqlStr("café") -> "'café'", sqlStr("it's") -> "'it”s'".
//
// tinySQL <= v0.6.0's lexer tokenized its input byte-by-byte instead of
// rune-by-rune, corrupting every multi-byte UTF-8 character inside a plain
// '...' string literal. Fixed in v0.16.0 (internal/engine/lexer.go now
// decodes via utf8.DecodeRuneInString), so plain SQL string literals with
// standard single-quote doubling are safe again.
func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// vecJSON encodes v as a JSON array for embedding into a VEC_FROM_JSON(...)
// call in a raw SQL string, falling back to "[]" (rather than propagating
// an error) on the practically-impossible case that []float64 fails to
// marshal.
func vecJSON(v []float64) string {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("WARN: failed to marshal vector: %v", err)
		return "[]"
	}
	return string(b)
}

// lastContentHash looks up the content_hash of any one stored chunk for
// sourceID (all chunks from the same load share the same hash), returning
// "" if the source has no chunks or the query fails. sourceID is escaped
// via sqlStr since tinySQL only takes raw SQL text.
func (s *tinySQLStore) lastContentHash(sourceID string) string {
	q := fmt.Sprintf("SELECT content_hash FROM chunks WHERE source_id = %s LIMIT 1", sqlStr(sourceID))
	rs, err := s.exec(q)
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return ""
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "content_hash")
	if !ok {
		return ""
	}
	return toStr(v)
}

// deleteSource removes every chunk row for sourceID, e.g. before
// re-ingesting it under a new load. sourceID is escaped via sqlStr since
// the DELETE is built as a raw SQL string.
func (s *tinySQLStore) deleteSource(sourceID string) error {
	_, err := s.exec(fmt.Sprintf("DELETE FROM chunks WHERE source_id = %s", sqlStr(sourceID)))
	return err
}

// insertChunks writes sc.Chunks and their vectors in batches of 16 rows per
// INSERT, each built as a raw SQL string with every value escaped via
// sqlStr/vecJSON (tinySQL has no parameterized-query support) and ids
// pre-reserved via allocIDs, then records one loads-table row summarizing
// the load. A batch failure returns the count inserted so far rather than
// rolling back — callers should treat a non-nil error as a partial load.
// Re-verified against tinySQL v0.21.0's "statement atomicity" claim (see
// docs/DEPENDENCIES.md): a malformed row still lets the rows before it
// land despite the statement erroring, so this partial-load behavior is
// unchanged, not stale documentation.
func (s *tinySQLStore) insertChunks(sc sourceChunks, embedModel string, vectors [][]float64, loadID string, loadedAt int64, hash string) (int, error) {
	if len(sc.Chunks) != len(vectors) {
		return 0, fmt.Errorf("chunk/vector count mismatch: %d chunks, %d vectors", len(sc.Chunks), len(vectors))
	}
	if len(sc.Chunks) == 0 {
		return 0, nil
	}

	batchSize := 16
	inserted := 0
	for i := 0; i < len(sc.Chunks); i += batchSize {
		end := i + batchSize
		if end > len(sc.Chunks) {
			end = len(sc.Chunks)
		}
		startID := s.allocIDs(end - i)

		vals := make([]string, 0, end-i)
		for j := i; j < end; j++ {
			tup := fmt.Sprintf(
				"(%d, %s, %s, %s, %s, %d, %s, %d, %d, %s, VEC_FROM_JSON('%s'), %s)",
				startID+(j-i), sqlStr(sc.SourceID), sqlStr(sc.SourceKind), sqlStr(sc.SourceName),
				sqlStr(loadID), loadedAt, sqlStr(hash), sc.DocDate,
				j, sqlStr(sc.Chunks[j]), vecJSON(vectors[j]), sqlStr(embedModel),
			)
			vals = append(vals, tup)
		}
		q := "INSERT INTO chunks VALUES " + strings.Join(vals, ",")
		if _, err := s.exec(q); err != nil {
			return inserted, fmt.Errorf("insert batch: %w", err)
		}
		inserted += end - i
	}
	if inserted > 0 {
		s.noteEmbedModel(embedModel)
	}

	_, _ = s.exec(fmt.Sprintf(
		"INSERT INTO loads VALUES (%s, %s, %s, %s, %d, %d, %d, '%s', '')",
		sqlStr(loadID), sqlStr(sc.SourceID), sqlStr(sc.SourceKind), sqlStr(sc.SourceName),
		loadedAt, loadedAt, inserted, "ok",
	))
	return inserted, nil
}

// vecFlatRowCap is the chunk-count threshold below which vectorCandidatesIndexed
// asks VEC_SEARCH for an exact `flat` scan and above which it switches to the
// approximate `hnsw` graph. tinySQL's rag-guide.md#2 puts a flat scan at "low
// single-digit ms up to ~100k rows" (SIMD + cached norms, no build cost) and
// reserves hnsw for "static data, many queries" because of its high build
// cost. R3's chunks table is neither: it's written continuously (every ingest
// invalidates any ANN index) and, once oversized in a pool-bounded mode,
// tinySQL rebuilds the index per query (see this file's header) — both of
// which make the exact, build-free flat scan the better default. hnsw's build
// cost only amortizes past ~100k rows, where the disk default keeps that graph
// resident and warm across queries.
const vecFlatRowCap = 100_000

// vecSearchIndex picks the VEC_SEARCH index mode by current table size (see
// vecFlatRowCap). docCount() is O(1) on a resident table, so this adds
// negligible cost next to the embedding/LLM work each query already does.
func (s *tinySQLStore) vecSearchIndex() string {
	if s.docCount() > vecFlatRowCap {
		return "hnsw"
	}
	return "flat"
}

// vectorCandidates picks the fastest search it can trust for the current
// table contents: VEC_SEARCH (an exact flat scan, or the HNSW graph for very
// large tables — see vecSearchIndex) when embedModel looks like the only
// embedding model ever stored (see isSoleEmbedModel), falling back to the
// brute-force VEC_COSINE_SIMILARITY scan otherwise — including on any error
// from the indexed path, so a stale/incompatible index never turns into a
// failed search.
func (s *tinySQLStore) vectorCandidates(queryVec []float64, embedModel string, limit int) ([]rankedHit, error) {
	if s.isSoleEmbedModel(embedModel) {
		hits, err := s.vectorCandidatesIndexed(queryVec, embedModel, limit)
		if err == nil {
			return hits, nil
		}
		log.Printf("WARN: VEC_SEARCH failed, falling back to full scan: %v", err)
	}
	return s.vectorCandidatesScan(queryVec, embedModel, limit)
}

// vectorCandidatesIndexed searches via tinySQL's VEC_SEARCH table function,
// letting vecSearchIndex choose an exact `flat` scan (the default — SIMD +
// cached norms, no build cost, exact recall) or the approximate `hnsw` graph
// once the table is large enough for its build cost to amortize. Either way
// VEC_SEARCH beats `ORDER BY VEC_COSINE_SIMILARITY LIMIT k` (~7x per the
// rag-guide) via a top-k heap and a parallel scan. Because VEC_SEARCH has no
// WHERE-pushdown, a row's embed_model is still checked in Go before it's
// accepted: correct results never depend on isSoleEmbedModel's caller-side
// check being exact, only on this per-row filter.
func (s *tinySQLStore) vectorCandidatesIndexed(queryVec []float64, embedModel string, limit int) ([]rankedHit, error) {
	q := fmt.Sprintf(
		`SELECT source_id, source_kind, source_name, load_id, loaded_at, doc_date, chunk_idx, content, embed_model, _vec_distance
		 FROM VEC_SEARCH('chunks', 'embedding', VEC_FROM_JSON('%s'), %d, 'cosine', '%s')`,
		vecJSON(queryVec), limit, s.vecSearchIndex(),
	)
	rs, err := s.exec(q)
	if err != nil {
		return nil, err
	}
	hits := make([]rankedHit, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		em, _ := tinysql.GetVal(row, "embed_model")
		if toStr(em) != embedModel {
			continue
		}
		content, ok := tinysql.GetVal(row, "content")
		if !ok || content == nil {
			continue
		}
		sid, _ := tinysql.GetVal(row, "source_id")
		skind, _ := tinysql.GetVal(row, "source_kind")
		sname, _ := tinysql.GetVal(row, "source_name")
		lid, _ := tinysql.GetVal(row, "load_id")
		loadedAt, _ := tinysql.GetVal(row, "loaded_at")
		docDate, _ := tinysql.GetVal(row, "doc_date")
		idx, _ := tinysql.GetVal(row, "chunk_idx")
		dist, _ := tinysql.GetVal(row, "_vec_distance")
		hits = append(hits, rankedHit{
			SourceID:    toStr(sid),
			SourceKind:  toStr(skind),
			SourceName:  toStr(sname),
			LoadID:      toStr(lid),
			LoadedAt:    int64(toInt(loadedAt)),
			DocDate:     int64(toInt(docDate)),
			ChunkIdx:    toInt(idx),
			Content:     toStr(content),
			VectorScore: 1 - toFloat(dist), // VEC_SEARCH's cosine metric returns 1-similarity
		})
	}
	return hits, nil
}

// vectorCandidatesScan is the exact brute-force fallback: score every row
// matching embedModel via VEC_COSINE_SIMILARITY, ordering and limiting
// server-side rather than pulling every row into Go. Used whenever the
// chunks table might hold more than one embedding model, since VEC_SEARCH
// has no way to pre-filter by embed_model before ranking.
func (s *tinySQLStore) vectorCandidatesScan(queryVec []float64, embedModel string, limit int) ([]rankedHit, error) {
	q := fmt.Sprintf(
		`SELECT source_id, source_kind, source_name, load_id, loaded_at, doc_date, chunk_idx, content,
		        VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON('%s')) AS score
		 FROM chunks WHERE embed_model = %s
		 ORDER BY score DESC LIMIT %d`,
		vecJSON(queryVec), sqlStr(embedModel), limit,
	)
	rs, err := s.exec(q)
	if err != nil {
		return nil, err
	}
	hits := make([]rankedHit, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		content, ok := tinysql.GetVal(row, "content")
		if !ok || content == nil {
			continue
		}
		sid, _ := tinysql.GetVal(row, "source_id")
		skind, _ := tinysql.GetVal(row, "source_kind")
		sname, _ := tinysql.GetVal(row, "source_name")
		lid, _ := tinysql.GetVal(row, "load_id")
		loadedAt, _ := tinysql.GetVal(row, "loaded_at")
		docDate, _ := tinysql.GetVal(row, "doc_date")
		idx, _ := tinysql.GetVal(row, "chunk_idx")
		score, _ := tinysql.GetVal(row, "score")
		hits = append(hits, rankedHit{
			SourceID:    toStr(sid),
			SourceKind:  toStr(skind),
			SourceName:  toStr(sname),
			LoadID:      toStr(lid),
			LoadedAt:    int64(toInt(loadedAt)),
			DocDate:     int64(toInt(docDate)),
			ChunkIdx:    toInt(idx),
			Content:     toStr(content),
			VectorScore: toFloat(score),
		})
	}
	return hits, nil
}

// keywordCandidatesFTS implements the optional ftsKeywordScorer capability
// (rank.go) using tinySQL's FTS_SEARCH table function (upstream since
// v0.19.0, see FUNCTIONS.sql) — a real BM25 rank (term frequency + document
// length normalization, stemming, stop-word filtering) against the
// 'content' column specifically, rather than scanning every TEXT column
// (source_id/source_kind/source_name would otherwise pollute matches).
// FTS_SEARCH is index-free (scores on the fly, cached per-row tokenization
// internally) and has no WHERE-pushdown, same limitation as VEC_SEARCH —
// so embed_model is filtered in Go here too, same pattern as
// vectorCandidatesIndexed. Keyed by source_id+chunk_idx (a source_id alone
// isn't unique — one source has many chunks) so rankedSearch can look up
// any candidate hit's score without a second round trip per row.
func (s *tinySQLStore) keywordCandidatesFTS(query, embedModel string, limit int) (map[string]float64, error) {
	q := fmt.Sprintf(
		`SELECT source_id, chunk_idx, embed_model, _fts_score
		 FROM FTS_SEARCH('chunks', %s, %d, 'content')`,
		sqlStr(query), limit,
	)
	rs, err := s.exec(q)
	if err != nil {
		return nil, err
	}
	scores := make(map[string]float64, len(rs.Rows))
	for _, row := range rs.Rows {
		em, _ := tinysql.GetVal(row, "embed_model")
		if toStr(em) != embedModel {
			continue
		}
		sid, _ := tinysql.GetVal(row, "source_id")
		idx, _ := tinysql.GetVal(row, "chunk_idx")
		score, _ := tinysql.GetVal(row, "_fts_score")
		scores[ftsCandidateKey(toStr(sid), toInt(idx))] = toFloat(score)
	}
	return scores, nil
}

// chunkByKey implements rank.go's optional chunkKeyFetcher capability: load
// one specific chunk (with its embedding, read back via VEC_TO_JSON exactly
// like exportAll does) by its (source_id, chunk_idx, embed_model) key, so
// rankedSearch can pull FTS-found candidates the vector search missed into
// the scored set. ok=false when the row doesn't exist or its embedding is
// unreadable.
func (s *tinySQLStore) chunkByKey(sourceID string, chunkIdx int, embedModel string) (rankedHit, []float64, bool) {
	q := fmt.Sprintf(
		`SELECT source_id, source_kind, source_name, load_id, loaded_at, doc_date, chunk_idx, content, VEC_TO_JSON(embedding) AS embedding_json
		 FROM chunks WHERE source_id = %s AND chunk_idx = %d AND embed_model = %s LIMIT 1`,
		sqlStr(sourceID), chunkIdx, sqlStr(embedModel),
	)
	rs, err := s.exec(q)
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return rankedHit{}, nil, false
	}
	row := rs.Rows[0]
	embJSON, _ := tinysql.GetVal(row, "embedding_json")
	var vec []float64
	if err := json.Unmarshal([]byte(toStr(embJSON)), &vec); err != nil || len(vec) == 0 {
		return rankedHit{}, nil, false
	}
	sid, _ := tinysql.GetVal(row, "source_id")
	skind, _ := tinysql.GetVal(row, "source_kind")
	sname, _ := tinysql.GetVal(row, "source_name")
	lid, _ := tinysql.GetVal(row, "load_id")
	loadedAt, _ := tinysql.GetVal(row, "loaded_at")
	docDate, _ := tinysql.GetVal(row, "doc_date")
	idx, _ := tinysql.GetVal(row, "chunk_idx")
	content, _ := tinysql.GetVal(row, "content")
	return rankedHit{
		SourceID:   toStr(sid),
		SourceKind: toStr(skind),
		SourceName: toStr(sname),
		LoadID:     toStr(lid),
		LoadedAt:   int64(toInt(loadedAt)),
		DocDate:    int64(toInt(docDate)),
		ChunkIdx:   toInt(idx),
		Content:    toStr(content),
	}, vec, true
}

// fetchSourceEmbeddings implements store.go's optional
// sourceEmbeddingFetcher capability: every stored chunk embedding of one
// source, keyed by the chunk content's hash — so replaceSourceChunks can
// reuse embeddings for chunks whose text didn't change instead of re-paying
// the embedding call for the whole source. Rows with unreadable embeddings
// are skipped (they'll simply be re-embedded).
func (s *tinySQLStore) fetchSourceEmbeddings(sourceID, embedModel string) (map[string][]float64, error) {
	q := fmt.Sprintf(
		`SELECT content, VEC_TO_JSON(embedding) AS embedding_json FROM chunks WHERE source_id = %s AND embed_model = %s`,
		sqlStr(sourceID), sqlStr(embedModel),
	)
	rs, err := s.exec(q)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]float64, len(rs.Rows))
	for _, row := range rs.Rows {
		content, _ := tinysql.GetVal(row, "content")
		embJSON, _ := tinysql.GetVal(row, "embedding_json")
		var vec []float64
		if err := json.Unmarshal([]byte(toStr(embJSON)), &vec); err != nil || len(vec) == 0 {
			continue
		}
		out[contentHash(toStr(content))] = vec
	}
	return out, nil
}

// fetchSourceChunks returns every stored chunk of sourceID, ordered by
// chunk_idx, in one query — rather than one round trip per index.
func (s *tinySQLStore) fetchSourceChunks(sourceID string) ([]sourceChunk, error) {
	q := fmt.Sprintf("SELECT chunk_idx, content, source_kind FROM chunks WHERE source_id = %s ORDER BY chunk_idx", sqlStr(sourceID))
	rs, err := s.exec(q)
	if err != nil {
		return nil, err
	}
	out := make([]sourceChunk, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		idx, _ := tinysql.GetVal(row, "chunk_idx")
		content, _ := tinysql.GetVal(row, "content")
		kind, _ := tinysql.GetVal(row, "source_kind")
		out = append(out, sourceChunk{ChunkIdx: toInt(idx), Content: toStr(content), SourceKind: toStr(kind)})
	}
	return out, nil
}

// listSources groups chunks by their source/load identity to recover one
// summary row per distinct load, newest first.
func (s *tinySQLStore) listSources() ([]sourceInfo, error) {
	q := `SELECT source_id, source_kind, source_name, load_id, loaded_at, doc_date, COUNT(*) AS cnt
	      FROM chunks GROUP BY source_id, source_kind, source_name, load_id, loaded_at, doc_date
	      ORDER BY loaded_at DESC`
	rs, err := s.exec(q)
	if err != nil {
		return nil, err
	}
	out := make([]sourceInfo, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		sid, _ := tinysql.GetVal(row, "source_id")
		skind, _ := tinysql.GetVal(row, "source_kind")
		sname, _ := tinysql.GetVal(row, "source_name")
		lid, _ := tinysql.GetVal(row, "load_id")
		loadedAt, _ := tinysql.GetVal(row, "loaded_at")
		docDate, _ := tinysql.GetVal(row, "doc_date")
		cnt, _ := tinysql.GetVal(row, "cnt")
		out = append(out, sourceInfo{
			SourceID:   toStr(sid),
			SourceKind: toStr(skind),
			SourceName: toStr(sname),
			LoadID:     toStr(lid),
			LoadedAt:   int64(toInt(loadedAt)),
			DocDate:    int64(toInt(docDate)),
			Chunks:     toInt(cnt),
		})
	}
	return out, nil
}

// docCount returns the total chunk row count, or 0 if the query fails.
func (s *tinySQLStore) docCount() int {
	rs, err := s.exec("SELECT COUNT(*) AS cnt FROM chunks")
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return 0
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "cnt")
	if !ok {
		return 0
	}
	return toInt(v)
}

// chunkListCap bounds how many rows listChunks ever pulls into Go, as a
// safety valve for an unfiltered (or barely-filtered) chunk-viewer query
// against a large mailbox. The chunk viewer is a debug/browse tool, not a
// paginated production listing, so a generous-but-bounded cap plus an
// honest "capped" flag is the right tradeoff over true database-side
// pagination (which would also need LIKE/substring support this schema
// doesn't rely on elsewhere).
const chunkListCap = 20000

// listChunks builds a dynamic WHERE clause from filter's exact-match
// fields (each value escaped via sqlStr, since this is raw SQL text),
// queries one row past chunkListCap, and reports capped=true (with the
// extra row trimmed) if that cap was reached — free-text search and
// substring filtering happen in Go over this result (see chunks.go).
func (s *tinySQLStore) listChunks(filter chunkFilter) ([]chunkRow, bool, error) {
	var where []string
	if filter.SourceKind != "" {
		where = append(where, fmt.Sprintf("source_kind = %s", sqlStr(filter.SourceKind)))
	}
	if filter.EmbedModel != "" {
		where = append(where, fmt.Sprintf("embed_model = %s", sqlStr(filter.EmbedModel)))
	}
	q := "SELECT id, source_id, source_kind, source_name, load_id, loaded_at, doc_date, chunk_idx, content, embed_model FROM chunks"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" LIMIT %d", chunkListCap+1)

	rs, err := s.exec(q)
	if err != nil {
		return nil, false, err
	}
	rows := make([]chunkRow, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		id, _ := tinysql.GetVal(row, "id")
		sid, _ := tinysql.GetVal(row, "source_id")
		skind, _ := tinysql.GetVal(row, "source_kind")
		sname, _ := tinysql.GetVal(row, "source_name")
		lid, _ := tinysql.GetVal(row, "load_id")
		loadedAt, _ := tinysql.GetVal(row, "loaded_at")
		docDate, _ := tinysql.GetVal(row, "doc_date")
		idx, _ := tinysql.GetVal(row, "chunk_idx")
		content, _ := tinysql.GetVal(row, "content")
		embedModel, _ := tinysql.GetVal(row, "embed_model")
		rows = append(rows, chunkRow{
			ID:         toInt(id),
			SourceID:   toStr(sid),
			SourceKind: toStr(skind),
			SourceName: toStr(sname),
			LoadID:     toStr(lid),
			LoadedAt:   int64(toInt(loadedAt)),
			DocDate:    int64(toInt(docDate)),
			ChunkIdx:   toInt(idx),
			Content:    toStr(content),
			EmbedModel: toStr(embedModel),
		})
	}
	capped := len(rows) > chunkListCap
	if capped {
		rows = rows[:chunkListCap]
	}
	return rows, capped, nil
}

// save persists pending writes. For ModeDisk/ModeIndex/ModeHybrid this is
// Sync(), which flushes only dirty tables — cheap enough to call after
// every ingested source. For ModeMemory/ModeWAL, tinySQL only knows how to
// persist via a full GOB snapshot; calling this after every source is the
// O(n²) import-time trap docs/VECTOR_DB.md warns about, so those modes are
// intentionally not the default (see parseStorageMode above).
func (s *tinySQLStore) save() error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	switch s.mode {
	case tinysql.ModeDisk, tinysql.ModeIndex, tinysql.ModeHybrid:
		return s.db.Sync()
	default:
		return tinysql.SaveToFile(s.db, s.path)
	}
}

// exportAll dumps every chunk row verbatim, converting each stored
// embedding to JSON server-side via VEC_TO_JSON before decoding it in Go,
// for one-shot migration to another backend (see migrate.go) — not used on
// the normal request path.
func (s *tinySQLStore) exportAll() ([]exportedChunk, error) {
	q := `SELECT source_id, source_kind, source_name, load_id, loaded_at, content_hash, doc_date, chunk_idx, content,
	             VEC_TO_JSON(embedding) AS embedding_json, embed_model FROM chunks`
	rs, err := s.exec(q)
	if err != nil {
		return nil, err
	}
	out := make([]exportedChunk, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		sid, _ := tinysql.GetVal(row, "source_id")
		skind, _ := tinysql.GetVal(row, "source_kind")
		sname, _ := tinysql.GetVal(row, "source_name")
		lid, _ := tinysql.GetVal(row, "load_id")
		loadedAt, _ := tinysql.GetVal(row, "loaded_at")
		hash, _ := tinysql.GetVal(row, "content_hash")
		docDate, _ := tinysql.GetVal(row, "doc_date")
		idx, _ := tinysql.GetVal(row, "chunk_idx")
		content, _ := tinysql.GetVal(row, "content")
		embJSON, _ := tinysql.GetVal(row, "embedding_json")
		embedModel, _ := tinysql.GetVal(row, "embed_model")

		var vec []float64
		if err := json.Unmarshal([]byte(toStr(embJSON)), &vec); err != nil {
			return nil, fmt.Errorf("unmarshal embedding for %s#%d: %w", toStr(sid), toInt(idx), err)
		}
		out = append(out, exportedChunk{
			SourceID:    toStr(sid),
			SourceKind:  toStr(skind),
			SourceName:  toStr(sname),
			LoadID:      toStr(lid),
			LoadedAt:    int64(toInt(loadedAt)),
			ContentHash: toStr(hash),
			DocDate:     int64(toInt(docDate)),
			ChunkIdx:    toInt(idx),
			Content:     toStr(content),
			Embedding:   vec,
			EmbedModel:  toStr(embedModel),
		})
	}
	return out, nil
}

// importRaw inserts rows exactly as exported, in batches of 16 built as raw
// SQL strings (values escaped via sqlStr/vecJSON, ids pre-reserved via
// allocIDs) with original provenance and content_hash preserved, for
// one-shot migration from another backend — see exportAll.
func (s *tinySQLStore) importRaw(rows []exportedChunk) error {
	if len(rows) == 0 {
		return nil
	}
	batchSize := 16
	for i := 0; i < len(rows); i += batchSize {
		end := i + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		startID := s.allocIDs(end - i)

		vals := make([]string, 0, end-i)
		for j := i; j < end; j++ {
			e := rows[j]
			embJSONBytes, err := json.Marshal(e.Embedding)
			if err != nil {
				return fmt.Errorf("marshal embedding for %s#%d: %w", e.SourceID, e.ChunkIdx, err)
			}
			tup := fmt.Sprintf(
				"(%d, %s, %s, %s, %s, %d, %s, %d, %d, %s, VEC_FROM_JSON('%s'), %s)",
				startID+(j-i), sqlStr(e.SourceID), sqlStr(e.SourceKind), sqlStr(e.SourceName),
				sqlStr(e.LoadID), e.LoadedAt, sqlStr(e.ContentHash), e.DocDate,
				e.ChunkIdx, sqlStr(e.Content), string(embJSONBytes), sqlStr(e.EmbedModel),
			)
			vals = append(vals, tup)
			s.noteEmbedModel(e.EmbedModel)
		}
		q := "INSERT INTO chunks VALUES " + strings.Join(vals, ",")
		if _, err := s.exec(q); err != nil {
			return fmt.Errorf("import batch: %w", err)
		}
	}
	return nil
}

// contentHash returns a stable hash of extracted text, used to detect that
// a source's content hasn't changed since the last ingest so re-embedding
// can be skipped. Generic (no tinySQL dependency), but lives here rather
// than store.go since it's only ever used alongside insertChunks/
// lastContentHash's hash comparisons.
func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
