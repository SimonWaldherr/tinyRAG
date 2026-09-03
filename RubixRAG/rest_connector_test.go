package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func restConn(name string) restConnectorConfig {
	return restConnectorConfig{connRuntime: connRuntime{Name: name}, Enabled: true, BaseURL: "https://logistic.rubix-intern.de"}
}

func TestValidateRESTConnectorsAcceptsGood(t *testing.T) {
	list := []restConnectorConfig{
		{connRuntime: connRuntime{Name: "sap-logistik"}, Enabled: true, BaseURL: "https://logistic.rubix-intern.de", AuthType: "none"},
		{connRuntime: connRuntime{Name: "erp_api"}, Enabled: true, BaseURL: "https://erp.example.com", AuthType: "bearer", Token: "x"},
	}
	if err := validateRESTConnectors(list); err != nil {
		t.Fatalf("valid REST connectors rejected: %v", err)
	}
}

func TestValidateRESTConnectorsRejections(t *testing.T) {
	cases := []struct {
		name string
		list []restConnectorConfig
	}{
		{"empty name", []restConnectorConfig{{connRuntime: connRuntime{Name: ""}, BaseURL: "https://x.com"}}},
		{"bad name char", []restConnectorConfig{{connRuntime: connRuntime{Name: "bad name!"}, BaseURL: "https://x.com"}}},
		{"collides with built-in", []restConnectorConfig{{connRuntime: connRuntime{Name: "jira"}, BaseURL: "https://x.com"}}},
		{"collides case-insensitively", []restConnectorConfig{{connRuntime: connRuntime{Name: "Freshservice"}, BaseURL: "https://x.com"}}},
		{"duplicate name", []restConnectorConfig{
			{connRuntime: connRuntime{Name: "sap"}, BaseURL: "https://a.com"},
			{connRuntime: connRuntime{Name: "SAP"}, BaseURL: "https://b.com"},
		}},
		{"missing base_url", []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}}}},
		{"non-https base_url", []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, BaseURL: "http://x.com"}}},
		{"bad auth_type", []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, BaseURL: "https://x.com", AuthType: "oauth2"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateRESTConnectors(c.list); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestRestConnectorByNameCaseInsensitive(t *testing.T) {
	s := appSettings{RESTConnectors: []restConnectorConfig{restConn("SAP-Logistik")}}
	if _, ok := restConnectorByName(s, "sap-logistik"); !ok {
		t.Fatal("expected case-insensitive match")
	}
	if _, ok := restConnectorByName(s, "  SAP-Logistik  "); !ok {
		t.Fatal("expected trimmed match")
	}
	if _, ok := restConnectorByName(s, "nope"); ok {
		t.Fatal("expected no match for unknown name")
	}
}

func TestValidateHTTPTemplateWithRESTConnector(t *testing.T) {
	s := appSettings{RESTConnectors: []restConnectorConfig{restConn("sap")}}

	good := []httpQueryTemplate{{
		Name:        "se16_mara_by_matnr",
		URLTemplate: "https://logistic.rubix-intern.de/se16/ZITEC/mara/{matnr}",
		AuthSource:  "sap",
		Parameters:  []sqlQueryParam{{Name: "matnr", Type: "string", Required: true}},
		Enabled:     true,
	}}
	if err := validateHTTPQueryTemplates(good, s); err != nil {
		t.Fatalf("valid REST-connector template rejected: %v", err)
	}

	// Host mismatch against the connector's base_url must be rejected (SSRF guard).
	bad := []httpQueryTemplate{{
		Name:        "evil",
		URLTemplate: "https://evil.example.com/se16/ZITEC/mara/{matnr}",
		AuthSource:  "sap",
		Parameters:  []sqlQueryParam{{Name: "matnr", Type: "string", Required: true}},
	}}
	if err := validateHTTPQueryTemplates(bad, s); err == nil {
		t.Fatal("expected host-mismatch rejection for a REST-connector template")
	}

	// An auth_source naming no configured connector (and not a built-in) is unknown.
	unknown := []httpQueryTemplate{{
		Name:        "t",
		URLTemplate: "https://logistic.rubix-intern.de/x/{id}",
		AuthSource:  "does-not-exist",
		Parameters:  []sqlQueryParam{{Name: "id", Type: "string"}},
	}}
	if err := validateHTTPQueryTemplates(unknown, s); err == nil {
		t.Fatal("expected unknown-auth-source rejection")
	}
}

func TestApplyHTTPTemplateAuthModes(t *testing.T) {
	newReq := func() *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "https://logistic.rubix-intern.de/x", nil)
		return r
	}

	t.Run("none built-in sets no auth", func(t *testing.T) {
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "none", appSettings{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("none must not set an Authorization header")
		}
	})

	t.Run("empty auth_source treated as none", func(t *testing.T) {
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "", appSettings{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rest connector none only pins host", func(t *testing.T) {
		s := appSettings{RESTConnectors: []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://logistic.rubix-intern.de", AuthType: "none"}}}
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "sap", s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("auth_type none must not set Authorization")
		}
	})

	t.Run("basic", func(t *testing.T) {
		s := appSettings{RESTConnectors: []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://x", AuthType: "basic", Username: "u", Password: "p"}}}
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "sap", s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
			t.Fatalf("want Basic auth, got %q", got)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		s := appSettings{RESTConnectors: []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://x", AuthType: "bearer", Token: "tok123"}}}
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "sap", s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Fatalf("want bearer token, got %q", got)
		}
	})

	t.Run("header with custom name and static headers", func(t *testing.T) {
		s := appSettings{RESTConnectors: []restConnectorConfig{{
			connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://x",
			AuthType: "header", HeaderName: "X-API-Key", Token: "abc",
			Headers: map[string]string{"X-System": "ZITEC"},
		}}}
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "sap", s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("X-API-Key"); got != "abc" {
			t.Fatalf("want X-API-Key header, got %q", got)
		}
		if got := req.Header.Get("X-System"); got != "ZITEC" {
			t.Fatalf("want static X-System header, got %q", got)
		}
	})

	t.Run("header auth defaults to Authorization", func(t *testing.T) {
		s := appSettings{RESTConnectors: []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://x", AuthType: "header", Token: "raw"}}}
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "sap", s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "raw" {
			t.Fatalf("want raw Authorization value, got %q", got)
		}
	})

	t.Run("disabled connector errors", func(t *testing.T) {
		s := appSettings{RESTConnectors: []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, Enabled: false, BaseURL: "https://x", AuthType: "none"}}}
		if err := applyHTTPTemplateAuth(newReq(), "sap", s); err == nil {
			t.Fatal("expected an error for a disabled connector")
		}
	})

	t.Run("missing credentials error", func(t *testing.T) {
		s := appSettings{RESTConnectors: []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://x", AuthType: "bearer"}}}
		if err := applyHTTPTemplateAuth(newReq(), "sap", s); err == nil {
			t.Fatal("expected an error for a bearer connector with no token")
		}
	})

	t.Run("token from env var", func(t *testing.T) {
		t.Setenv("R3_TEST_SAP_TOKEN", "envtok")
		s := appSettings{RESTConnectors: []restConnectorConfig{{connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://x", AuthType: "bearer", TokenEnv: "R3_TEST_SAP_TOKEN"}}}
		req := newReq()
		if err := applyHTTPTemplateAuth(req, "sap", s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer envtok" {
			t.Fatalf("want token resolved from env, got %q", got)
		}
	})
}

func TestMergeRESTConnPreservesMaskedSecrets(t *testing.T) {
	cur := restConnectorConfig{
		connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://logistic.rubix-intern.de",
		AuthType: "basic", Username: "u", Password: "realpw", Token: "realtok",
		Headers: map[string]string{"X-System": "ZITEC"},
	}
	patch := restConnectorConfig{
		connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://logistic.rubix-intern.de",
		AuthType: "basic", Username: "u", Password: "***set***", Token: "newtok",
		Headers: nil,
	}
	merged := mergeRESTConn(cur, patch)
	if merged.Password != "realpw" {
		t.Fatalf("masked password must be preserved, got %q", merged.Password)
	}
	if merged.Token != "newtok" {
		t.Fatalf("a real new token must overwrite, got %q", merged.Token)
	}
	if merged.Headers != nil {
		t.Fatalf("headers must be replaced wholesale (cleared), got %+v", merged.Headers)
	}
}

func TestHTTPTemplateExecutorViaRESTConnectorEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/se16/ZITEC/mara/000000000000000345") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sap-token" {
			t.Fatalf("expected bearer auth, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-System") != "ZITEC" {
			t.Fatalf("expected static X-System header, got %q", r.Header.Get("X-System"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"MATNR":"000000000000000345","MAKTX":"DIN472 J 125X4 FST PH"}]`))
	}))
	defer srv.Close()

	s := appSettings{RESTConnectors: []restConnectorConfig{{
		connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: srv.URL,
		AuthType: "bearer", Token: "sap-token",
		Headers: map[string]string{"X-System": "ZITEC"},
	}}}
	tmpl := httpQueryTemplate{
		Name:        "se16_mara_by_matnr",
		URLTemplate: srv.URL + "/se16/ZITEC/mara/{matnr}",
		AuthSource:  "sap",
		Parameters:  []sqlQueryParam{{Name: "matnr", Type: "string", Required: true}},
	}
	exec := httpTemplateToolExecutor(tmpl, s)
	result, err := exec(t.Context(), `{"matnr": "000000000000000345"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "DIN472 J 125X4 FST PH") {
		t.Fatalf("want the SAP article description in the result, got %q", result)
	}
}
