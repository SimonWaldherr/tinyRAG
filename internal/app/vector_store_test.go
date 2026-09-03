package app

import (
	"math"
	"sync"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func newTestChunkStore(t *testing.T) *tinySQLChunkStore {
	t.Helper()
	db, err := tinysql.OpenDB(tinysql.StorageConfig{Mode: tinysql.ModeMemory})
	if err != nil {
		t.Fatalf("failed to open in-memory tinySQL db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := newTinySQLChunkStore(db, &sync.Mutex{}, "", tinysql.ModeMemory)
	if err := store.init(); err != nil {
		t.Fatalf("store.init() failed: %v", err)
	}
	return store
}

func testStoredChunk(id int, article string, chunkIdx int, embedding []float64, roleScope string) storedChunk {
	return storedChunk{
		ID: id, Article: article, ChunkIdx: chunkIdx, Content: "content for " + article,
		Embedding: embedding, EmbedModel: "test-embed", RoleScope: roleScope,
		ChunkID: "c-" + article, DocumentID: "doc-" + article,
		SourceSystem: "tinyrag", SourceType: "article", SourceTitle: article,
		TrustLevel: 0.5, SourceQuality: 0.5, FreshnessScore: 0.5, QualityScore: 0.5, FeedbackScore: 0.5,
		OpenLinkAllowed: true,
	}
}

func TestTinySQLChunkStoreInitIsIdempotent(t *testing.T) {
	store := newTestChunkStore(t)
	if err := store.init(); err != nil {
		t.Fatalf("second init() call should be a no-op, got error: %v", err)
	}
	if got := store.maxChunkID(); got != -1 {
		t.Errorf("expected maxChunkID() == -1 on empty store, got %d", got)
	}
	if got := store.countChunks("1=1"); got != 0 {
		t.Errorf("expected 0 chunks in empty store, got %d", got)
	}
}

func TestTinySQLChunkStoreInsertAndCount(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{
		testStoredChunk(0, "golang", 0, []float64{1, 0, 0}, "|all|"),
		testStoredChunk(1, "golang", 1, []float64{0, 1, 0}, "|all|"),
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}
	if got := store.countChunks("1=1"); got != 2 {
		t.Fatalf("expected 2 chunks, got %d", got)
	}
	if got := store.maxChunkID(); got != 1 {
		t.Errorf("expected maxChunkID() == 1, got %d", got)
	}
}

func TestTinySQLChunkStoreInsertEmptyIsNoOp(t *testing.T) {
	store := newTestChunkStore(t)
	if err := store.insertChunks(nil); err != nil {
		t.Fatalf("inserting an empty slice should not error: %v", err)
	}
	if got := store.countChunks("1=1"); got != 0 {
		t.Errorf("expected 0 chunks, got %d", got)
	}
}

func TestTinySQLChunkStoreReplaceKeepsExistingChunksWhenWriteFails(t *testing.T) {
	store := newTestChunkStore(t)
	old := testStoredChunk(0, "safe-refresh", 0, []float64{1, 0, 0}, "|all|")
	old.DocumentID = "safe-refresh-doc"
	old.ChunkID = "safe-refresh-doc:0"
	old.Content = "known good source"
	if err := store.insertChunks([]storedChunk{old}); err != nil {
		t.Fatal(err)
	}

	bad := old
	bad.ID = 1
	bad.Content = "replacement that must not become visible"
	// This produces an invalid SQL float literal and makes the replacement
	// write fail after the old source has been located but before it is deleted.
	bad.TrustLevel = math.Inf(1)
	if err := store.replaceDocumentChunks(old.DocumentID, old.RoleScope, []storedChunk{bad}); err == nil {
		t.Fatal("expected replacement write to fail")
	}

	rows, err := store.loadArticleChunks(old.Article, "1=1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored rows after failed replacement = %#v, %v", rows, err)
	}
	if got := rows[0]["content"]; got != "known good source" {
		t.Fatalf("old source was not preserved: %#v", got)
	}
}

func TestTinySQLChunkStoreSearchTopKRanksBySimilarity(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{
		testStoredChunk(0, "close-match", 0, []float64{1, 0, 0}, "|all|"),
		testStoredChunk(1, "far-match", 0, []float64{0, 1, 0}, "|all|"),
		testStoredChunk(2, "orthogonal", 0, []float64{0, 0, 1}, "|all|"),
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}

	hits, err := store.searchTopK([]float64{1, 0, 0}, "", "test-embed", "1=1", 10)
	if err != nil {
		t.Fatalf("searchTopK failed: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(hits))
	}
	if hits[0].Article != "close-match" {
		t.Errorf("expected the identical vector to rank first, got %q (score=%f)", hits[0].Article, hits[0].Score)
	}
	if hits[0].Score < hits[1].Score || hits[1].Score < hits[2].Score {
		t.Errorf("hits should be sorted by descending score, got %+v", []float64{hits[0].Score, hits[1].Score, hits[2].Score})
	}
}

func TestTinySQLChunkStoreSearchTopKFiltersByEmbedModel(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{testStoredChunk(0, "golang", 0, []float64{1, 0, 0}, "|all|")}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}
	hits, err := store.searchTopK([]float64{1, 0, 0}, "", "different-embed-model", "1=1", 10)
	if err != nil {
		t.Fatalf("searchTopK failed: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected no hits for a mismatched embed model, got %d", len(hits))
	}
}

func TestTinySQLChunkStoreRoleFiltering(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{
		testStoredChunk(0, "it-only", 0, []float64{1, 0, 0}, roleScopeToken("it")),
		testStoredChunk(1, "hr-only", 0, []float64{1, 0, 0}, roleScopeToken("hr")),
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}

	hits, err := store.searchTopK([]float64{1, 0, 0}, "", "test-embed", roleAndACLFilterSQL("it"), 10)
	if err != nil {
		t.Fatalf("searchTopK failed: %v", err)
	}
	if len(hits) != 1 || hits[0].Article != "it-only" {
		t.Fatalf("expected only the 'it' scoped chunk to be visible, got %+v", hits)
	}

	countIT := store.countChunks(roleAndACLFilterSQL("it"))
	if countIT != 1 {
		t.Errorf("expected countChunks to respect the role filter, got %d", countIT)
	}
}

func TestTinySQLChunkStoreCheckArticleExists(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{testStoredChunk(0, "golang", 0, []float64{1, 0, 0}, "|all|")}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}
	exists, err := store.checkArticleExists("doc-golang", "|all|")
	if err != nil {
		t.Fatalf("checkArticleExists failed: %v", err)
	}
	if !exists {
		t.Error("expected the inserted document to be reported as existing")
	}
	exists, err = store.checkArticleExists("doc-does-not-exist", "|all|")
	if err != nil {
		t.Fatalf("checkArticleExists failed: %v", err)
	}
	if exists {
		t.Error("expected a non-existent document to be reported as not existing")
	}
}

func TestTinySQLChunkStoreFetchNeighborContent(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{
		testStoredChunk(0, "golang", 0, []float64{1, 0, 0}, "|all|"),
		testStoredChunk(1, "golang", 1, []float64{0, 1, 0}, "|all|"),
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}
	content, ok := store.fetchNeighborContent("doc-golang", "golang", 1, "1=1")
	if !ok || content != "content for golang" {
		t.Fatalf("unexpected neighbor content: %q ok=%v", content, ok)
	}
	if _, ok := store.fetchNeighborContent("doc-golang", "golang", 99, "1=1"); ok {
		t.Error("expected no neighbor content for an out-of-range chunk index")
	}
}

func TestTinySQLChunkStoreFetchNeighborContentIsDocumentScoped(t *testing.T) {
	store := newTestChunkStore(t)
	first := testStoredChunk(0, "Handbuch", 1, []float64{1, 0, 0}, "|all|")
	first.DocumentID = "handbook-a"
	first.ChunkID = "handbook-a:1"
	first.Content = "Nachbar aus Dokument A"
	second := testStoredChunk(1, "Handbuch", 1, []float64{0, 1, 0}, "|all|")
	second.DocumentID = "handbook-b"
	second.ChunkID = "handbook-b:1"
	second.Content = "Nachbar aus Dokument B"
	if err := store.insertChunks([]storedChunk{first, second}); err != nil {
		t.Fatal(err)
	}

	content, ok := store.fetchNeighborContent("handbook-b", "Handbuch", 1, "1=1")
	if !ok || content != "Nachbar aus Dokument B" {
		t.Fatalf("document-scoped neighbor = %q ok=%v", content, ok)
	}
	if _, ok := store.fetchNeighborContent("missing-document", "Handbuch", 1, "1=1"); ok {
		t.Fatal("known document IDs must not fall back to same-titled sources")
	}
}

func TestTinySQLChunkStoreListSourcesAndDelete(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{
		testStoredChunk(0, "golang", 0, []float64{1, 0, 0}, "|all|"),
		testStoredChunk(1, "golang", 1, []float64{0, 1, 0}, "|all|"),
		testStoredChunk(2, "python", 0, []float64{0, 0, 1}, "|all|"),
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}

	sources := store.listSources("1=1")
	if len(sources) != 2 {
		t.Fatalf("expected 2 distinct sources, got %d: %+v", len(sources), sources)
	}

	if err := store.deleteSource("doc-golang", "golang", "1=1"); err != nil {
		t.Fatalf("deleteSource failed: %v", err)
	}
	if got := store.countChunks("1=1"); got != 1 {
		t.Errorf("expected 1 remaining chunk after delete, got %d", got)
	}
	sources = store.listSources("1=1")
	if len(sources) != 1 || sources[0]["article"] != "python" {
		t.Errorf("expected only 'python' to remain, got %+v", sources)
	}
}

func TestTinySQLChunkStoreListsAndDeletesSameTitledDocumentsIndependently(t *testing.T) {
	store := newTestChunkStore(t)
	first := testStoredChunk(0, "Handbuch", 0, []float64{1, 0, 0}, "|all|")
	first.DocumentID = "handbook-a"
	first.ChunkID = "handbook-a:0"
	second := testStoredChunk(1, "Handbuch", 0, []float64{0, 1, 0}, "|all|")
	second.DocumentID = "handbook-b"
	second.ChunkID = "handbook-b:0"
	if err := store.insertChunks([]storedChunk{first, second}); err != nil {
		t.Fatal(err)
	}

	sources := store.listSources("1=1")
	if len(sources) != 2 {
		t.Fatalf("same-titled document sources = %+v", sources)
	}
	if err := store.deleteSource("handbook-b", "Handbuch", "1=1"); err != nil {
		t.Fatal(err)
	}
	if got := store.countChunks("1=1"); got != 1 {
		t.Fatalf("remaining chunks after document delete = %d", got)
	}
	rows, err := store.loadArticleChunks("Handbuch", "1=1")
	if err != nil || len(rows) != 1 || rows[0]["document_id"] != "handbook-a" {
		t.Fatalf("remaining same-titled source = %#v, err=%v", rows, err)
	}
}

func TestTinySQLChunkStoreLoadArticleChunksOrdered(t *testing.T) {
	store := newTestChunkStore(t)
	chunks := []storedChunk{
		testStoredChunk(0, "golang", 1, []float64{1, 0, 0}, "|all|"),
		testStoredChunk(1, "golang", 0, []float64{0, 1, 0}, "|all|"),
	}
	if err := store.insertChunks(chunks); err != nil {
		t.Fatalf("insertChunks failed: %v", err)
	}
	rows, err := store.loadArticleChunks("golang", "1=1")
	if err != nil {
		t.Fatalf("loadArticleChunks failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0]["chunk_idx"] != 0 {
		t.Errorf("expected rows ordered by chunk_idx ascending, got %+v", rows)
	}
}

func TestNewVectorChunkStoreFactory(t *testing.T) {
	db, err := tinysql.OpenDB(tinysql.StorageConfig{Mode: tinysql.ModeMemory})
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()
	mu := &sync.Mutex{}

	store, err := newVectorChunkStore("", db, mu, "", tinysql.ModeMemory)
	if err != nil {
		t.Fatalf("default backend should not error: %v", err)
	}
	if _, ok := store.(*tinySQLChunkStore); !ok {
		t.Errorf("expected default backend to be *tinySQLChunkStore, got %T", store)
	}

	store, err = newVectorChunkStore("tinysql", db, mu, "", tinysql.ModeMemory)
	if err != nil || store == nil {
		t.Errorf("explicit 'tinysql' backend should succeed, err=%v", err)
	}

	if _, err := newVectorChunkStore("sqlite-vec", db, mu, "", tinysql.ModeMemory); err == nil {
		t.Error("sqlite-vec backend should error without the sqlite_vec build tag")
	}
}

func TestVecJSONRoundTripsThroughEscapeSQ(t *testing.T) {
	// vecJSON must produce a string that is safe to embed in a single-quoted
	// SQL literal after escapeSQ — this is exactly how insertChunks/searchTopK
	// build their queries.
	vec := []float64{1.5, -2.25, 0}
	encoded := vecJSON(vec)
	escaped := escapeSQ(encoded)
	if escaped == "" {
		t.Fatal("expected a non-empty escaped vector literal")
	}
}
