package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// verboseMiddleware logs method, path and duration for every request when
// verbose mode is on (-verbose, wired to `make dev`; a no-op otherwise, so
// production/background runs stay quiet). Model-level detail ("which model
// was used how") is logged separately at the point of each actual LLM
// call — see llm.go's embed()/chatStream() — since that's the one place
// guaranteed to see every embed/chat call regardless of which handler
// triggered it.
func verboseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !verbose {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("[verbose] %s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}

// sessionCacheMiddleware parses the caller's session cookie (session.go)
// once per request and stashes the result in the request context, so every
// handler/helper downstream that calls currentSession(r) — there are
// around a dozen (requireAdminSession, userContextBlock, handleAsk, ...) —
// reuses that one parse instead of each re-decoding/HMAC-verifying/
// unmarshaling the cookie independently.
func sessionCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, withSessionCache(r))
	})
}

// registerRoutes wires every HTTP endpoint onto mux, including the static
// frontend assets and the health check. Doubles as the map of which
// routes are admin-gated (requireAdminSession) vs. API-key-gated
// (requireAPIKey) vs. wide open — see the inline comments below for why
// each ungated route is safe to leave that way.
func registerRoutes(mux *http.ServeMux, rag *ragSystem) {
	// Ungated, no dependencies checked — a liveness probe (load balancer,
	// k8s, uptime monitor) just needs to know the process is up and
	// serving HTTP; it shouldn't fail because the configured LLM backend
	// or a downstream connector happens to be unreachable right now.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	// Prometheus-style metrics are protected by R3_METRICS_TOKEN inside
	// handleMetrics, rather than inheriting browser-session auth.
	mux.HandleFunc("/metrics", handleMetrics)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, indexHTML)
	})
	mux.HandleFunc("/style.css", serveStaticAsset("text/css; charset=utf-8", styleCSS))
	mux.HandleFunc("/app.js", serveStaticAsset("application/javascript; charset=utf-8", appJS))
	mux.HandleFunc("/i18n.js", serveStaticAsset("application/javascript; charset=utf-8", i18nJS))
	mux.HandleFunc("/novapop.js", serveStaticAsset("application/javascript; charset=utf-8", novapopJS))

	mux.HandleFunc("/api/settings", requireAdminSession(handleSettings(rag)))
	mux.HandleFunc("/api/settings/history", requireAdminSession(handleSettingsHistory))
	mux.HandleFunc("/api/settings/export", requireAdminSession(handleSettingsExport))
	// Local user accounts (localusers.go/handlers_local_users.go) — an
	// alternative/complement to LDAP, see appSettings.LocalAuth's doc
	// comment. Admin-only CRUD, same requireAdminSession gate as every
	// other admin route above/below.
	mux.HandleFunc("/api/admin/users", requireAdminSession(handleLocalUsersList))
	mux.HandleFunc("/api/admin/users/create", requireAdminSession(handleLocalUserCreate))
	mux.HandleFunc("/api/admin/users/update", requireAdminSession(handleLocalUserUpdate))
	mux.HandleFunc("/api/admin/users/password", requireAdminSession(handleLocalUserSetPassword))
	mux.HandleFunc("/api/admin/users/delete", requireAdminSession(handleLocalUserDelete))
	// Token-usage log/chart (tokenusage.go) — admin-gated like the change
	// history above, since per-user token counts are operational/cost data,
	// not something every logged-in employee should see about each other.
	mux.HandleFunc("/api/token-usage", requireAdminSession(handleTokenUsage))
	// "Verbindung testen" endpoints — one per external interface configured
	// in the Settings tab (see conntest.go). Each takes that connector's
	// not-yet-saved form values directly, so an admin can validate
	// credentials before clicking "Einstellungen speichern".
	mux.HandleFunc("/api/settings/test/llm", requireAdminSession(handleTestLLM))
	mux.HandleFunc("/api/settings/test/llm-models", requireAdminSession(handleTestLLMModels))
	mux.HandleFunc("/api/settings/test/ldap", requireAdminSession(handleTestLDAP))
	mux.HandleFunc("/api/settings/test/sharepoint", requireAdminSession(handleTestSharePoint))
	mux.HandleFunc("/api/settings/test/onedrive", requireAdminSession(handleTestOneDrive))
	mux.HandleFunc("/api/settings/test/exchange", requireAdminSession(handleTestExchange))
	mux.HandleFunc("/api/settings/test/imap", requireAdminSession(handleTestIMAP))
	mux.HandleFunc("/api/settings/test/teams", requireAdminSession(handleTestTeams))
	mux.HandleFunc("/api/settings/test/confluence", requireAdminSession(handleTestConfluence))
	mux.HandleFunc("/api/settings/test/jira", requireAdminSession(handleTestJira))
	mux.HandleFunc("/api/settings/test/freshservice", requireAdminSession(handleTestFreshservice))
	mux.HandleFunc("/api/settings/test/folder", requireAdminSession(handleTestFolder))
	mux.HandleFunc("/api/settings/test/github", requireAdminSession(handleTestGitHub))
	mux.HandleFunc("/api/settings/test/sap-s4", requireAdminSession(handleTestSAPS4))
	mux.HandleFunc("/api/settings/test/smtp", requireAdminSession(handleTestSMTP))
	mux.HandleFunc("/api/settings/test/mssql", requireAdminSession(handleTestMSSQL))
	mux.HandleFunc("/api/settings/test/mssql-template", requireAdminSession(handleTestMSSQLTemplate))
	mux.HandleFunc("/api/settings/test/http-template", requireAdminSession(handleTestHTTPTemplate))
	mux.HandleFunc("/api/settings/test/shop", requireAdminSession(handleTestShop))
	mux.HandleFunc("/api/settings/test/shop-login", requireAdminSession(handleTestShopLogin))
	// Scheduler dashboard (see scheduler.go) — history, live per-job
	// status, ad-hoc runs, cancel and pause/resume. All admin-only: same
	// gate as the settings/test endpoints above, since it's operational
	// control over the same connectors, not end-user-facing data.
	mux.HandleFunc("/api/scheduler/history", requireAdminSession(handleSchedulerHistory))
	mux.HandleFunc("/api/scheduler/status", requireAdminSession(handleSchedulerStatus(rag)))
	mux.HandleFunc("/api/scheduler/run", requireAdminSession(handleSchedulerRun(rag)))
	mux.HandleFunc("/api/scheduler/cancel", requireAdminSession(handleSchedulerCancel))
	mux.HandleFunc("/api/scheduler/pause", requireAdminSession(handleSchedulerPause(rag)))
	mux.HandleFunc("/api/scheduler/reset-cursor", requireAdminSession(handleSchedulerResetCursor(rag)))
	mux.HandleFunc("/api/scheduler/alerts", requireAdminSession(handleSchedulerAlerts))
	mux.HandleFunc("/api/scheduler/alerts/ack", requireAdminSession(handleSchedulerAlertAcknowledge))
	// Admin notification feed (notifications.go) — polled by every logged-in
	// admin's browser (web/app.js's EventSource against the /stream route)
	// to surface server-side events (e.g. a scheduler job finishing) as a
	// novapop.js toast, regardless of which tab is open. Admin-gated for the
	// same reason as the scheduler endpoints above.
	mux.HandleFunc("/api/admin/notifications", requireAdminSession(handleAdminNotifications))
	mux.HandleFunc("/api/admin/notifications/stream", requireAdminSession(handleAdminNotificationsStream))
	// Agent tool-execution audit log (agent.go) — admin-gated: arguments
	// and result previews can contain content source_access would hide
	// from non-admins.
	mux.HandleFunc("/api/agent/audit", requireAdminSession(handleAgentAudit))
	// Live operational view: current LDAP presence and active Agent-tier
	// processes, deliberately admin-only because it exposes staff identity
	// and live workload metadata (never prompts/tool payloads).
	mux.HandleFunc("/api/admin/operations", requireAdminSession(handleOperationsStatus))
	// Ungated: an OpenAPI spec is documentation, not data — describes
	// /api/ask, /api/search and /api/apikeys' shapes for tools like
	// Swagger Editor/Postman/codegen (see docs/API.md), same reasoning as
	// why most public APIs don't gate their own spec file behind auth.
	mux.HandleFunc("/api/openapi.json", handleOpenAPISpec)
	mux.HandleFunc("/llms.txt", handleLLMsTxt)
	// /api/docs is an interactive OpenAPI/Swagger-style viewer for the spec
	// above — documentation, not data, ungated for the same reason.
	mux.HandleFunc("/api/docs", handleAPIDocs)
	// MCP is authenticated internally on every request. Unlike /api/ask it
	// has no opt-out mode: a remote tool server must never be anonymous.
	mux.HandleFunc("/mcp", handleMCP(rag))
	mux.HandleFunc("/openai-api", handleOpenAIAPIPage)
	mux.HandleFunc("/apidocs.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		io.WriteString(w, apiDocsJS)
	})
	mux.HandleFunc("/api/stats", handleStats(rag))
	// Operational storage telemetry (cache hit rate, load/eviction counts,
	// memory-vs-disk footprint, oversized/thrash flag) — admin-gated, unlike
	// the ungated /api/stats above which stays limited to non-sensitive
	// aggregates. See handlers_storage.go.
	mux.HandleFunc("/api/admin/storage", requireAdminSession(handleStorageStats(rag)))
	mux.HandleFunc("/api/presets", handlePresets)
	mux.HandleFunc("/api/ask", requireAPIKey(handleAsk(rag)))
	// Local voice transcription uses the same optional API-key policy as
	// /api/ask, but never stores audio or transcript data by itself.
	mux.HandleFunc("/api/voice/transcribe", requireAPIKey(handleVoiceTranscribe))
	mux.HandleFunc("/api/search", requireAPIKey(handleSearch(rag)))
	mux.HandleFunc("/api/apikeys", requireAdminSession(handleAPIKeys))
	mux.HandleFunc("/api/apikeys/revoke", requireAdminSession(handleRevokeAPIKey))
	mux.HandleFunc("/api/upload", requireAdminSession(handleUpload(rag)))
	mux.HandleFunc("/api/import/folder", requireAdminSession(handleImportFolder(rag)))
	// R3 self-ingestion (selfsource.go) — "load R3's own source code into
	// R3's own vector store so it can answer questions about itself".
	// Admin-only, always an explicit one-off action.
	mux.HandleFunc("/api/import/self-source", requireAdminSession(handleImportSelfSource(rag)))
	mux.HandleFunc("/api/import/pst/preview", requireAdminSession(handleImportPSTPreview(rag)))
	mux.HandleFunc("/api/import/pst/preview-path", requireAdminSession(handleImportPSTPreviewPath(rag)))
	mux.HandleFunc("/api/import/pst", requireAdminSession(handleImportPST(rag)))
	mux.HandleFunc("/api/import/pst/status", requireAdminSession(handleImportPSTStatus))
	mux.HandleFunc("/api/import/pst/cancel", requireAdminSession(handleImportPSTCancel))
	mux.HandleFunc("/api/import/pst/jobs", requireAdminSession(handleImportPSTJobs))
	mux.HandleFunc("/api/import/sharepoint/preview", requireAdminSession(handleSharePointPreview(rag)))
	mux.HandleFunc("/api/import/sharepoint", requireAdminSession(handleSharePointImport(rag)))
	mux.HandleFunc("/api/import/sharepoint/delta-sync", requireAdminSession(handleSharePointDeltaSync(rag)))
	// Site Pages (.aspx wiki/news/landing pages) — a separate Graph API
	// surface from the document-library routes above, see sharepoint.go's
	// package comment above spListPages/spGetPageText.
	mux.HandleFunc("/api/import/sharepoint/pages/preview", requireAdminSession(handleSharePointPagesPreview))
	mux.HandleFunc("/api/import/sharepoint/pages", requireAdminSession(handleSharePointPagesImport(rag)))
	// Individually-shared links (SharePoint's own "Copy link" action) — no
	// preview/checklist step, see spShareLinkImportRequest's doc comment.
	mux.HandleFunc("/api/import/sharepoint/sharelink", requireAdminSession(handleSharePointShareLinkImport(rag)))
	mux.HandleFunc("/api/import/onedrive/sync", requireAdminSession(handleOneDriveSync(rag)))
	// Discover: recursive, read-only structure preview (discover.go) —
	// distinct from the /preview endpoints above, which list one folder
	// level at a time for the actual file/item-selection pickers.
	mux.HandleFunc("/api/import/sharepoint/discover", requireAdminSession(handleDiscoverSharePoint))
	mux.HandleFunc("/api/import/folder/discover", requireAdminSession(handleDiscoverFolder))
	mux.HandleFunc("/api/import/exchange/discover", requireAdminSession(handleDiscoverExchange))
	mux.HandleFunc("/api/import/exchange/preview", requireAdminSession(handleExchangeMailPreview(rag)))
	mux.HandleFunc("/api/import/exchange", requireAdminSession(handleExchangeMailImport(rag)))
	mux.HandleFunc("/api/import/imap", requireAdminSession(handleIMAPImport(rag)))
	mux.HandleFunc("/api/import/teams/preview", requireAdminSession(handleTeamsPreview(rag)))
	mux.HandleFunc("/api/import/teams", requireAdminSession(handleTeamsImport(rag)))
	mux.HandleFunc("/api/import/confluence/preview", requireAdminSession(handleConfluencePreview(rag)))
	mux.HandleFunc("/api/import/confluence", requireAdminSession(handleConfluenceImport(rag)))
	mux.HandleFunc("/api/import/jira/preview", requireAdminSession(handleJiraPreview(rag)))
	mux.HandleFunc("/api/import/jira", requireAdminSession(handleJiraImport(rag)))
	mux.HandleFunc("/api/import/freshservice/preview", requireAdminSession(handleFreshservicePreview(rag)))
	mux.HandleFunc("/api/import/freshservice", requireAdminSession(handleFreshserviceImport(rag)))
	mux.HandleFunc("/api/import/github/sync", requireAdminSession(handleGitHubSync(rag)))
	mux.HandleFunc("/api/import/sap-s4/sync", requireAdminSession(handleSAPS4Sync(rag)))
	mux.HandleFunc("/api/import/web", requireAdminSession(handleWebImport(rag)))
	mux.HandleFunc("/api/import/rss", requireAdminSession(handleRSSImport(rag)))
	mux.HandleFunc("/api/sources", requireAdminSession(handleSources(rag)))
	mux.HandleFunc("/api/sources/refresh", requireAdminSession(handleSourceRefresh(rag)))
	mux.HandleFunc("/api/sources/delete", requireAdminSession(handleSourceDelete(rag)))
	mux.HandleFunc("/api/sources/delete-by-kind", requireAdminSession(handleSourceDeleteByKind(rag)))
	mux.HandleFunc("/api/sources/delete-by-prefix", requireAdminSession(handleSourceDeleteByPrefix(rag)))
	mux.HandleFunc("/api/sources/delete-by-filter", requireAdminSession(handleSourceDeleteByFilter(rag)))
	mux.HandleFunc("/api/sources/acl", requireAdminSession(handleSourceACL(rag)))
	// content/original/draft-reply stay ungated (no session required):
	// reachable from the chat citation popup for any user who can already
	// see that citation, not just admins. That does NOT skip source_access,
	// though — each handler calls sourceAccessAllowedForRequest so a
	// department-restricted source_kind still can't be fetched by
	// source_id alone. See handleSourceContent's doc comment.
	mux.HandleFunc("/api/sources/content", handleSourceContent(rag))
	mux.HandleFunc("/api/sources/original", handleSourceOriginal(rag))
	mux.HandleFunc("/api/draft/reply", handleDraftReply(rag))
	mux.HandleFunc("/api/draft/restyle", handleDraftRestyle(rag))
	// Writing a draft into the shared service mailbox is an operator
	// action, unlike generating one — see handleDraftSaveIMAP's doc
	// comment for why the gates differ.
	mux.HandleFunc("/api/draft/save-imap", requireAdminSession(handleDraftSaveIMAP))
	// Same gate as /api/draft/reply above — see handleDraftEml's doc
	// comment for why only the attachment-carrying case needs a server
	// round-trip at all.
	mux.HandleFunc("/api/draft/eml", handleDraftEml)
	// Interactive, per-user Exchange mailbox access (mail_graph.go) — each
	// handler resolves its own session+authorization via
	// requireInteractiveExchangeConn, so no wrapper middleware is needed
	// here (same self-contained-gate shape as handleDraftReply above).
	mux.HandleFunc("/api/mail/graph/options", handleMailGraphOptions)
	mux.HandleFunc("/api/mail/graph/folders", handleMailGraphFolders)
	mux.HandleFunc("/api/mail/graph/list", handleMailGraphList)
	mux.HandleFunc("/api/mail/graph/message", handleMailGraphMessage)
	mux.HandleFunc("/api/mail/graph/save-draft", handleMailGraphSaveDraft)
	// Ungated like the routes above (see their comment) for the same
	// reason: reachable by any logged-in user, not just admins —
	// handleChatEmail itself still requires *a* valid session inline.
	mux.HandleFunc("/api/chat/email", handleChatEmail(rag))
	// Ungated like /api/ask itself (feedback.go's handleFeedback doc
	// comment) — rating an answer isn't more sensitive than having asked
	// the question that produced it.
	mux.HandleFunc("/api/feedback", handleFeedback)
	mux.HandleFunc("/api/feedback/stats", requireAdminSession(handleFeedbackStats))
	mux.HandleFunc("/api/chunks", requireAdminSession(handleChunks(rag)))
	mux.HandleFunc("/api/chunks/search-test", requireAdminSession(handleChunksSearchTest(rag)))
	mux.HandleFunc("/api/prompts", requireAdminSession(handlePrompts))
	mux.HandleFunc("/api/prompts/index", requireAdminSession(handlePromptsSaveIndex))
	mux.HandleFunc("/api/prompts/draft", requireAdminSession(handlePromptsSaveDraftReply))
	mux.HandleFunc("/api/prompts/agent", requireAdminSession(handlePromptsSaveAgent))
	mux.HandleFunc("/api/prompts/skill", requireAdminSession(handlePromptsSaveSkill))
	mux.HandleFunc("/api/prompts/skill/delete", requireAdminSession(handlePromptsDeleteSkill))
	mux.HandleFunc("/api/prompts/skill-test", requireAdminSession(handlePromptsSkillTest))
	mux.HandleFunc("/api/department-rules", requireAdminSession(handleDepartmentRules))
	mux.HandleFunc("/api/department-rules/save", requireAdminSession(handleDepartmentRulesSave))
	mux.HandleFunc("/api/department-rules/reset", requireAdminSession(handleDepartmentRulesReset))
	mux.HandleFunc("/api/admin/check", handleAdminCheck)
	mux.HandleFunc("/api/auth/login", handleLDAPLogin)
	mux.HandleFunc("/api/auth/logout", handleLogout)
	mux.HandleFunc("/api/auth/status", handleAuthStatus)
	// Server-side chat history (chathistory.go) — requireSession, not
	// requireAdminSession: any logged-in account, not just admins, since
	// this is a regular-user feature (see settings.EnableChatHistory's
	// doc comment for why/when this applies instead of the older
	// browser-localStorage-only history).
	mux.HandleFunc("/api/chat/conversations", requireSession(handleChatHistoryList))
	mux.HandleFunc("/api/chat/conversations/get", requireSession(handleChatHistoryGet))
	mux.HandleFunc("/api/chat/conversations/save", requireSession(handleChatHistorySave))
	mux.HandleFunc("/api/chat/conversations/rename", requireSession(handleChatHistoryRename))
	mux.HandleFunc("/api/chat/conversations/delete", requireSession(handleChatHistoryDelete))
	mux.HandleFunc("/api/chat/conversations/delete-all", requireSession(handleChatHistoryDeleteAll))
	// Per-user preference overrides (userprefs.go) — today just a personal
	// Lang override on top of settings.Lang's admin default. Same
	// login-only (non-admin) gate as chat history above.
	mux.HandleFunc("/api/account/prefs", requireSession(handleUserPrefsGet))
	mux.HandleFunc("/api/account/prefs/set", requireSession(handleUserPrefsSet))
	// Separate save action from the one above — see
	// handleUserPrefsSetPersonalContext's doc comment (userprefs.go) for why
	// the language switcher and the "Mein Konto" personal-context form each
	// get their own endpoint instead of sharing one broader upsert.
	mux.HandleFunc("/api/account/prefs/personal", requireSession(handleUserPrefsSetPersonalContext))
}

// authTierActive reports whether ANY real login system is configured —
// LDAP/AD or local accounts (settings.LocalAuth) — i.e. whether a session
// can mean something more specific than "anonymous guest". Every
// "Registriert"-tier gate below used to check settings.LDAP.Enabled alone;
// now that a session can also come from a local account with LDAP off,
// every one of those checks needs to ask this question instead, or a
// logged-in-but-non-admin local user (LDAP off) would fall through
// wherever the old check assumed "no LDAP means nobody can be logged in
// at all".
func authTierActive(s appSettings) bool {
	return s.LDAP.Enabled || s.LocalAuth.Enabled
}

// requireAdminSession protects admin-only routes with a real session, but
// only once some login system is configured (authTierActive — LDAP or
// local accounts). Before that, admin routes are exactly as before: hidden
// by the UI, not actually enforced server-side (see README.md's documented
// access-control gap) — so enabling either is what turns "UI convenience"
// into real access control, without breaking deployments that haven't
// configured either yet.
//
// Checks claims.IsAdmin, not just "has a valid session" — since login now
// also serves non-admin employees (department-restricted content access,
// answer personalization — see ldapauth.go's package comment) and non-admin
// local accounts, a valid session alone no longer implies admin rights the
// way it did before.
func requireAdminSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authTierActive(settings.get()) {
			next(w, r)
			return
		}
		claims, ok := currentSession(r)
		if !ok || !claims.IsAdmin {
			writeJSONError(w, "login required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// requireSessionIfLDAP enforces the "Registriert" tier from
// docs/UI_HARDENING_PLAN.md: /api/draft/reply and /api/sources/original
// need *any* valid session (not admin — unlike requireAdminSession) once
// authTierActive(s) is true. With no login system configured at all,
// nobody can log in, so this is a no-op and every deployment that never
// configures LDAP or local accounts keeps exactly today's behavior (guest =
// the only tier). Returns false and writes a 401 when the caller should be
// rejected — same "call it, bail on false" shape as requireMethod.
func requireSessionIfLDAP(w http.ResponseWriter, r *http.Request, s appSettings) bool {
	if !authTierActive(s) {
		return true
	}
	if _, ok := currentSession(r); !ok {
		writeJSONError(w, "login required", http.StatusUnauthorized)
		return false
	}
	return true
}

// resolveAskProfile applies docs/UI_HARDENING_PLAN.md's "Registriert"
// tier to an /api/ask chat-profile request: an anonymous caller explicitly
// asking for "azure" while some login system exists at all (authActive —
// LDAP or local accounts) is either silently downgraded to defaultProfile
// or rejected (deny=true), per ldap.GuestAzureProfilePolicy — that policy
// field stays LDAP-specific (there is only one such policy, shared by
// whichever login system is active), so ldap is still passed through for
// denyGuestAzure() even though "is a tier active at all" now comes from
// authActive instead of ldap.Enabled. Every other case (no login system,
// has a session, or didn't ask for azure) passes profile through
// unchanged. Pure function — no request/response — so the policy is
// unit-testable without a live chat backend.
func resolveAskProfile(profile string, ldap ldapConfig, authActive, hasSession bool, defaultProfile string) (resolved string, deny bool) {
	profile = strings.ToLower(strings.TrimSpace(profile))
	if profile != "azure" || !authActive || hasSession {
		return profile, false
	}
	if ldap.denyGuestAzure() {
		return profile, true
	}
	return defaultProfile, false
}

// baselineRankingConfig resolves the rankingConfig used for handleAsk's own
// baseline rankedSearch/assembleContext call — the unconditional "Kontext:"
// block built once per request, before the req.Mode branch that decides
// which TOOLS get added (see handleAsk's own comment at that call site).
// Chat mode has no search_knowledge_base tool at all: the baseline context
// is its ONLY path to knowledge-base grounding, so mode != "agent" always
// returns cfg unchanged. Agent mode DOES have that on-demand tool
// (agent.go's buildAgentTools), so cfg.Ranking.AgentModeMinFinalScore lets
// it apply a stricter threshold just for this one baseline call — but only
// when configured (0 disables it, same "0 = unchanged" convention as every
// other rankingConfig threshold) — see AgentModeMinFinalScore's own doc
// comment (settings.go) for the full tradeoff this represents. The agent's
// own search_knowledge_base tool call is untouched either way; it always
// uses s.Ranking as passed to buildAgentTools, not this function's result.
//
// Pure function — no ragSystem/request — so the policy is unit-testable
// without a live retrieval backend, same reasoning as resolveAskProfile
// above.
func baselineRankingConfig(mode string, cfg rankingConfig) rankingConfig {
	if mode != "agent" || cfg.AgentModeMinFinalScore <= 0 {
		return cfg
	}
	cfg.MinFinalScore = cfg.AgentModeMinFinalScore
	return cfg
}

// mssqlToolAllowed applies docs/UI_HARDENING_PLAN.md's "Registriert" tier
// to the MSSQL tool: with some login system active at all (authActive —
// LDAP or local accounts), an anonymous (no session) caller must not be
// able to trigger a live database query merely by asking a chat question
// the model decides to answer with it.
func mssqlToolAllowed(authActive, hasSession bool) bool {
	return !authActive || hasSession
}

// requireAPIKey protects /api/ask and /api/search with an X-API-Key (or
// "Authorization: Bearer <key>") header, but only once
// settings.API.RequireAPIKey is turned on — off by default, so the bundled
// browser UI keeps working with zero setup on a fresh checkout, the same
// opt-in shape as requireAdminSession's LDAP gate above. Once enabled, the
// browser UI itself needs a key too (see app.js's askWithAPIKeyPrompt) —
// there's no special-casing for same-origin requests, so "on" really means
// every caller needs a key, not just external ones.
func requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		if !s.API.RequireAPIKey {
			next(w, r)
			return
		}
		presented := r.Header.Get("X-API-Key")
		if presented == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				presented = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		rec, ok := findAPIKey(s.API.Keys, presented)
		if !ok {
			writeJSONError(w, "missing or invalid API key", http.StatusUnauthorized)
			return
		}
		touchAPIKeyLastUsed(rec.ID)
		// Stash the key's admin-assigned Name so tokenUsageActor (tokenusage.go)
		// can attribute this caller's token usage to it instead of "anonym" —
		// an API-key caller has no session, so it's otherwise indistinguishable
		// from every other unauthenticated-by-session request.
		r = r.WithContext(withAPIKeyName(r.Context(), rec.Name))
		next(w, r)
	}
}

// apiKeyLastUsedPersistInterval avoids a settings-file write on every API/MCP
// request while keeping the operational "last used" signal accurate enough
// for key rotation and incident review. Authentication still compares the key
// on every request; only its display timestamp is coalesced.
const apiKeyLastUsedPersistInterval = time.Minute

// touchAPIKeyLastUsed records that the key with this ID was just used to
// authenticate a request. Silently a no-op if the ID no longer exists
// (e.g. revoked/deleted between validation and this call), or if its already
// persisted timestamp is recent enough.
func touchAPIKeyLastUsed(id string) {
	now := time.Now().Unix()
	if s := settings.get(); len(s.API.Keys) > 0 {
		for _, key := range s.API.Keys {
			if key.ID == id && key.LastUsedAt >= now-int64(apiKeyLastUsedPersistInterval/time.Second) {
				return
			}
		}
	}
	_ = settings.update(func(s *appSettings) {
		for i := range s.API.Keys {
			if s.API.Keys[i].ID == id {
				s.API.Keys[i].LastUsedAt = now
				return
			}
		}
	})
}

// apiKeyPublic is an apiKeyRecord with Hash omitted — the shape ever sent
// to the browser (both in GET /api/apikeys and embedded nowhere else;
// maskedSettings strips Keys from /api/settings entirely, see below).
type apiKeyPublic struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// toAPIKeyPublic strips the fields of r that should never leave the
// server (see apiKeyPublic's doc comment above).
func toAPIKeyPublic(r apiKeyRecord) apiKeyPublic {
	return apiKeyPublic{ID: r.ID, Name: r.Name, Prefix: r.Prefix, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, Enabled: r.Enabled}
}

type createAPIKeyRequest struct {
	Name string `json:"name"`
}

type createAPIKeyResponse struct {
	apiKeyPublic
	Key string `json:"key"` // plaintext — present only in this one response, never again
}

// handleAPIKeys serves GET (list, no plaintext/hash) and POST (create a
// new key, returning its plaintext exactly once — see apikey.go's doc
// comment on why nothing else ever retains it).
func handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s := settings.get()
		out := make([]apiKeyPublic, 0, len(s.API.Keys))
		for _, k := range s.API.Keys {
			out = append(out, toAPIKeyPublic(k))
		}
		writeJSON(w, out)
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req createAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body", 400)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "unnamed key"
	}
	plaintext, rec, err := generateAPIKey(name)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if err := settings.update(func(s *appSettings) {
		s.API.Keys = append(s.API.Keys, rec)
	}); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	logAudit(r, "api_key_create", fmt.Sprintf("name=%q id=%s", name, rec.ID))
	writeJSON(w, createAPIKeyResponse{apiKeyPublic: toAPIKeyPublic(rec), Key: plaintext})
}

type revokeAPIKeyRequest struct {
	ID string `json:"id"`
}

// handleRevokeAPIKey disables a key (rather than deleting its record) so
// "who had a key named X and when was it revoked" stays answerable.
func handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req revokeAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSONError(w, "invalid body", 400)
		return
	}
	found := false
	if err := settings.update(func(s *appSettings) {
		for i := range s.API.Keys {
			if s.API.Keys[i].ID == req.ID {
				s.API.Keys[i].Enabled = false
				found = true
				return
			}
		}
	}); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if !found {
		writeJSONError(w, "api key not found", 404)
		return
	}
	logAudit(r, "api_key_revoke", fmt.Sprintf("id=%s", req.ID))
	writeJSON(w, map[string]bool{"ok": true})
}

type ldapLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLDAPLogin authenticates against Active Directory (ldapauth.go) and,
// on success, issues a real session cookie (session.go) — the
// enforcement half of requireAdminSession above. Succeeds for any
// account that can bind, regardless of admin-group membership (see
// ldapauth.go's package comment); the response's is_admin tells the
// frontend whether to show admin UI, separate from being logged in at
// all.
//
// Also checks a break-glass local admin password (settings.
// AdminPasswordEnv) before touching LDAP at all — see that field's doc
// comment in settings.go. This lets an admin get in even if AD/LDAP is
// down or misconfigured; it's checked first specifically so this path
// never waits on a slow/hanging LDAP connection.
func handleLDAPLogin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s := settings.get()
	if !authTierActive(s) {
		writeJSONError(w, "login is not enabled", 403)
		return
	}
	key, ok := checkLoginRateLimit(w, r)
	if !ok {
		return
	}
	var req ldapLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body", 400)
		return
	}

	if localPW := os.Getenv(s.AdminPasswordEnv); localPW != "" &&
		subtle.ConstantTimeCompare([]byte(req.Password), []byte(localPW)) == 1 {
		globalLoginLimiter.recordSuccess(key)
		name := strings.TrimSpace(req.Username)
		if name == "" {
			name = "admin (lokal)"
		}
		localAdmin := &ldapUser{CN: name, DisplayName: name, AccountName: name, IsAdmin: true, DeptCode: defaultDepartmentCode}
		log.Printf("break-glass admin login used (bypassing LDAP) as %q", name)
		issueSession(w, localAdmin)
		logAuditAs(name, key, "login", "method=break_glass ok=true")
		writeJSON(w, map[string]any{"ok": true, "user": localAdmin.CN, "display_name": localAdmin.DisplayName, "account_name": localAdmin.AccountName, "is_admin": true})
		return
	}

	// Local accounts (settings.LocalAuth, localusers.go) are tried next, by
	// username, regardless of whether LDAP is also enabled — a local
	// account is meant to work independently of AD (see appSettings.
	// LocalAuth's doc comment). Deliberately no fallthrough to LDAP below
	// when the username matches a local account but the password doesn't
	// (or the account is disabled): otherwise an AD account that happened
	// to share the same username could authenticate instead, which would
	// be a confusing and enumerable difference in behavior depending on
	// which password was tried.
	if s.LocalAuth.Enabled && strings.TrimSpace(req.Username) != "" {
		if _, ok, _ := localUsers.getByUsername(req.Username); ok {
			user, err := localAuthenticate(localUsers, req.Username, req.Password)
			if err != nil {
				log.Printf("local login failed for %q: %v", req.Username, err)
				globalLoginLimiter.recordFailure(key)
				logAuditAs(strings.TrimSpace(req.Username), key, "login", fmt.Sprintf("method=local ok=false error=%q", err.Error()))
				writeJSONError(w, "Anmeldung fehlgeschlagen", http.StatusUnauthorized)
				return
			}
			globalLoginLimiter.recordSuccess(key)
			issueSession(w, user)
			log.Printf("local login succeeded: account=%q display_name=%q admin=%v", user.AccountName, user.DisplayName, user.IsAdmin)
			logAuditAs(sessionActor(sessionClaims{User: user.CN, AccountName: user.AccountName, Mail: user.Mail}), key, "login", fmt.Sprintf("method=local ok=true is_admin=%v account=%q dept_code=%q", user.IsAdmin, user.AccountName, user.DeptCode))
			writeJSON(w, map[string]any{
				"ok": true, "user": user.CN, "is_admin": user.IsAdmin,
				"display_name": user.DisplayName, "account_name": user.AccountName,
				"department": user.Department,
			})
			return
		}
	}

	if !s.LDAP.Enabled {
		writeJSONError(w, "Anmeldung fehlgeschlagen", http.StatusUnauthorized)
		return
	}
	user, err := ldapAuthenticate(s.LDAP, s.PromptsDir, req.Username, req.Password)
	if err != nil {
		log.Printf("LDAP login failed for %q: %v", req.Username, err)
		globalLoginLimiter.recordFailure(key)
		logAuditAs(strings.TrimSpace(req.Username), key, "login", fmt.Sprintf("method=ldap ok=false error=%q", err.Error()))
		writeJSONError(w, "Anmeldung fehlgeschlagen", http.StatusUnauthorized)
		return
	}
	globalLoginLimiter.recordSuccess(key)
	issueSession(w, user)
	log.Printf("LDAP login succeeded: account=%q display_name=%q admin=%v department=%q groups=%d", user.AccountName, user.DisplayName, user.IsAdmin, user.Department, len(user.MemberOf))
	logAuditAs(sessionActor(sessionClaims{User: user.CN, AccountName: user.AccountName, UserPrincipalName: user.UserPrincipalName, Mail: user.Mail}), key, "login", fmt.Sprintf("method=ldap ok=true is_admin=%v account=%q dept_code=%q groups=%d", user.IsAdmin, user.AccountName, user.DeptCode, len(user.MemberOf)))
	writeJSON(w, map[string]any{
		"ok": true, "user": user.CN, "is_admin": user.IsAdmin,
		"display_name": user.DisplayName, "account_name": user.AccountName, "user_principal_name": user.UserPrincipalName,
		"department": user.Department, "title": user.Title, "office": user.Office, "company": user.Company,
	})
}

// handleLogout clears the caller's session cookie (session.go) — ungated,
// since anyone holding a session is allowed to end it. No method check:
// safe to call as a GET or POST alike, and there's nothing to validate.
func handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

// handleAuthStatus tells the frontend whether LDAP login is enabled at
// all (so it knows which login form to render), whether the current
// visitor already has a valid session, and whether chat history is
// enabled — intentionally ungated, since both a login form and the
// regular (non-admin) chat UI need to know this before anyone has
// logged in, and neither value is a secret.
func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	claims, ok := currentSession(r)
	s := settings.get()
	// adminBootstrapWarning mirrors ldapauth.go's "granting admin access on
	// bind alone" WARN log: with neither RequiredGroupDN nor AdminUsers
	// configured, every successful AD bind becomes admin. That's silent
	// otherwise (server log only) — surfaced here so web/app.js can show a
	// one-time novapop.js toast to the admin who just logged in.
	adminBootstrapWarning := s.LDAP.Enabled && s.LDAP.RequiredGroupDN == "" && len(s.LDAP.AdminUsers) == 0
	// effectiveLang is the admin-configured default (s.Lang), overridden by
	// the logged-in caller's own userprefs.go row if they have one — an
	// anonymous caller or a deployment with LDAP off never has an owner key
	// to look up, so it's just s.Lang for them, same as before this existed.
	effectiveLang := s.Lang
	if ok && userPrefsDB != nil {
		if p, err := userPrefsDB.get(claims.User); err == nil && p.Lang != "" {
			effectiveLang = p.Lang
		}
	}
	writeJSON(w, map[string]any{
		"ldap_enabled":       s.LDAP.Enabled,
		"local_auth_enabled": s.LocalAuth.Enabled,
		"logged_in":          ok,
		"user":                  claims.User,
		"display_name":          claims.DisplayName,
		"account_name":          claims.AccountName,
		"user_principal_name":   claims.UserPrincipalName,
		"mail":                  claims.Mail,
		"is_admin":              claims.IsAdmin,
		"department":            claims.Department,
		"title":                 claims.Title,
		"office":                claims.Office,
		"company":               claims.Company,
		"chat_history_enabled":  s.EnableChatHistory,
		"draft_replies_enabled": s.EnableDraftReplies,
		// Lets Chat's and the Mail tab's "An mich senden" button (handleChatEmail)
		// hide itself when SMTP isn't configured, instead of showing a button
		// that always fails with "email is not configured" on click.
		"smtp_enabled": s.SMTP.Enabled,
		// Lets web/app.js's SESSION_HISTORY_MAX mirror the server's actual
		// resolved cap (settings.go's HistoryMaxTurns, 0 = askHistoryMaxDefault)
		// instead of a hardcoded client-side constant — an admin raising this
		// setting would otherwise have no visible effect: the browser would
		// keep trimming history to the old hardcoded value before the server
		// ever saw more.
		"history_max_turns": askHistoryMax(s),
		// Lets the Mail tab (mail_graph.go) show/hide the native mailbox
		// panel: true only for a logged-in user explicitly authorized on
		// at least one InteractiveEnabled Exchange connection. false for
		// everyone else — they keep the pre-existing manual copy-paste
		// workflow, unaffected.
		"mail_graph_available":    mailGraphAvailable(s, r),
		"admin_bootstrap_warning": adminBootstrapWarning,
		// Lets Chat/Agent (neither of which is admin-gated) show the right
		// hint next to the image-attach button — "read by the vision
		// model" vs. "converted via OCR" — without needing admin-only
		// /api/settings access. See uploadConfig and handleAsk's image
		// routing.
		"upload_image_mode": effectiveUploadImageMode(s.Upload),
		// Lets the chat profile dropdown (web/app.js's
		// applyAskProfileVisibility) hide any cloud backend that isn't
		// actually configured, instead of letting it be picked and fail at
		// request time with a confusing upstream auth error. See
		// configuredChatProfiles's doc comment.
		"configured_chat_profiles": configuredChatProfiles(s),
		// Where the Chat/Agent UI loads Mermaid/d3 rendering libraries from
		// (settings.go's renderConfig, resolved to CDN defaults when unset).
		// Ungated like the rest of this response: the chat UI every visitor
		// uses needs it, and a library URL is not a secret.
		"render": func() map[string]string {
			rc := resolveRenderConfig(s.Render)
			return map[string]string{"mermaid_url": rc.MermaidURL, "d3_url": rc.D3URL}
		}(),
		// Lets every visitor — not just an admin who happens to open
		// Settings (the only other place web/app.js calls setLocale) —
		// actually receive the configured UI language: the caller's own
		// personal override (userprefs.go) if logged in and one exists,
		// else the admin's server-wide default. Without this at all, an
		// anonymous chat user had no code path that ever learned any of it
		// and stayed on i18n.js's hardcoded "de" default regardless of what
		// was configured.
		"lang": effectiveLang,
		// Lets the Help tab describe only the knowledge-base sources/live
		// tools actually configured+enabled at this deployment, instead of a
		// fixed list naming every connector/tool R3 supports regardless of
		// whether it's active here — see configuredSourceKinds/
		// configuredToolKinds' doc comments (settings.go). Ungated like the
		// rest of this response: which connector KINDS exist (not their
		// credentials/hosts) is not a secret, and the Help tab is not
		// admin-gated.
		"available_sources": configuredSourceKinds(s),
		"available_tools":   configuredToolKinds(s),
	})
}

// userContextBlock renders a short "who's asking" block for the system
// prompt from two independent sources: the caller's session — name/
// department/title/office, AD-derived facts R3 never invents — and,
// additionally, that same person's own personal-context preferences
// (userprefs.go, Phase 4), gated a second time by their own explicit
// opt-in on top of settings.PersonalizeAnswers (see personalContextBlock).
// Called only when PersonalizeAnswers is on (handleAsk). Returns "" for an
// anonymous caller: no placeholder, since that would just be a tell that
// personalization is configured without revealing anything useful.
func userContextBlock(r *http.Request) string {
	claims, ok := currentSession(r)
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Nutzerkontext (nur für Anrede/Tonfall verwenden, keine Angaben erfinden): Name: %s", sessionDisplayName(claims))
	if claims.Department != "" {
		fmt.Fprintf(&b, ", Abteilung: %s", claims.Department)
	}
	if claims.Title != "" {
		fmt.Fprintf(&b, ", Titel: %s", claims.Title)
	}
	if claims.Office != "" {
		fmt.Fprintf(&b, ", Standort: %s", claims.Office)
	}
	if claims.Company != "" {
		fmt.Fprintf(&b, ", Unternehmen: %s", claims.Company)
	}
	if personal := personalContextBlock(claims.User); personal != "" {
		b.WriteString("\n\n")
		b.WriteString(personal)
	}
	return b.String()
}

// personalContextBlock renders owner's own personalization preferences
// (userprefs.go) as a system-prompt block — "" if userPrefsDB isn't
// configured, owner has no stored preferences, or (crucially) owner hasn't
// explicitly opted in via UsePersonalContext: this is the user's OWN words
// about themselves, so using it needs their own consent, not just the
// admin's PersonalizeAnswers toggle. Framed as background the model may
// draw on for tone/signature/register — deliberately NOT instructions to
// adopt a persona or override its own behavior, and never treated as a
// fact about the current question/mail (same "data, not instructions"
// posture as every other user-supplied context in this codebase — see
// draft.go's Instructions field, fetch_url's tool-result framing).
func personalContextBlock(owner string) string {
	if userPrefsDB == nil || strings.TrimSpace(owner) == "" {
		return ""
	}
	p, err := userPrefsDB.get(owner)
	if err != nil || !p.UsePersonalContext {
		return ""
	}
	var lines []string
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", label, v))
		}
	}
	add("Bevorzugter Name", p.DisplayName)
	add("Position", p.Position)
	add("Abteilung", p.Department)
	add("Kontaktdaten", p.ContactInfo)
	add("Bevorzugte Kommunikationsweise", p.CommunicationStyle)
	add("Typische Formulierungen", p.TypicalPhrasing)
	add("Hinweise für die KI", p.AINotes)
	if sig := strings.TrimSpace(p.Signature); sig != "" {
		lines = append(lines, "- Signatur (bei Bedarf am Ende einer E-Mail sinngemäß verwenden): "+strings.ReplaceAll(sig, "\n", " / "))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Persönlicher Kontext dieser Person (von ihr selbst hinterlegt — für Anrede/Ton/Signatur nutzen, nicht als Sachinformation über die aktuelle Anfrage werten):\n" + strings.Join(lines, "\n")
}

// handleSettingsExport serves the full settings blob as a downloadable JSON
// file, credentials fully cleared (exportableSettings, not just masked —
// see its doc comment for why: the file may leave this host, e.g. as a
// backup or to move config to another R3 instance). Content-Disposition
// triggers the browser's normal file-save UI instead of navigating away.
func handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	b, err := json.MarshalIndent(exportableSettings(settings.get()), "", "  ")
	if err != nil {
		writeJSONError(w, "export: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="r3-settings-export.json"`)
	w.Write(b)
}

// handleSettings serves GET (masking every configured profile's API key) and
// accepts POST updates. A successful POST rebuilds the local embed client and
// the complete chatLMs map so an edited base_url/model/deployment/key takes
// effect immediately, without a server restart.
const (
	settingsRevisionHeader  = "X-R3-Settings-Revision"
	settingsRequestMaxBytes = 2 << 20 // generous for templates, bounded against accidental giant pastes
)

// requestedSettingsRevision parses the optional optimistic-concurrency token
// sent by the Settings UI. It stays optional so existing automation that
// already POSTs /api/settings continues to work; browser saves always send it
// and therefore cannot silently clobber a newer admin edit.
func requestedSettingsRevision(r *http.Request) (uint64, bool, error) {
	raw := strings.TrimSpace(r.Header.Get(settingsRevisionHeader))
	if raw == "" {
		return 0, false, nil
	}
	revision, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s header", settingsRevisionHeader)
	}
	return revision, true, nil
}

func writeSettingsRevision(w http.ResponseWriter, revision uint64) {
	w.Header().Set(settingsRevisionHeader, strconv.FormatUint(revision, 10))
}

func handleSettings(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			snapshot, revision := settings.getWithRevision()
			writeSettingsRevision(w, revision)
			writeJSON(w, maskedSettings(snapshot))
			return
		}
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		expectedRevision, hasExpectedRevision, err := requestedSettingsRevision(r)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		var patch appSettings
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, settingsRequestMaxBytes))
		if err := dec.Decode(&patch); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSONError(w, "settings request exceeds 2 MiB", http.StatusRequestEntityTooLarge)
				return
			}
			writeJSONError(w, "invalid body: "+err.Error(), 400)
			return
		}
		// Decode exactly one JSON value. Without this a valid settings object
		// followed by accidental junk would be accepted and the trailing data
		// silently ignored.
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			writeJSONError(w, "invalid body: expected exactly one JSON object", http.StatusBadRequest)
			return
		}
		// Validated before settings.update runs, not inside it: an invalid
		// template here would silently expose a broken/dangerous tool to
		// every future chat request, so this rejects the whole save (400)
		// rather than accepting a partially-bad QueryTemplates list — see
		// validateSQLQueryTemplates' doc comment (mssql.go).
		if err := validateSQLQueryTemplates(patch.MSSQL.QueryTemplates); err != nil {
			writeJSONError(w, "mssql query templates: "+err.Error(), 400)
			return
		}
		// Validate the generic REST connectors BEFORE the HTTP templates:
		// a template's auth_source may reference one by name, and
		// validateHTTPQueryTemplates resolves that reference against patch
		// (which carries the full form, REST connectors included).
		if err := validateRESTConnectors(patch.RESTConnectors); err != nil {
			writeJSONError(w, "rest connectors: "+err.Error(), 400)
			return
		}
		// Same reasoning as the MSSQL check above, see
		// validateHTTPQueryTemplates' doc comment (http_tool.go) — patch
		// itself carries the full form (including confluence/jira/
		// freshservice base_url and the REST connectors just validated), so
		// it's what the auth_source host check validates against.
		if err := validateHTTPQueryTemplates(patch.HTTPTemplates, patch); err != nil {
			writeJSONError(w, "http query templates: "+err.Error(), 400)
			return
		}
		// Storage (backend/mode/path/max_memory_mb) only takes effect on the
		// next restart (vectorstore.go's storageSettings doc comment) — this
		// is the only chance to catch a typo before it lands in settings.json
		// and silently shapes the next startup.
		if err := validateStorageSettings(patch.Storage); err != nil {
			writeJSONError(w, "storage: "+err.Error(), 400)
			return
		}
		if err := validateLocalAuthConfig(patch.LocalAuth); err != nil {
			writeJSONError(w, "local_auth: "+err.Error(), 400)
			return
		}
		if err := validateToolRouterSettings(patch.ToolRouter); err != nil {
			writeJSONError(w, "tool_router: "+err.Error(), 400)
			return
		}
		if err := validateQueryRewriteSettings(patch.QueryRewrite); err != nil {
			writeJSONError(w, "query_rewrite: "+err.Error(), 400)
			return
		}
		if err := validateImportSettings(patch.Import); err != nil {
			writeJSONError(w, "import: "+err.Error(), 400)
			return
		}
		if err := validateOpenAIEndpoints(patch.OpenAIAPI.Endpoints); err != nil {
			writeJSONError(w, "openai_api: "+err.Error(), 400)
			return
		}
		// A blank/duplicate preset Name would otherwise save silently and
		// break agentConfig.DefaultPreset/DraftPreset/askRequest.Preset
		// lookups (findPreset) in a way no admin would notice until an
		// answer unexpectedly comes back unrestricted — see validatePresets.
		if err := validatePresets(patch.Presets); err != nil {
			writeJSONError(w, "presets: "+err.Error(), 400)
			return
		}
		if patch.ChatProfile != "" && !isSupportedChatProfile(patch.ChatProfile) {
			writeJSONError(w, "chat_profile: unbekanntes Profil", http.StatusBadRequest)
			return
		}
		if patch.EmbedProfile != "" && strings.ToLower(strings.TrimSpace(patch.EmbedProfile)) != "local" {
			writeJSONError(w, "embed_profile: Embeddings werden ausschließlich lokal mit LM Studio erzeugt", http.StatusBadRequest)
			return
		}
		if patch.Lang != "" && !isSupportedUILang(patch.Lang) {
			writeJSONError(w, "lang: unbekannte Sprache", http.StatusBadRequest)
			return
		}
		before, out, revision, err := settings.updateIfRevision(expectedRevision, hasExpectedRevision, func(cur *appSettings) {
			// The settings form's <select> always sends one of
			// supportedUILangs, never blank, so non-empty-overwrite (like
			// ChatProfile below) is enough — no "reset to unset" case to
			// handle here.
			if patch.Lang != "" {
				cur.Lang = patch.Lang
			}
			mergeProfile(&cur.Profiles.Local, patch.Profiles.Local)
			mergeProfile(&cur.Profiles.Azure, patch.Profiles.Azure)
			mergeProfile(&cur.Profiles.OpenAI, patch.Profiles.OpenAI)
			mergeProfile(&cur.Profiles.OpenRouter, patch.Profiles.OpenRouter)
			mergeProfile(&cur.Profiles.Claude, patch.Profiles.Claude)
			mergeProfile(&cur.Profiles.Gemini, patch.Profiles.Gemini)
			// Embeddings stay in the local LM-Studio vector space. Chat
			// providers must never silently change the stored embedding model.
			cur.EmbedProfile = "local"
			if patch.ChatProfile != "" {
				cur.ChatProfile = patch.ChatProfile
			}
			if patch.ChunkSize > 0 {
				cur.ChunkSize = patch.ChunkSize
			}
			if patch.K > 0 {
				cur.K = patch.K
			}
			cur.HistoryMaxTurns = patch.HistoryMaxTurns
			cur.RedactPII = patch.RedactPII
			cur.AllowShellExec = patch.AllowShellExec
			cur.DisableStreaming = patch.DisableStreaming
			cur.EnableDraftReplies = patch.EnableDraftReplies
			// Unlike the text fields above, "" is a meaningful choice here
			// ("use the default chat profile"), not just "not filled in
			// yet" — always overwrite rather than the merge-if-non-empty
			// pattern, so picking it back to "" from the dropdown works.
			cur.DraftChatProfile = patch.DraftChatProfile
			cur.EnableChatHistory = patch.EnableChatHistory
			// Always replace wholesale, not gated on CandidateLimit>0 as
			// before: 0 is now a legitimate, deliberate value for several
			// of these fields too (e.g. CandidateLimit/MaxSources/
			// MaxPrimaryContentChars's own "use the built-in default"
			// convention), so gating on it made the entire Ranking block
			// silently fail to save whenever an admin left any of those at
			// their default.
			cur.Ranking = patch.Ranking
			if patch.Import.MarkItDownBin != "" {
				cur.Import.MarkItDownBin = patch.Import.MarkItDownBin
			}
			// The Document Intelligence endpoint is optional and blank means
			// deliberately disable the cloud conversion path, so unlike the
			// executable names it is always overwritten.
			cur.Import.MarkItDownDocIntelEndpoint = strings.TrimSpace(patch.Import.MarkItDownDocIntelEndpoint)
			if patch.Import.FFmpegBin != "" {
				cur.Import.FFmpegBin = patch.Import.FFmpegBin
			}
			if patch.Import.SevenZipBin != "" {
				cur.Import.SevenZipBin = patch.Import.SevenZipBin
			}
			if patch.Import.TesseractBin != "" {
				cur.Import.TesseractBin = patch.Import.TesseractBin
			}
			if patch.Import.TesseractLang != "" {
				cur.Import.TesseractLang = patch.Import.TesseractLang
			}
			if patch.Import.WhisperBin != "" {
				cur.Import.WhisperBin = patch.Import.WhisperBin
			}
			if patch.Import.WhisperModel != "" {
				cur.Import.WhisperModel = patch.Import.WhisperModel
			}
			if patch.Import.WhisperLanguage != "" {
				cur.Import.WhisperLanguage = patch.Import.WhisperLanguage
			}
			cur.Import.WhisperTimeoutSeconds = patch.Import.WhisperTimeoutSeconds
			// WhisperThreads/BeamSize/MaxConcurrent: same "0 is a deliberate
			// default" wholesale-replace reasoning as WhisperTimeoutSeconds
			// above — gating on >0 would make resetting a field back to
			// "use the built-in default" silently impossible from the form.
			cur.Import.WhisperThreads = patch.Import.WhisperThreads
			cur.Import.WhisperBeamSize = patch.Import.WhisperBeamSize
			cur.Import.WhisperMaxConcurrent = patch.Import.WhisperMaxConcurrent
			// WhisperFlashAttn/WhisperVAD: checkboxes, so — like
			// AllowInternalFetch below — always overwritten, never gated on
			// truthiness (that would make unchecking either in the form a
			// no-op).
			cur.Import.WhisperFlashAttn = patch.Import.WhisperFlashAttn
			cur.Import.WhisperVAD = patch.Import.WhisperVAD
			cur.Import.WhisperVADModel = patch.Import.WhisperVADModel
			cur.Import.TeamsMaxRepliesPerThread = patch.Import.TeamsMaxRepliesPerThread
			if patch.Import.MaxFileMB > 0 {
				cur.Import.MaxFileMB = patch.Import.MaxFileMB
			}
			// Import throttle/limit knobs (import_limits.go): always
			// overwritten, since 0 is a meaningful, deliberate "use the
			// built-in default" the admin can set back to — same
			// wholesale-replace reasoning as the Ranking block above.
			cur.Import.MaxItemsPerRun = patch.Import.MaxItemsPerRun
			cur.Import.RequestDelayMS = patch.Import.RequestDelayMS
			cur.Import.PreviewLimit = patch.Import.PreviewLimit
			cur.Import.GraphMaxRetries = patch.Import.GraphMaxRetries
			cur.Import.ConnectorMaxRetries = patch.Import.ConnectorMaxRetries
			cur.Import.RESTConnectorTimeoutSeconds = patch.Import.RESTConnectorTimeoutSeconds
			cur.Import.EmbedBatchSize = patch.Import.EmbedBatchSize
			// AllowInternalFetch: a checkbox, so — like EnableChatHistory
			// above — always overwritten, never gated on truthiness (that
			// would make unchecking it in the form a no-op).
			cur.Import.AllowInternalFetch = patch.Import.AllowInternalFetch
			// Upload.ImageMode/VisionProfile: the settings form always sends
			// one of exactly two option values for ImageMode (never blank),
			// so non-empty-overwrites is enough — no "0/'' is a deliberate
			// choice" case to handle here, unlike the Import knobs above.
			if patch.Upload.ImageMode != "" {
				cur.Upload.ImageMode = patch.Upload.ImageMode
			}
			if patch.Upload.VisionProfile != "" {
				cur.Upload.VisionProfile = patch.Upload.VisionProfile
			}
			// VisionMaxDim/VisionJPEGQuality: always replace wholesale, same
			// "0 is a deliberate default" reasoning as the Import throttle
			// knobs above — effectiveVisionMaxDim/effectiveVisionJPEGQuality
			// (chatimages.go) normalize 0 back to the built-in default and
			// clamp anything out of range.
			cur.Upload.VisionMaxDim = patch.Upload.VisionMaxDim
			cur.Upload.VisionJPEGQuality = patch.Upload.VisionJPEGQuality
			// MaxAttachmentMB/MaxPromptChars: same wholesale-replace reasoning —
			// effectiveMaxAttachmentMB/effectiveMaxPromptChars (chatimages.go)
			// normalize 0 back to the built-in default and clamp out-of-range.
			cur.Upload.MaxAttachmentMB = patch.Upload.MaxAttachmentMB
			cur.Upload.MaxPromptChars = patch.Upload.MaxPromptChars
			// URLMappings: always replace wholesale (nil/empty clears all mappings).
			cur.URLMappings = patch.URLMappings
			// SourceVisibility: same reasoning — always replace wholesale
			// (an omitted/empty map means "everything visible again").
			cur.SourceVisibility = patch.SourceVisibility
			// SourceAccess: same reasoning — always replace wholesale (an
			// omitted/empty map means "no retrieval restrictions").
			cur.SourceAccess = patch.SourceAccess
			cur.PersonalizeAnswers = patch.PersonalizeAnswers

			cur.LDAP.Enabled = patch.LDAP.Enabled
			if patch.LDAP.URL != "" {
				cur.LDAP.URL = patch.LDAP.URL
			}
			if patch.LDAP.BaseDN != "" {
				cur.LDAP.BaseDN = patch.LDAP.BaseDN
			}
			if patch.LDAP.DomainPrefix != "" {
				cur.LDAP.DomainPrefix = patch.LDAP.DomainPrefix
			}
			// Unlike URL/BaseDN/DomainPrefix above, "" is a meaningful,
			// deliberate choice here (remove the group restriction) —
			// always overwrite, same reasoning as DraftChatProfile above.
			cur.LDAP.RequiredGroupDN = patch.LDAP.RequiredGroupDN
			// AdminUsers: same reasoning as URLMappings/SourceVisibility
			// above — always replace wholesale, since an emptied list is a
			// deliberate "clear the allow-list" rather than "not filled in".
			cur.LDAP.AdminUsers = patch.LDAP.AdminUsers
			// GuestAzureProfilePolicy: "" is meaningful (explicit default,
			// "fallback"), same always-overwrite reasoning as
			// RequiredGroupDN above.
			cur.LDAP.GuestAzureProfilePolicy = patch.LDAP.GuestAzureProfilePolicy

			// LocalAuth has no secret fields (account passwords live in
			// localUserStore, not settings.json) — always overwrite wholesale,
			// same as most of LDAP above.
			cur.LocalAuth = patch.LocalAuth

			// Each import connector is a named list
			// (connruntime.go) instead of one fixed struct — mergeConnList
			// applies the same masked-secret/"0 is deliberate"/server-managed-
			// field rules the old per-field blocks used to, just once per
			// connection by Name instead of duplicated per top-level field.
			// Folder has no secret field, so mergeFolderConn skips that part.
			cur.SharePoint = mergeConnList(cur.SharePoint, patch.SharePoint, mergeSharePointConn)
			cur.OneDrive = mergeConnList(cur.OneDrive, patch.OneDrive, mergeOneDriveConn)
			cur.ExchangeGraph = mergeConnList(cur.ExchangeGraph, patch.ExchangeGraph, mergeExchangeGraphConn)
			cur.IMAP = mergeConnList(cur.IMAP, patch.IMAP, mergeIMAPConn)
			cur.Teams = mergeConnList(cur.Teams, patch.Teams, mergeTeamsConn)
			cur.Confluence = mergeConnList(cur.Confluence, patch.Confluence, mergeConfluenceConn)
			cur.Jira = mergeConnList(cur.Jira, patch.Jira, mergeJiraConn)
			cur.Freshservice = mergeConnList(cur.Freshservice, patch.Freshservice, mergeFreshserviceConn)
			cur.Folder = mergeConnList(cur.Folder, patch.Folder, mergeFolderConn)
			cur.GitHub = mergeConnList(cur.GitHub, patch.GitHub, mergeGitHubConn)
			cur.SAPS4 = mergeConnList(cur.SAPS4, patch.SAPS4, mergeSAPS4Conn)

			cur.SMTP.Enabled = patch.SMTP.Enabled
			if patch.SMTP.Host != "" {
				cur.SMTP.Host = patch.SMTP.Host
			}
			if patch.SMTP.Port > 0 {
				cur.SMTP.Port = patch.SMTP.Port
			}
			if patch.SMTP.Username != "" {
				cur.SMTP.Username = patch.SMTP.Username
			}
			if patch.SMTP.Password != "" && !strings.Contains(patch.SMTP.Password, "***") {
				cur.SMTP.Password = patch.SMTP.Password
			}
			if patch.SMTP.PasswordEnv != "" {
				cur.SMTP.PasswordEnv = patch.SMTP.PasswordEnv
			}
			if patch.SMTP.From != "" {
				cur.SMTP.From = patch.SMTP.From
			}

			cur.MSSQL.Enabled = patch.MSSQL.Enabled
			if patch.MSSQL.Host != "" {
				cur.MSSQL.Host = patch.MSSQL.Host
			}
			if patch.MSSQL.Port > 0 {
				cur.MSSQL.Port = patch.MSSQL.Port
			}
			if patch.MSSQL.Database != "" {
				cur.MSSQL.Database = patch.MSSQL.Database
			}
			if patch.MSSQL.Username != "" {
				cur.MSSQL.Username = patch.MSSQL.Username
			}
			if patch.MSSQL.Password != "" && !strings.Contains(patch.MSSQL.Password, "***") {
				cur.MSSQL.Password = patch.MSSQL.Password
			}
			if patch.MSSQL.PasswordEnv != "" {
				cur.MSSQL.PasswordEnv = patch.MSSQL.PasswordEnv
			}
			cur.MSSQL.TrustServerCertificate = patch.MSSQL.TrustServerCertificate
			if patch.MSSQL.MaxRows > 0 {
				cur.MSSQL.MaxRows = patch.MSSQL.MaxRows
			}
			if patch.MSSQL.TimeoutSeconds > 0 {
				cur.MSSQL.TimeoutSeconds = patch.MSSQL.TimeoutSeconds
			}
			cur.MSSQL.AllowGenericQuery = patch.MSSQL.AllowGenericQuery
			// QueryTemplates: same reasoning as SourceVisibility/SourceAccess
			// above — always replace wholesale (an omitted/empty list means
			// "no templates"); already validated above before this closure
			// even runs.
			cur.MSSQL.QueryTemplates = patch.MSSQL.QueryTemplates
			// MaskColumns: same wholesale-replace reasoning — an omitted/
			// empty list means "nothing masked anymore", a deliberate
			// choice an admin can make same as clearing RequiredGroupDN.
			cur.MSSQL.MaskColumns = patch.MSSQL.MaskColumns
			// AccessControl: same wholesale-replace reasoning — an
			// emptied allow-list is a deliberate "no extra restriction",
			// not "not filled in yet" (see accessControl's doc comment).
			cur.MSSQL.AccessControl = patch.MSSQL.AccessControl

			// HTTPTemplates: same wholesale-replace reasoning as
			// MSSQL.QueryTemplates above; already validated above before
			// this closure even runs.
			cur.HTTPTemplates = patch.HTTPTemplates

			// RESTConnectors: merged by Name like the import connectors
			// (mergeRESTConn preserves masked secrets); already validated
			// above. An entry removed from the form is dropped, same
			// "omitted means gone" convention as every other connector list.
			cur.RESTConnectors = mergeConnList(cur.RESTConnectors, patch.RESTConnectors, mergeRESTConn)

			// Presets: same wholesale-replace reasoning as QueryTemplates/
			// MaskColumns above — an omitted/empty list means "no presets
			// defined", not "leave the old ones".
			cur.Presets = patch.Presets
			cur.DraftPreset = patch.DraftPreset
			cur.DraftMaxToolRounds = patch.DraftMaxToolRounds

			cur.Shop.Enabled = patch.Shop.Enabled
			if patch.Shop.BaseURL != "" {
				cur.Shop.BaseURL = patch.Shop.BaseURL
			}
			if patch.Shop.Username != "" {
				cur.Shop.Username = patch.Shop.Username
			}
			if patch.Shop.Password != "" && !strings.Contains(patch.Shop.Password, "***") {
				cur.Shop.Password = patch.Shop.Password
			}
			if patch.Shop.PasswordEnv != "" {
				cur.Shop.PasswordEnv = patch.Shop.PasswordEnv
			}
			if patch.Shop.ClientID != "" {
				cur.Shop.ClientID = patch.Shop.ClientID
			}
			if patch.Shop.ClientSecret != "" && !strings.Contains(patch.Shop.ClientSecret, "***") {
				cur.Shop.ClientSecret = patch.Shop.ClientSecret
			}
			if patch.Shop.ClientSecretEnv != "" {
				cur.Shop.ClientSecretEnv = patch.Shop.ClientSecretEnv
			}
			if patch.Shop.TimeoutSeconds > 0 {
				cur.Shop.TimeoutSeconds = patch.Shop.TimeoutSeconds
			}
			if patch.Shop.MaxResults > 0 {
				cur.Shop.MaxResults = patch.Shop.MaxResults
			}
			if patch.Shop.MaxRetries > 0 {
				cur.Shop.MaxRetries = patch.Shop.MaxRetries
			}
			// AccessControl: same wholesale-replace reasoning as
			// MSSQL.AccessControl above.
			cur.Shop.AccessControl = patch.Shop.AccessControl

			// Agent gates: booleans unconditional (false is a deliberate
			// "off"), rounds only when positive (0 = "use the default").
			cur.Agent.AllowCodeExecution = patch.Agent.AllowCodeExecution
			cur.Agent.AllowWebFetch = patch.Agent.AllowWebFetch
			cur.Agent.AllowWebResearch = patch.Agent.AllowWebResearch
			cur.Agent.SubagentsDisabled = patch.Agent.SubagentsDisabled
			// 0 = "use the built-in default" for all three (agent.go's
			// clampInt), a meaningful value an admin can set back to — so
			// always replace wholesale rather than gating on > 0.
			cur.Agent.MaxSubtasks = patch.Agent.MaxSubtasks
			cur.Agent.SubagentRounds = patch.Agent.SubagentRounds
			cur.Agent.MaxConcurrency = patch.Agent.MaxConcurrency
			cur.Agent.WebResearchRounds = patch.Agent.WebResearchRounds
			cur.Agent.WebResearchTimeoutSeconds = patch.Agent.WebResearchTimeoutSeconds
			cur.Agent.AllowWebSearch = patch.Agent.AllowWebSearch
			if patch.Agent.WebSearchAPIKey != "" && !strings.Contains(patch.Agent.WebSearchAPIKey, "***") {
				cur.Agent.WebSearchAPIKey = patch.Agent.WebSearchAPIKey
			}
			cur.Agent.WebSearchAPIKeyEnv = patch.Agent.WebSearchAPIKeyEnv
			cur.Agent.WebSearchMaxResults = patch.Agent.WebSearchMaxResults
			cur.Agent.WebSearchTimeoutSeconds = patch.Agent.WebSearchTimeoutSeconds
			cur.Agent.AllowAzureBingSearch = patch.Agent.AllowAzureBingSearch
			cur.Agent.AzureBingSearchTimeoutSeconds = patch.Agent.AzureBingSearchTimeoutSeconds
			if patch.Agent.MaxToolRounds > 0 {
				cur.Agent.MaxToolRounds = patch.Agent.MaxToolRounds
			}
			cur.Agent.DefaultPreset = patch.Agent.DefaultPreset
			cur.Agent.ContextCompactionDisabled = patch.Agent.ContextCompactionDisabled
			cur.Agent.ContextCompactionThresholdChars = patch.Agent.ContextCompactionThresholdChars
			cur.Agent.ContextCompactionKeepRounds = patch.Agent.ContextCompactionKeepRounds
			cur.Agent.SearchResultChars = patch.Agent.SearchResultChars
			cur.Agent.SourceContentChars = patch.Agent.SourceContentChars

			// Only the toggle is settable here — cur.API.Keys is never
			// touched by this generic merge, see maskedSettings's doc
			// comment above.
			cur.API.RequireAPIKey = patch.API.RequireAPIKey
			cur.API.GuestAskRateLimitPerMinute = patch.API.GuestAskRateLimitPerMinute
			cur.API.GuestVoiceRateLimitPerMinute = patch.API.GuestVoiceRateLimitPerMinute
			// Wholesale replace, same reasoning as Presets/QueryTemplates
			// above — no secrets in here to accidentally clobber, and
			// Enabled=false/Port=0/Preset="" are all meaningful, deliberate
			// values an admin can set back to, not "not filled in yet".
			cur.OpenAIAPI = patch.OpenAIAPI
			// Wholesale replace, normalized the same way parseStorageMode
			// normalizes at runtime (lower-cased, trimmed) — see
			// storageSettings' doc comment for why this only takes effect on
			// the next restart, and validateStorageSettings above for why an
			// unknown Backend/Mode can never reach here.
			cur.Storage = storageSettings{
				Backend:     strings.ToLower(strings.TrimSpace(patch.Storage.Backend)),
				Mode:        strings.ToLower(strings.TrimSpace(patch.Storage.Mode)),
				Path:        strings.TrimSpace(patch.Storage.Path),
				MaxMemoryMB: patch.Storage.MaxMemoryMB,
			}
			// Wholesale replace, normalized like Storage above — see
			// validateToolRouterSettings for why an unknown Profile can
			// never reach here. Enabled=false/Profile="" are meaningful,
			// deliberate values an admin can set back to.
			cur.ToolRouter = toolRouterConfig{
				Enabled: patch.ToolRouter.Enabled,
				Profile: strings.ToLower(strings.TrimSpace(patch.ToolRouter.Profile)),
			}
			// Wholesale replace, normalized like ToolRouter above — see
			// validateQueryRewriteSettings for why an unknown Profile can
			// never reach here.
			cur.QueryRewrite = queryRewriteConfig{
				Enabled: patch.QueryRewrite.Enabled,
				Profile: strings.ToLower(strings.TrimSpace(patch.QueryRewrite.Profile)),
			}
		})
		if err != nil {
			var conflict *settingsRevisionConflictError
			if errors.As(err, &conflict) {
				writeSettingsRevision(w, conflict.Current)
				writeJSONError(w, "settings changed by another administrator; reload before saving", http.StatusConflict)
				return
			}
			writeJSONError(w, err.Error(), 500)
			return
		}
		// A legacy/API client without a revision header may still save in
		// parallel. Re-read the latest committed snapshot before refreshing
		// live services, so a slower earlier request can never reinstall its
		// stale LLM/OpenAI runtime after a later save already won on disk.
		runtimeSettings := settings.get()
		embed, chat := buildLLMClients(runtimeSettings)
		rag.setLLM(embed, chat, runtimeSettings.ChatProfile)
		// Starts/stops/rebinds the OpenAI-compatible server (openai_api.go)
		// to match whatever was just saved — same "takes effect
		// immediately, no restart" treatment as the LLM clients above.
		reconcileOpenAIAPIServer(rag, runtimeSettings.OpenAIAPI)
		// source=import (set by the Settings tab's "Import Settings" button,
		// see app.js) is the only recognized value besides the default "" —
		// anything else collapses to "", so an arbitrary query string can't
		// forge a different label in the history entry.
		source := ""
		if r.URL.Query().Get("source") == "import" {
			source = "import"
		}
		// Field-level diff (settings_history.go): persisted to the
		// Änderungshistorie with secret values masked, and summarized
		// (paths only, never values) into the audit log's detail.
		logAudit(r, "settings_update", recordSettingsChange(r, before, out, source))
		writeSettingsRevision(w, revision)
		writeJSON(w, maskedSettings(out))
	}
}

// mergeProfile applies non-empty fields from patch onto cur, so a partial
// settings POST (e.g. only editing the Azure API key) never wipes the rest
// of that profile's configuration.
func mergeProfile(cur *llmProfile, patch llmProfile) {
	if patch.BaseURL != "" {
		cur.BaseURL = normalizeBaseURL(patch.BaseURL)
	}
	if patch.APIVersion != "" {
		cur.APIVersion = patch.APIVersion
	}
	if patch.ChatModel != "" {
		cur.ChatModel = patch.ChatModel
	}
	if patch.EmbedModel != "" {
		cur.EmbedModel = patch.EmbedModel
	}
	if patch.APIKey != "" && !strings.Contains(patch.APIKey, "***") {
		cur.APIKey = patch.APIKey
	}
	if patch.APIKeyEnv != "" {
		cur.APIKeyEnv = patch.APIKeyEnv
	}
}

// maskedSettings returns a copy of s with both profiles' inline API keys
// replaced by a placeholder, never round-tripping real secrets to the
// browser. APIKeyEnv (just a variable *name*, not a secret) is left as-is.
func maskedSettings(s appSettings) appSettings {
	return settingsWithSecretsRedacted(s, maskSecret)
}

// exportableSettings returns a copy of s with every credential field
// (inline API keys, client secrets, tokens, passwords) cleared to "" rather
// than placeholder-masked — used by handleSettingsExport so a downloaded
// settings file never carries a real secret, nor even the "a secret was
// configured here" signal a mask placeholder would leak. *_env fields
// (variable names, not secrets) are left as-is, same as maskedSettings —
// they're what makes a re-imported file still usable: the env var name
// survives, only the inline fallback value doesn't.
func exportableSettings(s appSettings) appSettings {
	return settingsWithSecretsRedacted(s, func(string) string { return "" })
}

// settingsWithSecretsRedacted is maskedSettings/exportableSettings' shared
// walk over every credential-bearing field in appSettings, differing only
// in how each secret value is replaced (redact). Keeping one list of fields
// means a newly added connector's secret only needs adding here once to be
// covered by both the browser-facing mask and the export/import redaction.
func settingsWithSecretsRedacted(s appSettings, redact func(string) string) appSettings {
	s.Profiles.Local.APIKey = redact(s.Profiles.Local.APIKey)
	s.Profiles.Azure.APIKey = redact(s.Profiles.Azure.APIKey)
	s.Profiles.OpenAI.APIKey = redact(s.Profiles.OpenAI.APIKey)
	s.Profiles.OpenRouter.APIKey = redact(s.Profiles.OpenRouter.APIKey)
	s.Profiles.Claude.APIKey = redact(s.Profiles.Claude.APIKey)
	s.Profiles.Gemini.APIKey = redact(s.Profiles.Gemini.APIKey)
	// Each connector is now a list of named connections — redact every
	// entry's secret, not just a single fixed struct's.
	s.SharePoint = maskConnList(s.SharePoint, func(c *sharePointConfig) { c.ClientSecret = redact(c.ClientSecret) })
	s.OneDrive = maskConnList(s.OneDrive, func(c *oneDriveConfig) { c.ClientSecret = redact(c.ClientSecret) })
	s.ExchangeGraph = maskConnList(s.ExchangeGraph, func(c *exchangeGraphConfig) { c.ClientSecret = redact(c.ClientSecret) })
	s.IMAP = maskConnList(s.IMAP, func(c *mailboxConfig) { c.Password = redact(c.Password) })
	s.Teams = maskConnList(s.Teams, func(c *teamsConfig) { c.ClientSecret = redact(c.ClientSecret) })
	s.Confluence = maskConnList(s.Confluence, func(c *confluenceConfig) { c.APIToken = redact(c.APIToken) })
	s.Jira = maskConnList(s.Jira, func(c *jiraConfig) { c.APIToken = redact(c.APIToken) })
	s.Freshservice = maskConnList(s.Freshservice, func(c *freshserviceConfig) { c.APIKey = redact(c.APIKey) })
	s.GitHub = maskConnList(s.GitHub, func(c *githubConfig) { c.Token = redact(c.Token) })
	s.SAPS4 = maskConnList(s.SAPS4, func(c *sapS4Config) {
		c.Password = redact(c.Password)
		c.Token = redact(c.Token)
	})
	// Generic REST connectors: redact both credential fields (Basic password
	// and bearer/header token). The static Headers map is intentionally NOT
	// touched — it's documented as non-secret (see restConnectorConfig).
	s.RESTConnectors = maskConnList(s.RESTConnectors, func(c *restConnectorConfig) {
		c.Password = redact(c.Password)
		c.Token = redact(c.Token)
	})
	s.SMTP.Password = redact(s.SMTP.Password)
	s.MSSQL.Password = redact(s.MSSQL.Password)
	s.Shop.Password = redact(s.Shop.Password)
	s.Shop.ClientSecret = redact(s.Shop.ClientSecret)
	s.Agent.WebSearchAPIKey = redact(s.Agent.WebSearchAPIKey)
	// API keys are managed exclusively through /api/apikeys (create/list/
	// revoke) — never round-tripped as part of the generic settings blob,
	// so there's no risk of a settings POST silently overwriting the key
	// list with stale client-side data.
	s.API.Keys = nil
	return s
}

// maskSecret hides a configured secret from the browser while still
// letting the settings UI show whether one is set at all: empty stays
// empty, anything else becomes the fixed placeholder "***set***" (never
// reveals length or content).
func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	return "***set***"
}

type adminCheckRequest struct {
	Password string `json:"password"`
}

// handleAdminCheck is a UI-visibility gate, not access control — see the
// AdminPasswordEnv doc comment in settings.go. It never returns the
// configured password to the client, only whether the submitted one
// matches. If no password is configured, every check succeeds (an
// unconfigured gate behaves as if there were no gate at all).
func handleAdminCheck(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	key, allowed := checkLoginRateLimit(w, r)
	if !allowed {
		return
	}
	var req adminCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	want := os.Getenv(settings.get().AdminPasswordEnv)
	ok := want == "" || req.Password == want
	if ok {
		globalLoginLimiter.recordSuccess(key)
	} else {
		globalLoginLimiter.recordFailure(key)
	}
	logAudit(r, "login_admin_check", fmt.Sprintf("ok=%v", ok))
	writeJSON(w, map[string]bool{"ok": ok})
}

// handleOpenAPISpec serves the checked-in OpenAPI spec (docs/openapi.json,
// embedded at build time) verbatim — any standard tool (Swagger Editor,
// Postman, Insomnia, a codegen step) can point at this URL directly.
//
// CORS is wide open (Access-Control-Allow-Origin: *) specifically here: the
// spec is documentation, not data, so there's nothing to leak — but without
// this header, editor.swagger.io (or any other cross-origin tool) can't
// fetch it at all, since browsers block a cross-origin fetch response
// without an explicit allow-origin regardless of the spec's own content.
func handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	io.WriteString(w, openAPISpec)
}

// handleLLMsTxt serves the checked-in llms.txt (embedded at build time)
// verbatim — the https://llmstxt.org convention for an LLM/agent-oriented
// summary of what this site/service is and how to call it, analogous to
// robots.txt for crawlers. Ungated, plain text, no dependency checks —
// same reasoning as handleOpenAPISpec/healthz above.
func handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, llmsTxt)
}

// handleAPIDocs serves the embedded interactive OpenAPI/Swagger-style
// viewer (web/apidocs.html, embedded at build time) — a self-contained
// alternative to the reference swagger-ui bundle: same underlying
// /api/openapi.json spec, but rendered with no vendored third-party JS
// and no CDN dependency, matching R3's single-binary/offline-capable
// posture. The page itself links out to the real Swagger Editor too, for
// anyone who specifically wants that tool.
func handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, apiDocsHTML)
}

func handleOpenAIAPIPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, openAIAPIHTML)
}

// handleStats reports total chunk/source counts plus which source_kinds
// currently have content, most-populated first — the browser uses `kinds`
// to seed the chat empty-state's suggestion chips with something actually
// grounded in this deployment's data instead of hardcoded placeholder
// questions (web/app.js's renderSuggestions). Ungated and read-only, but
// still respects settings.SourceVisibility the same way filterCitations
// (rank.go) does for citations — a source_kind an admin hid from citations
// shouldn't leak its existence via a suggested question either, even
// though only the kind name (not any source_id/name) is ever exposed here.
func handleStats(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, _ := rag.listSources()
		s := settings.get()
		chunksByKind := make(map[string]int)
		for _, src := range sources {
			if !s.citationsVisible(src.SourceKind) {
				continue
			}
			chunksByKind[src.SourceKind] += src.Chunks
		}
		kinds := make([]string, 0, len(chunksByKind))
		for k := range chunksByKind {
			kinds = append(kinds, k)
		}
		sort.Slice(kinds, func(i, j int) bool { return chunksByKind[kinds[i]] > chunksByKind[kinds[j]] })

		writeJSON(w, map[string]any{
			"chunks":  rag.docCount(),
			"sources": len(sources),
			"kinds":   kinds,
		})
	}
}

// presetSummary is the public shape of a sourcePreset (settings.go) — just
// enough for the Chat preset dropdown to render options. Deliberately
// omits Kinds/Tools: those describe what a preset restricts internally,
// not something an unauthenticated visitor needs to see, and exposing
// them would leak which source kinds/tools exist in this deployment at
// all.
type presetSummary struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// handlePresets lists every configured preset's name/display_name —
// ungated like /api/stats (no admin-only content), used to populate the
// Chat tab's preset dropdown. Agent/Mail don't need this endpoint at all:
// their preset is admin-fixed (agentConfig.DefaultPreset/DraftPreset),
// resolved server-side with no client-side selection.
func handlePresets(w http.ResponseWriter, r *http.Request) {
	presets := settings.get().Presets
	out := make([]presetSummary, len(presets))
	for i, p := range presets {
		out[i] = presetSummary{Name: p.Name, DisplayName: p.DisplayName}
	}
	writeJSON(w, out)
}

type askRequest struct {
	Question string `json:"question"`
	K        int    `json:"k,omitempty"`
	// Profile optionally overrides the default chat backend for this one
	// request ("local", "azure", "openai", "openrouter", "claude" or "gemini") — e.g. a "größeres Modell verwenden"
	// toggle in the UI. Empty uses settings.ChatProfile.
	Profile string `json:"profile,omitempty"`
	// Format selects the response shape. "" (default) streams NDJSON
	// exactly as before, for the bundled browser UI. "json" buffers the
	// full answer server-side and returns one application/json object —
	// the shape a plain HTTP client (curl, requests, another service)
	// typically wants, with no NDJSON parsing required.
	Format string `json:"format,omitempty"`
	// History carries prior turns of the same conversation (see
	// askHistoryTurn) so a follow-up question ("what about last month?")
	// has the context to resolve — every /api/ask call is otherwise
	// completely stateless. The browser UI sends its own in-memory
	// session transcript here; retrieval (rankedSearch below) still only
	// ever runs against the current Question, not the whole history.
	History []askHistoryTurn `json:"history,omitempty"`
	// Mode selects which base system prompt buildSystemPromptForMode uses:
	// "" (default, plain Chat tab) reads index.md; "agent" (the Agent tab)
	// reads agent.md instead — see skills.go. Superseded by Tier below when
	// that is set; kept for backward compatibility (the Agent tab still sends
	// mode:"agent", and resolveExecutionTier falls back to it when Tier is
	// empty).
	Mode string `json:"mode,omitempty"`
	// Tier is the ChatGPT-style reasoning/execution level:
	// "instant" (RAG only, no tools — fastest), "standard" (RAG + live
	// tools, one round — the classic Chat behavior) or "agent" (full agentic
	// tool set + multi-round). Empty falls back to Mode. See tier.go's
	// resolveExecutionTier.
	Tier string `json:"tier,omitempty"`
	// Preset names an appSettings.Presets entry (see settings.go/preset.go)
	// restricting which source kinds/tools this one question may draw on —
	// the Chat tab's per-request equivalent of agentConfig.DefaultPreset
	// (which governs agent mode instead, ignoring this field). Empty = no
	// restriction beyond SourceAccess, same as before presets existed.
	Preset string `json:"preset,omitempty"`
	// Images are files (photos/scans) attached to this one question —
	// see chatimages.go's buildUserMessage for how they're used (real
	// vision content parts if the resolved profile supports it,
	// otherwise OCR'd into plain text) and its doc comment for why
	// they're never persisted anywhere beyond this one request.
	Images []askImageInput `json:"images,omitempty"`
}

// askHistoryTurn is one prior message in the same conversation.
type askHistoryTurn struct {
	Role    string `json:"role"` // "user" | "assistant"
	Content string `json:"content"`
}

// askHistoryMaxDefault is the built-in fallback for
// appSettings.HistoryMaxTurns (settings.go) — see its doc comment for the
// full reasoning and the client-side mirror this must stay in sync with.
const askHistoryMaxDefault = 12

// askHistoryMax resolves the configured history-turn cap — 0 (default)
// uses askHistoryMaxDefault.
func askHistoryMax(s appSettings) int {
	if s.HistoryMaxTurns > 0 {
		return s.HistoryMaxTurns
	}
	return askHistoryMaxDefault
}

// historyToChatMsgs converts (and defensively sanitizes) askHistoryTurns
// into the chatMsg shape llm.go's chatWithTools expects, dropping anything
// malformed rather than failing the whole request over it. max is the
// resolved cap (see askHistoryMax) — the caller decides it since this
// function has no settings access of its own (same separation as most
// helpers in this file).
func historyToChatMsgs(history []askHistoryTurn, max int) []chatMsg {
	if max > 0 && len(history) > max {
		history = history[len(history)-max:]
	}
	msgs := make([]chatMsg, 0, len(history))
	for _, h := range history {
		if h.Role != "user" && h.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(h.Content) == "" {
			continue
		}
		msgs = append(msgs, chatMsg{Role: h.Role, Content: h.Content})
	}
	return msgs
}

// askJSONResponse is the body of a {"format":"json"} request — the
// non-streaming counterpart to askStreamMsg's "done" line. Answer/
// Citations are empty when Clarify is set — the agent stopped to ask a
// clarifying question instead of producing a real answer this turn (see
// askClarification's doc comment).
type askJSONResponse struct {
	Answer    string            `json:"answer"`
	Citations []sourceInfo      `json:"citations"`
	Clarify   *askClarification `json:"clarify,omitempty"`
	// Debug is only ever non-nil for the one session debugModeAllowed
	// recognizes (see its doc comment) — full system prompt, retrieved RAG
	// chunks, every tool call and the exact message sequence sent to the
	// model. Absent entirely for every other caller, not just empty.
	Debug *debugTrace `json:"debug,omitempty"`
}

// askClarification carries a clarifying question the Agent tab wants the
// user to answer before continuing, instead of guessing at an ambiguous
// task — see llm.go's ErrClarificationNeeded (which this is built from)
// and agent.go's clarifyToolDef (the tool the model calls to trigger it).
// Options, if non-empty, are short answer choices the UI renders as
// buttons (web/app.js's askAgentQuestion); empty means there's no
// sensible fixed choice list and the user should just type a reply.
type askClarification struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// askStreamMsg is one NDJSON line streamed to the browser by /api/ask —
// "token" (a piece of the answer, as it arrives from the LLM), "done"
// (final message, carries citations), "clarify" (the agent stopped to ask
// a clarifying question instead of answering — see askClarification) or
// "error" (something failed after streaming already started, so an HTTP
// error status is no longer possible — the client must treat
// "type":"error" as a failed request).
type askStreamMsg struct {
	Type      string            `json:"type"`
	Text      string            `json:"text,omitempty"`
	Citations []sourceInfo      `json:"citations,omitempty"`
	Clarify   *askClarification `json:"clarify,omitempty"`
	Error     string            `json:"error,omitempty"`
	// Step carries one live agent-progress event ("step" type lines) — a
	// tool starting/finishing or a sub-agent spawning/reporting — so the
	// Agent tab can show the orchestration unfold in real time (the Demo
	// mode). Only ever set in agent mode. See llm.go's agentStep.
	Step *agentStep `json:"step,omitempty"`
	// Debug: see askJSONResponse.Debug's doc comment — same gate, same
	// shape, carried on the "done" (and "clarify") line instead since this
	// is the NDJSON streaming path.
	Debug *debugTrace `json:"debug,omitempty"`
}

// draftStreamMsg is POST /api/draft/reply's NDJSON envelope — one line per
// event, same "type" discriminator convention as askStreamMsg above:
// "step" for a live tool-use signal (identical agentStep shape/meaning,
// including the pre-flight router's Phase:"router" steps), "done" carrying
// the finished draftReply, "error" on failure after streaming already
// started. Replaces this endpoint's previous single buffered JSON-object
// response so "Thinking"/"Using SQL"/etc. can show live while a draft is
// being generated (web/app.js's agentStepsPanel). This endpoint isn't part
// of the documented public API (web/apidocs.js lists no /api/draft/*
// route), so changing its response shape carries no external compatibility
// risk — only R3's own browser UI consumes it.
type draftStreamMsg struct {
	Type  string      `json:"type"` // "step" | "done" | "error"
	Step  *agentStep  `json:"step,omitempty"`
	Draft *draftReply `json:"draft,omitempty"`
	Error string      `json:"error,omitempty"`
}

// flushingTokenWriter turns every Write call into one NDJSON "token" line,
// flushed immediately so the browser sees text as it arrives. Used only
// when streaming is enabled; see handleAsk.
type flushingTokenWriter struct {
	enc     *json.Encoder
	flusher http.Flusher
}

// Write implements io.Writer so flushingTokenWriter can be handed directly
// to anything that streams text (e.g. an LLM client) without that caller
// knowing about NDJSON framing at all.
func (t *flushingTokenWriter) Write(p []byte) (int, error) {
	if err := t.enc.Encode(askStreamMsg{Type: "token", Text: string(p)}); err != nil {
		return 0, err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return len(p), nil
}

// handleAsk streams its response as newline-delimited JSON
// (application/x-ndjson): one line per token while the answer is
// generated, then one final "done" line carrying citations. Retrieval
// happens before any bytes are written, so retrieval failures still get a
// normal HTTP error status; once streaming starts, a mid-stream failure
// can only be reported as an in-band "error" line (the HTTP status is
// already committed at 200).
//
// settings.DisableStreaming controls whether tokens are flushed as they
// arrive or buffered and sent as one "token" line right before "done" —
// same wire format either way, so the frontend's parsing logic doesn't
// need to branch on it.
// debugModeAllowed reports whether r's caller is an admin session — the
// single gate every Debug-Modus code path below checks before doing any
// extra work (building a debugTrace, attaching it to a response) or
// exposing anything (full system prompt, raw tool arguments/results)
// that would otherwise never leave the server. Tied to the same
// sessionClaims.IsAdmin used everywhere else admin status is checked
// (ldap.admin_users / ldap.required_group_dn, see ldapauth.go), so any
// admin gets Debug-Modus automatically instead of one hardcoded AD CN.
// No session at all (LDAP off, or an anonymous caller) always resolves
// to false.
func debugModeAllowed(r *http.Request) bool {
	claims, ok := currentSession(r)
	return ok && claims.IsAdmin
}

func handleAsk(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req askRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
			writeJSONError(w, "missing question", 400)
			return
		}
		s := settings.get()
		// Rejected outright (not truncated) — same "tell the caller, don't
		// silently mangle their input" posture as decodeAskImages' size
		// check below. Rune count, not byte length: a non-Latin question
		// shouldn't hit the limit sooner just because its UTF-8 encoding is
		// wider per character.
		if maxChars := effectiveMaxPromptChars(s.Upload); len([]rune(req.Question)) > maxChars {
			writeJSONError(w, fmt.Sprintf("question too long (%d characters, limit %d)", len([]rune(req.Question)), maxChars), 400)
			return
		}
		// Audit signal #1 per ENTERPRISE_READINESS.md: who asked what — the
		// question only, never the generated answer (which can be much
		// longer and is already visible to the asker in their own browser).
		logAudit(r, "ask", fmt.Sprintf("tier=%s mode=%s %s", req.Tier, req.Mode, truncateRunesNote(req.Question, 300)))
		k := req.K
		if k <= 0 {
			k = s.K
		}
		deptCode := ""
		claims, hasSession := currentSession(r)
		user := ""
		if hasSession {
			deptCode = resolveDeptCode(claims.IsAdmin, claims.DeptCode)
			user = sessionActor(claims)
		}
		// Guest rate limit (docs/TODO.md C6): only anonymous callers count
		// against it — a logged-in session already proved identity via
		// LDAP, so it's exempt, same "authenticated callers are trusted
		// more" reasoning as requireAdminSession/requireAPIKey elsewhere in
		// this file. 0 (default) disables the limit entirely.
		if !hasSession && !globalAskLimiter.allow(clientKey(r), s.API.GuestAskRateLimitPerMinute, time.Minute) {
			writeJSONError(w, "rate limit exceeded, please slow down", http.StatusTooManyRequests)
			return
		}

		// Preset resolution: Chat uses the caller's own choice
		// (req.Preset), Agent mode always uses the admin-fixed
		// agentConfig.DefaultPreset instead — same "admin-fixed, no
		// per-request choice" rule as everywhere else Agent differs from
		// Chat. An unknown/empty name resolves to the zero-value preset
		// (no restriction beyond SourceAccess), never an error.
		// Reasoning/execution tier (tier.go): supersedes the legacy req.Mode
		// (falling back to it when unset). One value drives the base prompt,
		// preset axis, tool set and round budget below — instant (RAG only),
		// standard (RAG + live tools, one round) or agent (full agentic set).
		tierPlan := resolveExecutionTier(req.Tier, req.Mode)

		presetName := req.Preset
		if tierPlan.PromptMode == "agent" {
			presetName = s.Agent.DefaultPreset
		}
		preset, _ := findPreset(s.Presets, presetName)

		// baselineRankingConfig (see its own doc comment) only changes
		// anything for the agent tier (tierPlan.PromptMode == "agent") AND
		// when s.Ranking.AgentModeMinFinalScore is configured above 0 — every
		// other case (in particular the instant/standard tiers, and every
		// agent deployment that hasn't opted in) gets s.Ranking unchanged,
		// exactly as before this setting existed.
		baselineCfg := baselineRankingConfig(tierPlan.PromptMode, s.Ranking)
		// Conversation-aware query rewrite (query_rewrite.go): only ever
		// changes what rankedSearch searches FOR, never req.Question itself
		// — the prompt/history/citations/logging below all keep using the
		// caller's original wording. Skipped entirely (no LLM call) when
		// disabled or there's no history to rewrite against; fail-open on
		// any error, same as the tool router below.
		retrievalQuery := req.Question
		if s.QueryRewrite.Enabled && len(req.History) > 0 {
			rewriteLM := rag.getChatLM(resolveQueryRewriteProfile(s.QueryRewrite, s.ChatProfile))
			retrievalQuery = rewriteQueryForRetrieval(r.Context(), rewriteLM, req.Question, req.History, s.QueryRewrite, askHistoryMax(s))
		}
		hits, err := rag.rankedSearchForIdentity(retrievalQuery, k, baselineCfg, s.activeEmbedModel(), s.SourceAccess, deptCode, user, preset.Kinds)
		if err != nil {
			writeJSONError(w, "retrieval failed: "+err.Error(), 500)
			return
		}
		contextText, citations := rag.assembleContextForIdentity(hits, baselineCfg, s.SourceAccess, deptCode, user, preset.Kinds)
		for i := range citations {
			citations[i].SourceURL = resolveSourceURL(citations[i].SourceID, s.URLMappings)
		}

		agentPrompt, selectedSkills := buildSystemPromptForMode(s.PromptsDir, req.Question, tierPlan.PromptMode)
		if verbose {
			log.Printf("[verbose] skills selected=%v", selectedSkills)
		}
		system := agentPrompt + "\n\nKontext:\n" + contextText
		if s.PersonalizeAnswers {
			if block := userContextBlock(r); block != "" {
				system = block + "\n\n" + system
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()

		// Token-usage logging (tokenusage.go): wrap ctx once, unconditionally,
		// so every chatOnce/chatStreamMessages call this request makes (chat,
		// tool-router preflight, agent sub-tasks) records into the same
		// trace regardless of which branch (json/ndjson) below actually
		// returns — a single defer covers every return path in this function.
		ctx, usageTrace := withTokenUsage(ctx, tokenUsageActor(r), "ask")
		defer recordTokenUsage(usageTrace)

		// Long-agent-run context compaction (llm.go's compactOldToolRounds):
		// admin-configurable here since this is the one call site with
		// agentConfig in scope; every other caller (draft.go, agent.go's
		// sub-agents/web-research, openai_api.go) gets the built-in
		// defaults automatically (contextCompactionFromContext).
		ctx = withContextCompaction(ctx, contextCompactionConfig{
			disabled:       s.Agent.ContextCompactionDisabled,
			thresholdChars: s.Agent.ContextCompactionThresholdChars,
			keepRounds:     s.Agent.ContextCompactionKeepRounds,
		})

		// Debug-Modus (see debugModeAllowed's doc comment): wrap ctx with a
		// fresh trace before the model/tool-calling machinery runs, so
		// llm.go's chatWithToolsBudget/runToolCalls populate it as a side
		// effect — dbgTrace stays nil for everyone else, and every debug*
		// call below is nil-receiver-safe.
		var dbgTrace *debugTrace
		if debugModeAllowed(r) {
			ctx, dbgTrace = withDebugTrace(ctx)
			dbgTrace.RetrievedChunks = hits
			if retrievalQuery != req.Question {
				dbgTrace.RewrittenQuery = retrievalQuery
			}
			dbgTrace.SelectedSkills = selectedSkills
		}

		profile := req.Profile
		if profile == "" {
			profile = s.ChatProfile
		}

		// Attached images (chatimages.go): decode/validate up front so a
		// bad upload fails fast with a 400, before any retrieval/tool
		// work runs. Routing is an explicit admin policy (uploadConfig),
		// not a per-request guess: "vision" mode reroutes the WHOLE
		// request to s.Upload.VisionProfile (overriding req.Profile/
		// s.ChatProfile below), since the vision-capable backend often
		// isn't the same one used for everyday text chat. If that
		// backend isn't configured, or the same guest/Azure policy that
		// gates a manual Azure pick below would deny it here too, the
		// image is dropped with a warning rather than failing (or
		// silently upgrading) the whole question — the caller still
		// gets a normal, text-only answer.
		decodedImages, err := decodeAskImages(req.Images, s.Upload)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		// resolveUploadRouting (chatimages.go) is a pure function — see its
		// own doc comment for the policy and why it's factored out this
		// way (unit-testable without a live chat backend, same reasoning
		// as resolveAskProfile below).
		routing := resolveUploadRouting(len(decodedImages) > 0, s.Upload, s.LDAP, authTierActive(s), hasSession, s.ChatProfile)
		var imageWarnings []string
		if routing.UseVision {
			profile = routing.Profile
		}
		if routing.DropImages {
			decodedImages = nil
		}
		if routing.Warning != "" {
			imageWarnings = append(imageWarnings, routing.Warning)
		}

		// "Registriert" tier (docs/UI_HARDENING_PLAN.md): picking the Azure
		// backend is cost-relevant, so once LDAP login exists at all, an
		// anonymous caller explicitly requesting it is either silently
		// downgraded to the default profile or rejected, per
		// ldap.guest_azure_profile_policy — checked server-side so this
		// can't be bypassed by calling /api/ask directly instead of through
		// the browser UI, which already hides the Azure option from guests.
		// resolveAskProfile is a pure function (see below) so this decision
		// is unit-testable without a live chat backend. Also applies to a
		// vision-routed profile above, but that case was already checked
		// (and, if denied, cleared) inside resolveUploadRouting, so this
		// never denies a request the image routing didn't already handle.
		profile, deny := resolveAskProfile(profile, s.LDAP, authTierActive(s), hasSession, s.ChatProfile)
		if deny {
			writeJSONError(w, "login required to use the Azure profile", http.StatusUnauthorized)
			return
		}
		chatLM := rag.getChatLM(profile)
		userMsg, ocrWarnings := buildUserMessage(req.Question, decodedImages, routing.UseVision, s.Import, s.Upload, s.AllowShellExec)
		imageWarnings = append(imageWarnings, ocrWarnings...)
		if len(imageWarnings) > 0 {
			// No dedicated NDJSON message type for this yet — folding it
			// into the system prompt lets the model itself mention the
			// problem in its answer, cheaper than inventing a new
			// step/warning channel for what should be a rare case.
			system += "\n\nHinweis an dich (das Modell) — bitte kurz erwähnen: " + strings.Join(imageWarnings, " ")
		}
		if dbgTrace != nil {
			dbgTrace.Profile = profile
			dbgTrace.Preset = preset.Name
			dbgTrace.PresetKinds = preset.Kinds
			dbgTrace.PresetTools = preset.Tools
			dbgTrace.DeptCode = deptCode
		}

		// Shared by the pre-flight tool router below and Agent mode's own
		// tool building further down (which used to construct its own,
		// identical copy of this).
		sess := agentSession{User: "anonym", DeptCode: deptCode, IsAdmin: !authTierActive(s), PresetKinds: preset.Kinds, PresetTools: preset.Tools}
		if hasSession {
			sess.User = sessionActor(claims)
			sess.IsAdmin = claims.IsAdmin
			sess.Groups = claims.Groups
		}
		// Track only the full Agent tier, from the point it is ready to call
		// the model until the request returns. The registry is in-memory and
		// records no prompt/tool payload — it powers the admin live-status
		// panel and Prometheus gauges without becoming another content log.
		if tierPlan.AgentTools {
			started := time.Now()
			var finish func()
			ctx, finish = beginActiveAgentRun(ctx, sess.User, profile)
			logAudit(r, "agent_run_start", fmt.Sprintf("profile=%s", profile))
			defer func() {
				finish()
				logAudit(r, "agent_run_finish", fmt.Sprintf("profile=%s duration_ms=%d", profile, time.Since(started).Milliseconds()))
			}()
		}
		// routeAndPrependContext runs the optional pre-flight tool router
		// (tool_router.go) once s.ToolRouter.Enabled, and — if it decided a
		// tool was needed — prepends its result to system. Called once per
		// response branch below (json vs. streaming), right after that
		// branch's own progress-emitter wiring (if any) is in place, so a
		// router-caused tool_start/tool_end reaches the live step timeline
		// exactly like the main call's own tool use would.
		routeAndPrependContext := func() {
			// The pre-flight tool router only makes sense when live tools are
			// in play. The instant tier deliberately runs pure RAG (no tools,
			// max speed), so it skips this extra LLM round entirely.
			if !tierPlan.LiveTools {
				return
			}
			routerLM := rag.getChatLM(resolveRouterProfile(s.ToolRouter, profile))
			if routerContext := runToolRouter(ctx, routerLM, req.Question, s, sess, preset, mssqlToolAllowed(authTierActive(s), hasSession)); routerContext != "" {
				system = routerContext + system
			}
		}

		var tools []toolDef
		executors := map[string]toolExecutor{}
		// Live answer-time tools (MSSQL/Shop/HTTP templates) are offered from
		// the "standard" tier upward; the "instant" tier deliberately runs
		// pure RAG with no tools at all (tierPlan.LiveTools == false), so the
		// tool loop degenerates to a single stream (llm.go) — max speed.
		// buildLiveTools (agent.go) is the SAME builder Agent mode, Mail
		// drafts and the external OpenAI-compatible API (openai_api.go) all
		// call — one place assembles MSSQL/Shop/HTTP-template tools for
		// every surface, so enabling a live tool in Settings makes it
		// available everywhere at once, with no separate, driftable copy of
		// this wiring per entry point (this inline block used to be exactly
		// that copy).
		if tierPlan.LiveTools {
			tools, executors = buildLiveTools(s, sess, preset, mssqlToolAllowed(authTierActive(s), hasSession))
		}

		// The agent tier gets the real agentic tool set (agent.go) and a
		// multi-round budget; standard keeps the original single tool round;
		// instant runs 0 rounds. The live-tool executors above are
		// audit-wrapped in agent tier too, so the audit log covers every tool
		// the agent can reach, not just the agent-specific ones.
		maxRounds := tierPlan.Rounds
		if tierPlan.AgentTools {
			for name, exec := range executors {
				executors[name] = auditExecutor(sess.User, name, exec)
			}
			agentTools, agentExecs := buildAgentTools(rag, s, sess)
			tools = append(tools, agentTools...)
			for name, exec := range agentExecs {
				executors[name] = exec
			}
		}
		if maxRounds == agentRoundsSentinel {
			maxRounds = agentMaxRounds(s.Agent)
		}

		messages := append(historyToChatMsgs(req.History, askHistoryMax(s)), userMsg)

		if req.Format == "json" {
			routeAndPrependContext()
			var buffered strings.Builder
			if err := chatLM.chatWithToolsBudget(ctx, system, messages, tools, executors, &buffered, maxRounds); err != nil {
				var clar *ErrClarificationNeeded
				if errors.As(err, &clar) {
					dbgTrace.finish()
					writeJSON(w, askJSONResponse{Clarify: &askClarification{Question: clar.Question, Options: clar.Options}, Debug: dbgTrace})
					return
				}
				writeJSONError(w, "chat failed: "+err.Error(), 500)
				return
			}
			answer := strings.TrimSpace(buffered.String())
			if dbgTrace != nil {
				dbgTrace.RawAnswer = answer
			}
			dbgTrace.finish()
			writeJSON(w, askJSONResponse{Answer: answer, Citations: filterCitations(citations, answer, s), Debug: dbgTrace})
			return
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Accel-Buffering", "no") // defeat nginx's proxy buffering, if this ever sits behind one
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)

		// buffered always ends up holding the full answer text, streamed
		// or not — filterCitations (below) needs the complete text to know
		// which "[Qn]" markers the model actually used, which isn't known
		// until generation finishes either way.
		var buffered strings.Builder
		var tokenWriter io.Writer = &buffered
		if !s.DisableStreaming {
			tokenWriter = io.MultiWriter(&buffered, &flushingTokenWriter{enc: enc, flusher: flusher})
		}

		// Live agent-progress → NDJSON "step" lines (the Demo mode's live
		// view, and — since the pre-flight tool router was added — also
		// how Chat surfaces "using tool X"). Previously agent-mode-only;
		// now unconditional, since plain chat can also cause a tool call
		// (either via the router below or the model's own single tool
		// round) that's worth showing rather than leaving invisible.
		// Emitted from tool goroutines, so guard the shared encoder/flusher
		// with a mutex; no token writing overlaps (final answer streams
		// only after all tool rounds finish).
		var stepMu sync.Mutex
		ctx = withAgentProgress(ctx, func(st agentStep) {
			stepMu.Lock()
			defer stepMu.Unlock()
			step := st
			_ = enc.Encode(askStreamMsg{Type: "step", Step: &step})
			if flusher != nil {
				flusher.Flush()
			}
		})

		routeAndPrependContext()

		if err := chatLM.chatWithToolsBudget(ctx, system, messages, tools, executors, tokenWriter, maxRounds); err != nil {
			var clar *ErrClarificationNeeded
			if errors.As(err, &clar) {
				dbgTrace.finish()
				_ = enc.Encode(askStreamMsg{Type: "clarify", Clarify: &askClarification{Question: clar.Question, Options: clar.Options}, Debug: dbgTrace})
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			_ = enc.Encode(askStreamMsg{Type: "error", Error: err.Error()})
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		answer := strings.TrimSpace(buffered.String())
		if dbgTrace != nil {
			dbgTrace.RawAnswer = answer
		}
		dbgTrace.finish()
		if s.DisableStreaming {
			_ = enc.Encode(askStreamMsg{Type: "token", Text: answer})
		}
		_ = enc.Encode(askStreamMsg{Type: "done", Citations: filterCitations(citations, answer, s), Debug: dbgTrace})
		if flusher != nil {
			flusher.Flush()
		}
	}
}

type searchRequest struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

// handleSearch is the non-generating counterpart to handleAsk: runs the
// same ranked retrieval but returns the raw hits as one JSON response
// instead of feeding them to an LLM — useful for callers that just want
// citations/snippets, e.g. the API-key-authenticated /api/search route.
func handleSearch(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req searchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Query) == "" {
			writeJSONError(w, "missing query", 400)
			return
		}
		s := settings.get()
		k := req.K
		if k <= 0 {
			k = s.K
		}
		deptCode := ""
		user := ""
		if claims, ok := currentSession(r); ok {
			deptCode = resolveDeptCode(claims.IsAdmin, claims.DeptCode)
			user = sessionActor(claims)
		}
		// nil presetKinds: /api/search is the external API-key surface, not
		// Chat/Agent/Mail — presets are deliberately out of scope for it
		// (see preset.go's package comment), only SourceAccess applies.
		hits, err := rag.rankedSearchForIdentity(req.Query, k, s.Ranking, s.activeEmbedModel(), s.SourceAccess, deptCode, user, nil)
		if err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		writeJSON(w, hits)
	}
}

type chatEmailRequest struct {
	Question  string   `json:"question"`
	Answer    string   `json:"answer"`
	Citations []string `json:"citations,omitempty"`
	// Attachments lets the Mail tab's "An mich senden" button (which
	// reuses this same endpoint) attach a scan/photo alongside the draft
	// — see mail.go's mailAttachmentInput/decodeMailAttachments. Empty
	// for the plain chat-answer "send to me" use of this endpoint.
	Attachments []mailAttachmentInput `json:"attachments,omitempty"`
}

// handleChatEmail sends the current chat answer to the asking user's own AD
// mail address (session claims.Mail) — never a caller-supplied address, so
// R3 can't be used to relay mail to arbitrary recipients. Deliberately
// ungated (not requireAdminSession) but requires *some* valid session,
// following the same currentSession-inline-check pattern as handleAsk/
// handleDraftReply (both read claims without demanding admin).
func handleChatEmail(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		claims, ok := currentSession(r)
		if !ok {
			writeJSONError(w, "login required", 401)
			return
		}
		if claims.Mail == "" {
			writeJSONError(w, "no email address on file for this account", 400)
			return
		}
		s := settings.get()
		if !s.SMTP.Enabled {
			writeJSONError(w, "email is not configured", 400)
			return
		}
		var req chatEmailRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "invalid body", 400)
			return
		}
		if strings.TrimSpace(req.Answer) == "" {
			writeJSONError(w, "answer is empty", 400)
			return
		}

		subject := fmt.Sprintf("R3: %s", truncateForSubject(req.Question))
		var body strings.Builder
		if req.Question != "" {
			fmt.Fprintf(&body, "Frage:\n%s\n\n", req.Question)
		}
		fmt.Fprintf(&body, "Antwort:\n%s\n", req.Answer)
		if len(req.Citations) > 0 {
			body.WriteString("\nQuellen:\n")
			for _, c := range req.Citations {
				fmt.Fprintf(&body, "- %s\n", c)
			}
		}

		attachments, err := decodeMailAttachments(req.Attachments)
		if err != nil {
			writeJSONError(w, err.Error(), 400)
			return
		}
		if err := sendMail(s.SMTP, claims.Mail, subject, body.String(), attachments...); err != nil {
			writeJSONError(w, err.Error(), 502)
			return
		}
		logAudit(r, "chat_email_sent", fmt.Sprintf("to=%s attachments=%d", claims.Mail, len(attachments)))
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// truncateForSubject keeps an email subject line short even if the original
// question was long — 80 runes is generous for a subject while staying
// well under common client-truncation thresholds.
func truncateForSubject(question string) string {
	q := strings.TrimSpace(question)
	if q == "" {
		return "Antwort aus dem Chat"
	}
	r := []rune(q)
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return q
}

type webImportRequest struct {
	URLs   []string `json:"urls"`
	DryRun bool     `json:"dry_run,omitempty"`
	// Crawl mode: follow links from URLs (the crawl's seeds) instead of
	// treating them as an exhaustive, exact list — see webimport.go's
	// crawlWebPages doc comment for the depth/page/host-restriction
	// semantics.
	Crawl           bool `json:"crawl,omitempty"`
	MaxDepth        int  `json:"max_depth,omitempty"`
	MaxPages        int  `json:"max_pages,omitempty"`
	AllowOtherHosts bool `json:"allow_other_hosts,omitempty"`
}

type webStreamMsg struct {
	Type   string          `json:"type"`
	URL    string          `json:"url,omitempty"`
	Result webImportResult `json:"result"`
}

// handleWebImport fetches and ingests each pasted URL (see webimport.go's
// importWebPages — including its doc comment on why this connector is
// admin-gated and SSRF-relevant), streaming progress the same way
// handleExchangeMailImport does. No preview/select step: the admin already
// chose exactly which URLs to import by pasting them.
func handleWebImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req webImportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "invalid body", 400)
			return
		}
		if len(req.URLs) == 0 {
			writeJSONError(w, "no URLs given", 400)
			return
		}
		s := settings.get()

		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		var res webImportResult
		if req.Crawl {
			res = crawlWebPages(ctx, rag, s, s.activeEmbedModel(), req.URLs, req.MaxDepth, req.MaxPages, req.AllowOtherHosts, req.DryRun, func(p webProgress) {
				emit(webStreamMsg{Type: "progress", URL: p.URL, Result: p.Result})
			})
		} else {
			res = importWebPages(ctx, rag, s, s.activeEmbedModel(), req.URLs, req.DryRun, func(p webProgress) {
				emit(webStreamMsg{Type: "progress", URL: p.URL, Result: p.Result})
			})
		}
		logImportAudit(r, "web", res.baseImportResult)
		emit(webStreamMsg{Type: "done", Result: res})
	}
}

// handleSources lists every ingested source for the admin sources table,
// resolving each one's display URL (settings.URLMappings) along the way.
// No method restriction — GET-only in practice, but nothing enforces it.
type draftReplyRequest struct {
	SourceID string `json:"source_id,omitempty"`
	// RawEmail carries a pasted customer email verbatim (the "Mail" tab),
	// as an alternative to SourceID (the PST-import source popup). Exactly
	// one of the two must be set — RawEmail takes priority if both are.
	// Unlike SourceID, it isn't scoped to any department (there's no stored
	// source behind it), so that branch skips sourceAccessAllowedForRequest
	// entirely and searches with no department restriction, same as an
	// anonymous /api/ask call.
	RawEmail string `json:"raw_email,omitempty"`
	// Brief switches the request into "Neue E-Mail" mode (composeNewMail,
	// draft.go): a freeform description of the mail to write — recipient,
	// topic, key points — instead of an incoming message to reply to.
	// Takes priority over both fields above. Retrieval uses the caller's
	// session department (like /api/ask), not the RawEmail branch's
	// deliberately anonymous scope: a compose brief is the caller asking
	// on their own behalf, not a third-party mail of unknown provenance.
	Brief string `json:"brief,omitempty"`
	// Length/Format are the Mail tab's optional draft-shape selectors,
	// resolved server-side against a closed set (draft.go's
	// draftFormatInstruction) so a client can't inject arbitrary prompt text.
	// Empty = the model's default ("normal" length, prose). They only steer
	// tone/shape — never the facts, which stay grounded in the retrieved
	// context.
	Length string `json:"length,omitempty"`
	Format string `json:"format,omitempty"`
	// Instructions is the Mail tab's optional free-text "situativer
	// Kontext"/instruction field — e.g. an upcoming customer appointment or
	// a communication preference the human requesting the draft knows
	// about but that isn't in the knowledge base or the mail itself.
	// Folded into the system context of the generation prompt
	// (draft.go's composeDraftReply/composeNewMail) as an explicit
	// instruction, never treated as ground truth about the subject matter.
	Instructions string `json:"instructions,omitempty"`
}

// handleDraftReply generates a proposed reply/new-mail draft, grounded in
// similar cases from the knowledge base (composeDraftReply/composeNewMail,
// draft.go). Deliberately ungated by settings.EnableDraftReplies (unlike
// handleDraftSaveIMAP below) — computing and displaying a draft is a
// read-only LLM+knowledge-base call that never touches IMAP/SMTP/Exchange,
// the same trust level as /api/ask. Gated by requireSessionIfLDAP instead
// (docs/UI_HARDENING_PLAN.md's "Registriert" tier): drafting is a
// cost-relevant LLM call, so once LDAP login exists at all, it should be
// tied to a real identity — but with LDAP off (the only tier is guest),
// this stays exactly as open as /api/ask. EnableDraftReplies still
// controls two separate, narrower things: the
// "Antwortentwurf erstellen" button in the PST source popup (a UI-declutter
// toggle, see handleSourceContent), and the Agent tab's mail-draft tool. The
// SourceID path here additionally checks source_access
// (sourceAccessAllowedForRequest) like its sibling content/original
// endpoints — the target email itself, not just the supplementary
// rankedSearch context composeDraftReply pulls in, must respect the
// requester's department. The result is always a suggestion for a human to
// review and send manually — see draft.go's package comment: R3 never sends
// anything itself.
func handleDraftReply(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s := settings.get()
		if !requireSessionIfLDAP(w, r, s) {
			return
		}
		var req draftReplyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "invalid body", 400)
			return
		}

		// Mail always uses the admin-fixed DraftPreset — same "admin-fixed,
		// no per-request choice" rule as Agent's DefaultPreset.
		preset, _ := findPreset(s.Presets, s.DraftPreset)

		// Agentic Mail (buildMailTools, agent.go): the draft model gets the
		// read-only knowledge-base tools (so it can pull more chunks / open a
		// full source on its own) plus every live tool the settings + preset
		// allow (Shop, MSSQL generic/templates, HTTP templates), and a real
		// multi-round budget — the same capabilities as the Agent tab, minus
		// the tools that don't fit a one-shot draft (see buildMailTools).
		claims, hasMailSession := currentSession(r)
		mailSess := agentSession{User: "anonym", DeptCode: "", IsAdmin: !authTierActive(s), PresetKinds: preset.Kinds, PresetTools: preset.Tools}
		if hasMailSession {
			mailSess.User = sessionActor(claims)
			mailSess.IsAdmin = claims.IsAdmin
			mailSess.DeptCode = resolveDeptCode(claims.IsAdmin, claims.DeptCode)
			mailSess.Groups = claims.Groups
		}
		draftTools, draftExecutors := buildMailTools(rag, s, mailSess, preset, mssqlToolAllowed(authTierActive(s), hasMailSession))
		draftRounds := draftMaxToolRounds(s)

		// Fold the requester's own personal context (userprefs.go, Phase 4)
		// into whatever situational instructions they typed — same two
		// independent gates as userContextBlock (handleAsk): the admin's
		// deployment-wide PersonalizeAnswers AND the user's own
		// UsePersonalContext opt-in. Looked up by claims.User (the AD
		// CN/login userPrefsDB is keyed by — see requireSession), not
		// mailSess.User (which prefers claims.Mail for audit/dept purposes,
		// a different identity string). Especially relevant for mail: see
		// personalContextBlock's doc comment on why this stays framed as
		// background, never as a fact about the mail being answered.
		effectiveInstructions := strings.TrimSpace(req.Instructions)
		if hasMailSession && s.PersonalizeAnswers {
			if personal := personalContextBlock(claims.User); personal != "" {
				if effectiveInstructions != "" {
					effectiveInstructions += "\n\n" + personal
				} else {
					effectiveInstructions = personal
				}
			}
		}

		// NDJSON streaming setup, shared by both branches below (compose vs.
		// reply) — constructing the encoder/emitter here is harmless and
		// stateless; the actual response headers are only set in each
		// branch, right before it starts generating, so every early
		// writeJSONError above/below this point still returns a normal
		// plain-JSON error response undisturbed. Same "step" mechanism as
		// handleAsk: a live tool_start/tool_end/subagent_*/router-tagged
		// step per NDJSON line, then one final "done" line carrying the
		// complete draftReply — replacing the single buffered JSON object
		// this endpoint used to return, so "Thinking"/"Using SQL"/etc. can
		// show live while a draft is being generated (see draftStreamMsg's
		// doc comment).
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		var stepMu sync.Mutex
		emitStep := func(st agentStep) {
			stepMu.Lock()
			defer stepMu.Unlock()
			_ = enc.Encode(draftStreamMsg{Type: "step", Step: &st})
			if flusher != nil {
				flusher.Flush()
			}
		}
		startStreaming := func() {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("X-Accel-Buffering", "no")
		}
		routerLM := rag.getChatLM(resolveRouterProfile(s.ToolRouter, s.DraftChatProfile))

		if strings.TrimSpace(req.Brief) != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
			defer cancel()
			ctx, usageTrace := withTokenUsage(ctx, tokenUsageActor(r), "draft_new_mail")
			defer recordTokenUsage(usageTrace)
			// Debug-Modus: see debugModeAllowed's doc comment. composeNewMail
			// reads the trace back out of ctx itself and attaches it to the
			// returned draftReply.Debug — nothing further to do here.
			if debugModeAllowed(r) {
				ctx, _ = withDebugTrace(ctx)
			}
			startStreaming()
			ctx = withAgentProgress(ctx, emitStep)
			routerContext := runToolRouter(ctx, routerLM, req.Brief, s, mailSess, preset, mssqlToolAllowed(authTierActive(s), hasMailSession))
			draft, err := composeNewMail(ctx, rag, s.Ranking, s.activeEmbedModel(), s.DraftChatProfile, s.K, s.SourceAccess, mailSess.DeptCode, mailSess.User, preset.Kinds, s.PromptsDir, req.Brief, draftTools, draftExecutors, draftRounds, routerContext, draftFormatInstruction(req.Length, req.Format), effectiveInstructions)
			if err != nil {
				_ = enc.Encode(draftStreamMsg{Type: "error", Error: "generate draft: " + err.Error()})
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			for i := range draft.Citations {
				draft.Citations[i].SourceURL = resolveSourceURL(draft.Citations[i].SourceID, s.URLMappings)
			}
			logAudit(r, "draft", fmt.Sprintf("mode=compose %s", truncateRunesNote(req.Brief, 300)))
			_ = enc.Encode(draftStreamMsg{Type: "done", Draft: &draft})
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		var mail emailFields
		if strings.TrimSpace(req.RawEmail) != "" {
			mail = parseRawEmail(req.RawEmail)
		} else {
			if req.SourceID == "" {
				writeJSONError(w, "missing source_id or raw_email", 400)
				return
			}
			if !sourceAccessAllowedForRequest(r, s, rag, req.SourceID) {
				writeJSONError(w, "source not found", 404)
				return
			}
			content, ok := rag.fetchSourceContent(req.SourceID)
			if !ok {
				writeJSONError(w, "source not found", 404)
				return
			}
			mail = parseStoredEmail(content)
			// Attach the original email's own attachments deterministically
			// (by source_id prefix, see fetchAttachmentSourceContents) rather
			// than leaving it to rankedSearch's semantic luck — a reply
			// referencing "the attached invoice" should see the invoice even
			// if the retrieval query didn't happen to surface it.
			if attachments := rag.fetchAttachmentSourceContentsForIdentity(req.SourceID, s.SourceAccess, mailSess.DeptCode, mailSess.User); len(attachments) > 0 {
				mail.Body = mail.Body + "\n\n" + strings.Join(attachments, "\n\n")
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
		ctx, usageTrace := withTokenUsage(ctx, tokenUsageActor(r), "draft_reply")
		defer recordTokenUsage(usageTrace)
		if debugModeAllowed(r) {
			ctx, _ = withDebugTrace(ctx)
		}
		startStreaming()
		ctx = withAgentProgress(ctx, emitStep)
		routerContext := runToolRouter(ctx, routerLM, mail.Subject+"\n"+mail.Body, s, mailSess, preset, mssqlToolAllowed(authTierActive(s), hasMailSession))
		draft, err := composeDraftReply(ctx, rag, s.Ranking, s.activeEmbedModel(), s.DraftChatProfile, s.K, s.SourceAccess, mailSess.DeptCode, mailSess.User, preset.Kinds, s.PromptsDir, mail, draftTools, draftExecutors, draftRounds, routerContext, draftFormatInstruction(req.Length, req.Format), effectiveInstructions)
		if err != nil {
			_ = enc.Encode(draftStreamMsg{Type: "error", Error: "generate draft: " + err.Error()})
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		for i := range draft.Citations {
			draft.Citations[i].SourceURL = resolveSourceURL(draft.Citations[i].SourceID, s.URLMappings)
		}
		logAudit(r, "draft", fmt.Sprintf("mode=reply source_id=%s from=%s", req.SourceID, mail.From))
		_ = enc.Encode(draftStreamMsg{Type: "done", Draft: &draft})
		if flusher != nil {
			flusher.Flush()
		}
	}
}

type draftRestyleRequest struct {
	Text  string `json:"text"`
	Style string `json:"style"`
}

// handleDraftRestyle rewrites an already-generated draft's tone/wording —
// the Mail tab's "Stil" dropdown — without touching the knowledge base
// again (see draft.go's restyleDraftText doc comment). Same access gate
// as handleDraftReply (requireSessionIfLDAP): restyling is still a
// cost-relevant LLM call, just a much cheaper one (no retrieval, no
// tools).
func handleDraftRestyle(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s := settings.get()
		if !requireSessionIfLDAP(w, r, s) {
			return
		}
		var req draftRestyleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
			writeJSONError(w, "missing text", 400)
			return
		}
		if _, ok := draftStyleInstruction[req.Style]; !ok {
			writeJSONError(w, "unknown style", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		ctx, usageTrace := withTokenUsage(ctx, tokenUsageActor(r), "draft_restyle")
		defer recordTokenUsage(usageTrace)
		lm := rag.getChatLM(s.DraftChatProfile)
		restyled, err := restyleDraftText(ctx, lm, req.Text, req.Style)
		if err != nil {
			writeJSONError(w, "restyle: "+err.Error(), 500)
			return
		}
		logAudit(r, "draft_restyle", fmt.Sprintf("style=%s", req.Style))
		writeJSON(w, map[string]string{"text": restyled})
	}
}

// parseRawEmail wraps a pasted customer email (the "Mail" tab) as
// emailFields for composeDraftReply. Unlike parseStoredEmail (which
// reverses R3's own generated header format), pasted mail has no
// guaranteed structure, so this deliberately does no header parsing: the
// whole pasted text becomes Body, Subject/From stay empty. composeDraftReply
// tolerates that fine (draft.go's userMsg just prints empty strings for
// them).
func parseRawEmail(raw string) emailFields {
	return emailFields{Body: strings.TrimSpace(raw)}
}

type draftSaveIMAPRequest struct {
	To      string `json:"to,omitempty"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	// Attachments — see mail.go's mailAttachmentInput/decodeMailAttachments.
	Attachments []mailAttachmentInput `json:"attachments,omitempty"`
}

// handleDraftSaveIMAP IMAP-APPENDs a reviewed draft into the configured
// mailbox's Drafts folder (saveDraftToMailbox, draft.go) — the Mail tab's
// "In Postfach-Entwürfe speichern" action, and deliberately the only
// mailbox write in R3: the human still opens, reviews and sends the draft
// from their own mail client; there is no send path here. Admin-gated
// (unlike /api/draft/reply itself): generating a draft is a read-only
// LLM call any employee may use, but writing into the shared service
// mailbox is an operator action. Requires the IMAP connector to be
// enabled, same switch as importing from it.
func handleDraftSaveIMAP(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s := settings.get()
	// firstEnabledConn: this action predates multi-instance IMAP and has no
	// per-request way to pick among several configured mailboxes — same
	// "first enabled" fallback as agent.go's save_draft_to_mailbox tool.
	imapConn, imapOK := firstEnabledConn(s.IMAP)
	if !requireEnabled(w, imapOK, "IMAP is not enabled — configure it under Einstellungen -> IMAP first") {
		return
	}
	var req draftSaveIMAPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" {
		writeJSONError(w, "missing draft body", 400)
		return
	}
	attachments, err := decodeMailAttachments(req.Attachments)
	if err != nil {
		writeJSONError(w, err.Error(), 400)
		return
	}
	if err := saveDraftToMailbox(newIMAPClient(imapConn), imapConn, strings.TrimSpace(req.To), strings.TrimSpace(req.Subject), req.Body, attachments...); err != nil {
		writeJSONError(w, err.Error(), 502)
		return
	}
	logAudit(r, "draft_save_imap", fmt.Sprintf("mailbox=%s to=%s subject=%q attachments=%d", draftsMailboxOrDefault(imapConn), req.To, req.Subject, len(attachments)))
	writeJSON(w, map[string]any{"ok": true, "mailbox": draftsMailboxOrDefault(imapConn)})
}

// draftEmlRequest mirrors draftSaveIMAPRequest's shape — this endpoint
// exists purely so an attachment can be folded into valid MIME bytes
// server-side (buildMultipartEmail, mail.go) instead of teaching the
// browser a second, hand-rolled multipart writer just for the "Als .eml
// herunterladen" button. The attachment-free case still needs no server
// round-trip at all — app.js's client-side buildEmlText is unchanged and
// stays the path used whenever there's nothing to attach.
type draftEmlRequest struct {
	To          string                `json:"to,omitempty"`
	Subject     string                `json:"subject"`
	Body        string                `json:"body"`
	Attachments []mailAttachmentInput `json:"attachments,omitempty"`
}

// handleDraftEml renders a reviewed draft WITH attachments as raw .eml
// bytes for download. Ungated like /api/draft/reply itself — formatting
// a draft isn't an operator action; only writing it into the shared
// mailbox (handleDraftSaveIMAP) is.
func handleDraftEml(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req draftEmlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" {
		writeJSONError(w, "missing draft body", 400)
		return
	}
	attachments, err := decodeMailAttachments(req.Attachments)
	if err != nil {
		writeJSONError(w, err.Error(), 400)
		return
	}
	if len(attachments) == 0 {
		writeJSONError(w, "no attachments given — use the client-side .eml download instead", 400)
		return
	}
	// From is cosmetic here (the file is a draft to review/edit/send in a
	// real mail client, never sent by R3) — the logged-in account's own
	// address if known, otherwise left blank like the existing
	// client-side buildEmlText already does.
	from := ""
	if claims, ok := currentSession(r); ok {
		from = claims.Mail
	}
	msg := buildMultipartEmail(from, strings.TrimSpace(req.To), strings.TrimSpace(req.Subject), req.Body, attachments)
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", `attachment; filename="entwurf.eml"`)
	w.Write(msg)
}

// parseStoredEmail reverses emailFields.String() (extract.go) well enough
// to recover Subject/From/Body from a source's stored, chunked-and-
// rejoined content (see fetchSourceContent). The header lines it writes
// reliably survive chunking as the leading lines of the first chunk —
// they're written before the body and are each far shorter than the
// chunk size, so they never straddle a chunk boundary. Falls back to
// treating the whole content as Body when it doesn't look like R3's own
// header format (e.g. a non-email upload).
func parseStoredEmail(content string) emailFields {
	header, body, found := strings.Cut(content, "\n\n")
	if !found {
		return emailFields{Body: content}
	}
	var f emailFields
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "From: "):
			f.From = strings.TrimPrefix(line, "From: ")
		case strings.HasPrefix(line, "To: "):
			f.To = strings.TrimPrefix(line, "To: ")
		case strings.HasPrefix(line, "Subject: "):
			f.Subject = strings.TrimPrefix(line, "Subject: ")
		}
	}
	if f.Subject == "" && f.From == "" {
		return emailFields{Body: content}
	}
	f.Body = body
	return f
}
