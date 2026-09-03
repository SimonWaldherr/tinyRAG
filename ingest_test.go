package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func newTestRAGForIngest(t *testing.T) *ragSystem {
	t.Helper()
	r, err := newRAG(r3MockLM{}, 3, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatalf("newRAG failed: %v", err)
	}
	if err := r.init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	s := &settingsStore{}
	s.s.EmbedModel = "embed"
	s.s.ActiveRole = "it"
	s.s.ChunkSize = 800
	settings = s
	return r
}

type failAfterEmbeddingLM struct {
	r3MockLM
	calls      int
	failOnCall int
}

func (m *failAfterEmbeddingLM) embed(texts []string) ([][]float64, error) {
	m.calls++
	if m.calls >= m.failOnCall {
		return nil, fmt.Errorf("embedding service unavailable")
	}
	return m.r3MockLM.embed(texts)
}

func TestUpsertDocumentSkipsUnchangedAndReplacesChanged(t *testing.T) {
	r := newTestRAGForIngest(t)
	meta := R3IngestMetadata{
		DocumentID:   "doc-upsert",
		SourceSystem: "push:test",
		SourceType:   "official_doc",
		SourceTitle:  "Policy",
		UpdateMode:   "upsert",
	}
	first, err := r.addChunksWithMetadataResult("Policy", []string{"old policy text"}, "embed", []string{"it"}, meta)
	if err != nil {
		t.Fatalf("initial ingest failed: %v", err)
	}
	if first.Status != "inserted" {
		t.Fatalf("expected inserted, got %s", first.Status)
	}
	same, err := r.addChunksWithMetadataResult("Policy", []string{"old policy text"}, "embed", []string{"it"}, meta)
	if err != nil {
		t.Fatalf("same ingest failed: %v", err)
	}
	if same.Status != "skipped_unchanged" {
		t.Fatalf("expected skipped_unchanged, got %s", same.Status)
	}
	updated, err := r.addChunksWithMetadataResult("Policy", []string{"new policy text"}, "embed", []string{"it"}, meta)
	if err != nil {
		t.Fatalf("updated ingest failed: %v", err)
	}
	if updated.Status != "updated" {
		t.Fatalf("expected updated, got %s", updated.Status)
	}
	stmt, err := tinysql.ParseSQL("SELECT content FROM chunks WHERE document_id = 'doc-upsert'")
	if err != nil {
		t.Fatalf("parse SQL failed: %v", err)
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if rs == nil || len(rs.Rows) != 1 {
		rows := 0
		if rs != nil {
			rows = len(rs.Rows)
		}
		t.Fatalf("expected one replacement chunk, got %d", rows)
	}
	got, _ := tinysql.GetVal(rs.Rows[0], "content")
	if !strings.Contains(fmt.Sprint(got), "new policy text") {
		t.Fatalf("expected updated content, got %v", got)
	}
}

func TestFailedUpsertKeepsPreviousDocumentAvailable(t *testing.T) {
	r := newTestRAGForIngest(t)
	meta := R3IngestMetadata{
		DocumentID: "doc-safe-refresh",
		SourceType: "official_doc",
		UpdateMode: "upsert",
	}
	if _, err := r.addChunksWithMetadataResult("Runbook", []string{"stable previous content"}, "embed", []string{"it"}, meta); err != nil {
		t.Fatalf("initial ingest failed: %v", err)
	}

	// The replacement has two embedding batches. The second fails, so no write
	// may occur and the first version must remain retrievable.
	r.setLM(&failAfterEmbeddingLM{failOnCall: 2})
	replacement := make([]string, 17)
	for i := range replacement {
		replacement[i] = fmt.Sprintf("replacement chunk %d", i)
	}
	if _, err := r.addChunksWithMetadataResult("Runbook", replacement, "embed", []string{"it"}, meta); err == nil {
		t.Fatal("expected replacement embedding failure")
	}

	stmt, err := tinysql.ParseSQL("SELECT content FROM chunks WHERE document_id = 'doc-safe-refresh'")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	if err != nil || rs == nil || len(rs.Rows) != 1 {
		rows := 0
		if rs != nil {
			rows = len(rs.Rows)
		}
		t.Fatalf("previous source was not preserved (rows=%d, err=%v)", rows, err)
	}
	content, _ := tinysql.GetVal(rs.Rows[0], "content")
	if fmt.Sprint(content) != "stable previous content" {
		t.Fatalf("previous content = %q, want stable version", content)
	}
}

func TestDirectoryScanReindexesUpdatedFileWithSameName(t *testing.T) {
	r := newTestRAGForIngest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "runbook.md")
	if err := os.WriteFile(path, []byte("version one runbook"), 0o644); err != nil {
		t.Fatalf("write fixture failed: %v", err)
	}
	req := ragFolderScanRequest{
		Path:      dir,
		Recursive: true,
		Roles:     []string{"it"},
		Metadata: R3IngestMetadata{
			SourceSystem: "pull:test",
			SourceType:   "official_doc",
			UpdateMode:   "upsert",
		},
	}
	first := scanDirectoryIntoRAG(r, req, "test")
	if first.FilesChanged != 1 || first.FilesSkipped != 0 {
		t.Fatalf("expected first scan changed=1 skipped=0, got changed=%d skipped=%d", first.FilesChanged, first.FilesSkipped)
	}
	second := scanDirectoryIntoRAG(r, req, "test")
	if second.FilesChanged != 0 || second.FilesSkipped != 1 {
		t.Fatalf("expected unchanged scan changed=0 skipped=1, got changed=%d skipped=%d", second.FilesChanged, second.FilesSkipped)
	}
	if err := os.WriteFile(path, []byte("version two runbook"), 0o644); err != nil {
		t.Fatalf("update fixture failed: %v", err)
	}
	third := scanDirectoryIntoRAG(r, req, "test")
	if third.FilesChanged != 1 || third.FilesSkipped != 0 {
		t.Fatalf("expected updated scan changed=1 skipped=0, got changed=%d skipped=%d", third.FilesChanged, third.FilesSkipped)
	}
	docID := stableContentHash("folder|" + filepath.Clean(dir) + "|runbook.md")
	stmt, err := tinysql.ParseSQL("SELECT content FROM chunks WHERE document_id = '" + docID + "'")
	if err != nil {
		t.Fatalf("parse SQL failed: %v", err)
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if rs == nil || len(rs.Rows) != 1 {
		rows := 0
		if rs != nil {
			rows = len(rs.Rows)
		}
		t.Fatalf("expected one current chunk, got %d", rows)
	}
	got, _ := tinysql.GetVal(rs.Rows[0], "content")
	gotText := fmt.Sprint(got)
	if !strings.Contains(gotText, "version two") || strings.Contains(gotText, "version one") {
		t.Fatalf("expected only updated content, got %v", got)
	}
}
