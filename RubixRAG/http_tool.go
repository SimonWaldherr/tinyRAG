package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Generic HTTP query-template tool — the MCP-style analogue of mssql.go's
// SQL query templates, but for REST APIs (docs/MCP_CONNECTORS_PLAN.md
// section (A)): instead of a new Go type/tool per live lookup, an admin
// authors a named, typed, parameterized GET request that borrows an
// already-configured connector's own credentials. Every enabled template
// becomes its own function-calling tool with a typed parameter schema
// (mirroring mssqlTemplateToolDef); the model can only ever run the exact
// URL an admin wrote, with model-supplied values substituted into "{name}"
// placeholders — never a model-composed URL.
// ─────────────────────────────────────────────────────────────────────────────

// httpTemplateAuthSources lists which existing connector configs a template
// can borrow credentials from. SharePoint/Exchange (OAuth2 client-
// credentials, graph.go's token cache) are deliberately not included yet —
// the Basic-auth connectors are the simple, already-solved case; an OAuth2
// AuthSource would need its own token-acquisition wiring, left for when a
// concrete use case actually needs it.
var httpTemplateAuthSources = map[string]bool{
	"none":         true,
	"confluence":   true,
	"jira":         true,
	"freshservice": true,
}

var httpTemplatePlaceholderRe = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// restConnectorNameRe restricts REST-connector names — they become the
// auth_source token an HTTP template references, so keep them to a clean
// identifier (letters/digits/underscore/hyphen). Slightly more permissive
// than sqlTemplateNameRe (which must be a valid function/tool name) because a
// connector name is never itself a tool name.
var restConnectorNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

// restConnectorByName resolves a generic REST connector by name
// (case-insensitive, so an auth_source "SAP-Logistik" matches a connector
// named "sap-logistik"). Returns any match regardless of Enabled — the
// enabled check happens at call time in applyHTTPTemplateAuth so a template
// can still be saved/validated against a temporarily disabled connector.
func restConnectorByName(s appSettings, name string) (restConnectorConfig, bool) {
	name = strings.TrimSpace(name)
	for _, c := range s.RESTConnectors {
		if strings.EqualFold(strings.TrimSpace(c.Name), name) {
			return c, true
		}
	}
	return restConnectorConfig{}, false
}

// validateRESTConnectors is the save-time check for appSettings.RESTConnectors
// (called from handleSettings before validateHTTPQueryTemplates, since a
// template's auth_source may reference one): a clean unique name that doesn't
// collide with a built-in auth source, an https base_url, and a known
// auth_type. Credential presence is NOT required here — a connector may be
// configured before its secret env var is set — it's enforced at call time in
// applyHTTPTemplateAuth instead, same fail-at-use posture as the built-in
// auth sources.
func validateRESTConnectors(list []restConnectorConfig) error {
	seen := map[string]bool{}
	for i, c := range list {
		name := strings.TrimSpace(c.Name)
		if !restConnectorNameRe.MatchString(name) {
			return fmt.Errorf("REST connector %d: name %q must match %s (letters/digits/underscore/hyphen, starting with a letter)", i, c.Name, restConnectorNameRe.String())
		}
		lc := strings.ToLower(name)
		if httpTemplateAuthSources[lc] {
			return fmt.Errorf("REST connector %q: name collides with the built-in auth source %q — pick a different name", name, lc)
		}
		if seen[lc] {
			return fmt.Errorf("REST connector %d: duplicate name %q (names are case-insensitive)", i, name)
		}
		seen[lc] = true

		base := strings.TrimSpace(c.BaseURL)
		if base == "" {
			return fmt.Errorf("REST connector %q: base_url is required", name)
		}
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("REST connector %q: base_url must be a valid https URL (e.g. https://logistic.rubix-intern.de)", name)
		}
		switch strings.ToLower(strings.TrimSpace(c.AuthType)) {
		case "", "none", "basic", "bearer", "header":
		default:
			return fmt.Errorf("REST connector %q: unknown auth_type %q (want none|basic|bearer|header)", name, c.AuthType)
		}
	}
	return nil
}

// validateHTTPQueryTemplates rejects a template set with a name collision
// (with another template or a built-in tool name), a non-GET method, an
// unknown auth_source, a url_template whose host doesn't match its
// auth_source connector's own configured base_url (SSRF guard — the model
// only ever fills in placeholder values, never the domain, but an admin
// could still paste the wrong base host), or a parameter that isn't
// referenced in url_template (or vice versa) — checked once at save time,
// same reasoning as mssql.go's validateSQLQueryTemplates. s is the
// about-to-be-saved settings snapshot (not the templates' own struct) so
// the auth_source host check has something to check against.
func validateHTTPQueryTemplates(templates []httpQueryTemplate, s appSettings) error {
	seen := map[string]bool{}
	for i, t := range templates {
		name := strings.TrimSpace(t.Name)
		if !sqlTemplateNameRe.MatchString(name) {
			return fmt.Errorf("http template %d: name %q must match %s (letters/digits/underscore, starting with a letter or underscore)", i, t.Name, sqlTemplateNameRe.String())
		}
		if name == mssqlToolName || name == shopSearchToolName {
			return fmt.Errorf("http template %d: name %q collides with a built-in tool", i, name)
		}
		if seen[name] {
			return fmt.Errorf("http template %d: duplicate name %q", i, name)
		}
		seen[name] = true

		method := strings.ToUpper(strings.TrimSpace(t.Method))
		if method == "" {
			method = "GET"
		}
		if method != "GET" {
			return fmt.Errorf("http template %q: only GET is supported today", name)
		}

		// auth_source is one of the built-in Basic-auth connectors
		// (none|confluence|jira|freshservice, case-insensitive) OR the name of
		// a configured generic REST connector (case-insensitive, so keep the
		// raw form for that lookup and only lowercase for the built-in check).
		authSourceRaw := strings.TrimSpace(t.AuthSource)
		if authSourceRaw == "" {
			authSourceRaw = "none"
		}
		authSourceLC := strings.ToLower(authSourceRaw)
		_, isREST := restConnectorByName(s, authSourceRaw)
		if !httpTemplateAuthSources[authSourceLC] && !isREST {
			return fmt.Errorf("http template %q: unknown auth_source %q (want none|confluence|jira|freshservice or the name of a configured REST connector)", name, t.AuthSource)
		}

		urlTemplate := strings.TrimSpace(t.URLTemplate)
		if urlTemplate == "" {
			return fmt.Errorf("http template %q: url_template is required", name)
		}
		parsed, err := url.Parse(urlTemplate)
		if err != nil {
			return fmt.Errorf("http template %q: invalid url_template: %w", name, err)
		}
		if parsed.Scheme != "https" {
			return fmt.Errorf("http template %q: url_template must use https", name)
		}
		// Every non-"none" auth_source pins the host: the built-in connectors
		// via their configured base_url, a REST connector via its BaseURL
		// (which validateRESTConnectors already required). A REST connector
		// with auth_type "none" still pins the host this way — that's the
		// point of referencing it rather than the bare "none" source.
		if authSourceLC != "none" {
			baseURL := httpTemplateAuthSourceBaseURL(authSourceRaw, s)
			if strings.TrimSpace(baseURL) == "" {
				return fmt.Errorf("http template %q: auth_source %q is not configured (missing base_url)", name, t.AuthSource)
			}
			base, err := url.Parse(baseURL)
			if err != nil || !strings.EqualFold(parsed.Host, base.Host) {
				return fmt.Errorf("http template %q: url_template host %q must match the configured %s base_url host", name, parsed.Host, t.AuthSource)
			}
		}

		declared := map[string]bool{}
		for j, p := range t.Parameters {
			pname := strings.TrimSpace(p.Name)
			if pname == "" {
				return fmt.Errorf("http template %q, parameter %d: empty name", name, j)
			}
			if declared[pname] {
				return fmt.Errorf("http template %q: duplicate parameter %q", name, pname)
			}
			declared[pname] = true
			switch p.Type {
			case "string", "integer", "number", "boolean", "date":
			default:
				return fmt.Errorf("http template %q, parameter %q: unknown type %q (want string|integer|number|boolean|date)", name, pname, p.Type)
			}
			if err := validateTemplateParamOptions(p.Options); err != nil {
				return fmt.Errorf("http template %q, parameter %q: %w", name, pname, err)
			}
			if !strings.Contains(urlTemplate, "{"+pname+"}") {
				return fmt.Errorf("http template %q: parameter %q is declared but never referenced in url_template as {%s}", name, pname, pname)
			}
		}
		for _, ref := range httpTemplatePlaceholderRe.FindAllStringSubmatch(urlTemplate, -1) {
			if !declared[ref[1]] {
				return fmt.Errorf("http template %q: url_template references {%s}, which isn't declared as a parameter", name, ref[1])
			}
		}
	}
	return nil
}

// httpTemplateAuthSourceBaseURL resolves the configured base URL for one
// auth source, "" if not configured — used both by validation (SSRF host
// check) and execution (credential lookup below). Accepts either a built-in
// source (case-insensitive) or a generic REST connector name.
func httpTemplateAuthSourceBaseURL(authSource string, s appSettings) string {
	// firstEnabledConn: an HTTP template's built-in auth_source predates
	// multi-instance connections and has no per-template way to pick among
	// several — same "first enabled" fallback as agent.go's mail-draft
	// tool, see connruntime.go's doc comment.
	switch strings.ToLower(strings.TrimSpace(authSource)) {
	case "confluence":
		if c, ok := firstEnabledConn(s.Confluence); ok {
			return c.BaseURL
		}
	case "jira":
		if c, ok := firstEnabledConn(s.Jira); ok {
			return c.BaseURL
		}
	case "freshservice":
		if c, ok := firstEnabledConn(s.Freshservice); ok {
			return c.BaseURL
		}
	}
	// Not a built-in (or that built-in has no enabled connection): try a
	// generic REST connector by name. A REST connector's name can never
	// collide with a built-in (validateRESTConnectors rejects that), so this
	// order is unambiguous.
	if c, ok := restConnectorByName(s, authSource); ok {
		return c.BaseURL
	}
	return ""
}

// httpTemplateAuthHeader resolves the Authorization header value for one
// auth source, reusing each connector's own resolved-credential helper
// (confResolvedToken/jiraResolvedToken/freshserviceResolvedAPIKey) so a
// template never needs its own copy of a connector's credentials — "" (no
// header) for "none".
func httpTemplateAuthHeader(authSource string, s appSettings) (string, error) {
	switch authSource {
	case "none":
		return "", nil
	case "confluence":
		conn, connOK := firstEnabledConn(s.Confluence)
		token := confResolvedToken(conn)
		if !connOK || conn.Email == "" || token == "" {
			return "", fmt.Errorf("confluence email/api_token not configured")
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(conn.Email+":"+token)), nil
	case "jira":
		conn, connOK := firstEnabledConn(s.Jira)
		token := jiraResolvedToken(conn)
		if !connOK || conn.Email == "" || token == "" {
			return "", fmt.Errorf("jira email/api_token not configured")
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(conn.Email+":"+token)), nil
	case "freshservice":
		conn, connOK := firstEnabledConn(s.Freshservice)
		key := freshserviceResolvedAPIKey(conn)
		if !connOK || key == "" {
			return "", fmt.Errorf("freshservice api_key not configured")
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(key+":X")), nil
	default:
		return "", fmt.Errorf("unknown auth_source %q", authSource)
	}
}

// applyHTTPTemplateAuth attaches whatever authentication (and, for a generic
// REST connector, any static headers) an auth_source requires to req. It is
// the single execution-time credential path for the HTTP live tool, covering
// both the built-in Basic-auth connectors (via httpTemplateAuthHeader) and
// the generic restConnectorConfig sources. Errors fail the tool call rather
// than silently sending an unauthenticated request. Called by
// httpTemplateToolExecutor after the default Accept/User-Agent headers are
// set, so a REST connector's own Headers can deliberately override them.
func applyHTTPTemplateAuth(req *http.Request, authSource string, s appSettings) error {
	src := strings.TrimSpace(authSource)
	if src == "" {
		src = "none"
	}
	if httpTemplateAuthSources[strings.ToLower(src)] {
		authHeader, err := httpTemplateAuthHeader(strings.ToLower(src), s)
		if err != nil {
			return err
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		return nil
	}

	// Generic REST connector referenced by name.
	conn, ok := restConnectorByName(s, src)
	if !ok {
		return fmt.Errorf("unknown auth_source %q", authSource)
	}
	if !conn.Enabled {
		return fmt.Errorf("REST connector %q is not enabled", conn.Name)
	}
	// Static headers first, so the auth header below always wins over an
	// accidental "Authorization" entry here.
	for k, v := range conn.Headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	switch strings.ToLower(strings.TrimSpace(conn.AuthType)) {
	case "", "none":
		// Host pinning only, no credential attached.
	case "basic":
		pw := conn.resolvedPassword()
		if conn.Username == "" || pw == "" {
			return fmt.Errorf("REST connector %q: basic auth needs username and password", conn.Name)
		}
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(conn.Username+":"+pw)))
	case "bearer":
		tok := conn.resolvedToken()
		if tok == "" {
			return fmt.Errorf("REST connector %q: bearer auth needs a token", conn.Name)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case "header":
		tok := conn.resolvedToken()
		if tok == "" {
			return fmt.Errorf("REST connector %q: header auth needs a token", conn.Name)
		}
		headerName := strings.TrimSpace(conn.HeaderName)
		if headerName == "" {
			headerName = "Authorization"
		}
		req.Header.Set(headerName, tok)
	default:
		return fmt.Errorf("REST connector %q: unknown auth_type %q", conn.Name, conn.AuthType)
	}
	return nil
}

// httpTemplateToolDef describes one HTTP query template as an OpenAI-style
// function-calling tool — same shape as mssql.go's mssqlTemplateToolDef,
// just with a URL-placeholder parameter schema instead of a SQL one.
func httpTemplateToolDef(t httpQueryTemplate) toolDef {
	desc, props, required := queryTemplateToolSchema(t.Description, t.Parameters, t.ResultHint)
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name:        t.Name,
			Description: desc,
			Parameters: map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

// httpTemplateResultChars caps how much of a template's (possibly
// JSON-path-narrowed) response text reaches the model — same context-
// window reasoning as agent.go's agentSourceContentChars.
const (
	httpTemplateResultChars         = 6000
	httpTemplateResponseMaxBytes    = 1 << 20
	httpTemplateParamMaxBytes       = 4096
	httpTemplateResolvedURLMaxBytes = 8192
)

// convertHTTPTemplateParam decodes one JSON argument value into its string
// form for URL-placeholder substitution — unlike mssql.go's
// convertSQLTemplateParam, everything ends up as a string here since a URL
// has no native typed-parameter binding; Type still governs how strictly
// the incoming JSON value is validated before that.
func convertHTTPTemplateParam(paramType string, raw json.RawMessage) (string, error) {
	switch paramType {
	case "integer":
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", fmt.Errorf("expected an integer: %w", err)
		}
		return strconv.FormatInt(n, 10), nil
	case "number":
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", fmt.Errorf("expected a number: %w", err)
		}
		return strconv.FormatFloat(n, 'g', -1, 64), nil
	case "boolean":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", fmt.Errorf("expected a boolean: %w", err)
		}
		return strconv.FormatBool(b), nil
	default: // "string", "date" — passed through as text
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("expected a string: %w", err)
		}
		return s, nil
	}
}

// extractJSONPath pulls one field out of a JSON response body via a small
// dot-path syntax ("data.items", "items.0.name" and "items[0].name" both
// accepted — brackets are normalized to dots) — enough for "just the list
// of tickets, not the whole envelope" without needing real JSONPath/jq
// (see docs/MCP_CONNECTORS_PLAN.md's open question 1). Returns the
// extracted value re-marshaled as compact JSON text; errors if the path
// doesn't resolve (e.g. the upstream API's response shape changed).
func extractJSONPath(body []byte, path string) (string, error) {
	var cur any
	if err := json.Unmarshal(body, &cur); err != nil {
		return "", fmt.Errorf("parse response as JSON: %w", err)
	}
	normalized := strings.NewReplacer("[", ".", "]", "").Replace(path)
	for _, seg := range strings.Split(normalized, ".") {
		if seg == "" {
			continue
		}
		if idx, err := strconv.Atoi(seg); err == nil {
			arr, ok := cur.([]any)
			if !ok || idx < 0 || idx >= len(arr) {
				return "", fmt.Errorf("response_json_path %q: index %d not found", path, idx)
			}
			cur = arr[idx]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("response_json_path %q: %q is not an object", path, seg)
		}
		v, ok := m[seg]
		if !ok {
			return "", fmt.Errorf("response_json_path %q: field %q not found", path, seg)
		}
		cur = v
	}
	out, err := json.Marshal(cur)
	if err != nil {
		return "", fmt.Errorf("re-encode extracted value: %w", err)
	}
	return string(out), nil
}

// resolveHTTPTemplateURL substitutes each "{name}" placeholder in
// urlTemplate with values[name], escaped for exactly where that occurrence
// sits — never a blanket url.PathEscape regardless of position, which used
// to let a placeholder inside the query string inject an extra parameter:
// PathEscape leaves "&" and "=" untouched (they're valid literal characters
// within a single path segment), so a model-supplied value like
// "4711&status=closed" landing in "...?ticket_id={ticket_id}" widened the
// request into "...?ticket_id=4711&status=closed" — a second, attacker/
// model-controlled query parameter the admin's template never had. Every
// occurrence at or after urlTemplate's own first literal "?" is now
// escaped with url.QueryEscape (which does encode "&"/"="/"+" correctly
// for a query string); everything before it keeps url.PathEscape. A
// placeholder can never land in the host/scheme instead: validateHTTPQueryTemplates
// already requires url_template to parse as a valid https URL and pins its
// host to the referenced auth_source's own configured base_url before a
// template can ever be saved, so "{name}" literal text in the host
// position would already fail that host-equality check.
func resolveHTTPTemplateURL(urlTemplate string, values map[string]string) string {
	queryStart := strings.IndexByte(urlTemplate, '?')
	matches := httpTemplatePlaceholderRe.FindAllStringSubmatchIndex(urlTemplate, -1)
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end, nameStart, nameEnd := m[0], m[1], m[2], m[3]
		b.WriteString(urlTemplate[last:start])
		name := urlTemplate[nameStart:nameEnd]
		if queryStart >= 0 && start >= queryStart {
			b.WriteString(url.QueryEscape(values[name]))
		} else {
			b.WriteString(url.PathEscape(values[name]))
		}
		last = end
	}
	b.WriteString(urlTemplate[last:])
	return b.String()
}

// httpTemplateToolExecutor adapts one httpQueryTemplate to the generic
// toolExecutor shape: decode the model's JSON arguments, substitute each
// declared parameter into its "{name}" placeholder via resolveHTTPTemplateURL
// (context-appropriately escaped, never string-concatenated raw), resolve
// auth from the referenced connector, and run the GET request via
// connector.go's shared retrying client.
func httpTemplateToolExecutor(tmpl httpQueryTemplate, s appSettings) toolExecutor {
	return func(ctx context.Context, argsJSON string) (string, error) {
		var raw map[string]json.RawMessage
		if strings.TrimSpace(argsJSON) != "" {
			if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
				return "", fmt.Errorf("invalid tool arguments: %w", err)
			}
		}
		values := map[string]string{}
		for _, p := range tmpl.Parameters {
			v, ok := raw[p.Name]
			if !ok || string(v) == "null" {
				if p.Required {
					return "", fmt.Errorf("missing required parameter %q", p.Name)
				}
				continue
			}
			str, err := convertHTTPTemplateParam(p.Type, v)
			if err != nil {
				return "", fmt.Errorf("parameter %q: %w", p.Name, err)
			}
			if len(str) > httpTemplateParamMaxBytes {
				return "", fmt.Errorf("parameter %q exceeds %d bytes", p.Name, httpTemplateParamMaxBytes)
			}
			if len(p.Options) > 0 && !templateParamOptionMatches(p.Options, str) {
				return "", fmt.Errorf("parameter %q: %q is not one of the allowed values (%s)", p.Name, str, strings.Join(p.Options, ", "))
			}
			values[p.Name] = str
		}

		resolvedURL := resolveHTTPTemplateURL(tmpl.URLTemplate, values)
		if len(resolvedURL) > httpTemplateResolvedURLMaxBytes {
			return "", fmt.Errorf("http tool %q: resolved URL exceeds %d bytes", tmpl.Name, httpTemplateResolvedURLMaxBytes)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolvedURL, nil)
		if err != nil {
			return "", fmt.Errorf("http tool %q: build request: %w", tmpl.Name, err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", connectorUserAgent)
		// Auth (and any REST-connector static headers) last, so a connector
		// can intentionally override the defaults above for a picky backend.
		if err := applyHTTPTemplateAuth(req, tmpl.AuthSource, s); err != nil {
			return "", fmt.Errorf("http tool %q: %w", tmpl.Name, err)
		}

		body, err := doWithRetryLimitedNoRedirect(req, tmpl.InsecureSkipVerify, httpTemplateResponseMaxBytes)
		if err != nil {
			return "", fmt.Errorf("http tool %q: request failed: %w", tmpl.Name, err)
		}

		result := string(body)
		if strings.TrimSpace(tmpl.ResponseJSONPath) != "" {
			extracted, err := extractJSONPath(body, tmpl.ResponseJSONPath)
			if err != nil {
				return "", fmt.Errorf("http tool %q: %w", tmpl.Name, err)
			}
			result = extracted
		}
		return truncateRunesNote(strings.TrimSpace(result), httpTemplateResultChars), nil
	}
}
