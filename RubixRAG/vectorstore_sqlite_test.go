package main

import (
	"path/filepath"
	"testing"
)

func newTestSQLiteStore(t *testing.T) *sqliteStore {
	t.Helper()
	s, err := newSQLiteStore(storageSettings{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("newSQLiteStore: %v", err)
	}
	if err := s.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Release the file handle before the test's t.TempDir() is cleaned up —
	// matters on Windows, where an open SQLite file blocks its own deletion.
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSQLiteInsertFetchRoundTripsUTF8 mirrors the tinySQL backend's own
// UTF-8/quote round-trip guard — sqliteStore uses parameterized queries
// throughout (never string-interpolated SQL), so this is mostly a sanity
// check rather than a regression guard for a known-fragile lexer, but the
// two backends should agree on this basic contract.
func TestSQLiteInsertFetchRoundTripsUTF8(t *testing.T) {
	s := newTestSQLiteStore(t)
	want := "Grüße aus München — it's café ☕"
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "café's menü", Chunks: []string{want}}
	if _, err := s.insertChunks(sc, "test-model", [][]float64{{1, 0, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	chunks, err := s.fetchSourceChunks("doc-1")
	if err != nil {
		t.Fatalf("fetchSourceChunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Content != want {
		t.Fatalf("UTF-8/quote content corrupted, got %+v", chunks)
	}
}

// TestSQLiteVectorCandidatesRanksByCosineSimilarity checks the brute-force
// Go-side cosine scoring (vectorstore_sqlite.go has no native vector
// support, unlike tinySQL's VEC_COSINE_SIMILARITY) actually ranks the
// closest vector first with a near-1.0 score for an exact match.
func TestSQLiteVectorCandidatesRanksByCosineSimilarity(t *testing.T) {
	s := newTestSQLiteStore(t)
	vecs := [][]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b", "c"}}
	if _, err := s.insertChunks(sc, "model-a", vecs, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	hits, err := s.vectorCandidates([]float64{1, 0, 0}, "model-a", 2)
	if err != nil {
		t.Fatalf("vectorCandidates: %v", err)
	}
	if len(hits) == 0 || hits[0].Content != "a" {
		t.Fatalf("expected the identical vector (chunk 'a') to rank first, got %+v", hits)
	}
	if hits[0].VectorScore < 0.999999 {
		t.Fatalf("expected VectorScore ~1.0 for an identical vector, got %v", hits[0].VectorScore)
	}
}

// TestSQLiteVectorCandidatesFiltersEmbedModel guards the same cross-model
// leakage concern the tinySQL backend tests for: a query against model-a
// must never surface model-b's chunks, even though both live in the same
// table.
func TestSQLiteVectorCandidatesFiltersEmbedModel(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc1 := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a"}}
	sc2 := sourceChunks{SourceID: "doc-2", SourceKind: "file", SourceName: "n", Chunks: []string{"b"}}
	if _, err := s.insertChunks(sc1, "model-a", [][]float64{{1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if _, err := s.insertChunks(sc2, "model-b", [][]float64{{0, 1}}, "load-2", 1000, "hash-2"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	hits, err := s.vectorCandidates([]float64{1, 0}, "model-a", 10)
	if err != nil {
		t.Fatalf("vectorCandidates: %v", err)
	}
	if len(hits) != 1 || hits[0].Content != "a" {
		t.Fatalf("expected exactly chunk 'a', got %+v", hits)
	}
}

// TestSQLiteDeleteSourceAndLastContentHash checks the ingest-replace
// contract: lastContentHash reflects the most recent load, and
// deleteSource actually removes every chunk of that source_id.
func TestSQLiteDeleteSourceAndLastContentHash(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if got := s.lastContentHash("doc-1"); got != "hash-1" {
		t.Fatalf("want hash-1, got %q", got)
	}
	if err := s.deleteSource("doc-1"); err != nil {
		t.Fatalf("deleteSource: %v", err)
	}
	if got := s.lastContentHash("doc-1"); got != "" {
		t.Fatalf("want \"\" after delete, got %q", got)
	}
	if n := s.docCount(); n != 0 {
		t.Fatalf("want 0 chunks after delete, got %d", n)
	}
}

// TestSQLiteListSourcesAndListChunks checks the two admin-UI-facing
// listing paths: one summary row per (source_id, load_id) for
// listSources, and exact-match filtering for listChunks.
func TestSQLiteListSourcesAndListChunks(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc1 := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n1", Chunks: []string{"a", "b"}}
	sc2 := sourceChunks{SourceID: "doc-2", SourceKind: "pst_email", SourceName: "n2", Chunks: []string{"c"}}
	if _, err := s.insertChunks(sc1, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks sc1: %v", err)
	}
	if _, err := s.insertChunks(sc2, "model-a", [][]float64{{1, 1}}, "load-2", 2000, "hash-2"); err != nil {
		t.Fatalf("insertChunks sc2: %v", err)
	}

	sources, err := s.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("want 2 sources, got %+v", sources)
	}

	rows, capped, err := s.listChunks(chunkFilter{SourceKind: "pst_email"})
	if err != nil {
		t.Fatalf("listChunks: %v", err)
	}
	if capped {
		t.Fatalf("expected capped=false for a tiny result set")
	}
	if len(rows) != 1 || rows[0].SourceID != "doc-2" {
		t.Fatalf("want exactly doc-2's chunk, got %+v", rows)
	}
}

// TestSQLiteExportImportRoundTrip checks the migration primitives directly
// (see migrate_test.go for the end-to-end cross-backend test): exportAll
// must return every field importRaw needs to recreate the row exactly,
// including the embedding vector.
func TestSQLiteExportImportRoundTrip(t *testing.T) {
	src := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", DocDate: 1234}
	sc.Chunks = []string{"a", "b"}
	if _, err := src.insertChunks(sc, "model-a", [][]float64{{1, 0.5}, {0.25, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	exported, err := src.exportAll()
	if err != nil {
		t.Fatalf("exportAll: %v", err)
	}
	if len(exported) != 2 {
		t.Fatalf("want 2 exported rows, got %d", len(exported))
	}

	dst := newTestSQLiteStore(t)
	if err := dst.importRaw(exported); err != nil {
		t.Fatalf("importRaw: %v", err)
	}
	chunks, err := dst.fetchSourceChunks("doc-1")
	if err != nil {
		t.Fatalf("fetchSourceChunks: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Content != "a" || chunks[1].Content != "b" {
		t.Fatalf("want [a b] in order, got %+v", chunks)
	}
	if got := dst.lastContentHash("doc-1"); got != "hash-1" {
		t.Fatalf("want content_hash preserved as hash-1, got %q", got)
	}
}

// TestSQLiteKeywordCandidatesFTSRanksBM25 confirms sqliteStore satisfies
// rank.go's ftsKeywordScorer capability (real BM25 via FTS5) and that a
// chunk mentioning the query term more often/more centrally ranks with a
// higher (less negative-before-negation) score than one where the term is
// a minor mention — the whole point of using bm25() over
// keywordOverlapScore's cruder term-presence fraction.
func TestSQLiteKeywordCandidatesFTSRanksBM25(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{
		"Lieferantenrichtlinie Lieferantenrichtlinie Lieferantenrichtlinie für Kistenpfennig",
		"Ein kurzer Satz, der nur beiläufig die Lieferantenrichtlinie erwähnt.",
		"Völlig unrelated content about widgets and gears.",
	}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}, {1, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}

	scores, err := s.keywordCandidatesFTS("Lieferantenrichtlinie", "model-a", 10)
	if err != nil {
		t.Fatalf("keywordCandidatesFTS: %v", err)
	}
	key0 := ftsCandidateKey("doc-1", 0)
	key1 := ftsCandidateKey("doc-1", 1)
	key2 := ftsCandidateKey("doc-1", 2)
	if _, ok := scores[key2]; ok {
		t.Fatalf("chunk 2 (no match) should not appear in results, got %+v", scores)
	}
	s0, ok0 := scores[key0]
	s1, ok1 := scores[key1]
	if !ok0 || !ok1 {
		t.Fatalf("want both matching chunks scored, got %+v", scores)
	}
	if s0 <= s1 {
		t.Fatalf("want the heavily-repeated-term chunk (0) to score higher than the passing-mention chunk (1), got %v vs %v", s0, s1)
	}
}

// TestSQLiteKeywordCandidatesFTSFiltersEmbedModel mirrors
// TestSQLiteVectorCandidatesFiltersEmbedModel for the keyword path: a
// query scoped to model-a must never surface model-b's chunks even though
// chunks_fts holds both.
func TestSQLiteKeywordCandidatesFTSFiltersEmbedModel(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc1 := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"Rubix Logistik Bericht"}}
	sc2 := sourceChunks{SourceID: "doc-2", SourceKind: "file", SourceName: "n", Chunks: []string{"Rubix Logistik Bericht"}}
	if _, err := s.insertChunks(sc1, "model-a", [][]float64{{1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks sc1: %v", err)
	}
	if _, err := s.insertChunks(sc2, "model-b", [][]float64{{0, 1}}, "load-2", 1000, "hash-2"); err != nil {
		t.Fatalf("insertChunks sc2: %v", err)
	}
	scores, err := s.keywordCandidatesFTS("Logistik", "model-a", 10)
	if err != nil {
		t.Fatalf("keywordCandidatesFTS: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("want exactly 1 scored chunk (model-a only), got %+v", scores)
	}
	if _, ok := scores[ftsCandidateKey("doc-1", 0)]; !ok {
		t.Fatalf("want doc-1's chunk scored, got %+v", scores)
	}
}

// TestSQLiteKeywordCandidatesFTSNoTermsReturnsEmpty confirms a query with
// no tokenizable terms (rank.go's tokenize, e.g. only punctuation/short
// words) short-circuits to an empty map instead of sending an empty FTS5
// MATCH string, which FTS5 rejects as a syntax error.
func TestSQLiteKeywordCandidatesFTSNoTermsReturnsEmpty(t *testing.T) {
	s := newTestSQLiteStore(t)
	scores, err := s.keywordCandidatesFTS("? ! .", "model-a", 10)
	if err != nil {
		t.Fatalf("keywordCandidatesFTS: %v", err)
	}
	if len(scores) != 0 {
		t.Fatalf("want an empty (non-nil) map, got %+v", scores)
	}
}

// TestSQLiteDeleteSourceRemovesFTSEntries confirms deleteSource keeps
// chunks_fts in sync with chunks — a stale FTS row for a deleted source
// would otherwise keep surfacing as a keyword-search "hit" for content
// that's no longer actually retrievable (fetchSourceChunks would return
// nothing for it).
func TestSQLiteDeleteSourceRemovesFTSEntries(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"Einzigartiges Suchwort Zebraphant"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if scores, err := s.keywordCandidatesFTS("Zebraphant", "model-a", 10); err != nil || len(scores) != 1 {
		t.Fatalf("want 1 match before delete, got %+v (err %v)", scores, err)
	}
	if err := s.deleteSource("doc-1"); err != nil {
		t.Fatalf("deleteSource: %v", err)
	}
	scores, err := s.keywordCandidatesFTS("Zebraphant", "model-a", 10)
	if err != nil {
		t.Fatalf("keywordCandidatesFTS: %v", err)
	}
	if len(scores) != 0 {
		t.Fatalf("want no matches after deleteSource, got %+v", scores)
	}
}

// TestSQLiteImportRawPopulatesFTS confirms the migration path (importRaw)
// keeps chunks_fts in sync too, not just the normal ingest path
// (insertChunks) — otherwise a store repopulated via migrate.go would
// silently lose real BM25 scoring until its next re-ingest.
func TestSQLiteImportRawPopulatesFTS(t *testing.T) {
	src := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"Einzigartiges Suchwort Zebraphant"}}
	if _, err := src.insertChunks(sc, "model-a", [][]float64{{1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	exported, err := src.exportAll()
	if err != nil {
		t.Fatalf("exportAll: %v", err)
	}

	dst := newTestSQLiteStore(t)
	if err := dst.importRaw(exported); err != nil {
		t.Fatalf("importRaw: %v", err)
	}
	scores, err := dst.keywordCandidatesFTS("Zebraphant", "model-a", 10)
	if err != nil {
		t.Fatalf("keywordCandidatesFTS: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("want 1 match after importRaw, got %+v", scores)
	}
}

// TestEmbeddingEncodingRoundtrip covers both halves of decodeEmbedding: the
// new binary blob format (float32-exact values survive the roundtrip) and
// the legacy float64-JSON text older databases still contain.
func TestEmbeddingEncodingRoundtrip(t *testing.T) {
	vec := []float64{0.25, -1, 0.5, 0}
	got, err := decodeEmbedding(encodeEmbedding(vec))
	if err != nil {
		t.Fatalf("decode(encode): %v", err)
	}
	if len(got) != len(vec) {
		t.Fatalf("roundtrip length: want %d, got %d", len(vec), len(got))
	}
	for i := range vec {
		if got[i] != vec[i] { // all chosen values are float32-exact
			t.Fatalf("roundtrip value %d: want %v, got %v", i, vec[i], got[i])
		}
	}

	legacy, err := decodeEmbedding([]byte("[0.25,-1,0.5]"))
	if err != nil {
		t.Fatalf("decode legacy JSON: %v", err)
	}
	if len(legacy) != 3 || legacy[0] != 0.25 || legacy[1] != -1 || legacy[2] != 0.5 {
		t.Fatalf("legacy JSON decoded wrong: %v", legacy)
	}

	if _, err := decodeEmbedding(nil); err == nil {
		t.Fatalf("empty embedding must error")
	}
	if _, err := decodeEmbedding([]byte{embeddingBlobMagic, 1, 2}); err == nil {
		t.Fatalf("truncated binary embedding must error")
	}
}

// TestSQLiteLegacyJSONEmbeddingsInterop simulates a database written before
// the binary encoding existed: a row whose embedding column holds float64
// JSON text must stay searchable, exportable and key-fetchable next to rows
// in the new format — no migration required.
func TestSQLiteLegacyJSONEmbeddingsInterop(t *testing.T) {
	s := newTestSQLiteStore(t)
	// Legacy row, inserted exactly as the old code did (JSON text).
	if _, err := s.db.Exec(`INSERT INTO chunks
		(source_id, source_kind, source_name, load_id, loaded_at, content_hash, doc_date, chunk_idx, content, embedding, embed_model)
		VALUES ('old-doc', 'file', 'alt.txt', 'load-0', 900, 'hash-0', 0, 0, 'legacy content', '[1,0,0]', 'model-a')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO chunks_fts (content, source_id, chunk_idx, embed_model) VALUES ('legacy content', 'old-doc', 0, 'model-a')`); err != nil {
		t.Fatalf("insert legacy fts row: %v", err)
	}
	// New-format row alongside it.
	sc := sourceChunks{SourceID: "new-doc", SourceKind: "file", SourceName: "neu.txt", Chunks: []string{"new content"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{0, 1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}

	hits, err := s.vectorCandidates([]float64{1, 0, 0}, "model-a", 10)
	if err != nil {
		t.Fatalf("vectorCandidates: %v", err)
	}
	if len(hits) != 2 || hits[0].SourceID != "old-doc" || hits[0].VectorScore < 0.99 {
		t.Fatalf("legacy row must rank first for its exact vector, got %+v", hits)
	}

	all, err := s.exportAll()
	if err != nil {
		t.Fatalf("exportAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("exportAll must decode both encodings, got %d rows", len(all))
	}

	if _, _, ok := s.chunkByKey("old-doc", 0, "model-a"); !ok {
		t.Fatalf("chunkByKey must read the legacy row")
	}
}

func TestSQLiteChunkByKey(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	h, emb, ok := s.chunkByKey("doc-1", 1, "model-a")
	if !ok {
		t.Fatalf("chunkByKey: want ok for an existing key")
	}
	if h.Content != "b" || h.ChunkIdx != 1 || h.SourceKind != "file" {
		t.Fatalf("chunkByKey returned wrong row: %+v", h)
	}
	if len(emb) != 2 || emb[1] != 1 {
		t.Fatalf("chunkByKey returned wrong embedding: %v", emb)
	}
	if _, _, ok := s.chunkByKey("doc-1", 5, "model-a"); ok {
		t.Fatalf("chunkByKey: want !ok for a missing chunk_idx")
	}
	if _, _, ok := s.chunkByKey("doc-1", 1, "other-model"); ok {
		t.Fatalf("chunkByKey: want !ok for a different embed_model")
	}
}

func TestSQLiteFetchSourceEmbeddings(t *testing.T) {
	s := newTestSQLiteStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"alpha", "beta"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	got, err := s.fetchSourceEmbeddings("doc-1", "model-a")
	if err != nil {
		t.Fatalf("fetchSourceEmbeddings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 embeddings, got %d", len(got))
	}
	if v, ok := got[contentHash("alpha")]; !ok || len(v) != 2 || v[0] != 1 {
		t.Fatalf("embedding for 'alpha' wrong: %v (ok=%v)", v, ok)
	}
	// Different model: nothing to reuse.
	other, err := s.fetchSourceEmbeddings("doc-1", "model-b")
	if err != nil {
		t.Fatalf("fetchSourceEmbeddings(model-b): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("want no embeddings for a different model, got %v", other)
	}
}

// TestCosineSimilarityDimensionMismatch: differing dimensions score 0 (skip)
// instead of a meaningless truncated-prefix cosine.
func TestCosineSimilarityDimensionMismatch(t *testing.T) {
	if got := cosineSimilarity([]float64{1, 0}, []float64{1, 0, 0}); got != 0 {
		t.Fatalf("dimension mismatch must score 0, got %v", got)
	}
	if got := cosineSimilarity(nil, nil); got != 0 {
		t.Fatalf("empty vectors must score 0, got %v", got)
	}
	if got := cosineSimilarity([]float64{1, 0}, []float64{1, 0}); got < 0.99 {
		t.Fatalf("identical vectors must score ~1, got %v", got)
	}
}
