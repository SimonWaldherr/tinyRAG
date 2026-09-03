package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newFakeGraphServer stands up a single httptest server that answers both
// the OAuth2 token endpoint and arbitrary Graph API GETs, and points
// graphAuthHost/graphBaseURL at it for the duration of the test (see
// graph.go's doc comment: these are vars, not consts, specifically so
// tests can do this — there's no real Azure AD tenant reachable here).
// apiHandler serves everything under the fake Graph base URL.
func newFakeGraphServer(t *testing.T, apiHandler http.HandlerFunc) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/v2.0/token") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "fake-token",
				"expires_in":   3600,
			})
			return
		}
		apiHandler(w, r)
	})
	server := httptest.NewServer(mux)

	origBase, origAuth := graphBaseURL, graphAuthHost
	graphBaseURL = server.URL
	graphAuthHost = server.URL
	graphTokensMu.Lock()
	graphTokens = map[string]*graphTokenCache{}
	graphTokensMu.Unlock()

	t.Cleanup(func() {
		server.Close()
		graphBaseURL, graphAuthHost = origBase, origAuth
		graphTokensMu.Lock()
		graphTokens = map[string]*graphTokenCache{}
		graphTokensMu.Unlock()
	})
}

func testTeamsConfig() teamsConfig {
	return teamsConfig{
		Enabled: true, TenantID: "tenant", ClientID: "client", ClientSecret: "secret",
		TeamID: "team-1", ChannelID: "channel-1",
	}
}

func TestPreviewTeamsMessages(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/teams/team-1/channels/channel-1/messages") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value": [
			{"id": "msg-1", "subject": "Kickoff", "createdDateTime": "2026-01-02T10:00:00Z",
			 "from": {"user": {"displayName": "Alice"}},
			 "body": {"contentType": "html", "content": "<p>Let's start the project.</p>"}},
			{"id": "msg-2", "subject": "", "createdDateTime": "2026-01-03T10:00:00Z",
			 "deletedDateTime": "2026-01-04T10:00:00Z",
			 "from": {"user": {"displayName": "Bob"}},
			 "body": {"contentType": "text", "content": "This was deleted."}}
		]}`))
	})

	res, err := previewTeamsMessages(context.Background(), testTeamsConfig(), 50)
	if err != nil {
		t.Fatalf("previewTeamsMessages: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("want 1 item (deleted message filtered out), got %d: %+v", len(res.Items), res.Items)
	}
	item := res.Items[0]
	if item.ID != "msg-1" || item.Subject != "Kickoff" || item.From != "Alice" {
		t.Fatalf("unexpected item: %+v", item)
	}
	if !strings.Contains(item.Preview, "Let's start the project") {
		t.Fatalf("expected preview to contain the HTML-stripped body text, got %q", item.Preview)
	}
}

func TestImportTeamsMessages(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/teams/team-1/channels/channel-1/messages/msg-1/replies"):
			_, _ = w.Write([]byte(`{"value": []}`))
		case strings.HasSuffix(r.URL.Path, "/teams/team-1/channels/channel-1/messages/msg-1"):
			_, _ = w.Write([]byte(`{"id": "msg-1", "subject": "Kickoff", "createdDateTime": "2026-01-02T10:00:00Z",
				"from": {"user": {"displayName": "Alice"}},
				"body": {"contentType": "html", "content": "<p>Let's start the project — long enough to embed as a chunk.</p>"}}`))
		default:
			http.NotFound(w, r)
		}
	})

	rag, s := newTestRAG(t)
	cfg := testTeamsConfig()
	s.Teams = []teamsConfig{cfg}

	res, err := importTeamsMessages(t.Context(), rag, s, cfg, "test-embed", map[string]bool{"msg-1": true}, false, nil)
	if err != nil {
		t.Fatalf("importTeamsMessages: %v", err)
	}
	if res.Messages != 1 {
		t.Fatalf("want 1 message, got %d", res.Messages)
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
		if src.SourceID == "teams:team-1:channel-1:msg-1" {
			found = true
			if src.SourceKind != "teams_message" {
				t.Errorf("want source_kind teams_message, got %s", src.SourceKind)
			}
		}
	}
	if !found {
		t.Fatalf("expected source teams:team-1:channel-1:msg-1 to have been ingested, got %+v", sources)
	}
}

// TestPreviewTeamsMessagesFollowsPagination: Graph caps channel-message
// pages at 50 — the preview must follow @odata.nextLink instead of silently
// showing only the newest page.
func TestPreviewTeamsMessagesFollowsPagination(t *testing.T) {
	var base string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"value": [
				{"id": "msg-2", "subject": "Older", "createdDateTime": "2026-01-01T10:00:00Z",
				 "from": {"user": {"displayName": "Bob"}},
				 "body": {"contentType": "text", "content": "second page content"}}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{"value": [
			{"id": "msg-1", "subject": "Newer", "createdDateTime": "2026-01-02T10:00:00Z",
			 "from": {"user": {"displayName": "Alice"}},
			 "body": {"contentType": "text", "content": "first page content"}}
		], "@odata.nextLink": "` + base + `/teams/team-1/channels/channel-1/messages?page=2"}`))
	})
	base = graphBaseURL

	res, err := previewTeamsMessages(context.Background(), testTeamsConfig(), 10)
	if err != nil {
		t.Fatalf("previewTeamsMessages: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("want both pages' items, got %d: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].ID != "msg-1" || res.Items[1].ID != "msg-2" {
		t.Fatalf("want msg-1 then msg-2, got %+v", res.Items)
	}
}

// TestImportTeamsMessagesIncludesThreadReplies: the thread's replies must
// land in the same ingested document (searchable), in conversation order,
// and the doc_date must reflect the newest reply, not the opening post.
func TestImportTeamsMessagesIncludesThreadReplies(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/msg-1/replies"):
			// Graph returns replies newest-first; the importer must flip them.
			_, _ = w.Write([]byte(`{"value": [
				{"id": "r2", "createdDateTime": "2026-01-05T10:00:00Z",
				 "from": {"user": {"displayName": "Carol"}},
				 "body": {"contentType": "html", "content": "<p>REPLY-TWO with the final decision.</p>"}},
				{"id": "r1", "createdDateTime": "2026-01-03T10:00:00Z",
				 "from": {"user": {"displayName": "Bob"}},
				 "body": {"contentType": "text", "content": "REPLY-ONE with a question."}},
				{"id": "r-deleted", "createdDateTime": "2026-01-04T10:00:00Z", "deletedDateTime": "2026-01-04T11:00:00Z",
				 "from": {"user": {"displayName": "Mallory"}},
				 "body": {"contentType": "text", "content": "REPLY-DELETED must not appear."}}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/msg-1"):
			_, _ = w.Write([]byte(`{"id": "msg-1", "subject": "Kickoff", "createdDateTime": "2026-01-02T10:00:00Z",
				"from": {"user": {"displayName": "Alice"}},
				"body": {"contentType": "text", "content": "OPENING-POST long enough to embed."}}`))
		default:
			http.NotFound(w, r)
		}
	})

	rag, s := newTestRAG(t)
	cfg := testTeamsConfig()

	res, err := importTeamsMessages(t.Context(), rag, s, cfg, "test-embed", map[string]bool{"msg-1": true}, false, nil)
	if err != nil {
		t.Fatalf("importTeamsMessages: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", res.Errors)
	}

	content, ok := rag.fetchSourceContent("teams:team-1:channel-1:msg-1")
	if !ok {
		t.Fatalf("thread source not ingested")
	}
	for _, want := range []string{"OPENING-POST", "REPLY-ONE", "REPLY-TWO", "Antwort von Bob", "Antwort von Carol"} {
		if !strings.Contains(content, want) {
			t.Fatalf("thread document missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "REPLY-DELETED") {
		t.Fatalf("deleted reply leaked into the thread document:\n%s", content)
	}
	if strings.Index(content, "REPLY-ONE") > strings.Index(content, "REPLY-TWO") {
		t.Fatalf("replies out of conversation order:\n%s", content)
	}

	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	for _, src := range sources {
		if src.SourceID == "teams:team-1:channel-1:msg-1" {
			want := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC).Unix()
			if src.DocDate != want {
				t.Fatalf("doc_date must be the newest reply's timestamp %d, got %d", want, src.DocDate)
			}
		}
	}
}

// TestTeamsMaxRepliesPerThreadResolvesDefault checks the "0 = default"
// resolution convention for importConfig.TeamsMaxRepliesPerThread.
func TestTeamsMaxRepliesPerThreadResolvesDefault(t *testing.T) {
	if got := teamsMaxRepliesPerThread(importConfig{}); got != teamsMaxRepliesDefault {
		t.Fatalf("want default %d, got %d", teamsMaxRepliesDefault, got)
	}
	if got := teamsMaxRepliesPerThread(importConfig{TeamsMaxRepliesPerThread: 25}); got != 25 {
		t.Fatalf("want configured 25, got %d", got)
	}
}

// TestImportTeamsMessagesRespectsConfiguredReplyCap confirms
// s.Import.TeamsMaxRepliesPerThread actually bounds how many replies are
// fetched/ingested, not just the unconfigured default.
func TestImportTeamsMessagesRespectsConfiguredReplyCap(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/msg-1/replies"):
			_, _ = w.Write([]byte(`{"value": [
				{"id": "r1", "createdDateTime": "2026-01-03T10:00:00Z",
				 "from": {"user": {"displayName": "Bob"}},
				 "body": {"contentType": "text", "content": "REPLY-ONE, should be kept."}},
				{"id": "r2", "createdDateTime": "2026-01-04T10:00:00Z",
				 "from": {"user": {"displayName": "Carol"}},
				 "body": {"contentType": "text", "content": "REPLY-TWO, should be dropped by the cap."}}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/msg-1"):
			_, _ = w.Write([]byte(`{"id": "msg-1", "subject": "Kickoff", "createdDateTime": "2026-01-02T10:00:00Z",
				"from": {"user": {"displayName": "Alice"}},
				"body": {"contentType": "text", "content": "OPENING-POST long enough to embed."}}`))
		default:
			http.NotFound(w, r)
		}
	})

	rag, s := newTestRAG(t)
	s.Import.TeamsMaxRepliesPerThread = 1
	cfg := testTeamsConfig()

	if _, err := importTeamsMessages(t.Context(), rag, s, cfg, "test-embed", map[string]bool{"msg-1": true}, false, nil); err != nil {
		t.Fatalf("importTeamsMessages: %v", err)
	}
	content, ok := rag.fetchSourceContent("teams:team-1:channel-1:msg-1")
	if !ok {
		t.Fatalf("thread source not ingested")
	}
	if !strings.Contains(content, "REPLY-ONE") {
		t.Fatalf("want the first reply kept within the cap, got:\n%s", content)
	}
	if strings.Contains(content, "REPLY-TWO") {
		t.Fatalf("want the second reply dropped by the configured cap of 1, got:\n%s", content)
	}
}

// TestValidateImportSettingsTeamsMaxReplies covers the new field's bounds.
func TestValidateImportSettingsTeamsMaxReplies(t *testing.T) {
	if err := validateImportSettings(importConfig{TeamsMaxRepliesPerThread: -1}); err == nil {
		t.Fatal("expected negative teams_max_replies_per_thread to be rejected")
	}
	if err := validateImportSettings(importConfig{TeamsMaxRepliesPerThread: 2001}); err == nil {
		t.Fatal("expected oversized teams_max_replies_per_thread to be rejected")
	}
	if err := validateImportSettings(importConfig{TeamsMaxRepliesPerThread: 500}); err != nil {
		t.Fatalf("valid teams_max_replies_per_thread rejected: %v", err)
	}
}

// TestHandleSettingsPersistsTeamsMaxReplies guards the recurring
// "new settings field silently fails to persist" bug pattern (see
// AGENTS.md) for importConfig.TeamsMaxRepliesPerThread.
func TestHandleSettingsPersistsTeamsMaxReplies(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"import": map[string]any{"teams_max_replies_per_thread": 42},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := settings.get().Import.TeamsMaxRepliesPerThread; got != 42 {
		t.Fatalf("TeamsMaxRepliesPerThread did not persist: want 42, got %d", got)
	}
}
