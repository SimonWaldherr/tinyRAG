package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Freshservice ticket import via the REST API v2
// (https://api.<domain>.freshservice.com/api/v2/), Basic-auth'd with an
// account API key as the username and the literal string "X" as the
// password — Freshservice's documented convention, distinct from Jira/
// Confluence's email+token pair (confluence.go, jira.go) but the same
// env-var-indirection shape (APIKey/APIKeyEnv) for keeping the real secret
// out of settings.json. Same "preview list -> fetch full item on select"
// split as jira.go: the listing endpoint returns subject/status/updated_at
// but not the ticket body, so importing still needs one GET per ticket.
// ─────────────────────────────────────────────────────────────────────────────

// freshserviceResolvedAPIKey prefers the env-var-named key over the inline
// one, so a deployment can avoid committing the Freshservice API key to
// settings.json — same pattern as jiraResolvedToken.
func freshserviceResolvedAPIKey(cfg freshserviceConfig) string {
	return resolveSecret(cfg.APIKey, cfg.APIKeyEnv)
}

// freshserviceGet performs an authenticated GET against cfg.BaseURL+path.
func freshserviceGet(ctx context.Context, cfg freshserviceConfig, path string) ([]byte, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("freshservice: base_url not configured")
	}
	key := freshserviceResolvedAPIKey(cfg)
	if key == "" {
		return nil, fmt.Errorf("freshservice: api_key not configured (set api_key or api_key_env)")
	}
	return basicAuthREST{baseURL: cfg.BaseURL, username: key, password: "X", label: "freshservice"}.get(ctx, path)
}

// freshserviceStatusName maps Freshservice's numeric ticket status onto the
// same display strings the Freshservice UI itself uses — the API returns
// only the int (https://api.freshservice.com > Tickets > ticket properties).
func freshserviceStatusName(status int) string {
	switch status {
	case 2:
		return "Open"
	case 3:
		return "Pending"
	case 4:
		return "Resolved"
	case 5:
		return "Closed"
	default:
		return fmt.Sprintf("Status %d", status)
	}
}

// freshservicePreviewItem is one ticket's summary for the checklist UI.
type freshservicePreviewItem struct {
	ID      int    `json:"id"`
	Subject string `json:"subject"`
	Status  string `json:"status"`
	Updated string `json:"updated"`
}

type freshservicePreviewResult struct {
	Items []freshservicePreviewItem `json:"items"`
}

// freshserviceListResult is the shape of a GET /api/v2/tickets response,
// trimmed to the fields the preview path actually uses.
type freshserviceListResult struct {
	Tickets []struct {
		ID        int    `json:"id"`
		Subject   string `json:"subject"`
		Status    int    `json:"status"`
		UpdatedAt string `json:"updated_at"`
	} `json:"tickets"`
}

// previewFreshserviceTickets lists the most recently updated tickets
// (capped, newest-updated first — matching the other importers' "preview,
// then select" UX).
func previewFreshserviceTickets(ctx context.Context, cfg freshserviceConfig, limit int) (freshservicePreviewResult, error) {
	// Freshservice's per_page maxes out at 100; page past that if a larger
	// preview limit is configured, so an admin can review more than one
	// API page worth of candidates.
	raw, err := freshserviceGet(ctx, cfg, fmt.Sprintf("/api/v2/tickets?order_by=updated_at&order_type=desc&per_page=%d", clampPerPage(limit, 100)))
	if err != nil {
		return freshservicePreviewResult{}, err
	}
	var listing freshserviceListResult
	if err := json.Unmarshal(raw, &listing); err != nil {
		return freshservicePreviewResult{}, fmt.Errorf("freshservice: parse ticket list: %w", err)
	}
	items := make([]freshservicePreviewItem, 0, len(listing.Tickets))
	for _, t := range listing.Tickets {
		items = append(items, freshservicePreviewItem{ID: t.ID, Subject: t.Subject, Status: freshserviceStatusName(t.Status), Updated: t.UpdatedAt})
	}
	return freshservicePreviewResult{Items: items}, nil
}

// freshserviceTicketFull is the full ticket shape needed to build
// ingestable text. description_text is Freshservice's own plain-text
// rendering of the (HTML) description field, so — unlike Jira's wiki-markup
// description — no further rendering/mojibake concerns apply here.
type freshserviceTicketFull struct {
	Ticket struct {
		ID              int    `json:"id"`
		Subject         string `json:"subject"`
		DescriptionText string `json:"description_text"`
		Status          int    `json:"status"`
		UpdatedAt       string `json:"updated_at"`
		Requester       *struct {
			Name string `json:"name"`
		} `json:"requester"`
	} `json:"ticket"`
}

// fetchFreshserviceTicket fetches one ticket's full field set by id, for
// turning into ingestable text via freshserviceTicketText. include=requester
// adds the requester's display name, which the bare listing/ticket object
// doesn't carry (only requester_id).
func fetchFreshserviceTicket(ctx context.Context, cfg freshserviceConfig, id int) (freshserviceTicketFull, error) {
	raw, err := freshserviceGet(ctx, cfg, fmt.Sprintf("/api/v2/tickets/%d?include=requester", id))
	if err != nil {
		return freshserviceTicketFull{}, err
	}
	var ticket freshserviceTicketFull
	if err := json.Unmarshal(raw, &ticket); err != nil {
		return freshserviceTicketFull{}, fmt.Errorf("freshservice: parse ticket: %w", err)
	}
	return ticket, nil
}

// freshserviceTicketText renders a ticket as plain text for ingestion: a
// small header block (id/subject/status/requester, so those are always
// part of what gets embedded and can be searched/cited on) followed by the
// description body — same shape as jiraIssueText.
func freshserviceTicketText(ticket freshserviceTicketFull) string {
	t := ticket.Ticket
	var b strings.Builder
	fmt.Fprintf(&b, "#%d: %s\n", t.ID, strings.TrimSpace(t.Subject))
	fmt.Fprintf(&b, "Status: %s\n", freshserviceStatusName(t.Status))
	if t.Requester != nil && t.Requester.Name != "" {
		fmt.Fprintf(&b, "Anfragender: %s\n", t.Requester.Name)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(t.DescriptionText))
	return b.String()
}

// freshserviceImportResult/freshserviceProgress mirror jira.go's
// jiraImportResult/jiraProgress — same streaming-progress shape.
type freshserviceImportResult struct {
	baseImportResult
	Tickets int `json:"tickets"`
}

type freshserviceProgress struct {
	Result freshserviceImportResult
	ID     int
}

// freshserviceUpdatedLayout is the timestamp format Freshservice's REST API
// uses for ticket "updated_at" fields, e.g. "2026-01-05T10:00:00Z".
const freshserviceUpdatedLayout = time.RFC3339

// importFreshserviceTickets fetches and ingests the selected ticket ids
// (from a prior previewFreshserviceTickets call, or — for the scheduler's
// unattended sync, see scheduler.go — every id that preview returned).
func importFreshserviceTickets(ctx context.Context, rag *ragSystem, s appSettings, cfg freshserviceConfig, embedModel string, selected map[int]bool, dryRun bool, onProgress func(freshserviceProgress)) (freshserviceImportResult, error) {
	var res freshserviceImportResult
	res.DryRun = dryRun
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	if verbose {
		log.Printf("[verbose] freshservice import: base_url=%s selected=%d dry_run=%v", cfg.BaseURL, len(selected), dryRun)
	}

	for id := range selected {
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
		ticket, err := fetchFreshserviceTicket(ctx, cfg, id)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("#%d: %v", id, err))
			continue
		}
		res.Tickets++
		var docDate int64
		if t, err := time.Parse(freshserviceUpdatedLayout, ticket.Ticket.UpdatedAt); err == nil {
			docDate = t.Unix()
		}
		sourceID := fmt.Sprintf("freshservice:%s:%d", strings.TrimPrefix(strings.TrimPrefix(cfg.BaseURL, "https://"), "http://"), id)
		sourceName := fmt.Sprintf("#%d: %s", id, ticket.Ticket.Subject)

		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "freshservice_ticket", sourceName, freshserviceTicketText(ticket), docDate, dryRun)
		foldIngestOutcome(outcome, err, &res.Errors, &res.Skipped, &res.Chunks)
		pacer.count()
		if onProgress != nil {
			onProgress(freshserviceProgress{Result: res, ID: id})
		}
	}
	return res, nil
}
