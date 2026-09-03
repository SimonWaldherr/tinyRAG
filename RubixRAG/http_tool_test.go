package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestValidateHTTPQueryTemplatesAcceptsGoodTemplate(t *testing.T) {
	templates := []httpQueryTemplate{{
		Name:        "get_ticket",
		URLTemplate: "https://rubix.freshservice.com/api/v2/tickets/{ticket_id}",
		AuthSource:  "freshservice",
		Parameters: []sqlQueryParam{
			{Name: "ticket_id", Type: "integer", Required: true},
		},
		Enabled: true,
	}}
	s := appSettings{}
	s.Freshservice = []freshserviceConfig{{Enabled: true, BaseURL: "https://rubix.freshservice.com"}}
	if err := validateHTTPQueryTemplates(templates, s); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
}

func TestValidateHTTPQueryTemplatesAcceptsNoneAuthSource(t *testing.T) {
	templates := []httpQueryTemplate{{
		Name:        "lookup_public",
		URLTemplate: "https://example.com/api/{id}",
		AuthSource:  "none",
		Parameters:  []sqlQueryParam{{Name: "id", Type: "string", Required: true}},
		Enabled:     true,
	}}
	if err := validateHTTPQueryTemplates(templates, appSettings{}); err != nil {
		t.Fatalf("valid none-auth template rejected: %v", err)
	}
}

func TestValidateHTTPQueryTemplatesRejections(t *testing.T) {
	fsBaseURL := appSettings{}
	fsBaseURL.Freshservice = []freshserviceConfig{{Enabled: true, BaseURL: "https://rubix.freshservice.com"}}

	cases := []struct {
		name      string
		templates []httpQueryTemplate
		settings  appSettings
	}{
		{"invalid name", []httpQueryTemplate{{Name: "bad name!", URLTemplate: "https://x.com", AuthSource: "none"}}, appSettings{}},
		{"collides with built-in tool", []httpQueryTemplate{{Name: "search_shop_items", URLTemplate: "https://x.com", AuthSource: "none"}}, appSettings{}},
		{"duplicate name", []httpQueryTemplate{
			{Name: "t1", URLTemplate: "https://x.com/a", AuthSource: "none"},
			{Name: "t1", URLTemplate: "https://x.com/b", AuthSource: "none"},
		}, appSettings{}},
		{"non-get method", []httpQueryTemplate{{Name: "t1", Method: "POST", URLTemplate: "https://x.com", AuthSource: "none"}}, appSettings{}},
		{"unknown auth source", []httpQueryTemplate{{Name: "t1", URLTemplate: "https://x.com", AuthSource: "sharepoint"}}, appSettings{}},
		{"non-https url", []httpQueryTemplate{{Name: "t1", URLTemplate: "http://x.com", AuthSource: "none"}}, appSettings{}},
		{"auth source not configured", []httpQueryTemplate{{Name: "t1", URLTemplate: "https://rubix.freshservice.com/api", AuthSource: "freshservice"}}, appSettings{}},
		{"host mismatch with auth source", []httpQueryTemplate{{Name: "t1", URLTemplate: "https://evil.example.com/api", AuthSource: "freshservice"}}, fsBaseURL},
		{"unknown parameter type", []httpQueryTemplate{{
			Name: "t1", URLTemplate: "https://x.com/{id}", AuthSource: "none",
			Parameters: []sqlQueryParam{{Name: "id", Type: "wat"}},
		}}, appSettings{}},
		{"parameter not referenced in url", []httpQueryTemplate{{
			Name: "t1", URLTemplate: "https://x.com/fixed", AuthSource: "none",
			Parameters: []sqlQueryParam{{Name: "unused", Type: "string"}},
		}}, appSettings{}},
		{"url references undeclared parameter", []httpQueryTemplate{{
			Name: "t1", URLTemplate: "https://x.com/{id}", AuthSource: "none",
		}}, appSettings{}},
		{"duplicate parameter name", []httpQueryTemplate{{
			Name: "t1", URLTemplate: "https://x.com/{id}", AuthSource: "none",
			Parameters: []sqlQueryParam{{Name: "id", Type: "string"}, {Name: "id", Type: "integer"}},
		}}, appSettings{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateHTTPQueryTemplates(c.templates, c.settings); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestHTTPTemplateToolDefSchema(t *testing.T) {
	tmpl := httpQueryTemplate{
		Name:        "get_ticket",
		Description: "Fetch a Freshservice ticket by ID.",
		URLTemplate: "https://rubix.freshservice.com/api/v2/tickets/{ticket_id}",
		AuthSource:  "freshservice",
		Parameters: []sqlQueryParam{
			{Name: "ticket_id", Type: "integer", Required: true, Description: "The ticket's numeric ID."},
		},
	}
	def := httpTemplateToolDef(tmpl)
	if def.Function.Name != "get_ticket" {
		t.Fatalf("want tool name to match template name, got %q", def.Function.Name)
	}
	props, ok := def.Function.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("want a properties map, got %T", def.Function.Parameters["properties"])
	}
	prop, ok := props["ticket_id"].(map[string]any)
	if !ok || prop["type"] != "integer" {
		t.Fatalf("want ticket_id typed as integer, got %+v", props["ticket_id"])
	}
	required, _ := def.Function.Parameters["required"].([]string)
	if len(required) != 1 || required[0] != "ticket_id" {
		t.Fatalf("want ticket_id required, got %+v", required)
	}
}

func TestConvertHTTPTemplateParam(t *testing.T) {
	asRaw := func(v string) json.RawMessage { return json.RawMessage(v) }

	if v, err := convertHTTPTemplateParam("integer", asRaw(`42`)); err != nil || v != "42" {
		t.Fatalf("integer: got %v, %v", v, err)
	}
	if v, err := convertHTTPTemplateParam("boolean", asRaw(`true`)); err != nil || v != "true" {
		t.Fatalf("boolean: got %v, %v", v, err)
	}
	if v, err := convertHTTPTemplateParam("string", asRaw(`"hello world"`)); err != nil || v != "hello world" {
		t.Fatalf("string: got %v, %v", v, err)
	}
	if _, err := convertHTTPTemplateParam("integer", asRaw(`"not a number"`)); err == nil {
		t.Fatal("expected a type-mismatch error for integer parsed from a string")
	}
}

// TestResolveHTTPTemplateURLEscapesQueryPlaceholderSafely is a regression
// test for a real injection gap: a placeholder inside the query string used
// to be escaped with url.PathEscape (which leaves "&"/"=" untouched),
// letting a value like "4711&status=closed" inject a whole extra query
// parameter the admin's template never had. It must now come out fully
// percent-encoded via url.QueryEscape instead.
func TestResolveHTTPTemplateURLEscapesQueryPlaceholderSafely(t *testing.T) {
	got := resolveHTTPTemplateURL(
		"https://rubix.freshservice.com/api/v2/tickets?filter=all&ticket_id={ticket_id}",
		map[string]string{"ticket_id": "4711&status=closed"},
	)
	want := "https://rubix.freshservice.com/api/v2/tickets?filter=all&ticket_id=4711%26status%3Dclosed"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestResolveHTTPTemplateURLPathPlaceholderStillPathEscaped proves the fix
// didn't regress the ordinary path-segment case (e.g.
// http_tool_test.go's freshservice fixture): "/" in a supplied value must
// still be escaped so it can't add or remove a path segment.
func TestResolveHTTPTemplateURLPathPlaceholderStillPathEscaped(t *testing.T) {
	got := resolveHTTPTemplateURL(
		"https://rubix.freshservice.com/api/v2/tickets/{ticket_id}",
		map[string]string{"ticket_id": "../../admin"},
	)
	want := "https://rubix.freshservice.com/api/v2/tickets/..%2F..%2Fadmin"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestResolveHTTPTemplateURLMultiplePlaceholders proves a template mixing
// a path placeholder (before "?") and a query placeholder (after "?") gets
// each escaped by the correct rule, not one rule applied to both.
func TestResolveHTTPTemplateURLMultiplePlaceholders(t *testing.T) {
	got := resolveHTTPTemplateURL(
		"https://example.com/se16/{table}/lookup?id={id}",
		map[string]string{"table": "likp", "id": "4711&x=1"},
	)
	want := "https://example.com/se16/likp/lookup?id=4711%26x%3D1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractJSONPath(t *testing.T) {
	body := []byte(`{"tickets":[{"id":1,"status":"open"},{"id":2,"status":"closed"}],"meta":{"total":2}}`)

	if v, err := extractJSONPath(body, "meta.total"); err != nil || v != "2" {
		t.Fatalf("meta.total: got %q, %v", v, err)
	}
	if v, err := extractJSONPath(body, "tickets.1.status"); err != nil || v != `"closed"` {
		t.Fatalf("tickets.1.status (dot form): got %q, %v", v, err)
	}
	if v, err := extractJSONPath(body, "tickets[0].status"); err != nil || v != `"open"` {
		t.Fatalf("tickets[0].status (bracket form): got %q, %v", v, err)
	}
	if _, err := extractJSONPath(body, "tickets[9].status"); err == nil {
		t.Fatal("expected an error for an out-of-range index")
	}
	if _, err := extractJSONPath(body, "nope"); err == nil {
		t.Fatal("expected an error for a missing field")
	}
}

func TestHTTPTemplateToolExecutorMissingRequiredParam(t *testing.T) {
	tmpl := httpQueryTemplate{
		Name:        "get_ticket",
		URLTemplate: "https://example.com/api/{ticket_id}",
		AuthSource:  "none",
		Parameters: []sqlQueryParam{
			{Name: "ticket_id", Type: "integer", Required: true},
		},
	}
	exec := httpTemplateToolExecutor(tmpl, appSettings{})
	_, err := exec(nil, `{}`)
	if err == nil {
		t.Fatal("expected an error for a missing required parameter")
	}
}

func TestHTTPTemplateToolExecutorEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v2/tickets/4711") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Fatal("expected an Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ticket":{"id":4711,"status":"open"}}`))
	}))
	defer srv.Close()

	var s appSettings
	s.Freshservice = []freshserviceConfig{{Enabled: true, BaseURL: srv.URL, APIKey: "test-key"}}

	tmpl := httpQueryTemplate{
		Name:             "get_ticket",
		URLTemplate:      srv.URL + "/api/v2/tickets/{ticket_id}",
		AuthSource:       "freshservice",
		Parameters:       []sqlQueryParam{{Name: "ticket_id", Type: "integer", Required: true}},
		ResponseJSONPath: "ticket.status",
	}
	// The validity check (https-only, host match) is enforced at save time
	// (validateHTTPQueryTemplates), not at execution time, so an http://
	// httptest server works fine here for exercising the executor itself.
	exec := httpTemplateToolExecutor(tmpl, s)
	result, err := exec(t.Context(), `{"ticket_id": 4711}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `"open"` {
		t.Fatalf("want extracted status \"open\", got %q", result)
	}
}

// TestHTTPTemplateToolExecutorEndToEndBlocksQueryParamInjection is the
// full-stack regression test for the resolveHTTPTemplateURL fix: a model-
// supplied string parameter landing in the query string must never be able
// to inject an extra query parameter the admin's template didn't have —
// the server must see exactly the one "status" value the template asked
// for, never a smuggled-in second parameter.
func TestHTTPTemplateToolExecutorEndToEndBlocksQueryParamInjection(t *testing.T) {
	var sawRawQuery string
	var sawQueryValues url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRawQuery = r.URL.RawQuery
		sawQueryValues = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	var s appSettings
	tmpl := httpQueryTemplate{
		Name:        "search_tickets",
		URLTemplate: srv.URL + "/api/v2/tickets?filter=all&status={status}",
		AuthSource:  "none",
		Parameters:  []sqlQueryParam{{Name: "status", Type: "string", Required: true}},
	}
	exec := httpTemplateToolExecutor(tmpl, s)

	// A model (or a prompt-injected attacker instructing it) tries to smuggle
	// in an extra query parameter via the "status" value.
	if _, err := exec(t.Context(), `{"status": "open&admin=true"}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawQueryValues.Get("admin") != "" {
		t.Fatalf("injected admin=true query parameter reached the server: %q", sawRawQuery)
	}
	if got := sawQueryValues.Get("status"); got != "open&admin=true" {
		t.Fatalf("want the whole supplied value treated as one status, got %q (raw query: %q)", got, sawRawQuery)
	}
	if sawQueryValues.Get("filter") != "all" {
		t.Fatalf("want the template's own filter=all untouched, got query %q", sawRawQuery)
	}
}

func TestHTTPTemplateToolExecutorDoesNotFollowRedirects(t *testing.T) {
	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			redirected = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	exec := httpTemplateToolExecutor(httpQueryTemplate{
		Name:        "redirecting_lookup",
		URLTemplate: srv.URL + "/start",
		AuthSource:  "none",
	}, appSettings{})
	if _, err := exec(t.Context(), `{}`); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("want redirect response to be rejected, got %v", err)
	}
	if redirected {
		t.Fatal("HTTP template must not follow a redirect to a different request")
	}
}

func TestHTTPTemplateToolExecutorBoundsParameterAndResponseSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat("x", httpTemplateResponseMaxBytes+1)))
	}))
	defer srv.Close()

	exec := httpTemplateToolExecutor(httpQueryTemplate{
		Name:        "bounded_lookup",
		URLTemplate: srv.URL + "/items/{id}",
		AuthSource:  "none",
		Parameters:  []sqlQueryParam{{Name: "id", Type: "string", Required: true}},
	}, appSettings{})
	if _, err := exec(t.Context(), `{"id":"`+strings.Repeat("x", httpTemplateParamMaxBytes+1)+`"}`); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want oversized parameter rejection, got %v", err)
	}
	if _, err := exec(t.Context(), `{"id":"42"}`); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("want oversized response rejection, got %v", err)
	}
}

// TestHTTPTemplateToolExecutorInsecureSkipVerify reproduces the exact
// production failure ("tls: failed to verify certificate: x509:
// certificate signed by unknown authority" against an internal SAP se16
// gateway with a self-signed/internally-issued certificate): a template
// pointed at a TLS server whose certificate isn't in the trust store
// fails by default, and succeeds once InsecureSkipVerify is set — proving
// the opt-in actually takes effect, not just that it's threaded through.
func TestHTTPTemplateToolExecutorInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"MATNR":"123","MAKTX":"Testartikel"}`))
	}))
	defer srv.Close()

	var s appSettings
	tmpl := httpQueryTemplate{
		Name:        "sap_live",
		URLTemplate: srv.URL + "/se16/ZITEC/{table}/{number}",
		AuthSource:  "none",
		Parameters: []sqlQueryParam{
			{Name: "table", Type: "string"},
			{Name: "number", Type: "integer"},
		},
	}

	secureExec := httpTemplateToolExecutor(tmpl, s)
	if _, err := secureExec(t.Context(), `{"table":"mara","number":123}`); err == nil {
		t.Fatal("want a TLS verification error against a self-signed cert without InsecureSkipVerify")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("want a certificate-verification error, got: %v", err)
	}

	tmpl.InsecureSkipVerify = true
	insecureExec := httpTemplateToolExecutor(tmpl, s)
	out, err := insecureExec(t.Context(), `{"table":"mara","number":123}`)
	if err != nil {
		t.Fatalf("want the request to succeed with InsecureSkipVerify, got: %v", err)
	}
	if !strings.Contains(out, "Testartikel") {
		t.Fatalf("want the server's response body, got: %q", out)
	}
}
