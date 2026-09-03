package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Wikipedia fetcher
// ─────────────────────────────────────────────────────────────────────────────

func fetchWikipedia(article, lang string) (string, error) {
	u := fmt.Sprintf(
		"https://%s.wikipedia.org/w/api.php?action=query&prop=extracts&explaintext=1&titles=%s&format=json",
		lang, url.QueryEscape(article),
	)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	client := newExternalHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Wikipedia API returned HTTP %d for %q", resp.StatusCode, article)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "json") {
		return "", fmt.Errorf("Wikipedia API returned unexpected content-type %q for %q", ct, article)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var result struct {
		Query struct {
			Pages map[string]struct {
				Title   string `json:"title"`
				Extract string `json:"extract"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("Wikipedia JSON parse error for %q: %w", article, err)
	}
	for _, p := range result.Query.Pages {
		if p.Extract == "" {
			return "", fmt.Errorf("Wikipedia article %q has no content", article)
		}
		return p.Extract, nil
	}
	return "", fmt.Errorf("no pages found for %q", article)
}

// searchWikipedia performs a MediaWiki search and returns a slice of simple results
func searchWikipedia(query, lang string) ([]map[string]string, error) {
	if lang == "" {
		lang = "de"
	}
	apiURL := fmt.Sprintf("https://%s.wikipedia.org/w/api.php?action=query&list=search&srsearch=%s&utf8=&format=json&srlimit=10", lang, url.QueryEscape(query))
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	client := newHTTPClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("wikipedia search returned status %d", resp.StatusCode)
	}
	var root struct {
		Query struct {
			Search []struct {
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
				PageID  int    `json:"pageid"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(root.Query.Search))
	for _, s := range root.Query.Search {
		out = append(out, map[string]string{"title": s.Title, "snippet": s.Snippet, "pageid": fmt.Sprintf("%d", s.PageID)})
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Generic web scraper
// ─────────────────────────────────────────────────────────────────────────────

// maxToolResultRows caps the number of rows returned by sql_query and the
// maximum k value for vector_query to keep LLM context windows manageable.
const maxToolResultRows = 50
const maxFetchBodyBytes int64 = 2 * 1024 * 1024 // 2 MiB hard cap to limit untrusted remote payload size.

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)
var multiSpaceRe = regexp.MustCompile(`\s{3,}`)

// fetchURL retrieves and heuristically strips HTML from a URL,
// returning plain text suitable for chunking and embedding.
func fetchURL(rawURL string) (string, error) {
	return fetchURLCtx(context.Background(), rawURL)
}

// fetchURLCtx is the cancellable variant used by agent tool runs. Keeping the
// legacy wrapper preserves existing callers that do not carry a request
// context, while streamed requests stop the HTTP operation on cancellation.
func fetchURLCtx(ctx context.Context, rawURL string) (string, error) {
	if err := isSafeFetchURL(rawURL); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1")
	client := newExternalHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d for %s", resp.StatusCode, rawURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBodyBytes))
	if err != nil {
		return "", err
	}
	text := string(body)

	// Strip script/style and some layout blocks
	for _, tag := range []string{"script", "style", "nav", "footer", "header"} {
		re := regexp.MustCompile(`(?is)<` + tag + `[^>]*>.*?</` + tag + `>`)
		text = re.ReplaceAllString(text, " ")
	}
	text = htmlTagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = multiSpaceRe.ReplaceAllString(text, "\n")
	text = strings.TrimSpace(text)

	if len(text) < 50 {
		return "", fmt.Errorf("page too short after stripping HTML (%d chars)", len(text))
	}
	return text, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DuckDuckGo Instant Answer (fallback to HTML snippets)
// ─────────────────────────────────────────────────────────────────────────────

// fetchDuckDuckGo queries DuckDuckGo Instant Answer API and falls
// back to scraping HTML snippets when needed, returning markdown-ish text.
func fetchDuckDuckGo(query string) (string, error) {
	u := fmt.Sprintf(
		"https://api.duckduckgo.com/?q=%s&format=json&no_html=1&skip_disambig=1",
		url.QueryEscape(query),
	)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	client := newHTTPClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var result struct {
		Abstract       string `json:"Abstract"`
		AbstractSource string `json:"AbstractSource"`
		AbstractURL    string `json:"AbstractURL"`
		Heading        string `json:"Heading"`
		Answer         string `json:"Answer"`
		RelatedTopics  []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
		} `json:"RelatedTopics"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	var parts []string
	if result.Heading != "" {
		parts = append(parts, "# "+result.Heading)
	}
	if result.Abstract != "" {
		parts = append(parts, result.Abstract)
		if result.AbstractSource != "" {
			parts = append(parts, fmt.Sprintf("(Quelle: %s — %s)", result.AbstractSource, result.AbstractURL))
		}
	}
	if result.Answer != "" {
		parts = append(parts, "Antwort: "+result.Answer)
	}
	for i, rt := range result.RelatedTopics {
		if i >= 5 {
			break
		}
		if rt.Text != "" {
			parts = append(parts, "- "+rt.Text)
		}
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) != "" {
		return text, nil
	}

	// Fallback: scrape DuckDuckGo HTML search results
	htmlURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(query))
	htmlReq, err := http.NewRequest("GET", htmlURL, nil)
	if err != nil {
		return "", fmt.Errorf("DuckDuckGo returned no results for %q", query)
	}
	htmlReq.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	htmlResp, err := client.Do(htmlReq)
	if err != nil {
		return "", fmt.Errorf("DuckDuckGo HTML fallback failed: %w", err)
	}
	defer htmlResp.Body.Close()
	htmlBody, err := io.ReadAll(htmlResp.Body)
	if err != nil {
		return "", err
	}
	snippetRe := regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	matches := snippetRe.FindAllStringSubmatch(string(htmlBody), 10)
	var snippets []string
	for _, m := range matches {
		s := htmlTagRe.ReplaceAllString(m[1], "")
		s = html.UnescapeString(strings.TrimSpace(s))
		if s != "" {
			snippets = append(snippets, "- "+s)
		}
	}
	if len(snippets) == 0 {
		return "", fmt.Errorf("DuckDuckGo returned no results for %q", query)
	}
	return fmt.Sprintf("DuckDuckGo-Suchergebnisse für \"%s\":\n\n%s", query, strings.Join(snippets, "\n")), nil
}

func fetchWikidata(query string) (string, error) {
	u := fmt.Sprintf(
		"https://www.wikidata.org/w/api.php?action=wbsearchentities&search=%s&language=en&format=json&limit=5",
		url.QueryEscape(query),
	)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	resp, err := newHTTPClient(20 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("wikidata search HTTP %d", resp.StatusCode)
	}
	var root struct {
		Search []struct {
			ID          string `json:"id"`
			Label       string `json:"label"`
			Description string `json:"description"`
			ConceptURI  string `json:"concepturi"`
		} `json:"search"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return "", err
	}
	if len(root.Search) == 0 {
		return "", fmt.Errorf("no wikidata results for %q", query)
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("Wikidata-Ergebnisse für %q:", query))
	for i, item := range root.Search {
		if i >= 5 {
			break
		}
		line := fmt.Sprintf("- %s (%s)", item.Label, item.ID)
		if item.Description != "" {
			line += ": " + item.Description
		}
		if item.ConceptURI != "" {
			line += " — " + item.ConceptURI
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n"), nil
}

func fetchGitHub(query string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/search/repositories?q=%s&per_page=5", url.QueryEscape(query))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := newHTTPClient(20 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("github search HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var root struct {
		Items []struct {
			FullName        string `json:"full_name"`
			Description     string `json:"description"`
			HTMLURL         string `json:"html_url"`
			StargazersCount int    `json:"stargazers_count"`
			Language        string `json:"language"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return "", err
	}
	if len(root.Items) == 0 {
		return "", fmt.Errorf("no github results for %q", query)
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("GitHub-Repositories für %q:", query))
	for i, item := range root.Items {
		if i >= 5 {
			break
		}
		line := fmt.Sprintf("- %s", item.FullName)
		if item.Language != "" {
			line += " [" + item.Language + "]"
		}
		line += fmt.Sprintf(" ★%d", item.StargazersCount)
		if item.Description != "" {
			line += ": " + item.Description
		}
		if item.HTMLURL != "" {
			line += " — " + item.HTMLURL
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n"), nil
}

func fetchStackOverflow(query string) (string, error) {
	u := fmt.Sprintf(
		"https://api.stackexchange.com/2.3/search/advanced?order=desc&sort=relevance&q=%s&site=stackoverflow&pagesize=5&filter=default",
		url.QueryEscape(query),
	)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	resp, err := newHTTPClient(20 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("stackoverflow search HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var root struct {
		Items []struct {
			Title        string   `json:"title"`
			Link         string   `json:"link"`
			Score        int      `json:"score"`
			AnswerCount  int      `json:"answer_count"`
			IsAnswered   bool     `json:"is_answered"`
			CreationDate int64    `json:"creation_date"`
			Tags         []string `json:"tags"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return "", err
	}
	if len(root.Items) == 0 {
		return "", fmt.Errorf("no stackoverflow results for %q", query)
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("StackOverflow-Ergebnisse für %q:", query))
	for i, item := range root.Items {
		if i >= 5 {
			break
		}
		line := fmt.Sprintf("- %s (Score %d, Antworten %d", html.UnescapeString(item.Title), item.Score, item.AnswerCount)
		if item.IsAnswered {
			line += ", beantwortet"
		}
		line += ")"
		if len(item.Tags) > 0 {
			line += " [" + strings.Join(item.Tags, ", ") + "]"
		}
		if item.Link != "" {
			line += " — " + item.Link
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n"), nil
}

func fetchMultiWebSearch(query string) (string, error) {
	variants := expandExternalSearchQueries(query)
	// expandExternalSearchQueries always returns at least one element, but guard defensively
	if len(variants) == 0 {
		variants = []string{query}
	}
	var parts []string
	var errs []string

	// 1. DuckDuckGo: try up to 2 query variants
	for i, variant := range variants {
		if i >= 2 {
			break
		}
		if text, err := fetchDuckDuckGo(variant); err == nil && strings.TrimSpace(text) != "" {
			parts = append(parts, fmt.Sprintf("DuckDuckGo [%s]\n%s", variant, text))
			break // one good DDG result is enough
		} else if err != nil {
			errs = append(errs, "ddg("+variant+"): "+err.Error())
		}
	}

	// 2. MetaGer: best for German-language and privacy-sensitive queries
	if text, err := fetchMetaGer(buildEngineQuery(query, "metager")); err == nil && strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	} else if err != nil {
		errs = append(errs, "metager: "+err.Error())
	}

	// 3. Ecosia: additional perspective (Bing-backed, eco-focused)
	if text, err := fetchEcosia(buildEngineQuery(query, "ecosia")); err == nil && strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	} else if err != nil {
		errs = append(errs, "ecosia: "+err.Error())
	}

	// 4. Brave: independent index, different from DDG/Bing for controversial/niche queries
	if text, err := fetchBraveSearch(buildEngineQuery(query, "brave")); err == nil && strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	} else if err != nil {
		errs = append(errs, "brave: "+err.Error())
	}

	// 5. Wikidata for structured entity data
	if text, err := fetchWikidata(variants[0]); err == nil && strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	} else if err != nil {
		errs = append(errs, "wikidata: "+err.Error())
	}

	// 6. For technical queries: GitHub + StackOverflow
	if looksTechnicalQuery(query) {
		if text, err := fetchGitHub(variants[0]); err == nil && strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		} else if err != nil {
			errs = append(errs, "github: "+err.Error())
		}
		if text, err := fetchStackOverflow(variants[0]); err == nil && strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		} else if err != nil {
			errs = append(errs, "stackoverflow: "+err.Error())
		}
	}

	if len(parts) == 0 {
		if len(errs) == 0 {
			return "", fmt.Errorf("no search results for %q", query)
		}
		return "", fmt.Errorf("Errors: %s", strings.Join(errs, " | "))
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

// buildEngineQuery builds an optimized search query string for a specific
// search engine, adding appropriate operators, language hints, and filters.
//
//   - "metager"  — adds German-language hints for German queries
//   - "ecosia"   — same as base but with eco/environmental context stripped
//   - "brave"    — strips time-sensitive words for better index recall
//   - default    — returns the base query unchanged
func buildEngineQuery(query, engine string) string {
	base := strings.TrimSpace(query)
	if base == "" {
		return query
	}
	// Detect query language heuristic (German contains umlauts or common words)
	isGerman := strings.ContainsAny(base, "äöüÄÖÜß") ||
		hasAnyWord(base, []string{"und", "der", "die", "das", "ist", "von", "für", "mit", "was", "wie", "wer"})

	switch engine {
	case "metager":
		// MetaGer indexes many German and European sources; prefix with language hint
		if isGerman {
			return base // MetaGer already defaults to German sources
		}
		return base
	case "ecosia":
		// Ecosia is Bing-backed; strip question words to get a cleaner keyword query
		base = stripQuestionWords(base)
		return base
	case "brave":
		// Brave has its own index; remove news-biasing modifiers for recall
		base = strings.TrimPrefix(base, `news "`)
		base = strings.TrimSuffix(base, `"`)
		return strings.TrimSpace(base)
	default:
		return base
	}
}

// hasAnyWord reports whether s contains any of words as whole tokens (case-insensitive).
// It tokenizes s on whitespace and compares lowercased tokens against the target words.
func hasAnyWord(s string, words []string) bool {
	wordSet := make(map[string]bool, len(words))
	for _, w := range words {
		wordSet[strings.ToLower(w)] = true
	}
	// Strip trailing punctuation from each token before comparing
	punctRe := regexp.MustCompile(`[^\p{L}\p{N}]+$`)
	for _, tok := range strings.Fields(s) {
		clean := strings.ToLower(punctRe.ReplaceAllString(tok, ""))
		if wordSet[clean] {
			return true
		}
	}
	return false
}

// stripQuestionWords removes common question-word prefixes from a query
// so that search engines receive clean keyword queries.
func stripQuestionWords(q string) string {
	prefixes := []string{
		"was ist ", "wer ist ", "wie ist ", "wo ist ", "wann ist ", "warum ist ", "welche ",
		"what is ", "who is ", "where is ", "when is ", "why is ", "which ",
		"qu'est-ce que ", "qui est ", "où est ", "quand est ",
	}
	low := strings.ToLower(q)
	for _, p := range prefixes {
		if strings.HasPrefix(low, p) {
			return q[len(p):]
		}
	}
	return q
}

// ─────────────────────────────────────────────────────────────────────────────
// Additional search engines: MetaGer, Ecosia, Brave
// ─────────────────────────────────────────────────────────────────────────────

// fetchMetaGer scrapes the MetaGer meta-search engine results page.
// MetaGer is a privacy-respecting German meta-search engine that aggregates
// results from Bing, Yandex, and others without tracking.
func fetchMetaGer(query string) (string, error) {
	u := fmt.Sprintf("https://metager.de/meta/meta.ger3?eingabe=%s&s=0&bfe=on", url.QueryEscape(query))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	req.Header.Set("Accept-Language", "de,en;q=0.9")
	client := newHTTPClient(20 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("MetaGer fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}
	// Extract title + snippet pairs from the result HTML
	titleRe := regexp.MustCompile(`(?i)<h2[^>]*class="[^"]*result-title[^"]*"[^>]*>.*?<a[^>]*>(.*?)</a>`)
	snippetRe := regexp.MustCompile(`(?i)<p[^>]*class="[^"]*result-description[^"]*"[^>]*>(.*?)</p>`)
	titles := titleRe.FindAllStringSubmatch(string(body), 8)
	snippets := snippetRe.FindAllStringSubmatch(string(body), 8)
	var parts []string
	for i, t := range titles {
		title := html.UnescapeString(htmlTagRe.ReplaceAllString(t[1], ""))
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		entry := "• " + title
		if i < len(snippets) {
			snip := html.UnescapeString(htmlTagRe.ReplaceAllString(snippets[i][1], ""))
			snip = strings.TrimSpace(snip)
			if snip != "" {
				entry += "\n  " + snip
			}
		}
		parts = append(parts, entry)
		if len(parts) >= 6 {
			break
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("MetaGer returned no results for %q", query)
	}
	return fmt.Sprintf("MetaGer-Suchergebnisse für \"%s\":\n\n%s", query, strings.Join(parts, "\n\n")), nil
}

// fetchEcosia scrapes Ecosia search result snippets for the given query.
// Ecosia is an environmentally-focused search engine powered by Bing.
func fetchEcosia(query string) (string, error) {
	u := fmt.Sprintf("https://www.ecosia.org/search?method=index&q=%s", url.QueryEscape(query))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	req.Header.Set("Accept-Language", "de,en;q=0.9")
	client := newHTTPClient(20 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Ecosia fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}
	snippetRe := regexp.MustCompile(`(?i)<p[^>]*class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</p>`)
	matches := snippetRe.FindAllStringSubmatch(string(body), 8)
	var parts []string
	for _, m := range matches {
		s := html.UnescapeString(htmlTagRe.ReplaceAllString(m[1], ""))
		s = strings.TrimSpace(s)
		if s != "" {
			parts = append(parts, "- "+s)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("Ecosia returned no results for %q", query)
	}
	return fmt.Sprintf("Ecosia-Suchergebnisse für \"%s\":\n\n%s", query, strings.Join(parts, "\n")), nil
}

// fetchBraveSearch fetches results from the Brave Search HTML endpoint.
// Brave Search is an independent search index that does not rely on Google/Bing.
func fetchBraveSearch(query string) (string, error) {
	u := fmt.Sprintf("https://search.brave.com/search?q=%s&source=web", url.QueryEscape(query))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	req.Header.Set("Accept-Language", "de,en;q=0.9")
	client := newHTTPClient(20 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Brave fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}
	descRe := regexp.MustCompile(`(?i)<p[^>]*class="[^"]*snippet-description[^"]*"[^>]*>(.*?)</p>`)
	matches := descRe.FindAllStringSubmatch(string(body), 8)
	var parts []string
	for _, m := range matches {
		s := html.UnescapeString(htmlTagRe.ReplaceAllString(m[1], ""))
		s = strings.TrimSpace(s)
		if s != "" {
			parts = append(parts, "- "+s)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("Brave Search returned no results for %q", query)
	}
	return fmt.Sprintf("Brave-Suchergebnisse für \"%s\":\n\n%s", query, strings.Join(parts, "\n")), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Wiktionary / Dictionary
// ─────────────────────────────────────────────────────────────────────────────

// fetchWiktionary fetches a plain-text extract for `word` from the
// specified Wiktionary language and returns a formatted string.
func fetchWiktionary(word, lang string) (string, error) {
	u := fmt.Sprintf(
		"https://%s.wiktionary.org/w/api.php?action=query&prop=extracts&explaintext=1&titles=%s&format=json",
		lang, url.QueryEscape(word),
	)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1 (https://github.com/SimonWaldherr/tinyRAG)")
	client := newHTTPClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var result struct {
		Query struct {
			Pages map[string]struct {
				Title   string `json:"title"`
				Extract string `json:"extract"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	for _, p := range result.Query.Pages {
		if p.Extract == "" {
			return "", fmt.Errorf("no Wiktionary entry found for %q", word)
		}
		return fmt.Sprintf("Wiktionary: %s\n\n%s", p.Title, p.Extract), nil
	}
	return "", fmt.Errorf("no Wiktionary entry found for %q", word)
}

// ─────────────────────────────────────────────────────────────────────────────
// Text chunker
// ─────────────────────────────────────────────────────────────────────────────

// chunkText splits `text` into paragraphs and joins them into chunks
// of at most `maxLen` characters for embedding and storage.
// It retains a small overlap between chunks to maintain semantic context.
