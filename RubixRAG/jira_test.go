package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testJiraConfig(baseURL string) jiraConfig {
	return jiraConfig{
		Enabled: true, BaseURL: baseURL, Email: "bot@rubix.com", APIToken: "tok-123",
		ProjectKey: "OPS",
	}
}

func TestJiraAuthAndPreview(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/rest/api/2/search") {
			http.NotFound(w, r)
			return
		}
		if !strings.Contains(r.URL.Query().Get("jql"), "project=OPS") {
			t.Errorf("want jql containing project=OPS, got %q", r.URL.Query().Get("jql"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues": [
			{"key": "OPS-1", "fields": {"summary": "Drucker kaputt", "updated": "2026-01-05T10:00:00.000+0100", "status": {"name": "Offen"}}}
		]}`))
	}))
	defer server.Close()

	cfg := testJiraConfig(server.URL)
	res, err := previewJiraIssues(context.Background(), cfg, 50)
	if err != nil {
		t.Fatalf("previewJiraIssues: %v", err)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@rubix.com:tok-123"))
	if gotAuth != wantAuth {
		t.Fatalf("want Basic auth header %q, got %q", wantAuth, gotAuth)
	}
	if len(res.Items) != 1 || res.Items[0].Key != "OPS-1" || res.Items[0].Summary != "Drucker kaputt" {
		t.Fatalf("unexpected items: %+v", res.Items)
	}
}

func TestJiraResolvedTokenPrefersEnv(t *testing.T) {
	t.Setenv("R3_TEST_JIRA_TOKEN", "env-token")
	cfg := jiraConfig{APIToken: "inline-token", APITokenEnv: "R3_TEST_JIRA_TOKEN"}
	if got := jiraResolvedToken(cfg); got != "env-token" {
		t.Fatalf("want env-token, got %q", got)
	}
	cfg = jiraConfig{APIToken: "inline-token"}
	if got := jiraResolvedToken(cfg); got != "inline-token" {
		t.Fatalf("want inline-token when no env var configured, got %q", got)
	}
}

func TestImportJiraIssues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rest/api/2/issue/OPS-1") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key": "OPS-1", "fields": {
			"summary": "Drucker kaputt", "status": {"name": "Offen"},
			"updated": "2026-01-05T10:00:00.000+0100",
			"description": "Der Drucker im zweiten Stock zieht kein Papier mehr ein, Fehlermeldung E-42."
		}}`))
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	cfg := testJiraConfig(server.URL)
	s.Jira = []jiraConfig{cfg}

	res, err := importJiraIssues(context.Background(), rag, s, cfg, "test-embed", map[string]bool{"OPS-1": true}, false, nil)
	if err != nil {
		t.Fatalf("importJiraIssues: %v", err)
	}
	if res.Issues != 1 {
		t.Fatalf("want 1 issue, got %d", res.Issues)
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
		if src.SourceID == "jira:OPS:OPS-1" {
			found = true
			if src.SourceKind != "jira_issue" {
				t.Errorf("want source_kind jira_issue, got %s", src.SourceKind)
			}
		}
	}
	if !found {
		t.Fatalf("expected source jira:OPS:OPS-1 to have been ingested, got %+v", sources)
	}
}
