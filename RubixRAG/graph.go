package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared Microsoft Graph plumbing: token acquisition (app-only client-
// credentials flow) and authenticated GET, used by every Graph-backed
// connector — SharePoint (sharepoint.go), Exchange Online mail
// (graphmail.go), and Teams (teams.go). Each connector keeps its own
// tenant/client/secret fields in settings.go (matching how the "local"
// and "azure" LLM profiles already don't share a base struct — a real
// deployment can just paste the same app-registration values into all
// three if it uses one shared app, or different ones per connector), but
// they all funnel through the same HTTP mechanics here.
//
// Plain net/http rather than an Azure SDK: the token endpoint and Graph
// API are both simple REST/JSON, and the total call surface across all
// three connectors is small — not enough to justify a heavier dependency.
// ─────────────────────────────────────────────────────────────────────────────

// graphBaseURL/graphAuthHost are vars, not consts, so tests can point
// them at a fake server instead of the real Microsoft endpoints (there's
// no real Azure AD tenant reachable from an automated test environment).
var (
	graphBaseURL  = "https://graph.microsoft.com/v1.0"
	graphAuthHost = "https://login.microsoftonline.com"
)

// graphHTTPClient carries every Graph request (token + API) — a named
// client instead of http.DefaultClient/http.PostForm so conntest.go's
// "Verbindung testen" can capture the raw exchange via the context-gated
// tracingTransport (conntrace.go). No client-level timeout: imports can
// legitimately run long, and every call site now passes a context that
// bounds it appropriately instead.
var graphHTTPClient = &http.Client{Transport: tracingTransport{}}

// graphCreds is the minimal Azure AD app-registration credential set any
// Graph-based connector needs.
type graphCreds struct {
	TenantID        string
	ClientID        string
	ClientSecret    string
	ClientSecretEnv string
}

// resolvedSecret prefers the env-var-named secret over the inline one, so
// a deployment can avoid committing the client secret to settings.json.
func (c graphCreds) resolvedSecret() string {
	return resolveSecret(c.ClientSecret, c.ClientSecretEnv)
}

// cacheKey identifies the graphTokens entry for this tenant+client pair, so
// separate connectors sharing the same app registration also share one
// cached token instead of each acquiring their own.
func (c graphCreds) cacheKey() string {
	return c.TenantID + "|" + c.ClientID
}

// graphTokenCache holds the last access token for one tenant+client pair,
// refreshed slightly before actual expiry so a request never races a
// just-expired token.
type graphTokenCache struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

var (
	graphTokensMu sync.Mutex
	graphTokens   = map[string]*graphTokenCache{}
)

// graphMaxRetries bounds how many extra attempts graphGet/graphAccessToken
// make after a 429 (rate limit) or 5xx (transient) response before giving
// up. Microsoft Graph throttles aggressively under load (see ANLEITUNG.md's
// "SharePoint" section) — a large import job aborting outright on the
// first 429 instead of backing off a few seconds is the concrete failure
// mode this guards against. A var, not a const, so a test exhausting all
// retries can temporarily lower it instead of sleeping through the full
// real backoff schedule. This is the FALLBACK default when
// importConfig.GraphMaxRetries (settings.go) isn't set — see
// graphMaxRetriesLimit, which every retry loop in this file actually
// calls.
var graphMaxRetries = 4

// graphMaxRetriesLimit resolves the effective retry bound: the configured
// importConfig.GraphMaxRetries when positive, else the graphMaxRetries
// fallback above (which tests still mutate directly, since a configured
// settings value of 0 correctly falls through to it rather than to a
// hardcoded number here). A heavily-throttled large Graph tenant may need
// more than 4 retries to ride out sustained 429s unattended; an admin
// troubleshooting against a flaky test/staging tenant may want fewer, to
// fail fast instead of waiting through several rounds of exponential
// backoff (up to 30s each).
func graphMaxRetriesLimit() int {
	if n := settings.get().Import.GraphMaxRetries; n > 0 {
		return n
	}
	return graphMaxRetries
}

// parseRetryAfter reads the Retry-After response header (seconds, per
// RFC 9110) — 0 if absent or unparseable, in which case graphBackoff falls
// back to exponential backoff instead of honoring the server's own hint.
func parseRetryAfter(h http.Header) time.Duration {
	secs, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// graphBackoff picks how long to wait before the next retry attempt: the
// server's own Retry-After if it sent one, otherwise exponential backoff
// (500ms, 1s, 2s, 4s, ...) capped at 30s. A pure function so tests can
// assert on the chosen duration without an actual Sleep.
func graphBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := 500 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// graphBackoffJitter adds up to 25% random jitter on top of graphBackoff's
// exponential delay, so several goroutines retrying against the same
// throttled tenant at once (e.g. two SharePoint connections, or a
// SharePoint and an Exchange/Teams connection sharing one app
// registration) don't all wake up and re-fire at the exact same instant,
// compounding the throttle instead of spreading out from it. Never
// applied when the server sent its own Retry-After — that's an explicit
// server directive, not a value to randomize.
func graphBackoffJitter(attempt int, retryAfter time.Duration) time.Duration {
	d := graphBackoff(attempt, retryAfter)
	if retryAfter > 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(int64(d)/4+1))
}

// graphAccessToken returns a cached app-only Graph token for creds,
// acquiring a fresh one via the OAuth2 client-credentials flow when
// missing or close to expiry. Retries the token request itself on 429/5xx
// (see graphMaxRetries) — the token endpoint is throttled independently of
// the Graph API proper, so a busy tenant can rate-limit logins too.
func graphAccessToken(ctx context.Context, creds graphCreds) (string, error) {
	if creds.TenantID == "" || creds.ClientID == "" {
		return "", fmt.Errorf("graph: tenant_id/client_id not configured")
	}
	secret := creds.resolvedSecret()
	if secret == "" {
		return "", fmt.Errorf("graph: client secret not configured (set client_secret or client_secret_env)")
	}

	key := creds.cacheKey()
	graphTokensMu.Lock()
	cache, ok := graphTokens[key]
	if !ok {
		cache = &graphTokenCache{}
		graphTokens[key] = cache
	}
	graphTokensMu.Unlock()

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.token != "" && time.Now().Before(cache.expires) {
		return cache.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {creds.ClientID},
		"client_secret": {secret},
		"scope":         {"https://graph.microsoft.com/.default"},
	}
	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", graphAuthHost, url.PathEscape(creds.TenantID))

	maxRetries := graphMaxRetriesLimit()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", fmt.Errorf("graph: build token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", connectorUserAgent)
		resp, err := graphHTTPClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("graph: token request failed: %w", err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("graph: read token response: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			var tok struct {
				AccessToken string `json:"access_token"`
				ExpiresIn   int    `json:"expires_in"`
			}
			if err := json.Unmarshal(raw, &tok); err != nil {
				return "", fmt.Errorf("graph: parse token response: %w", err)
			}
			cache.token = tok.AccessToken
			cache.expires = time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second)
			return cache.token, nil
		}
		lastErr = fmt.Errorf("graph: token request %d: %s", resp.StatusCode, string(raw))
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < maxRetries {
			select {
			case <-time.After(graphBackoffJitter(attempt, parseRetryAfter(resp.Header))):
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "", lastErr
	}
	return "", lastErr
}

// graphGetOnce performs a single authenticated GET against
// graphBaseURL+path, no retry — graphGet wraps this with retry/backoff.
func graphGetOnce(ctx context.Context, token, path string) (raw []byte, status int, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphBaseURL+path, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", connectorUserAgent)
	resp, err := graphHTTPClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, 0, err
	}
	return raw, resp.StatusCode, parseRetryAfter(resp.Header), nil
}

// graphStatusError carries the HTTP status of a failed Graph call so a
// caller can distinguish specific codes (e.g. 410 Gone on an expired
// delta token, see graphIsGone) without parsing the formatted error
// string. Wraps the same message every caller previously built by hand,
// so existing error-string-based logging is unaffected.
type graphStatusError struct {
	status int
	msg    string
}

func (e *graphStatusError) Error() string { return e.msg }

// graphIsGone reports whether err came from a Graph call that returned
// HTTP 410 Gone — the documented response when a delta query's resume
// token has been invalidated (error codes like resyncChangesApplyDifferences/
// resyncChangesUploadDifferences), requiring the caller to restart
// enumeration from scratch rather than retry the same request.
func graphIsGone(err error) bool {
	var se *graphStatusError
	return errors.As(err, &se) && se.status == http.StatusGone
}

// graphGet performs an authenticated GET against graphBaseURL+path,
// retrying on 429/5xx with backoff (see graphBackoff/graphMaxRetries). A
// network-level error (no response at all) is returned immediately, not
// retried — that's usually not a transient server-side condition.
func graphGet(ctx context.Context, token, path string) ([]byte, error) {
	maxRetries := graphMaxRetriesLimit()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		raw, status, retryAfter, err := graphGetOnce(ctx, token, path)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			return raw, nil
		}
		lastErr = &graphStatusError{status: status, msg: fmt.Sprintf("graph GET %s: %d: %s", path, status, string(raw))}
		if (status == http.StatusTooManyRequests || status >= 500) && attempt < maxRetries {
			select {
			case <-time.After(graphBackoffJitter(attempt, retryAfter)):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, lastErr
	}
	return nil, lastErr
}

// graphWriteOnce performs a single authenticated POST/PATCH against
// graphBaseURL+path with a JSON body (nil for an empty body, e.g.
// createReply's POST with no payload), no retry — graphWrite wraps this
// with the same retry/backoff graphGet uses. Shared by every Graph WRITE
// call site (today: graphmail.go's draft creation) so a future write path
// doesn't reinvent the retry policy — but note this is plumbing only, not
// a safety boundary: it has no notion of "send" vs "draft", every call
// site is individually responsible for only ever pointing `method`/`path`
// at a draft-creating or draft-editing endpoint (see graphmail.go's
// createExchangeGraphDraft doc comment for the actual guarantee).
func graphWriteOnce(ctx context.Context, method, token, path string, body []byte) (raw []byte, status int, retryAfter time.Duration, err error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, graphBaseURL+path, bodyReader)
	if err != nil {
		return nil, 0, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", connectorUserAgent)
	resp, err := graphHTTPClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, 0, err
	}
	return raw, resp.StatusCode, parseRetryAfter(resp.Header), nil
}

// graphWrite performs an authenticated POST/PATCH against graphBaseURL+path,
// retrying on 429/5xx with backoff exactly like graphGet, and accepting any
// 2xx status as success (POST .../createReply and .../messages both reply
// 201 Created; PATCH .../messages replies 200 OK) rather than only 200.
func graphWrite(ctx context.Context, method, token, path string, body []byte) ([]byte, error) {
	maxRetries := graphMaxRetriesLimit()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		raw, status, retryAfter, err := graphWriteOnce(ctx, method, token, path, body)
		if err != nil {
			return nil, err
		}
		if status >= 200 && status < 300 {
			return raw, nil
		}
		lastErr = fmt.Errorf("graph %s %s: %d: %s", method, path, status, string(raw))
		if (status == http.StatusTooManyRequests || status >= 500) && attempt < maxRetries {
			select {
			case <-time.After(graphBackoffJitter(attempt, retryAfter)):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, lastErr
	}
	return nil, lastErr
}

// graphDoWithRetry executes req (headers/auth/body already set by the
// caller — every caller here is a bodyless GET, so req is safe to reuse
// across retry attempts) via graphHTTPClient, retrying on 429/5xx with the
// same Retry-After-aware jittered backoff graphGet/graphWrite use. Exists
// for calls that don't fit graphGet/graphWrite's graphBaseURL+path shape —
// e.g. SharePoint's short-lived pre-signed downloadUrl, an absolute URL
// outside graphBaseURL needing no Authorization header at all (see
// sharepoint.go's spDownloadFile/spDownloadItemContent) — so file-content
// downloads get the same throttling resilience as every metadata call
// instead of failing outright on a single transient 429/503. A
// network-level error (no response at all) is returned immediately, never
// retried — that's usually not a transient server-side condition.
func graphDoWithRetry(req *http.Request) ([]byte, error) {
	maxRetries := graphMaxRetriesLimit()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := graphHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return raw, nil
		}
		lastErr = &graphStatusError{status: resp.StatusCode, msg: fmt.Sprintf("%d: %s", resp.StatusCode, string(raw))}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < maxRetries {
			select {
			case <-time.After(graphBackoffJitter(attempt, parseRetryAfter(resp.Header))):
				continue
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return nil, lastErr
	}
	return nil, lastErr
}
