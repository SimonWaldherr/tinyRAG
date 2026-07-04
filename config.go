package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

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
	RedactPII   bool            `json:"redact_pii"`
	ChunkSize   int             `json:"chunk_size"`
	K           int             `json:"k"`
	CustomAPIs  []customAPI     `json:"custom_apis"`
	Modules     []moduleConfig  `json:"modules"`
	PullSources []ragPullSource `json:"pull_sources"`
	Personas    []persona       `json:"personas"`
	APIUsers    []adminAPIUser  `json:"api_users"`
	APIRoutes   []apiRouteRule  `json:"api_routes"`
	// VectorSearchThreshold is the minimum cosine-similarity score (0–1) for a
	// primary retrieval hit to be included in the answer context. Defaults to
	// 0.60 when zero. Neighbor chunks (Score=-1) are always included.
	VectorSearchThreshold float64 `json:"vector_search_threshold"`
	// RerankMode selects the second-stage re-ranking strategy applied after
	// vector retrieval: "off", "lexical" (default) or "llm".
	RerankMode string `json:"rerank_mode"`
	// AgentPlannerEnabled turns on the multi-step tool planner: before
	// streaming the answer, the LLM plans up to AgentMaxPlanSteps tool calls
	// which are executed upfront and injected into the conversation.
	AgentPlannerEnabled bool `json:"agent_planner_enabled"`
	// AgentMaxPlanSteps caps the number of planned tool steps (default 3, max 5).
	AgentMaxPlanSteps int `json:"agent_max_plan_steps"`

	// ── Vector store backend ──────────────────────────────────────────────────
	// VectorBackend selects the vector persistence backend.
	// Allowed values: "tinysql" (default), "sqlite-vec" (requires -tags sqlite_vec).
	// Switching backends requires re-ingesting all data.
	VectorBackend string `json:"vector_backend"`
	// StorageMode controls the tinySQL storage strategy when VectorBackend is
	// "tinysql".  Allowed values: "memory", "wal", "disk", "index", "hybrid".
	// When empty, the -storage CLI flag value is used; the CLI flag takes
	// precedence on startup so that operators can override without editing JSON.
	StorageMode string `json:"storage_mode"`

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

	// ── UI configuration & theming ────────────────────────────────────────────
	// UI controls which panels, chat modes, pickers and suggestions the web
	// frontend renders. Defaults to everything enabled (see normalizeUIConfig).
	UI uiConfig `json:"ui"`
	// CustomThemes holds operator-defined themes (CSS-variable maps applied
	// on top of a built-in base theme).
	CustomThemes []uiThemeDef `json:"custom_themes"`

	// ── Branding (white-label) ────────────────────────────────────────────────
	// AppName replaces "tinyRAG" throughout the UI. Empty → "tinyRAG".
	AppName string `json:"app_name"`
	// AppLogoURL is shown in the sidebar header. Can be an absolute URL or a
	// base64 data-URI. Empty → default tinyRAG wordmark.
	AppLogoURL string `json:"app_logo_url"`
	// CustomCSS is injected verbatim into <style> at page load.
	CustomCSS string `json:"custom_css"`

	// ── Web-UI authentication (optional) ─────────────────────────────────────
	// WebUIAuth enables the login screen. When false (default) the UI is
	// accessible without credentials (single-user / local mode).
	WebUIAuth bool `json:"web_ui_auth"`
	// WebUIUsers is the list of local login accounts. Each user has a
	// username, salted-SHA256 password hash, and a role ("admin"/"viewer").
	WebUIUsers []webUIUser `json:"web_ui_users"`
	// SessionTTLSeconds is the lifetime of a session cookie. Default: 86400 (24 h).
	SessionTTLSeconds int `json:"session_ttl_seconds"`

	// ── LDAP (optional) ───────────────────────────────────────────────────────
	// When LDAPEnabled is true, the login form also accepts LDAP credentials.
	// Local WebUIUsers are tried first; LDAP is used as a fallback.
	LDAPEnabled  bool   `json:"ldap_enabled"`
	LDAPServer   string `json:"ldap_server"`              // hostname or IP
	LDAPPort     int    `json:"ldap_port"`                // default 389 (636 for TLS)
	LDAPUseTLS   bool   `json:"ldap_use_tls"`             // LDAPS
	LDAPStartTLS bool   `json:"ldap_start_tls"`           // STARTTLS over plain connection
	LDAPBaseDN   string `json:"ldap_base_dn"`             // e.g. "dc=example,dc=com"
	LDAPBindDN   string `json:"ldap_bind_dn"`             // service-account DN for user search
	LDAPBindPass string `json:"ldap_bind_pass,omitempty"` // service-account password
	// LDAPUserAttr is the attribute used to match the username, e.g. "uid" (POSIX)
	// or "sAMAccountName" (Active Directory).
	LDAPUserAttr string `json:"ldap_user_attr"`
	// LDAPFilter is an additional LDAP filter appended to the user search,
	// e.g. "(memberOf=cn=rag-users,ou=groups,dc=example,dc=com)".
	// Leave empty for no extra filter.
	LDAPFilter string `json:"ldap_filter"`
	// LDAPAdminGroup is the DN of a group whose members receive admin role.
	// Leave empty to assign "viewer" role to all LDAP users by default.
	LDAPAdminGroup string `json:"ldap_admin_group"`
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

// sessions holds all active web-UI browser sessions.
var sessions = newSessionStore()
var connectorRegistryStore *connectorStore
var connectorRuntimeExec *connectorExecutor

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
	case "local_search", "datetime", "calculate", "llm", "vector_query", "sql_query":
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

func aclGroupsFilterSQL(role string) string {
	token := escapeSQ(roleScopeToken(role))
	return fmt.Sprintf("(acl_groups IS NULL OR acl_groups = '' OR acl_groups = '|all|' OR acl_groups LIKE '%%%s%%')", token)
}

func roleAndACLFilterSQL(role string) string {
	return fmt.Sprintf("(%s AND %s)", roleScopeFilterSQL(role), aclGroupsFilterSQL(role))
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
		PullSources:          []ragPullSource{},
		Personas:             defaultPersonas(),
		APIUsers:             []adminAPIUser{},
		APIRoutes:            defaultAPIRouteRules(),
		AllowCodeExec:        false,
		AllowNanoGo:          false,
		AllowShellExec:       false,
		AllowTinyGo:          false,
		AppName:              "",
		AppLogoURL:           "",
		CustomCSS:            "",
		WebUIAuth:            false,
		WebUIUsers:           []webUIUser{},
		SessionTTLSeconds:    86400,
		LDAPEnabled:          false,
		LDAPUserAttr:         "uid",
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
			if len(ss.s.CustomThemes) == 0 {
				ss.s.CustomThemes = defaultCustomThemes()
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
	ss.s.RerankMode = normalizeRerankMode(ss.s.RerankMode)
	ss.s.UI = normalizeUIConfig(ss.s.UI)
	validThemes := ss.s.CustomThemes[:0]
	for _, t := range ss.s.CustomThemes {
		if clean, err := sanitizeCustomTheme(t); err == nil {
			validThemes = append(validThemes, clean)
		}
	}
	ss.s.CustomThemes = validThemes
	if ss.s.AgentMaxPlanSteps <= 0 {
		ss.s.AgentMaxPlanSteps = 3
	}
	if ss.s.AgentMaxPlanSteps > 5 {
		ss.s.AgentMaxPlanSteps = 5
	}
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
	if len(ss.s.CustomThemes) == 0 {
		ss.s.CustomThemes = defaultCustomThemes()
	}
	ss.s.Modules = normalizeModules(ss.s.Modules)
	if ss.s.PullSources == nil {
		ss.s.PullSources = []ragPullSource{}
	}
	for i := range ss.s.PullSources {
		ss.s.PullSources[i] = normalizePullSource(ss.s.PullSources[i])
	}
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
