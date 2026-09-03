package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestExtractMBOX(t *testing.T) {
	raw := []byte("From a@example.com Mon Jan  1 00:00:00 2024\n" +
		"From: a@example.com\nSubject: First\nContent-Type: text/plain\n\nOne\n" +
		"From b@example.com Tue Jan  2 00:00:00 2024\n" +
		"From: b@example.com\nSubject: Second\nContent-Type: text/plain\n\nTwo\n")
	text, err := extractMBOX(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Subject: First") || !strings.Contains(text, "Subject: Second") || !strings.Contains(text, "One") || !strings.Contains(text, "Two") {
		t.Fatalf("mbox content missing: %q", text)
	}
}

func TestExtractMHTMLDecodesHTMLPart(t *testing.T) {
	raw := []byte("From: archive@example.com\r\n" +
		"Content-Type: multipart/related; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"PGh0bWw+PGJvZHk+QXJjaGl2ZWQ8L2JvZHk+PC9odG1sPg==\r\n" +
		"--BOUND--\r\n")
	text, err := extractMHTML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Archived" {
		t.Fatalf("want decoded HTML text, got %q", text)
	}
}

func TestExtractVCardsAndCalendar(t *testing.T) {
	vcf := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Erika Beispiel\r\nEMAIL;TYPE=work:erika@example.com\r\nNOTE:Zeile eins\\nZeile zwei\r\nEND:VCARD\r\n")
	contact, err := extractVCards(vcf)
	if err != nil || !strings.Contains(contact, "Erika Beispiel") || !strings.Contains(contact, "Zeile zwei") {
		t.Fatalf("vcard: %q / %v", contact, err)
	}
	ics := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Planung\r\nDESCRIPTION:Besprechung\\nRaum 2\r\nDTSTART:20260719T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	calendar, err := extractCalendar(ics)
	if err != nil || !strings.Contains(calendar, "Planung") || !strings.Contains(calendar, "Raum 2") {
		t.Fatalf("ics: %q / %v", calendar, err)
	}
}

func TestExtractSubtitles(t *testing.T) {
	srt := []byte("1\r\n00:00:01,000 --> 00:00:02,000\r\nHallo <i>Welt</i>\r\n\r\n")
	if got := extractSubtitles(srt, ".srt"); got != "Hallo Welt" {
		t.Fatalf("subtitle text: %q", got)
	}
}

func TestP0P1ExtensionsAreRecognized(t *testing.T) {
	for _, name := range []string{"data.jsonl", "export.ndjson", "table.tsv", "mail.mbox", "page.mhtml", "contact.vcf", "calendar.ics", "captions.vtt", "slides.docm", "book.xlsm", "meeting.pptm", "archive.tar.gz", "archive.7z", "recording.mp3", "video.mp4"} {
		ext := fileExtension(name)
		if !isExtractableDocument(ext) {
			t.Errorf("%s (%s) is not recognized as extractable", name, ext)
		}
	}
	if got := fileExtension("bundle.tar.gz"); got != ".tar.gz" {
		t.Fatalf("compound extension: got %q", got)
	}
}

func TestMarkItDownDocIntelEndpointValidation(t *testing.T) {
	if err := validateImportSettings(importConfig{MarkItDownDocIntelEndpoint: "http://intel.example"}); err == nil {
		t.Fatal("want non-HTTPS Document Intelligence endpoint rejected")
	}
	if err := validateImportSettings(importConfig{MarkItDownDocIntelEndpoint: "https://intel.example"}); err != nil {
		t.Fatalf("want HTTPS endpoint accepted: %v", err)
	}
}

func TestExtractZipArchiveRejectsTraversalAndReadsText(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"../escape.txt": "no", "docs/readme.txt": "yes"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := extractZipArchive(buf.Bytes(), appSettings{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "no") || !strings.Contains(text, "yes") {
		t.Fatalf("unexpected archive text: %q", text)
	}
}

func TestExtractTarGzipArchive(t *testing.T) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("inside tar")
	if err := tw.WriteHeader(&tar.Header{Name: "note.txt", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := extractArchive("archive.tgz", gzBuf.Bytes(), ".tgz", appSettings{})
	if err != nil || !strings.Contains(text, "inside tar") {
		t.Fatalf("tar.gz: %q / %v", text, err)
	}
}
