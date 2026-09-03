package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Confluence Cloud page import via the classic REST API
// (https://developer.atlassian.com/cloud/confluence/rest/v1/), Basic-auth'd
// with an account email + API token (Atlassian ID -> Security -> API
// tokens) rather than OAuth2 — simple enough that it doesn't need a
// graph.go-style token cache, and Confluence's own docs still recommend
// this API version for content CRUD. cfg.BaseURL already includes the
// "/wiki" path segment (see confluenceConfig's doc comment in settings.go),
// so every request here just appends "/rest/api/...".
// ─────────────────────────────────────────────────────────────────────────────

// confResolvedToken prefers the env-var-named token over the inline one, so
// a deployment can avoid committing the Confluence API token to settings.json.
func confResolvedToken(cfg confluenceConfig) string {
	return resolveSecret(cfg.APIToken, cfg.APITokenEnv)
}

// confGet performs an authenticated GET against cfg.BaseURL+path.
func confGet(ctx context.Context, cfg confluenceConfig, path string) ([]byte, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("confluence: base_url not configured")
	}
	token := confResolvedToken(cfg)
	if cfg.Email == "" || token == "" {
		return nil, fmt.Errorf("confluence: email/api_token not configured (set api_token or api_token_env)")
	}
	return basicAuthREST{baseURL: cfg.BaseURL, username: cfg.Email, password: token, label: "confluence"}.get(ctx, path)
}

// confluencePreviewItem is one page's summary for the checklist UI.
type confluencePreviewItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Updated string `json:"updated"`
}

type confluencePreviewResult struct {
	SpaceKey string                  `json:"space_key"`
	Items    []confluencePreviewItem `json:"items"`
}

type confluenceContentListItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Version struct {
		When   string `json:"when"`
		Number int    `json:"number"`
	} `json:"version"`
}

// previewConfluencePages lists pages in the configured space (capped,
// matching the other importers' "preview, then select" UX).
func previewConfluencePages(ctx context.Context, cfg confluenceConfig, limit int) (confluencePreviewResult, error) {
	if cfg.SpaceKey == "" {
		return confluencePreviewResult{}, fmt.Errorf("confluence: space_key not configured")
	}
	path := fmt.Sprintf("/rest/api/content?spaceKey=%s&type=page&limit=%d&expand=version&orderby=history.lastUpdated%%20desc",
		url.QueryEscape(cfg.SpaceKey), clampPerPage(limit, 100))
	raw, err := confGet(ctx, cfg, path)
	if err != nil {
		return confluencePreviewResult{}, err
	}
	var listing struct {
		Results []confluenceContentListItem `json:"results"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return confluencePreviewResult{}, fmt.Errorf("confluence: parse content listing: %w", err)
	}
	items := make([]confluencePreviewItem, 0, len(listing.Results))
	for _, p := range listing.Results {
		items = append(items, confluencePreviewItem{ID: p.ID, Title: p.Title, Updated: p.Version.When})
	}
	return confluencePreviewResult{SpaceKey: cfg.SpaceKey, Items: items}, nil
}

// confluencePageFull is the full page shape needed to build ingestable text.
type confluencePageFull struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Version struct {
		When string `json:"when"`
	} `json:"version"`
}

// fetchConfluencePage fetches one page's storage-format body and title by
// ID, for turning into ingestable text via confluencePageText.
func fetchConfluencePage(ctx context.Context, cfg confluenceConfig, id string) (confluencePageFull, error) {
	path := fmt.Sprintf("/rest/api/content/%s?expand=body.storage,version", url.PathEscape(id))
	raw, err := confGet(ctx, cfg, path)
	if err != nil {
		return confluencePageFull{}, err
	}
	var p confluencePageFull
	if err := json.Unmarshal(raw, &p); err != nil {
		return confluencePageFull{}, fmt.Errorf("confluence: parse page: %w", err)
	}
	return p, nil
}

// confluencePageText renders a page as plain text for ingestion — storage
// format is XHTML-based, close enough to plain HTML that htmlToText's
// tag-stripping (extract.go) produces a reasonable result even though it
// won't understand Confluence-specific macros (<ac:.../>, <ri:.../>).
func confluencePageText(p confluencePageFull) string {
	body := repairMojibake(htmlToText(p.Body.Storage.Value))
	return fmt.Sprintf("%s\n\n%s", repairMojibake(strings.TrimSpace(p.Title)), body)
}

// confluenceImportResult/confluenceProgress mirror graphmail.go's
// graphMailImportResult/graphMailProgress — same streaming-progress shape.
type confluenceImportResult struct {
	baseImportResult
	Pages int `json:"pages"`
}

type confluenceProgress struct {
	Result confluenceImportResult
	Title  string
}

// importConfluencePages fetches and ingests the selected page IDs (from a
// prior previewConfluencePages call).
func importConfluencePages(ctx context.Context, rag *ragSystem, s appSettings, cfg confluenceConfig, embedModel string, selected map[string]bool, dryRun bool, onProgress func(confluenceProgress)) (confluenceImportResult, error) {
	var res confluenceImportResult
	res.DryRun = dryRun
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	if verbose {
		log.Printf("[verbose] confluence import: space=%s selected=%d dry_run=%v", cfg.SpaceKey, len(selected), dryRun)
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
		p, err := fetchConfluencePage(ctx, cfg, id)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		res.Pages++
		title := repairMojibake(strings.TrimSpace(p.Title))
		sourceID := fmt.Sprintf("confluence:%s:%s", cfg.SpaceKey, id)

		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "confluence_page", title, confluencePageText(p), 0, dryRun)
		foldIngestOutcome(outcome, err, &res.Errors, &res.Skipped, &res.Chunks)
		pacer.count()
		if onProgress != nil {
			onProgress(confluenceProgress{Result: res, Title: title})
		}
	}
	return res, nil
}
