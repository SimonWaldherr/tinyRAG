// R3 — Rubix Ranked RAG.
//
// A lightweight, provenance-first RAG system for mailbox knowledge bases.
// MVP scope: ingest a PST export and arbitrary office/text files, answer
// questions from a browser UI with cited sources, ranked by a blend of
// vector similarity, keyword overlap and recency (see rank.go). Modeled on
// github.com/SimonWaldherr/tinyRAG's architecture and conventions.
//
// Beyond the MVP: PST/file/SharePoint/Exchange-Graph/IMAP import, LDAP
// login, and HITL draft replies proposed for new mail (never auto-sent) —
// see README.md for the full connector list.
//
// Author: Simon Waldherr. Questions or problems — contact
// simon.waldherr@rubix.com.
package main

import (
	"bytes"
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// indexTemplates holds the page shell (web/templates/layout.html) plus one
// file per tab panel and modal — previously one 1000+ line web/index.html.
// Split for maintainability only; renderIndexHTML below composes them back
// into the exact same single string index.html used to be, so nothing
// downstream (handlers.go's root handler, main.go's other //go:embed
// assets) needs to change.
//
//go:embed web/templates/*.html
var indexTemplates embed.FS

// indexHTML is the fully-composed page, rendered once at startup (there's
// no per-request dynamic data — every template file is static markup that
// app.js populates client-side) rather than re-executed on every request.
var indexHTML = renderIndexHTML()

// renderIndexHTML parses every web/templates/*.html file (each a
// `{{define "name"}}...{{end}}` block — see that directory) and executes
// "layout", the root template that `{{template}}`s in the rest. Panics on
// failure: a broken template is a build-time error, not something to limp
// along with at runtime.
func renderIndexHTML() string {
	tmpl := template.Must(template.ParseFS(indexTemplates, "web/templates/*.html"))
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", nil); err != nil {
		panic("renderIndexHTML: " + err.Error())
	}
	return buf.String()
}

//go:embed web/style.css
var styleCSS string

//go:embed web/app.js
var appJS string

//go:embed web/i18n.js
var i18nJS string

// novapopJS is a vendored copy of https://github.com/SimonWaldherr/novapop
// (MIT), a small dependency-free popup/toast library — used for the
// admin-notification toasts (notifications.go, handleAuthStatus's
// admin_bootstrap_warning) since R3 otherwise has no client-side UI
// component for transient alerts.
//
//go:embed web/novapop.js
var novapopJS string

//go:embed docs/openapi.json
var openAPISpec string

//go:embed llms.txt
var llmsTxt string

//go:embed web/apidocs.html
var apiDocsHTML string

//go:embed web/apidocs.js
var apiDocsJS string

//go:embed web/openai-api.html
var openAIAPIHTML string

// verbose enables per-request and per-LLM-call logging: every HTTP request
// (method/path/duration, via verboseMiddleware in handlers.go) and every
// actual embed/chat call (provider/model/base, in llm.go's embed()/
// chatStream()) gets logged. Set via -verbose, which `make dev` passes by
// default; `make run` doesn't, so a background/production run stays quiet.
var verbose bool

// main wires up a single run of R3: load/create settings, build the LLM
// client(s) from them, open the configured vector store, optionally run a
// one-shot migration and exit, or else start the HTTP server.
func main() {
	addr := flag.String("addr", ":8090", "server address")
	verboseFlag := flag.Bool("verbose", false, "log every request and which LLM model handled it (wired to `make dev`)")
	storageBackend := flag.String("storage-backend", "tinysql", "vector store backend: tinysql|sqlite (first run only)")
	storagePath := flag.String("storage-path", "r3-data", "vector store path (file for memory/wal, directory for disk/index/hybrid, single .db file for sqlite) (first run only)")
	storageMode := flag.String("storage-mode", "disk", "tinySQL storage mode: memory|wal|disk|index|hybrid — ignored by the sqlite backend (first run only). disk is the default: it keeps R3's one hot chunks table resident (stable *Table pointer → warm HNSW/FTS caches) with incremental saves and no memory-budget cliff; hybrid/index bound residency to -storage-max-mem-mb but rebuild those caches per query once the table outgrows the budget (see vectorstore_tinysql.go)")
	storageMaxMemMB := flag.Int64("storage-max-mem-mb", 256, "max in-memory cache/index size for tinySQL's index/hybrid modes — ignored by the sqlite backend (first run only)")
	migrateFromBackend := flag.String("migrate-from-backend", "", "one-shot: copy every chunk (with its existing embedding, no re-embedding) from this backend into the configured -storage-backend/-storage-path, then exit without starting the server")
	migrateFromPath := flag.String("migrate-from-path", "", "storage path to read from for -migrate-from-backend (required if that flag is set)")
	migrateFromMode := flag.String("migrate-from-mode", "hybrid", "tinySQL storage mode for -migrate-from-backend, if it's tinysql")
	settingsPath := flag.String("settings", "settings.json", "settings file path")
	localURL := flag.String("url", "http://localhost:1234", "local LLM API base URL, OpenAI-compatible (first run only)")
	chatModel := flag.String("chat", "mistralai/ministral-3-3b", "local chat model name (first run only)")
	embedModel := flag.String("embed", "text-embedding-nomic-embed-text-v1.5", "local embedding model name (first run only)")
	azureURL := flag.String("azure-url", "", "Azure OpenAI resource URL, e.g. https://myres.openai.azure.com (first run only)")
	azureChatDeployment := flag.String("azure-chat-deployment", "", "Azure chat deployment name (first run only)")
	azureEmbedDeployment := flag.String("azure-embed-deployment", "", "Azure embeddings deployment name (first run only)")
	azureAPIVersion := flag.String("azure-api-version", "2024-10-21", "Azure OpenAI api-version (first run only)")
	lang := flag.String("lang", "de", "UI/response language")
	chunkSize := flag.Int("chunk", 800, "chunk size for text splitting (first run only)")
	k := flag.Int("k", 5, "number of chunks retrieved as chat context (first run only)")
	flag.Parse()
	verbose = *verboseFlag

	storagePathResolved := *storagePath
	if *storageBackend == "sqlite" && storagePathResolved == "r3-data" {
		// "r3-data" is a sensible directory name for tinySQL's disk/hybrid
		// modes but an odd literal filename for a single SQLite file —
		// nudge the default only when the caller didn't override -storage-path.
		storagePathResolved = "r3-data.db"
	}

	defaults := defaultSettings(*localURL, *chatModel, *embedModel, *lang, *chunkSize, *k)
	defaults.Storage = storageSettings{
		Backend:     *storageBackend,
		Mode:        *storageMode,
		Path:        storagePathResolved,
		MaxMemoryMB: *storageMaxMemMB,
	}
	if *azureURL != "" {
		defaults.Profiles.Azure.BaseURL = normalizeBaseURL(*azureURL)
		defaults.Profiles.Azure.ChatModel = *azureChatDeployment
		defaults.Profiles.Azure.EmbedModel = *azureEmbedDeployment
		defaults.Profiles.Azure.APIVersion = *azureAPIVersion
	}
	var err error
	settings, err = loadOrCreateSettings(*settingsPath, defaults)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	// feedback.go's log lives next to whichever -settings file this
	// instance uses, so separate instances (see launch.json's r3-verify/
	// r3-verify2) don't share one feedback log.
	feedbackLogPath = filepath.Join(filepath.Dir(*settingsPath), "r3-feedback.jsonl")
	// audit.go's log follows the same per-instance-file convention.
	auditLogPath = filepath.Join(filepath.Dir(*settingsPath), "r3-audit.jsonl")
	// settings_history.go's change log likewise.
	settingsHistoryPath = filepath.Join(filepath.Dir(*settingsPath), "r3-settings-history.jsonl")
	if err := initSchedulerOperations(
		filepath.Join(filepath.Dir(*settingsPath), "r3-scheduler-history.jsonl"),
		filepath.Join(filepath.Dir(*settingsPath), "r3-scheduler-alerts.json"),
	); err != nil {
		log.Fatalf("scheduler operations: %v", err)
	}
	// session.go's persisted signing secret and session store follow the
	// same per-instance-file convention, so separate instances don't share
	// (or fight over) either file.
	sessionSecretPath = filepath.Join(filepath.Dir(*settingsPath), "r3-session-secret.bin")
	sessionStorePath = filepath.Join(filepath.Dir(*settingsPath), "r3-sessions.gob")
	initSessionPersistence()

	s := settings.get()
	embedLM, chatLMs := buildLLMClients(s)
	if err := embedLM.ping(); err != nil {
		log.Printf("WARN: embedding endpoint (%s profile) not reachable yet: %v", s.EmbedProfile, err)
	}
	if azure, ok := chatLMs["azure"]; ok {
		if err := azure.ping(); err != nil {
			log.Printf("WARN: Azure OpenAI endpoint not reachable yet: %v", err)
		}
	}

	store, err := newVectorStore(s.Storage)
	if err != nil {
		log.Fatalf("vector store: %v", err)
	}

	localUsers, err = newLocalUserStore(s.Storage)
	if err != nil {
		log.Fatalf("local user store: %v", err)
	}
	if err := localUsers.init(); err != nil {
		log.Fatalf("local user store init: %v", err)
	}

	chatHistory, err = newChatHistoryStore(s.ChatHistoryPath)
	if err != nil {
		log.Fatalf("chat history store: %v", err)
	}
	tokenUsage, err = newTokenUsageStore(s.TokenUsagePath)
	if err != nil {
		log.Fatalf("token usage store: %v", err)
	}
	userPrefsDB, err = newUserPrefsStore(s.UserPrefsPath)
	if err != nil {
		log.Fatalf("user prefs store: %v", err)
	}
	rag := newRAG(embedLM, chatLMs, s.ChatProfile, store)
	if err := rag.init(); err != nil {
		log.Fatalf("rag init: %v", err)
	}
	// Per-document ACLs intentionally live beside settings rather than inside
	// a vector backend: they survive a tinySQL/SQLite migration unchanged and
	// can be edited without rewriting embeddings.
	aclStore, err := newSourceACLStore(filepath.Join(filepath.Dir(*settingsPath), "r3-source-acl.json"))
	if err != nil {
		log.Fatalf("source ACL store: %v", err)
	}
	rag.sourceACLs = aclStore

	// One-shot migration mode: copy every chunk from a differently-backed
	// store into the one just opened above, then exit — doesn't start the
	// server. Lets `make migrate` move data from tinySQL to sqlite (or
	// back) without re-embedding, since exportAll/importRaw preserve
	// vectors verbatim (see migrate.go).
	if *migrateFromBackend != "" {
		if *migrateFromPath == "" {
			log.Fatalf("-migrate-from-path is required together with -migrate-from-backend")
		}
		srcStore, err := newVectorStore(storageSettings{
			Backend: *migrateFromBackend,
			Mode:    *migrateFromMode,
			Path:    *migrateFromPath,
		})
		if err != nil {
			log.Fatalf("open migration source (%s, %s): %v", *migrateFromBackend, *migrateFromPath, err)
		}
		if err := srcStore.init(); err != nil {
			log.Fatalf("init migration source: %v", err)
		}
		n, err := runMigration(srcStore, store)
		if err != nil {
			log.Fatalf("migration failed: %v", err)
		}
		if err := rag.save(); err != nil {
			log.Printf("WARN: save after migration failed: %v", err)
		}
		fmt.Printf("Migrated %d chunks from %s (%s) to %s (%s)\n", n, *migrateFromBackend, *migrateFromPath, s.Storage.Backend, s.Storage.Path)
		return
	}

	fmt.Printf("R3 (Rubix Ranked RAG) — %d chunks loaded from %s\n", rag.docCount(), s.Storage.Path)

	// OpenAI-compatible API server (openai_api.go) — its own port, off by
	// default (settings.OpenAIAPI.Enabled/Port). Re-reconciled on every
	// settings save (handlers.go's handleSettings), so this startup call
	// only matters for whatever was already saved before this run.
	reconcileOpenAIAPIServer(rag, s.OpenAIAPI)

	// Background scheduler (scheduler.go) — currently drives only
	// Freshservice's optional periodic sync (settings.Freshservice.
	// SyncIntervalMinutes); a no-op if that's unconfigured. schedCancel is
	// called on shutdown below so it stops cleanly alongside the final
	// rag.save().
	schedCtx, schedCancel := context.WithCancel(context.Background())
	go startScheduler(schedCtx, rag)

	mux := http.NewServeMux()
	registerRoutes(mux, rag)
	if verbose {
		fmt.Println("Verbose mode: logging every request and LLM call")
	}
	handler := verboseMiddleware(sessionCacheMiddleware(mux))

	// Flush pending writes on shutdown. Matters most for ModeMemory/ModeWAL
	// (a full GOB snapshot only ever happens on save()); ModeDisk/ModeIndex/
	// ModeHybrid sync incrementally on every ingest already, but a last
	// save() here is still cheap insurance against losing whatever changed
	// since the most recent ingest's own save call.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("Shutting down, flushing vector store...")
		schedCancel()
		stopOpenAIAPIServer()
		if err := rag.save(); err != nil {
			log.Printf("WARN: final save failed: %v", err)
		}
		os.Exit(0)
	}()

	fmt.Printf("Listening on %s\n", *addr)
	if err := http.ListenAndServe(*addr, handler); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
