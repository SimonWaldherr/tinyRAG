package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Jira Cloud issue import via the REST API v2
// (https://developer.atlassian.com/cloud/jira/platform/rest/v2/),
// Basic-auth'd with an account email + API token — the same credential
// shape as confluence.go (Atlassian ID -> Security -> API tokens; often
// literally the same token, since Jira/Confluence Cloud API tokens are
// interchangeable for whatever that account can read). REST API v2 (not
// v3) is used deliberately: v2 renders rich-text fields like `description`
// down to a plain wiki-markup string for backwards compatibility, while v3
// returns Atlassian Document Format (a nested JSON tree) that would need a
// dedicated renderer just to get plain text back out.
// ─────────────────────────────────────────────────────────────────────────────

// jiraResolvedToken prefers the env-var-named token over the inline one, so
// a deployment can avoid committing the Jira API token to settings.json.
func jiraResolvedToken(cfg jiraConfig) string {
	return resolveSecret(cfg.APIToken, cfg.APITokenEnv)
}

// jiraGet performs an authenticated GET against cfg.BaseURL+path.
func jiraGet(ctx context.Context, cfg jiraConfig, path string) ([]byte, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("jira: base_url not configured")
	}
	token := jiraResolvedToken(cfg)
	if cfg.Email == "" || token == "" {
		return nil, fmt.Errorf("jira: email/api_token not configured (set api_token or api_token_env)")
	}
	return basicAuthREST{baseURL: cfg.BaseURL, username: cfg.Email, password: token, label: "jira"}.get(ctx, path)
}

// jiraPreviewItem is one issue's summary for the checklist UI.
type jiraPreviewItem struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
	Updated string `json:"updated"`
}

type jiraPreviewResult struct {
	ProjectKey string            `json:"project_key"`
	Items      []jiraPreviewItem `json:"items"`
}

// jiraSearchResult is the shape of a /rest/api/2/search response, trimmed
// to the fields the preview/import paths actually use.
type jiraSearchResult struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Updated string `json:"updated"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
		} `json:"fields"`
	} `json:"issues"`
}

// previewJiraIssues lists issues in the configured project (capped,
// newest-updated first — matching the other importers' "preview, then
// select" UX).
func previewJiraIssues(ctx context.Context, cfg jiraConfig, limit int) (jiraPreviewResult, error) {
	if cfg.ProjectKey == "" {
		return jiraPreviewResult{}, fmt.Errorf("jira: project_key not configured")
	}
	jql := fmt.Sprintf("project=%s ORDER BY updated DESC", cfg.ProjectKey)
	path := fmt.Sprintf("/rest/api/2/search?jql=%s&maxResults=%d&fields=summary,updated,status", url.QueryEscape(jql), clampPerPage(limit, 100))
	raw, err := jiraGet(ctx, cfg, path)
	if err != nil {
		return jiraPreviewResult{}, err
	}
	var listing jiraSearchResult
	if err := json.Unmarshal(raw, &listing); err != nil {
		return jiraPreviewResult{}, fmt.Errorf("jira: parse search results: %w", err)
	}
	items := make([]jiraPreviewItem, 0, len(listing.Issues))
	for _, iss := range listing.Issues {
		items = append(items, jiraPreviewItem{Key: iss.Key, Summary: iss.Fields.Summary, Status: iss.Fields.Status.Name, Updated: iss.Fields.Updated})
	}
	return jiraPreviewResult{ProjectKey: cfg.ProjectKey, Items: items}, nil
}

// jiraIssueFull is the full issue shape needed to build ingestable text.
type jiraIssueFull struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Updated     string `json:"updated"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
	} `json:"fields"`
}

// fetchJiraIssue fetches one issue's full field set by key, for turning
// into ingestable text via jiraIssueText.
func fetchJiraIssue(ctx context.Context, cfg jiraConfig, key string) (jiraIssueFull, error) {
	path := fmt.Sprintf("/rest/api/2/issue/%s?fields=summary,description,updated,status,assignee", url.PathEscape(key))
	raw, err := jiraGet(ctx, cfg, path)
	if err != nil {
		return jiraIssueFull{}, err
	}
	var issue jiraIssueFull
	if err := json.Unmarshal(raw, &issue); err != nil {
		return jiraIssueFull{}, fmt.Errorf("jira: parse issue: %w", err)
	}
	return issue, nil
}

// jiraIssueText renders an issue as plain text for ingestion: a small
// header block (key/summary/status/assignee, so those are always part of
// what gets embedded and can be searched/cited on) followed by the
// description body.
func jiraIssueText(issue jiraIssueFull) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", issue.Key, repairMojibake(strings.TrimSpace(issue.Fields.Summary)))
	if issue.Fields.Status.Name != "" {
		fmt.Fprintf(&b, "Status: %s\n", issue.Fields.Status.Name)
	}
	if issue.Fields.Assignee != nil && issue.Fields.Assignee.DisplayName != "" {
		fmt.Fprintf(&b, "Zugewiesen an: %s\n", issue.Fields.Assignee.DisplayName)
	}
	b.WriteString("\n")
	b.WriteString(repairMojibake(strings.TrimSpace(issue.Fields.Description)))
	return b.String()
}

// jiraImportResult/jiraProgress mirror confluence.go's
// confluenceImportResult/confluenceProgress — same streaming-progress shape.
type jiraImportResult struct {
	baseImportResult
	Issues int `json:"issues"`
}

type jiraProgress struct {
	Result jiraImportResult
	Key    string
}

// jiraUpdatedLayout is the timestamp format Jira Cloud's REST API uses for
// issue "updated" fields, e.g. "2026-01-05T10:00:00.000+0100".
const jiraUpdatedLayout = "2006-01-02T15:04:05.000-0700"

// importJiraIssues fetches and ingests the selected issue keys (from a
// prior previewJiraIssues call).
func importJiraIssues(ctx context.Context, rag *ragSystem, s appSettings, cfg jiraConfig, embedModel string, selected map[string]bool, dryRun bool, onProgress func(jiraProgress)) (jiraImportResult, error) {
	var res jiraImportResult
	res.DryRun = dryRun
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	if verbose {
		log.Printf("[verbose] jira import: project=%s selected=%d dry_run=%v", cfg.ProjectKey, len(selected), dryRun)
	}

	for key := range selected {
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
		issue, err := fetchJiraIssue(ctx, cfg, key)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		res.Issues++
		var docDate int64
		if t, err := time.Parse(jiraUpdatedLayout, issue.Fields.Updated); err == nil {
			docDate = t.Unix()
		}
		sourceID := fmt.Sprintf("jira:%s:%s", cfg.ProjectKey, issue.Key)
		sourceName := fmt.Sprintf("%s: %s", issue.Key, issue.Fields.Summary)

		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "jira_issue", sourceName, jiraIssueText(issue), docDate, dryRun)
		foldIngestOutcome(outcome, err, &res.Errors, &res.Skipped, &res.Chunks)
		pacer.count()
		if onProgress != nil {
			onProgress(jiraProgress{Result: res, Key: issue.Key})
		}
	}
	return res, nil
}
