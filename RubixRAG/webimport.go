package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Generic website page import — no settings, no auth: an admin pastes one
// or more URLs, R3 fetches each over plain HTTP(S) and extracts text via
// htmlToText (extract.go), the same tag-stripping already used for
// Exchange/Teams HTML bodies and Confluence's storage format.
//
// SSRF: this is the one connector where R3 fetches a URL chosen by the
// caller at request time, rather than a URL derived from a pre-configured,
// already-trusted source (a specific mailbox/site/space/channel). A
// malicious or compromised admin session could otherwise use it to make
// the server fetch internal-only endpoints (cloud metadata services,
// internal admin panels, ...) and then read the result back through chat
// citations — citations are NOT admin-gated (see handleSourceContent), so
// anything ingested this way becomes readable by any R3 user, not just
// admins. isSafeWebURL plus the fetch transport block unsafe schemes,
// credentials, private/link-local addresses, unsafe redirect targets and DNS
// rebinding at the actual dial step. Treat this connector as sensitive
// admin-controlled input nevertheless: public pages can still contain hostile
// instructions, which are data and never executable commands.
// ─────────────────────────────────────────────────────────────────────────────

// webAllowPrivateHosts disables the private/loopback address check below.
// Only ever set by tests — there's no reachable public web server in this
// sandbox, so tests fetch from an httptest server on 127.0.0.1 instead.
var webAllowPrivateHosts = false

// isSafeWebURL rejects non-http(s) schemes and hosts that resolve to a
// loopback/private/link-local address. Its matching dial-time and redirect
// checks in newSafeWebFetchClient complete the SSRF defense for this
// connector. Equivalent to isSafeFetchURL(raw, false) — kept as the plain
// name since every caller except agent.go's fetch_url tool always wants the
// public-only check.
func isSafeWebURL(raw string) (*url.URL, error) {
	return isSafeFetchURL(raw, false)
}

// isSafeFetchURL is isSafeWebURL with one extra knob: allowPrivate lets a
// RFC1918/ULA host through (settings.Import.AllowInternalFetch, checked by
// agent.go's fetchURLExecutor) for pasting internal wiki/ticket/SharePoint
// links into chat. Loopback, link-local (incl. cloud instance-metadata
// services), unspecified, and multicast addresses are refused either way —
// see importConfig.AllowInternalFetch's doc comment for why those stay
// blocked regardless of the setting.
func isSafeFetchURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http/https URLs are allowed")
	}
	if u.User != nil {
		return nil, fmt.Errorf("URLs with embedded credentials are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve host: %w", err)
	}
	for _, ip := range ips {
		if err := validateFetchIP(host, ip, allowPrivate); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// validateFetchIP is shared by the initial URL validation and the transport's
// actual dial step. Resolving once and then dialing the hostname leaves a DNS
// rebinding window; validating the resolved address and dialing that exact IP
// closes it. webAllowPrivateHosts is intentionally test-only and retains the
// existing local httptest behavior.
func validateFetchIP(host string, ip net.IP, allowPrivate bool) error {
	if webAllowPrivateHosts {
		return nil
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("refusing to fetch %s: resolves to a loopback/link-local address (%s)", host, ip)
	}
	if ip.IsPrivate() && !allowPrivate {
		return fmt.Errorf("refusing to fetch %s: resolves to a private address (%s) — enable settings.import.allow_internal_fetch to allow internal hosts", host, ip)
	}
	return nil
}

const webFetchTimeout = 30 * time.Second

var (
	publicWebFetchClient   = newSafeWebFetchClient(false)
	internalWebFetchClient = newSafeWebFetchClient(true)
)

// newSafeWebFetchClient builds the client used only for caller/model-selected
// web pages. It deliberately bypasses environment proxies: a proxy resolves
// the final host itself, which would defeat the address-level SSRF check below.
// Credentialed, admin-configured API connectors keep their existing proxy
// behavior in connector.go; this is the stricter path for arbitrary URLs.
func newSafeWebFetchClient(allowPrivate bool) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = safeFetchDialContext(allowPrivate)
	return &http.Client{
		Transport: tracingTransport{base: base},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			_, err := isSafeFetchURL(req.URL.String(), allowPrivate)
			return err
		},
	}
}

func safeFetchDialContext(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split fetch address %q: %w", address, err)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve host %q: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("resolve host %q: no addresses", host)
		}
		for _, ip := range ips {
			if err := validateFetchIP(host, ip, allowPrivate); err != nil {
				return nil, err
			}
		}
		// Dial a resolved IP rather than the hostname so the connection uses
		// precisely the address that passed validation above. TLS still uses
		// the request hostname for SNI/certificate verification.
		dialer := net.Dialer{}
		var lastErr error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

func webFetchClient(allowPrivate bool) *http.Client {
	if allowPrivate {
		return internalWebFetchClient
	}
	return publicWebFetchClient
}

var titleTagRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// extractHTMLTitle returns the content of the first <title> tag, or "" if
// none is found — fetchWebPage falls back to the URL itself in that case.
func extractHTMLTitle(rawHTML string) string {
	m := titleTagRe.FindStringSubmatch(rawHTML)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(m[1]))
}

// fetchWebPageRaw fetches rawURL (after isSafeWebURL clears it) and
// returns the resolved URL plus the raw response body — shared by
// fetchWebPage (text extraction) and crawlWebPages (link discovery), so
// crawling a page for its links and ingesting its text share one fetch
// instead of hitting the same URL twice. maxBytes bounds how much of the
// response body is read, mirroring extractText's MaxFileMB guard for
// uploaded files. It uses this file's direct, dial-validated client and a
// fixed 30s per-page deadline — no retry, unlike the Basic-auth REST
// connectors: an arbitrary admin-supplied web page isn't a well-known,
// rate-limited API, so a 5xx here is more often a real error than a transient
// one worth retrying.
func fetchWebPageRaw(ctx context.Context, rawURL string, maxBytes int64, allowPrivate bool) (u *url.URL, body string, err error) {
	if maxBytes <= 0 {
		return nil, "", fmt.Errorf("max response size must be positive")
	}
	u, err = isSafeFetchURL(rawURL, allowPrivate)
	if err != nil {
		return nil, "", err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", connectorUserAgent)
	resp, err := webFetchClient(allowPrivate).Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, "", fmt.Errorf("fetch: response exceeds %d bytes", maxBytes)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(strings.ToLower(ct), "html") && !strings.Contains(strings.ToLower(ct), "text") {
		return nil, "", fmt.Errorf("unsupported content type %q", ct)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, "", fmt.Errorf("fetch: response exceeds %d bytes", maxBytes)
	}
	return u, string(raw), nil
}

// fetchWebPage downloads rawURL and returns its title (best-effort, from
// <title>) and extracted plain text — see fetchWebPageRaw for the actual
// fetch/SSRF-guard/size-cap behavior. allowPrivate mirrors fetchWebPageRaw's
// same-named parameter (false for every caller except agent.go's fetch_url
// tool, gated there behind settings.Import.AllowInternalFetch).
func fetchWebPage(ctx context.Context, rawURL string, maxBytes int64, allowPrivate bool) (title, text string, err error) {
	u, body, err := fetchWebPageRaw(ctx, rawURL, maxBytes, allowPrivate)
	if err != nil {
		return "", "", err
	}
	title = extractHTMLTitle(body)
	if title == "" {
		title = u.String()
	}
	text = htmlToText(body)
	return title, text, nil
}

// fetchWebPageForResearch fetches rawURL like fetchWebPage, but also
// returns the page's discovered links (extractHTMLLinks) — used only by
// the web-research sub-agent's own fetch tool (agent.go), never by the
// plain fetch_url tool, which must stay single-page/no-link-following
// per its own doc comment. Always public-only (allowPrivate=false): the
// research sub-agent follows links it discovers itself, so it must never
// be steered onto an internal host this way.
func fetchWebPageForResearch(ctx context.Context, rawURL string, maxBytes int64) (title, text string, links []string, err error) {
	u, body, err := fetchWebPageRaw(ctx, rawURL, maxBytes, false)
	if err != nil {
		return "", "", nil, err
	}
	title = extractHTMLTitle(body)
	if title == "" {
		title = u.String()
	}
	return title, htmlToText(body), extractHTMLLinks(body, u), nil
}

// hrefRe matches an <a href="..."> target — used by extractHTMLLinks for
// crawl-mode link discovery.
// aTagRe finds whole <a ...> opening tags; hrefAttrRe then looks for the
// href attribute *within* one such tag, requiring it to be preceded by
// whitespace or the tag's start — a single combined regex here would
// also match "href" as a mere substring of an unrelated attribute name
// (e.g. a stray "not-href=" or "data-href=" attribute), which real HTML
// occasionally has.
var aTagRe = regexp.MustCompile(`(?is)<a\s[^>]*>`)
var hrefAttrRe = regexp.MustCompile(`(?is)(?:^|\s)href\s*=\s*["']([^"']+)["']`)

// extractHTMLLinks returns every same-document <a href> target in
// rawHTML, resolved against base into absolute http/https URLs —
// non-http(s) schemes (mailto:, tel:, javascript:, ...), empty hrefs, and
// pure same-page fragment links (#anchor) are skipped; fragments are
// stripped from kept links so "/page#a" and "/page#b" aren't crawled as
// two pages. Deduped within one page's link set — crawlWebPages dedupes
// again globally against the whole crawl's visited set.
func extractHTMLLinks(rawHTML string, base *url.URL) []string {
	seen := map[string]bool{}
	var out []string
	for _, tag := range aTagRe.FindAllString(rawHTML, -1) {
		m := hrefAttrRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		href := strings.TrimSpace(html.UnescapeString(m[1]))
		if href == "" || strings.HasPrefix(href, "#") {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		abs := base.ResolveReference(ref)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			continue
		}
		abs.Fragment = ""
		s := abs.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// webImportResult/webProgress mirror pst.go's pstImportResult/pstProgress
// — same streaming-progress shape used by handlers.go's NDJSON endpoint.
type webImportResult struct {
	baseImportResult
	Pages int `json:"pages"`
}

type webProgress struct {
	Result webImportResult
	URL    string
}

// importWebPages fetches and ingests each URL in urls.
func importWebPages(ctx context.Context, rag *ragSystem, s appSettings, embedModel string, urls []string, dryRun bool, onProgress func(webProgress)) webImportResult {
	var res webImportResult
	res.DryRun = dryRun
	if verbose {
		log.Printf("[verbose] web import: %d url(s) dry_run=%v", len(urls), dryRun)
	}
	maxBytes := s.Import.MaxFileMB
	if maxBytes <= 0 {
		maxBytes = 25
	}
	maxBytes *= 1024 * 1024

	pacer := newImportPacer(s.Import, 0)
	for _, raw := range urls {
		if err := ctx.Err(); err != nil {
			res.Errors = append(res.Errors, err.Error())
			break
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			res.Errors = append(res.Errors, err.Error())
			break
		}
		title, text, err := fetchWebPage(ctx, raw, maxBytes, false)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", raw, err))
			if onProgress != nil {
				onProgress(webProgress{Result: res, URL: raw})
			}
			continue
		}
		res.Pages++
		sourceID := "web:" + raw
		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "web_page", title, text, 0, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", raw, err))
		} else if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
		pacer.count()
		if onProgress != nil {
			onProgress(webProgress{Result: res, URL: raw})
		}
	}
	return res
}

// crawlDefaultMaxDepth/crawlDefaultMaxPages apply when a caller omits (<=
// 0) the corresponding field — handlers.go's handleWebImport passes
// client-supplied values straight through. crawlMaxDepthCeiling/
// crawlMaxPagesCeiling hard-clamp the *requested* value regardless of
// what's asked for — a crawl is triggered by an admin, but still worth
// bounding server-side against a typo'd or unbounded request, the same
// "trust but cap" posture importConfig's MaxItemsPerRun already applies
// to every connector.
const (
	crawlDefaultMaxDepth = 2
	crawlDefaultMaxPages = 20
	crawlMaxDepthCeiling = 5
	crawlMaxPagesCeiling = 200
)

// crawlQueueItem is one pending fetch in crawlWebPages' BFS queue.
type crawlQueueItem struct {
	url   string
	depth int
}

// crawlWebPages breadth-first crawls from seeds, following same-page
// links up to maxDepth hops and maxPages total fetched pages (both
// hard-clamped to crawlMaxDepthCeiling/crawlMaxPagesCeiling regardless of
// what's requested). allowOtherHosts=false (the default) restricts every
// hop to its seed's own host — a link to a different domain is
// discovered but never fetched, the same "stay on this site" default any
// general-purpose crawler ships with. Each fetched page is ingested
// exactly like importWebPages does (ingestDocument, same "web:"+url
// source_id scheme, same dry-run/pacer/progress-callback shape) — this
// is still the same admin-trusted-input connector described at the top
// of this file; isSafeWebURL guards every single hop's fetch identically
// to a flat URL list, it's just reached automatically instead of by the
// admin pasting each one.
func crawlWebPages(ctx context.Context, rag *ragSystem, s appSettings, embedModel string, seeds []string, maxDepth, maxPages int, allowOtherHosts, dryRun bool, onProgress func(webProgress)) webImportResult {
	var res webImportResult
	res.DryRun = dryRun
	if maxDepth <= 0 {
		maxDepth = crawlDefaultMaxDepth
	} else if maxDepth > crawlMaxDepthCeiling {
		maxDepth = crawlMaxDepthCeiling
	}
	if maxPages <= 0 {
		maxPages = crawlDefaultMaxPages
	} else if maxPages > crawlMaxPagesCeiling {
		maxPages = crawlMaxPagesCeiling
	}
	if verbose {
		log.Printf("[verbose] web crawl: %d seed(s) max_depth=%d max_pages=%d allow_other_hosts=%v dry_run=%v", len(seeds), maxDepth, maxPages, allowOtherHosts, dryRun)
	}
	maxBytes := s.Import.MaxFileMB
	if maxBytes <= 0 {
		maxBytes = 25
	}
	maxBytes *= 1024 * 1024

	var queue []crawlQueueItem
	seedHosts := map[string]bool{}
	visited := map[string]bool{}
	for _, raw := range seeds {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil {
			seedHosts[u.Hostname()] = true
		}
		queue = append(queue, crawlQueueItem{url: raw, depth: 0})
	}

	pacer := newImportPacer(s.Import, 0)
	for len(queue) > 0 && res.Pages < maxPages {
		if err := ctx.Err(); err != nil {
			res.Errors = append(res.Errors, err.Error())
			break
		}
		item := queue[0]
		queue = queue[1:]
		if visited[item.url] {
			continue
		}
		visited[item.url] = true
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			res.Errors = append(res.Errors, err.Error())
			break
		}

		u, body, err := fetchWebPageRaw(ctx, item.url, maxBytes, false)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.url, err))
			if onProgress != nil {
				onProgress(webProgress{Result: res, URL: item.url})
			}
			continue
		}
		title := extractHTMLTitle(body)
		if title == "" {
			title = u.String()
		}
		text := htmlToText(body)

		res.Pages++
		sourceID := "web:" + item.url
		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "web_page", title, text, 0, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.url, err))
		} else if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
		pacer.count()
		if onProgress != nil {
			onProgress(webProgress{Result: res, URL: item.url})
		}

		if item.depth >= maxDepth || res.Pages >= maxPages {
			continue
		}
		for _, link := range extractHTMLLinks(body, u) {
			if visited[link] {
				continue
			}
			if !allowOtherHosts {
				lu, err := url.Parse(link)
				if err != nil || !seedHosts[lu.Hostname()] {
					continue
				}
			}
			queue = append(queue, crawlQueueItem{url: link, depth: item.depth + 1})
		}
	}
	return res
}
