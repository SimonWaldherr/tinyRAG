package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Interactive, per-user Exchange mailbox access for the Mail tab — distinct
// from graphmail.go's import/auto-draft path, which always reads/writes the
// ONE admin-fixed Mailbox. Here, an authorized, logged-in user (LDAP
// required — there's no "whose mailbox" without a real identity) browses
// and drafts replies to ONE OR MORE mailboxes they're authorized for,
// through the SAME app-only Graph credentials an exchangeGraphConfig
// connection already has (Graph app-only auth can act on any mailbox the
// app registration is granted — no per-user OAuth consent needed, and no
// new Graph permission beyond what import already required). Each
// InteractiveEnabled connection contributes exactly one selectable mailbox
// option: either the caller's OWN mailbox (sessionClaims.Mail, the
// original and still-default mode) or, if that connection sets
// InteractiveShared, a shared/team mailbox used exactly as the admin
// configured it (e.g. "vertrieb@rubix.com") — see
// findInteractiveExchangeOptions. Authorization is per connection:
// exchangeGraphConfig.InteractiveEnabled + AllowedUsers/AllowedGroups
// (settings.go).
//
// A user with no authorized option simply never sees this panel in the
// Mail tab — the pre-existing manual copy-paste workflow keeps working
// completely unaffected; this is a purely additive capability, and
// deliberately reuses /api/draft/reply's existing raw_email path for
// generation (see mailGraphMessageResponse.RawEmail) rather than adding a
// second draft-generation code path.
//
// Authorization also honors AllowedGroups (AD memberOf, via
// sessionClaims.Groups/ldapIsMemberOf) alongside AllowedUsers — either
// matching is enough; both empty still means nobody is authorized, the
// same deliberate deny-by-default posture AllowedUsers alone had before
// AllowedGroups existed.
//
// Folder scope: every handler below also accepts an optional folder
// override (well-known Graph name or a folder ID from
// /api/mail/graph/folders' tree) on top of the connection's own configured
// default — see handleMailGraphFolders/handleMailGraphList.
// ─────────────────────────────────────────────────────────────────────────────

// interactiveMailboxOption is one mailbox the caller may pick in the Mail
// tab's panel — the wire-facing half of findInteractiveExchangeOptions'
// result (interactiveExchangeOption below adds the resolved config,
// deliberately never serialized itself so a raw connection name/mailbox
// beyond what Label already shows never round-trips to the client as
// structured data).
type interactiveMailboxOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// interactiveExchangeOption pairs one wire-facing option with the resolved
// exchangeGraphConfig it represents (Mailbox already set appropriately) —
// internal only.
type interactiveExchangeOption struct {
	interactiveMailboxOption
	Conn exchangeGraphConfig
}

// findInteractiveExchangeOptions returns every mailbox the caller may
// browse in the Mail tab's native panel, in settings order: each
// InteractiveEnabled connection the caller is authorized for (AllowedUsers
// or AllowedGroups matching claims) contributes exactly one option —
// either "their own mailbox" (InteractiveShared=false, the original and
// still-default mode, Mailbox overridden to claims.Mail) or "this
// connection's own configured shared/team mailbox" (InteractiveShared=true,
// Mailbox used as-is, e.g. a team inbox like "vertrieb@rubix.com") — never
// both from the same connection; an admin wanting to offer a user both
// configures two separate connections. Key is derived from the
// connection's own Name (validateExchangeGraphConnections already requires
// uniqueness) so resolveInteractiveExchangeOption can re-derive the exact
// same option and re-check authorization from scratch on every call — the
// key itself carries no trust, it's just a lookup handle.
func findInteractiveExchangeOptions(s appSettings, claims sessionClaims) []interactiveExchangeOption {
	var opts []interactiveExchangeOption
	for _, conn := range s.ExchangeGraph {
		if !conn.Enabled || !conn.InteractiveEnabled {
			continue
		}
		authorized := ldapMatchesAdminUser(conn.AllowedUsers, claims.Mail, claims.User)
		if !authorized {
			for _, g := range conn.AllowedGroups {
				if g != "" && ldapIsMemberOf(claims.Groups, g) {
					authorized = true
					break
				}
			}
		}
		if !authorized {
			continue
		}
		if conn.InteractiveShared {
			mailbox := strings.TrimSpace(conn.Mailbox)
			if mailbox == "" {
				continue
			}
			opts = append(opts, interactiveExchangeOption{
				interactiveMailboxOption{Key: "shared:" + conn.Name, Label: mailbox},
				conn,
			})
			continue
		}
		mailbox := strings.TrimSpace(claims.Mail)
		if mailbox == "" {
			continue
		}
		conn.Mailbox = mailbox
		opts = append(opts, interactiveExchangeOption{
			interactiveMailboxOption{Key: "own:" + conn.Name, Label: "Mein Postfach (" + mailbox + ")"},
			conn,
		})
	}
	return opts
}

// resolveInteractiveExchangeOption re-derives the caller's options from
// scratch (findInteractiveExchangeOptions) and returns the one matching
// key — the ONLY path from a request's mailbox_key to an
// exchangeGraphConfig, so a stale, forged, or no-longer-authorized key can
// never resolve regardless of what a client sends. key == "" resolves to
// the first available option — backward compat for a not-yet-refreshed
// cached frontend that predates mailbox selection and never sends a key at
// all, preserving its previous single-mailbox behavior.
func resolveInteractiveExchangeOption(s appSettings, claims sessionClaims, key string) (exchangeGraphConfig, bool) {
	opts := findInteractiveExchangeOptions(s, claims)
	if len(opts) == 0 {
		return exchangeGraphConfig{}, false
	}
	if key == "" {
		return opts[0].Conn, true
	}
	for _, o := range opts {
		if o.Key == key {
			return o.Conn, true
		}
	}
	return exchangeGraphConfig{}, false
}

// mailGraphAvailable reports whether the current request's caller may use
// the interactive mailbox panel at all — the single check handleAuthStatus
// surfaces to the frontend (as mail_graph_available) so the Mail tab knows
// whether to render it. false for every anonymous/unauthorized caller,
// exactly like every other feature flag in that response.
func mailGraphAvailable(s appSettings, r *http.Request) bool {
	claims, ok := currentSession(r)
	if !ok {
		return false
	}
	return len(findInteractiveExchangeOptions(s, claims)) > 0
}

// handleMailGraphOptions serves GET /api/mail/graph/options: every mailbox
// the caller may browse (their own, plus any shared/team mailboxes they're
// authorized for) — lets the Mail tab render a picker instead of assuming
// there's exactly one, as it did before mailbox selection existed.
func handleMailGraphOptions(w http.ResponseWriter, r *http.Request) {
	s := settings.get()
	claims, ok := currentSession(r)
	if !ok {
		writeJSONError(w, "login required", http.StatusUnauthorized)
		return
	}
	opts := findInteractiveExchangeOptions(s, claims)
	out := make([]interactiveMailboxOption, len(opts))
	for i, o := range opts {
		out[i] = o.interactiveMailboxOption
	}
	writeJSON(w, struct {
		Options []interactiveMailboxOption `json:"options"`
	}{out})
}

// requireInteractiveExchangeConn resolves the caller's chosen, authorized
// connection (see resolveInteractiveExchangeOption) or writes a 401/403
// JSON error and returns ok=false — the shared gate every handler below
// opens with.
func requireInteractiveExchangeConn(w http.ResponseWriter, r *http.Request, s appSettings, mailboxKey string) (exchangeGraphConfig, bool) {
	claims, ok := currentSession(r)
	if !ok {
		writeJSONError(w, "login required", http.StatusUnauthorized)
		return exchangeGraphConfig{}, false
	}
	conn, allowed := resolveInteractiveExchangeOption(s, claims, mailboxKey)
	if !allowed {
		writeJSONError(w, "not authorized for interactive mailbox access", http.StatusForbidden)
		return exchangeGraphConfig{}, false
	}
	return conn, true
}

// handleMailGraphFolders serves POST /api/mail/graph/folders: the chosen
// mailbox's folder structure (exchangeDiscoverTree, graphmail.go — the same
// recursive walker Settings' admin-only "Struktur erkunden" button already
// uses), so the Mail tab can browse into any folder (e.g. "Entwürfe"), not
// just the connection's configured default.
func handleMailGraphFolders(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s := settings.get()
	var req struct {
		MailboxKey string `json:"mailbox_key,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	conn, ok := requireInteractiveExchangeConn(w, r, s, req.MailboxKey)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	node, err := exchangeDiscoverTree(ctx, conn, newDiscoverBudget())
	if err != nil {
		writeJSONError(w, "folder structure: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, node)
}

// mailGraphListItem is one message in the interactive mailbox list — the
// "Datum, Uhrzeit, Absender, Betreff" the Mail tab shows. Received is one
// ISO-8601 timestamp (Graph's own receivedDateTime); the frontend splits it
// into date/time for display rather than the server doing so, so locale
// formatting stays a client concern like everywhere else in this UI.
type mailGraphListItem struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"`
	From     string `json:"from"`
	Received string `json:"received"`
}

type mailGraphListResponse struct {
	Items []mailGraphListItem `json:"items"`
}

// mailGraphDefaultListLimit mirrors the Mail tab's "enough to scan at a
// glance without paging" sizing — same order of magnitude as
// previewExchangeMail's own import-preview callers use elsewhere.
const mailGraphDefaultListLimit = 25

// handleMailGraphList serves POST /api/mail/graph/list: the chosen
// mailbox's inbox (or a specific folder, overriding the connection's
// configured default — e.g. browsing into "Entwürfe"), newest first
// (previewExchangeMail, graphmail.go).
func handleMailGraphList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s := settings.get()
	var req struct {
		MailboxKey string `json:"mailbox_key,omitempty"`
		// Folder overrides the resolved connection's own configured Folder
		// for this one call — a Graph well-known folder name ("inbox",
		// "drafts", ...) or a folder ID, e.g. one picked from
		// /api/mail/graph/folders' tree. Empty keeps the connection's default
		// (inbox unless an admin configured otherwise), unchanged from
		// before folder browsing existed.
		Folder string `json:"folder,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	// An empty/absent body is fine — every field just stays its zero value;
	// json.NewDecoder on an empty reader returns EOF, deliberately ignored
	// here rather than treated as a request error.
	_ = json.NewDecoder(r.Body).Decode(&req)
	conn, ok := requireInteractiveExchangeConn(w, r, s, req.MailboxKey)
	if !ok {
		return
	}
	if f := strings.TrimSpace(req.Folder); f != "" {
		conn.Folder = f
	}
	limit := req.Limit
	if limit <= 0 {
		limit = mailGraphDefaultListLimit
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := previewExchangeMail(ctx, conn, limit)
	if err != nil {
		writeJSONError(w, "mailbox list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	items := make([]mailGraphListItem, 0, len(result.Items))
	for _, it := range result.Items {
		items = append(items, mailGraphListItem{ID: it.ID, Subject: it.Subject, From: it.From, Received: it.Received})
	}
	logAudit(r, "mail_graph_list", fmt.Sprintf("count=%d", len(items)))
	writeJSON(w, mailGraphListResponse{Items: items})
}

// mailGraphAttachmentInfo is one attachment of a live-browsed message —
// Filename always set; Text is its extracted content (OCR'd for images,
// same dispatch as ingestEmailAttachment) or empty with Error explaining
// why (unsupported type, OCR disabled, too large) — surfaced to the user
// rather than the attachment just silently not appearing anywhere.
type mailGraphAttachmentInfo struct {
	Filename string `json:"filename"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
}

// mailGraphMessageResponse is one message's full content for the Mail
// tab's inline reader — plus RawEmail, a ready-to-use "From/To/Date/
// Subject + body" rendering (emailFields.String()) that slots straight
// into /api/draft/reply's existing raw_email field, so generating a reply
// from a natively-browsed message needs no new draft-generation code path
// at all — it goes through exactly the same composeDraftReply call a
// manual copy-paste already uses. Attachments is the same content in
// structured form for the reader UI; RawEmail additionally has each
// attachment's extracted text appended (see handleMailGraphMessage) so a
// drafted reply can reference an attachment's content deterministically,
// not just by semantic-search luck.
type mailGraphMessageResponse struct {
	ID          string                    `json:"id"`
	Subject     string                    `json:"subject"`
	From        string                    `json:"from"`
	To          string                    `json:"to"`
	Received    string                    `json:"received"`
	Body        string                    `json:"body"`
	RawEmail    string                    `json:"raw_email"`
	Attachments []mailGraphAttachmentInfo `json:"attachments,omitempty"`
}

type mailGraphMessageRequest struct {
	ID         string `json:"id"`
	MailboxKey string `json:"mailbox_key,omitempty"`
}

// handleMailGraphMessage serves POST /api/mail/graph/message: one
// message's full content by ID, from the chosen mailbox
// (fetchGraphMail + graphMailToFields, graphmail.go).
func handleMailGraphMessage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s := settings.get()
	var req mailGraphMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSONError(w, "missing id", http.StatusBadRequest)
		return
	}
	conn, ok := requireInteractiveExchangeConn(w, r, s, req.MailboxKey)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token, err := graphAccessToken(ctx, egCreds(conn))
	if err != nil {
		writeJSONError(w, "graph auth: "+err.Error(), http.StatusInternalServerError)
		return
	}
	full, err := fetchGraphMail(ctx, conn, token, req.ID)
	if err != nil {
		writeJSONError(w, "fetch message: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fields := graphMailToFields(full)
	rawEmail := fields.String()

	// Attachments were previously invisible to the live mailbox reader
	// entirely (not even a filename) — fetched and extracted here with the
	// exact same dispatch import uses (extractAttachmentText, ingest.go),
	// so a reply drafted from a natively-browsed message can reference an
	// attachment's content just as reliably as one already imported.
	var attachmentInfos []mailGraphAttachmentInfo
	if full.HasAttachments {
		atts, attErr := fetchGraphMailAttachments(ctx, conn, token, req.ID)
		if attErr != nil {
			attachmentInfos = append(attachmentInfos, mailGraphAttachmentInfo{Filename: "(Anhänge)", Error: attErr.Error()})
		} else {
			for ai, att := range atts {
				if !strings.HasSuffix(att.ODataType, "fileAttachment") || att.ContentBytes == "" {
					continue
				}
				info := mailGraphAttachmentInfo{Filename: att.Name}
				if att.Size > 0 && int64(att.Size) > emailAttachmentMaxBytes(s) {
					info.Error = fmt.Sprintf("zu groß (%d Bytes)", att.Size)
				} else if data, decErr := base64.StdEncoding.DecodeString(att.ContentBytes); decErr != nil {
					info.Error = fmt.Sprintf("base64-Dekodierung fehlgeschlagen: %v", decErr)
				} else if text, extErr := extractAttachmentText(s, ai, att.Name, data); extErr != nil {
					info.Error = extErr.Error()
				} else {
					info.Text = strings.TrimSpace(text)
					rawEmail += fmt.Sprintf("\n\n--- Anhang %q ---\n%s", att.Name, info.Text)
				}
				attachmentInfos = append(attachmentInfos, info)
			}
		}
	}

	logAudit(r, "mail_graph_read", fmt.Sprintf("id=%s attachments=%d", req.ID, len(attachmentInfos)))
	writeJSON(w, mailGraphMessageResponse{
		ID:          req.ID,
		Subject:     fields.Subject,
		From:        fields.From,
		To:          fields.To,
		Received:    full.ReceivedDateTime,
		Body:        fields.Body,
		RawEmail:    rawEmail,
		Attachments: attachmentInfos,
	})
}

type mailGraphSaveDraftRequest struct {
	OriginalMessageID string `json:"original_message_id"`
	Body              string `json:"body"`
	MailboxKey        string `json:"mailbox_key,omitempty"`
}

type mailGraphSaveDraftResponse struct {
	DraftID string `json:"draft_id"`
}

// handleMailGraphSaveDraft serves POST /api/mail/graph/save-draft: files a
// reviewed reply draft directly into the CHOSEN mailbox's own Outlook
// Drafts folder (createExchangeGraphDraft, graphmail.go) — the interactive
// analogue of handleDraftSaveIMAP, just per-user/per-shared-mailbox instead
// of one fixed admin-configured mailbox. createExchangeGraphDraft itself
// still enforces cfg.EnableDraftReplies (the connection's own write opt-in)
// on top of the InteractiveEnabled+AllowedUsers/AllowedGroups check
// requireInteractiveExchangeConn already ran — two independent gates, same
// shape as every other write action in this codebase. Never sends — see
// createExchangeGraphDraft's own doc comment for the HARD INVARIANT.
func handleMailGraphSaveDraft(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s := settings.get()
	var req mailGraphSaveDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.OriginalMessageID) == "" || strings.TrimSpace(req.Body) == "" {
		writeJSONError(w, "missing original_message_id or body", http.StatusBadRequest)
		return
	}
	conn, ok := requireInteractiveExchangeConn(w, r, s, req.MailboxKey)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	draftID, err := createExchangeGraphDraft(ctx, conn, req.OriginalMessageID, req.Body)
	if err != nil {
		writeJSONError(w, "save draft: "+err.Error(), http.StatusInternalServerError)
		return
	}
	logAudit(r, "mail_graph_draft_saved", fmt.Sprintf("original_message_id=%s draft_id=%s", req.OriginalMessageID, draftID))
	writeJSON(w, mailGraphSaveDraftResponse{DraftID: draftID})
}
