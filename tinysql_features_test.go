package main

import (
	"context"
	"encoding/base64"
	"strings"
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
	stmt, err := tinysql.ParseSQL("SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tinysql.Execute(context.Background(), rag.db, "default", stmt); err != nil {
		t.Fatal(err)
	}
	if got := len(rag.db.AuditLog().Entries()); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
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
		t.Fatal("expected v0.19.1 VEC_SEARCH result cache to be enabled")
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
	if got := normalizeRetrievalMode("unknown"); got != "scalar" {
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
	hits, err := store.searchTopK([]float64{1, 0}, "test-embed", "1=1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Article != "nearest" {
		t.Fatalf("unexpected native vector hits: %#v", hits)
	}
}
