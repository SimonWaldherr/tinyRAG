package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTestAuditLog points auditLogPath at a fresh temp file for the
// duration of the test, restoring the previous value afterward — same
// "swap the package-level path, restore via t.Cleanup" shape used
// elsewhere in this codebase for per-test isolation of shared state.
func withTestAuditLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-audit.jsonl")
	prev := auditLogPath
	auditLogPath = path
	t.Cleanup(func() { auditLogPath = prev })
	return path
}

// readAuditEvents reads every line of path as one auditEvent each, failing
// the test on any malformed line rather than silently skipping it.
func readAuditEvents(t *testing.T, path string) []auditEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open audit log: %v", err)
	}
	defer f.Close()
	var out []auditEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev auditEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("malformed audit line %q: %v", scanner.Text(), err)
		}
		out = append(out, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan audit log: %v", err)
	}
	return out
}

func TestAppendAuditWritesOneJSONLinePerEvent(t *testing.T) {
	path := withTestAuditLog(t)
	if err := appendAudit(auditEvent{Time: 1000, Actor: "simon", Action: "login", Detail: "method=ldap ok=true"}); err != nil {
		t.Fatalf("appendAudit: %v", err)
	}
	if err := appendAudit(auditEvent{Time: 1001, Actor: "anonym", Action: "ask", Detail: "mode= hallo?"}); err != nil {
		t.Fatalf("appendAudit: %v", err)
	}
	events := readAuditEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Actor != "simon" || events[0].Action != "login" {
		t.Fatalf("unexpected first event: %+v", events[0])
	}
	if events[1].Actor != "anonym" || events[1].Action != "ask" {
		t.Fatalf("unexpected second event: %+v", events[1])
	}
}

func TestActorFromRequest(t *testing.T) {
	t.Run("no session: anonym", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if got := actorFromRequest(r); got != "anonym" {
			t.Fatalf("want \"anonym\", got %q", got)
		}
	})

	t.Run("session with mail: uses mail", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "Simon Waldherr", Mail: "simon.waldherr@rubix.com"})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		if got := actorFromRequest(r); got != "simon.waldherr@rubix.com" {
			t.Fatalf("want the session's mail, got %q", got)
		}
	})

	t.Run("session without mail: falls back to CN", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "test.user"})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		if got := actorFromRequest(r); got != "test.user" {
			t.Fatalf("want the session's CN as fallback, got %q", got)
		}
	})
}

func TestLogAuditRecordsRemoteIPAndActor(t *testing.T) {
	path := withTestAuditLog(t)
	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	r.RemoteAddr = "203.0.113.7:54321"
	logAudit(r, "ask", "mode= hallo?")

	events := readAuditEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Actor != "anonym" {
		t.Fatalf("want anonymous actor for a request with no session, got %q", ev.Actor)
	}
	if ev.RemoteIP != "203.0.113.7" {
		t.Fatalf("want the client IP without the port, got %q", ev.RemoteIP)
	}
	if ev.Action != "ask" || ev.Detail != "mode= hallo?" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestLogImportAuditFormatsConnectorCountsAndDryRun(t *testing.T) {
	path := withTestAuditLog(t)
	r := httptest.NewRequest(http.MethodPost, "/api/import/jira", nil)
	logImportAudit(r, "jira", baseImportResult{Chunks: 12, Skipped: 2, Errors: []string{"boom"}, DryRun: true})

	events := readAuditEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	want := "connector=jira chunks=12 skipped=2 errors=1 dry_run=true"
	if events[0].Detail != want {
		t.Fatalf("want detail %q, got %q", want, events[0].Detail)
	}
	if events[0].Action != "import" {
		t.Fatalf("want action \"import\", got %q", events[0].Action)
	}
}

// TestHandleSourceDeleteLogsAudit is an end-to-end check that the actual
// HTTP handler — not just audit.go's own primitives — appends a real
// audit line when a source is deleted, since that's the wiring that
// actually matters (a helper function nobody calls doesn't audit anything).
func TestHandleSourceDeleteLogsAudit(t *testing.T) {
	path := withTestAuditLog(t)
	s := newTestTinySQLStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	rag := &ragSystem{store: s}

	body := strings.NewReader(`{"source_id":"doc-1"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/sources/delete", body)
	w := httptest.NewRecorder()
	handleSourceDelete(rag)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	events := readAuditEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(events))
	}
	if events[0].Action != "source_delete" || events[0].Detail != "source_id=doc-1" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}

// TestSummarizeIngestOutcomes covers handleUpload/handleImportFolder's
// audit detail helper directly: total chunks summed across files, and an
// error count that only counts entries that actually recorded one.
func TestSummarizeIngestOutcomes(t *testing.T) {
	results := []ingestOutcome{
		{SourceName: "a.txt", Chunks: 3},
		{SourceName: "b.txt", Chunks: 5, Error: "extraction failed"},
		{SourceName: "c.txt", Chunks: 0, Skipped: true},
	}
	got := summarizeIngestOutcomes(results, false)
	want := "files=3 chunks=8 errors=1 dry_run=false"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}
