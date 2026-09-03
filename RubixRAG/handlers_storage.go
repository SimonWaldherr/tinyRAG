package main

import "net/http"

// ─────────────────────────────────────────────────────────────────────────────
// Admin storage telemetry
//
// tinySQL v0.20.0 bounds ModeHybrid/ModeIndex residency to MaxMemoryBytes. For
// R3's single always-hot `chunks` table that means: once the table outgrows
// the budget it can't stay resident, so it's re-decoded per query and tinySQL's
// pointer-keyed HNSW/vector/FTS caches are rebuilt on every search (see
// vectorstore_tinysql.go's header and warnIfOversized). This endpoint makes
// that condition observable in production instead of only guessed at startup.
// ─────────────────────────────────────────────────────────────────────────────

// storageStats is the backend-agnostic storage-health snapshot returned by the
// admin storage-telemetry endpoint. Oversized (together with a climbing
// LoadCount and a falling CacheHitRate) is exactly the per-query cache-rebuild
// condition warnIfOversized flags at startup. Byte counts are pre-divided to
// MiB for a human-readable admin view.
type storageStats struct {
	Backend string `json:"backend"`
	Mode    string `json:"mode"`
	// Path is the directory/file the backend actually opened at process
	// start — compare against the (possibly since-edited-but-not-yet-applied)
	// storageSettings.Path from GET /api/settings to tell "running with" from
	// "will run with after the next restart" apart in the settings UI.
	Path           string  `json:"path"`
	Chunks         int     `json:"chunks"`
	TablesInMemory int     `json:"tables_in_memory"`
	TablesOnDisk   int     `json:"tables_on_disk"`
	MemoryUsedMB   int64   `json:"memory_used_mb"`
	MemoryLimitMB  int64   `json:"memory_limit_mb"`
	DiskUsedMB     int64   `json:"disk_used_mb"`
	CacheHitRate   float64 `json:"cache_hit_rate"`
	LoadCount      int64   `json:"load_count"`
	EvictionCount  int64   `json:"eviction_count"`
	SyncCount      int64   `json:"sync_count"`
	// Oversized is true only in a pool-bounded mode (hybrid/index) whose
	// on-disk data already meets/exceeds the in-memory budget — i.e. the
	// chunks table can't stay resident and every search rebuilds the
	// HNSW/vector/FTS caches. Always false for disk/memory/wal (no bound).
	Oversized         bool             `json:"oversized"`
	VectorResultCache vectorCacheStats `json:"vector_result_cache"`
}

// vectorCacheStats mirrors the counters of tinysql.VectorCacheStats worth
// showing an operator — the process-wide VEC_SEARCH result cache enabled in
// newTinySQLStore. (This is the byte-identical-repeat-query cache, distinct
// from the per-*Table HNSW index cache the residency change actually affects.)
// ApproxBytes/HeapAllocBytes/RecentQueries were added by tinySQL's
// "configurable vector cache analytics" commit (2026-07-12, shipped in
// v0.21.1) — see docs/DEPENDENCIES.md.
type vectorCacheStats struct {
	Enabled       bool               `json:"enabled"`
	Entries       int                `json:"entries"`
	Hits          uint64             `json:"hits"`
	Misses        uint64             `json:"misses"`
	Evictions     uint64             `json:"evictions"`
	ApproxBytes   int64              `json:"approx_bytes"`
	HeapAllocMB   int64              `json:"heap_alloc_mb"`
	RecentQueries []vectorQueryEvent `json:"recent_queries,omitempty"`
}

// vectorQueryEvent mirrors one entry of tinysql.VectorCacheStats.RecentQueries
// — the bounded, in-memory VEC_SEARCH trace (query shape + cache hit/miss +
// duration, never the query vector or result rows themselves).
type vectorQueryEvent struct {
	At         int64  `json:"at"`
	Table      string `json:"table"`
	Column     string `json:"column"`
	Metric     string `json:"metric"`
	Index      string `json:"index"`
	K          int    `json:"k"`
	CacheHit   bool   `json:"cache_hit"`
	DurationMS int64  `json:"duration_ms"`
}

// storageStatser is an optional vectorStore capability (same pattern as
// ftsKeywordScorer in rank.go): backends that can report runtime storage
// health implement it. tinySQLStore does; sqliteStore doesn't, so the endpoint
// reports supported:false for it rather than inventing numbers.
type storageStatser interface {
	storageStats() storageStats
}

// handleStorageStats serves the admin-gated storage-telemetry endpoint
// (GET, read-only). For a backend without storageStatser (sqlite) it returns
// {"supported": false} rather than an error, so a UI can render a graceful
// "not available for this backend" state instead of failing.
func handleStorageStats(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		statser, ok := rag.store.(storageStatser)
		if !ok {
			writeJSON(w, map[string]any{"supported": false})
			return
		}
		writeJSON(w, statser.storageStats())
	}
}
