package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"embed"

	tinysql "github.com/SimonWaldherr/tinySQL"
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

// examplesFS embeds the static theme/scenario gallery (examples/*.html) as a
// standalone reference — pure static HTML/CSS/JS, no server-side rendering
// or API calls, browsable even outside the running app. Kept as a directory
// embed.FS (rather than single-file string embeds like the assets above) so
// more static example pages can be dropped into examples/ without touching
// main.go. See examples/gallery.html and docs/ui-customization.md.
//
//go:embed examples
var examplesFS embed.FS

// loginPageHTML is a minimal self-contained login page served at /login.
// Two %s placeholders: (1) page title / (2) app name shown as heading.
const loginPageHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>%s – Login</title>
  <style>
    *{box-sizing:border-box;margin:0;padding:0}
    body{font-family:system-ui,sans-serif;background:#f0f2f5;display:flex;align-items:center;justify-content:center;min-height:100vh}
    .card{background:#fff;border-radius:12px;padding:40px 36px;width:100%%;max-width:360px;box-shadow:0 4px 24px rgba(0,0,0,.1)}
    h1{font-size:1.4rem;font-weight:700;margin-bottom:6px;color:#111}
    .subtitle{color:#666;font-size:.9rem;margin-bottom:28px}
    label{display:block;font-size:.85rem;font-weight:500;margin-bottom:4px;color:#444}
    input{width:100%%;padding:10px 12px;border:1px solid #ddd;border-radius:8px;font-size:.95rem;outline:none;transition:border-color .15s}
    input:focus{border-color:#6366f1}
    .field{margin-bottom:18px}
    button{width:100%%;padding:11px;background:#6366f1;color:#fff;border:none;border-radius:8px;font-size:1rem;font-weight:600;cursor:pointer;transition:opacity .15s}
    button:hover{opacity:.88}
    #err{color:#dc2626;font-size:.85rem;margin-top:12px;display:none}
  </style>
</head>
<body>
<div class="card">
  <h1>%s</h1>
  <p class="subtitle">Sign in to continue</p>
  <form id="loginForm">
    <div class="field">
      <label for="username">Username</label>
      <input id="username" type="text" autocomplete="username" autofocus required>
    </div>
    <div class="field">
      <label for="password">Password</label>
      <input id="password" type="password" autocomplete="current-password" required>
    </div>
    <button type="submit">Sign in</button>
    <p id="err"></p>
  </form>
</div>
<script>
document.getElementById('loginForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const errEl = document.getElementById('err');
  errEl.style.display = 'none';
  const username = document.getElementById('username').value.trim();
  const password = document.getElementById('password').value;
  try {
    const res = await fetch('/api/auth/login', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({username, password})
    });
    if (res.ok) {
      const params = new URLSearchParams(window.location.search);
      window.location.href = params.get('next') || '/';
    } else {
      errEl.textContent = 'Invalid username or password.';
      errEl.style.display = 'block';
    }
  } catch(e) {
    errEl.textContent = 'Network error. Please try again.';
    errEl.style.display = 'block';
  }
});
</script>
</body>
</html>`

// runWebServer registers HTTP handlers and starts the web interface.
func runWebServer(rag *ragSystem, addr string, settings *settingsStore, chats *chatStore, customAPIs *apiStore, personas *personaStore, modules *moduleStore, connectors *connectorStore, connectorExec *connectorExecutor, llmAvailable bool, llmPingErr error) {
	mux := http.NewServeMux()
	adminUsers := newAdminUserStore(settings)
	apiRoutes := newAPIRouteStore(settings)
	connectorRegistryStore = connectors
	connectorRuntimeExec = connectorExec
	adminGuard := func(h http.HandlerFunc) http.HandlerFunc {
		return webUIAuthMiddleware(routePolicyMiddleware(settings, h))
	}

	// Static assets
	mux.HandleFunc("/", webUIAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	}))
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		fmt.Fprint(w, styleCSS)
	})
	mux.HandleFunc("/app.js", webUIAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		fmt.Fprint(w, appJS)
	}))

	// Static theme/scenario gallery — self-contained reference page(s) embedded
	// via examplesFS, no auth required (no sensitive data, same tier as style.css).
	mux.Handle("/examples/", http.FileServer(http.FS(examplesFS)))
	mux.HandleFunc("/gallery", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/examples/gallery.html", http.StatusFound)
	})

	// GET /api/settings — current settings
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			s := settings.get()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"base_url":                         s.BaseURL,
				"inference_api":                    s.InferenceAPI,
				"chat_base":                        s.ChatBase,
				"embed_base":                       s.EmbedBase,
				"chat_model":                       s.ChatModel,
				"embed_model":                      s.EmbedModel,
				"lang":                             s.Lang,
				"theme":                            s.Theme,
				"density":                          s.Density,
				"active_role":                      s.ActiveRole,
				"role_permissions":                 permissionsForRole(s.ActiveRole),
				"usage_profile":                    s.UsageProfile,
				"response_language_mode":           s.ResponseLanguageMode,
				"redact_pii":                       s.RedactPII,
				"chunk_size":                       s.ChunkSize,
				"k":                                s.K,
				"allow_nanogo":                     s.AllowNanoGo,
				"rerank_mode":                      s.RerankMode,
				"retrieval_mode":                   s.RetrievalMode,
				"tinysql_audit_enabled":            s.TinySQLAuditEnabled,
				"tinysql_audit_path":               s.TinySQLAuditPath,
				"storage_encryption_enabled":       s.StorageEncryptionEnabled,
				"geo_import_enabled":               s.GeoImportEnabled,
				"tinysql_vector_cache_entries":     s.TinySQLVectorCacheEntries,
				"tinysql_vector_cache_ttl_seconds": s.TinySQLVectorCacheTTLSeconds,
				"tinysql_vector_analytics":         s.TinySQLVectorAnalytics,
				"agent_planner_enabled":            s.AgentPlannerEnabled,
				"agent_max_plan_steps":             s.AgentMaxPlanSteps,
				// Do not return the API key itself; only expose whether one is configured
				"openai_key_present": s.OpenAIKey != "",
				// Branding (safe to expose publicly)
				"app_name":     s.AppName,
				"app_logo_url": s.AppLogoURL,
				"custom_css":   s.CustomCSS,
				// Auth capabilities (safe to expose so UI can show/hide login)
				"web_ui_auth":  s.WebUIAuth,
				"ldap_enabled": s.LDAPEnabled,
			})
			return

		case "POST":
			// Accept chat_base/embed_base and optional OpenAI key for mixed backends
			var req struct {
				BaseURL                      string `json:"base_url"`
				InferenceAPI                 string `json:"inference_api"`
				ChatBase                     string `json:"chat_base"`
				EmbedBase                    string `json:"embed_base"`
				ChatModel                    string `json:"chat_model"`
				EmbedModel                   string `json:"embed_model"`
				OpenAIKey                    string `json:"openai_api_key"`
				OpenAIKeyClear               bool   `json:"openai_api_key_clear"`
				Theme                        string `json:"theme"`
				ActiveRole                   string `json:"active_role"`
				UsageProfile                 string `json:"usage_profile"`
				ResponseLang                 string `json:"response_language_mode"`
				RedactPII                    *bool  `json:"redact_pii"`
				AllowNanoGo                  *bool  `json:"allow_nanogo"`
				RerankMode                   string `json:"rerank_mode"`
				RetrievalMode                string `json:"retrieval_mode"`
				TinySQLAuditEnabled          *bool  `json:"tinysql_audit_enabled"`
				TinySQLAuditPath             string `json:"tinysql_audit_path"`
				StorageEncryptionEnabled     *bool  `json:"storage_encryption_enabled"`
				GeoImportEnabled             *bool  `json:"geo_import_enabled"`
				TinySQLVectorCacheEntries    *int   `json:"tinysql_vector_cache_entries"`
				TinySQLVectorCacheTTLSeconds *int   `json:"tinysql_vector_cache_ttl_seconds"`
				TinySQLVectorAnalytics       *bool  `json:"tinysql_vector_analytics"`
				AgentPlanner                 *bool  `json:"agent_planner_enabled"`
				AgentMaxSteps                *int   `json:"agent_max_plan_steps"`
				Force                        bool   `json:"force"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			// Normalize and fall back
			inferenceAPISupplied := strings.TrimSpace(req.InferenceAPI) != ""
			if req.BaseURL != "" {
				req.BaseURL = normalizeBaseURL(req.BaseURL)
			}
			req.InferenceAPI = normalizeInferenceAPI(req.InferenceAPI)
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

			// Warn on embedding model changes if DB already has data.  Keep an
			// explicitly configured protocol when an older API client omits the
			// new field from an otherwise valid settings update.
			old := settings.get()
			if !inferenceAPISupplied {
				req.InferenceAPI = normalizeInferenceAPI(old.InferenceAPI)
			}
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
			settings.s.InferenceAPI = req.InferenceAPI
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
			if req.RerankMode != "" {
				settings.s.RerankMode = normalizeRerankMode(req.RerankMode)
			}
			if req.RetrievalMode != "" {
				settings.s.RetrievalMode = normalizeRetrievalMode(req.RetrievalMode)
			}
			if req.TinySQLAuditEnabled != nil {
				settings.s.TinySQLAuditEnabled = *req.TinySQLAuditEnabled
			}
			if req.TinySQLAuditPath != "" {
				settings.s.TinySQLAuditPath = strings.TrimSpace(req.TinySQLAuditPath)
			}
			if req.StorageEncryptionEnabled != nil {
				settings.s.StorageEncryptionEnabled = *req.StorageEncryptionEnabled
			}
			if req.GeoImportEnabled != nil {
				settings.s.GeoImportEnabled = *req.GeoImportEnabled
			}
			if req.TinySQLVectorCacheEntries != nil {
				settings.s.TinySQLVectorCacheEntries = max(0, min(*req.TinySQLVectorCacheEntries, 4096))
			}
			if req.TinySQLVectorCacheTTLSeconds != nil {
				settings.s.TinySQLVectorCacheTTLSeconds = max(0, min(*req.TinySQLVectorCacheTTLSeconds, 3600))
			}
			if req.TinySQLVectorAnalytics != nil {
				settings.s.TinySQLVectorAnalytics = *req.TinySQLVectorAnalytics
			}
			if req.AgentPlanner != nil {
				settings.s.AgentPlannerEnabled = *req.AgentPlanner
			}
			if req.AgentMaxSteps != nil {
				n := *req.AgentMaxSteps
				if n < 1 {
					n = 1
				}
				if n > 5 {
					n = 5
				}
				settings.s.AgentMaxPlanSteps = n
			}
			_ = settings.saveLocked()
			settings.mu.Unlock()

			// Apply runtime LM clients (may be composite)
			// Prefer persisted OpenAI key from settings; fallback to env var if none present
			applied := settings.get()
			configureTinySQLVectorCache(applied)
			key := applied.OpenAIKey
			if key == "" {
				key = os.Getenv("OPENAI_API_KEY")
			}
			chatLM := newLMClientWithAPI(applied.ChatBase, applied.EmbedModel, applied.ChatModel, key, applied.InferenceAPI)
			embedLM := newLMClientWithAPI(applied.EmbedBase, applied.EmbedModel, applied.ChatModel, key, applied.InferenceAPI)
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

	// Persistent agent memory is deliberately separate from chat history. Only
	// explicit UI/API actions can add or remove entries; model output and tool
	// results never mutate it.
	mux.HandleFunc("/api/memory", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			enabled, entries := settings.agentMemory()
			writeJSON(w, map[string]any{"enabled": enabled, "items": entries, "limit": maxAgentMemoryEntries})
		case http.MethodPost:
			var req struct {
				Content string `json:"content"`
				Enabled *bool  `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			if req.Enabled != nil {
				if err := settings.setAgentMemoryEnabled(*req.Enabled); err != nil {
					writeError(w, http.StatusInternalServerError, "could not save memory setting")
					return
				}
			}
			var created *agentMemoryEntry
			if strings.TrimSpace(req.Content) != "" {
				entry, err := settings.addAgentMemory(req.Content)
				if err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
				created = &entry
			}
			enabled, entries := settings.agentMemory()
			writeJSON(w, map[string]any{"ok": true, "enabled": enabled, "items": entries, "created": created, "limit": maxAgentMemoryEntries})
		default:
			writeError(w, http.StatusMethodNotAllowed, "GET or POST only")
		}
	}))
	mux.HandleFunc("/api/memory/delete", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		removed, err := settings.removeAgentMemory(req.ID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !removed {
			writeError(w, http.StatusNotFound, "memory entry not found")
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))

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
		req.Theme = strings.ToLower(strings.TrimSpace(req.Theme))
		settings.mu.Lock()
		valid := isBuiltinTheme(req.Theme)
		if !valid {
			for _, t := range settings.s.CustomThemes {
				if t.ID == req.Theme {
					valid = true
					break
				}
			}
		}
		if !valid {
			settings.mu.Unlock()
			writeError(w, 400, "unknown theme id")
			return
		}
		settings.s.Theme = req.Theme
		_ = settings.saveLocked()
		settings.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "theme": req.Theme})
	})

	// POST /api/settings/density — persist the UI layout density (comfortable|compact)
	mux.HandleFunc("/api/settings/density", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeError(w, 405, "POST only")
			return
		}
		var req struct {
			Density string `json:"density"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		settings.mu.Lock()
		settings.s.Density = normalizeDensity(req.Density)
		_ = settings.saveLocked()
		density := settings.s.Density
		settings.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "density": density})
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
		writeJSON(w, modules.list())
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
		writeJSON(w, mod)
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
		writeJSON(w, res)
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
		writeJSON(w, resp)
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
			tmp := newLMClientWithAPI(base, "x", "x", key, inferenceAPIAuto)
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
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "base_url": cur.BaseURL, "inference_api": cur.InferenceAPI, "message": msg})
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
		tmp := newLMClientWithAPI(req.BaseURL, "x", "x", key, req.InferenceAPI)
		models, err := tmp.listModels(req.BaseURL)
		resp := llmCheckResp{BaseURL: req.BaseURL, APIStyle: tmp.inferenceAPI(), ProviderHint: providerHintFromURL(req.BaseURL)}
		if err != nil {
			resp.OK = false
			resp.Error = err.Error()
		} else {
			resp.OK = true
			resp.Models = models
			resp.RecommendChat, resp.RecommendEmbed = recommendModels(models)
		}
		writeJSON(w, resp)
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
			Question    string `json:"question"`
			ChatID      string `json:"chat_id"`
			Debug       bool   `json:"debug"`
			Deep        bool   `json:"deep"`
			Offline     bool   `json:"offline"`
			AutoSearch  bool   `json:"auto_search"`
			Agent       bool   `json:"agent"` // optional: force the agent planner for this request
			PersonaID   string `json:"persona_id"`
			ImageBase64 string `json:"image_base64"` // optional: base64-encoded image for vision models
			ImageType   string `json:"image_type"`   // optional: MIME type, e.g. "image/jpeg"
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
		priorMessages := chats.historyBeforeLast(conv.ID, contextualRewriteHistoryMessages)
		retrievalQuestion := req.Question
		queryRewritten := false

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
		if !req.Offline {
			retrievalQuestion, queryRewritten = rewriteRetrievalQuery(r.Context(), rag.getLM(), req.Question, priorMessages)
		}

		// Prepare context: support Deep-Research mode with larger K
		var ctxText string
		var di *debugInfo
		var err error

		if req.Deep {
			log.Printf("REQ %s: DEEP: k=%d (base=%d, total_chunks=%d)", reqID, usedK, rag.k, totalChunks)
			ctxText, di, err = rag.prepareContextWithKContext(r.Context(), retrievalQuestion, req.Debug, usedK)
		} else {
			ctxText, di, err = rag.prepareContextWithKContext(r.Context(), retrievalQuestion, req.Debug, rag.k)
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
			RetrievalQuery:     retrievalQuestion,
			QueryRewritten:     queryRewritten,
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

		// Normal mode: call LM with SSE streaming via StreamingEngine
		engine := newStreamingEngine(rag.getLM(), rag, settings, customAPIs, modules, connectors, connectorExec)
		// Only capabilities that pass the same autonomous-execution policy as
		// the engine are advertised to the model. Manual and mutating actions
		// remain available through their explicit workflows, not via free-form
		// model output.
		allTools := engine.autonomousToolDefs(s, req.AutoSearch)
		// build system prompt; in deep mode add research instructions
		var systemPrompt string
		systemPrompt = buildToolSystemPrompt(ctxText, allTools, req.Deep, s)
		if personaPrompt != "" {
			systemPrompt = personaPrompt + "\n\n" + systemPrompt
		}

		// Validate system prompt isn't absurdly long
		if len(systemPrompt) > 32000 {
			log.Printf("REQ %s: WARN system prompt too long (%d chars), compacting context", reqID, len(systemPrompt))
			if len(ctxText) > 5000 {
				ctxText = compactAssembledContext(ctxText, 5000)
				if di != nil {
					di.ContextTruncated = true
				}
				systemPrompt = buildToolSystemPrompt(ctxText, allTools, req.Deep, s)
				if personaPrompt != "" {
					systemPrompt = personaPrompt + "\n\n" + systemPrompt
				}
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

		// Build the current user message – multimodal when an image is attached.
		lastMsg := chatMsg{Role: "user", Content: req.Question}
		if req.ImageBase64 != "" {
			mimeType := strings.TrimSpace(req.ImageType)
			if !strings.HasPrefix(mimeType, "image/") {
				mimeType = "image/jpeg"
			}
			dataURI := "data:" + mimeType + ";base64," + req.ImageBase64
			lastMsg = chatMsg{
				Role: "user",
				ContentParts: []contentPart{
					{Type: "text", Text: req.Question},
					{Type: "image_url", ImageURL: &imageURLContent{URL: dataURI, Detail: "auto"}},
				},
			}
			log.Printf("REQ %s: vision message attached (mime=%s, b64_len=%d)", reqID, mimeType, len(req.ImageBase64))
		}
		msgs = append(msgs, lastMsg)

		debugBase.HistoryMessages = len(msgs)

		// Apply routing heuristic for telemetry / future use
		nq := normalizeQuery(req.Question)
		route := routeQuery(nq, len(ctxText) > 0)
		debugBase.Mode = string(route.Mode)

		// Initialize telemetry
		tel := newRequestTelemetry(reqID, conv.ID, req.Question)
		tel.NormalizedQuery = nq.Lowercase
		tel.QuestionLen = len(req.Question)
		tel.SelectedMode = string(route.Mode)
		tel.RouteReason = route.Reason
		tel.RouteHints = route.Hints
		tel.ContextChars = len(ctxText)

		if req.Debug {
			// Add routing info to debug payload
			dbgJSON, _ := json.Marshal(debugBase)
			fmt.Fprintf(w, "event: debug\ndata: %s\n\n", dbgJSON)
			flusher.Flush()
			// Emit route decision as a separate debug event
			routeJSON, _ := json.Marshal(map[string]any{
				"mode": route.Mode, "reason": route.Reason, "hints": route.Hints,
			})
			fmt.Fprintf(w, "event: route\ndata: %s\n\n", routeJSON)
			flusher.Flush()
		}
		if di != nil && len(di.Citations) > 0 {
			citJSON, _ := json.Marshal(di.Citations)
			fmt.Fprintf(w, "event: citation_cards\ndata: %s\n\n", citJSON)
			flusher.Flush()
		}

		// Run the streaming engine
		sw := &sseWriter{w: w, flusher: flusher}
		engReq := EngineRequest{
			RequestID:    reqID,
			Question:     req.Question,
			SystemPrompt: systemPrompt,
			Messages:     msgs,
			AutoSearch:   req.AutoSearch,
			Debug:        req.Debug,
			PlanFirst:    req.Agent || s.AgentPlannerEnabled,
		}
		answerStr, engineErr := engine.Run(r.Context(), engReq, sw, tel)
		if di != nil && len(di.Citations) > 0 {
			answerStr = ensureCitedAnswer(answerStr, di.Citations)
			if !validateCitationsAgainstSources(answerStr, di.Citations) {
				answerStr = ensureCitedAnswer("Antwort aufgrund strenger Quellenrichtlinie gekürzt. Bitte verifiziere die Quellenbasis.", di.Citations)
			}
		}
		tel.VisibleChars = len(answerStr)
		if engineErr != nil {
			tel.finalize(false, engineErr.Error())
		} else {
			tel.finalize(true, "")
		}
		usageStats.recordFromTelemetry(tel, s.ActiveRole)

		log.Printf("REQ %s: Chat response complete: %d chars, continuations=%d, xml_blocks=%d",
			reqID, len(answerStr), tel.ContinuationCount, tel.XMLBlocksEmitted)

		s = settings.get()
		modelMeta := map[string]string{"base_url": s.BaseURL, "chat_model": s.ChatModel}
		chats.addMessageWithMeta(conv.ID, "assistant", answerStr, "", s.ChatModel, modelMeta)
	}))

	// POST /api/feedback — record a thumbs-up/down for the cited documents of
	// one answer. The handler stores identifiers only and does not alter live
	// retrieval ranking.
	mux.HandleFunc("/api/feedback", adminGuard(feedbackHandler(rag, settings)))
	// GET /api/debug/feedback — aggregate collected feedback for administrators.
	mux.HandleFunc("/api/debug/feedback", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		summaries, err := rag.feedbackSummaries(100)
		if err != nil {
			http.Error(w, "could not load feedback", http.StatusInternalServerError)
			return
		}
		writeJSON(w, summaries)
	}))

	// GET /api/tools — list available tools
	mux.HandleFunc("/api/tools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s := settings.get()
		allTools := append(customAPIs.allTools(), modules.enabledTools()...)
		if connectors != nil {
			allTools = append(allTools, connectors.enabledToolDefs()...)
		}
		json.NewEncoder(w).Encode(filterToolsForRole(allTools, s.ActiveRole))
	})

	// POST /api/tool/execute — execute a tool and add results to RAG
	mux.HandleFunc("/api/tool/execute", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req toolRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Tool) == "" || (strings.TrimSpace(req.Query) == "" && len(req.Arguments) == 0) {
			http.Error(w, "missing tool input", 400)
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
		text, source, fetchErr = executeToolRequestWithContext(r.Context(), req, s, rag, customAPIs, modules, connectors, connectorExec)

		if fetchErr != nil {
			http.Error(w, fmt.Sprintf("Tool %q fehlgeschlagen: %v", req.Tool, fetchErr), 500)
			return
		}

		pclass := (ToolPersistencePolicy{}).Classify(req.Tool, source)
		chunks, redactions := chunksForIngestWithDoc(text, s, stableContentHash(source), false)
		persisted := false
		if pclass == ToolPersistableAfterPolicy {
			if err := rag.addChunks(source, chunks, settings.get().EmbedModel); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			persisted = true
		}
		rag.logR3Audit(AuditEvent{
			EventType:   "manual_tool_execute",
			Actor:       s.ActiveRole,
			EntityType:  "tool",
			EntityID:    req.Tool,
			Decision:    map[bool]string{true: "allow", false: "deny"}[persisted],
			PolicyClass: string(pclass),
			Details:     source,
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tool":       req.Tool,
			"query":      req.Query,
			"arguments":  req.Arguments,
			"source":     source,
			"chars":      len(text),
			"chunks":     len(chunks),
			"total":      rag.docCountForRole(s.ActiveRole),
			"redactions": redactions,
			"persisted":  persisted,
			"policy":     pclass,
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
		writeJSON(w, results)
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
			Path       string           `json:"path"`
			Recursive  bool             `json:"recursive"`
			EmbedModel string           `json:"embed_model"`
			Roles      []string         `json:"roles"`
			Metadata   R3IngestMetadata `json:"metadata"`
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
		req.Metadata.UpdateMode = firstNonEmpty(req.Metadata.UpdateMode, "upsert")
		scan := scanDirectoryIntoRAG(rag, ragFolderScanRequest{
			Path:       req.Path,
			Recursive:  req.Recursive,
			EmbedModel: em,
			Roles:      roleScopes,
			Metadata:   req.Metadata,
		}, "")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files":         scan.FilesChanged,
			"files_seen":    scan.FilesSeen,
			"files_skipped": scan.FilesSkipped,
			"files_errored": scan.FilesErrored,
			"total_chars":   scan.TotalChars,
			"total_chunks":  scan.TotalChunks,
			"total":         rag.docCountForRole(s.ActiveRole),
			"errors":        scan.Errors,
			"results":       scan.Results,
			"roles":         roleScopes,
		})
	}))

	// POST /api/ingest/push — push one or more documents with optional R3 metadata.
	mux.HandleFunc("/api/ingest/push", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s := settings.get()
		if !permissionsForRole(s.ActiveRole).CanBulkIngest {
			http.Error(w, fmt.Sprintf("role %s is not allowed to run bulk ingest", demoRoleLabel(s.ActiveRole)), http.StatusForbidden)
			return
		}
		var req ragPushIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		docs := req.Documents
		if len(docs) == 0 {
			docs = []ragIngestDocument{{
				Source:     req.Source,
				Title:      req.Title,
				Text:       req.Text,
				EmbedModel: req.EmbedModel,
				Roles:      req.Roles,
				Metadata:   req.Metadata,
			}}
		}
		fallbackEmbed := firstNonEmpty(req.EmbedModel, s.EmbedModel)
		fallbackRoles := normalizeRoleScopes(req.Roles, s.ActiveRole)
		results := make([]ragIngestDocumentResult, 0, len(docs))
		var changed, skipped, failed, chunks int
		for _, doc := range docs {
			doc.Metadata = mergeR3Metadata(req.Metadata, doc.Metadata)
			if doc.Text == "" {
				res := ragIngestDocumentResult{Source: doc.Source, Title: doc.Title, Status: "error", Error: "missing text"}
				results = append(results, res)
				failed++
				continue
			}
			res := ingestDocument(rag, doc, fallbackEmbed, fallbackRoles, s)
			results = append(results, res)
			chunks += res.Chunks
			switch res.Status {
			case "inserted", "updated":
				changed++
			case "error":
				failed++
			default:
				skipped++
			}
		}
		rag.logR3Audit(AuditEvent{
			EventType:   "ingest_push",
			Actor:       s.ActiveRole,
			EntityType:  "ingest_batch",
			EntityID:    stableContentHash(fmt.Sprint(time.Now().UnixNano()))[:16],
			Decision:    "allow",
			PolicyClass: "ingestion",
			Details:     fmt.Sprintf("docs=%d changed=%d skipped=%d failed=%d meta=%s", len(results), changed, skipped, failed, encodeR3MetadataForAudit(req.Metadata)),
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"documents": results,
			"changed":   changed,
			"skipped":   skipped,
			"failed":    failed,
			"chunks":    chunks,
			"total":     rag.docCountForRole(s.ActiveRole),
		})
	}))

	// POST /api/ingest/pull-folder — run a pull-style directory scan once.
	mux.HandleFunc("/api/ingest/pull-folder", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s := settings.get()
		if !permissionsForRole(s.ActiveRole).CanBulkIngest {
			http.Error(w, fmt.Sprintf("role %s is not allowed to run bulk ingest", demoRoleLabel(s.ActiveRole)), http.StatusForbidden)
			return
		}
		var req ragFolderScanRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
			http.Error(w, "missing path", http.StatusBadRequest)
			return
		}
		info, err := os.Stat(req.Path)
		if err != nil {
			http.Error(w, "path not found: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !info.IsDir() {
			http.Error(w, "path is not a directory", http.StatusBadRequest)
			return
		}
		if req.EmbedModel == "" {
			req.EmbedModel = s.EmbedModel
		}
		req.Roles = normalizeRoleScopes(req.Roles, s.ActiveRole)
		req.Metadata.UpdateMode = firstNonEmpty(req.Metadata.UpdateMode, "upsert")
		res := scanDirectoryIntoRAG(rag, req, "")
		rag.logR3Audit(AuditEvent{
			EventType:   "ingest_pull_folder",
			Actor:       s.ActiveRole,
			EntityType:  "folder",
			EntityID:    req.Path,
			Decision:    "allow",
			PolicyClass: "ingestion",
			Details:     fmt.Sprintf("seen=%d changed=%d skipped=%d errors=%d meta=%s", res.FilesSeen, res.FilesChanged, res.FilesSkipped, res.FilesErrored, encodeR3MetadataForAudit(req.Metadata)),
		})
		writeJSON(w, res)
	}))

	// GET/POST /api/pull-sources — list or upsert configured pull sources.
	mux.HandleFunc("/api/pull-sources", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sources := settings.get().PullSources
			if sources == nil {
				sources = []ragPullSource{}
			}
			writeJSON(w, sources)
		case http.MethodPost:
			var src ragPullSource
			if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			src = normalizePullSource(src)
			if src.Kind != "folder" {
				http.Error(w, "only folder pull sources are supported", http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(src.Path) == "" {
				http.Error(w, "missing path", http.StatusBadRequest)
				return
			}
			settings.mu.Lock()
			replaced := false
			for i := range settings.s.PullSources {
				if settings.s.PullSources[i].ID == src.ID {
					settings.s.PullSources[i] = src
					replaced = true
					break
				}
			}
			if !replaced {
				settings.s.PullSources = append(settings.s.PullSources, src)
			}
			err := settings.saveLocked()
			settings.mu.Unlock()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, src)
		default:
			http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/pull-sources/delete", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		settings.mu.Lock()
		next := settings.s.PullSources[:0]
		for _, src := range settings.s.PullSources {
			if src.ID != req.ID {
				next = append(next, src)
			}
		}
		settings.s.PullSources = next
		err := settings.saveLocked()
		settings.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/pull-sources/run", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s := settings.get()
		if !permissionsForRole(s.ActiveRole).CanBulkIngest {
			http.Error(w, fmt.Sprintf("role %s is not allowed to run bulk ingest", demoRoleLabel(s.ActiveRole)), http.StatusForbidden)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		var src ragPullSource
		found := false
		for _, candidate := range settings.get().PullSources {
			if candidate.ID == req.ID {
				src = normalizePullSource(candidate)
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "pull source not found", http.StatusNotFound)
			return
		}
		if src.Kind != "folder" {
			http.Error(w, "only folder pull sources are supported", http.StatusBadRequest)
			return
		}
		res := scanDirectoryIntoRAG(rag, ragFolderScanRequest{
			Path:       src.Path,
			Recursive:  src.Recursive,
			EmbedModel: firstNonEmpty(src.EmbedModel, s.EmbedModel),
			Roles:      normalizeRoleScopes(src.Roles, s.ActiveRole),
			Metadata:   src.Metadata,
		}, src.ID)
		writeJSON(w, res)
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

		// Merge plain-text and binary document extensions into one allowed set.
		allowedExts := make(map[string]bool)
		for k, v := range allowedTextExtensions() {
			allowedExts[k] = v
		}
		for k, v := range allowedBinaryExtensions() {
			allowedExts[k] = v
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
					text, err := extractTextFromFile(content, f.Name)
					if err != nil {
						errorsList = append(errorsList, f.Name+": "+err.Error())
						continue
					}
					src := "upload:" + filename + ":" + f.Name
					chunks, _ := chunksForIngest(text, s)
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
					text, err := extractTextFromFile(content, hdr.Name)
					if err != nil {
						errorsList = append(errorsList, hdr.Name+": "+err.Error())
						continue
					}
					src := "upload:" + filename + ":" + hdr.Name
					chunks, _ := chunksForIngest(text, s)
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
		title := filepath.Base(header.Filename)
		fileExt := strings.ToLower(filepath.Ext(filename))

		// Image files: describe via vision model and ingest the description.
		imageMIME := map[string]string{
			".jpg": "image/jpeg", ".jpeg": "image/jpeg",
			".png": "image/png", ".gif": "image/gif",
			".webp": "image/webp",
		}
		if mimeType, ok := imageMIME[fileExt]; ok {
			if !isVisionModel(s.ChatModel) {
				http.Error(w, fmt.Sprintf(
					"image upload requires a vision-capable chat model; %q does not appear to support images. "+
						"Switch to a vision model (e.g. gpt-4o, llava, claude-3-opus) in settings.",
					s.ChatModel,
				), 400)
				return
			}
			desc, err := describeImageWithVision(r.Context(), rag.getLM(), data, mimeType, title)
			if err != nil {
				http.Error(w, "vision description failed: "+err.Error(), 500)
				return
			}
			ingestText := fmt.Sprintf("Image: %s\n\n%s", title, desc)
			chunks, _ := chunksForIngest(ingestText, s)
			if err := rag.addChunksWithRoles(title+" (image)", chunks, em, roleScopes); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"file":   title,
				"chars":  len(ingestText),
				"chunks": len(chunks),
				"total":  rag.docCountForRole(s.ActiveRole),
				"image":  true,
				"roles":  roleScopes,
			})
			return
		}

		text, err := extractTextFromFile(data, filename)
		if err != nil {
			http.Error(w, "could not extract text: "+err.Error(), 400)
			return
		}
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

	// POST /api/stt — Speech-to-Text proxy (Whisper)
	// Accepts a multipart/form-data request with a "file" audio field.
	// Forwards to the OpenAI /v1/audio/transcriptions endpoint and returns
	// {"text": "<transcription>"} as JSON.
	mux.HandleFunc("/api/stt", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		s := settings.get()
		apiKey := s.OpenAIKey
		if apiKey == "" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		chatBase := s.ChatBase
		if chatBase == "" {
			chatBase = s.BaseURL
		}
		base := normalizeBaseURL(chatBase)

		r.ParseMultipartForm(25 << 20)
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing audio file: "+err.Error(), 400)
			return
		}
		defer file.Close()

		model := r.FormValue("model")
		if model == "" {
			model = "whisper-1"
		}

		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", header.Filename)
		if err != nil {
			http.Error(w, "internal: "+err.Error(), 500)
			return
		}
		if _, err := io.Copy(fw, file); err != nil {
			http.Error(w, "internal: "+err.Error(), 500)
			return
		}
		mw.WriteField("model", model)
		mw.WriteField("response_format", "json")
		mw.Close()

		req, err := http.NewRequestWithContext(r.Context(), "POST", base+"/v1/audio/transcriptions", &buf)
		if err != nil {
			http.Error(w, "internal: "+err.Error(), 500)
			return
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		hc := newHTTPClient(60 * time.Second)
		resp, err := hc.Do(req)
		if err != nil {
			http.Error(w, "STT request failed: "+err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			http.Error(w, fmt.Sprintf("STT error %d: %s", resp.StatusCode, string(raw)), 502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))

	// POST /api/tts — Text-to-Speech proxy
	// Accepts JSON {"text":"...", "voice":"alloy", "model":"tts-1"} and
	// streams back the audio produced by the configured TTS endpoint.
	mux.HandleFunc("/api/tts", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		s := settings.get()
		apiKey := s.OpenAIKey
		if apiKey == "" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		chatBase := s.ChatBase
		if chatBase == "" {
			chatBase = s.BaseURL
		}
		base := normalizeBaseURL(chatBase)

		var body struct {
			Text   string `json:"text"`
			Voice  string `json:"voice"`
			Model  string `json:"model"`
			Format string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
			http.Error(w, "missing text", 400)
			return
		}
		if body.Voice == "" {
			body.Voice = "alloy"
		}
		if body.Model == "" {
			body.Model = "tts-1"
		}
		if body.Format == "" {
			body.Format = "mp3"
		}
		payload, _ := json.Marshal(map[string]string{
			"model":           body.Model,
			"input":           body.Text,
			"voice":           body.Voice,
			"response_format": body.Format,
		})
		req, err := http.NewRequestWithContext(r.Context(), "POST", base+"/v1/audio/speech", bytes.NewReader(payload))
		if err != nil {
			http.Error(w, "internal: "+err.Error(), 500)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		hc := newHTTPClient(60 * time.Second)
		resp, err := hc.Do(req)
		if err != nil {
			http.Error(w, "TTS request failed: "+err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			raw, _ := io.ReadAll(resp.Body)
			http.Error(w, fmt.Sprintf("TTS error %d: %s", resp.StatusCode, string(raw)), 502)
			return
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "audio/mpeg"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		io.Copy(w, resp.Body)
	}))

	// POST /api/vision — Multimodal vision chat (SSE streaming)
	// Accepts JSON {"question":"...","image_base64":"...","mime_type":"image/jpeg",
	// "chat_id":"...","persona_id":""} and streams LLM tokens via SSE.
	mux.HandleFunc("/api/vision", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Question    string `json:"question"`
			ImageBase64 string `json:"image_base64"`
			MimeType    string `json:"mime_type"`
			ChatID      string `json:"chat_id"`
			Debug       bool   `json:"debug"`
			PersonaID   string `json:"persona_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if strings.TrimSpace(req.Question) == "" && strings.TrimSpace(req.ImageBase64) == "" {
			http.Error(w, "missing question or image", 400)
			return
		}
		if req.MimeType == "" {
			req.MimeType = "image/jpeg"
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
		}
		userText := req.Question
		if userText == "" {
			userText = "[image]"
		}
		chats.addMessage(conv.ID, "user", userText)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}

		reqID := newRequestID()
		personaPrompt := ""
		personaName := ""
		if personaID != "" {
			if per, ok := personas.get(personaID); ok {
				personaPrompt = per.Prompt
				personaName = per.Name
			}
		}

		// Optionally enrich the prompt with RAG context if a question was given.
		var ctxText string
		if strings.TrimSpace(req.Question) != "" {
			ctxText, _, _ = rag.prepareContext(req.Question, false)
		}
		systemPrompt := buildToolSystemPrompt(ctxText, nil, false, s)
		if personaPrompt != "" {
			systemPrompt = personaPrompt + "\n\n" + systemPrompt
		}

		textContent := req.Question
		if ctxText != "" {
			textContent = "Kontext:\n" + ctxText + "\n\nFrage: " + req.Question
		}

		var parts []visionContentPart
		if textContent != "" {
			parts = append(parts, visionContentPart{Type: "text", Text: textContent})
		}
		if req.ImageBase64 != "" {
			dataURI := "data:" + req.MimeType + ";base64," + req.ImageBase64
			parts = append(parts, visionContentPart{
				Type:     "image_url",
				ImageURL: &visionImageURL{URL: dataURI, Detail: "auto"},
			})
		}

		msgs := []visionMsg{{Role: "user", Content: parts}}

		meta, _ := json.Marshal(map[string]any{
			"chat_id":      conv.ID,
			"request_id":   reqID,
			"mode":         "vision",
			"persona_id":   personaID,
			"persona_name": personaName,
			"active_role":  s.ActiveRole,
			"models":       map[string]string{"chat_model": s.ChatModel},
		})
		fmt.Fprintf(w, "event: meta\ndata: %s\n\n", meta)
		flusher.Flush()

		log.Printf("VISION[%s] chat=%s q=%q", reqID, conv.ID, req.Question)

		pr, pw := io.Pipe()
		var thinkBuf bytes.Buffer
		go func() {
			err := rag.getLM().chatStreamVision(r.Context(), systemPrompt, msgs, pw, &thinkBuf)
			if err != nil {
				pw.CloseWithError(err)
			} else {
				pw.Close()
			}
		}()

		sc := bufio.NewScanner(pr)
		sc.Split(bufio.ScanRunes)
		var answer strings.Builder
		for sc.Scan() {
			tok := sc.Text()
			answer.WriteString(tok)
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(tok))
			flusher.Flush()
		}
		if scErr := sc.Err(); scErr != nil {
			log.Printf("VISION[%s] scanner error: %v", reqID, scErr)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()

		answerStr := stripInternalThinking(answer.String())
		thinkingStr := strings.TrimSpace(thinkBuf.String())
		if thinkingStr != "" {
			fmt.Fprintf(w, "event: reasoning\ndata: %s\n\n", mustJSON(thinkingStr))
			flusher.Flush()
		}
		modelMeta := map[string]string{"chat_model": s.ChatModel}
		chats.addMessageWithMeta(conv.ID, "assistant", answerStr, thinkingStr, s.ChatModel, modelMeta)
		log.Printf("VISION[%s] complete: %d chars", reqID, len(answerStr))
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

	// GET /api/import/jobs — latest import job telemetry (admin/session or API-key guarded)
	mux.HandleFunc("/api/import/jobs", adminGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		stmt, err := tinysql.ParseSQL("SELECT * FROM r3_import_jobs ORDER BY updated_at DESC")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rag.dbMu.Lock()
		rs, err := tinysql.Execute(context.Background(), rag.db, "default", stmt)
		rag.dbMu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var rows any = []any{}
		if rs != nil {
			rows = rs.Rows
		}
		writeJSON(w, rows)
	}))

	// GET /api/debug/storage-stats — tinySQL backend observability (admin only)
	mux.HandleFunc("/api/debug/storage-stats", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		bs := rag.db.BackendStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"mode":               storageModeLabel(bs.Mode),
			"tables_in_memory":   bs.TablesInMemory,
			"tables_on_disk":     bs.TablesOnDisk,
			"memory_used_bytes":  bs.MemoryUsedBytes,
			"memory_limit_bytes": bs.MemoryLimitBytes,
			"disk_used_bytes":    bs.DiskUsedBytes,
			"cache_hit_rate":     bs.CacheHitRate,
			"sync_count":         bs.SyncCount,
			"load_count":         bs.LoadCount,
			"eviction_count":     bs.EvictionCount,
		})
	}))

	// GET /api/debug/vector-cache — tinySQL v0.49.0 vector cache telemetry.
	mux.HandleFunc("/api/debug/vector-cache", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, tinysql.VectorCacheAnalytics())
	}))

	// GET /api/debug/database-snapshot — portable tinySQL snapshot for an
	// administrator-operated backup. SaveToWriter is atomic from the caller's
	// perspective and avoids relying on the configured storage backend's files.
	mux.HandleFunc("/api/debug/database-snapshot", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		var snapshot bytes.Buffer
		rag.dbMu.Lock()
		err := tinysql.SaveToWriter(rag.db, &snapshot)
		rag.dbMu.Unlock()
		if err != nil {
			http.Error(w, "create database snapshot: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="tinyrag.snapshot.gob"`)
		w.Header().Set("Content-Length", strconv.Itoa(snapshot.Len()))
		_, _ = w.Write(snapshot.Bytes())
	}))

	// POST /api/import/csv — bulk-import a CSV/TSV file as RAG chunks (admin only).
	// Accepts multipart/form-data with field "file" (the CSV) and optional "source"
	// (article name used as the RAG source label; defaults to the filename).
	mux.HandleFunc("/api/import/csv", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "form parse error: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, fh, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing 'file' field: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		source := strings.TrimSpace(r.FormValue("source"))
		if source == "" {
			source = strings.TrimSuffix(fh.Filename, ".csv")
			source = strings.TrimSuffix(source, ".tsv")
		}
		job := ImportJob{
			JobID:         newRequestID(),
			SourceSystem:  "csv_import",
			Cursor:        source,
			Status:        ImportJobRunning,
			Processed:     0,
			Imported:      0,
			Skipped:       0,
			StartedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
			IdempotencyID: stableContentHash("csv|" + source),
		}
		rag.upsertImportJob(job)
		s := settings.get()
		result, chunks, impErr := importDelimitedAsChunks(r.Context(), file, source, s)
		if impErr != nil {
			job.Status = ImportJobFailed
			job.LastError = impErr.Error()
			job.UpdatedAt = time.Now().UTC()
			rag.upsertImportJob(job)
			http.Error(w, impErr.Error(), http.StatusInternalServerError)
			return
		}
		if err := rag.addChunks(source, chunks, s.EmbedModel); err != nil {
			job.Status = ImportJobFailed
			job.LastError = err.Error()
			job.UpdatedAt = time.Now().UTC()
			rag.upsertImportJob(job)
			http.Error(w, "ingest error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		doneAt := time.Now().UTC()
		job.Status = ImportJobCompleted
		job.Processed = int(result.RowsInserted)
		job.Imported = len(chunks)
		job.LastHash = stableContentHash(strings.Join(chunks, "\n"))
		job.UpdatedAt = doneAt
		job.CompletedAt = &doneAt
		rag.upsertImportJob(job)
		rag.logR3Audit(AuditEvent{
			EventType:   "import_csv",
			Actor:       s.ActiveRole,
			EntityType:  "import_job",
			EntityID:    job.JobID,
			Decision:    "allow",
			PolicyClass: "ingestion",
			Details:     source,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"job_id":        job.JobID,
			"source":        source,
			"rows_imported": result.RowsInserted,
			"chunks":        len(chunks),
			"columns":       result.ColumnNames,
		})
	}))

	// POST /api/import/json — bulk-import a JSON array file as RAG chunks (admin only).
	// Accepts multipart/form-data with field "file" (JSON array of objects) and
	// optional "source" (article name; defaults to filename).
	mux.HandleFunc("/api/import/json", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "form parse error: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, fh, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing 'file' field: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		source := strings.TrimSpace(r.FormValue("source"))
		if source == "" {
			source = strings.TrimSuffix(fh.Filename, ".json")
		}
		job := ImportJob{
			JobID:         newRequestID(),
			SourceSystem:  "json_import",
			Cursor:        source,
			Status:        ImportJobRunning,
			Processed:     0,
			Imported:      0,
			Skipped:       0,
			StartedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
			IdempotencyID: stableContentHash("json|" + source),
		}
		rag.upsertImportJob(job)
		s := settings.get()
		result, chunks, impErr := importJSONAsChunks(r.Context(), file, source, s)
		if impErr != nil {
			job.Status = ImportJobFailed
			job.LastError = impErr.Error()
			job.UpdatedAt = time.Now().UTC()
			rag.upsertImportJob(job)
			http.Error(w, impErr.Error(), http.StatusInternalServerError)
			return
		}
		if err := rag.addChunks(source, chunks, s.EmbedModel); err != nil {
			job.Status = ImportJobFailed
			job.LastError = err.Error()
			job.UpdatedAt = time.Now().UTC()
			rag.upsertImportJob(job)
			http.Error(w, "ingest error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		doneAt := time.Now().UTC()
		job.Status = ImportJobCompleted
		job.Processed = int(result.RowsInserted)
		job.Imported = len(chunks)
		job.LastHash = stableContentHash(strings.Join(chunks, "\n"))
		job.UpdatedAt = doneAt
		job.CompletedAt = &doneAt
		rag.upsertImportJob(job)
		rag.logR3Audit(AuditEvent{
			EventType:   "import_json",
			Actor:       s.ActiveRole,
			EntityType:  "import_job",
			EntityID:    job.JobID,
			Decision:    "allow",
			PolicyClass: "ingestion",
			Details:     source,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"job_id":        job.JobID,
			"source":        source,
			"rows_imported": result.RowsInserted,
			"chunks":        len(chunks),
			"columns":       result.ColumnNames,
		})
	}))

	// POST /api/import/geo — import GeoJSON, KML, or OSM XML as RAG chunks.
	mux.HandleFunc("/api/import/geo", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !settings.get().GeoImportEnabled {
			http.Error(w, "geo import is disabled by the administrator", http.StatusForbidden)
			return
		}
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "form parse error: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, fh, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing 'file' field: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		format := strings.ToLower(strings.TrimSpace(r.FormValue("format")))
		if format == "" {
			name := strings.ToLower(fh.Filename)
			switch {
			case strings.HasSuffix(name, ".geojson") || strings.HasSuffix(name, ".json"):
				format = "geojson"
			case strings.HasSuffix(name, ".kml"):
				format = "kml"
			case strings.HasSuffix(name, ".osm") || strings.HasSuffix(name, ".xml"):
				format = "osm"
			}
		}
		source := strings.TrimSpace(r.FormValue("source"))
		if source == "" {
			source = fh.Filename
		}
		s := settings.get()
		result, chunks, err := importGeoAsChunks(r.Context(), file, format, source, s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		meta := R3IngestMetadata{DocumentID: stableContentHash("geo|" + source), SourceSystem: "geo_import", SourceType: "geodata", SourceTitle: source, UpdateMode: "upsert"}
		write, err := rag.addChunksWithMetadataResult(source, chunks, s.EmbedModel, []string{s.ActiveRole}, meta)
		if err != nil {
			http.Error(w, "ingest error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		rag.logR3Audit(AuditEvent{EventType: "import_geo", Actor: s.ActiveRole, EntityType: "source", EntityID: write.DocumentID, Decision: "allow", PolicyClass: "ingestion", Details: format + ":" + source})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"source": source, "format": format, "rows_imported": result.RowsInserted, "chunks": write.Chunks, "status": write.Status})
	}))

	// POST /api/import/ckan — import CKAN/Open Knowledge metadata as governed
	// dataset cards. This intentionally imports metadata and schema guidance,
	// not raw resource files, so admins can stage source quality before bulk data.
	mux.HandleFunc("/api/import/ckan", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req ckanImportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		portal, err := normalizeCKANPortalURL(req.PortalURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.PortalURL = portal
		cursor := strings.TrimSpace(req.PackageID)
		if cursor == "" {
			cursor = strings.TrimSpace(req.Query)
		}
		job := ImportJob{
			JobID:         newRequestID(),
			SourceSystem:  "ckan_import",
			Cursor:        portal + "|" + cursor,
			Status:        ImportJobRunning,
			StartedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
			IdempotencyID: stableContentHash("ckan|" + portal + "|" + cursor),
		}
		rag.upsertImportJob(job)

		packages, _, fetchErr := fetchCKANPackages(r.Context(), newHTTPClient(30*time.Second), req)
		if fetchErr != nil {
			job.Status = ImportJobFailed
			job.LastError = fetchErr.Error()
			job.UpdatedAt = time.Now().UTC()
			rag.upsertImportJob(job)
			http.Error(w, "ckan fetch error: "+fetchErr.Error(), http.StatusBadGateway)
			return
		}

		s := settings.get()
		embedModel := strings.TrimSpace(req.EmbedModel)
		if embedModel == "" {
			embedModel = s.EmbedModel
		}
		var importedChunks, redactions int
		importedDatasets := make([]map[string]any, 0, len(packages))
		for _, pkg := range packages {
			meta := ckanPackageR3Metadata(portal, pkg)
			card := buildCKANDatasetCard(portal, pkg)
			title := strings.TrimSpace(meta.SourceTitle)
			if title == "" {
				title = "CKAN dataset " + meta.SourceObjectID
			}
			chunks, reds := chunksForIngestWithDoc(card, s, meta.DocumentID, false)
			redactions += reds
			if err := rag.addChunksWithMetadata(title, chunks, embedModel, req.Roles, meta); err != nil {
				job.Status = ImportJobFailed
				job.LastError = err.Error()
				job.Processed = len(importedDatasets)
				job.Imported = importedChunks
				job.UpdatedAt = time.Now().UTC()
				rag.upsertImportJob(job)
				http.Error(w, "ingest error: "+err.Error(), http.StatusInternalServerError)
				return
			}
			importedChunks += len(chunks)
			importedDatasets = append(importedDatasets, map[string]any{
				"title":       title,
				"document_id": meta.DocumentID,
				"source_url":  meta.SourceURL,
				"chunks":      len(chunks),
			})
		}

		doneAt := time.Now().UTC()
		job.Status = ImportJobCompleted
		job.Processed = len(packages)
		job.Imported = importedChunks
		job.Skipped = 0
		job.LastHash = stableContentHash(fmt.Sprint(importedDatasets))
		job.UpdatedAt = doneAt
		job.CompletedAt = &doneAt
		rag.upsertImportJob(job)
		rag.logR3Audit(AuditEvent{
			EventType:   "import_ckan",
			Actor:       s.ActiveRole,
			EntityType:  "import_job",
			EntityID:    job.JobID,
			Decision:    "allow",
			PolicyClass: "ingestion",
			Details:     portal + "|" + cursor,
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"job_id":     job.JobID,
			"portal_url": portal,
			"datasets":   importedDatasets,
			"processed":  len(packages),
			"chunks":     importedChunks,
			"redactions": redactions,
		})
	}))

	// GET /api/sources
	mux.HandleFunc("/api/sources", func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		writeJSON(w, rag.listSourcesForRole(s.ActiveRole))
	})

	// POST /api/sources/delete
	mux.HandleFunc("/api/sources/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			DocumentID string `json:"document_id"`
			Article    string `json:"article"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.DocumentID == "" && req.Article == "") {
			http.Error(w, "missing document_id or article", 400)
			return
		}
		s := settings.get()
		if err := rag.deleteSourceForRole(req.DocumentID, req.Article, s.ActiveRole); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"deleted": firstNonEmpty(req.DocumentID, req.Article), "total": rag.docCountForRole(s.ActiveRole)})
	})

	// GET /api/chats — list conversations
	mux.HandleFunc("/api/chats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, chats.list())
	})

	// GET /api/ui — UI configuration, theme list and branding for the frontend
	mux.HandleFunc("/api/ui", func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		writeJSON(w, map[string]any{
			"config":        s.UI,
			"theme":         s.Theme,
			"themes":        builtinThemes,
			"custom_themes": s.CustomThemes,
			"templates":     scenarioTemplates(),
			"density":       s.Density,
			"densities":     builtinDensities,
			"app_name":      s.AppName,
			"app_logo_url":  s.AppLogoURL,
		})
	})

	// POST /api/ui/config — replace the UI configuration (admin)
	mux.HandleFunc("/api/ui/config", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeError(w, 405, "POST only")
			return
		}
		var cfg uiConfig
		if err := readJSON(r, &cfg); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		settings.mu.Lock()
		settings.s.UI = normalizeUIConfig(cfg)
		_ = settings.saveLocked()
		cfgOut := settings.s.UI
		settings.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "config": cfgOut})
	}))

	// POST /api/ui/themes — upsert or delete a custom theme (admin)
	mux.HandleFunc("/api/ui/themes", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeError(w, 405, "POST only")
			return
		}
		var req struct {
			Delete string     `json:"delete,omitempty"` // theme id to remove
			Theme  uiThemeDef `json:"theme,omitempty"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		settings.mu.Lock()
		defer settings.mu.Unlock()
		if req.Delete != "" {
			id := strings.ToLower(strings.TrimSpace(req.Delete))
			themes, found := removeCustomTheme(settings.s.CustomThemes, id)
			if !found {
				writeError(w, 404, "theme not found")
				return
			}
			settings.s.CustomThemes = themes
			if settings.s.Theme == id {
				settings.s.Theme = "dark"
			}
			_ = settings.saveLocked()
			writeJSON(w, map[string]any{"ok": true, "custom_themes": settings.s.CustomThemes})
			return
		}
		clean, err := sanitizeCustomTheme(req.Theme)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		exists := false
		for _, t := range settings.s.CustomThemes {
			if t.ID == clean.ID {
				exists = true
				break
			}
		}
		if !exists && len(settings.s.CustomThemes) >= 24 {
			writeError(w, 400, "too many custom themes (max 24)")
			return
		}
		settings.s.CustomThemes = upsertCustomTheme(settings.s.CustomThemes, clean)
		_ = settings.saveLocked()
		writeJSON(w, map[string]any{"ok": true, "custom_themes": settings.s.CustomThemes})
	}))

	// GET /api/ui/templates — list built-in deployment scenario templates
	mux.HandleFunc("/api/ui/templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, scenarioTemplates())
	})

	// POST /api/ui/templates/apply — apply a scenario template's theme + UI config (admin)
	mux.HandleFunc("/api/ui/templates/apply", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeError(w, 405, "POST only")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		tmpl, ok := findScenarioTemplate(strings.TrimSpace(req.ID))
		if !ok {
			writeError(w, 404, "unknown template id")
			return
		}
		settings.mu.Lock()
		themeValid := isBuiltinTheme(tmpl.Theme)
		if !themeValid {
			for _, t := range settings.s.CustomThemes {
				if t.ID == tmpl.Theme {
					themeValid = true
					break
				}
			}
		}
		if themeValid {
			settings.s.Theme = tmpl.Theme
		}
		if tmpl.Density != "" {
			settings.s.Density = normalizeDensity(tmpl.Density)
		}
		settings.s.UI = normalizeUIConfig(tmpl.Config)
		_ = settings.saveLocked()
		out := map[string]any{"ok": true, "theme": settings.s.Theme, "density": settings.s.Density, "config": settings.s.UI}
		settings.mu.Unlock()
		writeJSON(w, out)
	}))

	// GET /api/stats/usage?days=30 — aggregated usage statistics for the dashboard
	mux.HandleFunc("/api/stats/usage", func(w http.ResponseWriter, r *http.Request) {
		days := 30
		if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				days = n
			}
		}
		writeJSON(w, usageStats.summarize(days))
	})

	// GET /api/chats/export?id=<chat>&format=markdown|html — download a conversation
	mux.HandleFunc("/api/chats/export", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeError(w, 400, "missing chat id")
			return
		}
		conv := chats.get(id)
		if conv == nil {
			writeError(w, 404, "chat not found")
			return
		}
		appName := settings.get().AppName
		if appName == "" {
			appName = "tinyRAG"
		}
		format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
		switch format {
		case "html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", exportFilename(conv, "html")))
			_, _ = w.Write([]byte(conv.exportHTML(appName)))
		default:
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", exportFilename(conv, "md")))
			_, _ = w.Write([]byte(conv.exportMarkdown(appName)))
		}
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
		writeJSON(w, conv)
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
		writeJSON(w, conv)
	})

	// Custom APIs (persisted)
	mux.HandleFunc("/api/settings/apis", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeJSON(w, customAPIs.list())
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
			writeJSON(w, api)
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
			writeJSON(w, personas.list())
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
			writeJSON(w, p)
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
		writeJSON(w, adminUsers.list())
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
		writeJSON(w, user)
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
		writeJSON(w, apiRoutes.list())
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
		writeJSON(w, rule)
	}))

	// Connector system routes
	registerConnectorRoutes(mux, connectors, connectorExec, adminGuard)

	// ── Web-UI Authentication endpoints ──────────────────────────────────────

	// GET /login — login page (HTML); redirects to / when auth is off or already logged in.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		if !s.WebUIAuth {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if sess := sessionFromRequest(r); sess != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		appName := s.AppName
		if appName == "" {
			appName = "tinyRAG"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, loginPageHTML, html.EscapeString(appName), html.EscapeString(appName))
	})

	// POST /api/auth/login — exchange username+password for a session cookie.
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password required", 400)
			return
		}
		s := settings.get()
		ttl := time.Duration(s.SessionTTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		// 1. Try local users
		for _, u := range s.WebUIUsers {
			if !u.Enabled || !strings.EqualFold(u.Username, req.Username) {
				continue
			}
			if verifyWebUIPassword(u, req.Password) {
				sess := sessions.create(u.ID, u.Username, u.Role, ttl)
				http.SetCookie(w, newSessionCookie(r, sess.Token, int(ttl.Seconds())))
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"ok":       true,
					"username": u.Username,
					"role":     u.Role,
				})
				return
			}
			// Username matched but password wrong — stop here (no LDAP fallback for known local user)
			http.Error(w, "invalid credentials", 401)
			return
		}
		// 2. Try LDAP if enabled
		if s.LDAPEnabled {
			role, err := ldapAuthenticate(s, req.Username, req.Password)
			if err == nil {
				sess := sessions.create("ldap:"+req.Username, req.Username, role, ttl)
				http.SetCookie(w, newSessionCookie(r, sess.Token, int(ttl.Seconds())))
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"ok":       true,
					"username": req.Username,
					"role":     role,
				})
				return
			}
			log.Printf("AUTH LDAP failed for %q: %v", req.Username, err)
		}
		http.Error(w, "invalid credentials", 401)
	})

	// POST /api/auth/logout — clear the session cookie.
	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session_token"); err == nil {
			sessions.delete(c.Value)
		}
		http.SetCookie(w, newSessionCookie(r, "", -1))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	// GET /api/auth/me — return current session info (or 401).
	mux.HandleFunc("/api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		if !s.WebUIAuth {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"authenticated": true, "web_ui_auth": false})
			return
		}
		sess := sessionFromRequest(r)
		if sess == nil {
			http.Error(w, "not authenticated", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"authenticated": true,
			"web_ui_auth":   true,
			"username":      sess.Username,
			"role":          sess.Role,
			"expires_at":    sess.ExpiresAt.UTC().Format(time.RFC3339),
		})
	})

	// ── Branding (admin-only) ────────────────────────────────────────────────

	// POST /api/admin/branding — save white-label settings.
	mux.HandleFunc("/api/admin/branding", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			AppName    string `json:"app_name"`
			AppLogoURL string `json:"app_logo_url"`
			CustomCSS  string `json:"custom_css"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		settings.mu.Lock()
		settings.s.AppName = strings.TrimSpace(req.AppName)
		settings.s.AppLogoURL = strings.TrimSpace(req.AppLogoURL)
		settings.s.CustomCSS = req.CustomCSS
		_ = settings.saveLocked()
		settings.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	// ── Web-UI user management (admin-only) ──────────────────────────────────

	// GET /api/admin/webusers — list web UI users (hashes omitted).
	mux.HandleFunc("/api/admin/webusers", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		s := settings.get()
		out := make([]map[string]any, 0, len(s.WebUIUsers))
		for _, u := range s.WebUIUsers {
			out = append(out, map[string]any{
				"id":         u.ID,
				"username":   u.Username,
				"role":       u.Role,
				"enabled":    u.Enabled,
				"created_at": u.CreatedAt,
			})
		}
		writeJSON(w, out)
	}))

	// POST /api/admin/webusers/create — create a new web UI user.
	mux.HandleFunc("/api/admin/webusers/create", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password required", 400)
			return
		}
		if len(req.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", 400)
			return
		}
		role := req.Role
		if role != "admin" && role != "viewer" {
			role = "viewer"
		}
		pwHash, err := hashWebUIPassword(req.Password)
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		newUser := webUIUser{
			ID:           fmt.Sprintf("wu-%d", time.Now().UnixNano()),
			Username:     req.Username,
			PasswordHash: pwHash,
			Role:         role,
			Enabled:      true,
			CreatedAt:    time.Now().Format(time.RFC3339),
		}
		settings.mu.Lock()
		// Reject duplicate usernames
		for _, u := range settings.s.WebUIUsers {
			if strings.EqualFold(u.Username, newUser.Username) {
				settings.mu.Unlock()
				http.Error(w, "username already exists", 409)
				return
			}
		}
		settings.s.WebUIUsers = append(settings.s.WebUIUsers, newUser)
		_ = settings.saveLocked()
		settings.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":       newUser.ID,
			"username": newUser.Username,
			"role":     newUser.Role,
		})
	}))

	// POST /api/admin/webusers/save — update role / enabled flag.
	mux.HandleFunc("/api/admin/webusers/save", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			http.Error(w, "id required", 400)
			return
		}
		settings.mu.Lock()
		defer settings.mu.Unlock()
		for i, u := range settings.s.WebUIUsers {
			if u.ID != req.ID {
				continue
			}
			if req.Role == "admin" || req.Role == "viewer" {
				settings.s.WebUIUsers[i].Role = req.Role
			}
			settings.s.WebUIUsers[i].Enabled = req.Enabled
			_ = settings.saveLocked()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		http.Error(w, "user not found", 404)
	}))

	// POST /api/admin/webusers/password — change a user's password.
	mux.HandleFunc("/api/admin/webusers/password", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID       string `json:"id"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			http.Error(w, "id required", 400)
			return
		}
		if len(req.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", 400)
			return
		}
		pwHash, err := hashWebUIPassword(req.Password)
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		settings.mu.Lock()
		defer settings.mu.Unlock()
		for i, u := range settings.s.WebUIUsers {
			if u.ID != req.ID {
				continue
			}
			settings.s.WebUIUsers[i].PasswordHash = pwHash
			_ = settings.saveLocked()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		http.Error(w, "user not found", 404)
	}))

	// POST /api/admin/webusers/delete — remove a web UI user.
	mux.HandleFunc("/api/admin/webusers/delete", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			http.Error(w, "id required", 400)
			return
		}
		settings.mu.Lock()
		defer settings.mu.Unlock()
		list := settings.s.WebUIUsers
		for i, u := range list {
			if u.ID != req.ID {
				continue
			}
			settings.s.WebUIUsers = append(list[:i], list[i+1:]...)
			_ = settings.saveLocked()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		http.Error(w, "user not found", 404)
	}))

	// POST /api/admin/auth — toggle WebUIAuth, update session TTL, and LDAP config.
	mux.HandleFunc("/api/admin/auth", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			WebUIAuth         *bool  `json:"web_ui_auth"`
			SessionTTLSeconds *int   `json:"session_ttl_seconds"`
			LDAPEnabled       *bool  `json:"ldap_enabled"`
			LDAPServer        string `json:"ldap_server"`
			LDAPPort          *int   `json:"ldap_port"`
			LDAPUseTLS        *bool  `json:"ldap_use_tls"`
			LDAPStartTLS      *bool  `json:"ldap_start_tls"`
			LDAPBaseDN        string `json:"ldap_base_dn"`
			LDAPBindDN        string `json:"ldap_bind_dn"`
			LDAPBindPass      string `json:"ldap_bind_pass"`
			LDAPUserAttr      string `json:"ldap_user_attr"`
			LDAPFilter        string `json:"ldap_filter"`
			LDAPAdminGroup    string `json:"ldap_admin_group"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		settings.mu.Lock()
		if req.WebUIAuth != nil {
			settings.s.WebUIAuth = *req.WebUIAuth
		}
		if req.SessionTTLSeconds != nil && *req.SessionTTLSeconds > 0 {
			settings.s.SessionTTLSeconds = *req.SessionTTLSeconds
		}
		if req.LDAPEnabled != nil {
			settings.s.LDAPEnabled = *req.LDAPEnabled
		}
		if req.LDAPServer != "" {
			settings.s.LDAPServer = strings.TrimSpace(req.LDAPServer)
		}
		if req.LDAPPort != nil {
			settings.s.LDAPPort = *req.LDAPPort
		}
		if req.LDAPUseTLS != nil {
			settings.s.LDAPUseTLS = *req.LDAPUseTLS
		}
		if req.LDAPStartTLS != nil {
			settings.s.LDAPStartTLS = *req.LDAPStartTLS
		}
		if req.LDAPBaseDN != "" {
			settings.s.LDAPBaseDN = req.LDAPBaseDN
		}
		if req.LDAPBindDN != "" {
			settings.s.LDAPBindDN = req.LDAPBindDN
		}
		if req.LDAPBindPass != "" {
			settings.s.LDAPBindPass = req.LDAPBindPass
		}
		if req.LDAPUserAttr != "" {
			settings.s.LDAPUserAttr = req.LDAPUserAttr
		}
		settings.s.LDAPFilter = req.LDAPFilter
		settings.s.LDAPAdminGroup = req.LDAPAdminGroup
		_ = settings.saveLocked()
		settings.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	// GET /api/admin/auth — return current auth + LDAP settings (sensitive fields masked).
	mux.HandleFunc("/api/admin/auth/get", requireAdminSession(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		s := settings.get()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"web_ui_auth":         s.WebUIAuth,
			"session_ttl_seconds": s.SessionTTLSeconds,
			"ldap_enabled":        s.LDAPEnabled,
			"ldap_server":         s.LDAPServer,
			"ldap_port":           s.LDAPPort,
			"ldap_use_tls":        s.LDAPUseTLS,
			"ldap_start_tls":      s.LDAPStartTLS,
			"ldap_base_dn":        s.LDAPBaseDN,
			"ldap_bind_dn":        s.LDAPBindDN,
			"ldap_bind_pass_set":  s.LDAPBindPass != "",
			"ldap_user_attr":      s.LDAPUserAttr,
			"ldap_filter":         s.LDAPFilter,
			"ldap_admin_group":    s.LDAPAdminGroup,
		})
	}))

	fmt.Printf("Web interface: http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
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
	usageStatsPath := flag.String("usage-stats", "usage_stats.jsonl", "Usage statistics JSONL path (empty=memory only, not persisted)")
	connectorsPath := flag.String("connectors", "connectors.json", "Connector registry JSON path")
	storageFlag := flag.String("storage-mode", "memory", "Storage mode: memory, wal, disk, index, hybrid")
	maxMemMB := flag.Int64("max-mem-mb", 256, "Max memory in MB for hybrid/index mode")

	// Defaults for first run (written to settings.json if it doesn't exist)
	urlFlag := flag.String("url", "http://localhost:1234", "Default OpenAI-compatible base URL (first run only)")
	inferenceAPIFlag := flag.String("inference-api", "", "Inference wire protocol: auto, openai or ollama (overrides settings when provided)")
	embedModel := flag.String("embed-model", "text-embedding-nomic-embed-text-v1.5", "Default embedding model (first run only)")
	chatModel := flag.String("chat-model", "mistralai/ministral-3-14b-reasoning", "Default chat model (first run only)")
	k := flag.Int("k", 5, "Top-K results (first run only)")
	lang := flag.String("lang", "de", "Wikipedia language (first run only)")
	chunkSize := flag.Int("chunk-size", 800, "Max characters per chunk (first run only)")

	// One-shot / CLI flags
	askFlag := flag.String("ask", "", "One-shot: answer this question and exit (overrides -web)")
	searchFlag := flag.String("searchq", "", "One-shot: run a semantic search and exit (overrides -web)")
	jsonOut := flag.Bool("jsonout", false, "One-shot: emit machine-readable JSON")
	noColor := flag.Bool("nocolor", false, "CLI: disable ANSI colors (NO_COLOR env is also honored)")
	listThemes := flag.Bool("list-themes", false, "One-shot: print available theme ids as JSON and exit")
	listTemplates := flag.Bool("list-templates", false, "One-shot: print built-in scenario templates as JSON and exit")
	applyTemplateFlag := flag.String("apply-template", "", "One-shot: apply a scenario template (theme+UI config) and exit — for provisioning")
	demoLLMModel := flag.String("demo-llm-model", "", "Demo mode: run a bundled pure-Go LLM (GopherLLM) in-process instead of an external LLM server. "+
		"Value is a path to a .gguf file, or \"auto\" to pick one from GopherLLM's default model directory. Requires building with -tags demo_llm.")
	demoLLMAddr := flag.String("demo-llm-addr", "127.0.0.1:8091", "Demo mode: local address the embedded GopherLLM server listens on")

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
	if strings.TrimSpace(*inferenceAPIFlag) != "" {
		settings.mu.Lock()
		settings.s.InferenceAPI = normalizeInferenceAPI(*inferenceAPIFlag)
		_ = settings.saveLocked()
		settings.mu.Unlock()
	}

	// Provisioning one-shots: pure settings/config operations that need
	// neither an LLM endpoint nor the RAG store, so they run before the LLM
	// probe below and exit immediately. Useful for deployment scripts, e.g.
	// a Docker entrypoint calling `tinyRAG -apply-template support-widget`.
	if *listThemes {
		os.Exit(runListThemes())
	}
	if *listTemplates {
		os.Exit(runListTemplates())
	}
	if *applyTemplateFlag != "" {
		os.Exit(runApplyTemplate(*applyTemplateFlag))
	}

	// Demo mode: run a bundled pure-Go LLM in-process (see demo_llm.go) so
	// tinyRAG works standalone, with no LM Studio/Ollama/llama.cpp install.
	// This overrides the configured chat/embed backend outright — skip the
	// usual reachability probing/auto-preference logic below.
	if *demoLLMModel != "" {
		modelName, err := startEmbeddedDemoLLM(*demoLLMModel, *demoLLMAddr)
		if err != nil {
			log.Fatalf("Failed to start embedded demo LLM: %v", err)
		}
		demoBase := "http://" + *demoLLMAddr
		settings.mu.Lock()
		settings.s.BaseURL = demoBase
		settings.s.InferenceAPI = inferenceAPIOpenAI
		settings.s.ChatBase = demoBase
		settings.s.EmbedBase = demoBase
		settings.s.ChatModel = modelName
		settings.s.EmbedModel = modelName
		_ = settings.saveLocked()
		settings.mu.Unlock()
		log.Printf("Demo LLM: tinyRAG is using the embedded model %q at %s — no external LLM server needed", modelName, *demoLLMAddr)
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

	chatLM := newLMClientWithAPI(chatBase, s.EmbedModel, s.ChatModel, openaiKey, s.InferenceAPI)
	embedLM := newLMClientWithAPI(embedBase, s.EmbedModel, s.ChatModel, openaiKey, s.InferenceAPI)

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
	closeTinySQLFeatures, err := configureTinySQLOptionalFeatures(rag, s, *dbPath)
	if err != nil {
		log.Fatalf("Failed to configure tinySQL features: %v", err)
	}
	defer closeTinySQLFeatures()
	if err := rag.init(); err != nil {
		log.Fatalf("Failed to init table: %v", err)
	}

	// Ensure database is flushed on exit
	defer func() {
		if err := rag.db.Close(); err != nil {
			log.Printf("Warning: failed to close database: %v", err)
		}
	}()

	// Flush the RAG store on Ctrl+C / SIGTERM instead of losing unsaved
	// in-memory data: Go's default signal handling terminates the process
	// immediately without running deferred functions, so without this the
	// GOB snapshot (memory/WAL modes) or final Sync (disk/hybrid/index
	// modes) would never happen.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-sigCtx.Done()
		log.Println("Shutting down, flushing data…")
		if err := rag.save(); err != nil {
			log.Printf("Warning: failed to save RAG store on shutdown: %v", err)
		}
		if err := rag.db.Close(); err != nil {
			log.Printf("Warning: failed to close database on shutdown: %v", err)
		}
		os.Exit(0)
	}()

	existing := rag.docCount()
	if existing > 0 {
		fmt.Printf("Database has %d existing chunks.\n", existing)
	}

	customAPIs := newAPIStore(settings)
	modules := newModuleStore(settings)
	personas := newPersonaStore(settings)
	chats := newChatStore(*chatsPath)
	usageStats = newUsageStore(*usageStatsPath)

	connectors, err := newConnectorStore(*connectorsPath)
	if err != nil {
		log.Fatalf("Failed to load connector store: %v", err)
	}
	connectorExec := newConnectorExecutor(connectors)

	// One-shot mode: answer/search once and exit (scripting-friendly).
	// Takes precedence over the web server so pipelines can call the binary
	// directly: tinyRAG -ask "…" -jsonout
	if *askFlag != "" || *searchFlag != "" {
		code := 0
		if *askFlag != "" {
			code = runOneShotAsk(rag, *askFlag, *jsonOut)
		} else {
			code = runOneShotSearch(rag, *searchFlag, *jsonOut)
		}
		if err := rag.db.Close(); err != nil {
			log.Printf("Warning: failed to close database: %v", err)
		}
		os.Exit(code)
	}

	if *web {
		pullCtx, stopPullScheduler := context.WithCancel(context.Background())
		defer stopPullScheduler()
		startPullScheduler(pullCtx, rag, settings)
		runWebServer(rag, *addr, settings, chats, customAPIs, personas, modules, connectors, connectorExec, llmAvailable, llmPingErr)
		return
	}

	// Interactive CLI/TUI mode
	runCLI(rag, personas, newCLIPalette(*noColor))
}
