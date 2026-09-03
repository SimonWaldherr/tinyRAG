package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Microsoft Teams channel message import via Microsoft Graph. App-only
// auth, needs the *application* permission ChannelMessage.Read.All plus
// admin consent — a tenant-wide read permission (there's no way to scope
// it to a single team the way an Exchange application access policy can
// scope mailbox access), so access to this app registration should be
// restricted accordingly. Token/HTTP mechanics shared with sharepoint.go/
// graphmail.go — see graph.go.
//
// A selected top-level post is imported together with its threaded replies
// (fetched via /messages/{id}/replies, paginated, capped at
// teamsMaxRepliesPerThread/importConfig.TeamsMaxRepliesPerThread): the
// whole thread becomes ONE document under the
// parent's source_id, so an answer buried in reply #7 is as searchable as
// the opening post, and a re-import after new replies arrive replaces the
// same source instead of duplicating it. The preview listing follows
// @odata.nextLink until the requested preview limit is reached — Graph
// caps channel-message pages at 50, so a single page silently hid
// anything older than the newest 50 posts.
// ─────────────────────────────────────────────────────────────────────────────

// teamsCreds adapts teamsConfig's credential fields to the shared graphCreds
// shape graph.go's token/HTTP helpers expect.
func teamsCreds(cfg teamsConfig) graphCreds {
	return graphCreds{
		TenantID:        cfg.TenantID,
		ClientID:        cfg.ClientID,
		ClientSecret:    cfg.ClientSecret,
		ClientSecretEnv: cfg.ClientSecretEnv,
	}
}

// teamsPreviewItem is one channel post's summary for the checklist UI.
type teamsPreviewItem struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	From    string `json:"from"`
	Preview string `json:"preview"`
	Created string `json:"created"`
}

type teamsPreviewResult struct {
	TeamID    string             `json:"team_id"`
	ChannelID string             `json:"channel_id"`
	Items     []teamsPreviewItem `json:"items"`
}

type teamsMessageFrom struct {
	User *struct {
		DisplayName string `json:"displayName"`
	} `json:"user"`
	Application *struct {
		DisplayName string `json:"displayName"`
	} `json:"application"`
}

// displayName resolves the human-readable sender name from whichever of
// User/Application Graph actually populated — a channel post can come from
// either a person or a connected app/bot.
func (f teamsMessageFrom) displayName() string {
	if f.User != nil && f.User.DisplayName != "" {
		return f.User.DisplayName
	}
	if f.Application != nil && f.Application.DisplayName != "" {
		return f.Application.DisplayName
	}
	return ""
}

type teamsMessageListItem struct {
	ID              string           `json:"id"`
	Subject         string           `json:"subject"`
	CreatedDateTime string           `json:"createdDateTime"`
	DeletedDateTime *string          `json:"deletedDateTime"`
	From            teamsMessageFrom `json:"from"`
	Body            struct {
		ContentType string `json:"contentType"` // "text" | "html"
		Content     string `json:"content"`
	} `json:"body"`
}

// previewTeamsMessages lists the most recent top-level posts in the
// configured team/channel (newest first per Graph's default ordering),
// matching the other importers' "preview, then select" UX. Follows
// @odata.nextLink until limit items are collected — Graph caps channel
// message pages at 50, so without pagination nothing beyond the newest 50
// posts was ever reachable, regardless of the configured preview limit.
func previewTeamsMessages(ctx context.Context, cfg teamsConfig, limit int) (teamsPreviewResult, error) {
	if cfg.TeamID == "" || cfg.ChannelID == "" {
		return teamsPreviewResult{}, fmt.Errorf("teams: team_id/channel_id not configured")
	}
	if limit < 1 {
		limit = importPreviewDefault
	}
	token, err := graphAccessToken(ctx, teamsCreds(cfg))
	if err != nil {
		return teamsPreviewResult{}, err
	}
	path := fmt.Sprintf("/teams/%s/channels/%s/messages?$top=%d",
		url.PathEscape(cfg.TeamID), url.PathEscape(cfg.ChannelID), clampPerPage(limit, 50))

	var items []teamsPreviewItem
	for path != "" && len(items) < limit {
		raw, err := graphGet(ctx, token, path)
		if err != nil {
			return teamsPreviewResult{}, err
		}
		var listing struct {
			Value    []teamsMessageListItem `json:"value"`
			NextLink string                 `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(raw, &listing); err != nil {
			return teamsPreviewResult{}, fmt.Errorf("teams: parse message listing: %w", err)
		}
		for _, m := range listing.Value {
			if m.DeletedDateTime != nil {
				continue
			}
			body := teamsBodyToText(m.Body.ContentType, m.Body.Content)
			items = append(items, teamsPreviewItem{
				ID:      m.ID,
				Subject: strings.TrimSpace(m.Subject),
				From:    m.From.displayName(),
				Preview: truncateRunes(body, 200),
				Created: m.CreatedDateTime,
			})
			if len(items) >= limit {
				break
			}
		}
		path = strings.TrimPrefix(listing.NextLink, graphBaseURL)
	}
	return teamsPreviewResult{TeamID: cfg.TeamID, ChannelID: cfg.ChannelID, Items: items}, nil
}

// teamsBodyToText strips HTML from a message body when Graph reports it as
// "html" content, leaving plain-text bodies untouched.
func teamsBodyToText(contentType, content string) string {
	body := strings.TrimSpace(content)
	if strings.EqualFold(contentType, "html") {
		body = htmlToText(body)
	}
	return body
}

// truncateRunes shortens s to at most n runes (not bytes, so multi-byte
// UTF-8 characters aren't split mid-sequence), appending an ellipsis when
// truncated — used for the preview-list snippet shown before import.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// fetchTeamsMessage retrieves one channel message's full record by ID —
// used at actual import time to re-fetch a message the user selected from
// previewTeamsMessages' lighter-weight listing.
func fetchTeamsMessage(ctx context.Context, cfg teamsConfig, token, id string) (teamsMessageListItem, error) {
	path := fmt.Sprintf("/teams/%s/channels/%s/messages/%s",
		url.PathEscape(cfg.TeamID), url.PathEscape(cfg.ChannelID), url.PathEscape(id))
	raw, err := graphGet(ctx, token, path)
	if err != nil {
		return teamsMessageListItem{}, err
	}
	var m teamsMessageListItem
	if err := json.Unmarshal(raw, &m); err != nil {
		return teamsMessageListItem{}, fmt.Errorf("teams: parse message: %w", err)
	}
	return m, nil
}

// teamsMaxRepliesDefault caps how many replies of one thread ride into its
// thread document when importConfig.TeamsMaxRepliesPerThread is unset —
// Graph pages replies at 50, so this bounds both request count (≤ 4 pages
// per thread) and document size for pathological mega-threads.
const teamsMaxRepliesDefault = 200

// teamsMaxRepliesPerThread resolves importConfig.TeamsMaxRepliesPerThread,
// 0 meaning teamsMaxRepliesDefault.
func teamsMaxRepliesPerThread(imp importConfig) int {
	if imp.TeamsMaxRepliesPerThread <= 0 {
		return teamsMaxRepliesDefault
	}
	return imp.TeamsMaxRepliesPerThread
}

// fetchTeamsReplies pages through one top-level message's /replies listing
// (deleted replies filtered out, capped at max), returned oldest-first so
// the thread document reads in natural conversation order regardless of
// Graph's reverse-chronological reply ordering.
func fetchTeamsReplies(ctx context.Context, cfg teamsConfig, token, id string, max int) ([]teamsMessageListItem, error) {
	path := fmt.Sprintf("/teams/%s/channels/%s/messages/%s/replies?$top=50",
		url.PathEscape(cfg.TeamID), url.PathEscape(cfg.ChannelID), url.PathEscape(id))
	var out []teamsMessageListItem
	for path != "" && len(out) < max {
		raw, err := graphGet(ctx, token, path)
		if err != nil {
			return out, err
		}
		var listing struct {
			Value    []teamsMessageListItem `json:"value"`
			NextLink string                 `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(raw, &listing); err != nil {
			return out, fmt.Errorf("teams: parse replies listing: %w", err)
		}
		for _, m := range listing.Value {
			if m.DeletedDateTime != nil {
				continue
			}
			out = append(out, m)
			if len(out) >= max {
				break
			}
		}
		path = strings.TrimPrefix(listing.NextLink, graphBaseURL)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedDateTime < out[j].CreatedDateTime })
	return out, nil
}

// teamsThreadText renders a top-level post plus its replies as one document
// — the parent in the shared emailFields layout, each reply as a clearly
// delimited "Antwort von …" block underneath, in conversation order.
func teamsThreadText(parent emailFields, replies []teamsMessageListItem) string {
	var b strings.Builder
	b.WriteString(parent.String())
	for _, rm := range replies {
		f := teamsMessageToFields(rm)
		if strings.TrimSpace(f.Body) == "" {
			continue
		}
		b.WriteString("\n\n--- Antwort")
		if f.From != "" {
			b.WriteString(" von " + f.From)
		}
		if !f.Date.IsZero() {
			b.WriteString(" (" + f.Date.Format("2006-01-02 15:04") + ")")
		}
		b.WriteString(" ---\n")
		b.WriteString(f.Body)
	}
	return b.String()
}

// teamsMessageToFields adapts a Graph channel message to the common
// emailFields shape ingestDocument expects, falling back to a truncated
// body snippet as the subject since channel posts often have none.
func teamsMessageToFields(m teamsMessageListItem) emailFields {
	subject := strings.TrimSpace(m.Subject)
	body := repairMojibake(teamsBodyToText(m.Body.ContentType, m.Body.Content))
	if subject == "" {
		subject = truncateRunes(body, 80)
	}
	f := emailFields{
		Subject: repairMojibake(subject),
		From:    repairMojibake(m.From.displayName()),
		Body:    body,
	}
	if t, err := time.Parse(time.RFC3339, m.CreatedDateTime); err == nil {
		f.Date = t
	}
	return f
}

// teamsImportResult/teamsProgress mirror graphmail.go's
// graphMailImportResult/graphMailProgress — same streaming-progress shape.
type teamsImportResult struct {
	baseImportResult
	Messages int `json:"messages"`
}

type teamsProgress struct {
	Result  teamsImportResult
	Subject string
}

// importTeamsMessages fetches and ingests the selected message IDs (from a
// prior previewTeamsMessages call).
func importTeamsMessages(ctx context.Context, rag *ragSystem, s appSettings, cfg teamsConfig, embedModel string, selected map[string]bool, dryRun bool, onProgress func(teamsProgress)) (teamsImportResult, error) {
	var res teamsImportResult
	res.DryRun = dryRun
	token, err := graphAccessToken(ctx, teamsCreds(cfg))
	if err != nil {
		return res, err
	}
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	if verbose {
		log.Printf("[verbose] teams import: team=%s channel=%s selected=%d dry_run=%v", cfg.TeamID, cfg.ChannelID, len(selected), dryRun)
	}

	// Sorted iteration: `selected` is a map, and when the per-run cap cuts
	// the run short, Go's random map order would truncate a *different*
	// random subset every run — sorted order makes the cut deterministic so
	// consecutive capped runs make forward progress instead of re-shuffling.
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			return res, err
		}
		m, err := fetchTeamsMessage(ctx, cfg, token, id)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		res.Messages++
		fields := teamsMessageToFields(m)

		// Thread replies ride along in the same document (see the file
		// header). Best-effort: a failing replies fetch is reported but
		// never blocks ingesting the top-level post itself.
		replies, rerr := fetchTeamsReplies(ctx, cfg, token, id, teamsMaxRepliesPerThread(s.Import))
		if rerr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: Antworten: %v", id, rerr))
		}
		text := teamsThreadText(fields, replies)

		// doc_date reflects the LATEST activity in the thread (parent or
		// newest reply), so recency scoring treats an actively discussed
		// old post as current rather than as stale as its opening date.
		docDate := unixOrZero(fields.Date)
		for _, rm := range replies {
			if t, err := time.Parse(time.RFC3339, rm.CreatedDateTime); err == nil && t.Unix() > docDate {
				docDate = t.Unix()
			}
		}

		sourceID := fmt.Sprintf("teams:%s:%s:%s", cfg.TeamID, cfg.ChannelID, id)
		sourceName := formatSourceName(fields.Subject, fields.From)

		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "teams_message", sourceName, text, docDate, dryRun)
		foldIngestOutcome(outcome, err, &res.Errors, &res.Skipped, &res.Chunks)
		if onProgress != nil {
			onProgress(teamsProgress{Result: res, Subject: fields.Subject})
		}
	}
	return res, nil
}
