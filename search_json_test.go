package main

import (
	"testing"
)

type orderedSearchChunkStore struct {
	*tinySQLChunkStore
	hits []retrievalHit
}

func (s *orderedSearchChunkStore) searchTopK(_ []float64, _, _, _ string, _ int) ([]retrievalHit, error) {
	return append([]retrievalHit(nil), s.hits...), nil
}

func TestSearchJSONKeepsPrimaryHitWhenItIsAnotherHitsNeighbor(t *testing.T) {
	previous := settings
	t.Cleanup(func() { settings = previous })
	settings = &settingsStore{s: appSettings{EmbedModel: "test-embed", ActiveRole: "it"}}

	base := newTestChunkStore(t)
	chunks := []storedChunk{
		{ID: 0, Article: "Runbook", ChunkIdx: 0, Content: "Primärtreffer null", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "runbook:0", DocumentID: "runbook"},
		{ID: 1, Article: "Runbook", ChunkIdx: 1, Content: "Primärtreffer eins", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "runbook:1", DocumentID: "runbook"},
	}
	if err := base.insertChunks(chunks); err != nil {
		t.Fatal(err)
	}
	store := &orderedSearchChunkStore{
		tinySQLChunkStore: base,
		hits: []retrievalHit{
			{Article: "Runbook", ChunkIdx: 1, Content: "Primärtreffer eins", Score: 0.99, ChunkID: "runbook:1", DocumentID: "runbook"},
			{Article: "Runbook", ChunkIdx: 0, Content: "Primärtreffer null", Score: 0.61, ChunkID: "runbook:0", DocumentID: "runbook"},
		},
	}
	r := &ragSystem{chunkStore: store, lm: r3MockLM{}, k: 2}

	results, err := r.searchJSON("query", 2)
	if err != nil {
		t.Fatal(err)
	}
	var primary []searchResult
	for _, result := range results {
		if result.Score >= 0 {
			primary = append(primary, result)
		}
	}
	if len(primary) != 2 {
		t.Fatalf("primary search results = %#v", results)
	}
	if primary[0].ChunkID != "runbook:1" || primary[1].ChunkID != "runbook:0" {
		t.Fatalf("unexpected primary identities: %#v", primary)
	}
}
