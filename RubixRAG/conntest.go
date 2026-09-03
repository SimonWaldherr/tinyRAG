package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// "Verbindung testen" for every external interface configured in the
// Settings tab (LLM backends, LDAP, SharePoint/OneDrive, Exchange, IMAP,
// Teams, Confluence, Jira, Freshservice, GitHub, SAP S/4, SMTP, MSSQL). Each endpoint takes the
// connector's config shape (the same JSON the Settings save button already
// sends) straight from the not-yet-saved form fields, so an admin can
// validate credentials before committing them — and returns a uniform
// {ok, detail} shape rather than a per-connector response, since the
// frontend only ever needs one line of text to show, colored green/red.
//
// Every test reuses the connector's real preview/dial/query function
// rather than reimplementing a lighter-weight probe — the whole point is
// to prove the actual settings work, not just that a URL is reachable.
// ─────────────────────────────────────────────────────────────────────────────

type testResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Exchanges carries the raw (secret-redacted, see conntrace.go)
	// request/response dumps of every HTTP call this test performed —
	// only present for the HTTP-based connectors; the frontend renders
	// them behind a click-to-expand "Details" toggle, never inline.
	Exchanges []connExchange `json:"exchanges,omitempty"`
}

// writeTestResult always answers 200: a failed connectivity test is a
// successful API call that reports a negative result, not an HTTP error —
// only a malformed request body (see the json.Decode checks below) is a
// real 4xx.
func writeTestResult(w http.ResponseWriter, detail string, err error) {
	writeTestResultTrace(w, detail, err, nil)
}

// writeTestResultTrace is writeTestResult plus the captured raw
// exchanges — passing a nil trace degrades to the plain result, so the
// non-HTTP tests (LDAP/IMAP/SMTP/MSSQL, whose transports aren't
// capturable, see conntrace.go's package comment) share one code path.
func writeTestResultTrace(w http.ResponseWriter, detail string, err error, trace *connTrace) {
	res := testResult{OK: err == nil, Detail: detail, Exchanges: trace.snapshot()}
	if err != nil {
		res.Detail = err.Error()
	}
	writeJSON(w, res)
}

// testCtx bounds a connectivity test at a fixed timeout independent of the
// request's own context — a hung dial/query shouldn't tie up the admin's
// browser tab indefinitely. Not every test below uses it: the underlying
// ldap/imap client libraries don't accept a context at all, so those tests
// are bounded only by those libraries' own (uncustomizable) network
// timeouts, same as a real login already is today.
func testCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

// resolveTestSecret returns formValue unless it's empty or the masked
// placeholder maskedSettings hands back for an already-configured secret
// ("***set***", see handlers.go's maskSecret) — in which case it falls
// back to the currently saved secret. Without this, testing an untouched
// secret field would send the literal placeholder text as the credential
// and fail every time.
func resolveTestSecret(formValue, saved string) string {
	if formValue == "" || strings.Contains(formValue, "***") {
		return saved
	}
	return formValue
}

// --- LLM backends -----------------------------------------------------------

// handleTestLLM exercises a minimal chat completion against every chat
// provider. Embeddings are tested only for the local LM-Studio profile,
// because all cloud profiles are deliberately chat-only in RubixRAG.
func handleTestLLM(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var profile llmProfile
	if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	if !isSupportedChatProfile(profile.Provider) {
		writeJSONError(w, "unknown chat provider", http.StatusBadRequest)
		return
	}
	saved := settings.get()
	savedProfile := savedLLMProfile(saved, profile.Provider)
	profile.APIKey = resolveTestSecret(profile.APIKey, savedProfile.APIKey)

	client := newLMClientFromProfile(profile)
	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)

	var embedMS int64
	var embedDim int
	if strings.EqualFold(strings.TrimSpace(profile.Provider), "local") {
		start := time.Now()
		vec, err := client.embedSingleCtx(ctx, "Verbindungstest")
		if err != nil {
			writeTestResultTrace(w, "", fmt.Errorf("Embedding-Aufruf fehlgeschlagen: %w", err), trace)
			return
		}
		embedMS = time.Since(start).Milliseconds()
		embedDim = len(vec)
	}

	start := time.Now()
	if _, err := client.chatOnce(ctx, []chatMsg{
		{Role: "system", Content: "Antworte ausschließlich mit dem einzelnen Wort OK."},
		{Role: "user", Content: "Verbindungstest"},
	}, nil); err != nil {
		if embedDim > 0 {
			writeTestResultTrace(w, "", fmt.Errorf("Embedding ok (%d ms, %d Dimensionen), aber Chat-Aufruf fehlgeschlagen: %w", embedMS, embedDim, err), trace)
		} else {
			writeTestResultTrace(w, "", fmt.Errorf("Chat-Aufruf fehlgeschlagen: %w", err), trace)
		}
		return
	}
	chatMS := time.Since(start).Milliseconds()
	if embedDim > 0 {
		writeTestResultTrace(w, fmt.Sprintf("Embedding ok (%d ms, %d Dimensionen), Chat-Aufruf ok (%d ms).", embedMS, embedDim, chatMS), nil, trace)
	} else {
		writeTestResultTrace(w, fmt.Sprintf("Chat-Aufruf ok (%d ms) (chat-only provider).", chatMS), nil, trace)
	}
}

func savedLLMProfile(s appSettings, provider string) llmProfile {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "azure":
		return s.Profiles.Azure
	case "openai":
		return s.Profiles.OpenAI
	case "openrouter":
		return s.Profiles.OpenRouter
	case "claude":
		return s.Profiles.Claude
	case "gemini":
		return s.Profiles.Gemini
	default:
		return s.Profiles.Local
	}
}

// llmModelsResult carries a model-id list alongside the usual {ok, detail}
// shape — the one test endpoint that returns more than a status line, so
// it gets its own response type rather than overloading testResult.
type llmModelsResult struct {
	OK     bool     `json:"ok"`
	Detail string   `json:"detail"`
	Models []string `json:"models,omitempty"`
}

// handleTestLLMModels lists every model id the local backend's GET
// /v1/models reports (see llm.go's listModels) — local/OpenAI-compatible
// only, since Azure has no equivalent tenant-scoped listing endpoint (an
// admin still types the Azure deployment name directly). Used by the
// Settings UI to offer a datalist for the Chat-/Embedding-Modell fields
// instead of requiring an exact, blind-typed model id.
func handleTestLLMModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var profile llmProfile
	if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	provider := strings.ToLower(strings.TrimSpace(profile.Provider))
	if provider == "claude" || provider == "gemini" || isAzureProvider(provider) {
		writeJSONError(w, "model listing is only supported for local/OpenAI-compatible backends", 400)
		return
	}
	saved := settings.get()
	profile.APIKey = resolveTestSecret(profile.APIKey, savedLLMProfile(saved, provider).APIKey)

	client := newLMClientFromProfile(profile)
	ctx, cancel := testCtx(r)
	defer cancel()
	models, err := client.listModels(ctx)
	if err != nil {
		writeJSON(w, llmModelsResult{OK: false, Detail: err.Error()})
		return
	}
	writeJSON(w, llmModelsResult{OK: true, Detail: fmt.Sprintf("%d Modell(e) gefunden.", len(models)), Models: models})
}

// --- LDAP -------------------------------------------------------------------

// ldapTestRequest embeds ldapConfig so the not-yet-saved connection
// fields round-trip as-is, plus a throwaway test account that's never
// persisted — ldapConfig itself carries no service-account credentials
// (see its doc comment in settings.go), so the only way to prove a bind
// actually works is a real, one-off login attempt, exactly like
// handleLDAPLogin does for a real user.
type ldapTestRequest struct {
	ldapConfig
	TestUsername string `json:"test_username"`
	TestPassword string `json:"test_password"`
}

func handleTestLDAP(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req ldapTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	if strings.TrimSpace(req.TestUsername) == "" || req.TestPassword == "" {
		writeJSONError(w, "Testbenutzer und Testpasswort werden benötigt", 400)
		return
	}
	u, err := ldapAuthenticate(req.ldapConfig, settings.get().PromptsDir, req.TestUsername, req.TestPassword)
	if err != nil {
		writeTestResult(w, "", err)
		return
	}
	admin := "nein"
	if u.IsAdmin {
		admin = "ja"
	}
	writeTestResult(w, fmt.Sprintf("Bind erfolgreich als %q (%s), Abteilung %q. Admin-Zugriff: %s.", u.CN, u.Mail, u.Department, admin), nil)
}

// --- SMTP --------------------------------------------------------------

// smtpTestRequest embeds smtpConfig plus a recipient that's only ever used
// for this one send, never persisted — smtpConfig.From's doc comment
// (settings.go) explains why real chat traffic can only ever mail the
// logged-in user's own AD address; that restriction doesn't apply here
// since this is an admin-only debug action, not end-user-reachable.
type smtpTestRequest struct {
	smtpConfig
	TestRecipient string `json:"test_recipient"`
}

func handleTestSMTP(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req smtpTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	if strings.TrimSpace(req.TestRecipient) == "" {
		writeJSONError(w, "Test-Empfänger fehlt", 400)
		return
	}
	saved := settings.get().SMTP
	req.Password = resolveTestSecret(req.Password, saved.Password)
	// Testing shouldn't require the admin to first tick "aktivieren" and
	// save — the test click itself is the intent signal.
	req.Enabled = true

	err := sendMail(req.smtpConfig, req.TestRecipient, "R3 SMTP-Test",
		"Dies ist eine Testmail von R3 (Rubix Ranked RAG), ausgelöst über Einstellungen -> E-Mail (SMTP) -> Verbindung testen.\n\nWenn diese Mail ankommt, ist die SMTP-Konfiguration korrekt.")
	writeTestResult(w, fmt.Sprintf("Testmail an %s gesendet.", req.TestRecipient), err)
}

// --- MSSQL -------------------------------------------------------------

func handleTestMSSQL(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg mssqlConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved := settings.get().MSSQL
	cfg.Password = resolveTestSecret(cfg.Password, saved.Password)

	dsn, err := mssqlDSN(cfg)
	if err != nil {
		writeTestResult(w, "", err)
		return
	}
	ctx, cancel := testCtx(r)
	defer cancel()
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		writeTestResult(w, "", fmt.Errorf("mssql: open: %w", err))
		return
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		writeTestResult(w, "", fmt.Errorf("mssql: ping: %w", err))
		return
	}
	writeTestResult(w, "Verbindung erfolgreich (Ping ok).", nil)
}

// mssqlTemplateTestRequest is handleTestMSSQLTemplate's body: the
// (possibly not-yet-saved) connection config plus the one template to try —
// same "test before saving" pattern as handleTestMSSQL above, one level
// more specific.
type mssqlTemplateTestRequest struct {
	Config   mssqlConfig      `json:"config"`
	Template sqlQueryTemplate `json:"template"`
}

// handleTestMSSQLTemplate actually runs one query template's SQL — unlike
// handleTestMSSQL's ping-only connection check, this proves the template
// really executes and returns rows, using each parameter's Example value
// (sqlQueryParam.Example) as the test input. Reuses the exact production
// execution path (mssqlTemplateToolExecutor, mssql.go) rather than a
// separate lighter probe, same "prove the real thing works" philosophy as
// every other test handler in this file — including its SELECT-only guard
// (validateSelectOnly, called unconditionally inside runMSSQLQueryArgs) and
// its column masking, so a broken or unsafe template is caught here, before
// an admin ever enables it for the model to call.
func handleTestMSSQLTemplate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req mssqlTemplateTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved := settings.get().MSSQL
	req.Config.Password = resolveTestSecret(req.Config.Password, saved.Password)

	// Same validation the settings-save path runs (handleSettings ->
	// validateSQLQueryTemplates) — catches a name/parameter-reference
	// mistake or an unsafe statement before ever opening a connection,
	// exactly as useful here (testing before saving) as there.
	if err := validateSQLQueryTemplates([]sqlQueryTemplate{req.Template}); err != nil {
		writeTestResult(w, "", err)
		return
	}
	argsJSON, err := exampleArgsJSON(req.Template.Parameters)
	if err != nil {
		writeTestResult(w, "", err)
		return
	}
	ctx, cancel := testCtx(r)
	defer cancel()
	result, err := mssqlTemplateToolExecutor(req.Config, req.Template)(ctx, argsJSON)
	writeTestResult(w, result, err)
}

// exampleArgsJSON builds a JSON tool-arguments object from params' Example
// values, typed per each parameter's declared Type — the exact shape
// mssqlTemplateToolExecutor expects from a real model tool call, so
// handleTestMSSQLTemplate can hand it to that unmodified executor unchanged.
// A required parameter with no Example is a hard error (nothing sensible to
// test with); an optional one with no Example is simply omitted, exactly
// like a model choosing not to supply it.
func exampleArgsJSON(params []sqlQueryParam) (string, error) {
	args := map[string]json.RawMessage{}
	for _, p := range params {
		if strings.TrimSpace(p.Example) == "" {
			if p.Required {
				return "", fmt.Errorf("parameter %q has no example value — needed to run the test", p.Name)
			}
			continue
		}
		raw, err := exampleParamJSON(p.Type, p.Example)
		if err != nil {
			return "", fmt.Errorf("parameter %q: %w", p.Name, err)
		}
		args[p.Name] = raw
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// exampleParamJSON converts one parameter's free-text Example value into
// the JSON literal convertSQLTemplateParam (mssql.go) expects for that
// type — a plain JSON string encoding would fail integer/number/boolean's
// json.Unmarshal into a typed Go value there.
func exampleParamJSON(paramType, example string) (json.RawMessage, error) {
	switch paramType {
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(example), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("example %q is not a valid integer", example)
		}
		return json.Marshal(n)
	case "number":
		n, err := strconv.ParseFloat(strings.TrimSpace(example), 64)
		if err != nil {
			return nil, fmt.Errorf("example %q is not a valid number", example)
		}
		return json.Marshal(n)
	case "boolean":
		b, err := strconv.ParseBool(strings.TrimSpace(example))
		if err != nil {
			return nil, fmt.Errorf("example %q is not a valid boolean", example)
		}
		return json.Marshal(b)
	default: // "string", "date" — bound as text either way (mssql.go's
		// convertSQLTemplateParam comment)
		return json.Marshal(example)
	}
}

// httpTemplateTestRequest is handleTestHTTPTemplate's body: the one HTTP
// query template to try, plus the REST connectors as currently edited in the
// form (so a template can be tested against a not-yet-saved connector, same
// "test before saving" pattern as handleTestMSSQLTemplate's Config field).
type httpTemplateTestRequest struct {
	Template       httpQueryTemplate     `json:"template"`
	RESTConnectors []restConnectorConfig `json:"rest_connectors,omitempty"`
}

// handleTestHTTPTemplate actually runs one HTTP query template with each
// parameter's Example value substituted in — the REST analogue of
// handleTestMSSQLTemplate: it proves the URL resolves, auth attaches and the
// backend answers, reusing the exact production execution path
// (httpTemplateToolExecutor) and the same save-time validation
// (validateRESTConnectors + validateHTTPQueryTemplates), so a broken template
// or auth_source is caught here before an admin ever enables it for the
// model. The form's REST connectors are folded onto the saved settings with
// masked secrets resolved from the saved connector of the same name (so a
// "***set***" placeholder doesn't blank a real credential).
func handleTestHTTPTemplate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req httpTemplateTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	s := settings.get()
	if len(req.RESTConnectors) > 0 {
		for i := range req.RESTConnectors {
			if saved, ok := restConnectorByName(s, req.RESTConnectors[i].Name); ok {
				req.RESTConnectors[i].Password = resolveTestSecret(req.RESTConnectors[i].Password, saved.Password)
				req.RESTConnectors[i].Token = resolveTestSecret(req.RESTConnectors[i].Token, saved.Token)
			}
		}
		s.RESTConnectors = req.RESTConnectors
		if err := validateRESTConnectors(s.RESTConnectors); err != nil {
			writeTestResult(w, "", err)
			return
		}
	}
	// Same validation the settings-save path runs — catches a bad
	// name/placeholder/host or unknown auth_source before any request goes out.
	if err := validateHTTPQueryTemplates([]httpQueryTemplate{req.Template}, s); err != nil {
		writeTestResult(w, "", err)
		return
	}
	argsJSON, err := exampleArgsJSON(req.Template.Parameters)
	if err != nil {
		writeTestResult(w, "", err)
		return
	}
	ctx, cancel := testCtx(r)
	defer cancel()
	result, err := httpTemplateToolExecutor(req.Template, s)(ctx, argsJSON)
	writeTestResult(w, result, err)
}

// --- Shop (de.rubix.com) -------------------------------------------------

// handleTestShop exercises the actual search-items call (shop.go's
// searchShopItems, itself acquiring a token via shopAccessToken first) with
// a harmless generic test term — proves both the token exchange and the
// search request work end to end, not just that the base URL is reachable.
func handleTestShop(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg shopConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved := settings.get().Shop
	cfg.Password = resolveTestSecret(cfg.Password, saved.Password)
	cfg.ClientSecret = resolveTestSecret(cfg.ClientSecret, saved.ClientSecret)

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	items, _, err := searchShopItemsCached(ctx, cfg, "schraube", 3, false)
	if err != nil {
		writeTestResultTrace(w, "", fmt.Errorf("shop: %w", err), trace)
		return
	}
	// The search above just (re-)authenticated and cached the result, so
	// this is a cache hit — cheap, and lets the result surface which auth
	// contract is actually in effect (see shopAccessToken's cookie-session
	// fallback) without the admin needing a separate "Login testen" click.
	authMode := "Bearer-Token"
	if session, sessErr := shopAccessToken(ctx, cfg); sessErr == nil && session.token == "" {
		authMode = "Cookie-Session"
	}
	writeTestResultTrace(w, fmt.Sprintf("Verbindung erfolgreich (%d Testergebnis(se) für \"schraube\", Auth: %s).", len(items), authMode), nil, trace)
}

// valueOrDash renders an empty diagnostic value as "-" instead of "" so a
// missing header reads as "deliberately absent" rather than looking like
// a formatting bug in the surrounding message.
func valueOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// handleTestShopLogin tests ONLY the token exchange (POST
// {baseURL}/rest-api/v1/tokens), separate from handleTestShop's full
// login+search check — since the token contract is unconfirmed (see
// shop.go's package comment), an admin debugging a login failure needs to
// see the raw server response for *this one step* rather than a single
// combined error that could originate from either the login or the search
// call. Always bypasses the token cache (shop.go's shopTokenRequest, not
// shopAccessToken) so every click is a fresh attempt against the real
// endpoint, never a stale cached result from an earlier try.
func handleTestShopLogin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg shopConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved := settings.get().Shop
	cfg.Password = resolveTestSecret(cfg.Password, saved.Password)
	cfg.ClientSecret = resolveTestSecret(cfg.ClientSecret, saved.ClientSecret)
	password := cfg.resolvedPassword()
	if strings.TrimSpace(cfg.Username) == "" || password == "" {
		writeTestResult(w, "", fmt.Errorf("shop: username/password not set"))
		return
	}
	if strings.TrimSpace(cfg.ClientID) == "" || cfg.resolvedClientSecret() == "" {
		writeTestResult(w, "", fmt.Errorf("shop: client_id/client_secret not set"))
		return
	}

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	// A fresh jar per click — same "never a stale cached result" reasoning
	// as bypassing the token cache below, just extended to cookies.
	jar, jarErr := cookiejar.New(nil)
	if jarErr != nil {
		writeTestResultTrace(w, "", fmt.Errorf("shop: create cookie jar: %w", jarErr), trace)
		return
	}
	attempt, err := shopTokenRequest(ctx, cfg, password, jar)
	if err != nil {
		writeTestResultTrace(w, "", err, trace)
		return
	}
	preview := truncateRunesNote(string(attempt.raw), 500)
	// requestedURL/finalURL diverging means Go's client silently followed a
	// redirect (e.g. to a WAF/consent/login page) instead of reaching the
	// token endpoint itself — the most common real cause of an HTTP 200
	// with an empty or unrecognizable body, distinct from "the field names
	// changed" (see shopTokenRequest's doc comment).
	requestedURL := shopBaseURLOrDefault(cfg.BaseURL) + "/rest-api/v1/tokens"
	diagnostics := fmt.Sprintf(" (Content-Type: %s", valueOrDash(attempt.contentType))
	if attempt.finalURL != "" && attempt.finalURL != requestedURL {
		diagnostics += fmt.Sprintf(", umgeleitet nach: %s", attempt.finalURL)
	}
	diagnostics += ")"
	if attempt.status != http.StatusOK {
		writeTestResultTrace(w, "", fmt.Errorf("HTTP %d%s — Rohantwort: %s", attempt.status, diagnostics, preview), trace)
		return
	}
	if strings.TrimSpace(string(attempt.raw)) == "" {
		if attempt.cookiesSet {
			// No JSON body but real session cookies (and usually a Userid
			// header) — de.rubix.com authenticates at least some accounts
			// this way instead of returning a bearer token (see shop.go's
			// shopAccessToken cookie-session fallback). That's a success,
			// not "never reached the login handler".
			who := attempt.userID
			if who == "" {
				who = "-"
			}
			writeTestResultTrace(w, fmt.Sprintf("Login erfolgreich (HTTP 200%s, leerer Body, aber Session-Cookies gesetzt — cookie-basierte Session statt JSON-Token; Userid: %s).", diagnostics, who), nil, trace)
			return
		}
		writeTestResultTrace(w, "", fmt.Errorf("HTTP 200%s, aber leerer Antwort-Body und keine Set-Cookie-Header — die Anfrage kam vermutlich nie beim eigentlichen Login-Endpunkt an", diagnostics), trace)
		return
	}
	// Token itself is deliberately not echoed into the result — no reason
	// to put a live bearer token in the admin's browser just to confirm it
	// was recognized. (The raw exchanges in trace are already redacted,
	// see conntrace.go's connTraceRedactions.)
	_, expiresIn, parseErr := parseShopTokenResponse(attempt.raw)
	if parseErr != nil {
		if attempt.cookiesSet {
			writeTestResultTrace(w, fmt.Sprintf("Login erfolgreich (HTTP 200%s, kein erkennbares Token-Feld, aber Session-Cookies gesetzt — cookie-basierte Session statt JSON-Token). Rohantwort: %s", diagnostics, preview), nil, trace)
			return
		}
		// A 200 with a body we can't recognize a token field in, and no
		// cookies either — report as a failure (nothing usable came out of
		// it) but still show the raw response, which is exactly what's
		// needed to fix parseShopTokenResponse's candidate field names.
		writeTestResultTrace(w, "", fmt.Errorf("HTTP 200%s, aber kein erkennbares Token-Feld — Rohantwort: %s", diagnostics, preview), trace)
		return
	}
	writeTestResultTrace(w, fmt.Sprintf("Login erfolgreich (HTTP 200, Token erkannt, gültig ~%.0f s). Rohantwort: %s", expiresIn, preview), nil, trace)
}

// --- IMAP --------------------------------------------------------------

func handleTestIMAP(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg mailboxConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().IMAP, cfg.Name)
	cfg.Password = resolveTestSecret(cfg.Password, saved.Password)

	cl := &realIMAPClient{cfg: cfg}
	conn, err := cl.dial()
	if err != nil {
		writeTestResult(w, "", err)
		return
	}
	defer conn.Close()
	defer conn.Logout()
	mailbox := cfg.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	writeTestResult(w, fmt.Sprintf("Login erfolgreich (%s@%s, Postfach %q).", cfg.Username, cfg.Host, mailbox), nil)
}

// --- SharePoint --------------------------------------------------------

func handleTestSharePoint(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg sharePointConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().SharePoint, cfg.Name)
	cfg.ClientSecret = resolveTestSecret(cfg.ClientSecret, saved.ClientSecret)

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	preview, err := previewSharePointFolder(ctx, cfg, "")
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — %d Element(e) im Stammordner der Dokumentbibliothek gefunden.", len(preview.Items)), err, trace)
}

// --- OneDrive -------------------------------------------------------------

func handleTestOneDrive(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg oneDriveConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	saved, _ := findConnByName(settings.get().OneDrive, cfg.Name)
	cfg.ClientSecret = resolveTestSecret(cfg.ClientSecret, saved.ClientSecret)
	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	items, deleted, _, err := oneDriveDeltaSyncFrom(ctx, cfg, "")
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — %d Datei(en), %d Löschung(en) im ersten Delta-Fenster.", len(items), len(deleted)), err, trace)
}

// --- Exchange Online (Graph) --------------------------------------------

func handleTestExchange(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg exchangeGraphConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().ExchangeGraph, cfg.Name)
	cfg.ClientSecret = resolveTestSecret(cfg.ClientSecret, saved.ClientSecret)

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	preview, err := previewExchangeMail(ctx, cfg, importPreviewLimit(settings.get().Import))
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — %d Nachricht(en) im Ordner %q gefunden.", len(preview.Items), preview.Folder), err, trace)
}

// --- Teams ---------------------------------------------------------------

func handleTestTeams(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg teamsConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().Teams, cfg.Name)
	cfg.ClientSecret = resolveTestSecret(cfg.ClientSecret, saved.ClientSecret)

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	preview, err := previewTeamsMessages(ctx, cfg, importPreviewLimit(settings.get().Import))
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — %d Nachricht(en) im Kanal gefunden.", len(preview.Items)), err, trace)
}

// --- Confluence ----------------------------------------------------------

func handleTestConfluence(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg confluenceConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().Confluence, cfg.Name)
	cfg.APIToken = resolveTestSecret(cfg.APIToken, saved.APIToken)

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	preview, err := previewConfluencePages(ctx, cfg, importPreviewLimit(settings.get().Import))
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — %d Seite(n) im Space %q gefunden.", len(preview.Items), preview.SpaceKey), err, trace)
}

// --- Jira ------------------------------------------------------------------

func handleTestJira(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg jiraConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().Jira, cfg.Name)
	cfg.APIToken = resolveTestSecret(cfg.APIToken, saved.APIToken)

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	preview, err := previewJiraIssues(ctx, cfg, importPreviewLimit(settings.get().Import))
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — %d Issue(s) im Projekt %q gefunden.", len(preview.Items), preview.ProjectKey), err, trace)
}

// --- Folder --------------------------------------------------------------

// handleTestFolder confirms Path exists and is a readable directory — the
// only thing there is to test for a connector with no network/credentials.
func handleTestFolder(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg folderConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		writeTestResult(w, "", fmt.Errorf("Pfad ist leer"))
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeTestResult(w, "", fmt.Errorf("Pfad nicht erreichbar: %w", err))
		return
	}
	if !info.IsDir() {
		writeTestResult(w, "", fmt.Errorf("Pfad ist kein Ordner"))
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		writeTestResult(w, "", fmt.Errorf("Ordner nicht lesbar: %w", err))
		return
	}
	writeTestResult(w, fmt.Sprintf("Ordner erreichbar (%d Eintrag/Einträge direkt darin).", len(entries)), nil)
}

// --- Freshservice ----------------------------------------------------------

func handleTestFreshservice(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg freshserviceConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().Freshservice, cfg.Name)
	cfg.APIKey = resolveTestSecret(cfg.APIKey, saved.APIKey)

	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	preview, err := previewFreshserviceTickets(ctx, cfg, importPreviewLimit(settings.get().Import))
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — %d Ticket(s) gefunden.", len(preview.Items)), err, trace)
}

// --- GitHub ---------------------------------------------------------------

func handleTestGitHub(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg githubConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	saved, _ := findConnByName(settings.get().GitHub, cfg.Name)
	cfg.Token = resolveTestSecret(cfg.Token, saved.Token)
	path, err := githubRepositoryPath(cfg)
	if err != nil {
		writeTestResult(w, "", err)
		return
	}
	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	raw, err := githubGet(ctx, cfg, path, nil)
	if err != nil {
		writeTestResultTrace(w, "", err, trace)
		return
	}
	var repo struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	}
	if err := json.Unmarshal(raw, &repo); err != nil {
		writeTestResultTrace(w, "", fmt.Errorf("github: parse repository response: %w", err), trace)
		return
	}
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — Repository %s%s erreichbar.", repo.FullName, map[bool]string{true: " (privat)"}[repo.Private]), nil, trace)
}

// --- SAP S/4HANA ----------------------------------------------------------

func handleTestSAPS4(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg sapS4Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	saved, _ := findConnByName(settings.get().SAPS4, cfg.Name)
	cfg.Password = resolveTestSecret(cfg.Password, saved.Password)
	cfg.Token = resolveTestSecret(cfg.Token, saved.Token)
	url, err := sapS4InitialURL(cfg, 1)
	if err != nil {
		writeTestResult(w, "", err)
		return
	}
	ctx, cancel := testCtx(r)
	defer cancel()
	ctx, trace := withConnTrace(ctx)
	raw, err := sapS4Get(ctx, cfg, url, false)
	if err != nil {
		writeTestResultTrace(w, "", err, trace)
		return
	}
	page, err := parseSAPS4Page(raw)
	if err != nil {
		writeTestResultTrace(w, "", err, trace)
		return
	}
	writeTestResultTrace(w, fmt.Sprintf("Zugriff ok — OData-Antwort mit %d Datensatz/Datensätzen erhalten.", len(page.Records)), nil, trace)
}
