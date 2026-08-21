package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

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

func TestChunkTextKeepsNumberedListTogether(t *testing.T) {
	text := "Vorgehen:\n1. Erster Schritt.\n2. Zweiter Schritt.\n3. Dritter Schritt.\n4. Vierter Schritt."
	chunks := chunkText(text, 60)
	found := false
	for _, c := range chunks {
		if strings.Contains(c, "1. Erster Schritt.") && strings.Contains(c, "2. Zweiter Schritt.") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected consecutive list items to be packed together when they fit, got %+v", chunks)
	}
}

func TestChunkTextKeepsTableRowsTogether(t *testing.T) {
	text := "Werte:\n| A | B |\n| 1 | 2 |\n| 3 | 4 |\n| 5 | 6 |"
	chunks := chunkText(text, 1000)
	if len(chunks) != 1 {
		t.Fatalf("expected the whole small table to fit in one chunk, got %d: %+v", len(chunks), chunks)
	}
	for _, row := range []string{"| A | B |", "| 1 | 2 |", "| 3 | 4 |", "| 5 | 6 |"} {
		if !strings.Contains(chunks[0], row) {
			t.Errorf("expected table row %q to survive intact in the chunk, got %q", row, chunks[0])
		}
	}
}

func TestChunkTextSplitsOversizedTableByRow(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "| row-%d | value-%d | another-column-%d |\n", i, i, i)
	}
	chunks := chunkText(b.String(), 80)
	if len(chunks) < 2 {
		t.Fatalf("expected an oversized table to be split across multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 160 {
			t.Errorf("chunk grew far beyond maxLen, oversized-unit fallback likely broken: %d bytes", len(c))
		}
		if strings.Count(c, "|") > 0 && strings.Count(c, "|")%2 != 0 {
			t.Errorf("chunk contains an unterminated table row: %q", c)
		}
	}
}

func TestChunkTextKeepsCodeFenceTogether(t *testing.T) {
	text := "Beispiel:\n```\nfunc main() {\n    fmt.Println(\"hi\")\n}\n```\nEnde."
	chunks := chunkText(text, 1000)
	if len(chunks) != 1 {
		t.Fatalf("expected the whole small snippet to fit in one chunk, got %d: %+v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "    fmt.Println(\"hi\")") {
		t.Errorf("expected code indentation to be preserved inside a fenced block, got %q", chunks[0])
	}
}

func TestChunkTextSplitsUnbreakableLongLine(t *testing.T) {
	long := strings.Repeat("wortohnegrenzen ", 50) // one very long "line", no newlines
	chunks := chunkText(long, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected an oversized single line to be split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 100 {
			t.Errorf("expected every chunk to respect maxLen=100, got %d bytes: %q", len(c), c)
		}
	}
	if strings.Join(chunks, " ") == "" {
		t.Fatal("splitting must not drop content")
	}
}

func TestChunkTextSplitsUnbreakableLongLineOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("ä", 300) // multi-byte runes throughout
	chunks := chunkText(long, 50)
	if len(chunks) < 2 {
		t.Fatalf("expected the long multi-byte line to be split, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Errorf("chunk is not valid UTF-8 (rune boundary corrupted): %q", c)
		}
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
