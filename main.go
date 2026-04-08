package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// --- Optimizations: caches and pools for performance-sensitive subsystems
var (
	// smallR context pool to reduce allocations when evaluating many expressions
	smallRPool sync.Pool

	// nanoGo concurrency limiter: restrict simultaneous interpreter instances
	// to a small number to avoid CPU/memory spikes from many concurrent runs.
	nanoGoSem = make(chan struct{}, 1) // allow 1 concurrent nanoGo execution by default
)

func init() {
	// smallR pool: create a new context on demand
	smallRPool.New = func() any { return smallr.NewContext() }
}

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
	Version    int    `json:"version"`
	BaseURL    string `json:"base_url"`    // without trailing /v1
	ChatBase   string `json:"chat_base"`   // optional per-model base URL (overrides BaseURL for chat)
	EmbedBase  string `json:"embed_base"`  // optional per-model base URL (overrides BaseURL for embeddings)
	ChatModel  string `json:"chat_model"`  // OpenAI compatible model ID
	EmbedModel string `json:"embed_model"` // OpenAI compatible model ID
	// OpenAIKey stores an OpenAI API key if the user set one in the UI.
	// This is persisted to settings.json. It will not be returned verbatim
	// over the HTTP API; only presence is exposed to the frontend.
	OpenAIKey string `json:"openai_api_key"`
	Lang      string `json:"lang"`
	Theme     string `json:"theme"`
	// ActiveRole is the currently selected demo role (light RBAC).
	// Intended as placeholder until AD/LDAP integration.
	ActiveRole string `json:"active_role"`
	// UsageProfile sets behavior defaults for private vs. business usage.
	// Allowed values: "personal", "commercial".
	UsageProfile string `json:"usage_profile"`
	// ResponseLanguageMode controls response language strategy.
	// Allowed values: "auto" (follow user input), "settings" (force Lang).
	ResponseLanguageMode string `json:"response_language_mode"`
	// RedactPII redacts common personally identifiable data on ingest.
	RedactPII  bool           `json:"redact_pii"`
	ChunkSize  int            `json:"chunk_size"`
	K          int            `json:"k"`
	CustomAPIs []customAPI    `json:"custom_apis"`
	Modules    []moduleConfig `json:"modules"`
	Personas   []persona      `json:"personas"`
	APIUsers   []adminAPIUser `json:"api_users"`
	APIRoutes  []apiRouteRule `json:"api_routes"`
	// AllowCodeExec must be explicitly enabled to allow running user
	// provided code. Defaults to false for safety.
	AllowCodeExec bool `json:"allow_code_exec"`
	// AllowNanoGo enables execution of untrusted Go source via the
	// embedded nanoGo interpreter. Default: false.
	AllowNanoGo bool `json:"allow_nanogo"`
	// AllowShellExec enables execution of shell commands on the server.
	// Default: false for security reasons.
	AllowShellExec bool `json:"allow_shell_exec"`
	// AllowTinyGo enables compilation and execution of TinyGo programs.
	// Default: false for security reasons.
	AllowTinyGo bool `json:"allow_tinygo"`
}

// settingsStore provides a thread-safe wrapper around persisted
// `appSettings`, handling reading and atomic writes to disk.
type settingsStore struct {
	mu   sync.Mutex
	path string
	s    appSettings
}

// package-level settings store (initialized in main)
var settings *settingsStore

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

func normalizeUsageProfile(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "commercial", "business", "enterprise":
		return "commercial"
	default:
		return "personal"
	}
}

func normalizeResponseLanguageMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "settings", "fixed", "force":
		return "settings"
	default:
		return "auto"
	}
}

func normalizeDemoRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "it":
		return "it"
	case "logistik", "logistics":
		return "logistik"
	case "vertrieb", "sales":
		return "vertrieb"
	case "hr", "human_resources", "human-resources":
		return "hr"
	default:
		return "it"
	}
}

func demoRoleLabel(role string) string {
	switch normalizeDemoRole(role) {
	case "logistik":
		return "Logistik"
	case "vertrieb":
		return "Vertrieb"
	case "hr":
		return "HR"
	default:
		return "IT"
	}
}

type demoRolePermissions struct {
	Role          string `json:"role"`
	CanWebFetch   bool   `json:"can_web_fetch"`
	CanBulkIngest bool   `json:"can_bulk_ingest"`
	CanRunModules bool   `json:"can_run_modules"`
	CanRunCode    bool   `json:"can_run_code"`
}

func permissionsForRole(role string) demoRolePermissions {
	norm := normalizeDemoRole(role)
	p := demoRolePermissions{Role: norm}
	switch norm {
	case "it":
		p.CanWebFetch = true
		p.CanBulkIngest = true
		p.CanRunModules = true
		p.CanRunCode = true
	case "logistik":
		p.CanWebFetch = true
		p.CanBulkIngest = true
		p.CanRunModules = true
		p.CanRunCode = false
	case "hr":
		p.CanWebFetch = true
		p.CanBulkIngest = true
		p.CanRunModules = false
		p.CanRunCode = false
	default: // vertrieb
		p.CanWebFetch = true
		p.CanBulkIngest = false
		p.CanRunModules = false
		p.CanRunCode = false
	}
	return p
}

func canRoleUseTool(role, tool string) bool {
	p := permissionsForRole(role)
	t := strings.TrimSpace(strings.ToLower(tool))
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "module:") {
		return p.CanRunModules
	}
	switch t {
	case "shell", "nanogo", "exec_code", "tinygo":
		return p.CanRunCode
	case "wikipedia", "duckduckgo", "wiktionary", "stackoverflow", "websearch", "news", "wikidata", "github":
		return p.CanWebFetch
	case "local_search", "datetime", "calculate", "llm":
		return true
	default:
		// Unknown tool IDs (e.g. custom APIs) are treated as external fetches.
		return p.CanWebFetch
	}
}

func allDemoRoles() []string {
	return []string{"it", "logistik", "vertrieb", "hr"}
}

func roleScopeToken(role string) string {
	return "|" + normalizeDemoRole(role) + "|"
}

func normalizeRoleScopes(scopes []string, fallbackRole string) []string {
	seen := make(map[string]bool, len(scopes))
	var out []string
	for _, raw := range scopes {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" {
			continue
		}
		if v == "all" {
			return allDemoRoles()
		}
		role := normalizeDemoRole(v)
		if !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}
	if len(out) == 0 {
		fb := normalizeDemoRole(fallbackRole)
		return []string{fb}
	}
	sort.Strings(out)
	return out
}

func serializeRoleScope(scopes []string) string {
	if len(scopes) == 0 {
		return "|all|"
	}
	var b strings.Builder
	for _, r := range scopes {
		b.WriteString(roleScopeToken(r))
	}
	return b.String()
}

func parseRoleCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func roleScopeFilterSQL(role string) string {
	token := escapeSQ(roleScopeToken(role))
	return fmt.Sprintf("(role_scope IS NULL OR role_scope = '' OR role_scope = '|all|' OR role_scope LIKE '%%%s%%')", token)
}

func defaultPersonas() []persona {
	return []persona{
		{
			ID:     "persona-default",
			Name:   "Standard",
			Prompt: "Du bist ein praeziser, nuetzlicher Assistent. Antworte klar, knapp und ohne Marketing-Ton. Trenne Fakten, Schlussfolgerungen und Unsicherheiten sauber.",
		},
		{
			ID:     "persona-formal",
			Name:   "Formal",
			Prompt: "Du antwortest sachlich, professionell und gut strukturiert. Vermeide Umgangssprache, bleibe knapp und formuliere belastbare Aussagen vorsichtig.",
		},
		{
			ID:     "persona-friendly",
			Name:   "Friendly",
			Prompt: "Du antwortest freundlich und gut verstaendlich, aber nicht flapsig. Erklaere nur so viel wie noetig und vermeide unbelegte Behauptungen.",
		},
		{
			ID:     "persona-expert",
			Name:   "Expert",
			Prompt: "Du antwortest wie ein fachlich starker Analyst. Prioritaet haben Genauigkeit, Annahmen, Randfaelle und belastbare Begruendung statt Schein-Sicherheit.",
		},
		{
			ID:     "persona-concise",
			Name:   "Concise",
			Prompt: "Du antwortest sehr knapp und direkt. Nur die wichtigsten Punkte, keine Wiederholungen, keine Fuellsaetze, keine spekulativen Ausschmueckungen.",
		},
	}
}

type adminAPIUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	APIKeyHash  string `json:"api_key_hash,omitempty"`
	APIKeyLast4 string `json:"api_key_last4,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

type apiRouteRule struct {
	Path        string `json:"path"`
	MatchType   string `json:"match_type"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Public      bool   `json:"public"`
}

func defaultAPIRouteRules() []apiRouteRule {
	return []apiRouteRule{
		{Path: "/api/process", MatchType: "exact", Description: "Structured processing endpoint", Enabled: true, Public: true},
		{Path: "/api/ask", MatchType: "exact", Description: "Chat SSE endpoint", Enabled: true, Public: true},
		{Path: "/api/search", MatchType: "exact", Description: "Semantic search", Enabled: true, Public: true},
		{Path: "/api/tool/execute", MatchType: "exact", Description: "Execute one tool manually", Enabled: true, Public: true},
		{Path: "/api/add-wiki", MatchType: "exact", Description: "Ingest from Wikipedia", Enabled: true, Public: true},
		{Path: "/api/add-url", MatchType: "exact", Description: "Ingest from URL", Enabled: true, Public: true},
		{Path: "/api/add-folder", MatchType: "exact", Description: "Bulk ingest from folder", Enabled: true, Public: true},
		{Path: "/api/add-text", MatchType: "exact", Description: "Ingest plain text", Enabled: true, Public: true},
		{Path: "/api/upload", MatchType: "exact", Description: "File upload ingest", Enabled: true, Public: true},
		{Path: "/api/modules/run", MatchType: "exact", Description: "Run configured module", Enabled: true, Public: true},
		{Path: "/api/modules/upload", MatchType: "exact", Description: "Module upload bridge", Enabled: true, Public: true},
		{Path: "/api/modules/download", MatchType: "exact", Description: "Module download bridge", Enabled: true, Public: true},
	}
}

func normalizeMatchType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "prefix":
		return "prefix"
	default:
		return "exact"
	}
}

func normalizeAPIRouteRules(rules []apiRouteRule) []apiRouteRule {
	if len(rules) == 0 {
		return defaultAPIRouteRules()
	}
	defaults := defaultAPIRouteRules()
	byPath := make(map[string]apiRouteRule, len(rules))
	for _, rule := range rules {
		rule.Path = strings.TrimSpace(rule.Path)
		if rule.Path == "" {
			continue
		}
		rule.MatchType = normalizeMatchType(rule.MatchType)
		byPath[rule.Path] = rule
	}
	out := make([]apiRouteRule, 0, len(defaults))
	for _, def := range defaults {
		rule, ok := byPath[def.Path]
		if !ok {
			out = append(out, def)
			continue
		}
		if rule.Description == "" {
			rule.Description = def.Description
		}
		if rule.MatchType == "" {
			rule.MatchType = def.MatchType
		}
		out = append(out, rule)
		delete(byPath, def.Path)
	}
	for _, rule := range byPath {
		out = append(out, rule)
	}
	return out
}

func generateAPIToken() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func apiTokenFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func findAPIRouteRule(path string, rules []apiRouteRule) (apiRouteRule, bool) {
	var matched apiRouteRule
	found := false
	longest := -1
	for _, rule := range rules {
		switch normalizeMatchType(rule.MatchType) {
		case "prefix":
			if strings.HasPrefix(path, rule.Path) && len(rule.Path) > longest {
				matched = rule
				found = true
				longest = len(rule.Path)
			}
		default:
			if path == rule.Path {
				return rule, true
			}
		}
	}
	return matched, found
}

func authenticateAPIUser(r *http.Request, users []adminAPIUser) (adminAPIUser, bool) {
	token := apiTokenFromRequest(r)
	if token == "" {
		return adminAPIUser{}, false
	}
	tokenHash := hashAPIToken(token)
	for _, user := range users {
		if !user.Enabled || user.APIKeyHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(user.APIKeyHash), []byte(tokenHash)) == 1 {
			return user, true
		}
	}
	return adminAPIUser{}, false
}

func routePolicyMiddleware(settings *settingsStore, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		rule, ok := findAPIRouteRule(r.URL.Path, s.APIRoutes)
		if !ok {
			next(w, r)
			return
		}
		if !rule.Enabled {
			http.Error(w, "API route disabled by admin policy", 403)
			return
		}
		if rule.Public {
			next(w, r)
			return
		}
		if _, ok := authenticateAPIUser(r, s.APIUsers); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tinyRAG API"`)
			http.Error(w, "valid API key required", 401)
			return
		}
		next(w, r)
	}
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localAdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			http.Error(w, "admin endpoints are only available from localhost", 403)
			return
		}
		next(w, r)
	}
}

// defaultSettingsFromFlags builds initial `appSettings` from CLI flags
// used on first-run when no settings file exists.
func defaultSettingsFromFlags(urlFlag, chatModelFlag, embedModelFlag, lang string, chunkSize, k int) appSettings {
	return appSettings{
		Version:              1,
		BaseURL:              normalizeBaseURL(urlFlag),
		ChatBase:             normalizeBaseURL(urlFlag),
		EmbedBase:            normalizeBaseURL(urlFlag),
		ChatModel:            chatModelFlag,
		EmbedModel:           embedModelFlag,
		OpenAIKey:            "",
		Lang:                 lang,
		ActiveRole:           "it",
		UsageProfile:         "personal",
		ResponseLanguageMode: "auto",
		RedactPII:            false,
		ChunkSize:            chunkSize,
		K:                    k,
		CustomAPIs:           []customAPI{},
		Modules:              defaultModules(),
		Personas:             defaultPersonas(),
		APIUsers:             []adminAPIUser{},
		APIRoutes:            defaultAPIRouteRules(),
		AllowCodeExec:        false,
		AllowNanoGo:          false,
		AllowShellExec:       false,
		AllowTinyGo:          false,
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
				ss.s.Personas = defaultPersonas()
			}
			if len(ss.s.Modules) == 0 {
				ss.s.Modules = defaultModules()
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
	ss.s.ActiveRole = normalizeDemoRole(ss.s.ActiveRole)
	ss.s.UsageProfile = normalizeUsageProfile(ss.s.UsageProfile)
	ss.s.ResponseLanguageMode = normalizeResponseLanguageMode(ss.s.ResponseLanguageMode)
	if ss.s.ChunkSize <= 0 {
		ss.s.ChunkSize = defaults.ChunkSize
	}
	if ss.s.K <= 0 {
		ss.s.K = defaults.K
	}
	ss.s.BaseURL = normalizeBaseURL(ss.s.BaseURL)
	if ss.s.ChatBase == "" {
		ss.s.ChatBase = ss.s.BaseURL
	} else {
		ss.s.ChatBase = normalizeBaseURL(ss.s.ChatBase)
	}
	if ss.s.EmbedBase == "" {
		ss.s.EmbedBase = ss.s.BaseURL
	} else {
		ss.s.EmbedBase = normalizeBaseURL(ss.s.EmbedBase)
	}
	// OpenAIKey may be empty; keep as-is (don't normalize)
	if len(ss.s.Personas) == 0 {
		ss.s.Personas = defaultPersonas()
	}
	ss.s.Modules = normalizeModules(ss.s.Modules)
	for i := range ss.s.APIUsers {
		ss.s.APIUsers[i].Role = normalizeDemoRole(ss.s.APIUsers[i].Role)
	}
	ss.s.APIRoutes = normalizeAPIRouteRules(ss.s.APIRoutes)
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
	client := newHTTPClient(30 * time.Second)
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
	client := newHTTPClient(30 * time.Second)
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
	var parts []string
	var errs []string

	for i, variant := range variants {
		if i >= 3 {
			break
		}
		if text, err := fetchDuckDuckGo(variant); err == nil && strings.TrimSpace(text) != "" {
			parts = append(parts, fmt.Sprintf("DuckDuckGo [%s]\n%s", variant, text))
		} else if err != nil {
			errs = append(errs, "ddg("+variant+"): "+err.Error())
		}
	}

	if text, err := fetchWikidata(variants[0]); err == nil && strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	} else if err != nil {
		errs = append(errs, "wikidata: "+err.Error())
	}

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
		return "", fmt.Errorf(strings.Join(errs, " | "))
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
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
func chunkText(text string, maxLen int) []string {
	paragraphs := strings.Split(text, "\n")
	var chunks []string
	var current []string
	currentLen := 0

	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pLen := len(p)

		if currentLen+pLen > maxLen && len(current) > 0 {
			chunks = append(chunks, strings.Join(current, "\n"))

			// Overlap: retain the last paragraph if it's not the only one,
			// and if its length isn't excessively large (e.g. < maxLen/2).
			lastP := current[len(current)-1]
			if len(current) > 1 && len(lastP) < maxLen/2 {
				current = []string{lastP}
				currentLen = len(lastP) + 1 // +1 for the newline when joining
			} else {
				current = nil
				currentLen = 0
			}
		}

		current = append(current, p)
		currentLen += pLen + 1
	}
	if len(current) > 0 {
		chunks = append(chunks, strings.Join(current, "\n"))
	}
	return chunks
}

var (
	piiEmailRe = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	piiPhoneRe = regexp.MustCompile(`\b\+?[0-9][0-9()\s./-]{6,}[0-9]\b`)
	piiIBANRe  = regexp.MustCompile(`(?i)\b[a-z]{2}[0-9]{2}[a-z0-9]{11,30}\b`)
	piiCardRe  = regexp.MustCompile(`\b(?:[0-9][ -]?){13,19}\b`)
)

func sanitizeTextForIngest(text string, s appSettings) (string, int) {
	if !s.RedactPII || strings.TrimSpace(text) == "" {
		return text, 0
	}
	out := text
	replacements := 0
	apply := func(re *regexp.Regexp, marker string) {
		m := re.FindAllStringIndex(out, -1)
		if len(m) == 0 {
			return
		}
		replacements += len(m)
		out = re.ReplaceAllString(out, marker)
	}
	apply(piiEmailRe, "[REDACTED_EMAIL]")
	apply(piiPhoneRe, "[REDACTED_PHONE]")
	apply(piiIBANRe, "[REDACTED_IBAN]")
	apply(piiCardRe, "[REDACTED_CARD]")
	return out, replacements
}

func chunksForIngest(text string, s appSettings) ([]string, int) {
	sanitized, redactions := sanitizeTextForIngest(text, s)
	return chunkText(sanitized, s.ChunkSize), redactions
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
	apiKey     string
	http       *http.Client
}

func newHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// lmProvider abstracts an LLM client used for embeddings and chat.
type lmProvider interface {
	embed(texts []string) ([][]float64, error)
	embedSingle(text string) ([]float64, error)
	chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error
	chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error
}

// compositeLM allows routing embeddings and chat to different backends.
type compositeLM struct {
	embedClient lmProvider
	chatClient  lmProvider
}

func (c *compositeLM) embed(texts []string) ([][]float64, error) {
	return c.embedClient.embed(texts)
}
func (c *compositeLM) embedSingle(text string) ([]float64, error) {
	return c.embedClient.embedSingle(text)
}
func (c *compositeLM) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	return c.chatClient.chatStream(ctx, system, msgs, w)
}
func (c *compositeLM) chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error {
	return c.chatClient.chatStreamDetailed(ctx, system, msgs, w, thinkW)
}

// newLMClient constructs an `lmClient` configured for the given
// base URL and model names.
func newLMClient(base, embedModel, chatModel, apiKey string) *lmClient {
	return &lmClient{
		base:       normalizeBaseURL(base),
		embedModel: embedModel,
		chatModel:  chatModel,
		apiKey:     apiKey,
		http:       newHTTPClient(120 * time.Second),
	}
}

// ping checks the LLM endpoint for reachability by requesting
// the list of available models.
func (c *lmClient) ping() error {
	req, err := http.NewRequest("GET", c.base+"/v1/models", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
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
	if c.apiKey != "" && strings.Contains(base, "api.openai.com") {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := newHTTPClient(10 * time.Second).Do(req)
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
	req, err := http.NewRequest("POST", c.base+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" && strings.Contains(c.base, "api.openai.com") {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
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

func writeStreamChunk(w io.Writer, s string) error {
	if w == nil || s == "" {
		return nil
	}
	_, err := io.WriteString(w, s)
	return err
}

func partialMarkerPrefixLen(s string, markers []string) int {
	maxLen := 0
	for _, marker := range markers {
		limit := len(marker) - 1
		if limit > len(s) {
			limit = len(s)
		}
		for n := limit; n > 0; n-- {
			if strings.HasSuffix(s, marker[:n]) {
				if n > maxLen {
					maxLen = n
				}
				break
			}
		}
	}
	return maxLen
}

func streamSplitThinkingChunk(tok string, pending *string, inThink *bool, visibleW io.Writer, thinkW io.Writer) error {
	const (
		startXML = "<think>"
		endXML   = "</think>"
		startBB  = "[THINK]"
		endBB    = "[/THINK]"
	)
	startMarkers := []string{startXML, startBB}
	endMarkers := []string{endXML, endBB}

	*pending += tok
	for len(*pending) > 0 {
		if !*inThink {
			nextIdx := -1
			nextMarker := ""
			for _, marker := range startMarkers {
				if idx := strings.Index(*pending, marker); idx != -1 && (nextIdx == -1 || idx < nextIdx) {
					nextIdx = idx
					nextMarker = marker
				}
			}
			if nextIdx == -1 {
				keep := partialMarkerPrefixLen(*pending, startMarkers)
				emitLen := len(*pending) - keep
				if emitLen > 0 {
					if err := writeStreamChunk(visibleW, (*pending)[:emitLen]); err != nil {
						return err
					}
					*pending = (*pending)[emitLen:]
				}
				break
			}
			if nextIdx > 0 {
				if err := writeStreamChunk(visibleW, (*pending)[:nextIdx]); err != nil {
					return err
				}
			}
			*pending = (*pending)[nextIdx+len(nextMarker):]
			*inThink = true
			continue
		}

		nextIdx := -1
		nextMarker := ""
		for _, marker := range endMarkers {
			if idx := strings.Index(*pending, marker); idx != -1 && (nextIdx == -1 || idx < nextIdx) {
				nextIdx = idx
				nextMarker = marker
			}
		}
		if nextIdx == -1 {
			keep := partialMarkerPrefixLen(*pending, endMarkers)
			emitLen := len(*pending) - keep
			if emitLen > 0 {
				if err := writeStreamChunk(thinkW, (*pending)[:emitLen]); err != nil {
					return err
				}
				*pending = (*pending)[emitLen:]
			}
			break
		}
		if nextIdx > 0 {
			if err := writeStreamChunk(thinkW, (*pending)[:nextIdx]); err != nil {
				return err
			}
		}
		*pending = (*pending)[nextIdx+len(nextMarker):]
		*inThink = false
	}
	return nil
}

// chatStream streams tokens from the chat completion endpoint and
// writes them to `w` as they arrive.
func (c *lmClient) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	return c.chatStreamDetailed(ctx, system, msgs, w, nil)
}

func (c *lmClient) chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error {
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
	if c.apiKey != "" && strings.Contains(c.base, "api.openai.com") {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
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

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var pending string
	inThink := false

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
			if tok != "" {
				if err := streamSplitThinkingChunk(tok, &pending, &inThink, w, thinkW); err != nil {
					return err
				}
			}
		}
	}
	if pending != "" {
		if inThink {
			if err := writeStreamChunk(thinkW, pending); err != nil {
				return err
			}
		} else {
			if err := writeStreamChunk(w, pending); err != nil {
				return err
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
	lm   lmProvider

	// DB mutex (tinySQL isn't designed for heavy concurrent writes)
	dbMu sync.Mutex

	// Monotonic chunk IDs (avoid collisions even after deletes)
	idMu   sync.Mutex
	nextID int
}

// newRAG initializes a new `ragSystem` backed by a tinySQL DB using
// the provided storage mode and memory constraints.
func newRAG(lm lmProvider, k int, dbPath string, storageMode tinysql.StorageMode, maxMemMB int64) (*ragSystem, error) {
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
func (r *ragSystem) setLM(lm lmProvider) {
	r.lmMu.Lock()
	defer r.lmMu.Unlock()
	r.lm = lm
}

// getLM returns the currently configured `lmClient`.
func (r *ragSystem) getLM() lmProvider {
	r.lmMu.RLock()
	defer r.lmMu.RUnlock()
	return r.lm
}

// getActiveEmbedModel returns the currently configured embedding model name
// used for newly created chunks or for retrieval filtering.
func (r *ragSystem) getActiveEmbedModel() string {
	s := settings.get()
	if s.EmbedModel != "" {
		return s.EmbedModel
	}
	return ""
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
	q := "CREATE TABLE IF NOT EXISTS chunks (id INT, article TEXT, chunk_idx INT, content TEXT, embedding VECTOR, embed_model TEXT, role_scope TEXT)"
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
	// Attempt to add embed_model column for older DBs (ignore errors)
	if alterStmt, err := tinysql.ParseSQL("ALTER TABLE chunks ADD COLUMN embed_model TEXT"); err == nil {
		_, _ = tinysql.Execute(context.Background(), r.db, "default", alterStmt)
	}
	// Attempt to add role_scope column for role-scoped visibility (ignore errors)
	if alterStmt, err := tinysql.ParseSQL("ALTER TABLE chunks ADD COLUMN role_scope TEXT"); err == nil {
		_, _ = tinysql.Execute(context.Background(), r.db, "default", alterStmt)
	}
	// Normalize older rows without a scope to global visibility.
	if updStmt, err := tinysql.ParseSQL("UPDATE chunks SET role_scope='|all|' WHERE role_scope IS NULL OR role_scope = ''"); err == nil {
		_, _ = tinysql.Execute(context.Background(), r.db, "default", updStmt)
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
func (r *ragSystem) addChunks(article string, chunks []string, embedModel string) error {
	return r.addChunksWithRoles(article, chunks, embedModel, nil)
}

// addChunksWithRoles stores chunks with an explicit role visibility scope.
// If roles are omitted, the current active role is used.
func (r *ragSystem) addChunksWithRoles(article string, chunks []string, embedModel string, roles []string) error {
	if len(chunks) == 0 {
		return nil
	}
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	normRoles := normalizeRoleScopes(roles, activeRole)
	roleScope := serializeRoleScope(normRoles)
	// If this article already exists in the DB, skip adding again to avoid duplicates.
	// This makes imports idempotent; to replace content delete the source first.
	checkQ := fmt.Sprintf(
		"SELECT COUNT(*) AS cnt FROM chunks WHERE article = '%s' AND role_scope = '%s'",
		escapeSQ(article), escapeSQ(roleScope),
	)
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

		// Bulk insert: construct a single multi-row INSERT statement for the batch
		var vals []string
		for j, v := range vecs {
			idx := i + j
			tup := fmt.Sprintf("(%d, '%s', %d, '%s', VEC_FROM_JSON('%s'), '%s', '%s')",
				startID+j, escapeSQ(article), idx, escapeSQ(batch[j]), vecJSON(v), escapeSQ(embedModel), escapeSQ(roleScope))
			vals = append(vals, tup)
		}
		q := "INSERT INTO chunks VALUES " + strings.Join(vals, ",")
		stmt, err := tinysql.ParseSQL(q)
		if err != nil {
			return fmt.Errorf("parse bulk insert: %w", err)
		}
		r.dbMu.Lock()
		if _, err := tinysql.Execute(context.Background(), r.db, "default", stmt); err != nil {
			r.dbMu.Unlock()
			return fmt.Errorf("exec bulk insert: %w", err)
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

func (r *ragSystem) docCountForRole(role string) int {
	normRole := normalizeDemoRole(role)
	q := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM chunks WHERE %s", roleScopeFilterSQL(normRole))
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

type retrievalHit struct {
	Article  string
	ChunkIdx int
	Content  string
	Score    float64
}

type chunkKey struct {
	article  string
	chunkIdx int
}

// searchJSON performs an embedding-based vector search for `query`,
// returning up to `k` primary hits along with neighbor chunks.
func (r *ragSystem) searchJSON(query string, k int) ([]searchResult, error) {
	candidates, _, _, err := r.searchCandidates(query, k)
	if err != nil {
		return nil, err
	}

	results := make([]searchResult, 0, k*3)
	seen := make(map[chunkKey]bool)
	primaryCount := 0
	for _, h := range candidates {
		if primaryCount >= k {
			break
		}
		if h.Score <= 0.6 {
			// skip low-score primary candidates
			continue
		}
		key := chunkKey{article: h.Article, chunkIdx: h.ChunkIdx}
		if seen[key] {
			continue
		}
		// add previous neighbor if exists and not seen
		if h.ChunkIdx > 0 {
			pkey := chunkKey{article: h.Article, chunkIdx: h.ChunkIdx - 1}
			if !seen[pkey] {
				if prevContent, ok := r.fetchNeighborContent(h.Article, h.ChunkIdx-1); ok {
					results = append(results, searchResult{Score: -1, Content: prevContent})
					seen[pkey] = true
				}
			}
		}

		// add primary hit
		results = append(results, searchResult{Score: h.Score, Content: h.Content})
		seen[key] = true
		primaryCount++

		// add next neighbor
		nkey := chunkKey{article: h.Article, chunkIdx: h.ChunkIdx + 1}
		if !seen[nkey] {
			if nextContent, ok := r.fetchNeighborContent(h.Article, h.ChunkIdx+1); ok {
				results = append(results, searchResult{Score: -1, Content: nextContent})
				seen[nkey] = true
			}
		}
	}

	return results, nil
}

func candidateLimitForK(k int) int {
	limit := 100
	if k*3 > limit {
		limit = k * 3
	}
	const maxLimit = 1000
	if limit > maxLimit {
		limit = maxLimit
	}
	return limit
}

func (r *ragSystem) searchCandidatesSingle(query string, k int) ([]retrievalHit, int64, int64, error) {
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	t0 := time.Now()
	qvec, err := r.getLM().embedSingle(query)
	if err != nil {
		return nil, 0, 0, err
	}
	embedMs := time.Since(t0).Milliseconds()

	q := fmt.Sprintf(
		"SELECT content, article, chunk_idx, VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON('%s')) AS score FROM chunks WHERE embed_model = '%s' AND %s ORDER BY score DESC LIMIT %d",
		vecJSON(qvec), escapeSQ(r.getActiveEmbedModel()), roleScopeFilterSQL(activeRole), candidateLimitForK(k),
	)

	t1 := time.Now()
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil, embedMs, 0, err
	}
	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
	if err != nil {
		return nil, embedMs, 0, err
	}
	searchMs := time.Since(t1).Milliseconds()

	hits := make([]retrievalHit, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		c, ok := tinysql.GetVal(row, "content")
		art, _ := tinysql.GetVal(row, "article")
		idxVal, _ := tinysql.GetVal(row, "chunk_idx")
		scoreVal, _ := tinysql.GetVal(row, "score")
		if !ok || art == nil || idxVal == nil || scoreVal == nil {
			continue
		}
		idx := 0
		switch iv := idxVal.(type) {
		case int:
			idx = iv
		case int64:
			idx = int(iv)
		case float64:
			idx = int(iv)
		}
		score := 0.0
		switch sv := scoreVal.(type) {
		case float64:
			score = sv
		case int:
			score = float64(sv)
		case int64:
			score = float64(sv)
		}
		hits = append(hits, retrievalHit{
			Article:  fmt.Sprint(art),
			ChunkIdx: idx,
			Content:  fmt.Sprint(c),
			Score:    score,
		})
	}
	return hits, embedMs, searchMs, nil
}

func (r *ragSystem) searchCandidates(query string, k int) ([]retrievalHit, int64, int64, error) {
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	variants := expandRetrievalQueries(query)
	if len(variants) <= 1 {
		return r.searchCandidatesSingle(query, k)
	}

	texts := make([]string, 0, len(variants))
	for _, variant := range variants {
		texts = append(texts, variant.Query)
	}
	t0 := time.Now()
	vecs, err := r.getLM().embed(texts)
	if err != nil {
		return nil, 0, 0, err
	}
	embedMs := time.Since(t0).Milliseconds()

	type aggKey struct {
		article  string
		chunkIdx int
	}
	best := map[aggKey]retrievalHit{}
	var totalSearchMs int64

	for i, vec := range vecs {
		if i >= len(variants) {
			break
		}
		q := fmt.Sprintf(
			"SELECT content, article, chunk_idx, VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON('%s')) AS score FROM chunks WHERE embed_model = '%s' AND %s ORDER BY score DESC LIMIT %d",
			vecJSON(vec), escapeSQ(r.getActiveEmbedModel()), roleScopeFilterSQL(activeRole), candidateLimitForK(k),
		)
		t1 := time.Now()
		stmt, err := tinysql.ParseSQL(q)
		if err != nil {
			return nil, embedMs, totalSearchMs, err
		}
		r.dbMu.Lock()
		rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
		r.dbMu.Unlock()
		if err != nil {
			return nil, embedMs, totalSearchMs, err
		}
		totalSearchMs += time.Since(t1).Milliseconds()
		for _, row := range rs.Rows {
			c, ok := tinysql.GetVal(row, "content")
			art, _ := tinysql.GetVal(row, "article")
			idxVal, _ := tinysql.GetVal(row, "chunk_idx")
			scoreVal, _ := tinysql.GetVal(row, "score")
			if !ok || art == nil || idxVal == nil || scoreVal == nil {
				continue
			}
			idx := 0
			switch iv := idxVal.(type) {
			case int:
				idx = iv
			case int64:
				idx = int(iv)
			case float64:
				idx = int(iv)
			}
			score := 0.0
			switch sv := scoreVal.(type) {
			case float64:
				score = sv
			case int:
				score = float64(sv)
			case int64:
				score = float64(sv)
			}
			hit := retrievalHit{
				Article:  fmt.Sprint(art),
				ChunkIdx: idx,
				Content:  fmt.Sprint(c),
				Score:    score,
			}
			hit.Score *= variants[i].Weight
			key := aggKey{article: hit.Article, chunkIdx: hit.ChunkIdx}
			if prev, ok := best[key]; !ok || hit.Score > prev.Score {
				best[key] = hit
			}
		}
	}

	hits := make([]retrievalHit, 0, len(best))
	for _, hit := range best {
		hits = append(hits, hit)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			if hits[i].Article == hits[j].Article {
				return hits[i].ChunkIdx < hits[j].ChunkIdx
			}
			return hits[i].Article < hits[j].Article
		}
		return hits[i].Score > hits[j].Score
	})
	return hits, embedMs, totalSearchMs, nil
}

func formatContextChunk(article string, chunkIdx int, content string) string {
	article = strings.TrimSpace(article)
	if article == "" {
		article = "unknown"
	}
	return fmt.Sprintf("[Quelle: %s | Chunk: %d]\n%s", article, chunkIdx, strings.TrimSpace(content))
}

func (r *ragSystem) loadArticleContext(article string, debug bool, embedMs int64) (string, *debugInfo, bool) {
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	q := fmt.Sprintf("SELECT article, chunk_idx, content FROM chunks WHERE LOWER(article) = LOWER('%s') AND %s ORDER BY chunk_idx", escapeSQ(article), roleScopeFilterSQL(activeRole))
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return "", nil, false
	}
	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return "", nil, false
	}

	var parts []string
	var dbgChunks []debugChunk
	resolvedArticle := article
	for _, row := range rs.Rows {
		c, ok := tinysql.GetVal(row, "content")
		if !ok {
			continue
		}
		if artVal, ok := tinysql.GetVal(row, "article"); ok && fmt.Sprint(artVal) != "" {
			resolvedArticle = fmt.Sprint(artVal)
		}
		idx := 0
		if idxVal, ok := tinysql.GetVal(row, "chunk_idx"); ok {
			switch iv := idxVal.(type) {
			case int:
				idx = iv
			case int64:
				idx = int(iv)
			case float64:
				idx = int(iv)
			}
		}
		content := fmt.Sprint(c)
		parts = append(parts, formatContextChunk(resolvedArticle, idx, content))
		if debug {
			dbgChunks = append(dbgChunks, debugChunk{Score: -1, Content: content, Article: resolvedArticle, ChunkIdx: idx, IsNeighbor: false})
		}
	}
	di := &debugInfo{Chunks: dbgChunks, EmbedMs: embedMs, SearchMs: 0, TotalChunks: r.docCountForRole(activeRole), UsedK: r.k, Decision: "article_specific"}
	return strings.Join(parts, "\n---\n"), di, true
}

func (r *ragSystem) assembleContext(hits []retrievalHit, usedK int, decision string, embedMs, searchMs int64) (string, *debugInfo, error) {
	seen := make(map[chunkKey]bool)
	var contextParts []string
	var dbgChunks []debugChunk

	appendChunk := func(article string, idx int, content string, score float64, isNeighbor bool) {
		key := chunkKey{article: article, chunkIdx: idx}
		if seen[key] {
			return
		}
		seen[key] = true
		contextParts = append(contextParts, formatContextChunk(article, idx, content))
		dbgChunks = append(dbgChunks, debugChunk{
			Score:      score,
			Content:    content,
			Article:    article,
			ChunkIdx:   idx,
			IsNeighbor: isNeighbor,
		})
	}

	for _, h := range hits {
		if h.ChunkIdx > 0 {
			if prevContent, ok := r.fetchNeighborContent(h.Article, h.ChunkIdx-1); ok {
				appendChunk(h.Article, h.ChunkIdx-1, prevContent, -1, true)
			}
		}
		appendChunk(h.Article, h.ChunkIdx, h.Content, h.Score, false)
		if nextContent, ok := r.fetchNeighborContent(h.Article, h.ChunkIdx+1); ok {
			appendChunk(h.Article, h.ChunkIdx+1, nextContent, -1, true)
		}
	}

	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	di := &debugInfo{Chunks: dbgChunks, EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCountForRole(activeRole), UsedK: usedK, Decision: decision}
	return strings.Join(contextParts, "\n---\n"), di, nil
}

// ── Tool / API definitions ─────────────────────────────────────────

// toolDef describes a built-in or custom tool available to the assistant.
type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ParamHint   string `json:"param_hint"`
}

// builtinTools defines the available built-in tools for the LLM assistant.
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
		Description: "Sucht relevante StackOverflow-Fragen und Antworten über die StackExchange-API (gut für Programmierfragen).",
		ParamHint:   "Suchbegriff (z.B. 'go http client timeout')",
	},
	{
		Name:        "websearch",
		Description: "Allgemeine Websuche mit Query-Varianten. Nutzt mehrere Formulierungen und kombiniert DuckDuckGo, Wikidata sowie bei technischen Themen GitHub und StackOverflow.",
		ParamHint:   "Suchbegriff (z.B. 'Wetter Berlin heute')",
	},
	{
		Name:        "news",
		Description: "Sucht nach aktuellen Nachrichten zu einem Thema.",
		ParamHint:   "Thema (z.B. 'Künstliche Intelligenz')",
	},
	{
		Name:        "wikidata",
		Description: "Sucht strukturierte Entitäten und Beschreibungen in Wikidata. Gut für Produkte, Firmen, Standards, technische Begriffe oder bekannte Objekte.",
		ParamHint:   "Entität oder Suchbegriff",
	},
	{
		Name:        "github",
		Description: "Sucht öffentliche GitHub-Repositories. Gut für Libraries, SDKs, Implementierungen und technische Referenzen.",
		ParamHint:   "Repository-, Projekt- oder Library-Suchbegriff",
	},
	{
		Name:        "nanogo",
		Description: "Führt sicheren, interpretierten Go-Code (nanoGo) aus. Verwende dies für Logik, Datenverarbeitung oder wenn du Berechnungen brauchst, die über einfache Arithmetik hinausgehen.",
		ParamHint:   "Go-Quelltext (kurze Snippets)",
	},
	{
		Name:        "calculate",
		Description: "Führt eine sichere Berechnung aus (arithmetische Ausdrücke). Nutzt smallR für schnelle Evaluation.",
		ParamHint:   "Expression (z.B. '3*2+(2^3)')",
	},
	{
		Name:        "shell",
		Description: "Führt häufig verwendete Shell-Befehle auf dem Server aus (z.B. 'ls', 'cat', 'curl'). Muss explizit in den Einstellungen aktiviert werden. WARNUNG: Sicherheitsrisiko!",
		ParamHint:   "Shell-Befehl (z.B. 'ls -la')",
	},
	{
		Name:        "local_search",
		Description: "Durchsucht die eigene lokale Wissensbasis (RAG-Datenbank) nach zusätzlichen Informationen. Nutze dies für interaktive Suchen in deinen Daten.",
		ParamHint:   "Suchbegriff für die Vektorsuche",
	},
	{
		Name:        "datetime",
		Description: "Gibt das aktuelle System-Datum und die Uhrzeit zurück. Gut für zeitliche Einordnungen.",
		ParamHint:   "Leer lassen oder 'now'",
	},
	{
		Name:        "tinygo",
		Description: "Alias für nanogo - interpretiert Go-Code direkt ohne Kompilierung. Sichere Sandbox-Umgebung für Go-Programme.",
		ParamHint:   "Go-Quelltext (z.B. 'package main; func main() { ... }')",
	},
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

func shouldAutoExecuteTool(s appSettings, tr toolRequest, autoSearch bool) bool {
	if !canRoleUseTool(s.ActiveRole, tr.Tool) {
		return false
	}
	if s.UsageProfile == "commercial" {
		switch tr.Tool {
		case "nanogo", "exec_code", "shell", "tinygo":
			return false
		}
	}
	switch tr.Tool {
	case "calculate", "local_search", "datetime":
		return true
	case "nanogo", "exec_code":
		return s.AllowNanoGo || s.AllowCodeExec
	case "shell":
		return s.AllowShellExec
	case "tinygo":
		return s.AllowTinyGo
	case "wikipedia", "duckduckgo", "wiktionary", "stackoverflow", "websearch", "news", "wikidata", "github":
		return autoSearch
	default:
		if strings.HasPrefix(tr.Tool, "module:") {
			return autoSearch
		}
		return autoSearch
	}
}

func executeToolRequest(tr toolRequest, s appSettings, rag *ragSystem, customAPIs *apiStore, modules *moduleStore) (string, string, error) {
	var text string
	var source string
	var fetchErr error

	switch tr.Tool {
	case "local_search":
		hits, err := rag.searchJSON(tr.Query, s.K)
		if err != nil {
			fetchErr = err
		} else if len(hits) == 0 {
			text = "Keine passenden lokalen Dokumente gefunden."
			source = "rag_local:" + tr.Query
		} else {
			var sb strings.Builder
			for i, h := range hits {
				sb.WriteString(fmt.Sprintf("Treffer %d (Score %.2f):\n%s\n\n", i+1, h.Score, h.Content))
			}
			text = sb.String()
			source = "rag_local:" + tr.Query
		}
	case "datetime":
		text = fmt.Sprintf("Aktuelles System-Datum und Uhrzeit: %s", time.Now().Format("2006-01-02 15:04:05 MST"))
		source = "system:datetime"
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
		text, fetchErr = fetchStackOverflow(tr.Query)
	case "websearch":
		source = "web:" + tr.Query
		text, fetchErr = fetchMultiWebSearch(tr.Query)
	case "news":
		source = "news:" + tr.Query
		text, fetchErr = fetchDuckDuckGo(`news "` + tr.Query + `"`)
	case "wikidata":
		source = "wikidata:" + tr.Query
		text, fetchErr = fetchWikidata(tr.Query)
	case "github":
		source = "github:" + tr.Query
		text, fetchErr = fetchGitHub(tr.Query)
	case "llm":
		var buf bytes.Buffer
		msgs := []chatMsg{{Role: "user", Content: tr.Query}}
		if err := rag.getLM().chatStream(context.Background(), "", msgs, &buf); err != nil {
			return "", "", fmt.Errorf("LLM error: %w", err)
		}
		text = buf.String()
		source = "llm:prompt"
	case "calculate":
		out, err := execSmallR(tr.Query)
		if err != nil {
			fetchErr = err
		} else {
			text = out
			source = "calc:" + tr.Query
		}
	case "nanogo", "exec_code":
		timeout := 5 * time.Second
		out, err := RunSafe(tr.Query, timeout)
		if err != nil {
			fetchErr = err
		} else {
			text = out
			if tr.Tool == "exec_code" {
				source = "code:exec"
			} else {
				source = "nanogo:exec"
			}
		}
	case "shell":
		if !s.AllowShellExec {
			fetchErr = fmt.Errorf("shell execution disabled in settings")
		} else {
			text, fetchErr = execShellCommand(tr.Query)
			if fetchErr == nil {
				source = "shell:exec"
			}
		}
	case "tinygo":
		if !s.AllowTinyGo {
			fetchErr = fmt.Errorf("tinygo execution disabled in settings")
		} else {
			text, fetchErr = execTinyGoProgram(tr.Query)
			if fetchErr == nil {
				source = "tinygo:exec"
			}
		}
	default:
		if strings.HasPrefix(tr.Tool, "module:") && modules != nil {
			modID := strings.TrimPrefix(tr.Tool, "module:")
			mod, ok := modules.get(modID)
			if !ok {
				fetchErr = fmt.Errorf("unknown module: %s", modID)
				break
			}
			if !mod.Enabled {
				fetchErr = fmt.Errorf("module %s is disabled", modID)
				break
			}
			action := "query"
			limit := 0
			arg := tr.Query
			if mod.Kind == "mail" {
				action = "query"
				limit = parseIntString(tr.Query, 0)
				arg = ""
			} else if mod.Kind == "http-folder" {
				action = "ingest"
			}
			res, err := executeModuleRun(mod, rag, settings.get().EmbedModel, action, arg, limit, true)
			if err != nil {
				fetchErr = err
				break
			}
			text = res.Text
			source = res.Source
			if source == "" {
				source = "module:" + mod.ID
			}
			break
		}
		if api, ok := customAPIs.get(tr.Tool); ok {
			finalURL := strings.ReplaceAll(api.Template, "$q", url.QueryEscape(tr.Query))
			source = "api:" + api.Name + ":" + tr.Query
			text, fetchErr = fetchURL(finalURL)
		} else {
			fetchErr = fmt.Errorf("unknown tool: %s", tr.Tool)
		}
	}

	return text, source, fetchErr
}

func filterToolsForRole(tools []toolDef, role string) []toolDef {
	out := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		if canRoleUseTool(role, t.Name) {
			out = append(out, t)
		}
	}
	return out
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

type adminUserStore struct {
	settings *settingsStore
}

func newAdminUserStore(settings *settingsStore) *adminUserStore {
	return &adminUserStore{settings: settings}
}

func sanitizeAdminUser(user adminAPIUser) adminAPIUser {
	user.APIKeyHash = ""
	return user
}

func (s *adminUserStore) list() []adminAPIUser {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	out := make([]adminAPIUser, 0, len(s.settings.s.APIUsers))
	for _, user := range s.settings.s.APIUsers {
		out = append(out, sanitizeAdminUser(user))
	}
	return out
}

func (s *adminUserStore) create(name, role string) (adminAPIUser, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return adminAPIUser{}, "", fmt.Errorf("name required")
	}
	token, err := generateAPIToken()
	if err != nil {
		return adminAPIUser{}, "", err
	}
	user := adminAPIUser{
		ID:          fmt.Sprintf("api-user-%d", time.Now().UnixNano()),
		Name:        name,
		Role:        normalizeDemoRole(role),
		Enabled:     true,
		APIKeyHash:  hashAPIToken(token),
		APIKeyLast4: token[max(len(token)-4, 0):],
		CreatedAt:   time.Now().Format(time.RFC3339),
	}
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	s.settings.s.APIUsers = append(s.settings.s.APIUsers, user)
	if err := s.settings.saveLocked(); err != nil {
		return adminAPIUser{}, "", err
	}
	return sanitizeAdminUser(user), token, nil
}

func (s *adminUserStore) update(id, name, role string, enabled bool) (adminAPIUser, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for i, user := range s.settings.s.APIUsers {
		if user.ID != id {
			continue
		}
		if strings.TrimSpace(name) != "" {
			user.Name = strings.TrimSpace(name)
		}
		user.Role = normalizeDemoRole(role)
		user.Enabled = enabled
		s.settings.s.APIUsers[i] = user
		if err := s.settings.saveLocked(); err != nil {
			return adminAPIUser{}, err
		}
		return sanitizeAdminUser(user), nil
	}
	return adminAPIUser{}, fmt.Errorf("user not found")
}

func (s *adminUserStore) regenerateKey(id string) (adminAPIUser, string, error) {
	token, err := generateAPIToken()
	if err != nil {
		return adminAPIUser{}, "", err
	}
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for i, user := range s.settings.s.APIUsers {
		if user.ID != id {
			continue
		}
		user.APIKeyHash = hashAPIToken(token)
		user.APIKeyLast4 = token[max(len(token)-4, 0):]
		s.settings.s.APIUsers[i] = user
		if err := s.settings.saveLocked(); err != nil {
			return adminAPIUser{}, "", err
		}
		return sanitizeAdminUser(user), token, nil
	}
	return adminAPIUser{}, "", fmt.Errorf("user not found")
}

func (s *adminUserStore) remove(id string) (bool, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	list := s.settings.s.APIUsers
	for i, user := range list {
		if user.ID == id {
			s.settings.s.APIUsers = append(list[:i], list[i+1:]...)
			return true, s.settings.saveLocked()
		}
	}
	return false, nil
}

type apiRouteStore struct {
	settings *settingsStore
}

func newAPIRouteStore(settings *settingsStore) *apiRouteStore {
	return &apiRouteStore{settings: settings}
}

func (s *apiRouteStore) list() []apiRouteRule {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	out := make([]apiRouteRule, len(s.settings.s.APIRoutes))
	copy(out, s.settings.s.APIRoutes)
	return out
}

func (s *apiRouteStore) update(path string, enabled, public bool) (apiRouteRule, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for i, rule := range s.settings.s.APIRoutes {
		if rule.Path != path {
			continue
		}
		rule.Enabled = enabled
		rule.Public = public
		s.settings.s.APIRoutes[i] = rule
		if err := s.settings.saveLocked(); err != nil {
			return apiRouteRule{}, err
		}
		return rule, nil
	}
	return apiRouteRule{}, fmt.Errorf("route not found")
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

func extractToolRequest(text string) (toolRequest, bool) {
	start := strings.Index(text, "[TOOL_REQUEST]")
	if start == -1 {
		return toolRequest{}, false
	}
	end := strings.Index(text[start:], "[/TOOL_REQUEST]")
	var body string
	if end == -1 {
		body = strings.TrimSpace(text[start+len("[TOOL_REQUEST]"):])
	} else {
		body = strings.TrimSpace(text[start+len("[TOOL_REQUEST]") : start+end])
	}
	var tr toolRequest
	if err := json.Unmarshal([]byte(body), &tr); err != nil {
		return toolRequest{}, false
	}
	if strings.TrimSpace(tr.Tool) == "" || strings.TrimSpace(tr.Query) == "" {
		return toolRequest{}, false
	}
	tr.Tool = strings.TrimSpace(tr.Tool)
	tr.Query = strings.TrimSpace(tr.Query)
	return tr, true
}

func stripToolRequest(text string) string {
	start := strings.Index(text, "[TOOL_REQUEST]")
	if start == -1 {
		return strings.TrimSpace(text)
	}
	end := strings.Index(text[start:], "[/TOOL_REQUEST]")
	if end == -1 {
		return strings.TrimSpace(text[:start])
	}
	end += start + len("[/TOOL_REQUEST]")
	return strings.TrimSpace(text[:start] + text[end:])
}

var (
	completeThinkBlockRE = regexp.MustCompile(`(?is)<think>.*?</think>|\[THINK\].*?\[/THINK\]`)
	openThinkTailRE      = regexp.MustCompile(`(?is)(<think>|\[THINK\]).*$`)
)

func stripInternalThinking(text string) string {
	cleaned := completeThinkBlockRE.ReplaceAllString(text, "")
	cleaned = openThinkTailRE.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func languageLabel(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "de":
		return "Deutsch"
	case "en":
		return "English"
	case "fr":
		return "Français"
	case "es":
		return "Español"
	case "it":
		return "Italiano"
	case "nl":
		return "Nederlands"
	case "pt":
		return "Português"
	case "pl":
		return "Polski"
	default:
		if code == "" {
			return "Deutsch"
		}
		return strings.ToLower(code)
	}
}

func buildAssistantPolicyPrompt(s appSettings) string {
	var sb strings.Builder
	sb.WriteString("Du bist ein praeziser RAG- und Research-Assistent fuer persoenliche und unternehmerische Nutzung.\n")
	sb.WriteString("Deine Prioritaeten sind: 1) Korrektheit, 2) klare Trennung von Fakten und Unsicherheit, 3) knappe, nuetzliche Antworten.\n")
	sb.WriteString(fmt.Sprintf("Aktive Rolle (Demo-RBAC): %s.\n", demoRoleLabel(s.ActiveRole)))
	switch s.ResponseLanguageMode {
	case "settings":
		sb.WriteString(fmt.Sprintf("Antworte durchgaengig auf %s.\n", languageLabel(s.Lang)))
	default:
		sb.WriteString(fmt.Sprintf("Antworte in der Sprache der Nutzeranfrage. Falls unklar, nutze %s.\n", languageLabel(s.Lang)))
	}
	if s.UsageProfile == "commercial" {
		sb.WriteString("Kontext: gewerbliche Nutzung in einem europaweiten Unternehmen. Priorisiere Nachvollziehbarkeit, neutrale Sprache und risikoarme Aussagen.\n")
	}
	sb.WriteString("Erfinde nichts. Wenn Informationen fehlen oder duenn belegt sind, sage das klar.\n")
	sb.WriteString("Vermeide Marketing-Sprache, Wiederholungen, Halluzinationen und unnoetige Ausschmueckungen.\n")
	sb.WriteString("Nutze interne Denkschritte nur implizit und zeige sie nicht.\n\n")
	return sb.String()
}

func buildContextPrompt(ctxText string) string {
	var sb strings.Builder
	if ctxText != "" {
		sb.WriteString("### RAG-Kontext\n")
		sb.WriteString("Hier sind relevante Informationen aus der lokalen Wissensbasis:\n")
		sb.WriteString(ctxText)
		sb.WriteString("\n\n")
		sb.WriteString("Behandle diesen Kontext als primaere Quelle. Wenn er nicht ausreicht oder zeitlich fraglich ist, nutze ein Tool.\n\n")
	} else {
		sb.WriteString("Es liegt kein hinreichender lokaler Kontext fuer diese Anfrage vor. Nutze bei Bedarf Tools.\n\n")
	}
	return sb.String()
}

func buildToolingPrompt(tools []toolDef) string {
	var sb strings.Builder
	sb.WriteString("### Tool-Nutzung\n")
	sb.WriteString("Wenn externe Informationen, Berechnungen oder Codeausfuehrung noetig sind, fordere genau ein Tool an.\n")
	sb.WriteString("Der Tool-Request muss die LETZTE Zeile deiner Antwort sein, exakt in diesem Format und in genau einer Zeile:\n\n")
	sb.WriteString("[TOOL_REQUEST]{\"tool\":\"websearch\",\"query\":\"Query\"}[/TOOL_REQUEST]\n\n")
	sb.WriteString("Ersetze nur `tool` und `query`. Keine Backticks, kein Markdown-Block, keine zweite JSON-Struktur, keine Zusatzzeichen.\n")
	sb.WriteString("Wenn kein Tool noetig ist, schreibe keinen Tool-Request.\n\n")

	sb.WriteString("### Verfügbare Tools:\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- **%s**: %s (Parameter: %s)\n", t.Name, t.Description, t.ParamHint))
	}
	sb.WriteString("\n")
	return sb.String()
}

func buildResponseInstructionsPrompt(deep bool, usageProfile string) string {
	var sb strings.Builder
	sb.WriteString("\n### Instruktionen:\n")
	sb.WriteString("- Beginne mit der Frage: Reicht der lokale Kontext aus, ist er unsicher oder fehlt er?\n")
	sb.WriteString("- Wenn der lokale Kontext ausreicht, antworte direkt und sage knapp, dass die Antwort auf der Wissensbasis beruht.\n")
	sb.WriteString("- Wenn Informationen fehlen oder potenziell veraltet sind, nutze genau ein passendes Tool.\n")
	sb.WriteString("- Fuer allgemeine externe Recherche bevorzuge `websearch`; fuer aktuelle Ereignisse `news`; fuer strukturierte Entitaeten `wikidata`; fuer Code- oder Library-Themen `github` und `stackoverflow`; fuer Rechenlogik `calculate` oder `nanogo`.\n")
	sb.WriteString("- Bei technischen Artikeln, Produktcodes oder Teilenummern darfst du praezisere Suchanfragen bilden, z.B. Teilstrings, exakte Phrasen mit Anfuehrungszeichen oder Varianten wie `Technische Details <Begriff>`.\n")
	sb.WriteString("- Behaupte nie mehr Sicherheit, als der Kontext hergibt.\n")
	sb.WriteString("- Erfinde keine Kontaktinformationen, URLs, APIs, Produktdetails, Roadmaps oder technische Interna.\n")
	sb.WriteString("- Wenn ein Tool benutzt wurde, liefere danach genau eine ueberarbeitete finale Antwort, nicht zwei Versionen.\n")
	sb.WriteString("- Trenne lokale Wissensbasis und externe Recherche explizit, wenn beide verwendet wurden.\n")
	sb.WriteString("- Wenn externe Recherche wenig hergibt, sage das offen statt Luecken zu fuellen.\n")
	if usageProfile == "commercial" {
		sb.WriteString("- Wenn die Wissensbasis genutzt wurde, fuege am Ende den Abschnitt `Quellenbasis` mit den verwendeten `[Quelle: ...]`-Bezeichnern an.\n")
		sb.WriteString("- Bei fehlender Beleglage liefere eine vorsichtige Empfehlung statt einer harten Zusage.\n")
	}
	if deep {
		sb.WriteString("- Im Deep-Research-Modus strukturiere die Antwort in: Kurzfazit, Befunde, Unsicherheiten, Schlussfolgerung.\n")
		sb.WriteString("- Priorisiere Genauigkeit und Einordnung vor Vollstaendigkeit.\n")
	} else {
		sb.WriteString("- Standardmodus: antworte kompakt, konkret und mit hoher Informationsdichte.\n")
	}
	return sb.String()
}

// buildToolSystemPrompt constructs the system prompt describing
// available tools and how the assistant should emit tool requests.
func buildToolSystemPrompt(ctxText string, tools []toolDef, deep bool, s appSettings) string {
	return buildAssistantPolicyPrompt(s) +
		buildContextPrompt(ctxText) +
		buildToolingPrompt(tools) +
		buildResponseInstructionsPrompt(deep, s.UsageProfile)
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
	ActiveRole         string      `json:"active_role"`
	RoleLabel          string      `json:"role_label"`
	PersonaID          string      `json:"persona_id"`
	PersonaName        string      `json:"persona_name"`
	PersonaPromptChars int         `json:"persona_prompt_chars"`
}

// prepareContext does the embedding + vector search and returns the context string and optional debug info.
// prepareContext computes embeddings for `question`, runs a vector
// search against the DB and returns the assembled context text and
// optional debug information.
func (r *ragSystem) prepareContext(question string, debug bool) (string, *debugInfo, error) {
	searchQuery := refineSearchQuery(question)
	hits, embedMs, searchMs, err := r.searchCandidates(searchQuery, r.k)
	if err != nil {
		return "", nil, err
	}
	if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
		return ctx, di, nil
	}

	// If we have a clear high-confidence hit, return context immediately.
	const highThreshold = 0.90
	var primaryCount int
	for _, h := range hits {
		if h.Score > highThreshold {
			primaryCount++
		}
	}

	// If high-confidence primary found, use those hits (top k by score)
	if primaryCount > 0 {
		var sel []retrievalHit
		for _, h := range hits {
			if h.Score > highThreshold {
				sel = append(sel, h)
				if len(sel) >= r.k {
					break
				}
			}
		}
		return r.assembleContext(sel, r.k, "high_confidence", embedMs, searchMs)
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
		summaryParts = append(summaryParts, fmt.Sprintf("%s (score=%.4f)", h.Article, h.Score))
	}
	summary := strings.Join(summaryParts, "; ")

	// Ask LM whether to answer directly or retrieve more context.
	decisionMap, derr := r.analyzeQuestion(question, summary)
	if derr != nil {
		var sel []retrievalHit
		thresh := 0.60
		for _, h := range hits {
			if h.Score >= thresh {
				sel = append(sel, h)
				if len(sel) >= r.k {
					break
				}
			}
		}
		return r.assembleContext(sel, r.k, "relaxed_fallback", embedMs, searchMs)
	}

	action, _ := decisionMap["action"].(string)
	if strings.ToUpper(action) == "ANSWER_DIRECT" {
		// Let the chat model answer without extra context.
		activeRole := "it"
		if settings != nil {
			activeRole = settings.get().ActiveRole
		}
		di := &debugInfo{EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCountForRole(activeRole), UsedK: 0, Decision: "answer_direct"}
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

	if searchQuery != refineSearchQuery(question) {
		hits, _, searchMs, err = r.searchCandidates(searchQuery, desiredK)
		if err != nil {
			return "", nil, err
		}
		if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
			di.UsedK = desiredK
			di.Decision = "article_specific_refined"
			return ctx, di, nil
		}
	}

	var sel []retrievalHit
	for _, h := range hits {
		if h.Score >= thresh {
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
	return r.assembleContext(sel, desiredK, "lm_requested_retrieval", embedMs, searchMs)
}

// prepareContextWithK does the same as prepareContext but allows specifying k (number of primary hits)
// prepareContextWithK behaves like prepareContext but allows specifying
// the number `k` of primary retrieval hits to consider.
func (r *ragSystem) prepareContextWithK(question string, debug bool, k int) (string, *debugInfo, error) {
	searchQuery := refineSearchQuery(question)
	hits, embedMs, searchMs, err := r.searchCandidates(searchQuery, k)
	if err != nil {
		return "", nil, err
	}
	if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
		di.UsedK = k
		return ctx, di, nil
	}

	const highThreshold = 0.90
	var primaryCount int
	for _, h := range hits {
		if h.Score > highThreshold {
			primaryCount++
		}
	}

	if primaryCount > 0 {
		var sel []retrievalHit
		for _, h := range hits {
			if h.Score > highThreshold {
				sel = append(sel, h)
				if len(sel) >= k {
					break
				}
			}
		}
		return r.assembleContext(sel, k, "high_confidence", embedMs, searchMs)
	}

	// Summarize top candidates
	var summaryParts []string
	topN := 5
	if len(hits) < topN {
		topN = len(hits)
	}
	for i := 0; i < topN; i++ {
		h := hits[i]
		summaryParts = append(summaryParts, fmt.Sprintf("%s (score=%.4f)", h.Article, h.Score))
	}
	summary := strings.Join(summaryParts, "; ")

	decisionMap, derr := r.analyzeQuestion(question, summary)
	if derr != nil {
		var sel []retrievalHit
		thresh := 0.60
		for _, h := range hits {
			if h.Score >= thresh {
				sel = append(sel, h)
				if len(sel) >= k {
					break
				}
			}
		}
		return r.assembleContext(sel, k, "relaxed_fallback", embedMs, searchMs)
	}

	action, _ := decisionMap["action"].(string)
	if strings.ToUpper(action) == "ANSWER_DIRECT" {
		activeRole := "it"
		if settings != nil {
			activeRole = settings.get().ActiveRole
		}
		di := &debugInfo{EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCountForRole(activeRole), UsedK: 0, Decision: "answer_direct"}
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

	if searchQuery != refineSearchQuery(question) {
		hits, _, searchMs, err = r.searchCandidates(searchQuery, desiredK)
		if err != nil {
			return "", nil, err
		}
		if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
			di.UsedK = desiredK
			di.Decision = "article_specific_refined"
			return ctx, di, nil
		}
	}

	var sel []retrievalHit
	for _, h := range hits {
		if h.Score >= thresh {
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
	return r.assembleContext(sel, desiredK, "lm_requested_retrieval", embedMs, searchMs)
}

func (r *ragSystem) prepareDirectContext(query string, k int) (string, *debugInfo, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", &debugInfo{UsedK: 0, Decision: "no_query"}, nil
	}
	if k <= 0 {
		k = r.k
	}
	hits, embedMs, searchMs, err := r.searchCandidates(query, k)
	if err != nil {
		return "", nil, err
	}
	if ctx, di, ok := r.loadArticleContext(query, false, embedMs); ok {
		di.UsedK = k
		di.Decision = "article_specific_direct"
		return ctx, di, nil
	}
	var sel []retrievalHit
	for _, h := range hits {
		if h.Score >= 0.60 {
			sel = append(sel, h)
			if len(sel) >= k {
				break
			}
		}
	}
	if len(sel) == 0 && len(hits) > 0 {
		for i := 0; i < k && i < len(hits); i++ {
			sel = append(sel, hits[i])
		}
	}
	if len(sel) == 0 {
		activeRole := "it"
		if settings != nil {
			activeRole = settings.get().ActiveRole
		}
		return "", &debugInfo{
			EmbedMs:     embedMs,
			SearchMs:    searchMs,
			TotalChunks: r.docCountForRole(activeRole),
			UsedK:       k,
			Decision:    "no_hits",
		}, nil
	}
	return r.assembleContext(sel, k, "direct_query", embedMs, searchMs)
}

type processRAGOptions struct {
	Enabled bool   `json:"enabled"`
	Query   string `json:"query"`
	K       int    `json:"k"`
}

type processOptions struct {
	ValidateJSON *bool `json:"validate_json"`
	RepairJSON   *bool `json:"repair_json"`
	MaxRetries   int   `json:"max_retries"`
}

type processRequest struct {
	RequestID      string            `json:"request_id"`
	Mode           string            `json:"mode"`
	SystemPrompt   string            `json:"system_prompt"`
	PrePrompt      string            `json:"pre_prompt"`
	Input          any               `json:"input"`
	PostPrompt     string            `json:"post_prompt"`
	ResponseSchema map[string]any    `json:"response_schema"`
	PersonaID      string            `json:"persona_id"`
	RAG            processRAGOptions `json:"rag"`
	Options        processOptions    `json:"options"`
}

type processResponse struct {
	RequestID       string     `json:"request_id"`
	OK              bool       `json:"ok"`
	Mode            string     `json:"mode"`
	ValidJSON       bool       `json:"valid_json"`
	Attempts        int        `json:"attempts"`
	DurationMS      int64      `json:"duration_ms"`
	RAGUsed         bool       `json:"rag_used"`
	RAGQuery        string     `json:"rag_query,omitempty"`
	ContextChars    int        `json:"context_chars,omitempty"`
	Raw             string     `json:"raw,omitempty"`
	Result          any        `json:"result,omitempty"`
	Error           string     `json:"error,omitempty"`
	ValidationError string     `json:"validation_error,omitempty"`
	Retrieval       *debugInfo `json:"retrieval,omitempty"`
}

var fencedJSONBlockRE = regexp.MustCompile("(?is)```(?:json)?\\s*(.*?)\\s*```")

func boolOrDefault(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func normalizeProcessMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "rag":
		return "rag"
	default:
		return "direct"
	}
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func processQueryFromInput(v any) string {
	raw := strings.TrimSpace(compactJSON(v))
	if len(raw) > 1000 {
		raw = raw[:1000]
	}
	return raw
}

func firstNonSpaceIndex(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return i
		}
	}
	return -1
}

func extractBalancedJSON(s string) (string, error) {
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '{' || s[i] == '[' {
			start = i
			break
		}
	}
	if start == -1 {
		return "", fmt.Errorf("no JSON object or array found")
	}
	stack := make([]byte, 0, 8)
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, ch)
		case '}':
			if len(stack) == 0 || stack[len(stack)-1] != '{' {
				return "", fmt.Errorf("invalid JSON structure")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return s[start : i+1], nil
			}
		case ']':
			if len(stack) == 0 || stack[len(stack)-1] != '[' {
				return "", fmt.Errorf("invalid JSON structure")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unterminated JSON structure")
}

func extractFirstJSONValue(text string) (string, error) {
	candidates := []string{strings.TrimSpace(text)}
	matches := fencedJSONBlockRE.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if len(m) > 1 {
			candidates = append(candidates, strings.TrimSpace(m[1]))
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if idx := firstNonSpaceIndex(candidate); idx >= 0 {
			if candidate[idx] == '{' || candidate[idx] == '[' {
				var parsed any
				if err := json.Unmarshal([]byte(candidate[idx:]), &parsed); err == nil {
					return strings.TrimSpace(candidate[idx:]), nil
				}
			}
		}
		if out, err := extractBalancedJSON(candidate); err == nil {
			var parsed any
			if err := json.Unmarshal([]byte(out), &parsed); err == nil {
				return out, nil
			}
		}
	}
	return "", fmt.Errorf("no valid JSON found in model output")
}

func jsonTypeName(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if math.Trunc(v) == v {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func schemaTypeMatches(expected string, value any) bool {
	switch expected {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && math.Trunc(n) == n
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

func validateJSONAgainstSchema(value any, schema map[string]any, path string) error {
	if len(schema) == 0 {
		return nil
	}
	if path == "" {
		path = "$"
	}
	if enumRaw, ok := schema["enum"].([]any); ok && len(enumRaw) > 0 {
		matched := false
		for _, allowed := range enumRaw {
			if reflect.DeepEqual(allowed, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %v is not part of enum", path, value)
		}
	}
	if rawType, ok := schema["type"]; ok {
		switch tv := rawType.(type) {
		case string:
			if !schemaTypeMatches(tv, value) {
				return fmt.Errorf("%s: expected %s, got %s", path, tv, jsonTypeName(value))
			}
		case []any:
			matched := false
			for _, item := range tv {
				if ts, ok := item.(string); ok && schemaTypeMatches(ts, value) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%s: unexpected type %s", path, jsonTypeName(value))
			}
		}
	}

	switch actual := value.(type) {
	case map[string]any:
		if reqRaw, ok := schema["required"].([]any); ok {
			for _, item := range reqRaw {
				key, ok := item.(string)
				if !ok {
					continue
				}
				if _, exists := actual[key]; !exists {
					return fmt.Errorf("%s.%s: required property missing", path, key)
				}
			}
		}
		properties := map[string]any{}
		if raw, ok := schema["properties"].(map[string]any); ok {
			properties = raw
		}
		if rawAP, ok := schema["additionalProperties"].(bool); ok && !rawAP {
			for key := range actual {
				if _, exists := properties[key]; !exists {
					return fmt.Errorf("%s.%s: additional property not allowed", path, key)
				}
			}
		}
		for key, item := range actual {
			childSchema, ok := properties[key].(map[string]any)
			if !ok {
				continue
			}
			if err := validateJSONAgainstSchema(item, childSchema, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		if minItems, ok := schema["minItems"].(float64); ok && len(actual) < int(minItems) {
			return fmt.Errorf("%s: expected at least %d items", path, int(minItems))
		}
		if maxItems, ok := schema["maxItems"].(float64); ok && len(actual) > int(maxItems) {
			return fmt.Errorf("%s: expected at most %d items", path, int(maxItems))
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for i, item := range actual {
				if err := validateJSONAgainstSchema(item, itemSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case string:
		if minLength, ok := schema["minLength"].(float64); ok && len(actual) < int(minLength) {
			return fmt.Errorf("%s: expected minimum length %d", path, int(minLength))
		}
		if maxLength, ok := schema["maxLength"].(float64); ok && len(actual) > int(maxLength) {
			return fmt.Errorf("%s: expected maximum length %d", path, int(maxLength))
		}
	case float64:
		if minimum, ok := schema["minimum"].(float64); ok && actual < minimum {
			return fmt.Errorf("%s: expected minimum %v", path, minimum)
		}
		if maximum, ok := schema["maximum"].(float64); ok && actual > maximum {
			return fmt.Errorf("%s: expected maximum %v", path, maximum)
		}
	}
	return nil
}

func buildStructuredProcessSystemPrompt(s appSettings, personaPrompt, customSystemPrompt, ctxText string, schema map[string]any) string {
	var sb strings.Builder
	sb.WriteString(buildAssistantPolicyPrompt(s))
	sb.WriteString("Du arbeitest als strukturierte JSON-Schnittstelle fuer Backend-Prozesse.\n")
	sb.WriteString("Gib exakt einen JSON-Wert zurueck, der zum angeforderten Schema passt.\n")
	sb.WriteString("Keine Markdown-Codeblocks, keine Kommentare, keine Erklaerungen, keine Vor- oder Nachsaetze.\n")
	sb.WriteString("Wenn Kontext aus der lokalen Wissensbasis vorhanden ist, nutze ihn nur als Zusatzinformation und markiere keine internen Quellen im JSON.\n")
	if personaPrompt != "" {
		sb.WriteString("\n### Persona\n")
		sb.WriteString(personaPrompt)
		sb.WriteString("\n")
	}
	if customSystemPrompt != "" {
		sb.WriteString("\n### System Prompt\n")
		sb.WriteString(customSystemPrompt)
		sb.WriteString("\n")
	}
	if ctxText != "" {
		sb.WriteString("\n### Lokaler Kontext\n")
		sb.WriteString(ctxText)
		sb.WriteString("\n")
	}
	if len(schema) > 0 {
		sb.WriteString("\n### Antwortschema\n")
		sb.WriteString(prettyJSON(schema))
		sb.WriteString("\n")
	}
	return sb.String()
}

func buildStructuredProcessUserPrompt(req processRequest) string {
	var sb strings.Builder
	if strings.TrimSpace(req.PrePrompt) != "" {
		sb.WriteString(strings.TrimSpace(req.PrePrompt))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Input JSON:\n")
	sb.WriteString(prettyJSON(req.Input))
	sb.WriteString("\n\n")
	if len(req.ResponseSchema) > 0 {
		sb.WriteString("Erwartetes JSON-Schema:\n")
		sb.WriteString(prettyJSON(req.ResponseSchema))
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(req.PostPrompt) != "" {
		sb.WriteString(strings.TrimSpace(req.PostPrompt))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Antworte ausschliesslich mit validem JSON.")
	return sb.String()
}

func buildStructuredRepairPrompt(req processRequest, validationErr, raw string) string {
	var sb strings.Builder
	sb.WriteString("Die vorige Antwort war kein gueltiges JSON fuer das angeforderte Schema.\n\n")
	if validationErr != "" {
		sb.WriteString("Validierungsfehler:\n")
		sb.WriteString(validationErr)
		sb.WriteString("\n\n")
	}
	if len(req.ResponseSchema) > 0 {
		sb.WriteString("Schema:\n")
		sb.WriteString(prettyJSON(req.ResponseSchema))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Urspruengliche Modellantwort:\n")
	sb.WriteString(raw)
	sb.WriteString("\n\nGib jetzt nur die reparierte JSON-Antwort zurueck.")
	return sb.String()
}

func processModelOutput(raw string, schema map[string]any, validate bool) (string, any, string, error) {
	cleaned := stripInternalThinking(strings.TrimSpace(raw))
	jsonText, err := extractFirstJSONValue(cleaned)
	if err != nil {
		return "", nil, "", err
	}
	var parsed any
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return jsonText, nil, "", err
	}
	if validate && len(schema) > 0 {
		if err := validateJSONAgainstSchema(parsed, schema, "$"); err != nil {
			return jsonText, parsed, err.Error(), err
		}
	}
	return jsonText, parsed, "", nil
}

func runStructuredProcess(ctx context.Context, rag *ragSystem, s appSettings, personaPrompt string, req processRequest) processResponse {
	start := time.Now()
	resp := processResponse{
		RequestID: req.RequestID,
		Mode:      normalizeProcessMode(req.Mode),
	}
	validateJSON := boolOrDefault(req.Options.ValidateJSON, true)
	repairJSON := boolOrDefault(req.Options.RepairJSON, true)
	maxRetries := req.Options.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries == 0 && repairJSON {
		maxRetries = 2
	}
	var ctxText string
	var retrieval *debugInfo
	if req.RAG.Enabled || resp.Mode == "rag" {
		resp.RAGUsed = true
		resp.Mode = "rag"
		query := strings.TrimSpace(req.RAG.Query)
		if query == "" {
			query = processQueryFromInput(req.Input)
		}
		k := req.RAG.K
		if k <= 0 {
			k = rag.k
		}
		var err error
		ctxText, retrieval, err = rag.prepareDirectContext(query, k)
		if err != nil {
			resp.Attempts = 0
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Error = err.Error()
			resp.RAGQuery = query
			return resp
		}
		resp.RAGQuery = query
		resp.ContextChars = len(ctxText)
		resp.Retrieval = retrieval
	}
	systemPrompt := buildStructuredProcessSystemPrompt(s, personaPrompt, req.SystemPrompt, ctxText, req.ResponseSchema)
	userPrompt := buildStructuredProcessUserPrompt(req)
	msgs := []chatMsg{{Role: "user", Content: userPrompt}}

	var raw string
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		var buf bytes.Buffer
		if err := rag.getLM().chatStream(ctx, systemPrompt, msgs, &buf); err != nil {
			resp.Attempts = attempt
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Error = err.Error()
			resp.Raw = strings.TrimSpace(buf.String())
			return resp
		}
		raw = strings.TrimSpace(buf.String())
		jsonText, parsed, validationErr, err := processModelOutput(raw, req.ResponseSchema, validateJSON)
		if err == nil {
			resp.OK = true
			resp.ValidJSON = true
			resp.Attempts = attempt
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Raw = jsonText
			resp.Result = parsed
			return resp
		}
		resp.Raw = raw
		resp.ValidationError = validationErr
		if attempt > maxRetries || !repairJSON {
			resp.Attempts = attempt
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Error = err.Error()
			return resp
		}
		msgs = []chatMsg{{
			Role:    "user",
			Content: buildStructuredRepairPrompt(req, validationErr, raw),
		}}
	}
	resp.DurationMS = time.Since(start).Milliseconds()
	resp.Error = "processing failed"
	return resp
}

type weightedSearchQuery struct {
	Query  string
	Weight float64
}

var searchTokenSplitter = regexp.MustCompile(`[^\p{L}\p{N}\-_.#+/]+`)

func splitSearchTokens(s string) []string {
	raw := searchTokenSplitter.Split(strings.TrimSpace(s), -1)
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func hasLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			return true
		}
	}
	return false
}

func isAlphaToken(s string) bool {
	return hasLetter(s) && !hasDigit(s)
}

func stopwordSet() map[string]bool {
	return map[string]bool{
		"was": true, "weisst": true, "weißt": true, "du": true, "ueber": true, "über": true,
		"wer": true, "ist": true, "erzaehl": true, "erzähl": true, "mir": true, "von": true,
		"tell": true, "me": true, "about": true, "what": true, "who": true,
		"the": true, "ein": true, "eine": true, "und": true, "oder": true, "für": true, "fuer": true,
		"with": true, "mit": true, "der": true, "die": true, "das": true,
	}
}

func looksTechnicalQuery(q string) bool {
	tokens := splitSearchTokens(q)
	techScore := 0
	for _, tok := range tokens {
		if hasDigit(tok) {
			techScore++
		}
		if strings.ContainsAny(tok, "-_/") {
			techScore++
		}
		low := strings.ToLower(tok)
		if strings.HasSuffix(low, "rs") || strings.HasSuffix(low, "zz") || strings.Contains(low, "lager") || strings.Contains(low, "bearing") {
			techScore++
		}
	}
	return techScore >= 2
}

func expandRetrievalQueries(q string) []weightedSearchQuery {
	base := strings.TrimSpace(refineSearchQuery(q))
	if base == "" {
		return nil
	}
	seen := map[string]bool{}
	add := func(out *[]weightedSearchQuery, query string, weight float64) {
		query = strings.TrimSpace(query)
		if query == "" {
			return
		}
		key := strings.ToLower(query)
		if seen[key] {
			return
		}
		seen[key] = true
		*out = append(*out, weightedSearchQuery{Query: query, Weight: weight})
	}

	var out []weightedSearchQuery
	add(&out, base, 1.00)

	tokens := splitSearchTokens(base)
	if len(tokens) == 0 {
		return out
	}

	if len(tokens) >= 2 && (hasDigit(tokens[0]) || hasDigit(tokens[1])) {
		add(&out, tokens[0]+" "+tokens[1], 0.98)
	}
	if hasDigit(tokens[0]) {
		add(&out, tokens[0], 0.95)
	}

	stopwords := stopwordSet()
	var alpha []string
	for _, tok := range tokens {
		low := strings.ToLower(tok)
		if stopwords[low] {
			continue
		}
		if isAlphaToken(tok) {
			alpha = append(alpha, tok)
		}
	}
	if len(alpha) > 0 {
		add(&out, alpha[len(alpha)-1], 0.93)
		if len(alpha) > 1 {
			add(&out, strings.Join(alpha, " "), 0.92)
		}
	}
	if len(tokens) >= 3 && hasDigit(tokens[0]) && hasLetter(tokens[len(tokens)-1]) {
		add(&out, tokens[0]+" "+tokens[len(tokens)-1], 0.91)
	}

	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func expandExternalSearchQueries(q string) []string {
	base := strings.TrimSpace(refineSearchQuery(q))
	if base == "" {
		return nil
	}
	seen := map[string]bool{}
	add := func(out *[]string, query string) {
		query = strings.TrimSpace(query)
		if query == "" {
			return
		}
		key := strings.ToLower(query)
		if seen[key] {
			return
		}
		seen[key] = true
		*out = append(*out, query)
	}

	var out []string
	add(&out, base)
	tokens := splitSearchTokens(base)
	if len(tokens) >= 2 && (hasDigit(tokens[0]) || hasDigit(tokens[1])) {
		add(&out, `"`+tokens[0]+` `+tokens[1]+`"`)
		add(&out, tokens[0]+" "+tokens[1])
	}
	if looksTechnicalQuery(base) {
		add(&out, "Technische Details "+base)
		add(&out, `"`+base+`"`)
	}
	for _, q := range expandRetrievalQueries(base) {
		add(&out, q.Query)
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
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
		`was ist (.+)`,
		`worum geht es bei (.+)`,
		`tell me about (.+)`,
		`who is (.+)`,
		`what is (.+)`,
		`qui est (.+)`,
		`parle-moi de (.+)`,
		`qu[ei]én es (.+)`,
		`háblame de (.+)`,
		`hablame de (.+)`,
		`chi è (.+)`,
		`chi e (.+)`,
		`parlami di (.+)`,
		`quem é (.+)`,
		`quem e (.+)`,
		`fale sobre (.+)`,
		`wie is (.+)`,
		`vertel me over (.+)`,
		`kim jest (.+)`,
		`powiedz mi o (.+)`,
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
	system := `You are a retrieval-planning agent for a RAG assistant.

Task:
- Decide whether the current retrieval candidates are already sufficient.
- Prefer ANSWER_DIRECT only if the likely answer can be grounded with good confidence from the shown candidates.
- Prefer RETRIEVE_MORE if the question is broad, ambiguous, entity-specific but under-supported, or likely needs fresher/more precise context.
- If useful, suggest a shorter and more retrieval-friendly query.

Rules:
- Return ONLY one JSON object.
- No markdown, no prose, no code fences.
- Allowed actions: "ANSWER_DIRECT", "RETRIEVE_MORE".
- Use conservative judgment. If uncertain, choose RETRIEVE_MORE.
- Keep k between 4 and 24.
- Keep threshold between 0.45 and 0.9.

Examples:
{"action":"ANSWER_DIRECT"}
{"action":"RETRIEVE_MORE","k":8,"threshold":0.6,"query":"Karte.Bayern"}
{"action":"RETRIEVE_MORE","k":12,"threshold":0.55}
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
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	q := fmt.Sprintf(
		"SELECT content FROM chunks WHERE article = '%s' AND chunk_idx = %d AND %s",
		escapeSQ(article), chunkIdx, roleScopeFilterSQL(activeRole),
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
	role := "it"
	if settings != nil {
		role = settings.get().ActiveRole
	}
	return r.listSourcesForRole(role)
}

func (r *ragSystem) listSourcesForRole(role string) []map[string]any {
	q := fmt.Sprintf("SELECT article, COUNT(*) AS cnt FROM chunks WHERE %s GROUP BY article ORDER BY article", roleScopeFilterSQL(role))
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
	role := "it"
	if settings != nil {
		role = settings.get().ActiveRole
	}
	return r.deleteSourceForRole(article, role)
}

func (r *ragSystem) deleteSourceForRole(article, role string) error {
	q := fmt.Sprintf("DELETE FROM chunks WHERE article = '%s' AND %s", escapeSQ(article), roleScopeFilterSQL(role))
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
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking,omitempty"`
	Time     string `json:"time"`
	// Model records which model was used to produce this message (assistant-only).
	Model     string            `json:"model,omitempty"`
	ModelMeta map[string]string `json:"model_meta,omitempty"`
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
		t := strings.TrimSpace(content)
		t = strings.ReplaceAll(t, "\n", " ")
		if len(t) > 50 {
			t = t[:47] + "..."
		}
		c.Title = t
	}
	_ = cs.saveLocked()
}

// addMessageWithMeta appends a message and also records model metadata for assistant messages.
func (cs *chatStore) addMessageWithMeta(id, role, content, thinking, model string, modelMeta map[string]string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.chats[id]
	if !ok {
		return
	}
	now := time.Now().Format(time.RFC3339)
	msg := chatMessage{Role: role, Content: content, Thinking: thinking, Time: now}
	if model != "" {
		msg.Model = model
	}
	if modelMeta != nil {
		msg.ModelMeta = modelMeta
	}
	c.Messages = append(c.Messages, msg)
	c.Updated = now
	if c.Title == "" && role == "user" {
		t := strings.TrimSpace(content)
		t = strings.ReplaceAll(t, "\n", " ")
		if len(t) > 50 {
			t = t[:47] + "..."
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
	BaseURL   string `json:"base_url"`
	OpenAIKey string `json:"openai_api_key"`
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

func isLocalLLMBase(base string) bool {
	u := strings.ToLower(strings.TrimSpace(base))
	return strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1")
}

func localLLMCandidates() []string {
	return []string{
		"http://localhost:1234",
		"http://localhost:11434",
	}
}

func probeLLMCandidate(base, apiKey string) (discoverCandidate, error) {
	c := discoverCandidate{BaseURL: base, ProviderHint: providerHintFromURL(base)}
	tmp := newLMClient(base, "x", "x", apiKey)
	models, err := tmp.listModels(base)
	if err != nil {
		c.OK = false
		c.Error = err.Error()
		return c, err
	}
	c.OK = true
	c.Models = models
	c.RecommendChat, c.RecommendEmbed = recommendModels(models)
	return c, nil
}

func firstModelOr(current string, models []string) string {
	if current != "" {
		for _, m := range models {
			if m == current {
				return current
			}
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return current
}

func maybePreferOfflineLLM(settings *settingsStore) appSettings {
	s := settings.get()
	apiKey := s.OpenAIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	currentBase := s.ChatBase
	if currentBase == "" {
		currentBase = s.BaseURL
	}
	if isLocalLLMBase(currentBase) {
		if _, err := probeLLMCandidate(currentBase, apiKey); err == nil {
			return s
		}
	}

	var preferred discoverCandidate
	found := false
	for _, base := range localLLMCandidates() {
		c, err := probeLLMCandidate(base, apiKey)
		if err == nil && c.OK {
			preferred = c
			found = true
			break
		}
	}
	if !found {
		return s
	}

	chatModel := firstModelOr(s.ChatModel, preferred.RecommendChat)
	if chatModel == "" {
		chatModel = firstModelOr(s.ChatModel, preferred.Models)
	}
	embedModel := firstModelOr(s.EmbedModel, preferred.RecommendEmbed)
	if embedModel == "" {
		embedModel = firstModelOr(s.EmbedModel, preferred.Models)
	}

	settings.mu.Lock()
	settings.s.BaseURL = preferred.BaseURL
	settings.s.ChatBase = preferred.BaseURL
	settings.s.EmbedBase = preferred.BaseURL
	settings.s.ChatModel = chatModel
	settings.s.EmbedModel = embedModel
	_ = settings.saveLocked()
	settings.mu.Unlock()

	log.Printf("LLM preference: switched to local provider %s (%s)", preferred.ProviderHint, preferred.BaseURL)
	return settings.get()
}

// runWebServer registers HTTP handlers and starts the web interface.
func runWebServer(rag *ragSystem, addr string, settings *settingsStore, chats *chatStore, customAPIs *apiStore, personas *personaStore, modules *moduleStore, llmAvailable bool, llmPingErr error) {
	mux := http.NewServeMux()
	adminUsers := newAdminUserStore(settings)
	apiRoutes := newAPIRouteStore(settings)
	adminGuard := func(h http.HandlerFunc) http.HandlerFunc { return routePolicyMiddleware(settings, h) }

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
				"base_url":               s.BaseURL,
				"chat_base":              s.ChatBase,
				"embed_base":             s.EmbedBase,
				"chat_model":             s.ChatModel,
				"embed_model":            s.EmbedModel,
				"lang":                   s.Lang,
				"theme":                  s.Theme,
				"active_role":            s.ActiveRole,
				"role_permissions":       permissionsForRole(s.ActiveRole),
				"usage_profile":          s.UsageProfile,
				"response_language_mode": s.ResponseLanguageMode,
				"redact_pii":             s.RedactPII,
				"chunk_size":             s.ChunkSize,
				"k":                      s.K,
				"allow_nanogo":           s.AllowNanoGo,
				// Do not return the API key itself; only expose whether one is configured
				"openai_key_present": s.OpenAIKey != "",
			})
			return

		case "POST":
			// Accept chat_base/embed_base and optional OpenAI key for mixed backends
			var req struct {
				BaseURL        string `json:"base_url"`
				ChatBase       string `json:"chat_base"`
				EmbedBase      string `json:"embed_base"`
				ChatModel      string `json:"chat_model"`
				EmbedModel     string `json:"embed_model"`
				OpenAIKey      string `json:"openai_api_key"`
				OpenAIKeyClear bool   `json:"openai_api_key_clear"`
				Theme          string `json:"theme"`
				ActiveRole     string `json:"active_role"`
				UsageProfile   string `json:"usage_profile"`
				ResponseLang   string `json:"response_language_mode"`
				RedactPII      *bool  `json:"redact_pii"`
				AllowNanoGo    *bool  `json:"allow_nanogo"`
				Force          bool   `json:"force"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			// Normalize and fall back
			if req.BaseURL != "" {
				req.BaseURL = normalizeBaseURL(req.BaseURL)
			}
			if req.ChatBase == "" {
				req.ChatBase = req.BaseURL
			} else {
				req.ChatBase = normalizeBaseURL(req.ChatBase)
			}
			if req.EmbedBase == "" {
				req.EmbedBase = req.BaseURL
			} else {
				req.EmbedBase = normalizeBaseURL(req.EmbedBase)
			}
			if req.ChatBase == "" || req.ChatModel == "" || req.EmbedModel == "" {
				http.Error(w, "chat_base, embed_base, chat_model and embed_model are required", 400)
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
			if req.BaseURL != "" {
				settings.s.BaseURL = req.BaseURL
			}
			settings.s.ChatBase = req.ChatBase
			settings.s.EmbedBase = req.EmbedBase
			settings.s.ChatModel = req.ChatModel
			settings.s.EmbedModel = req.EmbedModel
			// Persist provided OpenAI API key. If OpenAIKeyClear is true, clear stored key.
			if req.OpenAIKey != "" {
				settings.s.OpenAIKey = req.OpenAIKey
			} else if req.OpenAIKeyClear {
				settings.s.OpenAIKey = ""
			}
			if req.Theme != "" {
				settings.s.Theme = req.Theme
			}
			if req.ActiveRole != "" {
				settings.s.ActiveRole = normalizeDemoRole(req.ActiveRole)
			}
			if req.UsageProfile != "" {
				settings.s.UsageProfile = normalizeUsageProfile(req.UsageProfile)
			}
			if req.ResponseLang != "" {
				settings.s.ResponseLanguageMode = normalizeResponseLanguageMode(req.ResponseLang)
			}
			if req.RedactPII != nil {
				settings.s.RedactPII = *req.RedactPII
			}
			if req.AllowNanoGo != nil {
				settings.s.AllowNanoGo = *req.AllowNanoGo
			}
			_ = settings.saveLocked()
			settings.mu.Unlock()

			// Apply runtime LM clients (may be composite)
			// Prefer persisted OpenAI key from settings; fallback to env var if none present
			applied := settings.get()
			key := applied.OpenAIKey
			if key == "" {
				key = os.Getenv("OPENAI_API_KEY")
			}
			chatLM := newLMClient(applied.ChatBase, applied.EmbedModel, applied.ChatModel, key)
			embedLM := newLMClient(applied.EmbedBase, applied.EmbedModel, applied.ChatModel, key)
			var provider lmProvider
			if applied.ChatBase == applied.EmbedBase {
				provider = chatLM
			} else {
				provider = &compositeLM{embedClient: embedLM, chatClient: chatLM}
			}
			rag.setLM(provider)

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

	// POST /api/settings/lang — persist UI/wiki default language
	mux.HandleFunc("/api/settings/lang", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Lang string `json:"lang"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		lang := strings.ToLower(strings.TrimSpace(req.Lang))
		if lang == "" {
			http.Error(w, "missing lang", 400)
			return
		}
		settings.mu.Lock()
		settings.s.Lang = lang
		_ = settings.saveLocked()
		settings.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "lang": lang})
	})

	// POST /api/settings/role — switch demo role context (light RBAC)
	mux.HandleFunc("/api/settings/role", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		role := normalizeDemoRole(req.Role)
		settings.mu.Lock()
		settings.s.ActiveRole = role
		_ = settings.saveLocked()
		settings.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"role":        role,
			"permissions": permissionsForRole(role),
		})
	})

	// GET /api/modules — list configured modules
	mux.HandleFunc("/api/modules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(modules.list())
	})

	// POST /api/modules/save — update one module configuration
	mux.HandleFunc("/api/modules/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req moduleConfig
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		mod, err := modules.upsert(req)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mod)
	})

	// POST /api/modules/run — preview or ingest a module result into RAG
	mux.HandleFunc("/api/modules/run", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		role := settings.get().ActiveRole
		if !permissionsForRole(role).CanRunModules {
			http.Error(w, fmt.Sprintf("role %s is not allowed to run modules", demoRoleLabel(role)), 403)
			return
		}
		var req struct {
			ID     string `json:"id"`
			Action string `json:"action"`
			Arg    string `json:"arg"`
			Limit  int    `json:"limit"`
			Ingest bool   `json:"ingest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing module id", 400)
			return
		}
		mod, ok := modules.get(req.ID)
		if !ok {
			http.Error(w, "module not found", 404)
			return
		}
		if !mod.Enabled {
			http.Error(w, "module disabled", 403)
			return
		}
		res, err := executeModuleRun(mod, rag, settings.get().EmbedModel, req.Action, req.Arg, req.Limit, req.Ingest)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	}))

	// POST /api/modules/upload?id=<module>&target=<relative-path>
	mux.HandleFunc("/api/modules/upload", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		role := settings.get().ActiveRole
		if !permissionsForRole(role).CanRunModules {
			http.Error(w, fmt.Sprintf("role %s is not allowed to upload via modules", demoRoleLabel(role)), 403)
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		target := r.URL.Query().Get("target")
		if id == "" {
			http.Error(w, "missing module id", 400)
			return
		}
		mod, ok := modules.get(id)
		if !ok || mod.Kind != "http-folder" {
			http.Error(w, "http-folder module not found", 404)
			return
		}
		if !mod.Enabled {
			http.Error(w, "module disabled", 403)
			return
		}
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file", 400)
			return
		}
		defer file.Close()
		savedPath, err := saveUploadedModuleFile(mod, file, header, target)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp := map[string]any{
			"module_id": mod.ID,
			"path":      savedPath,
			"file":      header.Filename,
		}
		if parseBoolString(mod.Config["ingest_on_upload"]) {
			text, err := readFileForRAG(savedPath, 5*1024*1024)
			if err == nil && strings.TrimSpace(text) != "" {
				cfg := settings.get()
				source := "module:" + mod.ID + ":upload:" + filepath.Base(savedPath)
				chunks, redactions := chunksForIngest(text, cfg)
				if err := rag.addChunks(source, chunks, cfg.EmbedModel); err == nil {
					resp["chunks"] = len(chunks)
					resp["source"] = source
					if redactions > 0 {
						resp["redactions"] = redactions
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))

	// GET /api/modules/download?id=<module>&path=<relative-path>
	mux.HandleFunc("/api/modules/download", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		role := settings.get().ActiveRole
		if !permissionsForRole(role).CanRunModules {
			http.Error(w, fmt.Sprintf("role %s is not allowed to download via modules", demoRoleLabel(role)), 403)
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		pathArg := r.URL.Query().Get("path")
		if id == "" || pathArg == "" {
			http.Error(w, "missing id or path", 400)
			return
		}
		mod, ok := modules.get(id)
		if !ok || mod.Kind != "http-folder" {
			http.Error(w, "http-folder module not found", 404)
			return
		}
		target, err := resolveModulePath(mod.Config["root_dir"], pathArg, parseBoolString(mod.Config["allow_subfolders"]))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if parseBoolString(mod.Config["download_ingest"]) {
			if text, err := readFileForRAG(target, 5*1024*1024); err == nil && strings.TrimSpace(text) != "" {
				cfg := settings.get()
				source := "module:" + mod.ID + ":download:" + filepath.Base(target)
				chunks, _ := chunksForIngest(text, cfg)
				_ = rag.addChunks(source, chunks, cfg.EmbedModel)
			}
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(target)))
		http.ServeFile(w, r, target)
	}))

	// GET /api/discover — auto-discover common local endpoints
	mux.HandleFunc("/api/discover", func(w http.ResponseWriter, r *http.Request) {
		candidates := []string{
			"http://localhost:1234",  // LM Studio default
			"http://localhost:11434", // Ollama default
		}
		var out []discoverCandidate
		for _, base := range candidates {
			c := discoverCandidate{BaseURL: base, ProviderHint: providerHintFromURL(base)}
			// Use persisted OpenAI key if present, else env var
			key := settings.get().OpenAIKey
			if key == "" {
				key = os.Getenv("OPENAI_API_KEY")
			}
			tmp := newLMClient(base, "x", "x", key)
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

	// GET /api/llm/status — report whether initial LLM ping succeeded
	mux.HandleFunc("/api/llm/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		msg := ""
		ok := true
		if !llmAvailable {
			ok = false
			if llmPingErr != nil {
				msg = llmPingErr.Error()
			} else {
				msg = "LLM endpoint not available"
			}
		}
		// Use live settings snapshot for base URL
		cur := settings.get()
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "base_url": cur.BaseURL, "message": msg})
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
		// Prefer key provided in request (for quick tests), else stored or env var
		key := req.OpenAIKey
		if key == "" {
			key = settings.get().OpenAIKey
		}
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		tmp := newLMClient(req.BaseURL, "x", "x", key)
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
	mux.HandleFunc("/api/process", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req processRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if req.Input == nil {
			http.Error(w, "missing input", 400)
			return
		}
		if req.RequestID == "" {
			req.RequestID = newRequestID()
		}
		req.Mode = normalizeProcessMode(req.Mode)

		s := settings.get()
		personaID := strings.TrimSpace(req.PersonaID)
		if personaID == "" {
			personaID = personas.defaultID()
		}
		personaPrompt := ""
		if personaID != "" {
			if per, ok := personas.get(personaID); ok {
				personaPrompt = per.Prompt
			}
		}

		resp := runStructuredProcess(r.Context(), rag, s, personaPrompt, req)
		status := 200
		if !resp.OK {
			status = 500
			if resp.ValidationError != "" || !resp.ValidJSON {
				status = 422
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}))

	// POST /api/ask — SSE streaming answer
	mux.HandleFunc("/api/ask", adminGuard(func(w http.ResponseWriter, r *http.Request) {
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

		totalChunks := rag.docCountForRole(s.ActiveRole)
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
			"chat_id":          conv.ID,
			"title":            conv.Title,
			"request_id":       reqID,
			"mode":             mode,
			"k":                usedK,
			"base_k":           rag.k,
			"chunk_size":       s.ChunkSize,
			"total_chunks":     totalChunks,
			"storage_mode":     storageModeLabel(rag.storageMode),
			"db_path":          rag.dbPath,
			"auto_search":      req.AutoSearch,
			"debug":            req.Debug,
			"deep":             req.Deep,
			"offline":          req.Offline,
			"message_count":    len(conv.Messages),
			"created":          conv.Created,
			"updated":          conv.Updated,
			"persona_id":       personaID,
			"persona_name":     personaName,
			"active_role":      s.ActiveRole,
			"role_label":       demoRoleLabel(s.ActiveRole),
			"role_permissions": permissionsForRole(s.ActiveRole),
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
			ActiveRole:         s.ActiveRole,
			RoleLabel:          demoRoleLabel(s.ActiveRole),
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
			s = settings.get()
			modelMeta := map[string]string{"base_url": s.BaseURL, "chat_model": s.ChatModel}
			chats.addMessageWithMeta(conv.ID, "assistant", answer.String(), "", s.ChatModel, modelMeta)
			return
		}

		// Normal mode: call LM with SSE streaming
		pr, pw := io.Pipe()

		allTools := filterToolsForRole(append(customAPIs.allTools(), modules.enabledTools()...), s.ActiveRole)
		// build system prompt; in deep mode add research instructions
		var systemPrompt string
		systemPrompt = buildToolSystemPrompt(ctxText, allTools, req.Deep, s)
		if personaPrompt != "" {
			systemPrompt = personaPrompt + "\n\n" + systemPrompt
		}

		// Validate system prompt isn't absurdly long
		if len(systemPrompt) > 32000 {
			log.Printf("REQ %s: WARN system prompt too long (%d chars), truncating context", reqID, len(systemPrompt))
			// Truncate context to first 5000 chars as fallback
			if len(ctxText) > 5000 {
				ctxText = ctxText[:5000] + "\n[... Kontext gekürzt ...]"
				systemPrompt = buildToolSystemPrompt(ctxText, allTools, req.Deep, s)
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

		var thinkBuf bytes.Buffer
		streamErr := make(chan error, 1)
		go func() {
			err := rag.getLM().chatStreamDetailed(context.Background(), systemPrompt, msgs, pw, &thinkBuf)
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
			s = settings.get()
			modelMeta := map[string]string{"base_url": s.BaseURL, "chat_model": s.ChatModel}
			chats.addMessageWithMeta(conv.ID, "assistant", "Fehler im LLM-Stream: "+serr.Error(), "", s.ChatModel, modelMeta)
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
			s = settings.get()
			modelMeta := map[string]string{"base_url": s.BaseURL, "chat_model": s.ChatModel}
			if answer.Len() == 0 {
				chats.addMessageWithMeta(conv.ID, "assistant", "LLM-Fehler: "+err.Error(), "", s.ChatModel, modelMeta)
			} else {
				answerStr := stripInternalThinking(answer.String())
				thinkingStr := strings.TrimSpace(thinkBuf.String())
				if tr, ok := extractToolRequest(answerStr); ok {
					trJSON, _ := json.Marshal(tr)
					fmt.Fprintf(w, "event: tool_request\ndata: %s\n\n", trJSON)
					flusher.Flush()
					answerStr = stripToolRequest(answerStr)
				}
				if thinkingStr != "" {
					fmt.Fprintf(w, "event: reasoning\ndata: %s\n\n", mustJSON(thinkingStr))
					flusher.Flush()
				}
				chats.addMessageWithMeta(conv.ID, "assistant", answerStr, thinkingStr, s.ChatModel, modelMeta)
			}
			return
		}

		if tokenCount == 0 {
			log.Printf("REQ %s: WARN LM returned no tokens despite no error", reqID)
		}

		// Tool request marker handling
		answerStr := stripInternalThinking(answer.String())
		thinkingStr := strings.TrimSpace(thinkBuf.String())
		if tr, ok := extractToolRequest(answerStr); ok {
			trJSON, _ := json.Marshal(tr)
			// Notify frontend that a tool was requested
			fmt.Fprintf(w, "event: tool_request\ndata: %s\n\n", trJSON)
			flusher.Flush()

			// Decide whether to execute automatically based on policy
			s = settings.get()
			execAllowed := shouldAutoExecuteTool(s, tr, req.AutoSearch)

			if execAllowed {
				text, source, fetchErr := executeToolRequest(tr, s, rag, customAPIs, modules)

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
					chunks, _ := chunksForIngest(text, s)
					if err := rag.addChunks(source, chunks, settings.get().EmbedModel); err != nil {
						log.Printf("REQ %s: failed to add tool result to RAG: %v", reqID, err)
					} else {
						log.Printf("REQ %s: tool result added to RAG: %s (%d chunks)", reqID, source, len(chunks))
					}

					// Ask the model to rewrite the full answer from the tool result instead of appending a duplicate continuation.
					cleanAnswer := stripToolRequest(answerStr)
					contMsgs := make([]chatMsg, 0, len(msgs)+2)
					contMsgs = append(contMsgs, msgs...)
					contMsgs = append(contMsgs, chatMsg{Role: "assistant", Content: cleanAnswer})
					contMsgs = append(contMsgs, chatMsg{Role: "user", Content: fmt.Sprintf("Ich habe das Tool %s ausgefuehrt.\n\nTool-Ergebnis:\n%s\n\nErstelle jetzt eine einzige ueberarbeitete finale Antwort in der passenden Sprache gemaess den Systemregeln.\n\nZiel:\n- Beste moegliche Endfassung fuer den Nutzer.\n\nRegeln:\n- Nicht wiederholen oder anhaengen, sondern komplett ueberarbeiten.\n- Nutze lokale Wissensbasis und Tool-Ergebnis nur in dem Mass, wie sie belastbar sind.\n- Trenne klar zwischen lokalem Wissen und externer Recherche, wenn beides vorkommt.\n- Markiere unsichere, duenn belegte oder moeglicherweise veraltete Aussagen vorsichtig.\n- Wenn die Recherche wenig hergibt, sage das offen.\n- Schreibe kompakt, konkret und ohne Marketing-Sprache.\n- Keine TOOL_REQUEST-Marker und keine Meta-Erklaerungen ueber den internen Ablauf.\n", tr.Tool, text)})

					// Stream continuation
					pr2, pw2 := io.Pipe()
					var thinkBuf2 bytes.Buffer
					go func() {
						err := rag.getLM().chatStreamDetailed(context.Background(), systemPrompt, contMsgs, pw2, &thinkBuf2)
						if err != nil {
							pw2.CloseWithError(err)
							log.Printf("REQ %s: LM continuation failed: %v", reqID, err)
						} else {
							pw2.Close()
						}
					}()
					sc2 := bufio.NewScanner(pr2)
					sc2.Split(bufio.ScanRunes)
					var contAnswer strings.Builder
					for sc2.Scan() {
						tok := sc2.Text()
						contAnswer.WriteString(tok)
						fmt.Fprintf(w, "data: %s\n\n", mustJSON(tok))
						flusher.Flush()
					}
					if scErr := sc2.Err(); scErr != nil {
						log.Printf("REQ %s: continuation scanner error: %v", reqID, scErr)
					}
					if rewritten := stripInternalThinking(contAnswer.String()); rewritten != "" {
						answerStr = rewritten
					} else {
						answerStr = cleanAnswer
					}
					if strings.TrimSpace(thinkBuf2.String()) != "" {
						if thinkingStr != "" {
							thinkingStr += "\n\n"
						}
						thinkingStr += strings.TrimSpace(thinkBuf2.String())
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
			answerStr = stripToolRequest(answerStr)
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()

		log.Printf("REQ %s: Chat response complete: %d chars, tokens_streamed=%d", reqID, len(answerStr), tokenCount)
		s = settings.get()
		modelMeta := map[string]string{"base_url": s.BaseURL, "chat_model": s.ChatModel}
		if thinkingStr != "" {
			fmt.Fprintf(w, "event: reasoning\ndata: %s\n\n", mustJSON(thinkingStr))
			flusher.Flush()
		}
		chats.addMessageWithMeta(conv.ID, "assistant", answerStr, thinkingStr, s.ChatModel, modelMeta)
	}))

	// GET /api/tools — list available tools
	mux.HandleFunc("/api/tools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s := settings.get()
		json.NewEncoder(w).Encode(filterToolsForRole(append(customAPIs.allTools(), modules.enabledTools()...), s.ActiveRole))
	})

	// POST /api/tool/execute — execute a tool and add results to RAG
	mux.HandleFunc("/api/tool/execute", adminGuard(func(w http.ResponseWriter, r *http.Request) {
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
		if !canRoleUseTool(s.ActiveRole, req.Tool) {
			http.Error(w, fmt.Sprintf("tool %q is not allowed for role %s", req.Tool, demoRoleLabel(s.ActiveRole)), 403)
			return
		}

		var text string
		var source string
		var fetchErr error
		text, source, fetchErr = executeToolRequest(req, s, rag, customAPIs, modules)

		if fetchErr != nil {
			http.Error(w, fmt.Sprintf("Tool %q fehlgeschlagen: %v", req.Tool, fetchErr), 500)
			return
		}

		chunks, redactions := chunksForIngest(text, s)
		if err := rag.addChunks(source, chunks, settings.get().EmbedModel); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tool":       req.Tool,
			"query":      req.Query,
			"source":     source,
			"chars":      len(text),
			"chunks":     len(chunks),
			"total":      rag.docCountForRole(s.ActiveRole),
			"redactions": redactions,
		})
	}))

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
		if !permissionsForRole(s.ActiveRole).CanRunCode {
			http.Error(w, fmt.Sprintf("role %s is not allowed to execute code", demoRoleLabel(s.ActiveRole)), 403)
			return
		}
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
	mux.HandleFunc("/api/search", adminGuard(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	// POST /api/add-wiki
	mux.HandleFunc("/api/add-wiki", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Article    string   `json:"article"`
			Lang       string   `json:"lang"`
			EmbedModel string   `json:"embed_model"`
			Roles      []string `json:"roles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Article == "" {
			http.Error(w, "missing article", 400)
			return
		}
		s := settings.get()
		if !permissionsForRole(s.ActiveRole).CanWebFetch {
			http.Error(w, fmt.Sprintf("role %s is not allowed to fetch external web sources", demoRoleLabel(s.ActiveRole)), 403)
			return
		}
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
		chunks, redactions := chunksForIngest(text, s)
		em := settings.get().EmbedModel
		if req.EmbedModel != "" {
			em = req.EmbedModel
		}
		roleScopes := normalizeRoleScopes(req.Roles, s.ActiveRole)
		if err := rag.addChunksWithRoles(req.Article, chunks, em, roleScopes); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"article":    req.Article,
			"chars":      len(text),
			"chunks":     len(chunks),
			"total":      rag.docCountForRole(s.ActiveRole),
			"redactions": redactions,
			"roles":      roleScopes,
		})
	}))

	// POST /api/add-url
	mux.HandleFunc("/api/add-url", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			URL        string   `json:"url"`
			EmbedModel string   `json:"embed_model"`
			Roles      []string `json:"roles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
			http.Error(w, "missing url", 400)
			return
		}
		if _, err := url.ParseRequestURI(req.URL); err != nil {
			http.Error(w, "invalid url", 400)
			return
		}
		s := settings.get()
		if !permissionsForRole(s.ActiveRole).CanWebFetch {
			http.Error(w, fmt.Sprintf("role %s is not allowed to fetch external web sources", demoRoleLabel(s.ActiveRole)), 403)
			return
		}
		text, err := fetchURL(req.URL)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		chunks, redactions := chunksForIngest(text, s)
		em := settings.get().EmbedModel
		if req.EmbedModel != "" {
			em = req.EmbedModel
		}
		roleScopes := normalizeRoleScopes(req.Roles, s.ActiveRole)
		if err := rag.addChunksWithRoles(req.URL, chunks, em, roleScopes); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"source":     req.URL,
			"chars":      len(text),
			"chunks":     len(chunks),
			"total":      rag.docCountForRole(s.ActiveRole),
			"redactions": redactions,
			"roles":      roleScopes,
		})
	}))

	// POST /api/add-folder — import all text files from a server directory
	mux.HandleFunc("/api/add-folder", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Path       string   `json:"path"`
			Recursive  bool     `json:"recursive"`
			EmbedModel string   `json:"embed_model"`
			Roles      []string `json:"roles"`
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
		if !permissionsForRole(s.ActiveRole).CanBulkIngest {
			http.Error(w, fmt.Sprintf("role %s is not allowed to run bulk ingest", demoRoleLabel(s.ActiveRole)), 403)
			return
		}
		roleScopes := normalizeRoleScopes(req.Roles, s.ActiveRole)
		em := s.EmbedModel
		if req.EmbedModel != "" {
			em = req.EmbedModel
		}
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
			chunks, _ := chunksForIngest(text, s)
			if err := rag.addChunksWithRoles(source, chunks, em, roleScopes); err != nil {
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
			"total":        rag.docCountForRole(s.ActiveRole),
			"errors":       errors,
			"roles":        roleScopes,
		})
	}))

	// POST /api/add-text
	mux.HandleFunc("/api/add-text", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Title      string   `json:"title"`
			Text       string   `json:"text"`
			EmbedModel string   `json:"embed_model"`
			Roles      []string `json:"roles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
			http.Error(w, "missing text", 400)
			return
		}
		s := settings.get()
		if req.Title == "" {
			req.Title = "manual-" + strconv.FormatInt(time.Now().Unix(), 10)
		}
		chunks, redactions := chunksForIngest(req.Text, s)
		em := settings.get().EmbedModel
		if req.EmbedModel != "" {
			em = req.EmbedModel
		}
		roleScopes := normalizeRoleScopes(req.Roles, s.ActiveRole)
		if err := rag.addChunksWithRoles(req.Title, chunks, em, roleScopes); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"title":      req.Title,
			"chars":      len(req.Text),
			"chunks":     len(chunks),
			"total":      rag.docCountForRole(s.ActiveRole),
			"redactions": redactions,
			"roles":      roleScopes,
		})
	}))

	// POST /api/upload
	mux.HandleFunc("/api/upload", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		role := settings.get().ActiveRole
		if !permissionsForRole(role).CanBulkIngest {
			http.Error(w, fmt.Sprintf("role %s is not allowed to upload files", demoRoleLabel(role)), 403)
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
		// optional embed_model from form
		em := r.FormValue("embed_model")
		if em == "" {
			em = settings.get().EmbedModel
		}
		lower := strings.ToLower(filename)
		s := settings.get()
		roleScopes := normalizeRoleScopes(parseRoleCSV(r.FormValue("roles")), s.ActiveRole)
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
					chunks, _ := chunksForIngest(string(content), s)
					if err := rag.addChunksWithRoles(src, chunks, em, roleScopes); err != nil {
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
					chunks, _ := chunksForIngest(string(content), s)
					if err := rag.addChunksWithRoles(src, chunks, em, roleScopes); err != nil {
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
				"total":   rag.docCountForRole(s.ActiveRole),
				"errors":  errorsList,
				"roles":   roleScopes,
			})
			return
		}

		// regular single-file upload
		text := string(data)
		title := filepath.Base(header.Filename)
		chunks, redactions := chunksForIngest(text, s)
		if err := rag.addChunksWithRoles(title, chunks, em, roleScopes); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"file":       title,
			"chars":      len(text),
			"chunks":     len(chunks),
			"total":      rag.docCountForRole(s.ActiveRole),
			"redactions": redactions,
			"roles":      roleScopes,
		})
	}))

	// GET /api/stats
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"chunks":       rag.docCountForRole(s.ActiveRole),
			"sources":      rag.listSourcesForRole(s.ActiveRole),
			"active_role":  s.ActiveRole,
			"chunks_total": rag.docCount(),
		})
	})

	// GET /api/sources
	mux.HandleFunc("/api/sources", func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rag.listSourcesForRole(s.ActiveRole))
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
		s := settings.get()
		if err := rag.deleteSourceForRole(req.Article, s.ActiveRole); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"deleted": req.Article, "total": rag.docCountForRole(s.ActiveRole)})
	})

	// GET /api/chats — list conversations
	mux.HandleFunc("/api/chats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chats.list())
	})

	// POST /api/chunks/clear — delete all stored chunks (requires explicit confirm flag)
	mux.HandleFunc("/api/chunks/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Confirm bool `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if !req.Confirm {
			http.Error(w, "confirm required", 400)
			return
		}

		// Delete all chunks and reset counters
		delQ := "DELETE FROM chunks"
		if st, err := tinysql.ParseSQL(delQ); err == nil {
			rag.dbMu.Lock()
			_, _ = tinysql.Execute(context.Background(), rag.db, "default", st)
			rag.dbMu.Unlock()
		}
		rag.idMu.Lock()
		rag.nextID = rag.maxChunkIDLocked() + 1
		rag.idMu.Unlock()
		_ = rag.save()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "total": rag.docCount()})
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

	mux.HandleFunc("/api/admin/users", localAdminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(adminUsers.list())
	}))

	mux.HandleFunc("/api/admin/users/create", localAdminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Name string `json:"name"`
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			http.Error(w, "missing name", 400)
			return
		}
		user, token, err := adminUsers.create(req.Name, req.Role)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"user":    user,
			"api_key": token,
		})
	}))

	mux.HandleFunc("/api/admin/users/save", localAdminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Role    string `json:"role"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing id", 400)
			return
		}
		user, err := adminUsers.update(req.ID, req.Name, req.Role, req.Enabled)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)
	}))

	mux.HandleFunc("/api/admin/users/regenerate", localAdminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing id", 400)
			return
		}
		user, token, err := adminUsers.regenerateKey(req.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"user":    user,
			"api_key": token,
		})
	}))

	mux.HandleFunc("/api/admin/users/delete", localAdminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing id", 400)
			return
		}
		ok, err := adminUsers.remove(req.ID)
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
	}))

	mux.HandleFunc("/api/admin/routes", localAdminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiRoutes.list())
	}))

	mux.HandleFunc("/api/admin/routes/save", localAdminOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Path    string `json:"path"`
			Enabled bool   `json:"enabled"`
			Public  bool   `json:"public"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
			http.Error(w, "missing path", 400)
			return
		}
		rule, err := apiRoutes.update(req.Path, req.Enabled, req.Public)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rule)
	}))

	fmt.Printf("Web interface: http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// execSmallR executes the smallR demo to evaluate `expr` and returns its stdout.
// It prefers a local `./smallr` binary if present, otherwise falls back to
// `go run smallr.go -e` which requires the Go toolchain at runtime.
func execSmallR(expr string) (string, error) {
	// Acquire a smallR context from the pool to avoid repeated allocations
	v := smallRPool.Get()
	ctx := v.(*smallr.Context)
	defer smallRPool.Put(ctx)

	res, err := ctx.EvalString(expr)
	if err != nil {
		return "", fmt.Errorf("smallr eval failed: %w", err)
	}
	if strings.TrimSpace(res.Output) != "" {
		return res.Output, nil
	}
	return res.Value.String(), nil
}

// execShellCommand executes a safe set of shell commands on the server.
// Allowed commands are restricted for security reasons.
func execShellCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)

	// Whitelist of safe commands that can be executed
	allowedCommands := map[string]bool{
		"ls": true, "cat": true, "head": true, "tail": true,
		"echo": true, "curl": true, "wget": true, "date": true,
		"pwd": true, "whoami": true, "uname": true, "df": true,
	}

	// Extract the base command (first word)
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}

	baseCmd := filepath.Base(parts[0]) // Use basename to avoid path traversal
	if !allowedCommands[baseCmd] {
		return "", fmt.Errorf("command %q is not allowed", baseCmd)
	}

	// For security, use subprocess without shell interpretation
	// This prevents shell injection attacks
	out, err := exec.Command(baseCmd, parts[1:]...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("shell command failed: %w", err)
	}
	return string(out), nil
}

// execTinyGoProgram interprets Go code directly in a sandboxed environment.
// Similar to nanoGo, it doesn't compile but rather interprets the code.
func execTinyGoProgram(source string) (string, error) {
	timeout := 10 * time.Second // Slightly longer timeout for interpreted programs
	return RunSafe(source, timeout)
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
	// Limit concurrent interpreter instances to avoid spikes.
	nanoGoSem <- struct{}{}
	defer func() { <-nanoGoSem }()

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
	settings, err = loadOrCreateSettings(*settingsPath, defaults)
	if err != nil {
		log.Fatalf("Failed to load settings: %v", err)
	}
	s := maybePreferOfflineLLM(settings)
	// Prefer persisted OpenAI key from settings; fallback to env var if none present
	openaiKey := s.OpenAIKey
	if openaiKey == "" {
		openaiKey = os.Getenv("OPENAI_API_KEY")
	}

	// Connect to LLM endpoints (chat vs embed). Support mixed backends.
	chatBase := s.ChatBase
	embedBase := s.EmbedBase
	if chatBase == "" {
		chatBase = s.BaseURL
	}
	if embedBase == "" {
		embedBase = s.BaseURL
	}

	chatLM := newLMClient(chatBase, s.EmbedModel, s.ChatModel, openaiKey)
	embedLM := newLMClient(embedBase, s.EmbedModel, s.ChatModel, openaiKey)

	fmt.Printf("Connecting to chat endpoint (%s) and embed endpoint (%s)… ", chatBase, embedBase)
	var llmAvailable = false
	var llmPingErr error
	if err := chatLM.ping(); err == nil {
		llmAvailable = true
	} else {
		llmPingErr = err
	}
	if err := embedLM.ping(); err == nil {
		llmAvailable = true
	} else if llmPingErr == nil {
		llmPingErr = err
	}
	if llmAvailable {
		fmt.Println("OK")
	} else {
		fmt.Println("FAILED")
		log.Printf("Cannot reach any LLM endpoint (chat: %s, embed: %s). %v\nTip: open Settings in the UI and pick LM Studio (:1234) or Ollama (:11434), or set OPENAI_API_KEY for OpenAI.", chatBase, embedBase, llmPingErr)
	}

	var provider lmProvider
	if chatBase == embedBase {
		provider = chatLM
	} else {
		provider = &compositeLM{embedClient: embedLM, chatClient: chatLM}
	}

	rag, err := newRAG(provider, s.K, *dbPath, storageMode, *maxMemMB)
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
	modules := newModuleStore(settings)
	personas := newPersonaStore(settings)
	chats := newChatStore(*chatsPath)

	if *web {
		runWebServer(rag, *addr, settings, chats, customAPIs, personas, modules, llmAvailable, llmPingErr)
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
			chunks, _ := chunksForIngest(text, s)
			fmt.Printf("  %d chars -> %d chunks\n", len(text), len(chunks))
			if err := rag.addChunks(art, chunks, settings.get().EmbedModel); err != nil {
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
