package app

import (
	"strings"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func TestAssembleContextKeepsSameTitledDocumentsSeparate(t *testing.T) {
	previous := settings
	t.Cleanup(func() { settings = previous })
	settings = &settingsStore{s: appSettings{EmbedModel: "test-embed", ActiveRole: "it"}}

	r, err := newRAG(r3MockLM{}, 2, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.db.Close() })
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	store := r.chunkStore.(*tinySQLChunkStore)

	chunks := []storedChunk{
		{ID: 0, Article: "Handbuch", ChunkIdx: 0, Content: "Primärtext Dokument A", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "doc-a:0", DocumentID: "doc-a"},
		{ID: 1, Article: "Handbuch", ChunkIdx: 1, Content: "Nachbar Dokument A", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "doc-a:1", DocumentID: "doc-a"},
		{ID: 2, Article: "Handbuch", ChunkIdx: 0, Content: "Primärtext Dokument B", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "doc-b:0", DocumentID: "doc-b"},
		{ID: 3, Article: "Handbuch", ChunkIdx: 1, Content: "Nachbar Dokument B", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "doc-b:1", DocumentID: "doc-b"},
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatal(err)
	}

	hits := []retrievalHit{
		{Article: "Handbuch", ChunkIdx: 0, Content: "Primärtext Dokument A", DocumentID: "doc-a", ChunkID: "doc-a:0", R3Score: 0.9, Citation: Citation{DocumentID: "doc-a", ChunkID: "doc-a:0", Title: "Handbuch"}},
		{Article: "Handbuch", ChunkIdx: 0, Content: "Primärtext Dokument B", DocumentID: "doc-b", ChunkID: "doc-b:0", R3Score: 0.8, Citation: Citation{DocumentID: "doc-b", ChunkID: "doc-b:0", Title: "Handbuch"}},
	}
	context, debug, err := r.assembleContext(hits, 2, "test", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Primärtext Dokument A", "Nachbar Dokument A", "Primärtext Dokument B", "Nachbar Dokument B"} {
		if strings.Count(context, want) != 1 {
			t.Fatalf("context must contain %q exactly once, got:\n%s", want, context)
		}
	}
	if len(debug.Citations) != 2 {
		t.Fatalf("primary citations = %#v", debug.Citations)
	}
}

func TestLoadArticleContextDefersAmbiguousSameTitledDocuments(t *testing.T) {
	previous := settings
	t.Cleanup(func() { settings = previous })
	settings = &settingsStore{s: appSettings{EmbedModel: "test-embed", ActiveRole: "it"}}

	r, err := newRAG(r3MockLM{}, 2, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.db.Close() })
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	store := r.chunkStore.(*tinySQLChunkStore)
	chunks := []storedChunk{
		{ID: 0, Article: "Handbuch", ChunkIdx: 0, Content: "Dokument A", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "doc-a:0", DocumentID: "doc-a"},
		{ID: 1, Article: "Handbuch", ChunkIdx: 0, Content: "Dokument B", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "doc-b:0", DocumentID: "doc-b"},
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatal(err)
	}
	if context, _, ok := r.loadArticleContext("Handbuch", false, 0); ok || context != "" {
		t.Fatalf("ambiguous title should use ranked retrieval, got %q", context)
	}
}

func TestAssembleContextKeepsPrimaryWhenNeighborExceedsBudget(t *testing.T) {
	previousSettings := settings
	previousBudget := assembledContextBudgetChars
	t.Cleanup(func() {
		settings = previousSettings
		assembledContextBudgetChars = previousBudget
	})
	settings = &settingsStore{s: appSettings{EmbedModel: "test-embed", ActiveRole: "it"}}
	assembledContextBudgetChars = 800

	r, err := newRAG(r3MockLM{}, 1, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.db.Close() })
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	store := r.chunkStore.(*tinySQLChunkStore)
	hugeNeighbor := strings.Repeat("vorheriger Kontext ", 100)
	chunks := []storedChunk{
		{ID: 0, Article: "Runbook", ChunkIdx: 0, Content: hugeNeighbor, Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "runbook:0", DocumentID: "runbook"},
		{ID: 1, Article: "Runbook", ChunkIdx: 1, Content: "Primäre, zitierfähige Anleitung", Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "runbook:1", DocumentID: "runbook"},
		{ID: 2, Article: "Runbook", ChunkIdx: 2, Content: hugeNeighbor, Embedding: []float64{1, 0}, EmbedModel: "test-embed", RoleScope: "|all|", ChunkID: "runbook:2", DocumentID: "runbook"},
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatal(err)
	}

	context, debug, err := r.assembleContext([]retrievalHit{{
		Article: "Runbook", ChunkIdx: 1, Content: "Primäre, zitierfähige Anleitung",
		DocumentID: "runbook", ChunkID: "runbook:1", R3Score: 0.8,
		Citation: Citation{DocumentID: "runbook", ChunkID: "runbook:1", Title: "Runbook"},
	}}, 1, "test", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(context) > assembledContextBudgetChars {
		t.Fatalf("context length = %d, budget = %d", len(context), assembledContextBudgetChars)
	}
	if !strings.Contains(context, "Primäre, zitierfähige Anleitung") || strings.Contains(context, hugeNeighbor) {
		t.Fatalf("primary/neighbor packing failed:\n%s", context)
	}
	if !debug.ContextTruncated || strings.Count(context, contextOmissionMarker) != 1 {
		t.Fatalf("truncation debug/context = %#v, %q", debug, context)
	}
}
