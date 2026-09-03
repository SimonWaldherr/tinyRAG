package main

import (
	"errors"
	"testing"
)

type fullTextTestStore struct {
	*tinySQLChunkStore
	hits  []retrievalHit
	err   error
	query string
}

func (s *fullTextTestStore) searchFullTextCandidates(query string, _ []float64, _, _ string, _ int) ([]retrievalHit, error) {
	s.query = query
	return s.hits, s.err
}

func TestBuildFullTextCandidateQueryUsesLiteralTermsOnly(t *testing.T) {
	got := buildFullTextCandidateQuery(`XR-500 NOT "admin" OR gateway`)
	if got != "xr OR 500 OR admin OR gateway" {
		t.Fatalf("full-text query = %q", got)
	}
	if !shouldUseFullTextCandidates("XR-500") || !shouldUseFullTextCandidates("ABC123") {
		t.Fatal("expected technical identifiers to enable full-text candidates")
	}
	if shouldUseFullTextCandidates("wie ist das wetter") {
		t.Fatal("ordinary natural-language question should stay vector-only")
	}
}

func TestMergeFullTextCandidatesDeduplicatesAndUsesBoundedFusion(t *testing.T) {
	previous := settings
	settings = &settingsStore{s: appSettings{EmbedModel: "test-embed"}}
	t.Cleanup(func() { settings = previous })

	store := &fullTextTestStore{
		tinySQLChunkStore: newTestChunkStore(t),
		hits: []retrievalHit{
			{ChunkID: "shared", DocumentID: "doc-a", Article: "Vector source", ChunkIdx: 0, Score: 0.72},
			{ChunkID: "exact", DocumentID: "doc-b", Article: "Exact source", ChunkIdx: 0, Score: 0.31},
		},
	}
	rag := &ragSystem{chunkStore: store}
	vectorHits := []retrievalHit{{ChunkID: "shared", DocumentID: "doc-a", Article: "Vector source", ChunkIdx: 0, Score: 0.80}}

	got := rag.mergeFullTextCandidates("XR-500", []float64{1, 0, 0}, "it", 3, vectorHits)
	if store.query != "xr OR 500" {
		t.Fatalf("sanitized FTS query = %q", store.query)
	}
	if len(got) != 2 {
		t.Fatalf("candidate count = %d, want 2: %#v", len(got), got)
	}
	if got[0].ChunkID != "shared" || got[0].FullTextRank != 1 || got[0].RetrievalScore <= got[0].Score {
		t.Fatalf("overlapping candidate was not fused correctly: %#v", got[0])
	}
	if got[1].ChunkID != "exact" || got[1].FullTextRank != 2 || got[1].VectorRank != 0 || got[1].RetrievalScore <= got[1].Score {
		t.Fatalf("full-text-only candidate was not retained correctly: %#v", got[1])
	}
}

func TestMergeFullTextCandidatesFailsOpen(t *testing.T) {
	previous := settings
	settings = &settingsStore{s: appSettings{EmbedModel: "test-embed"}}
	t.Cleanup(func() { settings = previous })

	store := &fullTextTestStore{tinySQLChunkStore: newTestChunkStore(t), err: errors.New("full-text unavailable")}
	rag := &ragSystem{chunkStore: store}
	vectorHits := []retrievalHit{{ChunkID: "vector", Score: 0.80}}
	got := rag.mergeFullTextCandidates("XR-500", []float64{1, 0, 0}, "it", 3, vectorHits)
	if len(got) != 1 || got[0].VectorRank != 1 || got[0].RetrievalScore != 0.80 {
		t.Fatalf("failed-open candidates = %#v", got)
	}
}

func TestTinySQLFullTextCandidatesHonorModelAndAccessFilter(t *testing.T) {
	store := newTestChunkStore(t)
	it := testStoredChunk(0, "it-only", 0, []float64{1, 0, 0}, roleScopeToken("it"))
	it.Content = "Resolution for XR-500 gateway error"
	hr := testStoredChunk(1, "hr-only", 0, []float64{1, 0, 0}, roleScopeToken("hr"))
	hr.Content = "Resolution for XR-500 gateway error"
	oldModel := testStoredChunk(2, "old-model", 0, []float64{1, 0, 0}, roleScopeToken("it"))
	oldModel.Content = "Resolution for XR-500 gateway error"
	oldModel.EmbedModel = "old-embed"
	if err := store.insertChunks([]storedChunk{it, hr, oldModel}); err != nil {
		t.Fatal(err)
	}

	hits, err := store.searchFullTextCandidates("xr OR 500", []float64{1, 0, 0}, "test-embed", roleAndACLFilterSQL("it"), 64)
	if err != nil {
		t.Fatalf("full-text candidate search failed: %v", err)
	}
	if len(hits) != 1 || hits[0].Article != "it-only" || hits[0].FullTextRank < 1 {
		t.Fatalf("expected only authorized current-model hit, got %#v", hits)
	}
}
