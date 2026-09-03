package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testFreshserviceConfig(baseURL string) freshserviceConfig {
	return freshserviceConfig{
		Enabled: true, BaseURL: baseURL, APIKey: "key-123",
	}
}

func TestFreshserviceAuthAndPreview(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/api/v2/tickets") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tickets": [
			{"id": 1, "subject": "Drucker kaputt", "status": 2, "updated_at": "2026-01-05T10:00:00Z"}
		]}`))
	}))
	defer server.Close()

	cfg := testFreshserviceConfig(server.URL)
	res, err := previewFreshserviceTickets(context.Background(), cfg, 50)
	if err != nil {
		t.Fatalf("previewFreshserviceTickets: %v", err)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("key-123:X"))
	if gotAuth != wantAuth {
		t.Fatalf("want Basic auth header %q, got %q", wantAuth, gotAuth)
	}
	if len(res.Items) != 1 || res.Items[0].ID != 1 || res.Items[0].Subject != "Drucker kaputt" || res.Items[0].Status != "Open" {
		t.Fatalf("unexpected items: %+v", res.Items)
	}
}

func TestFreshserviceResolvedAPIKeyPrefersEnv(t *testing.T) {
	t.Setenv("R3_TEST_FS_KEY", "env-key")
	cfg := freshserviceConfig{APIKey: "inline-key", APIKeyEnv: "R3_TEST_FS_KEY"}
	if got := freshserviceResolvedAPIKey(cfg); got != "env-key" {
		t.Fatalf("want env-key, got %q", got)
	}
	cfg = freshserviceConfig{APIKey: "inline-key"}
	if got := freshserviceResolvedAPIKey(cfg); got != "inline-key" {
		t.Fatalf("want inline-key when no env var configured, got %q", got)
	}
}

func TestFreshserviceTicketText(t *testing.T) {
	var ticket freshserviceTicketFull
	ticket.Ticket.ID = 1
	ticket.Ticket.Subject = "Drucker kaputt"
	ticket.Ticket.Status = 3
	ticket.Ticket.DescriptionText = "Der Drucker im zweiten Stock zieht kein Papier mehr ein."
	ticket.Ticket.Requester = &struct {
		Name string `json:"name"`
	}{Name: "Max Mustermann"}

	text := freshserviceTicketText(ticket)
	for _, want := range []string{"#1: Drucker kaputt", "Status: Pending", "Anfragender: Max Mustermann", "zweiten Stock"} {
		if !strings.Contains(text, want) {
			t.Errorf("want text to contain %q, got:\n%s", want, text)
		}
	}
}

func TestImportFreshserviceTickets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v2/tickets/1") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ticket": {
			"id": 1, "subject": "Drucker kaputt", "status": 2,
			"updated_at": "2026-01-05T10:00:00Z",
			"description_text": "Der Drucker im zweiten Stock zieht kein Papier mehr ein, Fehlermeldung E-42."
		}}`))
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	cfg := testFreshserviceConfig(server.URL)
	s.Freshservice = []freshserviceConfig{cfg}

	res, err := importFreshserviceTickets(context.Background(), rag, s, cfg, "test-embed", map[int]bool{1: true}, false, nil)
	if err != nil {
		t.Fatalf("importFreshserviceTickets: %v", err)
	}
	if res.Tickets != 1 {
		t.Fatalf("want 1 ticket, got %d", res.Tickets)
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
	wantSourceID := "freshservice:" + strings.TrimPrefix(strings.TrimPrefix(server.URL, "https://"), "http://") + ":1"
	found := false
	for _, src := range sources {
		if src.SourceID == wantSourceID {
			found = true
			if src.SourceKind != "freshservice_ticket" {
				t.Errorf("want source_kind freshservice_ticket, got %s", src.SourceKind)
			}
		}
	}
	if !found {
		t.Fatalf("expected source %s to have been ingested, got %+v", wantSourceID, sources)
	}
}
