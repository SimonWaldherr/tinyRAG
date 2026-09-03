package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// llmProfile is one named backend configuration. R3 is meant to run with
// exactly two profiles side by side — "local" (an OpenAI-compatible server:
// LM Studio, Ollama, vLLM, ...) for fast/cheap models, and "azure" (Azure
// OpenAI Service) for larger models — selectable per chat request via
// askRequest.Profile, while embeddings default to a single profile
// (normally "local", since embedding calls are frequent and latency/cost
// sensitive).
type llmProfile struct {
	Provider   string `json:"provider"`              // local, azure, openai, openrouter, claude or gemini
	BaseURL    string `json:"base_url"`              // provider endpoint; Azure uses its resource URL
	APIVersion string `json:"api_version,omitempty"` // Azure only, e.g. "2024-10-21"
	ChatModel  string `json:"chat_model"`            // provider model id; Azure uses a deployment name
	EmbedModel string `json:"embed_model"`           // local LM-Studio model; cloud profiles are chat-only
	APIKey     string `json:"api_key,omitempty"`     // inline key (dev convenience; prefer APIKeyEnv)
	APIKeyEnv  string `json:"api_key_env,omitempty"` // env var name holding the key, e.g. "AZURE_OPENAI_API_KEY"
}

// uploadConfig controls how Chat/Agent-uploaded images (chatimages.go)
// are processed — a deliberate, explicit admin choice rather than a
// per-profile capability guess: R3 has no reliable way to detect from a
// model *name* whether it actually accepts image_url content parts, so
// the admin picks one of exactly two policies instead.
type uploadConfig struct {
	// ImageMode is "vision" or "ocr" ("" behaves like "ocr" — the safer
	// default, since it never risks routing a request to a paid backend
	// the admin didn't ask for). "vision": an attached image routes the
	// WHOLE request to VisionProfile, overriding whatever profile the
	// caller/UI selected — see handleAsk. "ocr": images are never sent to
	// any LLM; extractImageTextOCR (extract.go, tesseract) turns them
	// into plain text instead, folded into the question.
	ImageMode string `json:"image_mode,omitempty"`
	// VisionProfile names which configured chat profile handles image input.
	// actually receives the image when ImageMode is "vision" —
	// deliberately independent of the profile the request would otherwise
	// use, since the vision-capable backend (typically Azure) often isn't
	// the same one used for everyday text chat. If ImageMode is "vision"
	// but this is empty (not yet configured), an attached image is
	// dropped with a warning rather than guessing a backend.
	VisionProfile string `json:"vision_profile,omitempty"`
	// VisionMaxDim caps a vision-routed image's longest side in pixels
	// before it's downscaled (chatimages.go's downscaleForVision) — 0
	// means "use the built-in default" (see effectiveVisionMaxDim),
	// which is also what a settings.json predating this field resolves
	// to. Admin-configurable within [800,1600]: below ~800 a scanned
	// document starts losing legibility, above 1600 a vision model reads
	// it no better, it just costs more.
	VisionMaxDim int `json:"vision_max_dim,omitempty"`
	// VisionJPEGQuality is the JPEG quality (1-100) a downscaled vision
	// image is re-encoded at. 0 means "use the built-in default" (see
	// effectiveVisionJPEGQuality). Admin-configurable within [50,95].
	VisionJPEGQuality int `json:"vision_jpeg_quality,omitempty"`
	// MaxAttachmentMB caps how large a single Chat/Agent attachment (image
	// or document, askRequest.Images) may be, in megabytes, before base64
	// inflation — 0 means "use the built-in default" (see
	// effectiveMaxAttachmentMB), 8 MB, which is also what a settings.json
	// predating this field resolves to. Admin-configurable within [1,50].
	MaxAttachmentMB int `json:"max_attachment_mb,omitempty"`
	// MaxPromptChars caps how long a single Chat/Agent question
	// (askRequest.Question) may be, in characters — 0 means "use the
	// built-in default" (see effectiveMaxPromptChars), 20000, which is
	// also what a settings.json predating this field resolves to. Guards
	// against an accidental (or deliberate) huge paste inflating
	// retrieval/LLM cost, not against a genuinely long but reasonable
	// question. Admin-configurable within [2000,100000].
	MaxPromptChars int `json:"max_prompt_chars,omitempty"`
}

// appSettings holds persisted configuration for R3.
type appSettings struct {
	Version int `json:"version"`
	// Lang selects the frontend's UI locale (see web/i18n.js's MESSAGES
	// dictionary) — sent to the browser via GET /api/settings and
	// /api/auth/status and read on load. supportedUILangs below is the
	// only other place this list needs to stay in sync; adding a further
	// locale means populating MESSAGES[locale], adding it to the Settings
	// language picker, and adding it there too.
	Lang string `json:"lang"`

	Profiles struct {
		Local      llmProfile `json:"local"`
		Azure      llmProfile `json:"azure"`
		OpenAI     llmProfile `json:"openai"`
		OpenRouter llmProfile `json:"openrouter"`
		Claude     llmProfile `json:"claude"`
		Gemini     llmProfile `json:"gemini"`
	} `json:"profiles"`
	// EmbedProfile is retained for settings-file compatibility and is always
	// normalized to "local"; cloud profiles are chat-only.
	EmbedProfile string `json:"embed_profile"`
	// ChatProfile is the default chat backend used when a /api/ask request
	// doesn't explicitly request one (see askRequest.Profile in handlers.go).
	ChatProfile string `json:"chat_profile"`

	ChunkSize int `json:"chunk_size"`
	K         int `json:"k"` // final number of chunks handed to the LLM as context

	// HistoryMaxTurns caps how many prior conversation turns (askHistoryTurn,
	// handlers.go) /api/ask ever honors, regardless of how many the client
	// sends — bounds prompt size/cost and stops a pathological client from
	// ballooning every request indefinitely. Also caps how much history
	// query_rewrite.go's rewriteQueryForRetrieval sees, since it reuses the
	// same already-capped conversion. 0 = built-in default
	// (askHistoryMaxDefault, handlers.go, currently 12). Raise for a cheap/
	// self-hosted large-context model wanting deeper multi-turn continuity
	// and better query-rewrite grounding; lower to cap prompt cost on a
	// metered cloud API across long conversations. IMPORTANT: web/app.js's
	// own client-side SESSION_HISTORY_MAX mirror reads this same resolved
	// value via /api/auth/status — raising it here without that wiring
	// would be a silent no-op, since the browser would keep trimming
	// history to the old cap before the server ever sees more.
	HistoryMaxTurns int `json:"history_max_turns,omitempty"`

	RedactPII bool `json:"redact_pii"`

	// DisableStreaming turns off token-by-token streaming in /api/ask,
	// falling back to one buffered response. Named as a negative (rather
	// than "StreamingEnabled") so streaming stays on by default even for
	// settings.json files written before this field existed — Go's JSON
	// unmarshal leaves an absent bool at its zero value (false), and false
	// here means "don't disable it", i.e. streaming stays on with no
	// migration code needed. Once the tool runs unattended against
	// Exchange (draft prep in the background, no UI watching), this can be
	// set true — there's no one there to see tokens arrive.
	DisableStreaming bool `json:"disable_streaming"`

	// Render configures where the Chat/Agent UI loads the optional
	// client-side rendering libraries (Mermaid diagrams, d3.js charts) from —
	// see renderConfig. Zero value means "use the built-in CDN defaults"
	// (resolveRenderConfig), so an upgrade needs no migration.
	Render renderConfig `json:"render"`

	// Storage configures the vectorStore backend (see vectorstore.go).
	// Defaults to tinySQL in "hybrid" mode — incremental save() instead of
	// a full-database snapshot on every ingested source (see
	// docs/VECTOR_DB.md for why that distinction matters for import time).
	Storage storageSettings `json:"storage"`

	// Ranking controls the hybrid "ranked RAG" retrieval scoring. Scores are
	// combined as: VectorWeight*cosine + KeywordWeight*bm25lite + RecencyWeight*recency.
	Ranking rankingConfig `json:"ranking"`

	// Import holds defaults for the generic file/PST importer.
	Import importConfig `json:"import"`

	// Upload controls how Chat/Agent-uploaded images are processed —
	// vision LLM vs. local OCR. See uploadConfig's own doc comment.
	Upload uploadConfig `json:"upload"`

	// AllowShellExec must be true before extract.go is allowed to shell out
	// to the markitdown CLI for binary office formats. Default false so a
	// fresh checkout never executes external binaries without opt-in.
	AllowShellExec bool `json:"allow_shell_exec"`

	// PromptsDir holds index.md (the global system prompt) and any number
	// of skill_*.md files (domain-specific instructions), managed via the
	// admin "Prompts" tab — see skills.go. Missing entirely is fine: R3
	// falls back to a built-in default system prompt with no skills.
	PromptsDir string `json:"prompts_dir"`

	// AdminPasswordEnv names the environment variable holding the shared
	// admin password. Plays two different roles depending on
	// settings.LDAP.Enabled:
	//   - LDAP disabled: purely a UI-visibility convenience (see
	//     handleAdminCheck) — admin-only API endpoints are NOT protected
	//     by it, R3's original documented access-control gap.
	//   - LDAP enabled: this same password becomes a real break-glass
	//     admin credential (see handleLDAPLogin) — submitting it via the
	//     login form issues an actual admin session without touching
	//     LDAP at all, so admins can still administer R3 if AD/LDAP is
	//     unreachable or misconfigured. Set this env var to a real,
	//     private value before enabling LDAP if that fallback should
	//     exist; leave it unset to disable the fallback entirely (LDAP
	//     becomes the only way in, and a broken LDAP locks everyone out
	//     of admin routes).
	// If the env var isn't set, the legacy UI gate always succeeds
	// (equivalent to no gate configured yet) and the break-glass login
	// path is simply unavailable.
	AdminPasswordEnv string `json:"admin_password_env"`

	// URLMappings maps local path prefixes (source_id prefixes) to web URLs
	// so citation chips in the UI link directly to the original document,
	// e.g. on a SharePoint, Intranet, or file server. Each mapping is
	// checked in order; the first prefix match wins.
	// Example: {"prefix": "C:\\Freigaben\\", "url_prefix": "https://intranet.rubix.com/docs/"}
	URLMappings []urlMapping `json:"url_mappings"`

	// EnableDraftReplies gates two narrower things, NOT the public "Mail"
	// nav tab's core "compute and show a draft" action — that's always
	// available, same trust level as /api/ask (read-only LLM+knowledge-base
	// call, no IMAP/SMTP/Exchange touched — see handleDraftReply). This flag
	// instead controls: (1) the "Antwortentwurf erstellen" button in the
	// PST source popup (a UI-declutter toggle — see handleSourceContent),
	// and (2) whether the Agent tab's mail-draft tool is offered. Off by
	// default since those two are extra surface area, not something every
	// deployment necessarily wants exposed. composeDraftReply (draft.go)
	// itself has existed since before this setting. Always HITL — see
	// draft.go's package comment: R3 never sends anything, only proposes
	// text for a human to review.
	EnableDraftReplies bool `json:"enable_draft_replies"`
	// DraftChatProfile selects which backend drafts replies ("local" or
	// "azure"); empty means "use ChatProfile", matching how askRequest's
	// per-question profile override defaults.
	DraftChatProfile string `json:"draft_chat_profile"`
	// DraftPreset names the appSettings.Presets entry the Mail tab's draft
	// generation is restricted to (source_kind + tool scope) — admin-fixed,
	// no per-request choice, same "narrows SourceAccess, never widens it"
	// rule as every other preset use. Empty = no restriction beyond
	// SourceAccess (matches today's behavior before this setting existed).
	DraftPreset string `json:"draft_preset,omitempty"`
	// DraftMaxToolRounds overrides the Mail tab's tool-round budget
	// independently of agent.max_tool_rounds (the Agent tab's own budget,
	// see agentMaxRounds) — 0 (default) falls back to reusing that same
	// value, matching behavior before this setting existed. A reply draft
	// is typically a narrower, more one-shot task than an open-ended Agent
	// tab conversation ("search, refine, read one source, write the reply"
	// vs. a multi-step investigation), so a deployment that wants Mail
	// drafts to come back faster/cheaper can lower this without also
	// shrinking the Agent tab's budget; conversely a deployment leaning on
	// Mail's Shop/MSSQL/HTTP tool access for genuinely multi-step lookups
	// can raise it independently too. See draftMaxToolRounds (draft.go).
	DraftMaxToolRounds int `json:"draft_max_tool_rounds,omitempty"`

	// EnableChatHistory gates the sidebar's "Verlauf" (history) button and
	// its underlying persistence — off by default, matching every other
	// opt-in feature above. This setting only controls whether the UI
	// offers the capability at all; *where* a conversation is actually
	// stored depends on whether the caller is logged in (see
	// chathistory.go):
	//   - logged in (requires LDAP.Enabled) — stored server-side in
	//     ChatHistoryPath below, keyed by the session's AD username, so
	//     it follows that person across browsers/devices. Every read/
	//     write is filtered by that username; one person's conversations
	//     are never returned for another's session (chatHistoryStore's
	//     methods all take an owner argument and enforce it in SQL, not
	//     just in application code).
	//   - not logged in, or LDAP.Enabled is off entirely — exactly the
	//     original behavior: kept entirely in the browser's own
	//     localStorage (web/app.js's chat-history functions), scoped to
	//     that one browser/device, never sent to R3 at all.
	// Because it's ungated (visible via /api/auth/status) rather than a
	// secret, non-admin chat visitors can see whether it's on without
	// needing settings access.
	EnableChatHistory bool `json:"enable_chat_history"`
	// ChatHistoryPath is the SQLite file server-side chat history (the
	// logged-in case above) is stored in — independent of
	// storageSettings.Path, which is unrelated chunk/vector data, not
	// conversations.
	ChatHistoryPath string `json:"chat_history_path"`
	// TokenUsagePath is the SQLite file per-user, per-service token-usage
	// records (tokenusage.go) are stored in — same independence reasoning
	// as ChatHistoryPath above, just a different, unrelated concern.
	TokenUsagePath string `json:"token_usage_path"`
	// UserPrefsPath is the SQLite file per-user preference overrides
	// (userprefs.go — today just a personal Lang override on top of this
	// struct's own Lang default) are stored in — same independence
	// reasoning as ChatHistoryPath above.
	UserPrefsPath string `json:"user_prefs_path"`

	// LDAP configures real Active Directory-backed login (see ldapauth.go,
	// session.go), replacing the shared-password UI gate (handleAdminCheck)
	// with per-user AD credentials plus real server-side session
	// enforcement on admin routes — the gap both README.md and
	// docs/HARDENING_PLAN.md flag as R3's known access-control limitation.
	// Off by default: when disabled, R3 behaves exactly as before (the
	// admin tabs are only UI-hidden, not actually protected) so existing
	// deployments aren't forced onto LDAP. Flip Enabled once URL/BaseDN/
	// RequiredGroupDN are configured.
	LDAP ldapConfig `json:"ldap"`

	// LocalAuth configures individually-provisioned local accounts
	// (localusers.go) — an alternative and complement to LDAP: useful for
	// people without an AD account (external partners, contractors, test
	// logins) or for deployments that don't run AD/LDAP at all. A local
	// account and an AD account can both exist side by side; handleLDAPLogin
	// tries the local user store first (by username), then falls back to
	// LDAP if enabled. Off by default, same "opt-in, unchanged behavior
	// otherwise" convention as LDAP above. Enabling either LDAP or LocalAuth
	// is what turns admin routes from UI-hidden into actually
	// server-enforced — see authTierActive.
	LocalAuth localAuthConfig `json:"local_auth"`

	// SharePoint configures Microsoft Graph API access for importing
	// documents directly from a SharePoint site/folder (see sharepoint.go).
	// A list so several sites can be imported side by side, each with its
	// own credentials/cycle/limit/timeout (connRuntime) — empty means
	// nothing configured yet, same as the old zero-value single struct.
	SharePoint []sharePointConfig `json:"sharepoint"`

	// OneDrive configures imports from explicitly selected Microsoft 365
	// business drives via Microsoft Graph. Unlike SharePoint this is a
	// drive-level connection (DriveID), so personal/team drives can be kept
	// separate and each gets its own delta cursor and schedule.
	OneDrive []oneDriveConfig `json:"onedrive"`

	// ExchangeGraph configures reading shared Exchange Online mailboxes via
	// Microsoft Graph (see graphmail.go) — the Microsoft-365 path for
	// Outlook/Exchange import, one entry per mailbox. For on-prem Exchange
	// or generic IMAP mailboxes, see IMAP below instead.
	ExchangeGraph []exchangeGraphConfig `json:"exchange_graph"`

	// IMAP configures reading mailboxes via plain IMAP (see imap.go) —
	// works with on-prem Exchange and Microsoft 365 alike, using its own
	// credentials rather than an Azure AD app registration, one entry per
	// mailbox.
	IMAP []mailboxConfig `json:"imap"`

	// Teams configures importing Microsoft Teams channels' message
	// history via Graph (see teams.go), one entry per channel.
	Teams []teamsConfig `json:"teams"`

	// Confluence configures importing pages from Atlassian Confluence
	// spaces (see confluence.go), one entry per space.
	Confluence []confluenceConfig `json:"confluence"`

	// Jira configures importing issues from Atlassian Jira Cloud projects
	// (see jira.go), one entry per project — same account email + API
	// token auth model as Confluence above (often the very same Atlassian
	// account/token, since both are scoped by whatever that account can
	// read).
	Jira []jiraConfig `json:"jira"`

	// Freshservice configures importing tickets from Freshservice
	// instances (see freshservice.go), one entry per instance — optionally
	// kept in sync automatically by the scheduler (scheduler.go) via each
	// entry's SyncIntervalMinutes.
	Freshservice []freshserviceConfig `json:"freshservice"`

	// Folder watches one or more server-local directories, re-scanning
	// (recursively) on a cycle to pick up new/changed files — the scheduled
	// counterpart to the one-shot "Server-Ordner importieren" form in the
	// Import tab (ingestFolder/handleImportFolder), which stays available
	// for quick ad-hoc imports. No credentials: R3 reads with whatever
	// filesystem access its own process already has.
	Folder []folderConfig `json:"folder"`

	// GitHub imports the README plus Issues and Pull Requests from one
	// repository (GitHub.com or GitHub Enterprise), using a fine-grained
	// read-only token.
	GitHub []githubConfig `json:"github"`

	// SAPS4 imports one explicitly configured, read-only OData entity set
	// from SAP S/4HANA. The selected field list prevents the importer from
	// collecting an entire business object when only a few fields are useful
	// to the knowledge base.
	SAPS4 []sapS4Config `json:"sap_s4"`

	// SMTP configures R3's own outbound mail (see mail.go) — currently only
	// the chat "send this answer to me" feature (handlers.go's
	// handleChatEmail).
	SMTP smtpConfig `json:"smtp"`

	// MSSQL configures a live, read-only SQL Server connection the chat
	// model can query at answer time via OpenAI-style tool/function
	// calling (see mssql.go) — unlike every other connector above, this
	// isn't an import source: nothing from it is embedded or stored, each
	// query runs fresh against the live database when the model asks for
	// one. Requires a chat model/backend that actually supports tool
	// calling; many small local models don't, in which case this setting
	// simply has no effect (the model never gets called with a tools
	// schema).
	MSSQL mssqlConfig `json:"mssql"`

	// Shop configures live access to Rubix's own B2B online shop
	// (de.rubix.com) as a chat/agent/mail tool (see shop.go) — same
	// "live query at answer time, nothing embedded/stored" model as MSSQL
	// above, just an HTTP+token-auth backend instead of a database.
	Shop shopConfig `json:"shop"`

	// HTTPTemplates are admin-curated, named, parameterized HTTP GET
	// requests against an already-configured connector's own credentials
	// (Confluence, Jira, Freshservice) — the generic, MCP-style analogue
	// of MSSQL.QueryTemplates for REST APIs (see http_tool.go and
	// docs/MCP_CONNECTORS_PLAN.md section (A)): a new live lookup means
	// one admin-authored template, not a new Go type/tool per connector.
	// Each enabled template becomes its own named tool, gated the same way
	// as MSSQL/Shop above (Settings, Preset's Tools list "http").
	HTTPTemplates []httpQueryTemplate `json:"http_templates,omitempty"`

	// RESTConnectors are admin-configured generic REST backends (base URL +
	// auth scheme + optional static headers) that an HTTP query template can
	// reference by name via its auth_source — the generalization of the
	// built-in confluence/jira/freshservice auth sources to arbitrary
	// internal or third-party systems (e.g. an SAP se16 gateway at
	// https://logistic.rubix-intern.de). Unlike the import connectors above,
	// a REST connector stores nothing: it only supplies where a template may
	// call and how it authenticates. See restConnectorConfig and http_tool.go
	// (restConnectorByName / applyHTTPTemplateAuth), plus docs/CONNECTORS.md.
	RESTConnectors []restConnectorConfig `json:"rest_connectors,omitempty"`

	// Agent configures the Agent tab's extra capabilities beyond plain
	// chat — see agent.go and docs/AGENT_PLAN.md. Zero-value = everything
	// risky off, matching every other opt-in gate in this file.
	Agent agentConfig `json:"agent"`

	// ToolRouter configures the optional pre-flight tool-routing LLM call
	// (tool_router.go) shared by Chat, Agent, and Mail alike — deliberately
	// its own top-level block rather than part of agentConfig, since it
	// applies to all three surfaces, not just the Agent tab. Zero-value =
	// disabled: every request behaves exactly as before (the model still
	// decides tool use natively, mid-answer, same as today) until an admin
	// opts in to the extra LLM round-trip.
	ToolRouter toolRouterConfig `json:"tool_router"`

	// QueryRewrite configures the optional pre-flight conversation-aware
	// retrieval-query rewrite (query_rewrite.go) — Chat's /api/ask only
	// (Agent/Mail don't take a follow-up-question History the same way).
	// Same "own top-level block, zero-value disabled" shape as ToolRouter
	// above. Retrieval (rankedSearch) otherwise only ever sees the raw
	// current question, never the conversation history, so a follow-up
	// like "und bei Kistenpfennig?" searches on exactly that fragment
	// unless this is enabled.
	QueryRewrite queryRewriteConfig `json:"query_rewrite"`

	// API configures external (non-browser) access to /api/ask and
	// /api/search via API keys (see apikey.go/handlers.go's
	// requireAPIKey). Off by default, matching every other opt-in gate in
	// this file — a fresh checkout behaves exactly as before until
	// explicitly turned on.
	API apiConfig `json:"api"`

	// OpenAIAPI exposes R3 itself as one or more OpenAI-compatible chat
	// completions servers (see openai_api.go) on one shared port, separate
	// from the main UI/API port — for other tools (Open WebUI, IDE
	// assistants, other agents) to use R3 as a drop-in OpenAI-style backend.
	// Off by default; see openAIAPIConfig's own doc comment for the
	// multi-endpoint/capability model.
	OpenAIAPI openAIAPIConfig `json:"openai_api"`

	// SourceVisibility controls, per source_kind (e.g. "pst_email",
	// "sharepoint_file" — the same strings ingestDocument's callers pass,
	// see the source_kind column), whether that kind's chunks are shown to
	// the user as a citation once retrieved. A kind absent from this map is
	// visible (the pre-existing behavior) — this is opt-out, not opt-in,
	// so upgrading never silently hides citations. Setting a kind to false
	// still lets its chunks ground the answer (rankedSearch/assembleContext
	// don't consult this map at all); only the citations ultimately
	// returned to the client are filtered (see handleAsk's use of
	// filterCitations in rank.go) — the intended use is privacy-sensitive
	// kinds like "pst_email" that should inform an answer without exposing
	// the underlying mailbox content as a clickable/named source.
	SourceVisibility map[string]bool `json:"source_visibility,omitempty"`

	// SourceAccess restricts retrieval itself — not just citation display,
	// contrast with SourceVisibility above — to requesters whose AD
	// department/title classifies (see department.go's classifyDepartment)
	// into one of the codes listed for that source_kind. A kind absent
	// from this map, or mapped to an empty list, is unrestricted:
	// retrievable by anonymous visitors and every logged-in user alike —
	// the same opt-out-not-opt-in shape as SourceVisibility, so upgrading
	// never silently locks out existing public content. Restricted chunks
	// are excluded before ranking even starts (see rank.go's
	// filterByDeptAccess), so a denied chunk never reaches the LLM's
	// context at all — this is real access control, unlike
	// SourceVisibility. Requires settings.LDAP.Enabled to have any effect
	// for logged-in users; without a session, every caller is treated as
	// department "" (matches nothing an admin configured here, so only
	// unrestricted kinds are ever visible).
	SourceAccess map[string][]string `json:"source_access,omitempty"`

	// Presets are named, admin-curated bundles restricting which
	// source_kind values and live-tool categories ("mssql", "shop") a
	// given use case may draw on — a second, orthogonal axis on top of
	// SourceAccess above: SourceAccess is about WHO (department), Presets
	// are about WHAT FOR (use case: a Chat question, the Agent tab, a Mail
	// draft). The two always apply together as an intersection — a preset
	// can only narrow what SourceAccess already allows for the requester's
	// department, never widen it. Chat lets the asking person pick a
	// preset per request (askRequest.Preset); Agent and Mail each use one
	// admin-fixed preset instead (agentConfig.DefaultPreset, DraftPreset
	// below) — see preset.go for the actual filtering.
	Presets []sourcePreset `json:"presets,omitempty"`

	// PersonalizeAnswers, when true, has handleAsk prepend a short
	// "who's asking" block (name/department/title/office, from the LDAP
	// session — see session.go/userContextBlock in handlers.go) to the
	// system prompt so answers can be tailored in tone/register. Off by
	// default: a deployment may want SourceAccess restrictions without
	// every answer opening with "Hallo Herr Müller aus dem Vertrieb." A
	// no-op for anonymous requests regardless of this setting — there's
	// no session to draw from.
	PersonalizeAnswers bool `json:"personalize_answers"`
}

// adminDeptCode is a sentinel deptCode value meaning "unrestricted" —
// resolveDeptCode returns it for admin sessions so sourceAccessAllowed
// bypasses every department restriction. Admins already see every chunk
// unfiltered via the Chunks tab (chunks.go's handleChunks has no
// access/deptCode parameter at all), so gating retrieval or direct
// source access behind a department code would just be a narrower,
// inconsistent version of access they already have in full elsewhere.
const adminDeptCode = "*"

// resolveDeptCode is the single place that decides whether a session's
// department restrictions apply: admins get adminDeptCode (bypasses
// every check in sourceAccessAllowed), everyone else gets their own
// department code unchanged.
func resolveDeptCode(isAdmin bool, deptCode string) string {
	if isAdmin {
		return adminDeptCode
	}
	return deptCode
}

// sourceAccessAllowed reports whether source_kind's chunks may be
// retrieved at all for a requester classified as deptCode ("" for an
// anonymous visitor, "default" for a logged-in user department.go
// couldn't classify, adminDeptCode for an admin session — always
// allowed) — see appSettings.SourceAccess's doc comment for why this is
// real access control, not just citation filtering.
func sourceAccessAllowed(access map[string][]string, kind, deptCode string) bool {
	if deptCode == adminDeptCode {
		return true
	}
	allowed, ok := access[kind]
	if !ok || len(allowed) == 0 {
		return true
	}
	for _, code := range allowed {
		if strings.EqualFold(code, deptCode) {
			return true
		}
	}
	return false
}

// sourcePreset is one named, admin-curated entry in appSettings.Presets —
// see that field's doc comment for the "orthogonal to SourceAccess" design.
// Kinds/Tools use the same "absent/empty = unrestricted" convention as
// SourceAccess (see sourceAccessAllowed above), checked by preset.go's
// presetAllowsKind/presetAllowsTool.
type sourcePreset struct {
	// Name is the stable identifier referenced by agentConfig.DefaultPreset,
	// appSettings.DraftPreset, and askRequest.Preset — not shown to the user.
	Name string `json:"name"`
	// DisplayName is what the Chat preset dropdown actually shows.
	DisplayName string `json:"display_name"`
	// Kinds lists the allowed source_kind values (pst_email, jira_issue,
	// file, ...). Empty = no restriction on this axis.
	Kinds []string `json:"kinds,omitempty"`
	// Tools lists allowed tool categories: "mssql", "shop" — coarse
	// categories, not individual MSSQL query-template names. Empty = no
	// restriction on this axis.
	Tools []string `json:"tools,omitempty"`
}

// validatePresets rejects a blank or duplicate preset Name before it's
// saved. Every other complex settings field (query templates, REST
// connectors, storage, tool router, query rewrite, OpenAI endpoints) has a
// dedicated validate*() in handleSettings' chain — Presets previously had
// none, so a JSON-key typo inside one preset object (e.g. "kind" instead of
// "kinds") saved silently as valid JSON with an empty Kinds/Tools list,
// which per this struct's own "empty = unrestricted" convention makes that
// preset grant access to EVERYTHING instead of the admin's intended
// restricted subset — a silent security downgrade with no error anywhere.
// This can't catch that specific typo (Kinds/Tools have no fixed known-
// values list to check against — every connector/tool invents its own
// source_kind/category string, so validating against a hardcoded list here
// would itself go stale), but it does catch the other way presets silently
// misbehave: a blank Name can never be referenced by
// agentConfig.DefaultPreset/DraftPreset/askRequest.Preset at all, and two
// presets sharing a Name make findPreset's lookup (preset.go) return
// whichever happens to come first, silently discarding the other.
func validatePresets(presets []sourcePreset) error {
	seen := make(map[string]bool, len(presets))
	for i, p := range presets {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("preset #%d: name darf nicht leer sein", i+1)
		}
		if seen[strings.ToLower(name)] {
			return fmt.Errorf("preset %q: name ist bereits vergeben (Namen müssen eindeutig sein)", p.Name)
		}
		seen[strings.ToLower(name)] = true
	}
	return nil
}

type apiConfig struct {
	RequireAPIKey bool           `json:"require_api_key"`
	Keys          []apiKeyRecord `json:"keys,omitempty"`
	// GuestAskRateLimitPerMinute bounds how many /api/ask requests per
	// minute one anonymous (no valid session) caller may make — a
	// logged-in session is exempt, matching the existing "authenticated
	// callers are trusted more" pattern elsewhere in this file. 0 (the
	// zero value, so existing settings.json files keep today's behavior)
	// disables the limit entirely. See docs/TODO.md C6 — guards against
	// unbounded LLM-cost exposure from anonymous callers, e.g. once the
	// server is reachable more broadly than just the admin's own browser.
	GuestAskRateLimitPerMinute int `json:"guest_ask_rate_limit_per_minute,omitempty"`
	// GuestVoiceRateLimitPerMinute is GuestAskRateLimitPerMinute's
	// counterpart for /api/voice/transcribe — a request here is heavier
	// than a plain /api/ask call (it spawns an external ffmpeg/whisper.cpp
	// process, see whisper.go), so an anonymous caller left unbounded can
	// exhaust host CPU even faster. Same "0 = no limit, logged-in callers
	// exempt" convention. Independent of WhisperMaxConcurrent
	// (importConfig) — that caps total server-wide concurrency regardless
	// of caller, this caps one caller's own request rate.
	GuestVoiceRateLimitPerMinute int `json:"guest_voice_rate_limit_per_minute,omitempty"`
}

// openAIEndpointConfig is one named OpenAI-compatible endpoint exposed on
// R3's dedicated external API port (openAIAPIConfig.Port) — the mechanism
// behind "verschiedene Endpoints anbieten": different external tools often
// need different capability combinations from the very same request shape
// (a plain OpenAI chat completion), so each endpoint independently picks
// which of R3's own capabilities feed into it, entirely transparently to
// the caller (which only ever sees a normal chat completion response,
// never which internal capability produced it):
//
//   - EnableRAG=false, EnableTools=false: a bare LLM passthrough — R3
//     injects no system prompt/context of its own and forwards the
//     caller's own system/developer message unchanged. "Manche Tools
//     sollen einfach nur das konfigurierte Default-LLM nutzen" — leave
//     Profile empty too, and it uses appSettings.ChatProfile (Azure, in
//     this deployment), exactly like the bundled Chat tab's own default.
//   - EnableRAG=true, EnableTools=false: R3's normal ranked-retrieval
//     context (scoped by Preset) is folded into the system prompt before
//     answering, same grounding the Chat tab gives — but no tools.
//   - EnableRAG=false, EnableTools=true: no retrieval/context, but R3's
//     live tools (MSSQL/Shop/HTTP templates, scoped by Preset's Tools
//     list) are offered and resolved server-side (see MaxToolRounds).
//   - EnableRAG=true, EnableTools=true: both together — the original,
//     only-ever-existed-until-now behavior.
//
// Each named endpoint is mounted at its own URL path prefix
// ("/"+Name+"/v1/chat/completions", "/"+Name+"/v1/models") on the shared
// port, so a dozen partner tools can each get their own tailored endpoint
// without a dozen firewall rules — one rule for the port still isolates
// the whole surface from Import/Settings/admin routes exactly as before.
// Name "" is the one allowed exception: it mounts at the bare, unprefixed
// "/v1/..." paths (see reconcileOpenAIAPIServer), so a single-endpoint
// deployment — including one migrated from the pre-multi-endpoint shape,
// see migrateLegacyOpenAIAPIConfig — needs no path prefix at all.
type openAIEndpointConfig struct {
	// Name identifies this endpoint (unique, case-insensitive) and, unless
	// empty, becomes its URL path prefix. Validated at save time
	// (openai_api.go's validateOpenAIEndpoints/openAIEndpointNameRe): empty
	// (the unprefixed root — at most one endpoint may claim it) or
	// letters/digits/underscore/hyphen starting with a letter.
	Name string `json:"name"`
	// Enabled lets an endpoint be defined but temporarily withheld without
	// deleting its configuration — same convention as every other named
	// list in this file (connRuntime.connName's siblings).
	Enabled bool `json:"enabled"`
	// EnableRAG folds R3's own ranked-retrieval context into the system
	// prompt for requests through this endpoint — see the type doc comment
	// above for the full capability matrix this combines with EnableTools
	// into.
	EnableRAG bool `json:"enable_rag"`
	// EnableTools offers this endpoint R3's configured live tools
	// (MSSQL/Shop/HTTP templates), scoped by Preset's Tools list, exactly
	// like buildLiveTools already does for Chat/Agent/Mail — the same
	// function is reused here, so a live tool enabled once is
	// automatically available to every surface that wants it.
	EnableTools bool `json:"enable_tools"`
	// MaxToolRounds bounds how many server-side tool-calling round-trips
	// (llm.go's chatWithToolsBudget) one request through this endpoint may
	// use before the final answer is forced — ignored when EnableTools is
	// false. 0 (default) means exactly one round, the original behavior;
	// set higher to let an external caller's simple chat client benefit
	// from the same multi-step tool use the Agent tab gets, without that
	// caller needing its own tool-execution loop.
	MaxToolRounds int `json:"max_tool_rounds,omitempty"`
	// Profile pins this endpoint to one configured chat backend ("local",
	// "azure", "openai", "openrouter", "claude" or "gemini"), ignoring
	// whatever "model" a caller's request sends — useful when a partner
	// tool must never accidentally reach a paid or otherwise
	// not-approved-for-that-tool backend. Empty (the common case) falls
	// back to the caller's own "model" field, and from there to
	// appSettings.ChatProfile (Azure, in this deployment) exactly like
	// Chat's own askRequest.Profile — "einfach nur das konfigurierte
	// Default-LLM nutzen" needs no Profile set at all.
	Profile string `json:"profile,omitempty"`
	// Preset names the appSettings.Presets entry this endpoint is
	// restricted to (source_kind + tool scope) — admin-fixed, no
	// per-request choice, same rule as agentConfig.DefaultPreset/
	// appSettings.DraftPreset. Empty means no restriction beyond
	// SourceAccess. Meaningless when both EnableRAG and EnableTools are
	// false (a bare LLM passthrough draws on neither axis).
	Preset string `json:"preset,omitempty"`
}

// openAIAPIConfig configures R3's OpenAI-compatible server surface
// (openai_api.go): one or more named Endpoints (openAIEndpointConfig,
// see its own doc comment for the per-endpoint capability model), all
// sharing one dedicated TCP port so a single firewall rule keeps this
// whole external surface isolated from Import/Settings/admin routes.
type openAIAPIConfig struct {
	Enabled bool `json:"enabled"`
	// Port is a dedicated TCP port for this server, separate from the
	// main UI/API port (-addr) — e.g. so a firewall rule can expose only
	// this port to a partner tool without also exposing Import/Settings/
	// admin routes. 0 (default) means the server never starts, even if
	// Enabled is true — a deliberate two-key requirement (like
	// agent.allow_code_execution's setting+build gate) against an
	// accidental bind-to-something merely from Enabled being left on.
	Port int `json:"port,omitempty"`
	// Endpoints is the list of named, independently-capable endpoints
	// this server exposes (see openAIEndpointConfig) — empty means the
	// server never starts even if Enabled and Port are set (nothing to
	// serve). Validated at save time (validateOpenAIEndpoints).
	Endpoints []openAIEndpointConfig `json:"endpoints,omitempty"`
}

// agentConfig gates the Agent tab's tool set (agent.go). The knowledge-
// base tools (search/read/list) are always on — they expose nothing a
// plain /api/ask couldn't already retrieve, filtered by the same
// source_access rules. Everything with more reach is opt-in here.
type agentConfig struct {
	// AllowCodeExecution offers the run_code tool to the agent — and even
	// then only once a real sandbox implementation is compiled in
	// (agent.go's activeCodeSandbox; planned: nanoGo inside a wazero WASM
	// wall, see docs/AGENT_PLAN.md section D). Default false, and with no
	// sandbox present this flag alone changes nothing — defense in depth:
	// both the operator AND the build must opt in.
	AllowCodeExecution bool `json:"allow_code_execution"`
	// MaxToolRounds caps how many tool-calling round-trips one agent
	// question may use before the answer is forced (llm.go's
	// chatWithToolsBudget) — the cost/latency/runaway ceiling. 0 means
	// the default (agentDefaultMaxRounds, agent.go).
	MaxToolRounds int `json:"max_tool_rounds,omitempty"`
	// DefaultPreset names the appSettings.Presets entry the Agent tier
	// (tier.go) is restricted to (source_kind + tool scope) — admin-fixed,
	// no per-request choice: handleAsk substitutes this for askRequest.Preset
	// whenever the resolved tier is "agent", ignoring whatever the caller's
	// own preset selector was set to. Empty = no restriction beyond
	// SourceAccess.
	DefaultPreset string `json:"default_preset,omitempty"`
	// AllowWebFetch offers the fetch_url tool to the agent — read-only,
	// stage 1 of docs/AGENT_PLAN.md section C (fetch-and-read only, never
	// fetch-and-ingest — that stays an admin-only Import-tab action).
	// Default false: unlike the knowledge-base tools, this lets the agent
	// reach arbitrary public URLs, which is new exposure (SSRF surface via
	// webimport.go's isSafeWebURL, prompt-injection surface via fetched
	// page content) even though both are mitigated, not eliminated.
	AllowWebFetch bool `json:"allow_web_fetch"`
	// SubagentsDisabled turns OFF the delegate_subtasks tool. Inverted (a
	// disable flag, not an allow flag) so the default zero value = ENABLED:
	// sub-agent orchestration is read-only work over tools the agent
	// already has, bounded by subAgentMaxTasks/subAgentRounds, and
	// sub-agents can't recurse, so it's on by default (also what the Demo
	// mode showcases). Set true to force a single linear tool loop. Unlike
	// AllowCodeExecution/AllowWebFetch (new outward exposure → opt-in), this
	// adds no new reach, so it's opt-out.
	SubagentsDisabled bool `json:"subagents_disabled"`
	// MaxSubtasks / SubagentRounds bound the delegate_subtasks fan-out: how
	// many sub-agents one delegate call may spawn, and each sub-agent's own
	// tool-round budget. 0 = built-in default (subAgentDefaultMaxTasks /
	// subAgentDefaultRounds, agent.go), each clamped to a hard ceiling so a
	// fat-fingered value can't turn the fan-out into a runaway.
	MaxSubtasks    int `json:"max_subtasks,omitempty"`
	SubagentRounds int `json:"subagent_rounds,omitempty"`
	// MaxConcurrency caps how many sub-agents run at once (a weighted
	// semaphore over the parallel delegate fan-out — the heaviest
	// concurrent load an agent question can put on the chat backend). 0 =
	// default (agentDefaultConcurrency). Same "pace ourselves, don't flood
	// the upstream" reasoning as the import throttle (import_limits.go),
	// applied to the agent's own parallelism.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// AllowWebResearch offers web_research: a goal-directed sub-agent that
	// fetches a start page, reads it, follows same-page links it judges
	// relevant, and keeps going until it finds the sought information or
	// its own budget (WebResearchRounds/WebResearchTimeoutSeconds) runs
	// out — then returns a synthesized summary, never raw pages, to the
	// caller (agent.go's webResearchToolExecutor). Requires AllowWebFetch
	// (this is strictly more capable/costly than single-page fetch_url,
	// not an independent toggle) — checked at registration, not just
	// implied by naming. Off by default, same "new outward exposure →
	// opt-in" posture as AllowWebFetch/AllowCodeExecution above.
	AllowWebResearch bool `json:"allow_web_research,omitempty"`
	// WebResearchRounds / WebResearchTimeoutSeconds bound one web_research
	// call — its own tool-round budget and a wall-clock ceiling
	// (llm.go's chatWithToolsBudgetDeadline), so a research sub-agent that
	// keeps finding "one more promising link" can't run away in either
	// dimension. 0 = built-in default (webResearchDefaultRounds /
	// webResearchDefaultTimeoutSeconds, agent.go), each clamped to a hard
	// ceiling like MaxSubtasks/SubagentRounds above.
	WebResearchRounds         int `json:"web_research_rounds,omitempty"`
	WebResearchTimeoutSeconds int `json:"web_research_timeout_seconds,omitempty"`

	// AllowWebSearch offers web_search: a plain text-query lookup against
	// a third-party search API (Tavily, websearch.go) that returns a short
	// list of candidate URLs+snippets — the "discovery" step
	// web_research/fetch_url never had (both require an already-known
	// start URL; see AllowWebResearch's doc comment above). Unlike
	// AllowWebFetch/AllowWebResearch, this never has R3 itself fetch an
	// arbitrary caller-influenced URL — no new SSRF surface on this server,
	// since the search provider does its own crawling on its own
	// infrastructure — so it's independently gated rather than layered on
	// top of AllowWebFetch. Off by default: it still sends the agent's
	// search query text to a third-party API, which an admin should
	// consciously accept, same posture as every other external tool here.
	AllowWebSearch bool `json:"allow_web_search,omitempty"`
	// WebSearchAPIKey/WebSearchAPIKeyEnv follow the same inline-value-vs-
	// env-var-name pattern as every other external credential in this file
	// (e.g. shopConfig.ClientSecret/ClientSecretEnv) — WebSearchAPIKeyEnv
	// (an env var *name*) takes precedence over the inline value when both
	// are set (resolveSecret).
	WebSearchAPIKey    string `json:"web_search_api_key,omitempty"`
	WebSearchAPIKeyEnv string `json:"web_search_api_key_env,omitempty"`
	// WebSearchMaxResults / WebSearchTimeoutSeconds bound one web_search
	// call: how many results the provider returns per query, and how long
	// R3 waits for its response. 0 = built-in default
	// (webSearchDefaultMaxResults / webSearchDefaultTimeoutSeconds,
	// websearch.go), each clamped to a hard ceiling like WebResearchRounds
	// above.
	WebSearchMaxResults     int `json:"web_search_max_results,omitempty"`
	WebSearchTimeoutSeconds int `json:"web_search_timeout_seconds,omitempty"`

	// AllowAzureBingSearch offers azure_bing_search
	// (azurebingsearch.go): a grounded web-search call via Azure OpenAI's
	// Responses API "web_search" tool (Grounding with Bing Search under
	// the hood) — unlike web_search (Tavily) it returns an already
	// synthesized, cited answer rather than raw results, since that's all
	// Azure's own API exposes. Reuses appSettings.Profiles.Azure's
	// existing BaseURL/APIKey/ChatModel — no separate credential set —
	// and is only ever offered when that profile is actually configured
	// (see buildAgentTools' gate, agent.go). Off by default: sends the
	// agent's query to Azure/Bing as a third-party call, same opt-in
	// posture as AllowWebSearch/AllowWebFetch above.
	AllowAzureBingSearch bool `json:"allow_azure_bing_search,omitempty"`
	// AzureBingSearchTimeoutSeconds bounds one azure_bing_search call. 0 =
	// built-in default (azureBingSearchDefaultTimeoutSeconds,
	// azurebingsearch.go), clamped to a hard ceiling like every other
	// external-tool timeout here.
	AzureBingSearchTimeoutSeconds int `json:"azure_bing_search_timeout_seconds,omitempty"`

	// ContextCompactionDisabled turns OFF long-agent-run context
	// compaction (llm.go's compactOldToolRounds) — same inverted "default
	// zero value = ENABLED" shape as SubagentsDisabled above, since
	// compaction only shortens already-consumed tool-result bodies from
	// completed older rounds (never the assistant's own tool_calls, never
	// the most recent rounds), it's a safety net against unbounded context
	// growth on a long multi-round (especially sub-agent-heavy) run, not a
	// new exposure to opt into.
	ContextCompactionDisabled bool `json:"context_compaction_disabled,omitempty"`
	// ContextCompactionThresholdChars / ContextCompactionKeepRounds tune
	// when compaction kicks in and how much recent history it always
	// leaves untouched. 0 = built-in default
	// (contextCompactionDefaultThresholdChars /
	// contextCompactionDefaultKeepRounds, llm.go), same "0 means default,
	// not disabled" convention as MaxSubtasks/SubagentRounds above — use
	// ContextCompactionDisabled to actually turn it off.
	ContextCompactionThresholdChars int `json:"context_compaction_threshold_chars,omitempty"`
	ContextCompactionKeepRounds     int `json:"context_compaction_keep_rounds,omitempty"`

	// SearchResultChars / SourceContentChars cap how much text two of the
	// agent's highest-traffic tools return per call (search_knowledge_base's
	// per-hit snippet, get_source_content's full-text cap — also reused by
	// run_code's output cap). 0 = built-in default (agent.go's
	// agentSearchResultChars=400 / agentSourceContentChars=8000). rank.go's
	// directly analogous MaxPrimaryContentChars/MaxSiblingChars are already
	// configurable for the same "how much text enters the model's context"
	// concern — raise these for a large-context model to cut down on
	// redundant follow-up tool calls; lower them for a small-context local
	// model where a few tool calls in one turn could otherwise exhaust the
	// whole context budget before llm.go's context compaction even gets a
	// chance to help.
	SearchResultChars  int `json:"search_result_chars,omitempty"`
	SourceContentChars int `json:"source_content_chars,omitempty"`
}

// toolRouterConfig configures the optional pre-flight tool-routing step
// (tool_router.go's runToolRouter): one extra, cheap LLM call — offered only
// the "live" tools (MSSQL/Shop/HTTP templates, via buildLiveTools) — that
// decides up front whether a tool is needed at all and, if so, runs it,
// before the main answer/draft call ever sees the question. Its result (if
// any) is folded into the same context block as the RAG chunks. This is
// additive, not a replacement: the main call's own native multi-round tool
// calling (llm.go's chatWithToolsBudget) still runs afterward exactly as
// before, so a wrong or incomplete router decision is never the only chance
// to use a tool.
type toolRouterConfig struct {
	// Enabled turns the pre-flight call on. Default false: it's an extra
	// LLM round-trip on every Chat/Agent/Mail request, so — like
	// AllowWebFetch/AllowCodeExecution above — it's opt-in even though it
	// adds no new reach (it only calls tools the request could already
	// reach natively), purely because of the added cost/latency.
	Enabled bool `json:"enabled"`
	// Profile selects which chat backend runs the router call: "local" or
	// "azure". Empty means "the same profile the main answer/draft call
	// resolves to" — the common case, since the router is meant to be a
	// cheap pre-check, not necessarily a reason to pay for a second,
	// different backend.
	Profile string `json:"profile,omitempty"`
}

// validateToolRouterSettings rejects an unknown Profile before handleSettings
// persists it — same "catch it now, there's no later live-apply step that
// would surface a typo more visibly" reasoning as validateStorageSettings
// (vectorstore.go). "" (use the main call's own profile) is always valid.
func validateToolRouterSettings(c toolRouterConfig) error {
	switch strings.ToLower(strings.TrimSpace(c.Profile)) {
	case "", "local", "azure":
		return nil
	default:
		return fmt.Errorf("unknown profile %q (valid: \"\", local, azure)", c.Profile)
	}
}

// queryRewriteConfig configures the optional conversation-aware retrieval-
// query rewrite (query_rewrite.go's rewriteQueryForRetrieval): one extra,
// cheap LLM call — given only the recent history plus the latest question,
// no tools — that turns a follow-up like "und bei Kistenpfennig?" into a
// self-contained search query ("Lieferantenrichtlinie Kistenpfennig")
// before rankedSearch runs. Shaped identically to toolRouterConfig
// (Enabled + optional Profile override) since it solves the same kind of
// problem the same way: fail-open, additive, cheap pre-flight step.
type queryRewriteConfig struct {
	// Enabled turns the pre-flight rewrite on. Default false: like
	// ToolRouter, this is an extra LLM round-trip — here, on every /api/ask
	// call that carries non-empty History — so it's opt-in purely for the
	// added cost/latency, not because it reaches anything new.
	Enabled bool `json:"enabled"`
	// Profile selects which chat backend runs the rewrite call: "local" or
	// "azure". Empty means s.ChatProfile (the deployment's own default
	// chat backend) — the rewrite call happens before handleAsk resolves
	// req.Profile/the vision-routing override, so unlike
	// resolveRouterProfile there is no "main call's own profile" to fall
	// back to yet at this point in the request.
	Profile string `json:"profile,omitempty"`
}

// validateQueryRewriteSettings mirrors validateToolRouterSettings exactly
// — same reasoning, same valid values.
func validateQueryRewriteSettings(c queryRewriteConfig) error {
	switch strings.ToLower(strings.TrimSpace(c.Profile)) {
	case "", "local", "azure":
		return nil
	default:
		return fmt.Errorf("unknown profile %q (valid: \"\", local, azure)", c.Profile)
	}
}

// ldapConfig holds Active Directory connection details for real login.
// RequiredGroupDN is the actual access-control gate: a successful LDAP
// bind only proves the credentials are valid, not that the account should
// have R3 admin access — see ldapauth.go's ldapAuthenticate. Defaults here
// are the known-good values for Zitec's AD (see ndz/lib/ldap.go's Auth()),
// which is what Rubix GmbH staff currently authenticate against; adjust
// if a given deployment uses a different AD.
type ldapConfig struct {
	Enabled bool `json:"enabled"`
	// URL is an LDAP/LDAPS connection string, e.g.
	// "ldaps://inf-pla-04.zitec-intern.de:636".
	URL string `json:"url"`
	// BaseDN scopes the post-bind attribute/group-membership search, e.g.
	// "ou=Zitec,dc=zitec-intern,dc=de".
	BaseDN string `json:"base_dn"`
	// DomainPrefix is prepended as "DOMAIN\\user" to a bind username that
	// doesn't already contain a backslash — matches how ndz/lib/ldap.go's
	// Auth() normalizes "user" to "ZITEC\\user".
	DomainPrefix string `json:"domain_prefix"`
	// RequiredGroupDN, if set, restricts admin access to members of this
	// AD group (checked via the bound account's memberOf attribute) — e.g.
	// "CN=R3-Admins,OU=Groups,DC=zitec-intern,DC=de". Left empty until a
	// real group is designated; while empty, any successfully authenticated
	// AD account is granted admin access (logged as a warning each login —
	// see ldapauth.go) since a valid bind is still real per-user
	// authentication, just not yet role-restricted. This only ever gates
	// admin access (ldapUser.IsAdmin) — an account that fails this check
	// still gets a normal (non-admin) session, since login also serves
	// department-restricted content access (SourceAccess above) and
	// answer personalization (PersonalizeAnswers), which every employee
	// should be able to use regardless of admin rights.
	RequiredGroupDN string `json:"required_group_dn"`
	// AdminUsers is a fixed allow-list of accounts that always get admin
	// access, independent of RequiredGroupDN/AD group membership — matched
	// case-insensitively against the account's AD "mail" attribute, its
	// CN, or whatever identifier it logged in with (email, sAMAccountName,
	// DOMAIN\\user), so it works regardless of which of those the admin
	// happens to list here, e.g. ["simon.waldherr@rubix.com", "waldherr"].
	// Combines with RequiredGroupDN (either grants admin, not required
	// together) - and once this list is non-empty, it also turns off the
	// "empty RequiredGroupDN grants admin to everyone" bootstrap default
	// below, since an explicit list is itself a deliberate access
	// restriction, not an oversight to warn about.
	AdminUsers []string `json:"admin_users"`
	// GuestAzureProfilePolicy decides what happens when an anonymous (no
	// session) caller explicitly requests the "azure" chat profile in
	// /api/ask while Enabled is on — docs/UI_HARDENING_PLAN.md's
	// "Registriert" tier treats picking the Azure backend as
	// cost-relevant, so it gates it behind a real login once one exists
	// at all. "" or "fallback" (default): silently use the configured
	// default chat profile instead, no error — the plan's recommended
	// choice, least surprising for the UI. "deny": reject the request
	// instead, so a caller unambiguously learns the tier exists. Ignored
	// entirely when Enabled is false (no login exists, so no tier to
	// enforce).
	GuestAzureProfilePolicy string `json:"guest_azure_profile_policy,omitempty"`
}

// denyGuestAzure reports whether cfg's policy should reject (rather than
// silently fall back) an anonymous "azure" profile request.
func (cfg ldapConfig) denyGuestAzure() bool {
	return cfg.GuestAzureProfilePolicy == "deny"
}

// localAuthConfig configures individually-provisioned local accounts
// (localusers.go/handlers_local_users.go) — see appSettings.LocalAuth's doc
// comment for the "why" (LDAP alternative/complement). Local user rows
// themselves (username, bcrypt password hash, department, admin flag) live
// in their own localUserStore, selected via storageSettings.Backend/
// UsersPath, not in settings.json — this struct only holds the small,
// deployment-wide policy knobs.
type localAuthConfig struct {
	Enabled bool `json:"enabled"`
	// MinPasswordLength is enforced by handleLocalUserCreate/
	// handleLocalUserSetPassword when an admin sets or changes a local
	// user's password. 0 falls back to defaultLocalAuthMinPasswordLength.
	MinPasswordLength int `json:"min_password_length,omitempty"`
	// BcryptCost is bcrypt's work-factor parameter (bcrypt.DefaultCost is
	// 10; higher is slower to compute and slower to brute-force). 0 falls
	// back to defaultLocalAuthBcryptCost. Valid range enforced by
	// validateLocalAuthConfig is bcrypt.MinCost..bcrypt.MaxCost.
	BcryptCost int `json:"bcrypt_cost,omitempty"`
}

// defaultLocalAuthMinPasswordLength/defaultLocalAuthBcryptCost are the
// effective defaults when localAuthConfig's fields are left at their zero
// value — 12 characters and bcrypt cost 12 are both meaningfully stronger
// than bcrypt's own package default (cost 10), matching current guidance for
// an internal but network-reachable admin-facing login.
const (
	defaultLocalAuthMinPasswordLength = 12
	defaultLocalAuthBcryptCost        = 12
)

func (cfg localAuthConfig) effectiveMinPasswordLength() int {
	if cfg.MinPasswordLength > 0 {
		return cfg.MinPasswordLength
	}
	return defaultLocalAuthMinPasswordLength
}

func (cfg localAuthConfig) effectiveBcryptCost() int {
	if cfg.BcryptCost > 0 {
		return cfg.BcryptCost
	}
	return defaultLocalAuthBcryptCost
}

// validateLocalAuthConfig rejects an out-of-range BcryptCost/negative
// MinPasswordLength before handleSettings persists it — same "validate
// before it can break a future startup/login" reasoning as
// validateStorageSettings.
func validateLocalAuthConfig(cfg localAuthConfig) error {
	if cfg.MinPasswordLength < 0 {
		return fmt.Errorf("local_auth.min_password_length must be >= 0, got %d", cfg.MinPasswordLength)
	}
	if cfg.BcryptCost != 0 && (cfg.BcryptCost < bcrypt.MinCost || cfg.BcryptCost > bcrypt.MaxCost) {
		return fmt.Errorf("local_auth.bcrypt_cost must be 0 (default) or between %d and %d, got %d", bcrypt.MinCost, bcrypt.MaxCost, cfg.BcryptCost)
	}
	return nil
}

// exchangeGraphConfig configures reading a shared/service mailbox in
// Exchange Online via Microsoft Graph (see graphmail.go) — app-only,
// needs a Mail.Read (or Mail.ReadBasic.All) *application* permission,
// ideally scoped to Mailbox via an Exchange application access policy so
// the app registration can't read arbitrary mailboxes. Only applies to
// Microsoft 365; for on-prem Exchange or IMAP-reachable mailboxes in
// general, see mailboxConfig (imap.go) instead.
type exchangeGraphConfig struct {
	connRuntime
	Enabled         bool   `json:"enabled"`
	TenantID        string `json:"tenant_id"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty"`
	// Mailbox is the shared/service mailbox to read, e.g. "vertrieb@rubix.com".
	Mailbox string `json:"mailbox"`
	// Folder is a well-known Graph folder name ("inbox", "sentitems", ...)
	// or a folder display name; empty defaults to "inbox".
	Folder string `json:"folder"`

	// EnableDraftReplies gates this ONE connection's write access — Graph's
	// createReply-then-PATCH sequence (graphmail.go's
	// createExchangeGraphDraft) — needed in addition to the app
	// registration itself actually holding a Mail.ReadWrite (or narrower
	// Mail.ReadWrite.Shared, scoped via an application access policy the
	// same way Mail.Read is, see the struct doc comment above) *application*
	// permission; this flag is R3's own opt-in on top of whatever Graph
	// permissions the app registration happens to have, mirroring
	// appSettings.EnableDraftReplies' "off by default, explicit opt-in"
	// convention for the unrelated Mail-tab/IMAP draft feature. Off by
	// default: an existing Exchange connection that only ever read mail
	// before this setting existed keeps doing exactly that after an
	// upgrade. HARD INVARIANT regardless of this flag's value: R3 never
	// calls a Graph "send" endpoint anywhere — see createExchangeGraphDraft.
	EnableDraftReplies bool `json:"enable_draft_replies"`
	// EnableAutoDraftRules turns on the unattended side of drafting: the
	// scheduler's exchange-graph-sync job (scheduler.go) checking each
	// newly-seen message against AutoDraftRules below and, on a match,
	// calling createExchangeGraphDraft itself — no human click involved.
	// Off by default, and additionally requires EnableDraftReplies (the
	// underlying write capability) to also be on; either flag off means
	// the sync job behaves exactly as it did before auto-drafting existed
	// (read/import only). Still, always and only, a DRAFT: this flag
	// controls whether a draft gets created automatically, never whether
	// it gets sent — nothing in this codebase sends.
	EnableAutoDraftRules bool `json:"enable_auto_draft_rules"`
	// AutoDraftRules is the small admin-configured rule set the sync job
	// evaluates against each newly-seen message's From/Subject (see
	// autodraft.go's matchAutoDraftRule) — the classic example being "from
	// does NOT contain rubix.com" (PatternField: "from", Pattern:
	// "rubix.com", Negate: true) to auto-draft a reply to external senders.
	// Rules are evaluated in order; the first enabled match wins, later
	// rules are not consulted for that message.
	AutoDraftRules []exchangeAutoDraftRule `json:"auto_draft_rules,omitempty"`
	// AutoDraftedIDs is the dedup cursor for the rule engine: message IDs
	// already checked against AutoDraftRules (whether or not a rule
	// actually matched), so a message isn't re-evaluated — and, if it
	// matched, doesn't get a second duplicate draft — on every subsequent
	// scheduler tick. Server-managed exactly like SharePoint's DeltaLink/
	// IMAP's LastUID: never accept this from a settings POST body, only
	// ever write it after a sync run (see mergeExchangeGraphConn,
	// connruntime.go). Capped at autoDraftedIDsCap (autodraft.go) so a
	// long-lived mailbox doesn't grow this list forever.
	AutoDraftedIDs []string `json:"auto_drafted_ids,omitempty"`
	// LastSyncedReceived is the scheduled sync's incremental watermark: the
	// receivedDateTime (RFC3339) of the newest message a sync run has
	// attempted so far. Empty (a fresh or pre-upgrade connection) means the
	// next run bootstraps from a newest-N preview exactly as before this
	// field existed; once set, runs list FORWARD from here
	// (listExchangeMailSince, oldest first) so a backlog drains across
	// capped runs and a burst larger than one preview page is never lost.
	// Server-managed like AutoDraftedIDs above (mergeExchangeGraphConn);
	// reset it to "" to force a fresh newest-N bootstrap.
	LastSyncedReceived string `json:"last_synced_received,omitempty"`

	// ─────────────────────────────────────────────────────────────────
	// Interactive, per-user mailbox access (mail_graph.go) — distinct from
	// everything above, which all reads/writes the ONE fixed Mailbox for
	// import/auto-draft. InteractiveEnabled+AllowedUsers instead let an
	// authorized, logged-in user browse and draft-reply to THEIR OWN
	// mailbox (sessionClaims.Mail) from within the Mail tab, through this
	// connection's same app credentials — Graph's app-only auth can act on
	// any mailbox the app registration is granted, so no per-user OAuth
	// consent is needed, only R3's own authorization on top.
	// ─────────────────────────────────────────────────────────────────

	// InteractiveEnabled opts this connection into native, per-user mailbox
	// browsing in the Mail tab. Off by default, same "explicit opt-in"
	// convention as EnableDraftReplies — an existing connection configured
	// only for import/auto-draft keeps doing exactly that after an upgrade.
	InteractiveEnabled bool `json:"interactive_enabled"`
	// AllowedUsers restricts InteractiveEnabled to specific AD accounts
	// (email or login name, case-insensitive — matched the same way as
	// ldapConfig.AdminUsers, via ldapMatchesAdminUser) — "welche Benutzer
	// diesen Connector verwenden dürfen." Empty means nobody is authorized
	// yet: an opt-in allow-list, not opt-out, since this grants live
	// mailbox read (and, together with EnableDraftReplies, write-a-draft)
	// access to whichever mailbox a logged-in user happens to have. A user
	// not listed here is unaffected and keeps the pre-existing manual
	// copy-paste Mail-tab workflow.
	AllowedUsers []string `json:"allowed_users,omitempty"`
	// AllowedGroups is AllowedUsers' AD-group-DN counterpart — matched via
	// ldapIsMemberOf against the caller's memberOf (sessionClaims.Groups).
	// Either list matching is enough; both empty still means nobody is
	// authorized (same deliberate deny-by-default posture as AllowedUsers
	// alone had before this field existed — see findInteractiveExchangeOptions).
	AllowedGroups []string `json:"allowed_groups,omitempty"`
	// InteractiveShared flips this connection's InteractiveEnabled mode from
	// "browse MY OWN mailbox" (the original and still-default behavior,
	// Mailbox overridden to the caller's own address) to "browse THIS
	// connection's own configured Mailbox as-is" — for a team/shared inbox
	// (e.g. "vertrieb@rubix.com") that several authorized users should be
	// able to browse and draft-reply from, none of them personally owning
	// it. An admin wanting to offer a user BOTH their own mailbox and a
	// shared one configures two separate connections — one of each mode —
	// rather than this field trying to mean both at once. Off by default:
	// an existing InteractiveEnabled connection keeps meaning "own mailbox"
	// after an upgrade. See findInteractiveExchangeOptions.
	InteractiveShared bool `json:"interactive_shared,omitempty"`
}

func (c exchangeGraphConfig) isEnabled() bool { return c.Enabled }

// exchangeAutoDraftRule is one admin-configured "auto-draft a reply to
// messages matching this" rule (see exchangeGraphConfig.AutoDraftRules and
// autodraft.go's matchAutoDraftRule). Pattern matching is a plain
// case-insensitive substring test, not a glob/regex — the small, fixed set
// of fields here (From/Subject) rarely needs more, and a substring test is
// trivial for an admin to reason about ("does the sender contain
// rubix.com") without learning glob/regex syntax.
type exchangeAutoDraftRule struct {
	// PatternField selects which part of the message this rule inspects:
	// "from" (sender address/display name, the common case — an external
	// sender rule) or "subject". Anything else (including empty) falls
	// back to "from" — see matchAutoDraftRule.
	PatternField string `json:"pattern_field"`
	// Pattern is matched case-insensitively as a substring against
	// PatternField's value.
	Pattern string `json:"pattern"`
	// Negate flips the test: the rule matches when PatternField's value
	// does NOT contain Pattern, instead of when it does. This is what
	// makes "sender is NOT from rubix.com" expressible at all — without
	// it, only an allow-list ("matches this specific domain"), never a
	// deny-list/external-sender rule, could be written.
	Negate bool `json:"negate,omitempty"`
	// Enabled lets an admin keep a rule configured but temporarily
	// inactive without deleting and later re-typing it — same "keep
	// config, toggle behavior" convention as connRuntime.Paused.
	Enabled bool `json:"enabled"`
}

// sharePointConfig holds an Azure AD app registration's client-credentials
// (app-only, no interactive user) plus the target site — enough for
// sharepoint.go to call Microsoft Graph without a signed-in user, suitable
// for a background import job. ClientSecretEnv (an env var *name*) is
// preferred over inlining the secret, matching llmProfile.APIKeyEnv's
// pattern.
//
// Request the *application* permission `Sites.Selected`, not
// `Sites.Read.All`. Sites.Selected grants this app registration access to
// zero sites by default and requires an explicit, per-site grant (Graph's
// `sites/{id}/permissions`, done by IT, not by R3) before it can read a
// given site at all — Sites.Read.All instead hands out tenant-wide read
// access in one step, a much larger blast radius (personal OneDrives,
// other business units' restricted sites) than a connector that in
// practice only ever targets one or a handful of sites needs. See
// ANLEITUNG.md's "SharePoint" section for the full rollout/permission
// rationale.
type sharePointConfig struct {
	connRuntime
	Enabled         bool   `json:"enabled"`
	TenantID        string `json:"tenant_id"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty"`
	// SiteURL is the SharePoint site's web URL, e.g.
	// "https://rubix.sharepoint.com/sites/Vertrieb".
	SiteURL string `json:"site_url"`
	// DeltaLink is the last @odata.deltaLink Microsoft Graph returned for
	// this site's drive delta feed (see sharepoint.go's spDeltaSync) —
	// resuming from it returns only what changed since, instead of
	// re-listing the whole drive on every sync. Empty means "no delta sync
	// has run yet, do a full initial one." Server-managed, same as
	// mailboxConfig.LastUID: never accept this from a settings POST body,
	// only ever write it after a sync run (handlers.go's
	// handleSharePointDeltaSync).
	DeltaLink string `json:"delta_link,omitempty"`
	// ItemPaths maps each item's Graph drive-item id to the path it was
	// last ingested under — server-managed exactly like DeltaLink (never
	// accept this from a settings POST body, only ever write it after a
	// sync run alongside DeltaLink). Exists purely to reconcile renamed or
	// moved items across delta runs: Graph's delta feed reports an item's
	// *new* path on a rename/move but never re-emits a delete for its old
	// path (nor for any descendants, if a parent folder was renamed/
	// moved) — without tracking the id->path history ourselves, a
	// renamed/moved file's old-path chunks would stay in the vector store
	// forever, orphaned and never cleaned up. See deltaSyncSharePoint.
	ItemPaths map[string]string `json:"item_paths,omitempty"`
	// SyncIntervalMinutes, if > 0, has the scheduler (scheduler.go) run
	// the delta sync on this interval, in addition to the on-demand
	// "Delta-Sync" button in the Import tab. 0 (default) means
	// manual-only — same shape as freshserviceConfig.SyncIntervalMinutes.
	SyncIntervalMinutes int `json:"sync_interval_minutes"`
	// LiveSearchEnabled offers a search_sharepoint tool (agent.go's
	// buildLiveTools, sharepoint.go's spSearch) to Chat/Agent/Mail: a
	// LIVE Microsoft Graph Search query against this site at answer time,
	// gated further by the "sharepoint_search" preset tool category
	// (same axis as mssql/shop/http). Distinct from — and orthogonal to
	// — this connection's own import/delta-sync above: those pull
	// content INTO the vector store ahead of time (fast retrieval, needs
	// re-sync for freshness); this instead reaches out live every time
	// (always current, costs a Graph round-trip per use, no ranked-
	// retrieval integration with the rest of the knowledge base). Off by
	// default, same "new outward exposure → opt-in" posture as
	// agentConfig.AllowWebFetch. Microsoft's Search API's exact app-only
	// permission requirements can differ from every other SharePoint
	// feature in this file (Sites.Selected may not be sufficient in every
	// tenant/Graph version — Sites.Read.All might be required); verify a
	// real query actually returns hits before relying on this.
	//
	// NOT per-user security trimmed: Graph's /search/query only applies
	// its automatic security trimming to delegated (signed-in-user)
	// queries. R3 has no delegated/on-behalf-of Graph auth anywhere —
	// spSearch authenticates purely app-only, same client-credentials flow
	// as every other call in this file — so results here are bounded only
	// by this app's own Sites.Selected grant, not by the asking human's own
	// SharePoint permissions, exactly like R3's bulk-imported content (see
	// ANLEITUNG.md's SharePoint section). Enabling this is not a
	// safer/more permission-aware alternative to bulk import — same
	// exposure, just live instead of pre-ingested.
	LiveSearchEnabled bool `json:"live_search_enabled,omitempty"`
	// AccessControl additionally restricts THIS connection's live search
	// (search_sharepoint) to specific users/AD groups — same field/
	// semantics as mssqlConfig/shopConfig/restConnectorConfig's own
	// AccessControl. Previously missing entirely: the live-search tool
	// had no per-caller/department gate of any kind (only the coarse,
	// deployment-wide "sharepoint_search" preset tool category), unlike
	// this same site's IMPORTED content (source_kind "sharepoint_file"/
	// "sharepoint_page"), which SourceAccess already restricts by
	// department — so a site an admin scoped to one department via
	// SourceAccess got no equivalent protection on its live search.
	AccessControl accessControl `json:"access_control,omitempty"`
}

func (c sharePointConfig) isEnabled() bool { return c.Enabled }

// oneDriveConfig holds the app-only Microsoft Graph credentials and one
// explicitly selected business drive. DriveID is intentionally required
// rather than accepting /me: client-credentials have no signed-in person,
// and an explicit opaque drive ID makes the imported scope reviewable. The
// app registration normally needs the least broad Graph permission the
// tenant can support for that drive; verify the effective grant with the
// connection test before enabling a scheduled run.
//
// DeltaLink is server-managed and never accepted from a Settings POST. Files
// use their immutable Graph item ID as R3 source identity, so moves/renames
// require no path-to-path reconciliation state.
type oneDriveConfig struct {
	connRuntime
	Enabled         bool   `json:"enabled"`
	TenantID        string `json:"tenant_id"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty"`
	// DriveID is the immutable Graph drive id, not a display name.
	DriveID string `json:"drive_id"`
	// FolderPath optionally limits this connection to a subtree of the
	// selected drive. Empty imports the whole drive.
	FolderPath string `json:"folder_path,omitempty"`
	DeltaLink  string `json:"delta_link,omitempty"`
}

func (c oneDriveConfig) isEnabled() bool { return c.Enabled }

// teamsConfig holds an Azure AD app registration (own credentials, same
// reasoning as sharePointConfig) plus the target channel — needs
// ChannelMessage.Read.All *application* permission, admin-consented (a
// higher-privilege permission that reads across the tenant's teams, so
// scope who has access to this app registration accordingly).
type teamsConfig struct {
	connRuntime
	Enabled         bool   `json:"enabled"`
	TenantID        string `json:"tenant_id"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty"`
	// TeamID/ChannelID are the Graph object IDs — easiest way to find
	// them: open the channel in the Teams web app and copy them out of
	// the URL (.../channel/<channelId>/... ?groupId=<teamId>...).
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
}

func (c teamsConfig) isEnabled() bool { return c.Enabled }

// confluenceConfig holds Atlassian Confluence Cloud REST API access — an
// account email plus an API token (Atlassian ID -> Security -> API
// tokens), sent as HTTP Basic auth, scoped to whatever spaces that
// account can read.
type confluenceConfig struct {
	connRuntime
	Enabled bool `json:"enabled"`
	// BaseURL is the site's Confluence base, e.g.
	// "https://rubix.atlassian.net/wiki".
	BaseURL     string `json:"base_url"`
	Email       string `json:"email"`
	APIToken    string `json:"api_token,omitempty"`
	APITokenEnv string `json:"api_token_env,omitempty"`
	// SpaceKey is the space to import from, e.g. "VERTRIEB".
	SpaceKey string `json:"space_key"`
}

func (c confluenceConfig) isEnabled() bool { return c.Enabled }

// jiraConfig holds Atlassian Jira Cloud REST API access — an account
// email plus an API token (Atlassian ID -> Security -> API tokens), sent
// as HTTP Basic auth, same credential shape as confluenceConfig (often
// literally the same Atlassian account, since Jira and Confluence Cloud
// tokens are interchangeable for any project/space that account can read).
type jiraConfig struct {
	connRuntime
	Enabled bool `json:"enabled"`
	// BaseURL is the site's Jira base, e.g. "https://rubix.atlassian.net" —
	// unlike confluenceConfig.BaseURL, without a "/wiki" suffix.
	BaseURL     string `json:"base_url"`
	Email       string `json:"email"`
	APIToken    string `json:"api_token,omitempty"`
	APITokenEnv string `json:"api_token_env,omitempty"`
	// ProjectKey is the project to import from, e.g. "OPS".
	ProjectKey string `json:"project_key"`
}

func (c jiraConfig) isEnabled() bool { return c.Enabled }

// freshserviceConfig holds Freshservice REST API v2 access — an account API
// key sent as HTTP Basic auth with the literal string "X" as the password
// (Freshservice's documented convention: https://api.<domain>.freshservice.com,
// Basic base64(apiKey+":X")), same env-var-indirection shape as jiraConfig/
// confluenceConfig's APIToken/APITokenEnv.
type freshserviceConfig struct {
	connRuntime
	Enabled bool `json:"enabled"`
	// BaseURL is the site's Freshservice base, e.g.
	// "https://rubix.freshservice.com" — a full URL like jiraConfig.BaseURL,
	// not just the bare subdomain the Freshservice API docs sometimes use.
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// SyncIntervalMinutes, if > 0, has the scheduler (scheduler.go) pull and
	// ingest every ticket on this interval, in addition to the on-demand
	// preview/import in the Import tab. 0 (default) means manual-only,
	// matching every other connector's current behavior.
	SyncIntervalMinutes int `json:"sync_interval_minutes"`
}

func (c freshserviceConfig) isEnabled() bool { return c.Enabled }

// folderConfig watches one server-local directory (see ingest.go's
// ingestFolder, scheduler.go's folder-sync job). Deliberately the smallest
// connector config in the codebase — no secret (R3 reads with its own
// process's filesystem access) and no server-managed resume cursor (unlike
// SharePoint's DeltaLink/IMAP's LastUID, a plain directory has no
// server-side "what changed since X" API to resume from; repeated scans
// rely on ingestDocument's existing content-hash skip instead).
type folderConfig struct {
	connRuntime
	Enabled bool `json:"enabled"`
	// Path is the server-local directory to scan recursively, e.g.
	// "C:\Freigaben\Vertrieb". Must be reachable by the R3 process itself
	// (a local path or an already-mounted network share) — R3 doesn't
	// mount anything on its own.
	Path string `json:"path"`
}

func (c folderConfig) isEnabled() bool { return c.Enabled }

// githubConfig imports repository knowledge which is usually more useful
// than source code blobs for an internal assistant: README documentation,
// Issues and Pull Requests. BaseURL defaults to the public GitHub API but
// can point at a GitHub Enterprise API prefix such as
// https://github.example.com/api/v3. TokenEnv is preferred so a token never
// needs to be stored in settings.json.
//
// LastSyncedAt/CycleStartedAt/NextPage are server-managed pagination state.
// They make a large first import resumable without pretending GitHub's
// timestamp filter is a lossless cursor on its own.
type githubConfig struct {
	connRuntime
	Enabled             bool   `json:"enabled"`
	BaseURL             string `json:"base_url,omitempty"`
	Token               string `json:"token,omitempty"`
	TokenEnv            string `json:"token_env,omitempty"`
	Owner               string `json:"owner"`
	Repository          string `json:"repository"`
	IncludeReadme       bool   `json:"include_readme"`
	IncludeIssues       bool   `json:"include_issues"`
	IncludePullRequests bool   `json:"include_pull_requests"`
	LastSyncedAt        string `json:"last_synced_at,omitempty"`
	CycleStartedAt      string `json:"cycle_started_at,omitempty"`
	NextPage            int    `json:"next_page,omitempty"`
}

func (c githubConfig) isEnabled() bool { return c.Enabled }

// sapS4Config is a deliberately narrow OData importer. EntityPath is a
// relative entity-set path below BaseURL; IDField, TitleField and
// ContentFields select exactly which scalar fields become a document. This
// avoids an accidental broad mirror of an S/4 business object. SAP systems
// differ in whether they expose OData V2 or V4; sap_s4.go accepts both
// response envelopes and uses a delta link whenever the service supplies
// one, otherwise an idempotent full scan.
type sapS4Config struct {
	connRuntime
	Enabled     bool              `json:"enabled"`
	BaseURL     string            `json:"base_url"`
	AuthType    string            `json:"auth_type,omitempty"` // basic, bearer, header, none
	Username    string            `json:"username,omitempty"`
	Password    string            `json:"password,omitempty"`
	PasswordEnv string            `json:"password_env,omitempty"`
	Token       string            `json:"token,omitempty"`
	TokenEnv    string            `json:"token_env,omitempty"`
	HeaderName  string            `json:"header_name,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// EntityPath must be relative (e.g.
	// /sap/opu/odata/sap/API_BUSINESS_PARTNER/A_BusinessPartner), with no
	// query string. The importer constructs $select/$top itself.
	EntityPath     string   `json:"entity_path"`
	IDField        string   `json:"id_field"`
	TitleField     string   `json:"title_field,omitempty"`
	ContentFields  []string `json:"content_fields"`
	UpdatedAtField string   `json:"updated_at_field,omitempty"`
	// DeltaLink/NextLink are server-managed continuation URLs. Both are
	// validated against BaseURL before use so an upstream response cannot
	// turn a stored cursor into an outbound request to another host.
	DeltaLink string `json:"delta_link,omitempty"`
	NextLink  string `json:"next_link,omitempty"`
}

func (c sapS4Config) isEnabled() bool { return c.Enabled }

// smtpConfig holds outbound SMTP relay access for R3's own mail sending
// (mail.go) — currently used only by the chat "send this answer to me"
// feature (handlers.go's handleChatEmail), never for the HITL draft-reply
// path (draft.go), which stays send-free by design (see README's "Human-in-
// the-loop by design"). PasswordEnv follows the same indirection pattern as
// every other connector's credential field.
type smtpConfig struct {
	Enabled     bool   `json:"enabled"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
	// From is the envelope/header From address mail is sent as, e.g.
	// "r3-notifications@rubix.com" — distinct from the recipient, which is
	// always the asking user's own AD address (session claims.Mail), never
	// user-supplied, so R3 can't be used to relay mail to arbitrary
	// addresses.
	From string `json:"from"`
}

// mssqlConfig holds connection details for a live SQL Server the chat
// model can query via the query_mssql tool (see mssql.go). Strongly
// recommended: point Username at a database login that only has SELECT
// permission on the relevant tables/views — the app-layer SELECT-only
// check in mssql.go is defense in depth, not a substitute for a real
// read-only DB login, since it's a best-effort statement blocklist rather
// than a full SQL parser.
type mssqlConfig struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
	// PasswordEnv follows the same pattern as sharePointConfig.ClientSecret/
	// ClientSecretEnv.
	PasswordEnv string `json:"password_env,omitempty"`
	// TrustServerCertificate accepts the server's TLS certificate without
	// validating it against a trusted CA — common for on-prem SQL Server
	// instances using a self-signed cert. Defaults to true so a first-time
	// setup against a typical on-prem instance works out of the box; turn
	// it off for a deployment with a properly issued certificate.
	TrustServerCertificate bool `json:"trust_server_certificate"`
	// MaxRows caps how many result rows are ever returned to the model in
	// one tool call, so a broad query can't flood the context window (or
	// the token budget) with an entire table.
	MaxRows int `json:"max_rows"`
	// TimeoutSeconds bounds how long a single query is allowed to run.
	TimeoutSeconds int `json:"timeout_seconds"`
	// AllowGenericQuery gates the query_mssql tool above — the model can
	// compose *any* validated (SELECT-only) statement against the whole
	// database. Defaults to **false**: MSSQL access is opt-in at two
	// levels now, not one — turning on Enabled connects the database at
	// all, but the model still can't run arbitrary SQL until an admin
	// deliberately also flips this on, having actually verified the
	// connection/permissions/behavior first. The safer, narrower path is
	// QueryTemplates below (each an explicitly admin-authored, reviewed
	// statement) — enable this only once that's not enough and letting
	// the model compose its own SELECTs has been consciously accepted,
	// not just left on because it happened to be the default.
	AllowGenericQuery bool `json:"allow_generic_query"`
	// QueryTemplates are admin-curated, named, parameterized SQL queries,
	// each exposed to the chat model as its own tool (see mssql.go's
	// mssqlTemplateToolDef) — the MCP-style alternative to the single
	// generic query_mssql tool above: instead of letting the model
	// compose arbitrary SELECT statements, an admin decides exactly
	// which queries exist and which typed parameters they take. Managed
	// from Einstellungen -> "SQL-Abfrage-Vorlagen"; validated (unique
	// names, SELECT-only SQL, every declared parameter actually
	// referenced) before being accepted, since an invalid template here
	// would otherwise silently expose a broken/dangerous tool to every
	// future chat.
	QueryTemplates []sqlQueryTemplate `json:"query_templates,omitempty"`
	// MaskColumns names result columns (case-insensitive, exact match) that
	// get replaced with a fixed placeholder in every query_mssql/template
	// result before it ever reaches the chat model or a citation — e.g.
	// ["email", "phone", "iban"] for a customer table the model should be
	// able to reason over (row counts, aggregates, dates) without seeing
	// the underlying PII verbatim. Applies uniformly to both the generic
	// query_mssql tool and every QueryTemplate above, since both funnel
	// through runMSSQLQueryArgs (mssql.go) — same spirit as RedactPII for
	// imported documents, just at query-result time instead of import time.
	MaskColumns []string `json:"mask_columns,omitempty"`
	// AccessControl additionally restricts the MSSQL tool (generic query +
	// templates) to specific users/AD groups, on top of the existing
	// "Registriert"/admin tier gate (mssqlToolAllowed / the sub-agent
	// admin-only gate) — see accessControl's doc comment for its
	// empty-is-unrestricted semantics.
	AccessControl accessControl `json:"access_control,omitempty"`
}

// sqlQueryParam is one named parameter a sqlQueryTemplate accepts — both
// the JSON-schema shown to the model (Name/Type/Description/Required)
// and the type used to convert the model's JSON argument value before
// binding it into the query via sql.Named (see mssql.go's
// convertSQLTemplateParam), so a template's SQL text can safely take
// model-supplied values without ever string-concatenating them into the
// query text.
type sqlQueryParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // "string" | "integer" | "number" | "boolean" | "date"
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	// Example is a sample value shown to the model in the tool description
	// (and the JSON-schema property) so it understands the expected format
	// of a parameter it can't see the underlying SQL/URL for — e.g. an
	// order id "4711", a date "2026-01-31", a status "open". Optional.
	Example string `json:"example,omitempty"`
	// Options, when set, constrains this parameter to a fixed list of
	// valid values — e.g. the set of SAP table names a generic se16-style
	// URL template accepts ({table}/{id}: "likp", "vbak", "kna1", "mbew",
	// "mara", ...). Turns into a JSON-schema "enum" on the tool (most
	// providers steer the model toward only ever emitting a listed value)
	// AND is enforced server-side at execution time (mssqlTemplateToolExecutor/
	// httpTemplateToolExecutor reject a supplied value not in this list) —
	// the model choosing badly still can't reach an unintended table/
	// endpoint, the same "don't just trust the model" posture as the
	// SELECT-only SQL check. Empty (the common case) means "any value of
	// the declared Type", unchanged from before this field existed.
	Options []string `json:"options,omitempty"`
}

// sqlQueryTemplate is one admin-curated, named, parameterized SQL query —
// see QueryTemplates' doc comment above for the reasoning. SQL uses SQL
// Server's native @name parameter syntax (e.g. "SELECT TOP 50 * FROM
// Orders WHERE CustomerID = @customer_id"); every entry in Parameters
// must have a Name that actually appears in SQL as "@name" — checked by
// mssql.go's validateSQLQueryTemplates.
type sqlQueryTemplate struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	SQL         string          `json:"sql"`
	Parameters  []sqlQueryParam `json:"parameters,omitempty"`
	// ResultHint is an admin-authored note on what the query returns
	// (which columns/rows, in what unit), folded into the tool description
	// so the model knows what to expect back and can decide whether the
	// query answers the user's question — it never sees the SQL itself.
	// Optional. e.g. "Eine Zeile pro offener Bestellung: Bestellnr., Kunde,
	// Betrag in EUR, Bestelldatum."
	ResultHint string `json:"result_hint,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// httpQueryTemplate is one admin-curated, named, parameterized HTTP GET
// request — the generic analogue of sqlQueryTemplate above for REST APIs
// instead of SQL Server (see appSettings.HTTPTemplates' doc comment and
// http_tool.go). URLTemplate uses "{name}"-style placeholders (e.g.
// "https://rubix.freshservice.com/api/v2/tickets/{ticket_id}"); every entry
// in Parameters must have a Name that actually appears in URLTemplate as
// "{name}" — checked by http_tool.go's validateHTTPQueryTemplates, which
// also checks URLTemplate's host against AuthSource's own configured
// base_url (SSRF guard: an admin could otherwise point a template at an
// unrelated host while still "borrowing" a connector's credentials).
type httpQueryTemplate struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Method is currently always "GET" (validated) — write methods aren't
	// offered yet, mirroring MSSQL's SELECT-only posture.
	Method      string `json:"method,omitempty"`
	URLTemplate string `json:"url_template"`
	// AuthSource names which already-configured connector supplies
	// credentials for this request: "none" (no auth header), "confluence",
	// "jira" or "freshservice" — see http_tool.go's httpTemplateAuthHeader.
	AuthSource string          `json:"auth_source"`
	Parameters []sqlQueryParam `json:"parameters,omitempty"`
	// ResultHint mirrors sqlQueryTemplate.ResultHint — an admin note on what
	// the response contains, folded into the tool description. Optional.
	ResultHint string `json:"result_hint,omitempty"`
	// ResponseJSONPath, if set, extracts only this field from the JSON
	// response (e.g. "results" or "tickets.0.status") instead of returning
	// the full response body — keeps a verbose API envelope from flooding
	// the model's context window (see http_tool.go's extractJSONPath).
	ResponseJSONPath string `json:"response_json_path,omitempty"`
	// InsecureSkipVerify accepts the endpoint's TLS certificate without
	// validating it against a trusted CA — for an internal-only host
	// (e.g. an on-prem SAP se16-style gateway) whose certificate is
	// self-signed or issued by an internal CA the Go process doesn't
	// already trust, same "explicit, scoped opt-in" posture as
	// mssqlConfig.TrustServerCertificate. Off by default; a host with a
	// real, publicly-trusted certificate never needs this. See
	// connector.go's insecureConnectorHTTPClient for the actual client
	// this switches to.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`
	Enabled            bool `json:"enabled"`
}

// restConnectorConfig defines a generic, admin-configured REST backend that
// an httpQueryTemplate can reference by name via its AuthSource — the
// generalization of the built-in confluence/jira/freshservice auth sources
// (http_tool.go's httpTemplateAuthSources) to arbitrary HTTP systems: an SAP
// se16 gateway, an internal microservice, a third-party API. Unlike the
// import connectors, a REST connector holds no data and runs no import; it
// only tells the generic HTTP live tool WHERE a template may call (BaseURL,
// enforced as an SSRF host guard in validateHTTPQueryTemplates) and HOW it
// authenticates (AuthType + credentials + static Headers). See
// restConnectorByName / applyHTTPTemplateAuth (http_tool.go),
// validateRESTConnectors, and docs/CONNECTORS.md.
//
// It embeds connRuntime only for the shared Name key (and to satisfy the
// connWithName/connWithEnabled merge/lookup helpers) — the sync/limit/pause
// fields are meaningless for a live-only connector and are left unset.
type restConnectorConfig struct {
	connRuntime
	Enabled bool `json:"enabled"`
	// BaseURL pins the host every template that borrows this connector may
	// target (validateHTTPQueryTemplates checks the template URL's host
	// against it — the model only fills placeholder values, never the host,
	// but this stops an admin typo from pointing a credentialed template at
	// the wrong domain). Must be https, e.g. "https://logistic.rubix-intern.de".
	// The template still carries the full URL; BaseURL is not prepended.
	BaseURL string `json:"base_url"`
	// AuthType selects how credentials are attached to the request:
	// "none" (host pinning only), "basic" (Username+Password -> Authorization:
	// Basic), "bearer" (Token -> Authorization: Bearer) or "header" (Token ->
	// the header named by HeaderName, e.g. "X-API-Key"). Empty == "none".
	AuthType string `json:"auth_type,omitempty"`
	// Username/Password(+Env): HTTP Basic credentials (AuthType "basic").
	// PasswordEnv follows the same "prefer the env var" indirection every
	// other connector secret uses (resolveSecret).
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
	// Token(+Env): the bearer token (AuthType "bearer") or the raw header
	// value (AuthType "header"). TokenEnv mirrors PasswordEnv.
	Token    string `json:"token,omitempty"`
	TokenEnv string `json:"token_env,omitempty"`
	// HeaderName is the header the Token is placed in for AuthType "header"
	// (e.g. "X-API-Key", "apikey", "Ocp-Apim-Subscription-Key"). Ignored for
	// the other auth types; defaults to "Authorization" if left empty.
	HeaderName string `json:"header_name,omitempty"`
	// Headers are extra static request headers sent on every call through
	// this connector (e.g. a fixed Accept override, an "X-System" tag).
	// Deliberately NOT the place for the primary credential: values here are
	// round-tripped to the browser in clear (unlike Password/Token, which are
	// masked), so put secrets in Token/Password instead.
	Headers map[string]string `json:"headers,omitempty"`
	// AccessControl additionally restricts every HTTP template that
	// borrows this connector (auth_source) to specific users/AD groups —
	// see accessControl's doc comment for its empty-is-unrestricted
	// semantics.
	AccessControl accessControl `json:"access_control,omitempty"`
}

func (c restConnectorConfig) isEnabled() bool { return c.Enabled }

// accessControl is a reusable, generalized allow-list for a connector's
// live-tool access: which specific AD accounts and/or AD groups (matched
// against sessionClaims.Groups/agentSession.Groups, i.e. the caller's
// memberOf DNs) may use it. It's layered ADDITIONALLY on top of whatever
// coarser tier check (e.g. mssqlToolAllowed's "Registriert" gate) already
// applies to that tool — see accessControl.allows below for exactly how.
//
// Deliberately "empty = unrestricted" rather than "empty = nobody", unlike
// exchangeGraphConfig.AllowedUsers: MSSQL/Shop/REST connectors already
// existed with no per-identity restriction before this field was added, so
// an admin who upgrades without touching it must see unchanged behavior —
// this field only ever narrows access once an admin deliberately fills it
// in, never silently widens or revokes it.
type accessControl struct {
	// AllowedUsers matches the same way as ldapConfig.AdminUsers (email or
	// AD login name, case-insensitive, via ldapMatchesAdminUser).
	AllowedUsers []string `json:"allowed_users,omitempty"`
	// AllowedGroups matches by AD group DN against the caller's memberOf
	// list (ldapIsMemberOf) — coarser-grained than AllowedUsers, for
	// "anyone in this AD group" rules that don't need per-account upkeep.
	AllowedGroups []string `json:"allowed_groups,omitempty"`
}

// allows reports whether user (typically sess.User: an email address, or
// "anonym"/"api:..." for sessions without one) or any of groups
// (sess.Groups, the caller's AD memberOf DNs) is present in ac's
// allow-lists. An entirely empty accessControl (both lists) returns true —
// see the struct doc comment for why "unrestricted" is the deliberate
// empty-value default here, unlike exchangeGraphConfig.AllowedUsers.
func (ac accessControl) allows(user string, groups []string) bool {
	if len(ac.AllowedUsers) == 0 && len(ac.AllowedGroups) == 0 {
		return true
	}
	if ldapMatchesAdminUser(ac.AllowedUsers, user) {
		return true
	}
	for _, g := range ac.AllowedGroups {
		if g == "" {
			continue
		}
		if ldapIsMemberOf(groups, g) {
			return true
		}
	}
	return false
}

// resolvedPassword/resolvedToken prefer the *Env twin over the inline value,
// same convention as every other connector secret (resolveSecret).
func (c restConnectorConfig) resolvedPassword() string {
	return resolveSecret(c.Password, c.PasswordEnv)
}
func (c restConnectorConfig) resolvedToken() string { return resolveSecret(c.Token, c.TokenEnv) }

// resolvedPassword prefers PasswordEnv over the inline Password, so the SQL
// Server credential used by the MSSQL tool call doesn't need to sit in
// settings.json in plaintext.
func (c mssqlConfig) resolvedPassword() string {
	return resolveSecret(c.Password, c.PasswordEnv)
}

// shopConfig configures live access to Rubix's own B2B online shop
// (de.rubix.com) — a token-authenticated REST API (see shop.go), not a
// documented public API, so exact request/response shapes were confirmed
// by HTTP status codes/robots.txt rather than published docs (shop.go's
// package comment has the details and the known-open questions).
type shopConfig struct {
	Enabled bool `json:"enabled"`
	// BaseURL defaults to "https://de.rubix.com" (see shopBaseURLOrDefault)
	// when empty — overridable for a different Rubix country shop or a
	// staging environment.
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
	// PasswordEnv follows the same pattern as mssqlConfig.PasswordEnv above.
	PasswordEnv string `json:"password_env,omitempty"`
	// ClientID/ClientSecret are de.rubix.com's own shop frontend's fixed
	// "browser API client" credentials for POST /rest-api/v1/tokens
	// ("api_rest_browser"/"rbx2020" as observed in the shop's own browser
	// devtools network tab) — the same pair for every browser session
	// against this shop, not per-account, but still configured here rather
	// than hardcoded in shop.go so it isn't a literal secret-shaped string
	// baked into source. ClientSecretEnv follows the PasswordEnv pattern.
	ClientID        string `json:"client_id,omitempty"`
	ClientSecret    string `json:"client_secret,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty"`
	// TimeoutSeconds bounds how long a single search-items request (and the
	// token request that may precede it) is allowed to run. 0 = default
	// (shopDefaultTimeoutSeconds, shop.go).
	TimeoutSeconds int `json:"timeout_seconds"`
	// MaxResults caps how many items search_shop_items ever returns to the
	// model in one call, same reasoning as mssqlConfig.MaxRows. 0 = default
	// (shopDefaultMaxResults, shop.go).
	MaxResults int `json:"max_results"`
	// MaxRetries overrides shop.go's shopMaxRetries fallback (4) for how
	// many extra attempts a shop search/token request makes after a
	// transient failure before giving up — per-connection (unlike
	// graph/connector's deployment-wide GraphMaxRetries/
	// ConnectorMaxRetries) since a shop connection is itself a single,
	// specific external endpoint already configured per-connection here
	// (BaseURL, TimeoutSeconds). 0 = fallback.
	MaxRetries int `json:"max_retries,omitempty"`
	// AccessControl additionally restricts the three Shop tools to specific
	// users/AD groups — see accessControl's doc comment for its
	// empty-is-unrestricted semantics.
	AccessControl accessControl `json:"access_control,omitempty"`
}

// resolvedPassword prefers PasswordEnv over the inline Password, same
// convention as mssqlConfig.resolvedPassword above.
func (c shopConfig) resolvedPassword() string {
	return resolveSecret(c.Password, c.PasswordEnv)
}

// resolvedClientSecret prefers ClientSecretEnv over the inline
// ClientSecret, same convention as resolvedPassword above.
func (c shopConfig) resolvedClientSecret() string {
	return resolveSecret(c.ClientSecret, c.ClientSecretEnv)
}

// urlMapping translates a local path prefix (as stored in source_id) into a
// web-accessible URL prefix, enabling direct links from citation chips to the
// original document in SharePoint, an Intranet, or a web-served file store.
type urlMapping struct {
	Prefix    string `json:"prefix"`
	URLPrefix string `json:"url_prefix"`
}

type rankingConfig struct {
	VectorWeight        float64 `json:"vector_weight"`
	KeywordWeight       float64 `json:"keyword_weight"`
	RecencyWeight       float64 `json:"recency_weight"`
	RecencyHalfLifeDays float64 `json:"recency_half_life_days"`
	CandidateLimit      int     `json:"candidate_limit"`

	// MinVectorSimilarity drops a candidate hit entirely (before scoring,
	// in rank.go's rankedSearch) once its raw cosine similarity falls
	// below this threshold. 0 (the zero value, so existing settings.json
	// files keep today's behavior) disables the check — every one of the
	// top-K-by-similarity candidates is kept regardless of how weak the
	// match actually is. Guards against a sparse knowledge base filling a
	// context slot with a barely-related chunk just because nothing
	// better exists.
	MinVectorSimilarity float64 `json:"min_vector_similarity,omitempty"`

	// MinFinalScore drops a candidate hit (rank.go's rankedSearch, after
	// scoring, before truncating to K) once its relevance-for-filtering
	// score falls below this threshold. 0 disables the check (existing
	// behavior), same convention as MinVectorSimilarity above. For a hit
	// with zero KeywordScore — not one query term appears in the chunk at
	// all, via BM25 or literal overlap — the RecencyWeight contribution is
	// excluded from this check specifically: many embedding models put
	// "clearly unrelated" text at 0.5-0.65 raw cosine similarity, not near
	// 0, so a merely-recent, zero-keyword-overlap chunk could otherwise
	// pad out to K on recency alone. A hit that DOES share at least one
	// keyword keeps its full recency bonus — recency is a legitimate
	// tie-breaker once there's real textual relevance, just not a
	// substitute for it. Calibrated against this file's default
	// VectorWeight/KeywordWeight/RecencyWeight (0.7/0.2/0.1) below —
	// re-tune alongside those if they're changed materially.
	MinFinalScore float64 `json:"min_final_score,omitempty"`

	// AgentModeMinFinalScore overrides MinFinalScore, but ONLY for Agent
	// mode's baseline rankedSearch/assembleContext call in handlers.go's
	// handleAsk (the unconditional "Kontext:" block built once per request,
	// before the mode branch that decides which tools get added) — never
	// for Chat mode, and never for Agent's own on-demand
	// search_knowledge_base tool call (agent.go), which always uses
	// MinFinalScore unchanged. 0 (the default, so upgrading an existing
	// deployment changes nothing) disables the override entirely — Agent
	// mode's baseline call behaves exactly like Chat's, same MinFinalScore,
	// same convention as every other threshold in this struct.
	//
	// The reasoning for setting it above 0: Chat mode has no
	// search_knowledge_base tool at all — the baseline context is its ONLY
	// path to knowledge-base grounding, so it must never be filtered any
	// harder than MinFinalScore already does. Agent mode DOES have that
	// tool (an on-demand, multi-round, iterative version of the same
	// rankedSearch) — so its baseline context is often redundant: tokens
	// spent on it whenever the model doesn't end up needing it, since it
	// can always fetch what it actually needs itself. A stricter threshold
	// here trims that redundant baseline down (or drops it to nothing) for
	// Agent requests specifically.
	//
	// This is a genuine, disclosed TRADEOFF, not a strict improvement —
	// exactly why it defaults to off/unchanged: whenever the model actually
	// WOULD have used the (now-filtered-out) baseline context, it now pays
	// for an extra search_knowledge_base tool round-trip instead of already
	// having the answer in its first-pass context. Set this above
	// MinFinalScore only if that token/latency tradeoff is worth it for
	// your Agent traffic pattern.
	AgentModeMinFinalScore float64 `json:"agent_mode_min_final_score,omitempty"`

	// MaxSources caps how many distinct sources (not chunks) ever
	// contribute a citation to one answer's context (rank.go's
	// assembleContext/expandEmailFamilies) — 0 disables the cap (today's
	// behavior: as many distinct sources as the K hits happen to span,
	// plus email-family siblings). Bounds context size and answer
	// readability once K is set high enough that many barely-related
	// sources would otherwise each add their own citation block.
	MaxSources int `json:"max_sources,omitempty"`

	// MaxHitsPerSource caps how many of the K result slots any ONE source
	// may occupy (rank.go's capHitsPerSource, applied after scoring/
	// sorting) — a diversity guard so a single long document with many
	// similar chunks can't crowd every other source out of context. All
	// matched positions of a source that survive the cap still contribute
	// their own context windows (see fetchAllSourceChunks). 0 disables the
	// cap (previous behavior).
	MaxHitsPerSource int `json:"max_hits_per_source,omitempty"`

	// ContextChunksBefore/After bound how many of a matched chunk's own
	// neighboring chunks (by chunk_idx, same source) ride along in
	// context around it, instead of the source's entire chunk list —
	// e.g. 1/1 means "the matched chunk plus one neighbor on each side".
	// A negative value (defaultSettings' default: -1) means "unlimited",
	// i.e. today's behavior of pulling the whole source. 0 is a real,
	// distinct choice from "unlimited" — it means "no neighbors, just the
	// matched chunk itself" (the tightest, cheapest context). Only
	// disabled (both treated as unlimited) if either side is negative,
	// since defaultSettings always sets both together and a mismatched
	// pair would otherwise have no sensible single interpretation.
	ContextChunksBefore int `json:"context_chunks_before"`
	ContextChunksAfter  int `json:"context_chunks_after"`

	// MaxPrimaryContentChars caps how much text a single cited (matched)
	// source may contribute to context — 0 uses the built-in default
	// (rank.go's maxPrimaryContentCharsDefault, 6000), same "0 = default"
	// convention as agentConfig.MaxToolRounds.
	MaxPrimaryContentChars int `json:"max_primary_content_chars,omitempty"`
	// MaxSiblingChars caps how much text one email-family sibling
	// (expandEmailFamilies) may contribute — 0 uses the built-in default
	// (rank.go's maxSiblingCharsDefault, 4000).
	MaxSiblingChars int `json:"max_sibling_chars,omitempty"`
	// MaxFamilySiblings caps how many email-family siblings
	// (expandEmailFamilies) ride along in total — 0 uses the built-in
	// default (rank.go's maxFamilySiblingsDefault, 6).
	MaxFamilySiblings int `json:"max_family_siblings,omitempty"`
}

type importConfig struct {
	// MarkItDownBin is the executable used to convert office/PDF documents
	// to text/markdown. See https://github.com/microsoft/markitdown
	MarkItDownBin string `json:"markitdown_bin"`
	// MarkItDownDocIntelEndpoint optionally enables MarkItDown's Azure
	// Document Intelligence conversion path (`-d -e`). It is intentionally
	// empty by default because it sends document contents to Azure and may
	// incur usage charges.
	MarkItDownDocIntelEndpoint string `json:"markitdown_docintel_endpoint,omitempty"`
	// FFmpegBin extracts an audio track from video before the configured
	// MarkItDown audio-transcription converter processes it.
	FFmpegBin string `json:"ffmpeg_bin,omitempty"`
	// SevenZipBin is used for .7z archives. ZIP/TAR/GZIP are parsed natively
	// with the same archive entry/expanded-size limits.
	SevenZipBin string `json:"sevenzip_bin,omitempty"`
	MaxFileMB   int64  `json:"max_file_mb"`
	// MaxItemsPerRun caps how many items a single import run ingests,
	// across every connector — the guardrail against "one click pulls
	// 100k tickets/mails". A run that hits the cap stops early and says
	// so; a resumable connector (IMAP by UID, SharePoint by delta link)
	// simply continues from where it stopped on the next run/scheduler
	// tick, so the cap paces a large backlog into chunks rather than
	// dropping anything. 0 = importMaxItemsDefault. See import_limits.go.
	MaxItemsPerRun int `json:"max_items_per_run,omitempty"`
	// RequestDelayMS is a deliberate pause between an import's outbound
	// per-item requests — proactive throttling so R3 paces itself instead
	// of only backing off *after* an upstream 429 (see connector.go's
	// doWithRetry, which stays as the reactive second line). 0 = no
	// artificial delay (the previous behavior). See import_limits.go.
	RequestDelayMS int `json:"request_delay_ms,omitempty"`
	// PreviewLimit is how many items a connector's "preview, then select"
	// listing fetches (Confluence/Jira/Freshservice/Exchange/Teams). Was a
	// hard-coded 50 everywhere; now configurable so an admin can review
	// more (or fewer) candidates per preview. 0 = importPreviewDefault.
	PreviewLimit int `json:"preview_limit,omitempty"`
	// OriginalsDir stores a copy of the raw uploaded file when the browser
	// requests it (the upload form's "Original behalten" checkbox), so a
	// citation's source popup can offer a download of the exact original
	// alongside its extracted text. Off by default in effect — nothing is
	// written here unless a given upload explicitly asks for it (see
	// ingest.go's ingestUploadedFile), matching RedactPII/AllowShellExec's
	// same opt-in-per-action posture for anything that retains more than
	// the minimum needed data.
	OriginalsDir string `json:"originals_dir"`
	// TesseractBin is the executable used to OCR an uploaded image when no
	// vision-capable model is configured for the active profile (see
	// llmProfile.SupportsVision, runTesseractOCR in extract.go) — the same
	// "shell out to an external CLI, gated behind AllowShellExec" pattern
	// MarkItDownBin already uses. Empty defaults to "tesseract" (must
	// still be installed separately, e.g. `apt/brew/choco install
	// tesseract`).
	TesseractBin string `json:"tesseract_bin,omitempty"`
	// TesseractLang is tesseract's -l language argument, e.g. "deu+eng" to
	// recognize both German and English text in one pass. Empty defaults
	// to "deu+eng", matching this deployment's primary language.
	TesseractLang string `json:"tesseract_lang,omitempty"`
	// WhisperBin is the external Whisper-compatible executable used by the
	// local voice transcription endpoint. Empty uses "whisper-cli" from PATH.
	// The runner speaks the whisper.cpp CLI shape (-m/-f/-l/-otxt/-of), so the
	// binary and model remain entirely local and are never downloaded by R3.
	WhisperBin string `json:"whisper_bin,omitempty"`
	// WhisperModel is the local model file passed to Whisper's -m option.
	WhisperModel string `json:"whisper_model,omitempty"`
	// WhisperLanguage is the optional ISO language code passed to Whisper.
	// Empty lets Whisper auto-detect when the selected binary supports it.
	WhisperLanguage string `json:"whisper_language,omitempty"`
	// WhisperTimeoutSeconds bounds one transcription process. 0 uses 120s.
	WhisperTimeoutSeconds int `json:"whisper_timeout_seconds,omitempty"`
	// WhisperThreads sets whisper.cpp's --threads flag — how many CPU
	// threads one transcription process uses. 0 (default) omits the flag
	// entirely, leaving whisper.cpp's own built-in default (4) in effect.
	WhisperThreads int `json:"whisper_threads,omitempty"`
	// WhisperBeamSize sets whisper.cpp's --beam-size flag. whisper.cpp's
	// own default (5) favors accuracy over speed; 1 (greedy decoding) is
	// the standard low-latency trade for a short, informal voice-input
	// clip. 0 (default) omits the flag, keeping whisper.cpp's own default.
	WhisperBeamSize int `json:"whisper_beam_size,omitempty"`
	// WhisperFlashAttn adds --flash-attn, whisper.cpp's faster attention
	// kernel. Off by default — not every build/model/hardware combination
	// supports it identically, so this stays an explicit opt-in rather
	// than always-on.
	WhisperFlashAttn bool `json:"whisper_flash_attn,omitempty"`
	// WhisperVAD/WhisperVADModel add --vad --vad-model <path>, letting
	// whisper.cpp skip silent stretches (e.g. leading/trailing silence in
	// a short browser recording) before running the encoder — a real
	// speedup for typical push-to-talk clips. Both must be set together:
	// WhisperVAD without a WhisperVADModel path is rejected by
	// validateImportSettings, since whisper.cpp's --vad requires a model
	// file and a silent fallback would look like VAD is on when it isn't.
	WhisperVAD      bool   `json:"whisper_vad,omitempty"`
	WhisperVADModel string `json:"whisper_vad_model,omitempty"`
	// WhisperMaxConcurrent caps how many whisper.cpp/ffmpeg transcription
	// processes may run at once, server-wide (see whisper.go's
	// voiceTranscribeSemaphore) — each is a heavy, CPU-bound process that
	// reloads its full model from disk, so unbounded concurrency (e.g.
	// several colleagues push-to-talking at once) can thrash the host.
	// 0 uses the built-in default (2).
	WhisperMaxConcurrent int `json:"whisper_max_concurrent,omitempty"`
	// AllowInternalFetch lets the chat/agent fetch_url tool (agent.go's
	// fetchURLExecutor) follow URLs that resolve to a private/internal
	// address (RFC1918 etc.), not just the public internet — for pasting
	// links to an internal wiki/ticket/SharePoint page into chat. Off by
	// default, same opt-in-per-action posture as RedactPII/AllowShellExec:
	// fetch_url's result lands in chat, and chat citations aren't
	// admin-gated (see webimport.go's SSRF caveat), so an admin should
	// consciously accept that before any chat user's session can reach
	// internal-only endpoints through it. Loopback and link-local
	// addresses (127.0.0.0/8, 169.254.0.0/16 incl. cloud instance-metadata
	// services) stay blocked even when this is on — see isSafeWebURL.
	AllowInternalFetch bool `json:"allow_internal_fetch,omitempty"`
	// GraphMaxRetries overrides graph.go's graphMaxRetries fallback (4) for
	// how many extra attempts a Microsoft Graph call (SharePoint/Exchange/
	// Teams) makes after a 429/5xx before giving up. A tenant under heavy
	// throttling may need more retries to ride out sustained 429s
	// unattended during a large import; troubleshooting against a flaky
	// staging tenant may want fewer, to fail fast instead of waiting
	// through several rounds of backoff (up to 30s each). 0 = fallback.
	GraphMaxRetries int `json:"graph_max_retries,omitempty"`
	// ConnectorMaxRetries overrides connector.go's connectorMaxRetries
	// fallback (4) for the generic REST/HTTP-template connector's retry
	// loop (doWithRetry) — same reasoning as GraphMaxRetries, for
	// non-Graph connectors (Confluence, Jira, Freshservice, generic REST
	// templates). 0 = fallback.
	ConnectorMaxRetries int `json:"connector_max_retries,omitempty"`
	// RESTConnectorTimeoutSeconds overrides connector.go's connectorHTTPClient
	// hardcoded 30s timeout for the generic REST/HTTP-template connector —
	// a slow internal API (or one behind a high-latency VPN hop) may need
	// longer than 30s per request; a fast internal API behind a flaky
	// proxy may want a shorter timeout to fail over faster. 0 = 30s
	// fallback (connectorDefaultTimeoutSeconds).
	RESTConnectorTimeoutSeconds int `json:"rest_connector_timeout_seconds,omitempty"`
	// TeamsMaxRepliesPerThread overrides teams.go's teamsMaxReplies (200) —
	// how many of one channel thread's replies ride along in its thread
	// document (see teams.go's file header: a selected top-level post is
	// imported together with its replies as ONE document). 0 = default
	// (200). Higher bounds a very active thread's document size and Graph
	// request count (each 50-reply page costs one more call); lower trims
	// context for admins who only care about recent activity.
	TeamsMaxRepliesPerThread int `json:"teams_max_replies_per_thread,omitempty"`
	// EmbedBatchSize overrides store.go's replaceSourceChunks hardcoded 16
	// chunks-per-embed-call batch size. A larger batch means fewer HTTP
	// round-trips to the embedding endpoint for a big import (helps most
	// against a remote/cloud embedding API with per-call latency); a
	// smaller batch reduces the memory/time an admin waits to see the
	// first embedding failure, and matters more for a local/self-hosted
	// embedding model with a small max-batch limit of its own. 0 = default
	// (importEmbedBatchSizeDefault, import_limits.go).
	EmbedBatchSize int `json:"embed_batch_size,omitempty"`
}

func validateImportSettings(c importConfig) error {
	if c.WhisperTimeoutSeconds < 0 || c.WhisperTimeoutSeconds > 600 {
		return fmt.Errorf("whisper_timeout_seconds must be between 0 and 600")
	}
	if c.WhisperThreads < 0 {
		return fmt.Errorf("whisper_threads must be >= 0")
	}
	if c.WhisperBeamSize < 0 {
		return fmt.Errorf("whisper_beam_size must be >= 0")
	}
	if c.WhisperMaxConcurrent < 0 || c.WhisperMaxConcurrent > 32 {
		return fmt.Errorf("whisper_max_concurrent must be between 0 and 32")
	}
	if c.WhisperVAD && strings.TrimSpace(c.WhisperVADModel) == "" {
		return fmt.Errorf("whisper_vad_model is required when whisper_vad is enabled")
	}
	if c.TeamsMaxRepliesPerThread < 0 || c.TeamsMaxRepliesPerThread > 2000 {
		return fmt.Errorf("teams_max_replies_per_thread must be between 0 and 2000")
	}
	endpoint := strings.TrimSpace(c.MarkItDownDocIntelEndpoint)
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("markitdown_docintel_endpoint must be an https URL without credentials, query or fragment")
	}
	return nil
}

type settingsStore struct {
	mu       sync.Mutex
	path     string
	s        appSettings
	revision uint64
}

// settingsRevisionConflictError is returned when a browser tries to save a
// form snapshot that another administrator has already superseded. It is
// deliberately an in-memory generation rather than a persisted appSettings
// field: a server restart merely makes every newly loaded browser snapshot
// current again, while concurrent edits within one running instance fail
// safely instead of silently overwriting one another.
type settingsRevisionConflictError struct {
	Current uint64
}

func (e *settingsRevisionConflictError) Error() string {
	return "settings revision conflict"
}

var settings *settingsStore

// normalizeBaseURL strips trailing slashes and a trailing "/v1" so a
// profile's BaseURL is comparable/reusable regardless of whether the user
// pasted an OpenAI-style "/v1" endpoint or a bare host URL.
func normalizeBaseURL(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/v1") {
		u = strings.TrimSuffix(u, "/v1")
	}
	return strings.TrimRight(u, "/")
}

// defaultSettings seeds the "local" profile from CLI flags on first run.
// The "azure" profile starts empty — populate it via settings.json, the
// renderConfig configures where the browser loads the optional client-side
// rendering libraries from. The Chat/Agent UI can render ```mermaid diagrams
// and ```d3 charts; both need a JS library too large to bundle into the
// binary, so they load lazily — only when the assistant's output actually
// contains such a block — from these URLs. Both default to a public CDN
// (defaultMermaidURL/defaultD3URL); an air-gapped deployment points them at a
// self-hosted copy (any URL the browser can reach), or sets one to "" to
// disable that renderer entirely, in which case the source is shown as a
// labeled, copyable code block instead. JSON/XML/table/Markdown rendering
// needs no library and is always available regardless of these.
type renderConfig struct {
	MermaidURL string `json:"mermaid_url"`
	D3URL      string `json:"d3_url"`
}

// defaultMermaidURL/defaultD3URL are the CDN sources used when renderConfig
// leaves a URL unset. Mermaid is loaded as an ES module (jsDelivr's "+esm"
// endpoint bundles its dependencies into one importable module); d3 is loaded
// as a classic UMD script inside the sandboxed chart iframe (sets window.d3).
// Pinned to a major version so a breaking upstream release can't silently
// change rendering — bump deliberately.
const (
	defaultMermaidURL = "https://cdn.jsdelivr.net/npm/mermaid@11/+esm"
	defaultD3URL      = "https://cdn.jsdelivr.net/npm/d3@7/dist/d3.min.js"
)

// resolveRenderConfig fills unset URLs with the CDN defaults. A field
// explicitly set to a non-empty custom URL is kept; the sentinel "-" means
// "disable this renderer" and resolves to "" (the frontend then falls back to
// a code block). This keeps an absent/zero renderConfig (every settings.json
// written before this field existed) working with sensible defaults.
func resolveRenderConfig(c renderConfig) renderConfig {
	pick := func(v, def string) string {
		switch strings.TrimSpace(v) {
		case "":
			return def
		case "-":
			return ""
		default:
			return strings.TrimSpace(v)
		}
	}
	return renderConfig{
		MermaidURL: pick(c.MermaidURL, defaultMermaidURL),
		D3URL:      pick(c.D3URL, defaultD3URL),
	}
}

// Settings tab, or AZURE_OPENAI_* environment variables referenced through
// api_key_env.
func defaultSettings(localURL, chatModel, embedModel, lang string, chunkSize, k int) appSettings {
	s := appSettings{
		Version:          1,
		Lang:             lang,
		EmbedProfile:     "local",
		ChatProfile:      "local",
		ChunkSize:        chunkSize,
		K:                k,
		RedactPII:        false,
		PromptsDir:       "prompts",
		ChatHistoryPath:  "r3-chathistory.db",
		TokenUsagePath:   "r3-tokenusage.db",
		UserPrefsPath:    "r3-userprefs.db",
		AdminPasswordEnv: "R3_ADMIN_PASSWORD",
		Storage: storageSettings{
			Backend:     "tinysql",
			Mode:        "disk",
			Path:        "r3-data",
			MaxMemoryMB: 256,
		},
		Ranking: rankingConfig{
			VectorWeight:        0.7,
			KeywordWeight:       0.2,
			RecencyWeight:       0.1,
			RecencyHalfLifeDays: 180,
			CandidateLimit:      80,
			// See MinFinalScore's doc comment above for why 0.45 — calibrated
			// against the reported "irrelevant question still fills K" case.
			MinFinalScore: 0.45,
			// -1/-1: preserves the pre-existing "pull the whole source"
			// behavior for every deployment upgrading from a settings.json
			// that predates this feature — see the field doc comments above.
			ContextChunksBefore: -1,
			ContextChunksAfter:  -1,
		},
		Import: importConfig{
			MarkItDownBin:   "markitdown",
			FFmpegBin:       "ffmpeg",
			SevenZipBin:     "7z",
			MaxFileMB:       25,
			OriginalsDir:    "r3-originals",
			TesseractBin:    "tesseract",
			TesseractLang:   "deu+eng",
			WhisperBin:      "whisper-cli",
			WhisperLanguage: "de",
		},
		AllowShellExec: false,
		LDAP: ldapConfig{
			Enabled:      false,
			URL:          "ldaps://inf-pla-04.zitec-intern.de:636",
			BaseDN:       "ou=Zitec,dc=zitec-intern,dc=de",
			DomainPrefix: "ZITEC",
		},
		// SharePoint/ExchangeGraph/IMAP/Teams/Confluence/Jira/Freshservice
		// start as empty lists — a fresh install has zero connections
		// configured, same as the old zero-value single struct being
		// Enabled:false; the Settings UI's "+ Verbindung hinzufügen" adds
		// the first entry. IMAP.Port/UseTLS/Mailbox/ExchangeGraph.Folder
		// no longer have a seeded default here since there's no single
		// struct to seed — each new connection's card defaults those
		// fields client-side (web/app.js) instead.
		SMTP: smtpConfig{
			Enabled: false,
		},
		MSSQL: mssqlConfig{
			Enabled:                false,
			Port:                   1433,
			TrustServerCertificate: true,
			MaxRows:                200,
			TimeoutSeconds:         10,
			// Off by default alongside Enabled — see AllowGenericQuery's
			// doc comment above for why this needs its own deliberate
			// opt-in, not just inheriting Enabled's.
			AllowGenericQuery: false,
		},
		Shop: shopConfig{
			Enabled:        false,
			BaseURL:        "https://de.rubix.com",
			TimeoutSeconds: 10,
			MaxResults:     10,
		},
	}
	s.Profiles.Local = llmProfile{
		Provider:   "local",
		BaseURL:    normalizeBaseURL(localURL),
		ChatModel:  chatModel,
		EmbedModel: embedModel,
	}
	s.Profiles.Azure = llmProfile{
		Provider:  "azure",
		APIKeyEnv: "AZURE_OPENAI_API_KEY",
	}
	s.Profiles.OpenAI = llmProfile{Provider: "openai", BaseURL: "https://api.openai.com/v1", ChatModel: "gpt-4o-mini", APIKeyEnv: "OPENAI_API_KEY"}
	s.Profiles.OpenRouter = llmProfile{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", ChatModel: "openai/gpt-4o-mini", APIKeyEnv: "OPENROUTER_API_KEY"}
	s.Profiles.Claude = llmProfile{Provider: "claude", BaseURL: "https://api.anthropic.com", ChatModel: "claude-3-5-sonnet-20241022", APIKeyEnv: "ANTHROPIC_API_KEY"}
	s.Profiles.Gemini = llmProfile{Provider: "gemini", BaseURL: "https://generativelanguage.googleapis.com", ChatModel: "gemini-2.0-flash", APIKeyEnv: "GEMINI_API_KEY"}
	return s
}

// legacySingularConnectorKeys lists every appSettings JSON key that used to
// hold a single connector-config object and is now a list (SharePoint/
// ExchangeGraph/IMAP/Teams/Confluence/Jira/Freshservice) — see
// migrateLegacySingularConnectors.
var legacySingularConnectorKeys = []string{
	"sharepoint", "exchange_graph", "imap", "teams", "confluence", "jira", "freshservice",
}

// migrateLegacySingularConnectors rewrites a settings.json's raw bytes so
// every legacySingularConnectorKeys entry still in the old single-object
// shape becomes a one-element list instead, before loadOrCreateSettings's
// usual default-merge unmarshal runs. That merge (json.Unmarshal onto an
// already-populated struct) only works when the on-disk shape matches the
// target field's type — a bare object can't merge onto a slice field — so
// this is a real byte-level migration, the first non-additive change
// appSettings has ever needed (every prior change only added fields).
//
// A key that's absent, null, or already a JSON array is left untouched —
// this function is a no-op after the first run against any given file. A
// migrated single object gets "name":"default" injected if it doesn't
// already have a name, so it round-trips into the new connRuntime.Name
// field instead of leaving the migrated connection blank-named.
func migrateLegacySingularConnectors(data []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	changed := false
	for _, key := range legacySingularConnectorKeys {
		raw, ok := root[key]
		if !ok {
			continue
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue // already a list, null, or absent — nothing to migrate
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, fmt.Errorf("migrate %q: %w", key, err)
		}
		if _, hasName := obj["name"]; !hasName {
			nameJSON, err := json.Marshal("default")
			if err != nil {
				return nil, err
			}
			obj["name"] = nameJSON
		}
		objRaw, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		listRaw := make([]byte, 0, len(objRaw)+2)
		listRaw = append(listRaw, '[')
		listRaw = append(listRaw, objRaw...)
		listRaw = append(listRaw, ']')
		root[key] = listRaw
		changed = true
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(root)
}

// migrateLegacyOpenAIAPIConfig upgrades a pre-multi-endpoint
// settings.openai_api object ({enabled, port, preset}) into the current
// {enabled, port, endpoints: [...]} shape (see openAIAPIConfig/
// openAIEndpointConfig's doc comments), synthesizing one endpoint with
// Name "" — the unprefixed root path, so an existing integration already
// pointed at http://host:port/v1/chat/completions keeps working unchanged —
// that reproduces the old always-on behavior (RAG AND tools both enabled,
// using the old preset). A no-op if openai_api is absent, already carries
// an "endpoints" key (already migrated), or has no "preset" key (a fresh,
// still-default config with nothing to preserve). Same technique as
// migrateLegacySingularConnectors above, applied to one nested field
// instead of a whole top-level key.
func migrateLegacyOpenAIAPIConfig(data []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	raw, ok := root["openai_api"]
	if !ok {
		return data, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("migrate openai_api: %w", err)
	}
	if _, hasEndpoints := obj["endpoints"]; hasEndpoints {
		return data, nil
	}
	presetRaw, hasPreset := obj["preset"]
	if !hasPreset {
		return data, nil
	}
	var presetName string
	if err := json.Unmarshal(presetRaw, &presetName); err != nil {
		return nil, fmt.Errorf("migrate openai_api: preset: %w", err)
	}
	endpoint := map[string]any{
		"name": "", "enabled": true, "enable_rag": true, "enable_tools": true, "preset": presetName,
	}
	endpointRaw, err := json.Marshal(endpoint)
	if err != nil {
		return nil, err
	}
	endpointsRaw, err := json.Marshal([]json.RawMessage{endpointRaw})
	if err != nil {
		return nil, err
	}
	obj["endpoints"] = endpointsRaw
	delete(obj, "preset")
	objRaw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	root["openai_api"] = objRaw
	return json.Marshal(root)
}

// loadOrCreateSettings reads settings.json at path, or seeds it from
// defaults on first run.
//
// Any field missing from an on-disk settings.json — because it predates a
// later release that introduced it, at any nesting depth (a whole new
// connector struct, or one new field bolted onto an existing one) — is
// automatically backfilled from defaults. The mechanism is encoding/json's
// own merge behavior, not a hand-maintained list: unmarshaling a JSON
// object into an *already-populated* struct value only overwrites the
// fields that actually appear as keys in that object, at any depth —
// every field the object doesn't mention keeps whatever value it already
// had. So instead of unmarshaling into a zero-value appSettings{} and then
// patching specific known-stale fields by hand (the previous approach,
// which quietly stopped covering new settings the moment someone forgot
// to add a line here), ss.s starts out *equal to* defaults and the file's
// JSON is unmarshaled on top of that: whatever the file actually defines
// wins — even a deliberately unusual value like 0 — and whatever it
// doesn't mention falls back to today's defaults. This also means a
// brand-new nested config struct (e.g. a future connector) never needs an
// entry here at all: it simply stays at its default until the file is
// re-saved with real values, exactly like every other field.
//
// saveLocked below then writes the now-complete struct straight back to
// path, so the on-disk file itself grows to include every newly-added
// field/default the moment it's next loaded — not just in the in-memory
// copy — without requiring an admin to touch the Settings UI first.
func loadOrCreateSettings(path string, defaults appSettings) (*settingsStore, error) {
	ss := &settingsStore{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			ss.s = defaults
			if err := ss.saveLocked(); err != nil {
				return nil, err
			}
			return ss, nil
		}
		return nil, err
	}
	data, err = migrateLegacySingularConnectors(data)
	if err != nil {
		return nil, fmt.Errorf("settings JSON migration error: %w", err)
	}
	data, err = migrateLegacyOpenAIAPIConfig(data)
	if err != nil {
		return nil, fmt.Errorf("settings JSON migration error: %w", err)
	}
	ss.s = defaults
	if err := json.Unmarshal(data, &ss.s); err != nil {
		return nil, fmt.Errorf("settings JSON parse error: %w", err)
	}
	// Unlike the two raw-JSON migrations above (which reshape structure),
	// this one only rewrites a string field's contents, so it runs on the
	// already-unmarshaled struct — see migrateLegacySQLTemplateParamSyntax's
	// own doc comment for why it's safe to always run, not just once.
	migrateLegacySQLTemplateParamSyntax(ss.s.MSSQL.QueryTemplates)
	// The vector space is local-only; normalize legacy/cloud selections while
	// loading so the persisted settings and the runtime agree.
	ss.s.EmbedProfile = "local"
	// Normalization, not defaulting — applies regardless of whether the
	// value came from the file or from defaults above.
	ss.s.Profiles.Local.BaseURL = normalizeBaseURL(ss.s.Profiles.Local.BaseURL)
	_ = ss.save()
	return ss, nil
}

// get returns a copy of the current settings snapshot, safe to use without
// holding any lock afterward. A nil receiver (the global store not yet
// initialized — e.g. a unit test that never called withTestGlobalSettings)
// returns the zero appSettings rather than panicking, so read-only helpers
// that consult settings.get() for a default (import_limits.go's caps) stay
// safe to call from any context.
func (ss *settingsStore) get() appSettings {
	if ss == nil {
		return appSettings{}
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.s
}

// getWithRevision returns one coherent settings snapshot and its in-memory
// generation. HTTP settings clients send this generation back when saving so
// the server can reject a stale whole-form update instead of losing an
// unrelated admin's newer change.
func (ss *settingsStore) getWithRevision() (appSettings, uint64) {
	if ss == nil {
		return appSettings{}, 0
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.s, ss.revision
}

// update applies fn to a copy under lock and persists the result before
// publishing it. It deliberately does not advance the Settings-form revision:
// callers also use this low-level helper for operational cursors and API-key
// last-used timestamps, neither of which should make an administrator's
// unrelated form look stale.
func (ss *settingsStore) update(fn func(*appSettings)) error {
	if ss == nil {
		return fmt.Errorf("settings store is not initialized")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	after := ss.s
	fn(&after)
	if err := ss.saveValueLocked(after); err != nil {
		return err
	}
	ss.s = after
	return nil
}

// updateIfRevision applies fn to a copy, persists that copy, and swaps it
// into memory only after the atomic file replace succeeds. Besides preventing
// a failed disk write from leaving runtime and settings.json out of sync, the
// optional expected revision provides compare-and-swap semantics for the
// Settings form without breaking existing callers that don't send one.
func (ss *settingsStore) updateIfRevision(expected uint64, requireMatch bool, fn func(*appSettings)) (before, after appSettings, revision uint64, err error) {
	if ss == nil {
		return appSettings{}, appSettings{}, 0, fmt.Errorf("settings store is not initialized")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if requireMatch && expected != ss.revision {
		return ss.s, ss.s, ss.revision, &settingsRevisionConflictError{Current: ss.revision}
	}
	before = ss.s
	after = ss.s
	fn(&after)
	if err := ss.saveValueLocked(after); err != nil {
		return before, before, ss.revision, err
	}
	ss.s = after
	ss.revision++
	return before, after, ss.revision, nil
}

// save persists the current settings to disk, acquiring the lock itself —
// use this from outside settingsStore; saveLocked is for callers that
// already hold ss.mu.
func (ss *settingsStore) save() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.saveLocked()
}

// saveLocked writes settings.json via a temp file + rename so a crash or
// concurrent read mid-write never leaves a truncated/corrupt file on disk.
// Caller must already hold ss.mu.
//
// 0o600 (owner read/write only), not 0o644: settings.json holds every
// connector credential in plaintext (SMTP/MSSQL/Shop/SharePoint/Exchange/
// IMAP/Teams/Confluence/Jira/Freshservice passwords, client secrets, API
// tokens — see each config struct's Password/ClientSecret/APIToken
// fields) — see docs/ENTERPRISE_READINESS.md for why this isn't
// encrypted-at-rest yet. Nothing else on this host needs to read the
// file, so there's no reason it should be world-/group-readable. Because
// tmp is a fresh file every save (O_CREATE truncates but does NOT change
// an existing file's mode — only file *creation* applies the mode
// argument), even a settings.json created before this comment existed
// converges to 0o600 on its very next save via the rename below.
func (ss *settingsStore) saveLocked() error {
	return ss.saveValueLocked(ss.s)
}

// saveValueLocked writes value through the same owner-only temp-file replace
// as saveLocked. Keeping it separate lets updateIfRevision validate/persist a
// candidate before publishing it to readers. Caller must hold ss.mu.
func (ss *settingsStore) saveValueLocked(value appSettings) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}
	b = append(b, '\n')
	tmp := ss.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ss.path)
}

// activeEmbedModel returns the local LM-Studio model used for every embedding
// call. Every chunk row's embed_model column and every retrieval query's
// embed_model filter must agree on this value, or vector search silently
// returns zero rows because the WHERE clause excludes everything. Chat
// providers never change this vector space.
func (s appSettings) activeEmbedModel() string {
	// The vector index is intentionally anchored to the local/LM Studio
	// embedding model. Chat providers may change freely without invalidating
	// the stored vector space or triggering a re-index.
	return s.Profiles.Local.EmbedModel
}

// citationsVisible reports whether sources of the given source_kind should
// be shown to the user as a citation — see SourceVisibility's doc comment.
// A kind with no explicit entry defaults to visible.
func (s appSettings) citationsVisible(kind string) bool {
	visible, ok := s.SourceVisibility[kind]
	if !ok {
		return true
	}
	return visible
}

// buildLLMClients constructs the local embed client and the map of chat clients
// (local, Azure, OpenAI, OpenRouter, Claude and Gemini) from current settings.
// Called at startup and again whenever settings are updated via the API so
// profile edits take effect immediately.
func buildLLMClients(s appSettings) (embed *lmClient, chat map[string]*lmClient) {
	local := newLMClientFromProfile(s.Profiles.Local)
	chat = map[string]*lmClient{"local": local}
	if strings.TrimSpace(s.Profiles.Azure.BaseURL) != "" {
		chat["azure"] = newLMClientFromProfile(s.Profiles.Azure)
	}
	for _, entry := range []struct {
		name    string
		profile llmProfile
	}{{"openai", s.Profiles.OpenAI}, {"openrouter", s.Profiles.OpenRouter}, {"claude", s.Profiles.Claude}, {"gemini", s.Profiles.Gemini}} {
		if strings.TrimSpace(entry.profile.ChatModel) != "" {
			chat[entry.name] = newLMClientFromProfile(entry.profile)
		}
	}

	embed = local
	return embed, chat
}

// supportedUILangs are the locales web/i18n.js's MESSAGES actually
// translates — the Settings language picker offers exactly these, and
// handleSettings rejects anything else via isSupportedUILang so a typo'd
// or stale client can't silently persist an untranslated locale.
var supportedUILangs = []string{"de", "en", "fr", "it"}

func isSupportedUILang(lang string) bool {
	for _, l := range supportedUILangs {
		if l == lang {
			return true
		}
	}
	return false
}

// configuredSourceKinds reports which knowledge-base import connectors have
// at least one enabled connection configured — used by handleAuthStatus so
// the Help tab can describe only the sources actually in play at this
// deployment instead of a fixed list naming every connector R3 supports
// (e.g. mentioning Jira when no Jira connection is enabled). File upload
// and Web/RSS import have no persistent Enabled gate of their own (upload
// is always available; a Web/RSS import is a one-off admin action, not a
// standing connection) — the Help tab treats those as always-on separately
// from this list. Mirrors configuredChatProfiles' shape: a plain []string
// of active kind tags, built with firstEnabledConn (connruntime.go) so a
// newly added connector type only needs one more line here, not a new
// per-type helper.
func configuredSourceKinds(s appSettings) []string {
	var kinds []string
	if _, ok := firstEnabledConn(s.SharePoint); ok {
		kinds = append(kinds, "sharepoint")
	}
	if _, ok := firstEnabledConn(s.OneDrive); ok {
		kinds = append(kinds, "onedrive")
	}
	if _, ok := firstEnabledConn(s.ExchangeGraph); ok {
		kinds = append(kinds, "exchange_graph")
	}
	if _, ok := firstEnabledConn(s.IMAP); ok {
		kinds = append(kinds, "imap")
	}
	if _, ok := firstEnabledConn(s.Teams); ok {
		kinds = append(kinds, "teams")
	}
	if _, ok := firstEnabledConn(s.Confluence); ok {
		kinds = append(kinds, "confluence")
	}
	if _, ok := firstEnabledConn(s.Jira); ok {
		kinds = append(kinds, "jira")
	}
	if _, ok := firstEnabledConn(s.Freshservice); ok {
		kinds = append(kinds, "freshservice")
	}
	if _, ok := firstEnabledConn(s.Folder); ok {
		kinds = append(kinds, "folder")
	}
	if _, ok := firstEnabledConn(s.GitHub); ok {
		kinds = append(kinds, "github")
	}
	if _, ok := firstEnabledConn(s.SAPS4); ok {
		kinds = append(kinds, "sap_s4")
	}
	return kinds
}

// configuredToolKinds reports which live/agent tools are actually usable
// from settings alone — the Help tab's "available tools" counterpart to
// configuredSourceKinds above. MSSQL/Shop/HTTP-templates/generic REST
// connectors CAN additionally be narrowed further per caller (LDAP session
// state, department AccessControl allow-lists, the resolved preset's Tools
// axis — see buildLiveTools, agent.go) — this reports only the
// admin-configured baseline ("is this enabled at all"), not a specific
// caller's actual final access, since the Help tab has no fixed preset/
// department to resolve against in general. The frontend adds one generic
// caveat sentence for that instead of trying to replicate per-caller
// resolution here.
func configuredToolKinds(s appSettings) []string {
	var kinds []string
	if s.MSSQL.Enabled {
		kinds = append(kinds, "mssql")
	}
	if s.Shop.Enabled {
		kinds = append(kinds, "shop")
	}
	for _, t := range s.HTTPTemplates {
		if t.Enabled {
			kinds = append(kinds, "http")
			break
		}
	}
	for _, c := range s.RESTConnectors {
		if c.Enabled {
			kinds = append(kinds, "rest_connectors")
			break
		}
	}
	if len(sharePointLiveSearchConns(s.SharePoint)) > 0 {
		kinds = append(kinds, "sharepoint_search")
	}
	if s.Agent.AllowWebFetch {
		kinds = append(kinds, "fetch_url")
	}
	if s.Agent.AllowWebFetch && s.Agent.AllowWebResearch {
		kinds = append(kinds, "web_research")
	}
	if s.Agent.AllowWebSearch {
		kinds = append(kinds, "web_search")
	}
	if s.Agent.AllowAzureBingSearch && strings.TrimSpace(s.Profiles.Azure.BaseURL) != "" && strings.TrimSpace(s.Profiles.Azure.ChatModel) != "" {
		kinds = append(kinds, "azure_bing_search")
	}
	if !s.Agent.SubagentsDisabled {
		kinds = append(kinds, "subagents")
	}
	return kinds
}

// configuredChatProfiles reports which cloud chat profiles are actually
// usable — same BaseURL/ChatModel gating buildLLMClients uses, plus (for
// openai/openrouter/claude/gemini) a resolved, non-empty API key, since
// ChatModel alone is pre-seeded by defaultSettings even with zero
// credentials configured (see those four Profiles.* assignments). "local"
// is deliberately excluded — the chat dropdown's Standard/Lokal options
// are never gated, same as before this existed. Used by handleAuthStatus
// so web/app.js can hide any dropdown option that would otherwise fail at
// request time with a confusing upstream auth error.
func configuredChatProfiles(s appSettings) []string {
	names := []string{}
	if strings.TrimSpace(s.Profiles.Azure.BaseURL) != "" {
		names = append(names, "azure")
	}
	for _, entry := range []struct {
		name    string
		profile llmProfile
	}{{"openai", s.Profiles.OpenAI}, {"openrouter", s.Profiles.OpenRouter}, {"claude", s.Profiles.Claude}, {"gemini", s.Profiles.Gemini}} {
		if strings.TrimSpace(entry.profile.ChatModel) == "" {
			continue
		}
		key := entry.profile.APIKey
		if key == "" && entry.profile.APIKeyEnv != "" {
			key = os.Getenv(entry.profile.APIKeyEnv)
		}
		if strings.TrimSpace(key) != "" {
			names = append(names, entry.name)
		}
	}
	return names
}
