package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMailboxConfigResolvedPassword(t *testing.T) {
	cfg := mailboxConfig{Password: "inline"}
	if got := cfg.resolvedPassword(); got != "inline" {
		t.Fatalf("want %q, got %q", "inline", got)
	}

	t.Setenv("R3_TEST_IMAP_PASSWORD", "from-env")
	cfg = mailboxConfig{Password: "inline", PasswordEnv: "R3_TEST_IMAP_PASSWORD"}
	if got := cfg.resolvedPassword(); got != "from-env" {
		t.Fatalf("PasswordEnv should take precedence, got %q", got)
	}

	cfg = mailboxConfig{Password: "fallback", PasswordEnv: "R3_TEST_IMAP_PASSWORD_UNSET"}
	if got := cfg.resolvedPassword(); got != "fallback" {
		t.Fatalf("should fall back to inline Password when the env var is unset, got %q", got)
	}
}

// fakeIMAPClient satisfies imapClient with canned messages, so
// importIMAPMessages's ingestion/LastUID-tracking logic (the part R3 owns)
// can be tested without a real IMAP server. realIMAPClient itself is a thin
// wrapper around github.com/emersion/go-imap/v2's wire protocol and needs
// verification against a real/staging mailbox, which isn't reachable from
// this sandbox.
type fakeIMAPClient struct {
	messages []incomingMail
	sinceUID uint32
}

func (f *fakeIMAPClient) ListNewMessages(ctx context.Context, sinceUID uint32) ([]incomingMail, error) {
	f.sinceUID = sinceUID
	var out []incomingMail
	for _, m := range f.messages {
		if m.UID > sinceUID {
			out = append(out, m)
		}
	}
	return out, nil
}

// newTestRAG stands up a ragSystem backed by a real (temp-file) sqlite
// vectorStore and a fake embeddings HTTP server, mirroring the harness the
// SharePoint/Exchange-Graph connector tests used.
func newTestRAG(t *testing.T) (*ragSystem, appSettings) {
	t.Helper()
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var resp embResp
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float64 `json:"embedding"`
			}{Embedding: []float64{0.1, 0.2, 0.3}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(embedServer.Close)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := newVectorStore(storageSettings{Backend: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatalf("newVectorStore: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(*sqliteStore); ok {
			closer.Close()
		}
	})

	embedClient := newLMClientFull("local", embedServer.URL, "", "test-embed", "test-chat", "")
	rag := newRAG(embedClient, map[string]*lmClient{"local": embedClient}, "local", store)
	if err := rag.init(); err != nil {
		t.Fatalf("rag.init: %v", err)
	}

	s := appSettings{ChunkSize: 500, K: 5}
	return rag, s
}

func TestImportIMAPMessages(t *testing.T) {
	rag, s := newTestRAG(t)
	cfg := mailboxConfig{Username: "user@example.com", Mailbox: "INBOX", LastUID: 5}

	client := &fakeIMAPClient{messages: []incomingMail{
		{UID: 6, Mailbox: "INBOX", Fields: emailFields{
			Subject: "Hello", From: "a@example.com",
			Body: "First new message body, long enough to be embedded as a chunk.",
			Date: time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
		}},
		{UID: 7, Mailbox: "INBOX", Fields: emailFields{
			Subject: "World", From: "b@example.com",
			Body: "Second new message body, also long enough to be embedded.",
			Date: time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC),
		}},
		// Below LastUID: must never reach ingestDocument.
		{UID: 3, Mailbox: "INBOX", Fields: emailFields{Subject: "Old", Body: "Stale message."}},
	}}

	var progressCalls int
	res, err := importIMAPMessages(context.Background(), client, rag, s, "test-embed", cfg, false, func(p imapProgress) { progressCalls++ })
	if err != nil {
		t.Fatalf("importIMAPMessages: %v", err)
	}
	if client.sinceUID != 5 {
		t.Fatalf("want ListNewMessages called with sinceUID=5, got %d", client.sinceUID)
	}
	if res.Messages != 2 {
		t.Fatalf("want 2 messages ingested, got %d", res.Messages)
	}
	if res.LastUID != 7 {
		t.Fatalf("want LastUID advanced to the highest fetched UID (7), got %d", res.LastUID)
	}
	if res.Chunks == 0 {
		t.Fatalf("expected at least one chunk to be ingested, got %+v", res)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", res.Errors)
	}
	if progressCalls != 2 {
		t.Fatalf("want 2 progress callbacks, got %d", progressCalls)
	}

	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	want := map[string]bool{
		"imap:user@example.com:INBOX:6": false,
		"imap:user@example.com:INBOX:7": false,
	}
	for _, src := range sources {
		if _, ok := want[src.SourceID]; ok {
			want[src.SourceID] = true
			if src.SourceKind != "imap_email" {
				t.Errorf("source %s: want source_kind imap_email, got %s", src.SourceID, src.SourceKind)
			}
		}
	}
	for id, found := range want {
		if !found {
			t.Errorf("expected source %s to have been ingested", id)
		}
	}

	// Re-running with the advanced LastUID must be a no-op: the fake
	// client's filter means nothing new is returned.
	cfg.LastUID = res.LastUID
	res2, err := importIMAPMessages(context.Background(), client, rag, s, "test-embed", cfg, false, nil)
	if err != nil {
		t.Fatalf("importIMAPMessages (rerun): %v", err)
	}
	if res2.Messages != 0 {
		t.Fatalf("rerun with advanced LastUID should fetch nothing new, got %d messages", res2.Messages)
	}
}

// TestImportIMAPMessagesSurfacesAttachmentWarnings confirms both
// incomingMail.AttachmentError (a whole-message parse failure from
// extractMailAttachments) and per-attachment ingest failures fold into
// res.AttachmentWarnings instead of vanishing into an undifferentiated
// Skipped count — the gap that made "23 attachments skipped" and "23
// skipped because OCR is disabled" look identical in an import summary.
func TestImportIMAPMessagesSurfacesAttachmentWarnings(t *testing.T) {
	rag, s := newTestRAG(t)
	cfg := mailboxConfig{Username: "user@example.com", Mailbox: "INBOX"}

	client := &fakeIMAPClient{messages: []incomingMail{
		{UID: 1, Mailbox: "INBOX", Fields: emailFields{
			Subject: "Mail mit kaputtem Multipart", From: "a@example.com",
			Body: "Body text long enough to be embedded as a chunk for this test.",
		}, AttachmentError: "parse eml: unexpected EOF"},
		{UID: 2, Mailbox: "INBOX", Fields: emailFields{
			Subject: "Mail mit unsupported Anhang", From: "b@example.com",
			Body: "Second message body, also long enough to be embedded.",
		}, Attachments: []mailAttachment{{Filename: "archive.zip", Data: []byte("PK\x03\x04 not really a zip but has bytes")}}},
	}}

	res, err := importIMAPMessages(context.Background(), client, rag, s, "test-embed", cfg, false, nil)
	if err != nil {
		t.Fatalf("importIMAPMessages: %v", err)
	}
	if len(res.AttachmentWarnings) != 2 {
		t.Fatalf("want 2 attachment warnings (1 parse error + 1 unsupported attachment), got %d: %v", len(res.AttachmentWarnings), res.AttachmentWarnings)
	}
	joined := strings.Join(res.AttachmentWarnings, "\n")
	if !strings.Contains(joined, "unexpected EOF") {
		t.Errorf("want the AttachmentError surfaced as a warning, got %v", res.AttachmentWarnings)
	}
	if !strings.Contains(joined, "archive.zip") {
		t.Errorf("want the unsupported-attachment failure naming archive.zip, got %v", res.AttachmentWarnings)
	}
}
