package main

import (
	"math"
	"path/filepath"
	"testing"
)

func newTestTinySQLStore(t *testing.T) *tinySQLStore {
	t.Helper()
	// An explicit temp path, never the empty default: with no Path set,
	// the store falls back to the CWD-relative "r3-data" — and if a dev
	// server is running in this repo (whose r3-data is a *directory*),
	// opening that as a memory-db file fails and every test here goes
	// permanently red until that server stops.
	s, err := newTinySQLStore(storageSettings{Mode: "memory", Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("newTinySQLStore: %v", err)
	}
	if err := s.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s
}

// TestSqlStrRoundTripsUTF8AndQuotes guards against a regression of the
// lexer bug tinySQL fixed in v0.16.0: sqlStr used to have to base64-encode
// every string because multi-byte UTF-8 characters got corrupted by the
// old lexer. Plain '...'-escaping must round-trip umlauts, an em-dash and
// an embedded apostrophe unchanged.
func TestSqlStrRoundTripsUTF8AndQuotes(t *testing.T) {
	s := newTestTinySQLStore(t)
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

// TestVectorCandidatesUsesIndexedPathForSoleModel exercises the new
// VEC_SEARCH/HNSW path (vectorCandidatesIndexed) and checks that it ranks
// the exact-match vector first with a similarity score of ~1.0.
func TestVectorCandidatesUsesIndexedPathForSoleModel(t *testing.T) {
	s := newTestTinySQLStore(t)
	vecs := [][]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b", "c"}}
	if _, err := s.insertChunks(sc, "model-a", vecs, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if !s.isSoleEmbedModel("model-a") {
		t.Fatalf("expected model-a to be detected as the sole embed model")
	}
	hits, err := s.vectorCandidates([]float64{1, 0, 0}, "model-a", 2)
	if err != nil {
		t.Fatalf("vectorCandidates: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit")
	}
	if hits[0].Content != "a" {
		t.Fatalf("expected the identical vector (chunk 'a') to rank first, got %+v", hits[0])
	}
	if math.Abs(hits[0].VectorScore-1.0) > 1e-9 {
		t.Fatalf("expected VectorScore ~1.0 for an identical vector, got %v", hits[0].VectorScore)
	}
}

// TestVecSearchIndexPrefersFlatBelowCap pins the rag-guide.md#2 default: below
// vecFlatRowCap the exact, build-free flat scan is used (not the approximate
// hnsw graph). The >100k hnsw branch isn't exercised here — inserting that
// many rows would make the test slow — but is covered by the threshold logic.
func TestVecSearchIndexPrefersFlatBelowCap(t *testing.T) {
	s := newTestTinySQLStore(t)
	if got := s.vecSearchIndex(); got != "flat" {
		t.Fatalf("empty table should use flat, got %q", got)
	}
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b", "c"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}, {1, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if got := s.vecSearchIndex(); got != "flat" {
		t.Fatalf("small table (well below %d rows) should use flat, got %q", vecFlatRowCap, got)
	}
}

// TestVectorCandidatesFallsBackWithMixedEmbedModels checks that once more
// than one embed_model has been written, vectorCandidates stops trusting
// the indexed path (which has no WHERE-pushdown) and never leaks a
// different model's chunk into the result.
func TestVectorCandidatesFallsBackWithMixedEmbedModels(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc1 := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a"}}
	sc2 := sourceChunks{SourceID: "doc-2", SourceKind: "file", SourceName: "n", Chunks: []string{"b"}}
	if _, err := s.insertChunks(sc1, "model-a", [][]float64{{1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if _, err := s.insertChunks(sc2, "model-b", [][]float64{{0, 1}}, "load-2", 1000, "hash-2"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if s.isSoleEmbedModel("model-a") {
		t.Fatalf("expected mixed embed models to disable the sole-model fast path")
	}
	hits, err := s.vectorCandidates([]float64{1, 0}, "model-a", 10)
	if err != nil {
		t.Fatalf("vectorCandidates: %v", err)
	}
	for _, h := range hits {
		if h.Content == "b" {
			t.Fatalf("model-b's chunk leaked into a model-a search: %+v", h)
		}
	}
	if len(hits) != 1 || hits[0].Content != "a" {
		t.Fatalf("expected exactly chunk 'a', got %+v", hits)
	}
}

// TestKeywordCandidatesFTSRanksBM25 exercises the new (tinySQL v0.19.0+)
// FTS_SEARCH-backed keyword scorer: a chunk mentioning the query term more
// often, in a shorter document, should score higher (real BM25 term-
// frequency + document-length normalization) than one mentioning it once in
// a much longer document — behavior keywordOverlapScore's cruder
// term-presence fraction couldn't distinguish at all (both would score a
// flat 1.0, one matched term out of one).
func TestKeywordCandidatesFTSRanksBM25(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{
		"delivery delivery delivery schedule",
		"delivery mentioned once here, padded out with a lot of unrelated filler text so the document is much longer than the other one and dilutes the single hit",
	}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	scores, err := s.keywordCandidatesFTS("delivery", "model-a", 10)
	if err != nil {
		t.Fatalf("keywordCandidatesFTS: %v", err)
	}
	dense := scores[ftsCandidateKey("doc-1", 0)]
	sparse := scores[ftsCandidateKey("doc-1", 1)]
	if dense <= 0 || sparse <= 0 {
		t.Fatalf("expected both chunks to score > 0 for a matched term, got dense=%v sparse=%v", dense, sparse)
	}
	if dense <= sparse {
		t.Fatalf("expected the dense, shorter chunk to outscore the sparse, longer one (real BM25), got dense=%v sparse=%v", dense, sparse)
	}
}

// TestKeywordCandidatesFTSFiltersEmbedModel guards the same cross-model
// leakage concern as TestVectorCandidatesFallsBackWithMixedEmbedModels:
// FTS_SEARCH has no WHERE-pushdown either, so the Go-side embed_model
// filter must actually exclude a different model's chunk.
func TestKeywordCandidatesFTSFiltersEmbedModel(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc1 := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"delivery schedule"}}
	sc2 := sourceChunks{SourceID: "doc-2", SourceKind: "file", SourceName: "n", Chunks: []string{"delivery schedule"}}
	if _, err := s.insertChunks(sc1, "model-a", [][]float64{{1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if _, err := s.insertChunks(sc2, "model-b", [][]float64{{0, 1}}, "load-2", 1000, "hash-2"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	scores, err := s.keywordCandidatesFTS("delivery", "model-a", 10)
	if err != nil {
		t.Fatalf("keywordCandidatesFTS: %v", err)
	}
	if _, leaked := scores[ftsCandidateKey("doc-2", 0)]; leaked {
		t.Fatalf("model-b's chunk leaked into a model-a keyword search: %+v", scores)
	}
	if _, ok := scores[ftsCandidateKey("doc-1", 0)]; !ok {
		t.Fatalf("expected doc-1's chunk to be scored, got %+v", scores)
	}
}

// TestFetchAllSourceChunksSingleQuery checks rank.go's fetchAllSourceChunks
// against the tinySQL backend directly: it must return every chunk of a
// source in order, substituting matchedContent at matchedIdx.
func TestFetchAllSourceChunksSingleQuery(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b", "c"}}
	vecs := [][]float64{{1, 0}, {0, 1}, {1, 1}}
	if _, err := s.insertChunks(sc, "model-a", vecs, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	rag := &ragSystem{store: s}
	got := rag.fetchAllSourceChunks("doc-1", []rankedHit{{ChunkIdx: 1, Content: "MATCHED"}}, -1, -1) // -1/-1: unlimited, the whole source
	want := []string{"a", "MATCHED", "c"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

// TestFetchAllSourceChunksWindowing checks rankingConfig.ContextChunksBefore/
// After's actual effect: a bounded window around matchedIdx instead of every
// chunk the source has, on both sides and at the boundary.
func TestFetchAllSourceChunksWindowing(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b", "c", "d", "e"}}
	vecs := [][]float64{{1, 0}, {0, 1}, {1, 1}, {2, 1}, {1, 2}}
	if _, err := s.insertChunks(sc, "model-a", vecs, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	rag := &ragSystem{store: s}

	cases := []struct {
		name          string
		matchedIdx    int
		before, after int
		want          []string
	}{
		{"no neighbors (0/0)", 2, 0, 0, []string{"MATCHED"}},
		{"one before, one after", 2, 1, 1, []string{"b", "MATCHED", "d"}},
		{"clamped at the start", 0, 2, 0, []string{"MATCHED"}},
		{"clamped at the end", 4, 0, 2, []string{"MATCHED"}},
		{"unlimited (-1/-1)", 2, -1, -1, []string{"a", "b", "MATCHED", "d", "e"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rag.fetchAllSourceChunks("doc-1", []rankedHit{{ChunkIdx: c.matchedIdx, Content: "MATCHED"}}, c.before, c.after)
			if len(got) != len(c.want) {
				t.Fatalf("want %v, got %v", c.want, got)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("want %v, got %v", c.want, got)
				}
			}
		})
	}
}

// TestFetchSourceKindRoundTrips guards handleSourceContent/handleSourceOriginal/
// handleDraftReply's source_access enforcement (handlers.go's
// sourceAccessAllowedForRequest): they all resolve a source_id's kind via
// ragSystem.fetchSourceKind before deciding whether to disclose its
// content, so this must return the SourceKind insertChunks actually stored,
// not just succeed.
func TestFetchSourceKindRoundTrips(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "pst_email", SourceName: "n", Chunks: []string{"a", "b"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	rag := &ragSystem{store: s}
	kind, ok := rag.fetchSourceKind("doc-1")
	if !ok || kind != "pst_email" {
		t.Fatalf("want (\"pst_email\", true), got (%q, %v)", kind, ok)
	}
	if _, ok := rag.fetchSourceKind("no-such-source"); ok {
		t.Fatalf("expected ok=false for a source_id with no stored chunks")
	}
}

// TestTinySQLChunkByKeyAndSourceEmbeddings covers the two new optional
// capabilities on the tinySQL backend — chunkByKey (rank.go's hybrid-union
// fetch) and fetchSourceEmbeddings (store.go's embedding reuse) — including
// the VEC_TO_JSON read-back of stored vectors.
func TestTinySQLChunkByKeyAndSourceEmbeddings(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"alpha", "beta"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}

	h, emb, ok := s.chunkByKey("doc-1", 1, "model-a")
	if !ok {
		t.Fatalf("chunkByKey: want ok for an existing key")
	}
	if h.Content != "beta" || h.ChunkIdx != 1 || h.SourceKind != "file" {
		t.Fatalf("chunkByKey returned wrong row: %+v", h)
	}
	if len(emb) != 2 || emb[1] != 1 {
		t.Fatalf("chunkByKey returned wrong embedding: %v", emb)
	}
	if _, _, ok := s.chunkByKey("doc-1", 7, "model-a"); ok {
		t.Fatalf("chunkByKey: want !ok for a missing chunk_idx")
	}
	if _, _, ok := s.chunkByKey("doc-1", 1, "model-b"); ok {
		t.Fatalf("chunkByKey: want !ok for a different embed_model")
	}

	got, err := s.fetchSourceEmbeddings("doc-1", "model-a")
	if err != nil {
		t.Fatalf("fetchSourceEmbeddings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 embeddings, got %d", len(got))
	}
	if v, ok := got[contentHash("beta")]; !ok || len(v) != 2 || v[1] != 1 {
		t.Fatalf("embedding for 'beta' wrong: %v (ok=%v)", v, ok)
	}
}

// TestFetchAllSourceChunksMultipleMatchWindows: two far-apart matches in the
// same source must produce both windows with a "[…]" gap marker between
// them; adjacent/overlapping windows must merge without a marker.
func TestFetchAllSourceChunksMultipleMatchWindows(t *testing.T) {
	s := newTestTinySQLStore(t)
	chunks := []string{"a", "b", "c", "d", "e", "f", "g"}
	vecs := make([][]float64, len(chunks))
	for i := range vecs {
		vecs[i] = []float64{1, 0}
	}
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: chunks}
	if _, err := s.insertChunks(sc, "model-a", vecs, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	rag := &ragSystem{store: s}

	// Far apart, 0/0 windows: both matches, gap marker between.
	got := rag.fetchAllSourceChunks("doc-1", []rankedHit{
		{ChunkIdx: 1, Content: "B-MATCH"},
		{ChunkIdx: 5, Content: "F-MATCH"},
	}, 0, 0)
	want := []string{"B-MATCH", "[…]", "F-MATCH"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}

	// Overlapping 1/1 windows around 1 and 3: contiguous 0..4, no marker.
	got = rag.fetchAllSourceChunks("doc-1", []rankedHit{
		{ChunkIdx: 1, Content: "B-MATCH"},
		{ChunkIdx: 3, Content: "D-MATCH"},
	}, 1, 1)
	want = []string{"a", "B-MATCH", "c", "D-MATCH", "e"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}
