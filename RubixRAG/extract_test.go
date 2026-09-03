package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeTransferEncodingBase64(t *testing.T) {
	// "hello world" base64-encoded, with an embedded line break the way a
	// real MIME body would wrap long lines.
	encoded := []byte("aGVsbG8g\r\nd29ybGQ=")
	got := decodeTransferEncoding(encoded, "base64")
	if string(got) != "hello world" {
		t.Fatalf("want %q, got %q", "hello world", string(got))
	}
}

func TestDecodeTransferEncodingUnknownPassesThrough(t *testing.T) {
	data := []byte("plain bytes")
	got := decodeTransferEncoding(data, "")
	if string(got) != "plain bytes" {
		t.Fatalf("want data unchanged, got %q", string(got))
	}
}

func TestExtractMailAttachmentsFindsFileAttachment(t *testing.T) {
	raw := []byte("From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: Test\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Body text.\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; name=\"note.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"aGVsbG8gYXR0YWNobWVudA==\r\n" +
		"--BOUND--\r\n")

	atts, warnings, err := extractMailAttachments(raw, 0)
	if err != nil {
		t.Fatalf("extractMailAttachments: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("want no warnings, got %v", warnings)
	}
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d: %+v", len(atts), atts)
	}
	if atts[0].Filename != "note.txt" {
		t.Fatalf("want filename note.txt, got %q", atts[0].Filename)
	}
	if string(atts[0].Data) != "hello attachment" {
		t.Fatalf("want decoded data %q, got %q", "hello attachment", string(atts[0].Data))
	}
}

func TestExtractMailAttachmentsSinglePartHasNone(t *testing.T) {
	raw := []byte("From: a@example.com\r\nSubject: Test\r\nContent-Type: text/plain\r\n\r\nJust a plain body.\r\n")
	atts, _, err := extractMailAttachments(raw, 0)
	if err != nil {
		t.Fatalf("extractMailAttachments: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("want no attachments for a single-part message, got %d", len(atts))
	}
}

func TestExtractMailAttachmentsOversizedIsSkippedNotTruncated(t *testing.T) {
	raw := []byte("From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: Test\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Body text.\r\n" +
		"--BOUND\r\n" +
		"Content-Type: application/octet-stream; name=\"big.bin\"\r\n" +
		"Content-Disposition: attachment; filename=\"big.bin\"\r\n" +
		"\r\n" +
		"0123456789\r\n" +
		"--BOUND--\r\n")

	atts, warnings, err := extractMailAttachments(raw, 5)
	if err != nil {
		t.Fatalf("extractMailAttachments: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("want the oversized attachment skipped rather than truncated, got %d: %+v", len(atts), atts)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "big.bin") {
		t.Fatalf("want a warning naming big.bin, got %v", warnings)
	}
}

// TestCollapseRepeatedRunsDots reproduces the "PDF table-of-contents
// dot-leader" case: a long run of the same punctuation rune, purely visual,
// collapses to exactly 3 occurrences.
func TestCollapseRepeatedRunsDots(t *testing.T) {
	in := "Kapitel 3 " + strings.Repeat(".", 80) + " 42"
	want := "Kapitel 3 ... 42"
	if got := collapseRepeatedRuns(in); got != want {
		t.Fatalf("dot-leader: want %q, got %q", want, got)
	}
}

// TestCollapseRepeatedRunsDashes covers a markdown table separator row's
// alignment dashes.
func TestCollapseRepeatedRunsDashes(t *testing.T) {
	in := strings.Repeat("-", 38)
	want := "---"
	if got := collapseRepeatedRuns(in); got != want {
		t.Fatalf("dash run: want %q, got %q", want, got)
	}
}

// TestCollapseRepeatedRunsSpaces covers a run of the same whitespace rune.
func TestCollapseRepeatedRunsSpaces(t *testing.T) {
	in := "a" + strings.Repeat(" ", 20) + "b"
	want := "a   b"
	if got := collapseRepeatedRuns(in); got != want {
		t.Fatalf("space run: want %q, got %q", want, got)
	}
}

// TestCollapseRepeatedRunsLeavesDigitRunsUntouched guards the critical
// scoping rule: a long run of the same DIGIT (a real part/serial number)
// must never be collapsed, or real data would be silently corrupted.
func TestCollapseRepeatedRunsLeavesDigitRunsUntouched(t *testing.T) {
	in := "Seriennummer: " + strings.Repeat("0", 10) + "42"
	if got := collapseRepeatedRuns(in); got != in {
		t.Fatalf("digit run must be left untouched, want %q, got %q", in, got)
	}
}

// TestCollapseRepeatedRunsLeavesLetterRunsUntouched mirrors the digit case
// for a long run of the same LETTER.
func TestCollapseRepeatedRunsLeavesLetterRunsUntouched(t *testing.T) {
	in := "Teilenummer: " + strings.Repeat("A", 12) + "-1000"
	if got := collapseRepeatedRuns(in); got != in {
		t.Fatalf("letter run must be left untouched, want %q, got %q", in, got)
	}
}

// TestCollapseRepeatedRunsMarkdownTableSeparator checks a realistic mixed
// line: a markdown table separator row with long dash runs in multiple
// cells, alongside pipe/space characters that must not themselves trigger
// any (incorrect) collapsing since none of them individually repeats 4+
// times in a row.
func TestCollapseRepeatedRunsMarkdownTableSeparator(t *testing.T) {
	in := "| " + strings.Repeat("-", 38) + " | " + strings.Repeat("-", 8) + " |"
	want := "| --- | --- |"
	if got := collapseRepeatedRuns(in); got != want {
		t.Fatalf("markdown table separator: want %q, got %q", want, got)
	}
}

// TestCollapseRepeatedRunsMixedRealWorldContent checks that real
// surrounding text (letters/digits, short runs) survives untouched while an
// embedded dot-leader is collapsed — the "don't corrupt real text around
// it" requirement.
func TestCollapseRepeatedRunsMixedRealWorldContent(t *testing.T) {
	in := "Inhaltsverzeichnis\nKapitel 1: Einleitung " + strings.Repeat(".", 40) + " 3\n" +
		"Kapitel 2: Teil-Nr. 555555555 " + strings.Repeat(".", 25) + " 17\n"
	want := "Inhaltsverzeichnis\nKapitel 1: Einleitung ... 3\n" +
		"Kapitel 2: Teil-Nr. 555555555 ... 17\n"
	if got := collapseRepeatedRuns(in); got != want {
		t.Fatalf("mixed content: want %q, got %q", want, got)
	}
}

// TestCollapseRepeatedRunsMultiByteRunes checks a run of the same
// multi-byte UTF-8 rune (an em-dash) collapses correctly without breaking
// on a rune boundary — the naive byte-oriented approach this deliberately
// avoids would corrupt the UTF-8 encoding entirely.
func TestCollapseRepeatedRunsMultiByteRunes(t *testing.T) {
	in := "Text " + strings.Repeat("—", 10) + " Ende"
	want := "Text ——— Ende"
	got := collapseRepeatedRuns(in)
	if got != want {
		t.Fatalf("multi-byte rune run: want %q, got %q", want, got)
	}
	if !utf8OK(got) {
		t.Fatalf("result is not valid UTF-8: %q", got)
	}
}

// TestCollapseRepeatedRunsShortRunsUntouched checks the "4+" boundary: a
// run of exactly 3 (or fewer) is left completely unchanged, only 4+ gets
// truncated down to 3.
func TestCollapseRepeatedRunsShortRunsUntouched(t *testing.T) {
	in := "a...b--c"
	if got := collapseRepeatedRuns(in); got != in {
		t.Fatalf("runs of 3 or fewer must be untouched, want %q, got %q", in, got)
	}
}

// utf8OK is a tiny local helper so TestCollapseRepeatedRunsMultiByteRunes
// doesn't need to import unicode/utf8 just for one assertion elsewhere in
// this file's style.
func utf8OK(s string) bool {
	return strings.ToValidUTF8(s, "�") == s
}

// TestExtractTextImageRequiresShellExec: image files are now a supported
// upload/folder type (routed through tesseract OCR like mail attachments
// always were) — but only with allow_shell_exec on; otherwise the error
// must say exactly what to enable instead of "unsupported file type".
func TestExtractTextImageRequiresShellExec(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scan.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nnot really a png"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := extractText(path, appSettings{})
	if err == nil {
		t.Fatalf("want an error with OCR disabled")
	}
	if !strings.Contains(err.Error(), "allow_shell_exec") {
		t.Fatalf("error must point at allow_shell_exec, got %v", err)
	}
	if strings.Contains(err.Error(), "unsupported file type") {
		t.Fatalf("image must no longer be 'unsupported', got %v", err)
	}
}

// TestIngestFolderSkipsImagesWithoutShellExec: a folder walk with OCR
// disabled must silently skip images (like any unsupported type) instead of
// producing one error row per photo.
func TestIngestFolderSkipsImagesWithoutShellExec(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notiz.txt"), []byte("Ein Textdokument mit genug Inhalt für einen ordentlichen Chunk im Testverzeichnis."), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foto.jpg"), []byte("fake image bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	rag, s := newTestRAG(t)
	results, err := ingestFolder(context.Background(), rag, s, "test-embed", dir, 0, false)
	if err != nil {
		t.Fatalf("ingestFolder: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want only the .txt processed (image skipped, no error row), got %+v", results)
	}
	if !strings.HasSuffix(results[0].SourceName, "notiz.txt") {
		t.Fatalf("unexpected processed file: %+v", results[0])
	}
}
