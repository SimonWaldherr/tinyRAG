package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared plumbing for the importer/connector family (Confluence, Jira,
// Freshservice, SharePoint, IMAP, Exchange/Graph mail, Teams, Web, PST).
// Each connector keeps its own preview/import function with its own
// auth scheme, pagination and item→text mapping — those genuinely differ
// per connector and aren't unified here. What *is* identical across all of
// them (secret resolution, the NDJSON streaming handler skeleton, the
// enabled-gate check, and the ingestDocument/ingestEmailAttachment result-
// folding branches) lives in this file once instead of once per connector.
// MSSQL (a live read-only query tool, not an importer) and migrate.go (a
// storage-backend migration tool) don't participate in any of this.
// ─────────────────────────────────────────────────────────────────────────────

// resolveSecret prefers the env-var-named secret over the inline one, so a
// deployment can avoid committing API tokens/passwords to settings.json.
// Every connector config with a Foo/FooEnv field pair (Confluence's
// api_token/api_token_env, Jira's, Freshservice's, SharePoint/Teams/
// Exchange's client_secret/client_secret_env via graphCreds, IMAP's
// password/password_env, SMTP's, MSSQL's) delegates to this one
// implementation through its own named resolved*/resolvedX accessor —
// those accessors keep their existing names/signatures so callers and
// existing tests are unaffected.
func resolveSecret(inline, envVar string) string {
	if envVar != "" {
		if v := os.Getenv(envVar); v != "" {
			return v
		}
	}
	return inline
}

// ndjsonStream sets the NDJSON response headers and returns an emit
// closure that JSON-encodes v and flushes it immediately if the
// ResponseWriter supports flushing (tolerating one that doesn't, same as
// the handlers this replaces) — the "Content-Type + X-Accel-Buffering +
// flusher + json.Encoder" scaffolding every streaming import handler
// (Confluence, Jira, Freshservice, SharePoint, Teams, Exchange mail, IMAP,
// Web, PST) previously repeated by hand around its own typed progress/done
// messages. Callers still build and pass their own XStreamMsg values, so
// the wire format is unchanged — this only shares the mechanics of writing
// them.
func ndjsonStream(w http.ResponseWriter) func(v any) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	return func(v any) {
		_ = enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// requireEnabled writes a 403 with msg and returns false if enabled is
// false — the guard every connector's preview/import handler checks first
// (each with its own connector-specific msg).
func requireEnabled(w http.ResponseWriter, enabled bool, msg string) bool {
	if !enabled {
		writeJSONError(w, msg, 403)
		return false
	}
	return true
}

// formatSourceName builds the "Subject — From" label used as SourceName for
// every mail-shaped import (IMAP, Exchange/Graph mail, PST, Teams).
func formatSourceName(subject, from string) string {
	return fmt.Sprintf("%s — %s", subject, from)
}

// unixOrZero returns t as unix seconds, or 0 if t is the zero time — the
// docDate every mail-shaped connector computes from its item's timestamp.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// foldIngestOutcome applies the three-way branch every importer repeats
// after calling ingestDocument: an error is recorded against the item's
// source_id (still set on outcome even when err != nil, see
// ingestDocument), an unchanged source increments skipped, anything else
// adds its chunk count.
func foldIngestOutcome(outcome ingestOutcome, err error, errs *[]string, skipped, chunks *int) {
	switch {
	case err != nil:
		*errs = append(*errs, fmt.Sprintf("%s: %v", outcome.SourceID, err))
	case outcome.Skipped:
		*skipped++
	default:
		*chunks += outcome.Chunks
	}
}

// foldAttachmentOutcome applies the analogous three-way branch every
// mail-shaped importer (IMAP, Exchange/Graph mail, PST) repeats after
// calling ingestEmailAttachment: a failed or unsupported attachment counts
// as skipped rather than as a hard error (most mailboxes carry plenty of
// inline images/signatures no RAG answer would ever cite), anything else
// counts as ingested. The skip reason is also appended to warnings (rather
// than only bumping a counter) so an admin can tell "23 attachments
// skipped" apart from "23 attachments skipped, all because tesseract OCR
// is disabled" — the former looks like normal mailbox noise, the latter is
// an actionable, fixable gap.
func foldAttachmentOutcome(outcome ingestOutcome, err error, attachments, skipped, chunks *int, warnings *[]string) {
	switch {
	case err != nil:
		*skipped++
		*warnings = append(*warnings, fmt.Sprintf("%s: %v", outcome.SourceName, err))
	case outcome.Skipped:
		*skipped++
	default:
		*attachments++
		*chunks += outcome.Chunks
	}
}

// mailAttachmentWarnings is embedded anonymously (like baseImportResult) by
// every mail-shaped XImportResult that calls foldAttachmentOutcome, so
// per-attachment skip reasons surface to the caller without being confused
// with the harder top-level Errors (which usually abort the whole import).
type mailAttachmentWarnings struct {
	AttachmentWarnings []string `json:"attachment_warnings,omitempty"`
}

// baseImportResult is the common tail every XImportResult embeds anonymously
// — encoding/json promotes its fields to the top level, so the JSON shape
// (and therefore web/app.js) is unchanged versus each connector declaring
// these three fields itself.
type baseImportResult struct {
	Chunks  int      `json:"chunks"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
	// DryRun is set (once, by each importX function itself, not by the
	// folder/fold helpers below) when the request asked to simulate rather
	// than actually import — see ingestDocument's doc comment (ingest.go).
	// Every count above still reflects what *would* have happened; nothing
	// was embedded or written to the vector store.
	DryRun bool `json:"dry_run,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP hardening shared by the Basic-auth REST connectors (Confluence/Jira/
// Freshservice, via restconnector.go's basicAuthREST) and Web
// (webimport.go). graph.go's Microsoft Graph plumbing (SharePoint/Teams/
// Exchange mail) has its own independently-evolving timeout/retry story and
// is deliberately not routed through this — see connector.go's package
// comment.
// ─────────────────────────────────────────────────────────────────────────────

// connectorUserAgent identifies R3 to every upstream connector API — the
// same string webimport.go already sent, now the one source for all of them.
const connectorUserAgent = "R3-RAG-Importer/1.0"

// queryTemplateToolSchema builds the shared tool-description + JSON-schema
// parameter block for an admin-authored query template (MSSQL or HTTP) —
// the single place that turns a template's admin-facing fields into what
// the LLM actually sees. The model never sees the SQL/URL itself, so the
// description has to carry everything it needs to decide *whether* and
// *how* to call the query: the admin's own description, a compact
// per-parameter reference (type, required/optional, description, example),
// and an optional note on what comes back. Each parameter's example is
// also placed into its JSON-schema property (some models read it there).
func queryTemplateToolSchema(desc string, params []sqlQueryParam, resultHint string) (fullDesc string, props map[string]any, required []string) {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(desc))
	props = map[string]any{}
	// Never nil, even with zero required parameters: json.Marshal(nil
	// []string) produces `null`, and a JSON-schema "required" field must be
	// an array — several providers (observed: an OpenAI-compatible backend
	// rejecting the whole request with "Invalid schema for function ...:
	// None is not of type 'array'") reject the tool outright otherwise, not
	// just this one template.
	required = []string{}
	for _, p := range params {
		prop := map[string]any{
			"type":        jsonSchemaTypeFor(p.Type),
			"description": strings.TrimSpace(p.Description),
		}
		if strings.TrimSpace(p.Example) != "" {
			prop["examples"] = []any{strings.TrimSpace(p.Example)}
		}
		if len(p.Options) > 0 {
			// A real JSON-schema "enum", not just prose — most providers
			// steer the model toward only ever emitting a listed value.
			// Execution-time enforcement (mssqlTemplateToolExecutor/
			// httpTemplateToolExecutor) is the actual guarantee; this is the
			// steering that makes hitting that guard rare in practice.
			enum := make([]any, len(p.Options))
			for i, o := range p.Options {
				enum[i] = o
			}
			prop["enum"] = enum
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	if len(params) > 0 {
		b.WriteString("\n\nParameter:")
		for _, p := range params {
			req := "optional"
			if p.Required {
				req = "erforderlich"
			}
			// The admin's declared type (e.g. "date") is more informative to
			// the model than the coarser JSON-schema type it maps to
			// ("string") — a "date" hint tells the model the expected format.
			ptype := strings.TrimSpace(p.Type)
			if ptype == "" {
				ptype = "string"
			}
			fmt.Fprintf(&b, "\n- %s (%s, %s)", p.Name, ptype, req)
			if d := strings.TrimSpace(p.Description); d != "" {
				fmt.Fprintf(&b, ": %s", d)
			}
			if ex := strings.TrimSpace(p.Example); ex != "" {
				fmt.Fprintf(&b, " [Beispiel: %s]", ex)
			}
			if len(p.Options) > 0 {
				fmt.Fprintf(&b, " [erlaubte Werte: %s]", strings.Join(p.Options, ", "))
			}
		}
	}
	if rh := strings.TrimSpace(resultHint); rh != "" {
		fmt.Fprintf(&b, "\n\nRückgabe: %s", rh)
	}
	return b.String(), props, required
}

// templateParamOptionMatches reports whether converted (the already
// type-converted parameter value — a string for an HTTP template, any of
// string/int64/float64/bool for a SQL template) case-insensitively matches
// one of options — the execution-time half of sqlQueryParam.Options'
// enforcement, called by mssqlTemplateToolExecutor/httpTemplateToolExecutor
// after the JSON-schema "enum" (which only steers, never guarantees) has
// already been offered to the model.
func templateParamOptionMatches(options []string, converted any) bool {
	value := fmt.Sprint(converted)
	for _, o := range options {
		if strings.EqualFold(value, o) {
			return true
		}
	}
	return false
}

// validateTemplateParamOptions is shared by validateSQLQueryTemplates and
// validateHTTPQueryTemplates: sqlQueryParam.Options, when set, must have no
// empty or (case-insensitively) duplicate entries — an admin typo here
// would otherwise silently make one of the "allowed" values impossible to
// hit exactly, or let a duplicate slip past validation while looking
// intentional in the settings UI.
func validateTemplateParamOptions(options []string) error {
	seen := map[string]bool{}
	for _, o := range options {
		trimmed := strings.TrimSpace(o)
		if trimmed == "" {
			return fmt.Errorf("options: empty value")
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			return fmt.Errorf("options: duplicate value %q (case-insensitive)", trimmed)
		}
		seen[key] = true
	}
	return nil
}

// connectorDefaultTimeoutSeconds is the fallback per-attempt HTTP timeout
// for connectorHTTPClient/insecureConnectorHTTPClient when
// importConfig.RESTConnectorTimeoutSeconds isn't set — see
// connectorTimeout. These three previously used http.DefaultClient, which
// has none at all — a hung upstream connection would otherwise block an
// import indefinitely.
const connectorDefaultTimeoutSeconds = 30

// connectorHTTPClient is the shared client every Basic-auth REST connector
// GET goes through. Deliberately has NO client-level Timeout: the
// configurable per-attempt timeout is applied in doWithRetry via
// context.WithTimeout instead, so an admin raising
// RESTConnectorTimeoutSeconds above the 30s default actually takes effect
// (a fixed client.Timeout would otherwise silently cap every attempt at
// whatever it was set to at startup, regardless of later settings changes).
// The tracingTransport wrapper (conntrace.go) is a pure pass-through
// unless the request context opted into capture via withConnTrace —
// only the Settings connection tests do.
var connectorHTTPClient = &http.Client{Transport: tracingTransport{}}

// insecureConnectorHTTPClient mirrors connectorHTTPClient but skips TLS
// certificate verification — used ONLY when an admin explicitly opts a
// specific HTTP query template into it (httpQueryTemplate.
// InsecureSkipVerify, http_tool.go), for an internal endpoint whose
// certificate is self-signed or issued by an internal CA the Go process
// doesn't already trust (e.g. an on-prem SAP se16-style gateway) — same
// "explicit, scoped opt-in for a known-internal system" posture as
// mssqlConfig.TrustServerCertificate. Deliberately a SEPARATE client from
// connectorHTTPClient, which every other connector (Confluence/Jira/
// Freshservice — external SaaS with real, publicly-trusted certificates)
// still goes through unconditionally: this must never become the default
// for a connector talking to the public internet.
var insecureConnectorHTTPClient = &http.Client{
	Transport: tracingTransport{base: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
}

// connectorTimeout resolves the effective per-attempt HTTP timeout: the
// configured importConfig.RESTConnectorTimeoutSeconds when positive, else
// connectorDefaultTimeoutSeconds. A slow internal API (or one behind a
// high-latency VPN hop) may need longer than 30s per request; a fast
// internal API behind a flaky proxy may want a shorter timeout to fail
// over faster instead of hanging the whole import.
func connectorTimeout() time.Duration {
	if s := settings.get().Import.RESTConnectorTimeoutSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return connectorDefaultTimeoutSeconds * time.Second
}

// connectorMaxRetries bounds how many extra attempts doWithRetry makes
// after a 429 (rate limit) or 5xx (transient) response before giving up —
// the same policy shape as graph.go's graphMaxRetries, independently
// applied here since Confluence/Jira/Freshservice don't go through graph.go.
// A var, not a const, so a test exhausting all retries can lower it instead
// of sleeping through the full real backoff schedule. This is the FALLBACK
// default when importConfig.ConnectorMaxRetries (settings.go) isn't set —
// see connectorMaxRetriesLimit, which doWithRetry actually calls.
var connectorMaxRetries = 4

// connectorMaxRetriesLimit resolves the effective retry bound, same
// settings-first/package-var-fallback shape as graph.go's
// graphMaxRetriesLimit.
func connectorMaxRetriesLimit() int {
	if n := settings.get().Import.ConnectorMaxRetries; n > 0 {
		return n
	}
	return connectorMaxRetries
}

// parseConnectorRetryAfter reads the Retry-After response header (seconds,
// per RFC 9110) — 0 if absent or unparseable, in which case
// connectorBackoff falls back to exponential backoff instead of honoring
// the server's own hint.
func parseConnectorRetryAfter(h http.Header) time.Duration {
	secs, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// connectorBackoff picks how long to wait before the next retry attempt:
// the server's own Retry-After if it sent one, otherwise exponential
// backoff (500ms, 1s, 2s, 4s, ...) capped at 30s.
func connectorBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := 500 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// doWithRetry executes req (method/headers/auth already set — every caller
// here is a bodyless GET, so req is safe to reuse across retry attempts)
// via connectorHTTPClient (or insecureConnectorHTTPClient when
// insecureSkipVerify is true — see its doc comment), retrying on 429/5xx
// with backoff honoring Retry-After when the server sends one. Returns
// the raw body on 200; on any other status (retries exhausted, or a
// non-retryable code) returns an error already formatted as "<status>:
// <body>", matching what every caller here previously built by hand. A
// backoff wait is itself cancelled immediately if req's context is done.
// A network-level error (no response at all) is returned immediately,
// never retried — that's usually not a transient server-side condition.
func doWithRetry(req *http.Request, insecureSkipVerify bool) ([]byte, error) {
	client := connectorHTTPClient
	if insecureSkipVerify {
		client = insecureConnectorHTTPClient
	}
	return doWithRetryClient(req, client, 0)
}

// doWithRetryLimitedNoRedirect is the stricter variant for LLM-triggerable
// HTTP query templates. A configured endpoint may legitimately redirect an
// administrator using a browser, but following that redirect from a live tool
// could turn one pinned GET into a request to an unreviewed host. The template
// result is also destined for an LLM, so reading an unbounded response only to
// truncate it later wastes memory and can starve concurrent requests.
func doWithRetryLimitedNoRedirect(req *http.Request, insecureSkipVerify bool, maxResponseBytes int64) ([]byte, error) {
	base := connectorHTTPClient
	if insecureSkipVerify {
		base = insecureConnectorHTTPClient
	}
	client := *base
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return doWithRetryClient(req, &client, maxResponseBytes)
}

// doWithRetryClient contains the shared retry loop. maxResponseBytes of zero
// retains the historic unbounded behavior for import connectors; live HTTP
// templates use the bounded no-redirect wrapper above.
func doWithRetryClient(req *http.Request, client *http.Client, maxResponseBytes int64) ([]byte, error) {
	timeout := connectorTimeout()
	maxRetries := connectorMaxRetriesLimit()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptCtx, cancel := context.WithTimeout(req.Context(), timeout)
		resp, err := client.Do(req.Clone(attemptCtx))
		if err != nil {
			cancel()
			return nil, err
		}
		if maxResponseBytes > 0 && resp.ContentLength > maxResponseBytes {
			resp.Body.Close()
			cancel()
			return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
		}
		var raw []byte
		if maxResponseBytes > 0 {
			raw, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		} else {
			raw, err = io.ReadAll(resp.Body)
		}
		resp.Body.Close()
		cancel()
		if err != nil {
			return nil, err
		}
		if maxResponseBytes > 0 && int64(len(raw)) > maxResponseBytes {
			return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
		}
		if resp.StatusCode == http.StatusOK {
			return raw, nil
		}
		// Error bodies are often HTML proxy pages. Keep the diagnostic useful
		// without letting an upstream response flood logs, audit previews or
		// an agent's tool-result message.
		lastErr = fmt.Errorf("%d: %s", resp.StatusCode, truncateRunesNote(string(raw), 4096))
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < maxRetries {
			select {
			case <-time.After(connectorBackoff(attempt, parseConnectorRetryAfter(resp.Header))):
				continue
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return nil, lastErr
	}
	return nil, lastErr
}
