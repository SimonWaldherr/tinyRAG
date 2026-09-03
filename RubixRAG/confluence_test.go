package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testConfluenceConfig(baseURL string) confluenceConfig {
	return confluenceConfig{
		Enabled: true, BaseURL: baseURL, Email: "bot@rubix.com", APIToken: "tok-123",
		SpaceKey: "VERTRIEB",
	}
}

func TestConfluenceAuthAndPreview(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/rest/api/content") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("spaceKey") != "VERTRIEB" {
			t.Errorf("want spaceKey=VERTRIEB in query, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results": [
			{"id": "111", "title": "Onboarding", "version": {"when": "2026-01-05T10:00:00Z", "number": 3}}
		]}`))
	}))
	defer server.Close()

	cfg := testConfluenceConfig(server.URL + "/wiki")
	res, err := previewConfluencePages(context.Background(), cfg, 50)
	if err != nil {
		t.Fatalf("previewConfluencePages: %v", err)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@rubix.com:tok-123"))
	if gotAuth != wantAuth {
		t.Fatalf("want Basic auth header %q, got %q", wantAuth, gotAuth)
	}
	if len(res.Items) != 1 || res.Items[0].ID != "111" || res.Items[0].Title != "Onboarding" {
		t.Fatalf("unexpected items: %+v", res.Items)
	}
}

func TestConfluenceResolvedTokenPrefersEnv(t *testing.T) {
	t.Setenv("R3_TEST_CONF_TOKEN", "env-token")
	cfg := confluenceConfig{APIToken: "inline-token", APITokenEnv: "R3_TEST_CONF_TOKEN"}
	if got := confResolvedToken(cfg); got != "env-token" {
		t.Fatalf("want env-token, got %q", got)
	}
	cfg = confluenceConfig{APIToken: "inline-token"}
	if got := confResolvedToken(cfg); got != "inline-token" {
		t.Fatalf("want inline-token when no env var configured, got %q", got)
	}
}

func TestImportConfluencePages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rest/api/content/111") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": "111", "title": "Onboarding",
			"body": {"storage": {"value": "<h1>Welcome</h1><p>This page explains onboarding steps in enough detail to be chunked.</p>"}},
			"version": {"when": "2026-01-05T10:00:00Z"}}`))
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	cfg := testConfluenceConfig(server.URL + "/wiki")
	s.Confluence = []confluenceConfig{cfg}

	res, err := importConfluencePages(context.Background(), rag, s, cfg, "test-embed", map[string]bool{"111": true}, false, nil)
	if err != nil {
		t.Fatalf("importConfluencePages: %v", err)
	}
	if res.Pages != 1 {
		t.Fatalf("want 1 page, got %d", res.Pages)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", res.Errors)
	}
	if res.Chunks == 0 {
		t.Fatalf("expected at least one chunk to be ingested")
	}

	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	found := false
	for _, src := range sources {
		if src.SourceID == "confluence:VERTRIEB:111" {
			found = true
			if src.SourceKind != "confluence_page" {
				t.Errorf("want source_kind confluence_page, got %s", src.SourceKind)
			}
		}
	}
	if !found {
		t.Fatalf("expected source confluence:VERTRIEB:111 to have been ingested, got %+v", sources)
	}
}
