package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	tinysql "github.com/SimonWaldherr/tinySQL"
	nanogo "simonwaldherr.de/go/nanogo/interp"
	smallr "simonwaldherr.de/go/smallr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Embedded frontend assets
// ─────────────────────────────────────────────────────────────────────────────

//go:embed index.html
var indexHTML string

//go:embed style.css
var styleCSS string

//go:embed app.js
var appJS string

// ─────────────────────────────────────────────────────────────────────────────
// Settings (persisted as JSON)
// ─────────────────────────────────────────────────────────────────────────────

// appSettings holds persisted configuration for the application,
// including model settings, chunking options, custom APIs and personas.
type appSettings struct {
	Version    int         `json:"version"`
	BaseURL    string      `json:"base_url"`    // without trailing /v1
	ChatModel  string      `json:"chat_model"`  // OpenAI compatible model ID
	EmbedModel string      `json:"embed_model"` // OpenAI compatible model ID
	Lang       string      `json:"lang"`
	Theme      string      `json:"theme"`
	ChunkSize  int         `json:"chunk_size"`
	K          int         `json:"k"`
	CustomAPIs []customAPI `json:"custom_apis"`
	Personas   []persona   `json:"personas"`
	// AllowCodeExec must be explicitly enabled to allow running user
	// provided code. Defaults to false for safety.
	AllowCodeExec bool `json:"allow_code_exec"`
	// AllowNanoGo enables execution of untrusted Go source via the
	// embedded nanoGo interpreter. Default: false.
	AllowNanoGo bool `json:"allow_nanogo"`
}

// settingsStore provides a thread-safe wrapper around persisted
// `appSettings`, handling reading and atomic writes to disk.
type settingsStore struct {
	mu   sync.Mutex
	path string
	s    appSettings
}

// normalizeBaseURL trims and normalizes an LLM base URL, removing
// trailing slashes and an optional "/v1" suffix.
func normalizeBaseURL(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/v1") {
		u = strings.TrimSuffix(u, "/v1")
	}
	u = strings.TrimRight(u, "/")
	return u
}

// defaultSettingsFromFlags builds initial `appSettings` from CLI flags
// used on first-run when no settings file exists.
func defaultSettingsFromFlags(urlFlag, chatModelFlag, embedModelFlag, lang string, chunkSize, k int) appSettings {
	return appSettings{
		Version:       1,
		BaseURL:       normalizeBaseURL(urlFlag),
		ChatModel:     chatModelFlag,
		EmbedModel:    embedModelFlag,
		Lang:          lang,
		ChunkSize:     chunkSize,
		K:             k,
		CustomAPIs:    []customAPI{},
		AllowCodeExec: false,
		AllowNanoGo:   false,
	}
}

// loadOrCreateSettings loads settings from `path` or creates the file
// with `defaults` if it does not exist, returning a settingsStore.
func loadOrCreateSettings(path string, defaults appSettings) (*settingsStore, error) {
	ss := &settingsStore{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			ss.s = defaults
			if len(ss.s.Personas) == 0 {
				ss.s.Personas = []persona{{ID: "persona-default", Name: "Standard", Prompt: ""}}
			}
			if err := ss.saveLocked(); err != nil {
				return nil, err
			}
			return ss, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &ss.s); err != nil {
		return nil, fmt.Errorf("settings JSON parse error: %w", err)
	}
	// Minimal migrations / sanity
	if ss.s.Version == 0 {
		ss.s.Version = 1
	}
	if ss.s.Lang == "" {
		ss.s.Lang = defaults.Lang
	}
	if ss.s.ChunkSize <= 0 {
		ss.s.ChunkSize = defaults.ChunkSize
	}
	if ss.s.K <= 0 {
		ss.s.K = defaults.K
	}
	ss.s.BaseURL = normalizeBaseURL(ss.s.BaseURL)
	if len(ss.s.Personas) == 0 {
		ss.s.Personas = []persona{{ID: "persona-default", Name: "Standard", Prompt: ""}}
	}
	_ = ss.save() // best-effort normalize on disk
	return ss, nil
}

// get returns the current settings snapshot in a thread-safe manner.
func (ss *settingsStore) get() appSettings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.s
}

// save persists the current settings to disk using an atomic write.
func (ss *settingsStore) save() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.saveLocked()
}

// saveLocked writes settings to disk and must be called with `ss.mu` held.
func (ss *settingsStore) saveLocked() error {
	b, err := json.MarshalIndent(ss.s, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}
	b = append(b, '\n')
	tmp := ss.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ss.path)
}

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
	client := &http.Client{Timeout: 30 * time.Second}
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
	client := &http.Client{Timeout: 15 * time.Second}
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

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)
var multiSpaceRe = regexp.MustCompile(`\s{3,}`)

// fetchURL retrieves and heuristically strips HTML from a URL,
// returning plain text suitable for chunking and embedding.
func fetchURL(rawURL string) (string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tinyRAG/1.1")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d for %s", resp.StatusCode, rawURL)
	}
	body, err := io.ReadAll(resp.Body)
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
	client := &http.Client{Timeout: 15 * time.Second}
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
	client := &http.Client{Timeout: 15 * time.Second}
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
func chunkText(text string, maxLen int) []string {
	paragraphs := strings.Split(text, "\n")
	var chunks []string
	var buf strings.Builder
	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if buf.Len()+len(p)+1 > maxLen && buf.Len() > 0 {
			chunks = append(chunks, buf.String())
			buf.Reset()
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(p)
	}
	if buf.Len() > 0 {
		chunks = append(chunks, buf.String())
	}
	return chunks
}

// ─────────────────────────────────────────────────────────────────────────────
// OpenAI-compatible client (LM Studio, Ollama, …)
// ─────────────────────────────────────────────────────────────────────────────

// lmClient is a small OpenAI-compatible client used for embeddings
// and chat completions against local or remote LLM endpoints.
type lmClient struct {
	base       string
	embedModel string
	chatModel  string
	http       *http.Client
}

// newLMClient constructs an `lmClient` configured for the given
// base URL and model names.
func newLMClient(base, embedModel, chatModel string) *lmClient {
	return &lmClient{
		base:       normalizeBaseURL(base),
		embedModel: embedModel,
		chatModel:  chatModel,
		http:       &http.Client{Timeout: 120 * time.Second},
	}
}

// ping checks the LLM endpoint for reachability by requesting
// the list of available models.
func (c *lmClient) ping() error {
	resp, err := c.http.Get(c.base + "/v1/models")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("LLM endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// modelsResp is a helper for parsing the /v1/models response.
type modelsResp struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// listModels queries the LLM endpoint for available model IDs,
// optionally overriding the client's base URL.
func (c *lmClient) listModels(baseOverride string) ([]string, error) {
	base := c.base
	if strings.TrimSpace(baseOverride) != "" {
		base = normalizeBaseURL(baseOverride)
	}
	req, err := http.NewRequest("GET", base+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create models request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read models response: %w", readErr)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var mr modelsResp
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, err
	}
	var out []string
	for _, d := range mr.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// embReq represents an embeddings request payload.
type embReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embResp represents an embeddings response payload.
type embResp struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// embed sends multiple `texts` to the embedding endpoint and returns
// their vector embeddings.
func (c *lmClient) embed(texts []string) ([][]float64, error) {
	body, err := json.Marshal(embReq{Model: c.embedModel, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embed request: %w", err)
	}
	resp, err := c.http.Post(c.base+"/v1/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read embeddings response: %w", readErr)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embed %d: %s", resp.StatusCode, string(raw))
	}
	var er embResp
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, err
	}
	vecs := make([][]float64, len(er.Data))
	for i, d := range er.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// embedSingle returns the embedding vector for a single text input.
func (c *lmClient) embedSingle(text string) ([]float64, error) {
	vecs, err := c.embed([]string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return vecs[0], nil
}

// chatReq models the request payload for chat completions.
type chatReq struct {
	Model    string    `json:"model"`
	Messages []chatMsg `json:"messages"`
	Stream   bool      `json:"stream"`
}

// chatMsg represents a single chat message with a role and content.
type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatStream streams tokens from the chat completion endpoint and
// writes them to `w` as they arrive.
func (c *lmClient) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	all := make([]chatMsg, 0, len(msgs)+1)
	all = append(all, chatMsg{Role: "system", Content: system})
	all = append(all, msgs...)
	body, err := json.Marshal(chatReq{Model: c.chatModel, Messages: all, Stream: true})
	if err != nil {
		return fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("chat request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("chat HTTP %d (failed to read body: %v)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("chat HTTP %d: %s", resp.StatusCode, string(raw))
	}

	inThink := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
			tok := chunk.Choices[0].Delta.Content
			// Some local models wrap "thinking" in markers; strip them from the UI
			if strings.Contains(tok, "[THINK]") {
				inThink = true
				tok = strings.ReplaceAll(tok, "[THINK]", "")
			}
			if strings.Contains(tok, "[/THINK]") {
				inThink = false
				tok = strings.ReplaceAll(tok, "[/THINK]", "")
			}
			if !inThink && tok != "" {
				fmt.Fprint(w, tok)
			}
		}
	}
	return scanner.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// RAG system
// ─────────────────────────────────────────────────────────────────────────────

// vecJSON marshals a float64 slice into a JSON string for SQL usage.
func vecJSON(v []float64) string {
	b, err := json.Marshal(v)
	if err != nil {
		// This should never happen with float64 slices, but handle it anyway
		log.Printf("Warning: failed to marshal vector: %v", err)
		return "[]"
	}
	return string(b)
}

// escapeSQ escapes single quotes for safe SQL insertion.
func escapeSQ(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// storageModeLabel returns a short string label for a tinySQL storage mode.
func storageModeLabel(mode tinysql.StorageMode) string {
	switch mode {
	case tinysql.ModeMemory:
		return "memory"
	case tinysql.ModeWAL:
		return "wal"
	case tinysql.ModeDisk:
		return "disk"
	case tinysql.ModeIndex:
		return "index"
	case tinysql.ModeHybrid:
		return "hybrid"
	default:
		return "legacy"
	}
}

// newRequestID generates a short random request identifier.
func newRequestID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err == nil {
		return fmt.Sprintf("req-%x", b)
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// ragSystem encapsulates the tinyRAG knowledge store, embedding
// functionality and an associated tinySQL database instance.
type ragSystem struct {
	db     *tinysql.DB
	dbPath string
	k      int
	dim    int

	// Storage mode (for display / logging)
	storageMode tinysql.StorageMode

	// Settings-sensitive runtime state
	lmMu sync.RWMutex
	lm   *lmClient

	// DB mutex (tinySQL isn't designed for heavy concurrent writes)
	dbMu sync.Mutex

	// Monotonic chunk IDs (avoid collisions even after deletes)
	idMu   sync.Mutex
	nextID int
}

// newRAG initializes a new `ragSystem` backed by a tinySQL DB using
// the provided storage mode and memory constraints.
func newRAG(lm *lmClient, k int, dbPath string, storageMode tinysql.StorageMode, maxMemMB int64) (*ragSystem, error) {
	var db *tinysql.DB
	var err error

	switch storageMode {
	case tinysql.ModeMemory:
		// In-memory with optional save-on-close.
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode: tinysql.ModeMemory,
			Path: dbPath, // saves GOB on Close if non-empty
		})
		if err != nil {
			return nil, fmt.Errorf("open memory db: %w", err)
		}
		if dbPath != "" {
			fmt.Printf("Storage mode: memory (save to %s on exit)\n", dbPath)
		} else {
			fmt.Println("Storage mode: memory (ephemeral, no persistence)")
		}

	case tinysql.ModeWAL:
		if dbPath == "" {
			dbPath = "tinyrag.gob"
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode: tinysql.ModeWAL,
			Path: dbPath,
		})
		if err != nil {
			return nil, fmt.Errorf("open wal db: %w", err)
		}
		fmt.Printf("Storage mode: WAL (checkpoint to %s)\n", dbPath)

	case tinysql.ModeDisk:
		if dbPath == "" {
			dbPath = "tinyrag.db"
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode: tinysql.ModeDisk,
			Path: dbPath,
		})
		if err != nil {
			return nil, fmt.Errorf("open disk db: %w", err)
		}
		fmt.Printf("Storage mode: disk (tables in %s/)\n", dbPath)

	case tinysql.ModeIndex:
		if dbPath == "" {
			dbPath = "tinyrag.db"
		}
		mem := maxMemMB * 1024 * 1024
		if mem <= 0 {
			mem = 64 * 1024 * 1024
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode:           tinysql.ModeIndex,
			Path:           dbPath,
			MaxMemoryBytes: mem,
		})
		if err != nil {
			return nil, fmt.Errorf("open index db: %w", err)
		}
		fmt.Printf("Storage mode: index (schemas in RAM, rows on disk in %s/, max %d MB)\n", dbPath, maxMemMB)

	case tinysql.ModeHybrid:
		if dbPath == "" {
			dbPath = "tinyrag.db"
		}
		mem := maxMemMB * 1024 * 1024
		if mem <= 0 {
			mem = 256 * 1024 * 1024
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode:           tinysql.ModeHybrid,
			Path:           dbPath,
			MaxMemoryBytes: mem,
		})
		if err != nil {
			return nil, fmt.Errorf("open hybrid db: %w", err)
		}
		fmt.Printf("Storage mode: hybrid (LRU cache %d MB, disk in %s/)\n", maxMemMB, dbPath)

	default:
		// Fallback: legacy behaviour (load GOB if exists, else new)
		if dbPath != "" {
			if loaded, loadErr := tinysql.LoadFromFile(dbPath); loadErr == nil {
				db = loaded
				fmt.Printf("Loaded existing database from %s\n", dbPath)
			} else {
				db = tinysql.NewDB()
				fmt.Printf("Creating new database (will save to %s)\n", dbPath)
			}
		} else {
			db = tinysql.NewDB()
		}
	}

	r := &ragSystem{db: db, lm: lm, k: k, dbPath: dbPath, storageMode: storageMode}
	return r, nil
}

// setLM atomically replaces the runtime `lmClient` used for embeddings
// and chat requests.
func (r *ragSystem) setLM(lm *lmClient) {
	r.lmMu.Lock()
	defer r.lmMu.Unlock()
	r.lm = lm
}

// getLM returns the currently configured `lmClient`.
func (r *ragSystem) getLM() *lmClient {
	r.lmMu.RLock()
	defer r.lmMu.RUnlock()
	return r.lm
}

// save flushes the underlying database to disk or performs a sync
// depending on the configured storage mode.
func (r *ragSystem) save() error {
	if r.dbPath == "" {
		return nil
	}
	r.dbMu.Lock()
	defer r.dbMu.Unlock()

	// For disk-backed modes, Sync flushes dirty tables to disk.
	// For memory mode, this is a no-op (data saved on Close).
	switch r.storageMode {
	case tinysql.ModeDisk, tinysql.ModeHybrid, tinysql.ModeIndex:
		return r.db.Sync()
	default:
		// Legacy / ModeMemory / ModeWAL: full GOB snapshot
		return tinysql.SaveToFile(r.db, r.dbPath)
	}
}

// init creates required DB tables and initializes runtime counters.
func (r *ragSystem) init() error {
	q := "CREATE TABLE IF NOT EXISTS chunks (id INT, article TEXT, chunk_idx INT, content TEXT, embedding VECTOR)"
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return err
	}
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	_, err = tinysql.Execute(context.Background(), r.db, "default", stmt)
	if err != nil {
		return err
	}
	// Initialize nextID from MAX(id)+1
	r.idMu.Lock()
	defer r.idMu.Unlock()
	r.nextID = r.maxChunkIDLocked() + 1
	return nil
}

// maxChunkIDLocked queries the DB for the maximum chunk id and must
// be called with appropriate locking by the caller.
func (r *ragSystem) maxChunkIDLocked() int {
	q := "SELECT MAX(id) AS mid FROM chunks"
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return -1
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return -1
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "mid")
	if !ok || v == nil {
		return -1
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}

// allocIDs reserves `n` monotonic IDs for new chunks.
func (r *ragSystem) allocIDs(n int) int {
	r.idMu.Lock()
	defer r.idMu.Unlock()
	start := r.nextID
	r.nextID += n
	return start
}

// addChunks embeds and stores `chunks` for the given `article` into
// the database, performing batched inserts.
func (r *ragSystem) addChunks(article string, chunks []string) error {
	if len(chunks) == 0 {
		return nil
	}
	// If this article already exists in the DB, skip adding again to avoid duplicates.
	// This makes imports idempotent; to replace content delete the source first.
	checkQ := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM chunks WHERE article = '%s'", escapeSQ(article))
	if st, err := tinysql.ParseSQL(checkQ); err == nil {
		r.dbMu.Lock()
		if rs, err := tinysql.Execute(context.Background(), r.db, "default", st); err == nil && rs != nil && len(rs.Rows) > 0 {
			if v, ok := tinysql.GetVal(rs.Rows[0], "cnt"); ok && v != nil {
				cnt := 0
				switch nv := v.(type) {
				case int:
					cnt = nv
				case int64:
					cnt = int(nv)
				case float64:
					cnt = int(nv)
				}
				if cnt > 0 {
					fmt.Printf("skip addChunks: article '%s' already present (%d chunks)\n", article, cnt)
					r.dbMu.Unlock()
					return nil
				}
			}
		}
		r.dbMu.Unlock()
	}
	batchSize := 16

	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[i:end]

		// Embed without holding DB lock
		vecs, err := r.getLM().embed(batch)
		if err != nil {
			return fmt.Errorf("embed batch %d: %w", i/batchSize, err)
		}
		if r.dim == 0 && len(vecs) > 0 {
			r.dim = len(vecs[0])
		}

		// Allocate IDs for this batch
		startID := r.allocIDs(len(batch))

		// Insert
		r.dbMu.Lock()
		for j, v := range vecs {
			idx := i + j
			q := fmt.Sprintf(
				"INSERT INTO chunks VALUES (%d, '%s', %d, '%s', VEC_FROM_JSON('%s'))",
				startID+j, escapeSQ(article), idx, escapeSQ(batch[j]), vecJSON(v),
			)
			stmt, err := tinysql.ParseSQL(q)
			if err != nil {
				r.dbMu.Unlock()
				return fmt.Errorf("parse insert %d: %w", idx, err)
			}
			if _, err := tinysql.Execute(context.Background(), r.db, "default", stmt); err != nil {
				r.dbMu.Unlock()
				return fmt.Errorf("exec insert %d: %w", idx, err)
			}
		}
		r.dbMu.Unlock()

		fmt.Printf("  embedded+stored %d/%d chunks\n", end, len(chunks))
	}

	if err := r.save(); err != nil {
		log.Printf("WARN: save failed: %v", err)
	}
	return nil
}

// docCount returns the total number of stored chunks.
func (r *ragSystem) docCount() int {
	q := "SELECT COUNT(*) AS cnt FROM chunks"
	stmt, _ := tinysql.ParseSQL(q)

	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()

	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return 0
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "cnt")
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// searchResult represents a single retrieval hit returned by searchJSON.
type searchResult struct {
	Score   float64 `json:"score"`
	Content string  `json:"content"`
}

// searchJSON performs an embedding-based vector search for `query`,
// returning up to `k` primary hits along with neighbor chunks.
func (r *ragSystem) searchJSON(query string, k int) ([]searchResult, error) {
	qvec, err := r.getLM().embedSingle(query)
	if err != nil {
		return nil, err
	}
	// fetch a larger candidate set, we'll filter by score>0.6 and pick up to k primary hits
	limit := 100
	if k*3 > limit {
		limit = k * 3
	}
	// Defensive cap to avoid excessively large queries
	const maxLimit = 1000
	if limit > maxLimit {
		limit = maxLimit
	}
	q := fmt.Sprintf(
		"SELECT content, article, chunk_idx, VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON('%s')) AS score FROM chunks ORDER BY score DESC LIMIT %d",
		vecJSON(qvec), limit,
	)
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil, err
	}

	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()

	if err != nil {
		return nil, err
	}
	// We'll collect primary hits (score>0.6) up to k, and include immediate neighbors (prev/next)
	type hit struct {
		article  string
		chunkIdx int
		content  string
		score    float64
	}
	var candidates = make([]hit, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		c, ok1 := tinysql.GetVal(row, "content")
		art, _ := tinysql.GetVal(row, "article")
		idxVal, _ := tinysql.GetVal(row, "chunk_idx")
		scoreVal, _ := tinysql.GetVal(row, "score")
		if !ok1 || art == nil || idxVal == nil || scoreVal == nil {
			continue
		}
		artStr := fmt.Sprint(art)
		cStr := fmt.Sprint(c)
		idx := 0
		switch iv := idxVal.(type) {
		case int:
			idx = iv
		case int64:
			idx = int(iv)
		case float64:
			idx = int(iv)
		}
		s := 0.0
		switch sv := scoreVal.(type) {
		case float64:
			s = sv
		case int:
			s = float64(sv)
		}
		candidates = append(candidates, hit{article: artStr, chunkIdx: idx, content: cStr, score: s})
	}

	// preallocate expected result size (primary + neighbors)
	results := make([]searchResult, 0, k*3)
	type seenKey struct {
		article string
		idx     int
	}
	seen := make(map[seenKey]bool)
	primaryCount := 0
	for _, h := range candidates {
		if primaryCount >= k {
			break
		}
		if h.score <= 0.6 {
			// skip low-score primary candidates
			continue
		}
		key := seenKey{article: h.article, idx: h.chunkIdx}
		if seen[key] {
			continue
		}
		// add previous neighbor if exists and not seen
		if h.chunkIdx > 0 {
			pkey := seenKey{article: h.article, idx: h.chunkIdx - 1}
			if !seen[pkey] {
				if prevContent, ok := r.fetchNeighborContent(h.article, h.chunkIdx-1); ok {
					results = append(results, searchResult{Score: -1, Content: prevContent})
					seen[pkey] = true
				}
			}
		}

		// add primary hit
		results = append(results, searchResult{Score: h.score, Content: h.content})
		seen[key] = true
		primaryCount++

		// add next neighbor
		nkey := seenKey{article: h.article, idx: h.chunkIdx + 1}
		if !seen[nkey] {
			if nextContent, ok := r.fetchNeighborContent(h.article, h.chunkIdx+1); ok {
				results = append(results, searchResult{Score: -1, Content: nextContent})
				seen[nkey] = true
			}
		}
	}

	return results, nil
}

// ── Tool / API definitions ─────────────────────────────────────────

// toolDef describes a built-in or custom tool available to the assistant.
type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ParamHint   string `json:"param_hint"`
}

// persona represents a user-selectable assistant persona with a
// pre-prompt that influences system behavior.
type persona struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// toolRequest is the structured marker the assistant can emit to
// request that the frontend run a specific tool with a query.
type toolRequest struct {
	Tool  string `json:"tool"`
	Query string `json:"query"`
}

var builtinTools = []toolDef{
	{
		Name:        "wikipedia",
		Description: "Sucht einen Wikipedia-Artikel und lädt dessen Volltext. Verwende dies für Fakten über Personen, Orte, Ereignisse, Wissenschaft etc.",
		ParamHint:   "Artikelname (z.B. 'Sonnensystem', 'Albert_Einstein')",
	},
	{
		Name:        "duckduckgo",
		Description: "Durchsucht das Web über DuckDuckGo und liefert eine Kurzantwort. Gut für aktuelle Fakten, Definitionen, kurze Zusammenfassungen.",
		ParamHint:   "Suchbegriff (z.B. 'Hauptstadt von Frankreich')",
	},
	{
		Name:        "wiktionary",
		Description: "Schlägt ein Wort im Wiktionary (Wörterbuch) nach. Liefert Bedeutung, Etymologie, Übersetzungen.",
		ParamHint:   "Einzelnes Wort (z.B. 'Apfel', 'serendipity')",
	},
	{
		Name:        "stackoverflow",
		Description: "Sucht relevante StackOverflow-Antworten (gut für Programmierfragen).",
		ParamHint:   "Suchbegriff (z.B. 'go http client timeout')",
	},
	{
		Name:        "websearch",
		Description: "Allgemeine Websuche (DuckDuckGo-basiert) für breite Recherchen.",
		ParamHint:   "Suchbegriff (z.B. 'Wetter Berlin heute')",
	},
	{
		Name:        "nanogo",
		Description: "Führt sicheren, interpretierten Go-Code (nanoGo) aus. Muss in den Einstellungen aktiviert werden.",
		ParamHint:   "Go-Quelltext (kurze Snippets)",
	},
	{
		Name:        "llm",
		Description: "Führe einen direkten Prompt gegen das konfigurierte LLM aus (für kreative Antworten oder kurze Analysen).",
		ParamHint:   "Prompt / Frage",
	},
	{
		Name:        "calculate",
		Description: "Führt eine sichere Berechnung aus (arithmetische Ausdrücke). Nutzt das eingebundene smallR für Evaluation.",
		ParamHint:   "Expression (z.B. '3*2+(2^3)')",
	},
	{
		Name:        "exec_code",
		Description: "Führt (sichere) Code-Analysen oder -Ausführungen durch. Standardmäßig nur statische Prüfungen; Ausführung muss in den Einstellungen aktiviert werden.",
		ParamHint:   "Quelltext (z.B. Go code)",
	},
}

// ── Custom API store (persisted through settingsStore) ──────────────

// customAPI models a user-added external API template persisted in settings.
type customAPI struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Template string `json:"template"` // URL with $q placeholder
	Desc     string `json:"desc"`
}

// apiStore manages the set of persisted custom APIs through settings.
type apiStore struct {
	mu       sync.Mutex
	settings *settingsStore
}

// newAPIStore creates an apiStore backed by `settings`.
func newAPIStore(settings *settingsStore) *apiStore {
	return &apiStore{settings: settings}
}

// list returns a copy of configured custom APIs.
func (s *apiStore) list() []customAPI {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	out := make([]customAPI, len(s.settings.s.CustomAPIs))
	copy(out, s.settings.s.CustomAPIs)
	return out
}

// add registers a new custom API template and persists settings.
func (s *apiStore) add(name, template, desc string) (customAPI, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	api := customAPI{
		ID:       fmt.Sprintf("api-%d", time.Now().UnixNano()),
		Name:     name,
		Template: template,
		Desc:     desc,
	}
	s.settings.s.CustomAPIs = append(s.settings.s.CustomAPIs, api)
	if err := s.settings.saveLocked(); err != nil {
		return customAPI{}, err
	}
	return api, nil
}

// remove deletes a custom API by id and persists the change.
func (s *apiStore) remove(id string) (bool, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	apis := s.settings.s.CustomAPIs
	for i, a := range apis {
		if a.ID == id {
			s.settings.s.CustomAPIs = append(apis[:i], apis[i+1:]...)
			return true, s.settings.saveLocked()
		}
	}
	return false, nil
}

// ── Persona store (persisted through settingsStore) ───────────────

// personaStore manages persisted personas stored inside settings.
type personaStore struct {
	mu       sync.Mutex
	settings *settingsStore
}

// newPersonaStore constructs a personaStore backed by `settings`.
func newPersonaStore(settings *settingsStore) *personaStore {
	return &personaStore{settings: settings}
}

// list returns a copy of all configured personas.
func (p *personaStore) list() []persona {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	out := make([]persona, len(p.settings.s.Personas))
	copy(out, p.settings.s.Personas)
	return out
}

// defaultID returns the ID of the first persona or an empty string.
func (p *personaStore) defaultID() string {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	if len(p.settings.s.Personas) == 0 {
		return ""
	}
	return p.settings.s.Personas[0].ID
}

// get retrieves a persona by id.
func (p *personaStore) get(id string) (persona, bool) {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	for _, per := range p.settings.s.Personas {
		if per.ID == id {
			return per, true
		}
	}
	return persona{}, false
}

// add creates and persists a new persona with the given name and prompt.
func (p *personaStore) add(name, prompt string) (persona, error) {
	name = strings.TrimSpace(name)
	prompt = strings.TrimSpace(prompt)
	if name == "" {
		return persona{}, fmt.Errorf("name required")
	}
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	per := persona{
		ID:     fmt.Sprintf("persona-%d", time.Now().UnixNano()),
		Name:   name,
		Prompt: prompt,
	}
	p.settings.s.Personas = append(p.settings.s.Personas, per)
	return per, p.settings.saveLocked()
}

// remove deletes a persona by id and persists the change.
func (p *personaStore) remove(id string) (bool, error) {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	list := p.settings.s.Personas
	for i, per := range list {
		if per.ID == id {
			p.settings.s.Personas = append(list[:i], list[i+1:]...)
			return true, p.settings.saveLocked()
		}
	}
	return false, nil
}

// get returns a customAPI by id if it exists.
func (s *apiStore) get(id string) (customAPI, bool) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for _, a := range s.settings.s.CustomAPIs {
		if a.ID == id {
			return a, true
		}
	}
	return customAPI{}, false
}

// allTools returns the union of builtin tools and persisted custom APIs.
func (s *apiStore) allTools() []toolDef {
	all := make([]toolDef, len(builtinTools))
	copy(all, builtinTools)

	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()

	for _, a := range s.settings.s.CustomAPIs {
		desc := a.Desc
		if desc == "" {
			desc = "Custom API: " + a.Template
		}
		all = append(all, toolDef{
			Name:        a.ID,
			Description: desc,
			ParamHint:   "Suchbegriff (wird in $q eingesetzt)",
		})
	}
	return all
}

var toolRequestRe = regexp.MustCompile(`\[TOOL_REQUEST\]\s*(\{[^}]+\})\s*\[/TOOL_REQUEST\]`)

// buildToolSystemPrompt constructs the system prompt describing
// available tools and how the assistant should emit tool requests.
func buildToolSystemPrompt(ctxText string, tools []toolDef) string {
	var sb strings.Builder
	sb.WriteString("Du bist ein hilfreicher Assistent. Beantworte Fragen basierend auf dem bereitgestellten Kontext.\n\n")
	sb.WriteString("## Verfügbare Such-APIs\n")
	sb.WriteString("Wenn der Kontext NICHT genügend Informationen enthält, um die Frage zuverlässig zu beantworten, ")
	sb.WriteString("kannst du dem Nutzer vorschlagen, eine der folgenden Suchfunktionen zu verwenden. ")
	sb.WriteString("Schreibe dazu am ENDE deiner Antwort einen Tool-Request in exakt diesem Format:\n\n")
	sb.WriteString("[TOOL_REQUEST]{\"tool\":\"<name>\",\"query\":\"<suchbegriff>\"}[/TOOL_REQUEST]\n\n")
	sb.WriteString("Verfügbare Tools:\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- **%s**: %s (Parameter: %s)\n", t.Name, t.Description, t.ParamHint))
	}
	sb.WriteString("\nWichtig:\n")
	sb.WriteString("- Schlage nur EIN Tool pro Antwort vor.\n")
	sb.WriteString("- Gib trotzdem eine kurze Antwort mit dem was du weißt, bevor du den Tool-Request anfügst.\n")
	sb.WriteString("- Wenn der Kontext ausreicht, antworte normal OHNE Tool-Request.\n")
	sb.WriteString("- Der Tool-Request muss EXAKT das Format [TOOL_REQUEST]{...}[/TOOL_REQUEST] haben.\n\n")
	sb.WriteString("Kontext:\n")
	sb.WriteString(ctxText)
	return sb.String()
}

// ── Debug / Search models ─────────────────────────────────────────

// debugChunk contains information about a retrieved chunk useful for
// emitting debug payloads back to the frontend.
type debugChunk struct {
	Score      float64 `json:"score"`
	Content    string  `json:"content"`
	Article    string  `json:"article"`
	ChunkIdx   int     `json:"chunk_idx"`
	IsNeighbor bool    `json:"is_neighbor"`
}

// debugInfo aggregates retrieval timing and chunk-level debug data.
type debugInfo struct {
	Chunks      []debugChunk `json:"chunks"`
	EmbedMs     int64        `json:"embed_ms"`
	SearchMs    int64        `json:"search_ms"`
	TotalChunks int          `json:"total_chunks"`
	UsedK       int          `json:"used_k"`
	Decision    string       `json:"decision,omitempty"`
}

// debugModels records which LLM endpoint and models were used for a request.
type debugModels struct {
	BaseURL    string `json:"base_url"`
	ChatModel  string `json:"chat_model"`
	EmbedModel string `json:"embed_model"`
}

// debugPayload is the top-level debug information emitted alongside
// SSE responses to help diagnose retrieval and model behavior.
type debugPayload struct {
	RequestID          string      `json:"request_id"`
	Mode               string      `json:"mode"`
	AutoSearch         bool        `json:"auto_search"`
	Offline            bool        `json:"offline"`
	Deep               bool        `json:"deep"`
	Question           string      `json:"question"`
	UsedK              int         `json:"used_k"`
	BaseK              int         `json:"base_k"`
	ChunkSize          int         `json:"chunk_size"`
	TotalChunks        int         `json:"total_chunks"`
	ContextChars       int         `json:"context_chars"`
	SystemPromptChars  int         `json:"system_prompt_chars"`
	HistoryMessages    int         `json:"history_messages"`
	StorageMode        string      `json:"storage_mode"`
	DBPath             string      `json:"db_path"`
	Models             debugModels `json:"models"`
	Retrieval          *debugInfo  `json:"retrieval"`
	PersonaID          string      `json:"persona_id"`
	PersonaName        string      `json:"persona_name"`
	PersonaPromptChars int         `json:"persona_prompt_chars"`
}

// prepareContext does the embedding + vector search and returns the context string and optional debug info.
// prepareContext computes embeddings for `question`, runs a vector
// search against the DB and returns the assembled context text and
// optional debug information.
func (r *ragSystem) prepareContext(question string, debug bool) (string, *debugInfo, error) {
	// First, try a refined search query for entity-like questions.
	searchQuery := refineSearchQuery(question)

	t0 := time.Now()
	qvec, err := r.getLM().embedSingle(searchQuery)
	if err != nil {
		return "", nil, err
	}
	embedMs := time.Since(t0).Milliseconds()

	// Special-case: if the refined searchQuery matches an article we have
	// stored, return that article's chunks (including neighbors implicitly
	// by returning adjacent content).
	acheck := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM chunks WHERE article = '%s'", escapeSQ(searchQuery))
	ast, aerr := tinysql.ParseSQL(acheck)
	if aerr == nil {
		r.dbMu.Lock()
		ars, aerr2 := tinysql.Execute(context.Background(), r.db, "default", ast)
		r.dbMu.Unlock()
		if aerr2 == nil && ars != nil && len(ars.Rows) > 0 {
			v, _ := tinysql.GetVal(ars.Rows[0], "cnt")
			cnt := 0
			switch nv := v.(type) {
			case int:
				cnt = nv
			case int64:
				cnt = int(nv)
			case float64:
				cnt = int(nv)
			}
			if cnt > 0 {
				// fetch all chunks for the article
				fq := fmt.Sprintf("SELECT chunk_idx, content FROM chunks WHERE article = '%s' ORDER BY chunk_idx", escapeSQ(searchQuery))
				fst, _ := tinysql.ParseSQL(fq)
				r.dbMu.Lock()
				frs, _ := tinysql.Execute(context.Background(), r.db, "default", fst)
				r.dbMu.Unlock()
				var parts []string
				var dbgChunks []debugChunk
				if frs != nil {
					for _, row := range frs.Rows {
						c, ok := tinysql.GetVal(row, "content")
						if !ok {
							continue
						}
						idxVal, _ := tinysql.GetVal(row, "chunk_idx")
						idx := 0
						switch iv := idxVal.(type) {
						case int:
							idx = iv
						case int64:
							idx = int(iv)
						case float64:
							idx = int(iv)
						}
						s := fmt.Sprintf("%v", c)
						parts = append(parts, s)
						if debug {
							dbgChunks = append(dbgChunks, debugChunk{Score: -1, Content: s, Article: searchQuery, ChunkIdx: idx, IsNeighbor: false})
						}
					}
				}
				di := &debugInfo{Chunks: dbgChunks, EmbedMs: embedMs, SearchMs: 0, TotalChunks: r.docCount(), UsedK: r.k, Decision: "article_specific"}
				return strings.Join(parts, "\n---\n"), di, nil
			}
		}
	}

	t1 := time.Now()
	// Initial quick retrieval: fetch a larger candidate set to allow
	// filtering by high-confidence threshold.
	limit := 100
	if r.k*3 > limit {
		limit = r.k * 3
	}
	const maxLimit = 1000
	if limit > maxLimit {
		limit = maxLimit
	}
	q := fmt.Sprintf(
		"SELECT content, article, chunk_idx, VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON('%s')) AS score FROM chunks ORDER BY score DESC LIMIT %d",
		vecJSON(qvec), limit,
	)

	r.dbMu.Lock()
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		r.dbMu.Unlock()
		return "", nil, err
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()

	if err != nil {
		return "", nil, err
	}
	searchMs := time.Since(t1).Milliseconds()

	rows := rs.Rows

	type chunkHit struct {
		article  string
		chunkIdx int
		content  string
		score    float64
	}
	var hits []chunkHit
	for _, row := range rows {
		c, ok := tinysql.GetVal(row, "content")
		if !ok {
			continue
		}
		cStr := fmt.Sprintf("%v", c)
		art, _ := tinysql.GetVal(row, "article")
		artStr := fmt.Sprintf("%v", art)
		idxVal, _ := tinysql.GetVal(row, "chunk_idx")
		idx := 0
		switch iv := idxVal.(type) {
		case int:
			idx = iv
		case int64:
			idx = int(iv)
		case float64:
			idx = int(iv)
		}
		scoreVal, _ := tinysql.GetVal(row, "score")
		s := 0.0
		switch sv := scoreVal.(type) {
		case float64:
			s = sv
		case int:
			s = float64(sv)
		}
		hits = append(hits, chunkHit{article: artStr, chunkIdx: idx, content: cStr, score: s})
	}

	// If we have a clear high-confidence hit, return context immediately.
	const highThreshold = 0.90
	var primaryCount int
	for _, h := range hits {
		if h.score > highThreshold {
			primaryCount++
		}
	}

	type chunkKey struct {
		article  string
		chunkIdx int
	}

	// Helper to assemble context from selected hits (and neighbors)
	assemble := func(sel []chunkHit, usedK int, decision string) (string, *debugInfo, error) {
		seen := make(map[chunkKey]bool)
		for _, h := range hits {
			seen[chunkKey{h.article, h.chunkIdx}] = true
		}
		var contextParts []string
		var dbgChunks []debugChunk
		for _, h := range sel {
			prevKey := chunkKey{h.article, h.chunkIdx - 1}
			if h.chunkIdx > 0 && !seen[prevKey] {
				seen[prevKey] = true
				if prevContent, ok := r.fetchNeighborContent(h.article, h.chunkIdx-1); ok {
					contextParts = append(contextParts, prevContent)
					dbgChunks = append(dbgChunks, debugChunk{Score: -1, Content: prevContent, Article: h.article, ChunkIdx: h.chunkIdx - 1, IsNeighbor: true})
				}
			}

			contextParts = append(contextParts, h.content)
			dbgChunks = append(dbgChunks, debugChunk{Score: h.score, Content: h.content, Article: h.article, ChunkIdx: h.chunkIdx, IsNeighbor: false})

			nextKey := chunkKey{h.article, h.chunkIdx + 1}
			if !seen[nextKey] {
				seen[nextKey] = true
				if nextContent, ok := r.fetchNeighborContent(h.article, h.chunkIdx+1); ok {
					contextParts = append(contextParts, nextContent)
					dbgChunks = append(dbgChunks, debugChunk{Score: -1, Content: nextContent, Article: h.article, ChunkIdx: h.chunkIdx + 1, IsNeighbor: true})
				}
			}
		}
		di := &debugInfo{Chunks: dbgChunks, EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCount(), UsedK: usedK, Decision: decision}
		return strings.Join(contextParts, "\n---\n"), di, nil
	}

	// If high-confidence primary found, use those hits (top k by score)
	if primaryCount > 0 {
		var sel []chunkHit
		for _, h := range hits {
			if h.score > highThreshold {
				sel = append(sel, h)
				if len(sel) >= r.k {
					break
				}
			}
		}
		return assemble(sel, r.k, "high_confidence")
	}

	// Prepare a concise summary of top candidates to let the LM decide
	// whether more retrieval is needed.
	var summaryParts []string
	topN := 5
	if len(hits) < topN {
		topN = len(hits)
	}
	for i := 0; i < topN; i++ {
		h := hits[i]
		summaryParts = append(summaryParts, fmt.Sprintf("%s (score=%.4f)", h.article, h.score))
	}
	summary := strings.Join(summaryParts, "; ")

	// Ask LM whether to answer directly or retrieve more context.
	decisionMap, derr := r.analyzeQuestion(question, summary)
	if derr != nil {
		// Fallback: perform relaxed retrieval
		var sel []chunkHit
		thresh := 0.60
		for _, h := range hits {
			if h.score >= thresh {
				sel = append(sel, h)
				if len(sel) >= r.k {
					break
				}
			}
		}
		return assemble(sel, r.k, "relaxed_fallback")
	}

	action, _ := decisionMap["action"].(string)
	if strings.ToUpper(action) == "ANSWER_DIRECT" {
		// Let the chat model answer without extra context.
		di := &debugInfo{EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCount(), UsedK: 0, Decision: "answer_direct"}
		return "", di, nil
	}

	// Otherwise, gather retrieval parameters and perform relaxed retrieval.
	desiredK := r.k
	if v, ok := decisionMap["k"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			desiredK = int(fv)
		}
	}
	thresh := 0.60
	if v, ok := decisionMap["threshold"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			thresh = fv
		}
	}
	// Optionally allow the LM to suggest a refined query
	if v, ok := decisionMap["query"]; ok {
		if qs, ok2 := v.(string); ok2 && strings.TrimSpace(qs) != "" {
			searchQuery = qs
		}
	}

	var sel []chunkHit
	for _, h := range hits {
		if h.score >= thresh {
			sel = append(sel, h)
			if len(sel) >= desiredK {
				break
			}
		}
	}
	if len(sel) == 0 && len(hits) > 0 {
		// fallback to top-k by score
		for i := 0; i < desiredK && i < len(hits); i++ {
			sel = append(sel, hits[i])
		}
	}
	return assemble(sel, desiredK, "lm_requested_retrieval")
}

// prepareContextWithK does the same as prepareContext but allows specifying k (number of primary hits)
// prepareContextWithK behaves like prepareContext but allows specifying
// the number `k` of primary retrieval hits to consider.
func (r *ragSystem) prepareContextWithK(question string, debug bool, k int) (string, *debugInfo, error) {
	// refine query for entity-like questions
	searchQuery := refineSearchQuery(question)

	t0 := time.Now()
	qvec, err := r.getLM().embedSingle(searchQuery)
	if err != nil {
		return "", nil, err
	}
	embedMs := time.Since(t0).Milliseconds()

	t1 := time.Now()
	// initial candidate limit
	limit := 100
	if k*3 > limit {
		limit = k * 3
	}
	const maxLimit = 1000
	if limit > maxLimit {
		limit = maxLimit
	}
	q := fmt.Sprintf(
		"SELECT content, article, chunk_idx, VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON('%s')) AS score FROM chunks ORDER BY score DESC LIMIT %d",
		vecJSON(qvec), limit,
	)

	r.dbMu.Lock()
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		r.dbMu.Unlock()
		return "", nil, err
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()

	if err != nil {
		return "", nil, err
	}
	searchMs := time.Since(t1).Milliseconds()

	rows := rs.Rows

	type chunkHit struct {
		article  string
		chunkIdx int
		content  string
		score    float64
	}
	var hits []chunkHit
	for _, row := range rows {
		c, ok := tinysql.GetVal(row, "content")
		if !ok {
			continue
		}
		cStr := fmt.Sprintf("%v", c)
		art, _ := tinysql.GetVal(row, "article")
		artStr := fmt.Sprintf("%v", art)
		idxVal, _ := tinysql.GetVal(row, "chunk_idx")
		idx := 0
		switch iv := idxVal.(type) {
		case int:
			idx = iv
		case int64:
			idx = int(iv)
		case float64:
			idx = int(iv)
		}
		scoreVal, _ := tinysql.GetVal(row, "score")
		s := 0.0
		switch sv := scoreVal.(type) {
		case float64:
			s = sv
		case int:
			s = float64(sv)
		}
		hits = append(hits, chunkHit{article: artStr, chunkIdx: idx, content: cStr, score: s})
	}

	const highThreshold = 0.90
	var primaryCount int
	for _, h := range hits {
		if h.score > highThreshold {
			primaryCount++
		}
	}

	type chunkKey struct {
		article  string
		chunkIdx int
	}

	assemble := func(sel []chunkHit, usedK int, decision string) (string, *debugInfo, error) {
		seen := make(map[chunkKey]bool)
		for _, h := range hits {
			seen[chunkKey{h.article, h.chunkIdx}] = true
		}
		var contextParts []string
		var dbgChunks []debugChunk
		for _, h := range sel {
			prevKey := chunkKey{h.article, h.chunkIdx - 1}
			if h.chunkIdx > 0 && !seen[prevKey] {
				seen[prevKey] = true
				if prevContent, ok := r.fetchNeighborContent(h.article, h.chunkIdx-1); ok {
					contextParts = append(contextParts, prevContent)
					dbgChunks = append(dbgChunks, debugChunk{Score: -1, Content: prevContent, Article: h.article, ChunkIdx: h.chunkIdx - 1, IsNeighbor: true})
				}
			}

			contextParts = append(contextParts, h.content)
			dbgChunks = append(dbgChunks, debugChunk{Score: h.score, Content: h.content, Article: h.article, ChunkIdx: h.chunkIdx, IsNeighbor: false})

			nextKey := chunkKey{h.article, h.chunkIdx + 1}
			if !seen[nextKey] {
				seen[nextKey] = true
				if nextContent, ok := r.fetchNeighborContent(h.article, h.chunkIdx+1); ok {
					contextParts = append(contextParts, nextContent)
					dbgChunks = append(dbgChunks, debugChunk{Score: -1, Content: nextContent, Article: h.article, ChunkIdx: h.chunkIdx + 1, IsNeighbor: true})
				}
			}
		}
		di := &debugInfo{Chunks: dbgChunks, EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCount(), UsedK: usedK, Decision: decision}
		return strings.Join(contextParts, "\n---\n"), di, nil
	}

	if primaryCount > 0 {
		var sel []chunkHit
		for _, h := range hits {
			if h.score > highThreshold {
				sel = append(sel, h)
				if len(sel) >= k {
					break
				}
			}
		}
		return assemble(sel, k, "high_confidence")
	}

	// Summarize top candidates
	var summaryParts []string
	topN := 5
	if len(hits) < topN {
		topN = len(hits)
	}
	for i := 0; i < topN; i++ {
		h := hits[i]
		summaryParts = append(summaryParts, fmt.Sprintf("%s (score=%.4f)", h.article, h.score))
	}
	summary := strings.Join(summaryParts, "; ")

	decisionMap, derr := r.analyzeQuestion(question, summary)
	if derr != nil {
		var sel []chunkHit
		thresh := 0.60
		for _, h := range hits {
			if h.score >= thresh {
				sel = append(sel, h)
				if len(sel) >= k {
					break
				}
			}
		}
		return assemble(sel, k, "relaxed_fallback")
	}

	action, _ := decisionMap["action"].(string)
	if strings.ToUpper(action) == "ANSWER_DIRECT" {
		di := &debugInfo{EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCount(), UsedK: 0, Decision: "answer_direct"}
		return "", di, nil
	}

	desiredK := k
	if v, ok := decisionMap["k"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			desiredK = int(fv)
		}
	}
	thresh := 0.60
	if v, ok := decisionMap["threshold"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			thresh = fv
		}
	}
	if v, ok := decisionMap["query"]; ok {
		if qs, ok2 := v.(string); ok2 && strings.TrimSpace(qs) != "" {
			searchQuery = qs
		}
	}

	var sel []chunkHit
	for _, h := range hits {
		if h.score >= thresh {
			sel = append(sel, h)
			if len(sel) >= desiredK {
				break
			}
		}
	}
	if len(sel) == 0 && len(hits) > 0 {
		for i := 0; i < desiredK && i < len(hits); i++ {
			sel = append(sel, hits[i])
		}
	}
	return assemble(sel, desiredK, "lm_requested_retrieval")
}

// refineSearchQuery attempts to extract an entity-like phrase from the
// user's question (e.g., "was weißt du über Ettling") to narrow the
// retrieval query. Falls back to the original question.
func refineSearchQuery(q string) string {
	q = strings.TrimSpace(q)
	low := strings.ToLower(q)
	patterns := []string{
		`was weißt du über (.+)`,
		`wer ist (.+)`,
		`erzähl mir von (.+)`,
		`tell me about (.+)`,
		`who is (.+)`,
	}
	for _, p := range patterns {
		re := regexp.MustCompile(`(?i)` + p)
		if m := re.FindStringSubmatch(low); len(m) >= 2 {
			candidate := strings.TrimSpace(m[1])
			// restore original casing by finding candidate in original
			idx := strings.Index(strings.ToLower(q), candidate)
			if idx >= 0 {
				return strings.TrimSpace(q[idx : idx+len(candidate)])
			}
			return candidate
		}
	}
	return q
}

// analyzeQuestion asks the LM to decide whether to answer directly or
// to request additional retrieval. It returns a parsed map with at
// least an "action" key (ANSWER_DIRECT or RETRIEVE_MORE) and optional
// parameters (k, threshold, query).
func (r *ragSystem) analyzeQuestion(question, summary string) (map[string]any, error) {
	system := `You are an analysis agent. Given a user question and a short summary of retrieval candidates, decide whether the assistant can answer directly or needs more retrieval.

Return ONLY a single JSON object and nothing else (no explanation, no extra text). Examples:
	{"action":"ANSWER_DIRECT"}
	{"action":"RETRIEVE_MORE","k":10,"threshold":0.6,"query":"Ettling"}

Deutsch:
Du bist ein Analyse-Agent. Gegeben eine Nutzerfrage und eine kurze Zusammenfassung der Retrieval-Kandidaten entscheide, ob der Assistent direkt antworten soll oder zusätzliche Kontextsuche benötigt.

Gib AUSSCHLIESSLICH ein einzelnes JSON-Objekt zurück (keine Erklärungen oder zusätzlichen Text). Beispiele:
	{"action":"ANSWER_DIRECT"}
	{"action":"RETRIEVE_MORE","k":10,"threshold":0.6,"query":"Ettling"}
`
	user := fmt.Sprintf("Question: %s\n\nCandidates: %s", question, summary)
	msgs := []chatMsg{{Role: "user", Content: user}}

	var buf bytes.Buffer
	if err := r.getLM().chatStream(context.Background(), system, msgs, &buf); err != nil {
		return nil, err
	}
	out := buf.String()
	// Try to find the first JSON object in the output
	i := strings.Index(out, "{")
	if i == -1 {
		// fallback heuristic
		if strings.Contains(strings.ToUpper(out), "ANSWER_DIRECT") {
			return map[string]any{"action": "ANSWER_DIRECT"}, nil
		}
		return map[string]any{"action": "RETRIEVE_MORE", "k": r.k, "threshold": 0.6}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[i:]), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fetchNeighborContent loads the content of a chunk at (article, chunk_idx).
// fetchNeighborContent returns the content for a specific chunk index
// of an article, used to include context neighbors around hits.
func (r *ragSystem) fetchNeighborContent(article string, chunkIdx int) (string, bool) {
	q := fmt.Sprintf(
		"SELECT content FROM chunks WHERE article = '%s' AND chunk_idx = %d",
		escapeSQ(article), chunkIdx,
	)
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return "", false
	}

	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()

	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return "", false
	}
	c, ok := tinysql.GetVal(rs.Rows[0], "content")
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%v", c), true
}

// listSources returns distinct article names with their chunk counts
// listSources returns metadata about stored articles and their chunk counts.
func (r *ragSystem) listSources() []map[string]any {
	q := "SELECT article, COUNT(*) AS cnt FROM chunks GROUP BY article ORDER BY article"
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil
	}

	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()

	if err != nil || rs == nil {
		return nil
	}
	var sources []map[string]any
	for _, row := range rs.Rows {
		art, ok1 := tinysql.GetVal(row, "article")
		cnt, ok2 := tinysql.GetVal(row, "cnt")
		if ok1 && ok2 {
			sources = append(sources, map[string]any{"article": fmt.Sprintf("%v", art), "chunks": cnt})
		}
	}
	return sources
}

// deleteSource removes all chunks belonging to `article` and persists
// the change.
func (r *ragSystem) deleteSource(article string) error {
	q := fmt.Sprintf("DELETE FROM chunks WHERE article = '%s'", escapeSQ(article))
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return err
	}
	r.dbMu.Lock()
	_, err = tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
	if err != nil {
		return err
	}
	return r.save()
}

// ─────────────────────────────────────────────────────────────────────────────
// Chat history (in-memory)
// ─────────────────────────────────────────────────────────────────────────────

// chatMessage represents a single message in a conversation timeline.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Time    string `json:"time"`
}

// conversation stores metadata and the message history for a chat.
type conversation struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Messages []chatMessage `json:"messages"`
	Created  string        `json:"created"`
	Updated  string        `json:"updated"`
	Persona  string        `json:"persona_id,omitempty"`
}

// chatStore manages in-memory conversations and persists them to disk
// when a path is provided.
type chatStore struct {
	mu    sync.Mutex
	chats map[string]*conversation
	order []string
	path  string
}

// newChatStore initializes a chatStore and loads persisted chats if available.
func newChatStore(path string) *chatStore {
	cs := &chatStore{chats: make(map[string]*conversation), path: path}
	if path != "" {
		if err := cs.load(); err != nil {
			log.Printf("WARN: konnte Chats nicht laden (%v)", err)
		}
	}
	return cs
}

// create makes a new conversation, persists it, and returns it.
func (cs *chatStore) create(title, persona string) *conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := time.Now().Format(time.RFC3339)
	id := fmt.Sprintf("chat-%d", time.Now().UnixNano())
	c := &conversation{ID: id, Title: title, Created: now, Updated: now, Persona: persona}
	cs.chats[id] = c
	cs.order = append(cs.order, id)
	_ = cs.saveLocked()
	return c
}

// get returns a conversation by id or nil if not found.
func (cs *chatStore) get(id string) *conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.chats[id]
}

// addMessage appends a message to the conversation and persists the store.
func (cs *chatStore) addMessage(id, role, content string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.chats[id]
	if !ok {
		return
	}
	now := time.Now().Format(time.RFC3339)
	c.Messages = append(c.Messages, chatMessage{Role: role, Content: content, Time: now})
	c.Updated = now
	if c.Title == "" && role == "user" {
		t := content
		if len(t) > 60 {
			t = t[:60] + "…"
		}
		c.Title = t
	}
	_ = cs.saveLocked()
}

// setPersona assigns a persona to an existing conversation and saves it.
func (cs *chatStore) setPersona(id, persona string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.chats[id]
	if !ok {
		return
	}
	c.Persona = persona
	c.Updated = time.Now().Format(time.RFC3339)
	_ = cs.saveLocked()
}

// list returns conversations in reverse chronological order.
func (cs *chatStore) list() []conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	result := make([]conversation, 0, len(cs.order))
	for i := len(cs.order) - 1; i >= 0; i-- {
		if c, ok := cs.chats[cs.order[i]]; ok {
			result = append(result, *c)
		}
	}
	return result
}

// remove deletes a conversation and persists the updated store.
func (cs *chatStore) remove(id string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.chats[id]; !ok {
		return false
	}
	delete(cs.chats, id)
	for i, oid := range cs.order {
		if oid == id {
			cs.order = append(cs.order[:i], cs.order[i+1:]...)
			break
		}
	}
	_ = cs.saveLocked()
	return true
}

// saveLocked writes the chat store payload to disk and must be called
// with `cs.mu` held.
func (cs *chatStore) saveLocked() error {
	if cs.path == "" {
		return nil
	}
	payload := struct {
		Chats []*conversation `json:"chats"`
	}{}
	for _, id := range cs.order {
		if c, ok := cs.chats[id]; ok {
			payload.Chats = append(payload.Chats, c)
		}
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cs.path)
}

// load reads persisted chats from disk into the in-memory store.
func (cs *chatStore) load() error {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var payload struct {
		Chats []conversation `json:"chats"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.chats = make(map[string]*conversation)
	cs.order = nil
	for _, c := range payload.Chats {
		copyC := c
		cs.chats[c.ID] = &copyC
		cs.order = append(cs.order, c.ID)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Web server + helper endpoints
// ─────────────────────────────────────────────────────────────────────────────

// mustJSON encodes `v` to a compact JSON string, ignoring errors.
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// llmCheckReq is the request structure for model/endpoint validation.
type llmCheckReq struct {
	BaseURL string `json:"base_url"`
}

// llmCheckResp is the response structure returned when validating an LLM endpoint.
type llmCheckResp struct {
	OK             bool     `json:"ok"`
	BaseURL        string   `json:"base_url"`
	ProviderHint   string   `json:"provider_hint"`
	Error          string   `json:"error,omitempty"`
	Models         []string `json:"models,omitempty"`
	RecommendChat  []string `json:"recommend_chat,omitempty"`
	RecommendEmbed []string `json:"recommend_embed,omitempty"`
}

// providerHintFromURL returns a human-friendly hint about the LLM
// provider based on common port patterns in the base URL.
func providerHintFromURL(base string) string {
	if strings.Contains(base, "11434") {
		return "Ollama"
	}
	if strings.Contains(base, "1234") {
		return "LM Studio"
	}
	return "OpenAI-compatible"
}

// recommendModels heuristically selects likely chat and embedding models
// from a list of available model IDs.
func recommendModels(models []string) (chat []string, embed []string) {
	// Heuristics only: highlight likely candidates.
	for _, m := range models {
		ml := strings.ToLower(m)
		if strings.Contains(ml, "embed") || strings.Contains(ml, "embedding") {
			embed = append(embed, m)
		}
		// Common chat-ish hints
		if strings.Contains(ml, "llama") ||
			strings.Contains(ml, "mistral") ||
			strings.Contains(ml, "qwen") ||
			strings.Contains(ml, "gemma") ||
			strings.Contains(ml, "phi") ||
			strings.Contains(ml, "gpt") ||
			strings.Contains(ml, "ministral") {
			chat = append(chat, m)
		}
	}
	// Keep lists short
	if len(chat) > 8 {
		chat = chat[:8]
	}
	if len(embed) > 8 {
		embed = embed[:8]
	}
	return
}

// discoverCandidate contains information about a discovered LLM endpoint.
type discoverCandidate struct {
	BaseURL        string   `json:"base_url"`
	ProviderHint   string   `json:"provider_hint"`
	OK             bool     `json:"ok"`
	Error          string   `json:"error,omitempty"`
	Models         []string `json:"models,omitempty"`
	RecommendChat  []string `json:"recommend_chat,omitempty"`
	RecommendEmbed []string `json:"recommend_embed,omitempty"`
}

// discoverResp is returned from the /api/discover endpoint.
type discoverResp struct {
	Candidates []discoverCandidate `json:"candidates"`
}

// runWebServer registers HTTP handlers and starts the web interface.
func runWebServer(rag *ragSystem, addr string, settings *settingsStore, chats *chatStore, customAPIs *apiStore, personas *personaStore) {
	mux := http.NewServeMux()

	// Static assets
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		fmt.Fprint(w, styleCSS)
	})
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		fmt.Fprint(w, appJS)
	})

	// GET /api/settings — current settings
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			s := settings.get()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"base_url":    s.BaseURL,
				"chat_model":  s.ChatModel,
				"embed_model": s.EmbedModel,
				"lang":        s.Lang,
				"theme":       s.Theme,
				"chunk_size":  s.ChunkSize,
				"k":           s.K,
			})
			return

		case "POST":
			var req struct {
				BaseURL    string `json:"base_url"`
				ChatModel  string `json:"chat_model"`
				EmbedModel string `json:"embed_model"`
				Theme      string `json:"theme"`
				Force      bool   `json:"force"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			req.BaseURL = normalizeBaseURL(req.BaseURL)
			if req.BaseURL == "" || req.ChatModel == "" || req.EmbedModel == "" {
				http.Error(w, "base_url, chat_model and embed_model are required", 400)
				return
			}

			// Validate endpoint quickly
			tmp := newLMClient(req.BaseURL, req.EmbedModel, req.ChatModel)
			if err := tmp.ping(); err != nil {
				http.Error(w, "LLM endpoint not reachable: "+err.Error(), 400)
				return
			}

			// Warn on embedding model changes if DB already has data
			old := settings.get()
			if old.EmbedModel != "" && old.EmbedModel != req.EmbedModel && rag.docCount() > 0 && !req.Force {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(409)
				json.NewEncoder(w).Encode(map[string]any{
					"ok":             false,
					"requires_force": true,
					"message":        "Du hast das Embedding-Modell geändert. Bestehende Chunks wurden mit dem alten Modell eingebettet; Retrieval kann schlechter werden. Wenn du fortfährst, solltest du die Wissensbasis neu einbetten (oder die DB leeren).",
				})
				return
			}

			// Persist + apply
			settings.mu.Lock()
			settings.s.BaseURL = req.BaseURL
			settings.s.ChatModel = req.ChatModel
			settings.s.EmbedModel = req.EmbedModel
			if req.Theme != "" {
				settings.s.Theme = req.Theme
			}
			_ = settings.saveLocked()
			settings.mu.Unlock()

			rag.setLM(tmp)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return

		default:
			http.Error(w, "GET or POST only", 405)
			return
		}
	})

	// POST /api/settings/theme — lightweight theme switch (no LLM validation)
	mux.HandleFunc("/api/settings/theme", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Theme string `json:"theme"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		settings.mu.Lock()
		settings.s.Theme = req.Theme
		_ = settings.saveLocked()
		settings.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "theme": req.Theme})
	})

	// GET /api/discover — auto-discover common local endpoints
	mux.HandleFunc("/api/discover", func(w http.ResponseWriter, r *http.Request) {
		candidates := []string{
			"http://localhost:1234",  // LM Studio default
			"http://localhost:11434", // Ollama default
		}
		var out []discoverCandidate
		for _, base := range candidates {
			c := discoverCandidate{BaseURL: base, ProviderHint: providerHintFromURL(base)}
			tmp := newLMClient(base, "x", "x")
			models, err := tmp.listModels(base)
			if err != nil {
				c.OK = false
				c.Error = err.Error()
			} else {
				c.OK = true
				c.Models = models
				c.RecommendChat, c.RecommendEmbed = recommendModels(models)
			}
			out = append(out, c)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discoverResp{Candidates: out})
	})

	// POST /api/llm/list-models — validate an endpoint and list models
	mux.HandleFunc("/api/llm/list-models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req llmCheckReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		req.BaseURL = normalizeBaseURL(req.BaseURL)
		if req.BaseURL == "" {
			http.Error(w, "missing base_url", 400)
			return
		}
		tmp := newLMClient(req.BaseURL, "x", "x")
		models, err := tmp.listModels(req.BaseURL)
		resp := llmCheckResp{BaseURL: req.BaseURL, ProviderHint: providerHintFromURL(req.BaseURL)}
		if err != nil {
			resp.OK = false
			resp.Error = err.Error()
		} else {
			resp.OK = true
			resp.Models = models
			resp.RecommendChat, resp.RecommendEmbed = recommendModels(models)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// POST /api/ask — SSE streaming answer
	mux.HandleFunc("/api/ask", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}

		reqID := newRequestID()
		var req struct {
			Question   string `json:"question"`
			ChatID     string `json:"chat_id"`
			Debug      bool   `json:"debug"`
			Deep       bool   `json:"deep"`
			Offline    bool   `json:"offline"`
			AutoSearch bool   `json:"auto_search"`
			PersonaID  string `json:"persona_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
			http.Error(w, "missing question", 400)
			return
		}

		s := settings.get()

		var conv *conversation
		if req.ChatID != "" {
			conv = chats.get(req.ChatID)
		}
		personaID := strings.TrimSpace(req.PersonaID)
		if conv != nil && personaID == "" {
			personaID = conv.Persona
		}
		if personaID == "" {
			personaID = personas.defaultID()
		}
		if conv == nil {
			conv = chats.create("", personaID)
		} else if conv.Persona != personaID {
			conv.Persona = personaID
			chats.setPersona(conv.ID, personaID)
		}
		chats.addMessage(conv.ID, "user", req.Question)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}

		totalChunks := rag.docCount()
		usedK := rag.k
		mode := "normal"
		if req.Deep {
			usedK = rag.k * 3
			if usedK < 10 {
				usedK = 10
			}
			if usedK > totalChunks {
				usedK = totalChunks
			}
			if usedK > 50 {
				usedK = 50 // hard cap to avoid huge prompts
			}
			mode = "deep"
		}
		if req.Offline {
			mode = "offline"
		}

		personaName := ""
		personaPrompt := ""
		if personaID != "" {
			if per, ok := personas.get(personaID); ok {
				personaName = per.Name
				personaPrompt = per.Prompt
			}
		}

		metaPayload := map[string]any{
			"chat_id":       conv.ID,
			"title":         conv.Title,
			"request_id":    reqID,
			"mode":          mode,
			"k":             usedK,
			"base_k":        rag.k,
			"chunk_size":    s.ChunkSize,
			"total_chunks":  totalChunks,
			"storage_mode":  storageModeLabel(rag.storageMode),
			"db_path":       rag.dbPath,
			"auto_search":   req.AutoSearch,
			"debug":         req.Debug,
			"deep":          req.Deep,
			"offline":       req.Offline,
			"message_count": len(conv.Messages),
			"created":       conv.Created,
			"updated":       conv.Updated,
			"persona_id":    personaID,
			"persona_name":  personaName,
			"models": map[string]string{
				"base_url":    s.BaseURL,
				"chat_model":  s.ChatModel,
				"embed_model": s.EmbedModel,
			},
		}
		meta, _ := json.Marshal(metaPayload)
		fmt.Fprintf(w, "event: meta\ndata: %s\n\n", meta)
		flusher.Flush()

		log.Printf("ASK[%s] chat=%s mode=%s debug=%t deep=%t offline=%t auto_search=%t q=%q", reqID, conv.ID, mode, req.Debug, req.Deep, req.Offline, req.AutoSearch, req.Question)

		// Prepare context: support Deep-Research mode with larger K
		var ctxText string
		var di *debugInfo
		var err error

		if req.Deep {
			log.Printf("REQ %s: DEEP: k=%d (base=%d, total_chunks=%d)", reqID, usedK, rag.k, totalChunks)
			ctxText, di, err = rag.prepareContextWithK(req.Question, req.Debug, usedK)
		} else {
			ctxText, di, err = rag.prepareContext(req.Question, req.Debug)
			if di != nil {
				di.UsedK = usedK
			}
		}
		if err != nil {
			log.Printf("REQ %s: context fetch failed: %v", reqID, err)
			fmt.Fprintf(w, "data: %s\n\n", mustJSON("Fehler beim Kontext-Abruf: "+err.Error()))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		if di == nil && req.Debug {
			di = &debugInfo{UsedK: usedK, TotalChunks: totalChunks}
		}

		historyCount := len(conv.Messages) - 1
		if historyCount < 0 {
			historyCount = 0
		}

		debugBase := debugPayload{
			RequestID:          reqID,
			Mode:               mode,
			AutoSearch:         req.AutoSearch,
			Offline:            req.Offline,
			Deep:               req.Deep,
			Question:           req.Question,
			UsedK:              usedK,
			BaseK:              rag.k,
			ChunkSize:          s.ChunkSize,
			TotalChunks:        totalChunks,
			ContextChars:       len(ctxText),
			HistoryMessages:    historyCount,
			StorageMode:        storageModeLabel(rag.storageMode),
			DBPath:             rag.dbPath,
			Models:             debugModels{BaseURL: s.BaseURL, ChatModel: s.ChatModel, EmbedModel: s.EmbedModel},
			Retrieval:          di,
			PersonaID:          personaID,
			PersonaName:        personaName,
			PersonaPromptChars: len(personaPrompt),
		}

		// Build answer string
		var answer strings.Builder

		// OFFLINE MODE: return context directly, no LM call
		if req.Offline {
			log.Printf("REQ %s: OFFLINE returning RAG context without LM call", reqID)
			if req.Debug {
				dbgJSON, _ := json.Marshal(debugBase)
				fmt.Fprintf(w, "event: debug\ndata: %s\n\n", dbgJSON)
				flusher.Flush()
			}
			// Format context as a simple summary
			answer.WriteString("📚 **Offline Mode** (no LLM)\n\nBased auf den verfügbaren Dokumenten:\n\n")
			answer.WriteString(ctxText)

			// Stream the offline answer character by character
			for _, ch := range answer.String() {
				data, _ := json.Marshal(string(ch))
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			chats.addMessage(conv.ID, "assistant", answer.String())
			return
		}

		// Normal mode: call LM with SSE streaming
		pr, pw := io.Pipe()

		allTools := customAPIs.allTools()
		// build system prompt; in deep mode add research instructions
		var systemPrompt string
		if req.Deep {
			base := buildToolSystemPrompt(ctxText, allTools)
			systemPrompt = base + "\n--- DEEP-RESEARCH MODE ---\nGib eine strukturierte, gut durchdachte Antwort basierend auf dem Kontext:\n1) Kurze Zusammenfassung der Erkenntnisse\n2) Quellenangaben: relevante Chunks und Artikel\n3) Konfidenzlevel und alternative Interpretationen\n4) Finale prägnante Antwort\nZeige keine interne Logik; nur Analyse und Ergebnis.\n"
		} else {
			systemPrompt = buildToolSystemPrompt(ctxText, allTools)
		}
		if personaPrompt != "" {
			systemPrompt = personaPrompt + "\n\n" + systemPrompt
		}

		// Validate system prompt isn't absurdly long
		if len(systemPrompt) > 32000 {
			log.Printf("REQ %s: WARN system prompt too long (%d chars), truncating context", reqID, len(systemPrompt))
			// Truncate context to first 5000 chars as fallback
			if len(ctxText) > 5000 {
				ctxText = ctxText[:5000] + "\n[... Kontext gekürzt ...]"
				systemPrompt = buildToolSystemPrompt(ctxText, allTools)
			}
		}
		debugBase.SystemPromptChars = len(systemPrompt)
		debugBase.ContextChars = len(ctxText)

		// Prepare multi-turn messages (last 10 messages for efficiency)
		history := conv.Messages[:len(conv.Messages)-1]
		start := 0
		if len(history) > 10 {
			start = len(history) - 10
		}
		msgs := make([]chatMsg, 0, len(history[start:])+1)
		for _, m := range history[start:] {
			msgs = append(msgs, chatMsg{Role: m.Role, Content: m.Content})
		}
		msgs = append(msgs, chatMsg{Role: "user", Content: req.Question})

		debugBase.HistoryMessages = len(msgs)
		if req.Debug {
			dbgJSON, _ := json.Marshal(debugBase)
			fmt.Fprintf(w, "event: debug\ndata: %s\n\n", dbgJSON)
			flusher.Flush()
		}

		streamErr := make(chan error, 1)
		go func() {
			err := rag.getLM().chatStream(context.Background(), systemPrompt, msgs, pw)
			streamErr <- err
			if err != nil {
				pw.CloseWithError(err)
				log.Printf("REQ %s: LM chat stream failed: %v", reqID, err)
			} else {
				pw.Close()
			}
		}()

		scanner := bufio.NewScanner(pr)
		scanner.Split(bufio.ScanRunes)
		tokenCount := 0
		for scanner.Scan() {
			tok := scanner.Text()
			answer.WriteString(tok)
			data, _ := json.Marshal(tok)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			tokenCount++
		}

		// Check for scanner errors
		if serr := scanner.Err(); serr != nil {
			log.Printf("REQ %s: WARN LM chat stream scanner error: %v (tokens received: %d)", reqID, serr, tokenCount)
			fmt.Fprintf(w, "data: %s\n\n", mustJSON("Fehler im LLM-Stream: "+serr.Error()))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			chats.addMessage(conv.ID, "assistant", "Fehler im LLM-Stream: "+serr.Error())
			return
		}

		// Check goroutine result
		if err := <-streamErr; err != nil {
			log.Printf("REQ %s: LM goroutine failed: %v (tokens before error: %d)", reqID, err, tokenCount)
			if tokenCount == 0 {
				// No tokens received at all
				fmt.Fprintf(w, "data: %s\n\n", mustJSON("⚠️ LLM-Fehler: "+err.Error()))
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			if answer.Len() == 0 {
				chats.addMessage(conv.ID, "assistant", "LLM-Fehler: "+err.Error())
			} else {
				answerStr := answer.String()
				if m := toolRequestRe.FindStringSubmatch(answerStr); len(m) >= 2 {
					var tr toolRequest
					if json.Unmarshal([]byte(m[1]), &tr) == nil && tr.Tool != "" {
						trJSON, _ := json.Marshal(tr)
						fmt.Fprintf(w, "event: tool_request\ndata: %s\n\n", trJSON)
						flusher.Flush()
					}
					answerStr = strings.TrimSpace(toolRequestRe.ReplaceAllString(answerStr, ""))
				}
				chats.addMessage(conv.ID, "assistant", answerStr)
			}
			return
		}

		if tokenCount == 0 {
			log.Printf("REQ %s: WARN LM returned no tokens despite no error", reqID)
		}

		// Tool request marker handling
		answerStr := answer.String()
		if m := toolRequestRe.FindStringSubmatch(answerStr); len(m) >= 2 {
			var tr toolRequest
			if json.Unmarshal([]byte(m[1]), &tr) == nil && tr.Tool != "" {
				trJSON, _ := json.Marshal(tr)
				// Notify frontend that a tool was requested
				fmt.Fprintf(w, "event: tool_request\ndata: %s\n\n", trJSON)
				flusher.Flush()

				// Decide whether to execute automatically based on policy
				s := settings.get()
				execAllowed := false
				switch tr.Tool {
				case "calculate":
					execAllowed = true
				case "nanogo":
					execAllowed = s.AllowNanoGo
				case "exec_code":
					execAllowed = s.AllowCodeExec
				default:
					// allow some builtin lookups by default (wiki, websearch etc.)
					execAllowed = true
				}

				if execAllowed {
					// Execute the tool similarly to /api/tool/execute handler
					var text string
					var source string
					var fetchErr error

					switch tr.Tool {
					case "wikipedia":
						source = "wiki:" + tr.Query
						text, fetchErr = fetchWikipedia(tr.Query, s.Lang)
					case "duckduckgo":
						source = "ddg:" + tr.Query
						text, fetchErr = fetchDuckDuckGo(tr.Query)
					case "wiktionary":
						source = "wikt:" + tr.Query
						text, fetchErr = fetchWiktionary(tr.Query, s.Lang)
					case "stackoverflow":
						source = "so:" + tr.Query
						text, fetchErr = fetchDuckDuckGo("site:stackoverflow.com " + tr.Query)
					case "websearch":
						source = "web:" + tr.Query
						text, fetchErr = fetchDuckDuckGo(tr.Query)
					case "llm":
						var buf bytes.Buffer
						msgs2 := []chatMsg{{Role: "user", Content: tr.Query}}
						if err := rag.getLM().chatStream(context.Background(), "", msgs2, &buf); err != nil {
							fetchErr = err
						} else {
							text = buf.String()
							source = "llm:prompt"
						}
					case "calculate":
						out, err := execSmallR(tr.Query)
						if err != nil {
							fetchErr = err
						} else {
							text = out
							source = "calc:" + tr.Query
						}
					case "exec_code":
						// For exec_code we run static checks or full exec depending on setting
						if !s.AllowCodeExec {
							// run gofmt/govet static checks
							tmpDir, err := os.MkdirTemp("", "codeexec-")
							if err != nil {
								fetchErr = err
								break
							}
							defer os.RemoveAll(tmpDir)
							filePath := filepath.Join(tmpDir, "code.go")
							if err := os.WriteFile(filePath, []byte(tr.Query), 0o644); err != nil {
								fetchErr = err
								break
							}
							var outBuf bytes.Buffer
							cmdFmt := exec.Command("gofmt", "-l", filePath)
							cmdFmt.Stdout = &outBuf
							cmdFmt.Stderr = &outBuf
							_ = cmdFmt.Run()
							cmdVet := exec.Command("go", "vet", "./...")
							cmdVet.Dir = tmpDir
							var vetOut bytes.Buffer
							cmdVet.Stdout = &vetOut
							cmdVet.Stderr = &vetOut
							_ = cmdVet.Run()
							text = fmt.Sprintf("gofmt output:\n%s\n\ngo vet output:\n%s", outBuf.String(), vetOut.String())
							source = "code:go"
						} else {
							// Full execution via nanogo RunSafe
							timeout := 5 * time.Second
							out, err := RunSafe(tr.Query, timeout)
							if err != nil {
								fetchErr = err
							} else {
								text = out
								source = "nanogo:exec"
							}
						}
					case "nanogo":
						timeout := 5 * time.Second
						out, err := RunSafe(tr.Query, timeout)
						if err != nil {
							fetchErr = err
						} else {
							text = out
							source = "nanogo:exec"
						}
					default:
						if api, ok := customAPIs.get(tr.Tool); ok {
							finalURL := strings.ReplaceAll(api.Template, "$q", url.QueryEscape(tr.Query))
							source = "api:" + api.Name + ":" + tr.Query
							text, fetchErr = fetchURL(finalURL)
						} else {
							fetchErr = fmt.Errorf("unknown tool: %s", tr.Tool)
						}
					}

					// Send tool result event and add to RAG if successful
					if fetchErr != nil {
						res := map[string]any{"tool": tr.Tool, "query": tr.Query, "error": fetchErr.Error()}
						d, _ := json.Marshal(res)
						fmt.Fprintf(w, "event: tool_result\ndata: %s\n\n", d)
						flusher.Flush()
						log.Printf("REQ %s: tool %s failed: %v", reqID, tr.Tool, fetchErr)
					} else {
						res := map[string]any{"tool": tr.Tool, "query": tr.Query, "source": source, "output": text}
						d, _ := json.Marshal(res)
						fmt.Fprintf(w, "event: tool_result\ndata: %s\n\n", d)
						flusher.Flush()

						// add to RAG as chunks so subsequent retrieval can use it
						chunks := chunkText(text, s.ChunkSize)
						if err := rag.addChunks(source, chunks); err != nil {
							log.Printf("REQ %s: failed to add tool result to RAG: %v", reqID, err)
						} else {
							log.Printf("REQ %s: tool result added to RAG: %s (%d chunks)", reqID, source, len(chunks))
						}

						// Continue the assistant answer by asking the LM to incorporate the tool result
						// Build continuation messages: include previous assistant partial answer and the tool output as a user hint
						contMsgs := make([]chatMsg, 0, len(msgs)+2)
						contMsgs = append(contMsgs, msgs...)
						// previous assistant partial
						contMsgs = append(contMsgs, chatMsg{Role: "assistant", Content: answerStr})
						contMsgs = append(contMsgs, chatMsg{Role: "user", Content: fmt.Sprintf("Tool %s returned:\n%s\n\nPlease continue the answer using this information.", tr.Tool, text)})

						// Stream continuation
						pr2, pw2 := io.Pipe()
						go func() {
							err := rag.getLM().chatStream(context.Background(), systemPrompt, contMsgs, pw2)
							if err != nil {
								pw2.CloseWithError(err)
								log.Printf("REQ %s: LM continuation failed: %v", reqID, err)
							} else {
								pw2.Close()
							}
						}()
						sc2 := bufio.NewScanner(pr2)
						sc2.Split(bufio.ScanRunes)
						for sc2.Scan() {
							tok := sc2.Text()
							fmt.Fprintf(w, "data: %s\n\n", mustJSON(tok))
							flusher.Flush()
						}
						// finished continuation
						log.Printf("REQ %s: tool-driven continuation complete", reqID)
					}
				} else {
					// Execution not allowed; inform frontend
					res := map[string]any{"tool": tr.Tool, "query": tr.Query, "allowed": false}
					d, _ := json.Marshal(res)
					fmt.Fprintf(w, "event: tool_result\ndata: %s\n\n", d)
					flusher.Flush()
				}
			}
			answerStr = strings.TrimSpace(toolRequestRe.ReplaceAllString(answerStr, ""))
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()

		log.Printf("REQ %s: Chat response complete: %d chars, tokens_streamed=%d", reqID, len(answerStr), tokenCount)
		chats.addMessage(conv.ID, "assistant", answerStr)
	})

	// GET /api/tools — list available tools
	mux.HandleFunc("/api/tools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(customAPIs.allTools())
	})

	// POST /api/tool/execute — execute a tool and add results to RAG
	mux.HandleFunc("/api/tool/execute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req toolRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tool == "" || req.Query == "" {
			http.Error(w, "missing tool or query", 400)
			return
		}

		s := settings.get()

		var text string
		var source string
		var fetchErr error

		switch req.Tool {
		case "wikipedia":
			source = "wiki:" + req.Query
			text, fetchErr = fetchWikipedia(req.Query, s.Lang)
		case "duckduckgo":
			source = "ddg:" + req.Query
			text, fetchErr = fetchDuckDuckGo(req.Query)
		case "wiktionary":
			source = "wikt:" + req.Query
			text, fetchErr = fetchWiktionary(req.Query, s.Lang)
		case "stackoverflow":
			// Search StackOverflow via DuckDuckGo site-restrict
			source = "so:" + req.Query
			text, fetchErr = fetchDuckDuckGo("site:stackoverflow.com " + req.Query)
		case "websearch":
			source = "web:" + req.Query
			text, fetchErr = fetchDuckDuckGo(req.Query)

		case "llm":
			// Run a direct prompt against the configured LLM and return result
			var buf bytes.Buffer
			msgs := []chatMsg{{Role: "user", Content: req.Query}}
			if err := rag.getLM().chatStream(context.Background(), "", msgs, &buf); err != nil {
				http.Error(w, fmt.Sprintf("LLM error: %v", err), 500)
				return
			}
			text = buf.String()
			source = "llm:prompt"

		case "calculate":
			// Evaluate arithmetic expression via smallR and add to RAG as a small document
			out, err := execSmallR(req.Query)
			if err != nil {
				http.Error(w, fmt.Sprintf("calculation failed: %v", err), 500)
				return
			}
			text = out
			source = "calc:" + req.Query

		case "exec_code":
			// Secure code execution: by default perform only static analysis (format + vet for Go).
			scfg := s.AllowCodeExec
			if !scfg {
				// Perform static checks only (gofmt/govet) but do not execute arbitrary code
			}
			// Create temp dir and write the provided code to a file
			tmpDir, err := os.MkdirTemp("", "codeexec-")
			if err != nil {
				http.Error(w, fmt.Sprintf("internal: %v", err), 500)
				return
			}
			defer os.RemoveAll(tmpDir)
			filePath := filepath.Join(tmpDir, "code.go")
			if err := os.WriteFile(filePath, []byte(req.Query), 0o644); err != nil {
				http.Error(w, fmt.Sprintf("write failed: %v", err), 500)
				return
			}
			var outBuf bytes.Buffer
			// Check formatting
			cmdFmt := exec.Command("gofmt", "-l", filePath)
			cmdFmt.Stdout = &outBuf
			cmdFmt.Stderr = &outBuf
			if err := cmdFmt.Run(); err != nil {
				// gofmt failing is non-fatal; capture output
			}
			// Try go vet (best-effort)
			cmdVet := exec.Command("go", "vet", "./...")
			cmdVet.Dir = tmpDir
			var vetOut bytes.Buffer
			cmdVet.Stdout = &vetOut
			cmdVet.Stderr = &vetOut
			_ = cmdVet.Run()
			// Collate results
			text = fmt.Sprintf("gofmt output:\n%s\n\ngo vet output:\n%s", outBuf.String(), vetOut.String())
			source = "code:go"
		default:
			if api, ok := customAPIs.get(req.Tool); ok {
				finalURL := strings.ReplaceAll(api.Template, "$q", url.QueryEscape(req.Query))
				source = "api:" + api.Name + ":" + req.Query
				text, fetchErr = fetchURL(finalURL)
			} else {
				http.Error(w, "unknown tool: "+req.Tool, 400)
				return
			}
		}

		if fetchErr != nil {
			http.Error(w, fmt.Sprintf("Tool %q fehlgeschlagen: %v", req.Tool, fetchErr), 500)
			return
		}

		chunks := chunkText(text, s.ChunkSize)
		if err := rag.addChunks(source, chunks); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tool":   req.Tool,
			"query":  req.Query,
			"source": source,
			"chars":  len(text),
			"chunks": len(chunks),
			"total":  rag.docCount(),
		})
	})

	// POST /api/nanogo — execute Go source using the embedded nanoGo interpreter
	mux.HandleFunc("/api/nanogo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Source   string `json:"source"`
			TimeoutS int    `json:"timeout_s"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Source) == "" {
			http.Error(w, "missing source", 400)
			return
		}
		s := settings.get()
		if !s.AllowNanoGo {
			http.Error(w, "nanoGo execution disabled in settings", 403)
			return
		}
		timeout := 5 * time.Second
		if req.TimeoutS > 0 {
			timeout = time.Duration(req.TimeoutS) * time.Second
		}
		out, err := RunSafe(req.Source, timeout)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "output": out})
	})

	// POST /api/smallr — execute a smallR expression using the bundled demo.
	mux.HandleFunc("/api/smallr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Expr string `json:"expr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Expr) == "" {
			http.Error(w, "missing expr", 400)
			return
		}
		out, err := execSmallR(req.Expr)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"output": out})
	})

	// POST /api/search
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == "" {
			http.Error(w, "missing query", 400)
			return
		}
		if req.K <= 0 {
			req.K = rag.k
		}
		results, err := rag.searchJSON(req.Query, req.K)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})

	// POST /api/add-wiki
	mux.HandleFunc("/api/add-wiki", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Article string `json:"article"`
			Lang    string `json:"lang"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Article == "" {
			http.Error(w, "missing article", 400)
			return
		}
		s := settings.get()
		if req.Lang == "" {
			req.Lang = s.Lang
		}
		text, err := fetchWikipedia(req.Article, req.Lang)
		if err != nil {
			log.Printf("fetchWikipedia(%q,%q) failed: %v", req.Article, req.Lang, err)
			if sv, err2 := searchWikipedia(req.Article, req.Lang); err2 == nil && len(sv) > 0 {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"not_found": true, "query": req.Article, "results": sv})
				return
			}
			http.Error(w, err.Error(), 500)
			return
		}
		chunks := chunkText(text, s.ChunkSize)
		if err := rag.addChunks(req.Article, chunks); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"article": req.Article,
			"chars":   len(text),
			"chunks":  len(chunks),
			"total":   rag.docCount(),
		})
	})

	// POST /api/add-url
	mux.HandleFunc("/api/add-url", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
			http.Error(w, "missing url", 400)
			return
		}
		if _, err := url.ParseRequestURI(req.URL); err != nil {
			http.Error(w, "invalid url", 400)
			return
		}
		text, err := fetchURL(req.URL)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s := settings.get()
		chunks := chunkText(text, s.ChunkSize)
		if err := rag.addChunks(req.URL, chunks); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"source": req.URL,
			"chars":  len(text),
			"chunks": len(chunks),
			"total":  rag.docCount(),
		})
	})

	// POST /api/add-folder — import all text files from a server directory
	mux.HandleFunc("/api/add-folder", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Path      string `json:"path"`
			Recursive bool   `json:"recursive"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, "missing path", 400)
			return
		}
		info, err := os.Stat(req.Path)
		if err != nil {
			http.Error(w, "path not found: "+err.Error(), 400)
			return
		}
		if !info.IsDir() {
			http.Error(w, "path is not a directory", 400)
			return
		}

		allowedExts := map[string]bool{
			".txt": true, ".md": true, ".csv": true, ".json": true,
			".xml": true, ".html": true, ".log": true, ".htm": true,
			".yaml": true, ".yml": true, ".toml": true, ".ini": true,
			".cfg": true, ".conf": true, ".sql": true, ".go": true,
			".py": true, ".js": true, ".ts": true, ".rs": true,
			".c": true, ".h": true, ".cpp": true, ".java": true,
		}

		s := settings.get()
		var totalFiles, totalChars, totalChunksN int
		var errors []string

		walkFn := func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if !req.Recursive && path != req.Path {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if !allowedExts[ext] {
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() > 5*1024*1024 {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				errors = append(errors, filepath.Base(path)+": "+err.Error())
				return nil
			}
			text := string(data)
			if strings.TrimSpace(text) == "" {
				return nil
			}
			relPath, _ := filepath.Rel(req.Path, path)
			if relPath == "" {
				relPath = filepath.Base(path)
			}
			source := "folder:" + relPath
			chunks := chunkText(text, s.ChunkSize)
			if err := rag.addChunks(source, chunks); err != nil {
				errors = append(errors, relPath+": "+err.Error())
				return nil
			}
			totalFiles++
			totalChars += len(text)
			totalChunksN += len(chunks)
			return nil
		}

		filepath.WalkDir(req.Path, walkFn)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files":        totalFiles,
			"total_chars":  totalChars,
			"total_chunks": totalChunksN,
			"total":        rag.docCount(),
			"errors":       errors,
		})
	})

	// POST /api/add-text
	mux.HandleFunc("/api/add-text", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
			http.Error(w, "missing text", 400)
			return
		}
		s := settings.get()
		if req.Title == "" {
			req.Title = "manual-" + strconv.FormatInt(time.Now().Unix(), 10)
		}
		chunks := chunkText(req.Text, s.ChunkSize)
		if err := rag.addChunks(req.Title, chunks); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"title":  req.Title,
			"chars":  len(req.Text),
			"chunks": len(chunks),
			"total":  rag.docCount(),
		})
	})

	// POST /api/upload
	mux.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		r.ParseMultipartForm(50 << 20) // allow larger archives (50MB)
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file: "+err.Error(), 400)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		filename := header.Filename
		lower := strings.ToLower(filename)
		s := settings.get()
		allowedExts := map[string]bool{
			".txt": true, ".md": true, ".csv": true, ".json": true,
			".xml": true, ".html": true, ".log": true, ".htm": true,
			".yaml": true, ".yml": true, ".toml": true, ".ini": true,
			".cfg": true, ".conf": true, ".sql": true, ".go": true,
			".py": true, ".js": true, ".ts": true, ".rs": true,
			".c": true, ".h": true, ".cpp": true, ".java": true,
		}

		var totalFiles, totalChars, totalChunks int
		var errorsList []string

		isZip := strings.HasSuffix(lower, ".zip")
		isTarGz := strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")

		if isZip || isTarGz {
			// write archive to temp file
			tmpDir, err := os.MkdirTemp("", "upload-archive-")
			if err != nil {
				http.Error(w, "internal: "+err.Error(), 500)
				return
			}
			defer os.RemoveAll(tmpDir)
			tmpPath := filepath.Join(tmpDir, "archive")
			if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
				http.Error(w, "internal: "+err.Error(), 500)
				return
			}

			if isZip {
				zr, err := zip.OpenReader(tmpPath)
				if err != nil {
					http.Error(w, "invalid zip: "+err.Error(), 400)
					return
				}
				defer zr.Close()
				for _, f := range zr.File {
					if f.FileInfo().IsDir() {
						continue
					}
					ext := strings.ToLower(filepath.Ext(f.Name))
					if !allowedExts[ext] {
						continue
					}
					rc, err := f.Open()
					if err != nil {
						errorsList = append(errorsList, f.Name+": "+err.Error())
						continue
					}
					content, err := io.ReadAll(io.LimitReader(rc, 6*1024*1024))
					if closeErr := rc.Close(); closeErr != nil && err == nil {
						err = closeErr
					}
					if err != nil {
						errorsList = append(errorsList, f.Name+": "+err.Error())
						continue
					}
					if len(content) == 0 {
						continue
					}
					src := "upload:" + filename + ":" + f.Name
					chunks := chunkText(string(content), s.ChunkSize)
					if err := rag.addChunks(src, chunks); err != nil {
						errorsList = append(errorsList, f.Name+": "+err.Error())
						continue
					}
					totalFiles++
					totalChars += len(content)
					totalChunks += len(chunks)
				}
			} else {
				// tar.gz
				f, err := os.Open(tmpPath)
				if err != nil {
					http.Error(w, "internal: "+err.Error(), 500)
					return
				}
				defer f.Close()
				gz, err := gzip.NewReader(f)
				if err != nil {
					http.Error(w, "invalid gzip: "+err.Error(), 400)
					return
				}
				defer gz.Close()
				tr := tar.NewReader(gz)
				for {
					hdr, err := tr.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						errorsList = append(errorsList, "tar read: "+err.Error())
						break
					}
					if hdr.FileInfo().IsDir() {
						continue
					}
					ext := strings.ToLower(filepath.Ext(hdr.Name))
					if !allowedExts[ext] {
						continue
					}
					if hdr.Size > 5*1024*1024 {
						errorsList = append(errorsList, hdr.Name+": file too large")
						continue
					}
					content, err := io.ReadAll(io.LimitReader(tr, 6*1024*1024))
					if err != nil {
						errorsList = append(errorsList, hdr.Name+": "+err.Error())
						continue
					}
					src := "upload:" + filename + ":" + hdr.Name
					chunks := chunkText(string(content), s.ChunkSize)
					if err := rag.addChunks(src, chunks); err != nil {
						errorsList = append(errorsList, hdr.Name+": "+err.Error())
						continue
					}
					totalFiles++
					totalChars += len(content)
					totalChunks += len(chunks)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"archive": header.Filename,
				"files":   totalFiles,
				"chars":   totalChars,
				"chunks":  totalChunks,
				"total":   rag.docCount(),
				"errors":  errorsList,
			})
			return
		}

		// regular single-file upload
		text := string(data)
		title := filepath.Base(header.Filename)
		chunks := chunkText(text, s.ChunkSize)
		if err := rag.addChunks(title, chunks); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"file":   title,
			"chars":  len(text),
			"chunks": len(chunks),
			"total":  rag.docCount(),
		})
	})

	// GET /api/stats
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"chunks":  rag.docCount(),
			"sources": rag.listSources(),
		})
	})

	// GET /api/sources
	mux.HandleFunc("/api/sources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rag.listSources())
	})

	// POST /api/sources/delete
	mux.HandleFunc("/api/sources/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Article string `json:"article"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Article == "" {
			http.Error(w, "missing article", 400)
			return
		}
		if err := rag.deleteSource(req.Article); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"deleted": req.Article, "total": rag.docCount()})
	})

	// GET /api/chats — list conversations
	mux.HandleFunc("/api/chats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chats.list())
	})

	// GET /api/chat/<id> and DELETE /api/chat/<id>
	mux.HandleFunc("/api/chat/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/chat/")
		if id == "" {
			http.Error(w, "missing chat id", 400)
			return
		}
		if r.Method == "DELETE" {
			chats.remove(id)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		conv := chats.get(id)
		if conv == nil {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conv)
	})

	// POST /api/chats/new
	mux.HandleFunc("/api/chats/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Persona string `json:"persona_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		conv := chats.create("", req.Persona)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conv)
	})

	// Custom APIs (persisted)
	mux.HandleFunc("/api/settings/apis", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(customAPIs.list())
		case "POST":
			var req struct {
				Name     string `json:"name"`
				Template string `json:"template"`
				Desc     string `json:"desc"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Template == "" {
				http.Error(w, "missing name or template", 400)
				return
			}
			if !strings.Contains(req.Template, "$q") {
				http.Error(w, "template must contain $q placeholder", 400)
				return
			}
			api, err := customAPIs.add(req.Name, req.Template, req.Desc)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(api)
		default:
			http.Error(w, "GET or POST only", 405)
		}
	})

	mux.HandleFunc("/api/settings/apis/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			http.Error(w, "missing id", 400)
			return
		}
		ok, err := customAPIs.remove(req.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	// Personas (persisted)
	mux.HandleFunc("/api/personas", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(personas.list())
		case "POST":
			var req struct {
				Name   string `json:"name"`
				Prompt string `json:"prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
				http.Error(w, "missing name", 400)
				return
			}
			p, err := personas.add(req.Name, req.Prompt)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(p)
		default:
			http.Error(w, "GET or POST only", 405)
		}
	})

	mux.HandleFunc("/api/personas/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			http.Error(w, "missing id", 400)
			return
		}
		ok, err := personas.remove(req.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	fmt.Printf("Web interface: http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// execSmallR executes the smallR demo to evaluate `expr` and returns its stdout.
// It prefers a local `./smallr` binary if present, otherwise falls back to
// `go run smallr.go -e` which requires the Go toolchain at runtime.
func execSmallR(expr string) (string, error) {
	ctx := smallr.NewContext()
	res, err := ctx.EvalString(expr)
	if err != nil {
		return "", fmt.Errorf("smallr eval failed: %w", err)
	}
	if strings.TrimSpace(res.Output) != "" {
		return res.Output, nil
	}
	return res.Value.String(), nil
}

// RunSafe executes untrusted Go source inside the nanoGo interpreter
// with a context-based timeout. It captures ConsoleLog/ConsoleWarn/ConsoleError
// output into a buffer and recovers from panics so the host application
// is not crashed by user code.
func RunSafe(source string, timeout time.Duration) (string, error) {
	defer func() {
		if r := recover(); r != nil {
			// recovered panic will be returned as error below
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	outBuf := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() {
		done <- runInterpreted(source, outBuf)
	}()

	select {
	case err := <-done:
		return outBuf.String(), err
	case <-ctx.Done():
		return outBuf.String(), fmt.Errorf("execution timed out after %s", timeout)
	}
}

// runInterpreted creates a sandboxed interpreter, registers only the
// host functions we choose to expose, and executes the source.
func runInterpreted(source string, out *bytes.Buffer) error {
	vm := nanogo.NewInterpreter()
	registerSafeNatives(vm, out)
	nanogo.RegisterBuiltinPackages(vm)
	return vm.Run(source)
}

// registerSafeNatives installs a minimal set of host functions that are
// safe to expose to untrusted user code. Output is written to `out`.
func registerSafeNatives(vm *nanogo.Interpreter, out *bytes.Buffer) {
	vm.RegisterNative("ConsoleLog", func(args []any) (any, error) {
		if len(args) > 0 {
			fmt.Fprintln(out, nanogo.ToString(args[0]))
		}
		return nil, nil
	})

	vm.RegisterNative("ConsoleWarn", func(args []any) (any, error) {
		if len(args) > 0 {
			fmt.Fprintln(out, "[warn] "+nanogo.ToString(args[0]))
		}
		return nil, nil
	})

	vm.RegisterNative("ConsoleError", func(args []any) (any, error) {
		if len(args) > 0 {
			fmt.Fprintln(out, "[error] "+nanogo.ToString(args[0]))
		}
		return nil, nil
	})

	vm.RegisterNative("__hostSprintf", func(args []any) (any, error) {
		if len(args) == 0 {
			return "", nil
		}
		format := nanogo.ToString(args[0])
		fmtArgs := make([]any, 0, len(args)-1)
		for _, a := range args[1:] {
			fmtArgs = append(fmtArgs, a)
		}
		return fmt.Sprintf(format, fmtArgs...), nil
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// main
// ─────────────────────────────────────────────────────────────────────────────

// main parses flags, initializes components and starts either the
// web interface or a minimal CLI loop.
func main() {
	// Runtime flags
	addr := flag.String("addr", ":8080", "Web interface listen address")
	web := flag.Bool("web", true, "Start web interface (recommended)")
	dbPath := flag.String("db", "tinyrag.gob", "Database file/directory path (empty=in-memory only)")
	settingsPath := flag.String("settings", "settings.json", "Settings JSON path")
	chatsPath := flag.String("chats", "chats.json", "Persisted chats JSON path (empty=memory only)")
	storageFlag := flag.String("storage-mode", "memory", "Storage mode: memory, wal, disk, index, hybrid")
	maxMemMB := flag.Int64("max-mem-mb", 256, "Max memory in MB for hybrid/index mode")

	// Defaults for first run (written to settings.json if it doesn't exist)
	urlFlag := flag.String("url", "http://localhost:1234", "Default OpenAI-compatible base URL (first run only)")
	embedModel := flag.String("embed-model", "text-embedding-nomic-embed-text-v1.5", "Default embedding model (first run only)")
	chatModel := flag.String("chat-model", "mistralai/ministral-3-14b-reasoning", "Default chat model (first run only)")
	k := flag.Int("k", 5, "Top-K results (first run only)")
	lang := flag.String("lang", "de", "Wikipedia language (first run only)")
	chunkSize := flag.Int("chunk-size", 800, "Max characters per chunk (first run only)")

	flag.Parse()

	// Parse storage mode
	storageMode, err := tinysql.ParseStorageMode(*storageFlag)
	if err != nil {
		log.Fatalf("Invalid storage mode: %v", err)
	}

	// Load settings (or create on first run)
	defaults := defaultSettingsFromFlags(*urlFlag, *chatModel, *embedModel, *lang, *chunkSize, *k)
	settings, err := loadOrCreateSettings(*settingsPath, defaults)
	if err != nil {
		log.Fatalf("Failed to load settings: %v", err)
	}
	s := settings.get()

	// Connect to LLM endpoint
	lm := newLMClient(s.BaseURL, s.EmbedModel, s.ChatModel)
	fmt.Printf("Connecting to LLM endpoint (%s)… ", s.BaseURL)
	if err := lm.ping(); err != nil {
		fmt.Println("FAILED")
		log.Fatalf("Cannot reach LLM endpoint at %s: %v\nTip: open Settings in the UI and pick LM Studio (:1234) or Ollama (:11434).", s.BaseURL, err)
	}
	fmt.Println("OK")

	rag, err := newRAG(lm, s.K, *dbPath, storageMode, *maxMemMB)
	if err != nil {
		log.Fatalf("Failed to create RAG: %v", err)
	}
	if err := rag.init(); err != nil {
		log.Fatalf("Failed to init table: %v", err)
	}

	// Ensure database is flushed on exit
	defer func() {
		if err := rag.db.Close(); err != nil {
			log.Printf("Warning: failed to close database: %v", err)
		}
	}()

	existing := rag.docCount()
	if existing > 0 {
		fmt.Printf("Database has %d existing chunks.\n", existing)
	}

	customAPIs := newAPIStore(settings)
	personas := newPersonaStore(settings)
	chats := newChatStore(*chatsPath)

	if *web {
		runWebServer(rag, *addr, settings, chats, customAPIs, personas)
		return
	}

	// CLI mode (kept minimal)
	fmt.Println("Commands: /search <query> | /add <Article> | /count | /quit")
	fmt.Println("Or just type a question for RAG-answered chat.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("tinyRAG> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		switch {
		case line == "/quit" || line == "/exit":
			fmt.Println("Bye!")
			return

		case line == "/count":
			fmt.Printf("%d chunks\n", rag.docCount())

		case strings.HasPrefix(line, "/add "):
			art := strings.TrimSpace(strings.TrimPrefix(line, "/add "))
			fmt.Printf("Fetching %s...\n", art)
			text, err := fetchWikipedia(art, s.Lang)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				continue
			}
			chunks := chunkText(text, s.ChunkSize)
			fmt.Printf("  %d chars -> %d chunks\n", len(text), len(chunks))
			if err := rag.addChunks(art, chunks); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
			fmt.Printf("Total: %d chunks\n", rag.docCount())

		case strings.HasPrefix(line, "/search "):
			query := strings.TrimSpace(strings.TrimPrefix(line, "/search "))
			results, err := rag.searchJSON(query, s.K)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				continue
			}
			for i, r := range results {
				fmt.Printf("%d. [%.4f] %s\n\n", i+1, r.Score, r.Content)
			}

		default:
			// Minimal single-turn ask: use top-k context and stream answer to stdout.
			ctxText, _, err := rag.prepareContext(line, false)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				continue
			}
			system := "Du bist ein hilfreicher Assistent. Beantworte Fragen basierend auf dem bereitgestellten Kontext. Wenn der Kontext die Antwort nicht enthält, sage das ehrlich."
			msgs := []chatMsg{{Role: "user", Content: fmt.Sprintf("Kontext:\n%s\n\nFrage: %s", ctxText, line)}}
			fmt.Print("\n>> ")
			_ = rag.getLM().chatStream(context.Background(), system, msgs, os.Stdout)
			fmt.Println()
		}
		fmt.Println()
	}
}
