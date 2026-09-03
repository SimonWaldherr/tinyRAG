package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildPlainTextEmail(t *testing.T) {
	msg := string(buildPlainTextEmail("r3@rubix.com", "user@rubix.com", "Test-Betreff äöü", "Zeile 1\nZeile 2"))

	for _, want := range []string{
		"From: r3@rubix.com\r\n",
		"To: user@rubix.com\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=\"UTF-8\"\r\n",
		"Zeile 1\r\nZeile 2",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("want message to contain %q, got:\n%s", want, msg)
		}
	}
	// Subject must be RFC 2047-encoded since it contains non-ASCII.
	if strings.Contains(msg, "Subject: Test-Betreff äöü") {
		t.Errorf("want Subject to be RFC 2047-encoded, got raw UTF-8:\n%s", msg)
	}
	if !strings.Contains(msg, "Subject: =?UTF-8?") {
		t.Errorf("want RFC 2047-encoded Subject header, got:\n%s", msg)
	}
	if !strings.Contains(msg, "\r\n\r\n") {
		t.Errorf("want a blank line separating headers from body, got:\n%s", msg)
	}
}

// TestBuildMultipartEmailRoundTripsWithExtractMailAttachments closes the
// loop between the outbound writer (buildMultipartEmail, mail.go) and the
// existing inbound reader (extractMailAttachments, extract.go) — proves
// an attachment written out can be read back byte-identical, without
// needing a real mail server anywhere.
func TestBuildMultipartEmailRoundTripsWithExtractMailAttachments(t *testing.T) {
	original := []byte("%PDF-1.4 pretend this is a scanned document\x00\x01\x02\xff")
	msg := buildMultipartEmail("r3@rubix.com", "user@rubix.com", "Scan im Anhang äöü", "Siehe Anhang.", []mailAttachment{
		{Filename: "scan.pdf", Data: original},
	})

	if !strings.Contains(string(msg), "multipart/mixed") {
		t.Fatalf("want a multipart/mixed message, got:\n%s", msg)
	}

	atts, _, err := extractMailAttachments(msg, 0)
	if err != nil {
		t.Fatalf("extractMailAttachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment read back, got %d", len(atts))
	}
	if atts[0].Filename != "scan.pdf" {
		t.Errorf("want filename %q, got %q", "scan.pdf", atts[0].Filename)
	}
	if !bytes.Equal(atts[0].Data, original) {
		t.Errorf("attachment bytes don't round-trip:\nwant %v\ngot  %v", original, atts[0].Data)
	}

	// The text body must still be readable independently of the
	// attachment part — readMailBody is exercised via emlToFields.
	fields, err := emlToFields(msg)
	if err != nil {
		t.Fatalf("emlToFields: %v", err)
	}
	if !strings.Contains(fields.Body, "Siehe Anhang.") {
		t.Errorf("want the text body preserved, got %q", fields.Body)
	}
}

func TestBuildMultipartEmailNoAttachmentsStillParses(t *testing.T) {
	msg := buildMultipartEmail("r3@rubix.com", "user@rubix.com", "Ohne Anhang", "Nur Text.", nil)
	atts, _, err := extractMailAttachments(msg, 0)
	if err != nil {
		t.Fatalf("extractMailAttachments: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("want 0 attachments, got %d", len(atts))
	}
}

func TestSendMailValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  smtpConfig
		to   string
	}{
		{"disabled", smtpConfig{Host: "smtp.example.com", From: "r3@rubix.com"}, "user@rubix.com"},
		{"missing host", smtpConfig{Enabled: true, From: "r3@rubix.com"}, "user@rubix.com"},
		{"missing from", smtpConfig{Enabled: true, Host: "smtp.example.com"}, "user@rubix.com"},
		{"missing recipient", smtpConfig{Enabled: true, Host: "smtp.example.com", From: "r3@rubix.com"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := sendMail(c.cfg, c.to, "subject", "body"); err == nil {
				t.Fatalf("want validation error, got nil")
			}
		})
	}
}
