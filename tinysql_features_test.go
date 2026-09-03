package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func TestStorageEncryptionKey(t *testing.T) {
	t.Setenv("TINYRAG_STORAGE_KEY", base64.StdEncoding.EncodeToString(make([]byte, tinysql.EncryptionKeySize)))
	key, err := storageEncryptionKey(true)
	if err != nil || len(key) != tinysql.EncryptionKeySize {
		t.Fatalf("storageEncryptionKey() = %d bytes, %v", len(key), err)
	}
	if _, err := storageEncryptionKey(false); err != nil {
		t.Fatalf("disabled encryption: %v", err)
	}
	t.Setenv("TINYRAG_STORAGE_KEY", "invalid")
	if _, err := storageEncryptionKey(true); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestTinySQLAuditFeature(t *testing.T) {
	rag, err := newRAG(r3MockLM{}, 3, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatal(err)
	}
	closeAudit, err := configureTinySQLOptionalFeatures(rag, appSettings{TinySQLAuditEnabled: true, TinySQLAuditPath: t.TempDir() + "/audit.jsonl"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeAudit()
	if _, err := tinysql.ExecSQL(tinysql.WithAuditText(context.Background(), "SELECT 1"), rag.db, "default", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	entries := rag.db.AuditLog().Entries()
	if got := len(entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	if entries[0].Statement != "SELECT 1" {
		t.Fatalf("audit statement = %q, want original SQL", entries[0].Statement)
	}
}

func TestTinySQLPortableSnapshot(t *testing.T) {
	db := tinysql.NewDB()
	if _, err := tinysql.ExecSQL(context.Background(), db, "default", "CREATE TABLE snapshots (id INT, title TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tinysql.ExecSQL(context.Background(), db, "default", "INSERT INTO snapshots VALUES (1, 'portable')"); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := tinysql.SaveToWriter(db, &snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := tinysql.LoadFromReader(bytes.NewReader(snapshot.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	rs, err := tinysql.ExecSQL(context.Background(), restored, "default", "SELECT title FROM snapshots")
	if err != nil || len(rs.Rows) != 1 {
		t.Fatalf("restored snapshot query = %#v, %v", rs, err)
	}
	if title, _ := tinysql.GetVal(rs.Rows[0], "title"); title != "portable" {
		t.Fatalf("restored title = %#v", title)
	}
}

func TestTinySQLVectorCacheConfiguration(t *testing.T) {
	configureTinySQLVectorCache(appSettings{
		TinySQLVectorCacheEntries:    4,
		TinySQLVectorCacheTTLSeconds: 10,
		TinySQLVectorAnalytics:       true,
	})
	t.Cleanup(func() { tinysql.ConfigureVectorCache(tinysql.DefaultVectorCacheConfig()) })
	stats := tinysql.VectorCacheAnalytics()
	if !stats.Enabled {
		t.Fatal("expected v0.49.0 VEC_SEARCH result cache to be enabled")
	}
}

func TestImportGeoAsChunks(t *testing.T) {
	geoJSON := `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"name":"Rathaus","city":"Berlin"},"geometry":{"type":"Point","coordinates":[13.4,52.5]}}]}`
	result, chunks, err := importGeoAsChunks(context.Background(), strings.NewReader(geoJSON), "geojson", "places.geojson", appSettings{ChunkSize: 800})
	if err != nil {
		t.Fatal(err)
	}
	if result.RowsInserted != 1 || len(chunks) == 0 || !strings.Contains(strings.Join(chunks, "\n"), "Rathaus") {
		t.Fatalf("unexpected geo import: rows=%d chunks=%q", result.RowsInserted, chunks)
	}
}

func TestNormalizeRetrievalMode(t *testing.T) {
	if got := normalizeRetrievalMode("VEC_SEARCH"); got != "vector" {
		t.Fatalf("vector mode = %q", got)
	}
	if got := normalizeRetrievalMode("BM25"); got != "hybrid" {
		t.Fatalf("hybrid mode = %q", got)
	}
	if got := normalizeRetrievalMode("unknown"); got != "scalar" {
		t.Fatalf("fallback mode = %q", got)
	}
}

func TestTinySQLChunkStoreInitWarmsVectorIndexWhenConfigured(t *testing.T) {
	for _, mode := range []string{"vector", "hybrid"} {
		for _, indexMode := range []string{"flat", "ivf", "hnsw"} {
			t.Run(mode+"/"+indexMode, func(t *testing.T) {
				previous := settings
				settings = &settingsStore{s: appSettings{RetrievalMode: mode, VectorIndexMode: indexMode}}
				t.Cleanup(func() { settings = previous })

				db, err := tinysql.OpenDB(tinysql.StorageConfig{Mode: tinysql.ModeMemory})
				if err != nil {
					t.Fatalf("failed to open in-memory tinySQL db: %v", err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store := newTinySQLChunkStore(db, &sync.Mutex{}, "", tinysql.ModeMemory)
				if err := store.init(); err != nil {
					t.Fatalf("store.init() with VEC_WARM enabled failed: %v", err)
				}
				// init() on an empty table must succeed (row_count=0); insert a
				// chunk and re-init to also exercise the non-empty warm-up path.
				if err := store.insertChunks([]storedChunk{
					testStoredChunk(0, "warm", 0, []float64{1, 0, 0}, "|all|"),
				}); err != nil {
					t.Fatalf("insertChunks failed: %v", err)
				}
				if err := store.init(); err != nil {
					t.Fatalf("store.init() after insert with VEC_WARM enabled failed: %v", err)
				}
			})
		}
	}
}

func TestNormalizeVectorIndexMode(t *testing.T) {
	if got := normalizeVectorIndexMode("HNSW"); got != "hnsw" {
		t.Fatalf("hnsw mode = %q", got)
	}
	if got := normalizeVectorIndexMode("ivf"); got != "ivf" {
		t.Fatalf("ivf mode = %q", got)
	}
	if got := normalizeVectorIndexMode("unknown"); got != "flat" {
		t.Fatalf("fallback mode = %q", got)
	}
}

func TestNativeVectorRetrievalMode(t *testing.T) {
	store := newTestChunkStore(t)
	if err := store.insertChunks([]storedChunk{
		testStoredChunk(1, "nearest", 0, []float64{1, 0}, "|all|"),
		testStoredChunk(2, "other", 0, []float64{0, 1}, "|all|"),
	}); err != nil {
		t.Fatal(err)
	}
	previous := settings
	settings = &settingsStore{s: appSettings{RetrievalMode: "vector"}}
	t.Cleanup(func() { settings = previous })
	hits, err := store.searchTopK([]float64{1, 0}, "", "test-embed", "1=1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Article != "nearest" {
		t.Fatalf("unexpected native vector hits: %#v", hits)
	}
}

func TestHybridRetrievalMode(t *testing.T) {
	store := newTestChunkStore(t)
	if err := store.insertChunks([]storedChunk{
		testStoredChunk(1, "close-vector-match", 0, []float64{1, 0}, "|all|"),
		testStoredChunk(2, "far-vector-match", 0, []float64{0, 1}, "|all|"),
	}); err != nil {
		t.Fatal(err)
	}
	previous := settings
	settings = &settingsStore{s: appSettings{RetrievalMode: "hybrid", VectorIndexMode: "flat"}}
	t.Cleanup(func() { settings = previous })

	// Vector-only query: no lexical signal, should behave like plain vector search.
	hits, err := store.searchTopK([]float64{1, 0}, "", "test-embed", "1=1", 2)
	if err != nil {
		t.Fatalf("hybrid searchTopK (no query text) failed: %v", err)
	}
	if len(hits) != 2 || hits[0].Article != "close-vector-match" {
		t.Fatalf("unexpected hybrid (vector-only) hits: %#v", hits)
	}

	// With a lexical query term, HYBRID_SEARCH fuses BM25 + vector via RRF.
	hits, err = store.searchTopK([]float64{1, 0}, "content", "test-embed", "1=1", 2)
	if err != nil {
		t.Fatalf("hybrid searchTopK (with query text) failed: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one fused hybrid hit")
	}
	for _, h := range hits {
		if h.Score < -1 || h.Score > 1 {
			t.Errorf("hybrid mode must feed plain cosine similarity as Score, got %f for %q", h.Score, h.Article)
		}
	}
}
