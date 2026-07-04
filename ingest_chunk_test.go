package main

import "testing"

func TestChunkTextRespectsMaxLen(t *testing.T) {
	text := "Absatz eins ist kurz.\nAbsatz zwei ist auch kurz.\nAbsatz drei ist ebenfalls kurz."
	chunks := chunkText(text, 40)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for maxLen=40, got %d: %+v", len(chunks), chunks)
	}
	for _, c := range chunks {
		if c == "" {
			t.Error("chunkText must not produce empty chunks")
		}
	}
}

func TestChunkTextSingleParagraphFitsOneChunk(t *testing.T) {
	chunks := chunkText("Ein einzelner kurzer Absatz.", 1000)
	if len(chunks) != 1 {
		t.Fatalf("expected exactly 1 chunk, got %d", len(chunks))
	}
}

func TestChunkTextIgnoresBlankLines(t *testing.T) {
	chunks := chunkText("Absatz A\n\n\nAbsatz B", 1000)
	if len(chunks) != 1 || chunks[0] != "Absatz A\nAbsatz B" {
		t.Fatalf("blank lines should be dropped, got %+v", chunks)
	}
}

func TestChunkTextEmptyInput(t *testing.T) {
	if chunks := chunkText("", 100); len(chunks) != 0 {
		t.Errorf("empty input should produce no chunks, got %+v", chunks)
	}
}

func TestSanitizeTextForIngestRedactsPII(t *testing.T) {
	s := appSettings{RedactPII: true}
	text := "Kontakt: max.mustermann@example.com oder +49 170 1234567."
	out, n := sanitizeTextForIngest(text, s)
	if n == 0 {
		t.Fatal("expected at least one redaction")
	}
	if !contains(out, "[REDACTED_EMAIL]") {
		t.Errorf("expected email to be redacted, got %q", out)
	}
}

func TestSanitizeTextForIngestNoOpWhenDisabled(t *testing.T) {
	s := appSettings{RedactPII: false}
	text := "Kontakt: max.mustermann@example.com"
	out, n := sanitizeTextForIngest(text, s)
	if n != 0 || out != text {
		t.Errorf("expected no-op when RedactPII is false, got out=%q n=%d", out, n)
	}
}

func TestChunksForIngestProducesChunks(t *testing.T) {
	s := appSettings{ChunkSize: 50}
	chunks, redactions := chunksForIngest("Ein Testabsatz mit ausreichend Text fuer Chunking-Tests.", s)
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	if redactions != 0 {
		t.Errorf("expected zero redactions without PII, got %d", redactions)
	}
}
