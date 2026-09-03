package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestIngestDocumentDryRunSkipsWrite proves ingestDocument's dryRun=true path
// runs extraction/chunking/hash-compare for real (accurate reported Chunks)
// but never calls rag.replaceSourceChunks: no source_id shows up afterwards
// and lastContentHash stays empty, so a later real (non-dry) ingest of the
// same content is NOT treated as "unchanged" and still gets embedded/stored.
func TestIngestDocumentDryRunSkipsWrite(t *testing.T) {
	rag, s := newTestRAG(t)
	const sourceID = "file:dry-run-test.txt"
	const text = "This is a long enough paragraph of body text to be chunked and embedded for the dry-run test."

	out, err := ingestDocument(rag, s, "test-embed", sourceID, "file", "dry-run-test.txt", text, 0, true)
	if err != nil {
		t.Fatalf("ingestDocument(dryRun=true): %v", err)
	}
	if !out.DryRun {
		t.Fatalf("want out.DryRun=true, got false")
	}
	if out.Skipped {
		t.Fatalf("want out.Skipped=false on first ingest, got true")
	}
	if out.Chunks == 0 {
		t.Fatalf("want a nonzero reported chunk count even in dry-run, got 0")
	}

	if hash := rag.lastContentHash(sourceID); hash != "" {
		t.Fatalf("dry run must not persist a content hash, got %q", hash)
	}
	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	for _, src := range sources {
		if src.SourceID == sourceID {
			t.Fatalf("dry run must not create a source, but found %q in listSources", sourceID)
		}
	}

	// A real (non-dry) ingest of the exact same content afterwards must still
	// embed/store it — the dry run must not have left behind a hash that
	// would make this look "unchanged" and get skipped.
	out2, err := ingestDocument(rag, s, "test-embed", sourceID, "file", "dry-run-test.txt", text, 0, false)
	if err != nil {
		t.Fatalf("ingestDocument(dryRun=false): %v", err)
	}
	if out2.Skipped {
		t.Fatalf("real ingest after a dry run must not be skipped as unchanged")
	}
	if out2.Chunks == 0 {
		t.Fatalf("want a nonzero chunk count from the real ingest, got 0")
	}
	if rag.lastContentHash(sourceID) == "" {
		t.Fatalf("real ingest must persist a content hash")
	}
}

// TestIngestEmailAttachmentRejectsOversizedAttachment confirms the size
// gate (emailAttachmentMaxBytes) rejects an oversized attachment BEFORE any
// extraction is attempted, rather than the old behavior of just failing
// deep inside extension-based dispatch (or, worse, an image silently
// getting OCR'd against a truncated/corrupted buffer).
func TestIngestEmailAttachmentRejectsOversizedAttachment(t *testing.T) {
	rag, s := newTestRAG(t)
	s.Import.MaxFileMB = 1 // 1MB ceiling, so a 2MB attachment is rejected quickly
	data := bytes.Repeat([]byte("a"), 2*1024*1024)

	_, err := ingestEmailAttachment(rag, s, "test-embed", "pst:mail.pst:folder:msg-1", 0, "pst_attachment", "big.txt", data, "Anfrage", "kunde@example.com", 0, false)
	if err == nil {
		t.Fatal("want an error for an oversized attachment, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want a clear 'too large' error, got %q", err.Error())
	}
}

// TestIngestEmailAttachmentRoutesImagesToOCR confirms attachment content is
// sniffed via http.DetectContentType (not just filename extension) so a
// real image attachment reaches the OCR path — previously every mail-shaped
// importer (PST/IMAP/Exchange) fell through the extension-only dispatch,
// which has no image entry, and rejected every image attachment as
// "unsupported" no matter how it arrived. Real tesseract isn't available in
// this sandbox, so this only confirms the ROUTING decision: the error is
// the "OCR disabled" message (i.e. the image branch was taken), not
// "unsupported attachment type" (the old extension-dispatch rejection).
func TestIngestEmailAttachmentRoutesImagesToOCR(t *testing.T) {
	rag, s := newTestRAG(t)
	s.AllowShellExec = false // OCR requires shell exec (tesseract) — deliberately off here
	// Minimal valid 1x1 PNG, so http.DetectContentType sniffs "image/png"
	// even though the filename below carries no recognizable extension.
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde, 0x00, 0x00, 0x00,
		0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x00, 0x03, 0x00, 0x01, 0x1a, 0xd3, 0x2b, 0xa8, 0x00, 0x00, 0x00,
		0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}

	_, err := ingestEmailAttachment(rag, s, "test-embed", "pst:mail.pst:folder:msg-1", 0, "pst_attachment", "signature.dat", png, "Anfrage", "kunde@example.com", 0, false)
	if err == nil {
		t.Fatal("want an error since OCR (AllowShellExec) is disabled, got nil")
	}
	if !strings.Contains(err.Error(), "OCR deaktiviert") {
		t.Fatalf("want the image branch's OCR-disabled error (content-sniffed as an image despite no image extension), got %q", err.Error())
	}
}

// TestIngestEmailAttachmentNonImageDispatchUnaffected confirms a normal
// non-image attachment (a .txt file, wrongly-labeled-as-image content
// aside) still goes through the original extension-based dispatch and gets
// ingested normally — the image-routing change must not affect the
// existing native-text/HTML/.eml/markitdown path.
func TestIngestEmailAttachmentNonImageDispatchUnaffected(t *testing.T) {
	rag, s := newTestRAG(t)
	out, err := ingestEmailAttachment(rag, s, "test-embed", "pst:mail.pst:folder:msg-1", 0, "pst_attachment", "notes.txt", []byte("Body text long enough to be chunked and embedded for this attachment test."), "Anfrage", "kunde@example.com", 0, false)
	if err != nil {
		t.Fatalf("ingestEmailAttachment: %v", err)
	}
	if out.Chunks == 0 {
		t.Fatalf("want at least one chunk ingested, got %+v", out)
	}
}
