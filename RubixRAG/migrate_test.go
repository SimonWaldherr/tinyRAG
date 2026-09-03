package main

import "testing"

// TestRunMigrationTinySQLToSQLite is the actual "is switching backends
// easy" guarantee VECTOR_DB.md promises: every chunk (with its original
// embedding, provenance and content_hash) must survive a copy from one
// backend into a freshly-opened instance of the other, with no re-embedding
// call involved — vectorCandidates against the destination must still find
// the migrated vectors afterward, not just docCount matching.
func TestRunMigrationTinySQLToSQLite(t *testing.T) {
	src := newTestTinySQLStore(t)
	sc1 := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n1", Chunks: []string{"a", "b"}}
	sc2 := sourceChunks{SourceID: "doc-2", SourceKind: "pst_email", SourceName: "n2", Chunks: []string{"c"}}
	if _, err := src.insertChunks(sc1, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks sc1: %v", err)
	}
	if _, err := src.insertChunks(sc2, "model-a", [][]float64{{1, 1}}, "load-2", 2000, "hash-2"); err != nil {
		t.Fatalf("insertChunks sc2: %v", err)
	}

	dst := newTestSQLiteStore(t)
	n, err := runMigration(src, dst)
	if err != nil {
		t.Fatalf("runMigration: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 migrated chunks, got %d", n)
	}
	if got := dst.docCount(); got != 3 {
		t.Fatalf("want 3 chunks in destination, got %d", got)
	}

	hits, err := dst.vectorCandidates([]float64{1, 1}, "model-a", 1)
	if err != nil {
		t.Fatalf("vectorCandidates on migrated destination: %v", err)
	}
	if len(hits) != 1 || hits[0].Content != "c" {
		t.Fatalf("expected the migrated vector to still be findable by similarity search, got %+v", hits)
	}
	if got := dst.lastContentHash("doc-1"); got != "hash-1" {
		t.Fatalf("want content_hash preserved as hash-1, got %q", got)
	}
}

// TestRunMigrationSQLiteToTinySQL checks the reverse direction — migration
// isn't a one-way door, matching -migrate-from-backend's documented ability
// to move data either way.
func TestRunMigrationSQLiteToTinySQL(t *testing.T) {
	src := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n"}
	sc.Chunks = []string{"a", "b"}
	if _, err := src.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}

	dst := newTestTinySQLStore(t)
	n, err := runMigration(src, dst)
	if err != nil {
		t.Fatalf("runMigration: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 migrated chunks, got %d", n)
	}
	chunks, err := dst.fetchSourceChunks("doc-1")
	if err != nil {
		t.Fatalf("fetchSourceChunks: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Content != "a" || chunks[1].Content != "b" {
		t.Fatalf("want [a b] in order, got %+v", chunks)
	}
}

// TestRunMigrationEmptySourceIsANoOp guards against importRaw ever being
// called with a zero-length slice on an empty source store — should just
// report 0 migrated rows, not error.
func TestRunMigrationEmptySourceIsANoOp(t *testing.T) {
	src := newTestTinySQLStore(t)
	dst := newTestSQLiteStore(t)
	n, err := runMigration(src, dst)
	if err != nil {
		t.Fatalf("runMigration on an empty source: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 migrated chunks for an empty source, got %d", n)
	}
}
