package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const githubDefaultBaseURL = "https://api.github.com"
const githubMaxResponseBytes int64 = 8 << 20

func githubBaseURL(cfg githubConfig) (*url.URL, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = githubDefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("github: base_url must be a valid https API URL without credentials/query")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func githubResolvedToken(cfg githubConfig) string { return resolveSecret(cfg.Token, cfg.TokenEnv) }

func githubRepositoryPath(cfg githubConfig) (string, error) {
	owner, repo := strings.TrimSpace(cfg.Owner), strings.TrimSpace(cfg.Repository)
	if owner == "" || repo == "" {
		return "", fmt.Errorf("github: owner and repository must be configured")
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo), nil
}

// githubGet allows only a GET to the configured API origin and explicitly
// refuses redirects. This matters more than it first appears: an automatic
// redirect could otherwise carry a repository token to a different host.
func githubGet(ctx context.Context, cfg githubConfig, endpoint string, query url.Values) ([]byte, error) {
	base, err := githubBaseURL(cfg)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(endpoint, "/") || strings.Contains(endpoint, "//") {
		return nil, fmt.Errorf("github: invalid API path")
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + endpoint
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	if token := githubResolvedToken(cfg); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", connectorUserAgent)
	raw, err := doWithRetryLimitedNoRedirect(req, false, githubMaxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("github GET %s: %w", endpoint, err)
	}
	return raw, nil
}

type githubIssue struct {
	ID        int64  `json:"id"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

func githubIssueText(item githubIssue, isPR bool) string {
	kind := "Issue"
	if isPR {
		kind = "Pull Request"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s #%d: %s\n", kind, item.Number, strings.TrimSpace(item.Title))
	fmt.Fprintf(&b, "Status: %s\n", strings.TrimSpace(item.State))
	if item.User.Login != "" {
		fmt.Fprintf(&b, "Autor: %s\n", item.User.Login)
	}
	if len(item.Labels) > 0 {
		labels := make([]string, 0, len(item.Labels))
		for _, l := range item.Labels {
			if name := strings.TrimSpace(l.Name); name != "" {
				labels = append(labels, name)
			}
		}
		if len(labels) > 0 {
			fmt.Fprintf(&b, "Labels: %s\n", strings.Join(labels, ", "))
		}
	}
	if item.HTMLURL != "" {
		fmt.Fprintf(&b, "URL: %s\n", item.HTMLURL)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(item.Body))
	return b.String()
}

func githubSourcePrefix(cfg githubConfig) (string, error) {
	base, err := githubBaseURL(cfg)
	if err != nil {
		return "", err
	}
	return "github:" + base.Host + ":" + strings.TrimSpace(cfg.Owner) + "/" + strings.TrimSpace(cfg.Repository), nil
}

func githubImportIssue(rag *ragSystem, s appSettings, cfg githubConfig, embedModel string, item githubIssue, dryRun bool) (ingestOutcome, error) {
	prefix, err := githubSourcePrefix(cfg)
	if err != nil {
		return ingestOutcome{}, err
	}
	isPR := item.PullRequest != nil
	kind, sourceKind := "issue", "github_issue"
	if isPR {
		kind, sourceKind = "pull", "github_pull_request"
	}
	var docDate int64
	if t, err := time.Parse(time.RFC3339, item.UpdatedAt); err == nil {
		docDate = t.Unix()
	}
	return ingestDocument(rag, s, embedModel,
		fmt.Sprintf("%s:%s:%d", prefix, kind, item.Number), sourceKind,
		fmt.Sprintf("%s #%d: %s", strings.Title(kind), item.Number, item.Title),
		githubIssueText(item, isPR), docDate, dryRun)
}

type githubReadme struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	HTMLURL  string `json:"html_url"`
}

func importGitHubReadme(ctx context.Context, rag *ragSystem, s appSettings, cfg githubConfig, embedModel string, dryRun bool) (ingestOutcome, error) {
	path, err := githubRepositoryPath(cfg)
	if err != nil {
		return ingestOutcome{}, err
	}
	raw, err := githubGet(ctx, cfg, path+"/readme", nil)
	if err != nil {
		return ingestOutcome{}, err
	}
	var readme githubReadme
	if err := json.Unmarshal(raw, &readme); err != nil {
		return ingestOutcome{}, fmt.Errorf("github: parse README: %w", err)
	}
	if !strings.EqualFold(readme.Encoding, "base64") {
		return ingestOutcome{}, fmt.Errorf("github: unsupported README encoding %q", readme.Encoding)
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(readme.Content, "\n", ""))
	if err != nil {
		return ingestOutcome{}, fmt.Errorf("github: decode README: %w", err)
	}
	prefix, err := githubSourcePrefix(cfg)
	if err != nil {
		return ingestOutcome{}, err
	}
	name := readme.Name
	if name == "" {
		name = "README"
	}
	return ingestDocument(rag, s, embedModel, prefix+":readme", "github_readme", name, string(content), 0, dryRun)
}

type githubSyncResult struct {
	baseImportResult
	Issues       int `json:"issues"`
	PullRequests int `json:"pull_requests"`
	Readmes      int `json:"readmes"`
}

// syncGitHubRepository imports a bounded run of GitHub's stable
// updated-ascending issue listing. State advances only when every attempted
// document succeeded, so a temporary embedding/API failure remains
// retryable. The persisted page counter makes a first-time repository import
// resumable; after its final page, CycleStartedAt becomes LastSyncedAt and
// later runs ask GitHub only for updates since that completed cycle.
func syncGitHubRepository(ctx context.Context, rag *ragSystem, s appSettings, cfg githubConfig, embedModel string, dryRun bool) (githubSyncResult, githubConfig, error) {
	res := githubSyncResult{baseImportResult: baseImportResult{DryRun: dryRun}}
	next := cfg
	if _, err := githubRepositoryPath(cfg); err != nil {
		return res, next, err
	}
	if githubResolvedToken(cfg) == "" {
		return res, next, fmt.Errorf("github: token not configured (set token or token_env)")
	}
	page := cfg.NextPage
	if page < 1 {
		page = 1
	}
	cycleStarted := cfg.CycleStartedAt
	if cycleStarted == "" {
		cycleStarted = time.Now().UTC().Format(time.RFC3339)
	}

	// README is one small, stable documentation source. Import it at the
	// beginning of a cycle; it is content-hash idempotent thereafter.
	if cfg.IncludeReadme && page == 1 {
		outcome, err := importGitHubReadme(ctx, rag, s, cfg, embedModel, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, "README: "+err.Error())
		} else if outcome.Skipped {
			res.Skipped++
		} else {
			res.Readmes++
			res.Chunks += outcome.Chunks
		}
	}

	limit := cfg.effectiveMaxItems(s.Import)
	path, _ := githubRepositoryPath(cfg)
	complete := false
	for processed := 0; processed < limit && !complete; {
		perPage := limit - processed
		if perPage > 100 {
			perPage = 100
		}
		q := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if cfg.LastSyncedAt != "" {
			q.Set("since", cfg.LastSyncedAt)
		}
		raw, err := githubGet(ctx, cfg, path+"/issues", q)
		if err != nil {
			return res, cfg, err
		}
		var items []githubIssue
		if err := json.Unmarshal(raw, &items); err != nil {
			return res, cfg, fmt.Errorf("github: parse issues: %w", err)
		}
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return res, cfg, err
			}
			isPR := item.PullRequest != nil
			if (isPR && !cfg.IncludePullRequests) || (!isPR && !cfg.IncludeIssues) {
				processed++
				continue
			}
			outcome, err := githubImportIssue(rag, s, cfg, embedModel, item, dryRun)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("#%d: %v", item.Number, err))
			} else if outcome.Skipped {
				res.Skipped++
			} else if isPR {
				res.PullRequests++
				res.Chunks += outcome.Chunks
			} else {
				res.Issues++
				res.Chunks += outcome.Chunks
			}
			processed++
		}
		if len(items) < perPage {
			complete = true
		} else {
			page++
		}
	}
	if len(res.Errors) > 0 {
		// Do not commit README/page progress past a failed record. Existing
		// successful documents are idempotent, so retrying is safe.
		return res, cfg, nil
	}
	if complete {
		next.LastSyncedAt = cycleStarted
		next.CycleStartedAt = ""
		next.NextPage = 0
	} else {
		next.CycleStartedAt = cycleStarted
		next.NextPage = page
	}
	return res, next, nil
}
