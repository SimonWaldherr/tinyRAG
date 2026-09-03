package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// resetNotifState clears notifHistory/notifNextID and drops any live
// subscribers between tests — package-level state, same reasoning as other
// tests in this codebase that reset shared globals via t.Cleanup.
func resetNotifState(t *testing.T) {
	t.Helper()
	notifMu.Lock()
	notifHistory = nil
	notifNextID = 0
	notifMu.Unlock()
	notifSubMu.Lock()
	for ch := range notifSubs {
		delete(notifSubs, ch)
		close(ch)
	}
	notifSubMu.Unlock()
}

func TestPushAdminNotificationAssignsIncrementingIDs(t *testing.T) {
	resetNotifState(t)
	pushAdminNotification("import_done", "first")
	pushAdminNotification("import_done", "second")
	got := notificationsSince(0)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("want IDs 1,2 in order, got %+v", got)
	}
}

func TestNotificationsSinceOnlyReturnsNewer(t *testing.T) {
	resetNotifState(t)
	pushAdminNotification("import_done", "first")
	pushAdminNotification("import_done", "second")
	pushAdminNotification("import_done", "third")
	got := notificationsSince(1)
	if len(got) != 2 || got[0].Message != "second" || got[1].Message != "third" {
		t.Fatalf("want [second, third], got %+v", got)
	}
}

func TestHandleAdminNotificationsStreamDeliversCatchUp(t *testing.T) {
	resetNotifState(t)
	pushAdminNotification("import_done", "SharePoint-Sync: fertig")
	pushAdminNotification("import_error", "Jira-Sync: Fehler")

	// An already-canceled context: the catch-up loop writes unconditionally
	// (it doesn't check ctx), so both notifications still land in the
	// response before the handler's very first ctx.Done() check ends the
	// connection — a deterministic way to test catch-up without needing a
	// live, concurrently-read connection.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/stream?since=0", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	handleAdminNotificationsStream(w, r)

	body := w.Body.String()
	if !strings.Contains(body, "SharePoint-Sync: fertig") {
		t.Errorf("want the first notification in the catch-up, got: %s", body)
	}
	if !strings.Contains(body, "Jira-Sync: Fehler") {
		t.Errorf("want the second notification in the catch-up, got: %s", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("want Content-Type text/event-stream, got %q", ct)
	}
}

func TestHandleAdminNotificationsStreamSinceSkipsAlreadySeen(t *testing.T) {
	resetNotifState(t)
	pushAdminNotification("import_done", "already-seen")
	pushAdminNotification("import_done", "new-one")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/stream?since=1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	handleAdminNotificationsStream(w, r)

	body := w.Body.String()
	if strings.Contains(body, "already-seen") {
		t.Errorf("want ?since=1 to skip the already-seen notification, got: %s", body)
	}
	if !strings.Contains(body, "new-one") {
		t.Errorf("want the newer notification delivered, got: %s", body)
	}
}

// TestHandleAdminNotificationsStreamLivePush proves a notification pushed
// WHILE the connection is open reaches the subscriber in real time — the
// actual point of the SSE endpoint over the plain poll (handleAdminNotifications).
func TestHandleAdminNotificationsStreamLivePush(t *testing.T) {
	resetNotifState(t)

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/stream", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handleAdminNotificationsStream(w, r)
		close(done)
	}()

	// Give the handler time to reach notifSubscribe() before pushing, so
	// the push isn't lost to a subscription that hasn't registered yet.
	time.Sleep(30 * time.Millisecond)
	pushAdminNotification("import_done", "Freshservice-Sync: fertig")
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancellation")
	}

	// Safe to read w.Body now without further synchronization: close(done)
	// happens-after the handler's last write, and this read happens-after
	// the receive on done above.
	body := w.Body.String()
	if !strings.Contains(body, "Freshservice-Sync: fertig") {
		t.Fatalf("want the live-pushed notification in the body, got: %s", body)
	}
}

func TestNotifBroadcastDoesNotBlockOnFullSubscriberBuffer(t *testing.T) {
	resetNotifState(t)
	sub := notifSubscribe()
	defer notifUnsubscribe(sub)

	// Fill the subscriber's buffer past capacity — notifBroadcast must drop
	// the excess rather than block the pushing goroutine.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			pushAdminNotification("import_done", "n")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pushAdminNotification blocked on a full subscriber channel")
	}
}
