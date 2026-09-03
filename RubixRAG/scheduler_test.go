package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleSchedulerResetCursorClearsIMAPCursorAndRecordsHistory covers
// the reset-cursor endpoint end to end: it must actually clear the
// connection's LastUID (resetConnCursor's job), AND — this is the part
// that was missing entirely before — leave a visible trace in
// schedulerHistory (the Jobs tab's "Verlauf" list), so an admin who clicks
// "Cursor zurücksetzen" sees *something* happened instead of the tab
// looking exactly the same as before the click.
func TestHandleSchedulerResetCursorClearsIMAPCursorAndRecordsHistory(t *testing.T) {
	rag, s := newTestRAG(t)
	s.IMAP = []mailboxConfig{{
		connRuntime: connRuntime{Name: "support"},
		Enabled:     true,
		Username:    "support@example.com",
		Mailbox:     "INBOX",
		LastUID:     42,
	}}
	withTestGlobalSettings(t, s)

	// Isolate schedulerHistory for this test — it's a package-level var
	// shared across the whole process, same reasoning as
	// withTestFeedbackLog isolating the feedback log per test.
	schedulerHistoryMu.Lock()
	origHistory := schedulerHistory
	schedulerHistory = nil
	schedulerHistoryMu.Unlock()
	t.Cleanup(func() {
		schedulerHistoryMu.Lock()
		schedulerHistory = origHistory
		schedulerHistoryMu.Unlock()
	})

	jobName := "imap-sync:support"
	body := strings.NewReader(`{"job":"` + jobName + `"}`)
	rec := httptest.NewRecorder()
	handleSchedulerResetCursor(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/scheduler/reset-cursor", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	if got := settings.get().IMAP[0].LastUID; got != 0 {
		t.Fatalf("want LastUID cleared to 0, got %d", got)
	}

	run := lastSchedulerRun(jobName)
	if run == nil {
		t.Fatalf("want a schedulerHistory entry for %q after reset-cursor, got none", jobName)
	}
	if !run.OK {
		t.Fatalf("want the recorded entry marked OK, got %+v", run)
	}
	if run.Trigger != "manuell" {
		t.Fatalf("want Trigger=%q, got %q", "manuell", run.Trigger)
	}
}

// TestHandleSchedulerResetCursorRejectsUnresettableKind confirms a
// connector kind with no resumable cursor (e.g. one that always re-lists
// everything, unlike SharePoint/IMAP) is rejected rather than silently
// no-op'd — findSchedulerJob succeeds (the job exists) but
// handleSchedulerResetCursor must still reject it as unresettable.
func TestHandleSchedulerResetCursorRejectsUnresettableKind(t *testing.T) {
	rag, s := newTestRAG(t)
	s.Freshservice = []freshserviceConfig{{
		connRuntime: connRuntime{Name: "helpdesk"},
		Enabled:     true,
		BaseURL:     "https://example.freshservice.com",
		APIKey:      "k",
	}}
	withTestGlobalSettings(t, s)

	body := strings.NewReader(`{"job":"freshservice-sync:helpdesk"}`)
	rec := httptest.NewRecorder()
	handleSchedulerResetCursor(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/scheduler/reset-cursor", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a connector kind with no resettable cursor, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}
