package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// ─────────────────────────────────────────────────────────────────────────────
// realIMAPClient: the imapClient implementation (imap.go) backed by
// github.com/emersion/go-imap/v2. One connection per ListNewMessages call —
// this is a manual "import now" action (or an infrequent external
// scheduler hit), not a long-lived IDLE connection, so there's no
// reconnect/keepalive logic to maintain.
// ─────────────────────────────────────────────────────────────────────────────

type realIMAPClient struct {
	cfg mailboxConfig
}

// newIMAPClient constructs the imapClient implementation for cfg; dialing
// and login are deferred to dial(), called fresh for every ListNewMessages.
func newIMAPClient(cfg mailboxConfig) *realIMAPClient {
	return &realIMAPClient{cfg: cfg}
}

// dial opens a fresh connection and logs in, defaulting to port 993 and
// leaving TLS choice (DialTLS vs DialInsecure) to cfg.UseTLS — the caller
// owns closing the returned client.
func (c *realIMAPClient) dial() (*imapclient.Client, error) {
	if c.cfg.Host == "" {
		return nil, fmt.Errorf("imap: host not configured")
	}
	port := c.cfg.Port
	if port == 0 {
		port = 993
	}
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, port)

	var cl *imapclient.Client
	var err error
	if c.cfg.UseTLS {
		cl, err = imapclient.DialTLS(addr, nil)
	} else {
		cl, err = imapclient.DialInsecure(addr, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w%s", addr, err, imapErrorHint(c.cfg, port, err))
	}

	pass := c.cfg.resolvedPassword()
	if c.cfg.Username == "" || pass == "" {
		cl.Close()
		return nil, fmt.Errorf("imap: username/password not configured (set password or password_env)")
	}
	if err := cl.Login(c.cfg.Username, pass).Wait(); err != nil {
		cl.Close()
		return nil, fmt.Errorf("imap: login failed: %w%s", err, imapErrorHint(c.cfg, port, err))
	}
	return cl, nil
}

// imapErrorHint turns a raw net/imap error into an actionable, German
// suffix for the admin-facing "Verbindung testen" result (and the same
// log line during a real ListNewMessages poll) — a bare "unexpected EOF"
// or "i/o timeout" means nothing to someone who didn't already know the
// IMAP/TLS handshake internals well enough to guess the fix. Returns ""
// when no known pattern matches, so the original error message stands
// alone rather than getting a useless "keine weiteren Hinweise" tacked on.
func imapErrorHint(cfg mailboxConfig, port int, err error) string {
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "i/o timeout") || strings.Contains(lower, "deadline exceeded"):
		return fmt.Sprintf(" (Zeitüberschreitung beim Verbindungsaufbau zu %s:%d — meist eine Firewall/ein Proxy, der ausgehende Verbindungen zu diesem Host/Port blockiert. Bei einem externen Server wie Office 365 typischerweise die eigene Firewall; bei einem internen Server ggf. falscher Port.)", cfg.Host, port)
	case strings.Contains(lower, "connection refused"):
		return fmt.Sprintf(" (Verbindung zu %s:%d abgelehnt — der Host ist erreichbar, aber auf diesem Port läuft kein IMAP-Dienst. Portnummer prüfen: 993 für IMAPS/implicit TLS, 143 für IMAP mit STARTTLS.)", cfg.Host, port)
	case strings.Contains(lower, "no such host") || strings.Contains(lower, "server misbehaving"):
		return fmt.Sprintf(" (Hostname %q konnte nicht aufgelöst werden — DNS-Eintrag prüfen bzw. Tippfehler im Servernamen ausschließen.)", cfg.Host)
	case strings.Contains(lower, "unexpected eof") && !cfg.UseTLS && port == 993:
		return fmt.Sprintf(" (Port %d ist der Standardport für IMAPS mit \"implicit TLS\" — TLS wird sofort beim Verbindungsaufbau erwartet. \"TLS verwenden\" ist hier aber deaktiviert, R3 hat also im Klartext gesprochen; der Server hat die Verbindung deshalb sofort getrennt. Bitte \"TLS verwenden\" aktivieren.)", port)
	case strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "connection reset"):
		return " (Die Verbindung wurde unerwartet beendet, bevor eine vollständige Antwort kam — häufig ein TLS-Mismatch zwischen Port und \"TLS verwenden\"-Einstellung, oder eine Firewall/ein Proxy, der die Verbindung nach dem TCP-Handshake wieder kappt.)"
	case strings.Contains(lower, "certificate") || strings.Contains(lower, "x509"):
		return " (TLS-Zertifikatsproblem — das Serverzertifikat wird nicht als vertrauenswürdig erkannt, z. B. selbstsigniert oder von einer internen CA, die diesem Rechner nicht bekannt ist.)"
	case strings.Contains(lower, "authenticationfailed") || strings.Contains(lower, "invalid credentials") || strings.Contains(lower, "login failed"):
		return " (Zugangsdaten vom Server abgelehnt — Benutzername/Passwort prüfen; bei Office 365 ggf. App-Passwort oder moderne Authentifizierung statt Basic-Auth nötig.)"
	default:
		return ""
	}
}

// ListNewMessages fetches every message with UID > sinceUID from the
// configured mailbox, read-only (BodySection.Peek leaves \Seen untouched).
func (c *realIMAPClient) ListNewMessages(ctx context.Context, sinceUID uint32) ([]incomingMail, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cl, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	defer cl.Logout()

	mailbox := c.cfg.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if _, err := cl.Select(mailbox, nil).Wait(); err != nil {
		return nil, fmt.Errorf("imap: select %q: %w", mailbox, err)
	}

	var uidRange imap.UIDSet
	uidRange.AddRange(imap.UID(sinceUID+1), 0) // stop=0 means open-ended ("*")
	searchData, err := cl.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{uidRange}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: search: %w", err)
	}
	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}
	// Per-run cap (import_limits.go): a first import with LastUID=0 would
	// otherwise FETCH an entire multi-year mailbox in one shot. UIDs come
	// back ascending, so keeping the LOWEST N means importIMAPMessages
	// advances LastUID by exactly this batch and the next run/scheduler
	// tick resumes at the next-higher UID — a big backlog drains in
	// bounded chunks, nothing is skipped.
	maxItems := c.cfg.effectiveMaxItems(settings.get().Import)
	if len(uids) > maxItems {
		log.Printf("imap: %d neue Nachrichten gefunden, in diesem Lauf auf %d begrenzt (Import-Limit) — Rest folgt beim nächsten Lauf", len(uids), maxItems)
		uids = uids[:maxItems]
	}

	var fetchSet imap.UIDSet
	fetchSet.AddNum(uids...)

	fetchOptions := &imap.FetchOptions{
		UID:         true,
		Envelope:    true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}
	cmd := cl.Fetch(fetchSet, fetchOptions)

	var out []incomingMail
	var fetchErrs []string
	for {
		if err := ctx.Err(); err != nil {
			cmd.Close()
			return out, err
		}
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			fetchErrs = append(fetchErrs, err.Error())
			continue
		}
		raw := buf.FindBodySection(&imap.FetchItemBodySection{})
		fields, err := emlToFields(raw)
		if err != nil {
			// Fall back to the envelope alone rather than dropping the
			// message outright — a malformed body shouldn't lose the
			// message's existence/subject from the index.
			fields = emailFields{}
			fetchErrs = append(fetchErrs, fmt.Sprintf("uid %d: parse body: %v", buf.UID, err))
		}
		if buf.Envelope != nil {
			if strings.TrimSpace(fields.Subject) == "" {
				fields.Subject = buf.Envelope.Subject
			}
			if fields.Date.IsZero() {
				fields.Date = buf.Envelope.Date
			}
			if strings.TrimSpace(fields.From) == "" && len(buf.Envelope.From) > 0 {
				fields.From = formatAddress(buf.Envelope.From[0].Name, buf.Envelope.From[0].Addr())
			}
		}
		fields.Subject = repairMojibake(fields.Subject)
		fields.From = repairMojibake(fields.From)
		fields.To = repairMojibake(fields.To)
		fields.Body = repairMojibake(fields.Body)

		// Attachment extraction is best-effort and reuses the same raw
		// bytes already fetched for the body — a malformed/unusual
		// multipart structure just means no attachments for this message,
		// not a reason to drop the message itself. The error/warnings are
		// still kept so importIMAPMessages can surface them instead of
		// vanishing unseen.
		attachments, attWarnings, attErr := extractMailAttachments(raw, emailAttachmentMaxBytes(settings.get()))
		var attErrText string
		if attErr != nil {
			attErrText = attErr.Error()
		}

		out = append(out, incomingMail{
			Account:            c.cfg.Username,
			Mailbox:            mailbox,
			UID:                uint32(buf.UID),
			Fields:             fields,
			ReceivedAt:         fields.Date,
			Attachments:        attachments,
			AttachmentError:    attErrText,
			AttachmentWarnings: attWarnings,
		})
	}
	if err := cmd.Close(); err != nil {
		return out, fmt.Errorf("imap: fetch: %w", err)
	}
	if len(fetchErrs) > 0 && len(out) == 0 {
		return nil, fmt.Errorf("imap: all %d message(s) failed: %s", len(fetchErrs), strings.Join(fetchErrs, "; "))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// AppendDraft IMAP-APPENDs a fully built RFC 5322 message into mailbox
// with the \Draft flag set — the one deliberate write R3 performs against
// the account (see saveDraftToMailbox in draft.go for the policy side:
// drafts only, never a send). A fresh dial per call, same as
// ListNewMessages: this is a manual, occasional human action, not a hot
// path worth a persistent connection.
func (c *realIMAPClient) AppendDraft(mailbox string, msg []byte) error {
	cl, err := c.dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	defer cl.Logout()

	cmd := cl.Append(mailbox, int64(len(msg)), &imap.AppendOptions{
		Flags: []imap.Flag{imap.FlagDraft},
		Time:  time.Now(),
	})
	if _, err := cmd.Write(msg); err != nil {
		cmd.Close()
		return fmt.Errorf("imap: append to %q: write: %w", mailbox, err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("imap: append to %q: %w", mailbox, err)
	}
	if _, err := cmd.Wait(); err != nil {
		return fmt.Errorf("imap: append to %q: %w", mailbox, err)
	}
	return nil
}

// imapImportResult/imapProgress mirror pst.go's pstImportResult/pstProgress
// — same streaming-progress shape used by handlers.go's NDJSON endpoint.
type imapImportResult struct {
	baseImportResult
	mailAttachmentWarnings
	Messages    int    `json:"messages"`
	Attachments int    `json:"attachments"`
	LastUID     uint32 `json:"last_uid"`
}

type imapProgress struct {
	Result  imapImportResult
	Subject string
}

// importIMAPMessages fetches everything new since cfg.LastUID and ingests
// it, returning the new high-water-mark UID so the caller can persist it
// back into settings — mirrors importExchangeMail (graphmail.go) but
// without a preview/select step, since IMAP UIDs are already deduplicated
// via LastUID (unlike Graph mail, where the folder can hold years of
// history and a "preview newest 50, pick some" UX makes more sense).
func importIMAPMessages(ctx context.Context, client imapClient, rag *ragSystem, s appSettings, embedModel string, cfg mailboxConfig, dryRun bool, onProgress func(imapProgress)) (imapImportResult, error) {
	var res imapImportResult
	res.LastUID = cfg.LastUID
	res.DryRun = dryRun

	messages, err := client.ListNewMessages(ctx, cfg.LastUID)
	if err != nil {
		return res, err
	}
	if verbose {
		log.Printf("[verbose] imap import: account=%s mailbox=%s since_uid=%d fetched=%d dry_run=%v", cfg.Username, cfg.Mailbox, cfg.LastUID, len(messages), dryRun)
	}

	mailbox := cfg.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}

	for _, m := range messages {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Messages++
		docDate := unixOrZero(m.Fields.Date)
		sourceID := fmt.Sprintf("imap:%s:%s:%d", cfg.Username, mailbox, m.UID)
		sourceName := formatSourceName(m.Fields.Subject, m.Fields.From)

		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "imap_email", sourceName, m.Fields.String(), docDate, dryRun)
		foldIngestOutcome(outcome, err, &res.Errors, &res.Skipped, &res.Chunks)

		if m.AttachmentError != "" {
			res.AttachmentWarnings = append(res.AttachmentWarnings, fmt.Sprintf("%s: Anhänge konnten nicht gelesen werden: %s", sourceName, m.AttachmentError))
		}
		for _, w := range m.AttachmentWarnings {
			res.Skipped++
			res.AttachmentWarnings = append(res.AttachmentWarnings, fmt.Sprintf("%s — Anhang: %s", sourceName, w))
		}
		for ai, att := range m.Attachments {
			attOutcome, attErr := ingestEmailAttachment(rag, s, embedModel, sourceID, ai, "imap_attachment", att.Filename, att.Data, m.Fields.Subject, m.Fields.From, docDate, dryRun)
			foldAttachmentOutcome(attOutcome, attErr, &res.Attachments, &res.Skipped, &res.Chunks, &res.AttachmentWarnings)
		}

		if m.UID > res.LastUID {
			res.LastUID = m.UID
		}
		if onProgress != nil {
			onProgress(imapProgress{Result: res, Subject: m.Fields.Subject})
		}
	}
	return res, nil
}
