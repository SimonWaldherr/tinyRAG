package main

import (
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Outbound SMTP — the first mail-sending code in R3. Used only by the chat
// "send this answer to me" feature (handlers.go's handleChatEmail); never by
// draft.go's HITL draft-reply path, which stays send-free by design (see
// README's "Human-in-the-loop by design"). Recipients are never user-
// supplied free text — see smtpConfig.From's doc comment in settings.go.
//
// Uses only net/smtp (stdlib) rather than a library like gomail.v2 — no new
// dependency, matching the house style of most connectors (confluence.go,
// jira.go, freshservice.go) using only net/http from the stdlib.
// smtp.SendMail negotiates STARTTLS with the server's default (verifying)
// tls.Config automatically when the server advertises it; unlike
// external/sendmail_test's reference config, InsecureSkipVerify is
// deliberately not offered here.
// ─────────────────────────────────────────────────────────────────────────────

// smtpResolvedPassword prefers PasswordEnv over the inline Password, same
// pattern as every other connector's credential field (e.g.
// jiraResolvedToken, mssqlConfig's resolvedPassword).
func smtpResolvedPassword(cfg smtpConfig) string {
	return resolveSecret(cfg.Password, cfg.PasswordEnv)
}

// buildPlainTextEmail renders a minimal RFC 5322 message: headers, a blank
// line, then the plain-text body. A pure function so it's testable without
// a network round trip. Subject is RFC 2047-encoded since it commonly
// contains non-ASCII (German umlauts in the asked question).
func buildPlainTextEmail(from, to, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("\r\n")
	// CRLF line endings per RFC 5322; body may already use \n only.
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}

// buildMultipartEmail renders a multipart/mixed RFC 5322 message: the
// same headers as buildPlainTextEmail, then a text/plain body part
// followed by one part per attachment — the outbound mirror of
// extractMailAttachments' reading side (extract.go), reusing its
// mailAttachment{Filename, Data} type directly so the two directions
// share one shape. Only called when there's at least one attachment;
// buildPlainTextEmail stays the unchanged single-part path otherwise.
func buildMultipartEmail(from, to, subject, body string, attachments []mailAttachment) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	b.WriteString("MIME-Version: 1.0\r\n")

	mw := multipart.NewWriter(&b)
	// The blank line here ends the top-level headers — mw.CreatePart
	// below writes the leading "--boundary" marker itself, but never a
	// preceding blank line, so this one is on us.
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", mw.Boundary())

	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", `text/plain; charset="UTF-8"`)
	if textPart, err := mw.CreatePart(textHeader); err == nil {
		textPart.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	}

	for _, att := range attachments {
		attHeader := textproto.MIMEHeader{}
		ct := mime.TypeByExtension(filepath.Ext(att.Filename))
		if ct == "" {
			ct = "application/octet-stream"
		}
		attHeader.Set("Content-Type", ct)
		attHeader.Set("Content-Transfer-Encoding", "base64")
		attHeader.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, att.Filename))
		part, err := mw.CreatePart(attHeader)
		if err != nil {
			continue
		}
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(att.Data)))
		base64.StdEncoding.Encode(encoded, att.Data)
		// Wrap at 76 chars per RFC 2045 — most MIME parsers tolerate
		// unwrapped base64, but not all, and it's a one-line loop to do
		// properly.
		for i := 0; i < len(encoded); i += 76 {
			end := i + 76
			if end > len(encoded) {
				end = len(encoded)
			}
			part.Write(encoded[i:end])
			part.Write([]byte("\r\n"))
		}
	}
	mw.Close()
	return []byte(b.String())
}

// mailAttachmentInput is one attachment as it arrives over JSON (Mail
// tab requests) — base64 bytes, same convention as chatimages.go's
// askImageInput. mailAttachmentMaxCount/mailAttachmentMaxBytes are more
// generous than the chat image limits since these never go anywhere near
// an LLM (no vision-payload cost/latency concern), just an outgoing
// email.
type mailAttachmentInput struct {
	Filename   string `json:"filename"`
	DataBase64 string `json:"data_base64"`
}

const (
	mailAttachmentMaxCount = 5
	mailAttachmentMaxBytes = 15 * 1024 * 1024 // 15 MB per attachment
)

// decodeMailAttachments base64-decodes and validates every attachment a
// Mail-tab request carries, enforcing mailAttachmentMaxCount/
// mailAttachmentMaxBytes — same "fail with a clear error rather than
// silently drop/truncate" contract as chatimages.go's decodeAskImages.
func decodeMailAttachments(in []mailAttachmentInput) ([]mailAttachment, error) {
	if len(in) > mailAttachmentMaxCount {
		return nil, fmt.Errorf("zu viele Anhänge (max. %d)", mailAttachmentMaxCount)
	}
	out := make([]mailAttachment, 0, len(in))
	for _, a := range in {
		data, err := base64.StdEncoding.DecodeString(a.DataBase64)
		if err != nil {
			return nil, fmt.Errorf("Anhang %q: ungültige Daten: %w", a.Filename, err)
		}
		if len(data) == 0 {
			continue
		}
		if len(data) > mailAttachmentMaxBytes {
			return nil, fmt.Errorf("Anhang %q: zu groß (%.1f MB, Limit %d MB)", a.Filename, float64(len(data))/(1024*1024), mailAttachmentMaxBytes/(1024*1024))
		}
		filename := strings.TrimSpace(a.Filename)
		if filename == "" {
			filename = "anhang.bin"
		}
		out = append(out, mailAttachment{Filename: filename, Data: data})
	}
	return out, nil
}

// sendMail sends a plain-text email (or, if attachments is non-empty, a
// multipart/mixed one) via cfg's SMTP relay. Validates cfg up front so a
// misconfigured/disabled SMTP block fails with a clear error rather than
// a confusing dial/nil-auth failure deeper in net/smtp.
func sendMail(cfg smtpConfig, to, subject, body string, attachments ...mailAttachment) error {
	if !cfg.Enabled {
		return fmt.Errorf("smtp: not enabled")
	}
	if cfg.Host == "" {
		return fmt.Errorf("smtp: host not configured")
	}
	if cfg.From == "" {
		return fmt.Errorf("smtp: from address not configured")
	}
	if to == "" {
		return fmt.Errorf("smtp: recipient address is empty")
	}

	port := cfg.Port
	if port <= 0 {
		port = 25
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)

	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, smtpResolvedPassword(cfg), cfg.Host)
	}

	var msg []byte
	if len(attachments) > 0 {
		msg = buildMultipartEmail(cfg.From, to, subject, body, attachments)
	} else {
		msg = buildPlainTextEmail(cfg.From, to, subject, body)
	}
	if err := smtp.SendMail(addr, auth, cfg.From, []string{to}, msg); err != nil {
		// The stdlib's own message ("smtp: server doesn't support AUTH") is
		// accurate but not actionable on its own — this specific failure
		// only happens when auth != nil (a username was configured) and the
		// server's EHLO response didn't advertise AUTH at all, which is the
		// normal shape of an internal relay that only allows unauthenticated
		// relaying from trusted IPs. Point at the fix directly rather than
		// making an admin go look up what "doesn't support AUTH" implies.
		if auth != nil && strings.Contains(err.Error(), "doesn't support AUTH") {
			return fmt.Errorf("%w (der Server verlangt offenbar keine Anmeldung — Feld \"Benutzername\" in den SMTP-Einstellungen leer lassen, falls das Relay unauthentifiziert arbeitet)", err)
		}
		return err
	}
	return nil
}
