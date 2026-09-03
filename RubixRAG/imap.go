package main

import (
	"context"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// On-prem Exchange / generic IMAP mailbox access — the non-Microsoft-365
// path for "Outlook/Exchange" (see graphmail.go for the Exchange Online
// path). Read-only: messages are fetched with Peek so \Seen is never set,
// and nothing is ever deleted or moved. Real connection logic lives in
// imapmail.go; this file only pins down the config/interface shape so
// handlers.go can depend on imapClient without caring which library backs
// it.
//
//   - auth: LOGIN (plain username/password) — most on-prem Exchange/Dovecot
//     setups. XOAUTH2 (Microsoft 365 mailboxes reachable via IMAP) is not
//     implemented; use graphmail.go for Microsoft 365 instead.
//   - polling: no IMAP IDLE — a manual "Import jetzt" (or an external
//     scheduler hitting the same endpoint) triggers ListNewMessages, which
//     tracks the highest seen UID per mailbox (LastUID) to stay incremental.
//   - every fetched message produces a source_id of the form
//     "imap:<account>:<mailbox>:<uid>" so it participates in the same
//     replace-on-update semantics as everything else in store.go
// ─────────────────────────────────────────────────────────────────────────────

// mailboxConfig configures per-account IMAP access, wired into
// appSettings/imapmail.go.
type mailboxConfig struct {
	connRuntime
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// Password/PasswordEnv follow the same pattern as
	// sharePointConfig.ClientSecret/ClientSecretEnv — PasswordEnv (an env
	// var *name*) is preferred over inlining the password in settings.json.
	Password     string `json:"password,omitempty"`
	PasswordEnv  string `json:"password_env,omitempty"`
	OAuthToken   string `json:"oauth_token,omitempty"`
	Mailbox      string `json:"mailbox"` // e.g. "INBOX"
	UseTLS       bool   `json:"use_tls"`
	PollInterval int    `json:"poll_interval_seconds"`
	// DraftsMailbox is where saveDraftToMailbox (draft.go) IMAP-APPENDs
	// reviewed drafts — empty means the near-universal default "Drafts"
	// (see draftsMailboxOrDefault). Only relevant once the Mail tab's
	// "In Postfach-Entwürfe speichern" action is used; plain importing
	// never writes to the mailbox at all.
	DraftsMailbox string `json:"drafts_mailbox,omitempty"`
	// LastUID is the highest IMAP UID already imported, persisted here so
	// repeated "Import jetzt" clicks (imapmail.go) are incremental rather
	// than re-fetching the whole mailbox every time — updated after each
	// successful import run.
	LastUID uint32 `json:"last_uid"`
}

func (c mailboxConfig) isEnabled() bool { return c.Enabled }

// draftsMailboxOrDefault resolves the drafts folder name, defaulting to
// "Drafts" — the IMAP special-use name virtually every server exposes
// (localized display names like "Entwürfe" are a client-side rendering of
// the same folder, not its wire name).
func draftsMailboxOrDefault(c mailboxConfig) string {
	if strings.TrimSpace(c.DraftsMailbox) != "" {
		return c.DraftsMailbox
	}
	return "Drafts"
}

// resolvedPassword prefers the env-var-named password over the inline one,
// so a deployment can avoid committing the mailbox password to settings.json.
func (c mailboxConfig) resolvedPassword() string {
	return resolveSecret(c.Password, c.PasswordEnv)
}

// incomingMail is the normalized shape an IMAP fetch should produce, mirroring
// emailFields (extract.go) plus the identifiers needed for idempotent ingest
// and for later HITL draft placement.
type incomingMail struct {
	Account     string
	Mailbox     string
	UID         uint32
	Fields      emailFields
	ReceivedAt  time.Time
	Attachments []mailAttachment
	// AttachmentError is set when extractMailAttachments failed on this
	// message's raw bytes — the message itself is still imported (a
	// malformed/unusual multipart structure isn't a reason to drop it), but
	// importIMAPMessages surfaces this into AttachmentWarnings rather than
	// swallowing it silently, since "we couldn't even look for attachments"
	// is a different, more actionable problem than "there were none".
	AttachmentError string
	// AttachmentWarnings carries per-attachment skip reasons from
	// extractMailAttachments (currently: over the configured size limit) —
	// distinct from AttachmentError, which is a whole-message parse failure.
	AttachmentWarnings []string
}

// imapClient is the interface handlers.go depends on, so it never needs to
// know which IMAP library backs it. realIMAPClient (imapmail.go) is the
// only implementation, backed by github.com/emersion/go-imap/v2.
type imapClient interface {
	// ListNewMessages returns messages with UID > sinceUID in the
	// configured mailbox, read-only (no flags/deletions are written). ctx
	// is checked between messages while decoding the already-fetched
	// response, not mid-flight during the underlying blocking IMAP wire
	// calls (Select/UIDSearch/Fetch) — the emersion/go-imap/v2 client
	// doesn't expose per-command cancellation, so a slow/stuck server
	// still can't be aborted mid-round-trip; this only stops early once
	// control returns to Go code.
	ListNewMessages(ctx context.Context, sinceUID uint32) ([]incomingMail, error)
}
