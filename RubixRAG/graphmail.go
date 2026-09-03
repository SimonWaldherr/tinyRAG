package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Exchange Online mail import via Microsoft Graph — the Microsoft-365
// path for "Outlook/Exchange" (see imapmail.go for the on-prem/generic-
// IMAP path). App-only auth, needs a Mail.Read (or narrower
// Mail.ReadBasic.All) *application* permission; scope it to
// exchangeGraphConfig.Mailbox via an Exchange application access policy
// if the app registration shouldn't be able to read every mailbox in the
// tenant. Token/HTTP mechanics shared with sharepoint.go/teams.go — see
// graph.go.
// ─────────────────────────────────────────────────────────────────────────────

// egCreds adapts exchangeGraphConfig to the shared graphCreds shape so this
// connector can reuse graph.go's token acquisition/caching.
func egCreds(cfg exchangeGraphConfig) graphCreds {
	return graphCreds{
		TenantID:        cfg.TenantID,
		ClientID:        cfg.ClientID,
		ClientSecret:    cfg.ClientSecret,
		ClientSecretEnv: cfg.ClientSecretEnv,
	}
}

// egFolder defaults to "inbox" when cfg.Folder is unset, matching Graph's
// own well-known folder name.
func egFolder(cfg exchangeGraphConfig) string {
	if cfg.Folder != "" {
		return cfg.Folder
	}
	return "inbox"
}

// graphMailPreviewItem is one message's summary, enough for a checklist
// UI without pulling the (potentially large) body over the wire.
type graphMailPreviewItem struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"`
	From     string `json:"from"`
	Received string `json:"received"`
}

type graphMailPreviewResult struct {
	Mailbox string                 `json:"mailbox"`
	Folder  string                 `json:"folder"`
	Items   []graphMailPreviewItem `json:"items"`
}

type graphMailListItem struct {
	ID               string `json:"id"`
	Subject          string `json:"subject"`
	ReceivedDateTime string `json:"receivedDateTime"`
	From             struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
}

// previewExchangeMail lists the most recent messages (capped, newest
// first) in cfg.Mailbox/cfg.Folder — a flat list, not paginated, matching
// the other importers' "preview, then select" UX at a scope that stays
// fast for a single review pass.
func previewExchangeMail(ctx context.Context, cfg exchangeGraphConfig, limit int) (graphMailPreviewResult, error) {
	if cfg.Mailbox == "" {
		return graphMailPreviewResult{}, fmt.Errorf("exchange: mailbox not configured")
	}
	token, err := graphAccessToken(ctx, egCreds(cfg))
	if err != nil {
		return graphMailPreviewResult{}, err
	}
	folder := egFolder(cfg)
	path := fmt.Sprintf("/users/%s/mailFolders/%s/messages?$top=%d&$orderby=receivedDateTime%%20desc&$select=id,subject,from,receivedDateTime",
		url.PathEscape(cfg.Mailbox), url.PathEscape(folder), clampPerPage(limit, 100))
	raw, err := graphGet(ctx, token, path)
	if err != nil {
		return graphMailPreviewResult{}, err
	}
	var listing struct {
		Value []graphMailListItem `json:"value"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return graphMailPreviewResult{}, fmt.Errorf("exchange: parse message listing: %w", err)
	}

	items := make([]graphMailPreviewItem, 0, len(listing.Value))
	for _, m := range listing.Value {
		from := m.From.EmailAddress.Name
		if from == "" {
			from = m.From.EmailAddress.Address
		}
		items = append(items, graphMailPreviewItem{ID: m.ID, Subject: m.Subject, From: from, Received: m.ReceivedDateTime})
	}
	return graphMailPreviewResult{Mailbox: cfg.Mailbox, Folder: folder, Items: items}, nil
}

// listExchangeMailSince lists messages received at or after since (RFC3339),
// OLDEST first, following @odata.nextLink up to max items — the incremental
// counterpart to previewExchangeMail's single newest-N page, used by the
// scheduler's watermark-based sync (see exchangeGraphConfig.
// LastSyncedReceived). Before this existed, scheduled Exchange sync could
// only ever see the newest PreviewLimit messages: anything older was never
// imported, and a burst of more than PreviewLimit new messages between two
// ticks permanently lost the overflow.
//
// The filter uses `ge` (not `gt`) deliberately: several messages can share
// one receivedDateTime second, and re-listing the already-imported watermark
// message only costs a content-hash skip, while `gt` would silently lose its
// same-second siblings.
func listExchangeMailSince(ctx context.Context, cfg exchangeGraphConfig, since string, max int) ([]graphMailListItem, error) {
	if cfg.Mailbox == "" {
		return nil, fmt.Errorf("exchange: mailbox not configured")
	}
	if max < 1 {
		max = importMaxItemsDefault
	}
	token, err := graphAccessToken(ctx, egCreds(cfg))
	if err != nil {
		return nil, err
	}
	folder := egFolder(cfg)
	query := url.Values{}
	query.Set("$top", fmt.Sprintf("%d", clampPerPage(max, 100)))
	query.Set("$filter", fmt.Sprintf("receivedDateTime ge %s", since))
	query.Set("$orderby", "receivedDateTime asc")
	query.Set("$select", "id,subject,from,receivedDateTime")
	path := fmt.Sprintf("/users/%s/mailFolders/%s/messages?%s",
		url.PathEscape(cfg.Mailbox), url.PathEscape(folder), query.Encode())

	var out []graphMailListItem
	for path != "" && len(out) < max {
		raw, err := graphGet(ctx, token, path)
		if err != nil {
			return out, err
		}
		var listing struct {
			Value    []graphMailListItem `json:"value"`
			NextLink string              `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(raw, &listing); err != nil {
			return out, fmt.Errorf("exchange: parse message listing: %w", err)
		}
		for _, m := range listing.Value {
			out = append(out, m)
			if len(out) >= max {
				break
			}
		}
		path = strings.TrimPrefix(listing.NextLink, graphBaseURL)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Discover — recursive mail-folder structure preview (see discover.go for
// the shared discoverNode/discoverBudget shape used by SharePoint/Folder
// too). Distinct from previewExchangeMail above: this lists FOLDERS (with
// Graph's own per-folder item counts), not messages within one folder —
// answering "what folders even exist in this mailbox" before an admin
// picks one for cfg.Folder, which previewExchangeMail/importExchangeMail
// then read messages from.
// ─────────────────────────────────────────────────────────────────────────────

// graphMailFolderNode is one mailFolder resource, trimmed to what Discover
// needs — Graph pre-aggregates totalItemCount/childFolderCount per folder,
// so no separate "list items in this folder" call is needed to get a count
// the way SharePoint/Folder need one Graph/filesystem call per folder.
type graphMailFolderNode struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ChildFolderCount int    `json:"childFolderCount"`
	TotalItemCount   int    `json:"totalItemCount"`
}

type graphMailFolderListing struct {
	Value    []graphMailFolderNode `json:"value"`
	NextLink string                `json:"@odata.nextLink"`
}

// listGraphMailFolders lists parentID's child folders (parentID "" =
// mailbox top-level), following @odata.nextLink so a folder with many
// custom subfolders is still fully enumerated, not silently first-page-only
// — same nextLink-following shape as sharepoint.go's spDeltaSync.
func listGraphMailFolders(ctx context.Context, cfg exchangeGraphConfig, token, parentID string) ([]graphMailFolderNode, error) {
	path := fmt.Sprintf("/users/%s/mailFolders?$select=id,displayName,childFolderCount,totalItemCount&$top=100", url.PathEscape(cfg.Mailbox))
	if parentID != "" {
		path = fmt.Sprintf("/users/%s/mailFolders/%s/childFolders?$select=id,displayName,childFolderCount,totalItemCount&$top=100", url.PathEscape(cfg.Mailbox), url.PathEscape(parentID))
	}

	var all []graphMailFolderNode
	for path != "" {
		raw, err := graphGet(ctx, token, path)
		if err != nil {
			return nil, err
		}
		var listing graphMailFolderListing
		if err := json.Unmarshal(raw, &listing); err != nil {
			return nil, fmt.Errorf("exchange: parse mail folder listing: %w", err)
		}
		all = append(all, listing.Value...)
		path = strings.TrimPrefix(listing.NextLink, graphBaseURL)
	}
	return all, nil
}

// exchangeDiscoverTree recurses through cfg.Mailbox's folder structure,
// always starting at the mailbox root regardless of cfg.Folder (which
// selects an import source, not a discovery starting point — the whole
// point of Discover is showing what folders exist before picking one for
// cfg.Folder). Synthesizes one wrapping root node named after the mailbox,
// since /mailFolders itself returns a list with no single natural root
// (unlike a filesystem or a SharePoint document library) — keeps the wire
// contract "always exactly one discoverNode" the same across all three
// Discover connectors, so the frontend renderer never needs an
// Exchange-specific case.
func exchangeDiscoverTree(ctx context.Context, cfg exchangeGraphConfig, budget *discoverBudget) (discoverNode, error) {
	if cfg.Mailbox == "" {
		return discoverNode{}, fmt.Errorf("exchange: mailbox not configured")
	}
	token, err := graphAccessToken(ctx, egCreds(cfg))
	if err != nil {
		return discoverNode{}, err
	}
	root := discoverNode{Name: cfg.Mailbox, Path: ""}
	children, err := exchangeDiscoverChildren(ctx, cfg, token, "", 0, budget)
	if err != nil {
		return discoverNode{}, err
	}
	root.Children = children
	return root, nil
}

// exchangeDiscoverChildren lists parentID's child folders (parentID "" =
// mailbox top-level) and recurses into each, bounded by budget. A folder
// failing to list (e.g. a permission-scoped subfolder) is recorded on that
// node's own Error field — siblings/ancestors are unaffected.
func exchangeDiscoverChildren(ctx context.Context, cfg exchangeGraphConfig, token, parentID string, depth int, budget *discoverBudget) ([]discoverNode, error) {
	if !budget.admit(ctx) {
		return nil, nil
	}
	folders, err := listGraphMailFolders(ctx, cfg, token, parentID)
	if err != nil {
		return nil, err
	}
	nodes := make([]discoverNode, 0, len(folders))
	for _, f := range folders {
		node := discoverNode{Name: f.DisplayName, Path: f.ID, ItemCount: f.TotalItemCount}
		if f.ChildFolderCount > 0 {
			if depth >= budget.maxDepth {
				node.Truncated = true
			} else if children, err := exchangeDiscoverChildren(ctx, cfg, token, f.ID, depth+1, budget); err != nil {
				node.Error = err.Error()
			} else {
				node.Children = children
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// graphMailFull is the full message shape needed to build emailFields.
type graphMailFull struct {
	Subject          string `json:"subject"`
	ReceivedDateTime string `json:"receivedDateTime"`
	HasAttachments   bool   `json:"hasAttachments"`
	From             struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
	ToRecipients []struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"toRecipients"`
	Body struct {
		ContentType string `json:"contentType"` // "text" | "html"
		Content     string `json:"content"`
	} `json:"body"`
}

// fetchGraphMail fetches one message's full body/recipients by ID, for
// turning into emailFields via graphMailToFields.
func fetchGraphMail(ctx context.Context, cfg exchangeGraphConfig, token, id string) (graphMailFull, error) {
	path := fmt.Sprintf("/users/%s/messages/%s?$select=subject,from,toRecipients,body,receivedDateTime,hasAttachments", url.PathEscape(cfg.Mailbox), url.PathEscape(id))
	raw, err := graphGet(ctx, token, path)
	if err != nil {
		return graphMailFull{}, err
	}
	var m graphMailFull
	if err := json.Unmarshal(raw, &m); err != nil {
		return graphMailFull{}, fmt.Errorf("exchange: parse message: %w", err)
	}
	return m, nil
}

// graphMailToFields normalizes a Graph message into emailFields, stripping
// HTML bodies to plain text and running repairMojibake on every text field
// since Graph content isn't guaranteed clean of the same charset corruption
// other import paths guard against.
func graphMailToFields(m graphMailFull) emailFields {
	from := formatAddress(m.From.EmailAddress.Name, m.From.EmailAddress.Address)
	var toParts []string
	for _, r := range m.ToRecipients {
		toParts = append(toParts, formatAddress(r.EmailAddress.Name, r.EmailAddress.Address))
	}
	body := strings.TrimSpace(m.Body.Content)
	if strings.EqualFold(m.Body.ContentType, "html") {
		body = htmlToText(body)
	}
	f := emailFields{
		Subject: repairMojibake(strings.TrimSpace(m.Subject)),
		From:    repairMojibake(from),
		To:      repairMojibake(strings.Join(toParts, ", ")),
		Body:    repairMojibake(body),
	}
	if t, err := time.Parse(time.RFC3339, m.ReceivedDateTime); err == nil {
		f.Date = t
	}
	return f
}

// graphAttachmentItem is one entry of a message's
// /attachments listing. Only fileAttachment (@odata.type) carries
// contentBytes directly — itemAttachment (a forwarded message) and
// referenceAttachment (a OneDrive/SharePoint link) don't, and are skipped
// rather than mishandled.
type graphAttachmentItem struct {
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentBytes string `json:"contentBytes"`
	// Size is the attachment's byte size (Graph's "size" property) —
	// requested so importExchangeMail can reject an oversized attachment
	// before base64-decoding contentBytes into memory, mirroring the PST
	// path's GetAttachSize() pre-check.
	Size int `json:"size"`
}

// fetchGraphMailAttachments lists message id's attachments, for turning
// into ingestable sources via ingestEmailAttachment.
func fetchGraphMailAttachments(ctx context.Context, cfg exchangeGraphConfig, token, id string) ([]graphAttachmentItem, error) {
	path := fmt.Sprintf("/users/%s/messages/%s/attachments?$select=name,contentType,contentBytes,size", url.PathEscape(cfg.Mailbox), url.PathEscape(id))
	raw, err := graphGet(ctx, token, path)
	if err != nil {
		return nil, err
	}
	var listing struct {
		Value []graphAttachmentItem `json:"value"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, fmt.Errorf("exchange: parse attachment listing: %w", err)
	}
	return listing.Value, nil
}

// graphMailImportResult/graphMailProgress mirror pst.go's
// pstImportResult/pstProgress — same streaming-progress shape.
type graphMailImportResult struct {
	baseImportResult
	mailAttachmentWarnings
	Messages    int `json:"messages"`
	Attachments int `json:"attachments"`
}

type graphMailProgress struct {
	Result  graphMailImportResult
	Subject string
}

// importExchangeMail fetches and ingests the selected message IDs (from a
// prior previewExchangeMail call). Go randomizes map iteration order, so the
// ids are sorted first — when the per-run cap cuts a run short, a
// deterministic order means consecutive runs truncate the same way and make
// forward progress instead of re-shuffling which subset survives each time.
func importExchangeMail(ctx context.Context, rag *ragSystem, s appSettings, cfg exchangeGraphConfig, embedModel string, selected map[string]bool, dryRun bool, onProgress func(graphMailProgress)) (graphMailImportResult, error) {
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	res, _, err := importExchangeMailIDs(ctx, rag, s, cfg, embedModel, ids, dryRun, onProgress)
	return res, err
}

// importExchangeMailIDs fetches and ingests messages in exactly the given
// order, additionally returning how many of ids were attempted before the
// cap/context cut the run short. The scheduler's incremental sync passes ids
// in ascending receivedDateTime order and advances its watermark only across
// attempted messages — so a capped run resumes exactly where it stopped
// instead of skipping past mail it never looked at. A message whose fetch
// fails still counts as attempted (its error is reported); a permanently
// broken message must not wedge the watermark forever.
func importExchangeMailIDs(ctx context.Context, rag *ragSystem, s appSettings, cfg exchangeGraphConfig, embedModel string, ids []string, dryRun bool, onProgress func(graphMailProgress)) (graphMailImportResult, int, error) {
	var res graphMailImportResult
	res.DryRun = dryRun
	processed := 0
	token, err := graphAccessToken(ctx, egCreds(cfg))
	if err != nil {
		return res, processed, err
	}
	folder := egFolder(cfg)
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	if verbose {
		log.Printf("[verbose] exchange import: mailbox=%s folder=%s selected=%d dry_run=%v", cfg.Mailbox, folder, len(ids), dryRun)
	}

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, processed, err
		}
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			return res, processed, err
		}
		m, err := fetchGraphMail(ctx, cfg, token, id)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			processed++
			continue
		}
		res.Messages++
		fields := graphMailToFields(m)

		docDate := unixOrZero(fields.Date)
		sourceID := fmt.Sprintf("outlook:%s:%s:%s", cfg.Mailbox, folder, id)
		sourceName := formatSourceName(fields.Subject, fields.From)

		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "outlook_email", sourceName, fields.String(), docDate, dryRun)
		foldIngestOutcome(outcome, err, &res.Errors, &res.Skipped, &res.Chunks)

		// hasAttachments is already in the fetched message — skip the
		// per-message attachments round trip entirely for the (typical)
		// majority of mail that carries none, instead of paying one extra
		// Graph GET per message just to learn "empty list".
		if !m.HasAttachments {
			// nothing to fetch
		} else if atts, attErr := fetchGraphMailAttachments(ctx, cfg, token, id); attErr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: attachments: %v", sourceID, attErr))
		} else {
			for ai, att := range atts {
				if !strings.HasSuffix(att.ODataType, "fileAttachment") || att.ContentBytes == "" {
					continue // itemAttachment/referenceAttachment: no retrievable bytes
				}
				if att.Size > 0 && int64(att.Size) > emailAttachmentMaxBytes(s) {
					res.Skipped++
					res.AttachmentWarnings = append(res.AttachmentWarnings, fmt.Sprintf("%s — Anhang: %s: zu groß (%d Bytes)", sourceName, att.Name, att.Size))
					continue
				}
				data, decErr := base64.StdEncoding.DecodeString(att.ContentBytes)
				if decErr != nil {
					res.Skipped++
					res.AttachmentWarnings = append(res.AttachmentWarnings, fmt.Sprintf("%s — Anhang: %s: base64-Dekodierung fehlgeschlagen: %v", sourceName, att.Name, decErr))
					continue
				}
				attOutcome, attErr := ingestEmailAttachment(rag, s, embedModel, sourceID, ai, "outlook_attachment", att.Name, data, fields.Subject, fields.From, docDate, dryRun)
				foldAttachmentOutcome(attOutcome, attErr, &res.Attachments, &res.Skipped, &res.Chunks, &res.AttachmentWarnings)
			}
		}

		pacer.count()
		processed++
		if onProgress != nil {
			onProgress(graphMailProgress{Result: res, Subject: fields.Subject})
		}
	}
	return res, processed, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Draft-only write access — see the HARD SAFETY INVARIANT documented
// throughout this repo (docs/PROJEKTPLAN.md "Meilenstein 1", draft.go's
// package comment, agent.go's save_draft_to_mailbox): R3 never sends mail
// automatically, or at all, under any configuration. The only two Graph
// calls createExchangeGraphDraft below ever makes are POST
// .../messages/{id}/createReply (Graph's own "start a reply" call, which
// itself only ever produces a new DRAFT message — Graph has no equivalent
// one-call "reply and send") and PATCH .../messages/{id} (edits that same
// draft's body). Neither is a send; there is no code path here, automatic
// or triggered, that calls the /send endpoint. If a future change to this
// function ever needs a Graph call whose purpose is unclear, do not guess
// — Microsoft's own docs are unambiguous about which Message actions
// transition to "sent" (POST .../send, POST .../sendMail) versus which
// stay a draft (createReply/createReplyAll/createForward, then PATCH).
// ─────────────────────────────────────────────────────────────────────────────

// graphDraftMessage is the subset of Graph's Message resource
// createExchangeGraphDraft needs from the createReply response: just the
// new draft's own id, so the follow-up PATCH knows what to edit.
type graphDraftMessage struct {
	ID string `json:"id"`
}

// graphMessageBodyPatch is the PATCH .../messages/{id} request body shape
// for replacing a draft's body content — Graph's Message.body ItemBody
// resource (contentType + content), same shape graphMailFull.Body reads
// on the way in.
type graphMessageBodyPatch struct {
	Body struct {
		ContentType string `json:"contentType"` // "text" | "html" — always "text" here, see createExchangeGraphDraft
		Content     string `json:"content"`
	} `json:"body"`
}

// createExchangeGraphDraft replies to originalMessageID with a new DRAFT
// carrying bodyText, returning the created draft's message ID — never a
// send, see the package comment above. Two Graph calls:
//  1. POST /users/{mailbox}/messages/{originalMessageID}/createReply —
//     Graph creates a new message in the Drafts folder, pre-addressed
//     (To/Cc, subject "RE: ...", quoted original body) exactly like
//     clicking "Reply" in Outlook does, EXCEPT it stops there instead of
//     opening a compose window; this call needs no request body at all.
//  2. PATCH /users/{mailbox}/messages/{draftID} — replaces that draft's
//     body with bodyText (composeDraftReply's generated reply text),
//     since createReply's own body is just the quoted original, not the
//     text a human/model actually wants to send.
//
// Requires cfg.EnableDraftReplies (this connection's own opt-in for write
// access, on top of whatever Graph permission the app registration holds —
// see exchangeGraphConfig's doc comment) — checked here, not just by
// callers, so a future call site can't accidentally create a draft for a
// connection that was never opted into writes.
func createExchangeGraphDraft(ctx context.Context, cfg exchangeGraphConfig, originalMessageID, bodyText string) (string, error) {
	if !cfg.EnableDraftReplies {
		return "", fmt.Errorf("exchange: draft replies not enabled for connection %q", cfg.Name)
	}
	if cfg.Mailbox == "" {
		return "", fmt.Errorf("exchange: mailbox not configured")
	}
	if strings.TrimSpace(originalMessageID) == "" {
		return "", fmt.Errorf("exchange: original message id required")
	}
	token, err := graphAccessToken(ctx, egCreds(cfg))
	if err != nil {
		return "", err
	}

	replyPath := fmt.Sprintf("/users/%s/messages/%s/createReply", url.PathEscape(cfg.Mailbox), url.PathEscape(originalMessageID))
	raw, err := graphWrite(ctx, "POST", token, replyPath, nil)
	if err != nil {
		return "", fmt.Errorf("exchange: createReply: %w", err)
	}
	var draft graphDraftMessage
	if err := json.Unmarshal(raw, &draft); err != nil {
		return "", fmt.Errorf("exchange: parse createReply response: %w", err)
	}
	if draft.ID == "" {
		return "", fmt.Errorf("exchange: createReply returned no message id")
	}

	var patch graphMessageBodyPatch
	patch.Body.ContentType = "text"
	patch.Body.Content = bodyText
	patchBody, err := json.Marshal(patch)
	if err != nil {
		return "", fmt.Errorf("exchange: build draft body patch: %w", err)
	}
	patchPath := fmt.Sprintf("/users/%s/messages/%s", url.PathEscape(cfg.Mailbox), url.PathEscape(draft.ID))
	if _, err := graphWrite(ctx, "PATCH", token, patchPath, patchBody); err != nil {
		return "", fmt.Errorf("exchange: patch draft body: %w", err)
	}

	// Every draft creation is audited regardless of caller (today: the
	// scheduler's auto-draft rule engine, autodraft.go) — logged here
	// rather than at each call site so a future caller can't forget it,
	// same reasoning as this function itself owning the EnableDraftReplies
	// check above. logAuditAs (not logAudit) since this runs from
	// scheduler.go's background job, which has no *http.Request — same
	// "detached job" shape as handleImportPST's own logAuditAs use
	// (audit.go).
	logAuditAs("system", "", "exchange_draft_created", fmt.Sprintf("connection=%s mailbox=%s original_message_id=%s draft_id=%s", cfg.Name, cfg.Mailbox, originalMessageID, draft.ID))

	return draft.ID, nil
}
