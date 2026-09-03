package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// A minimal in-memory admin notification feed, pushed live to every
// logged-in admin's browser over SSE (web/app.js's EventSource against
// handleAdminNotificationsStream below) and rendered as a novapop.js toast
// — e.g. "Import X fertig". History storage is modeled on scheduler.go's
// schedulerHistory ring buffer: same no-persistence, capped-size tradeoff
// (a restart drops pending notifications, same as it drops sessions), no
// new dependency beyond the standard library's http.Flusher.
//
// Originally a plain 8-second poll (GET /api/admin/notifications) from
// every open admin tab — harmless at small scale but needlessly chatty in
// the access log, and each poll incurs its own request/response/JSON round
// trip for what's almost always "nothing new". SSE keeps one long-lived
// connection per tab that the server writes to only when something
// actually happens; handleAdminNotifications (the original one-shot poll)
// stays registered for any caller that genuinely wants point-in-time
// polling instead (a script, a future non-browser integration) — the two
// share notifHistory/notificationsSince, they just differ in whether the
// server pushes live updates on top of the initial catch-up.
// ─────────────────────────────────────────────────────────────────────────────

// adminNotification is one toast-worthy event. Kind drives the toast's
// visual style client-side (web/app.js maps a "_error" suffix to novapop's
// "error" type, everything else to "success").
type adminNotification struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

const notifHistoryLimit = 50

var (
	notifMu      sync.Mutex
	notifHistory []adminNotification
	notifNextID  int64
)

// pushAdminNotification appends a new notification, trimming notifHistory
// to notifHistoryLimit, then broadcasts it to every live SSE subscriber —
// called from scheduler.go when a job finishes.
func pushAdminNotification(kind, message string) {
	notifMu.Lock()
	notifNextID++
	n := adminNotification{
		ID:      notifNextID,
		Kind:    kind,
		Message: message,
		At:      time.Now().Unix(),
	}
	notifHistory = append(notifHistory, n)
	if len(notifHistory) > notifHistoryLimit {
		notifHistory = notifHistory[len(notifHistory)-notifHistoryLimit:]
	}
	notifMu.Unlock()

	notifBroadcast(n)
}

// ─── SSE broadcast ──────────────────────────────────────────────────────────

// notifSubscriber is one active SSE connection's delivery channel —
// buffered so a slow/blocked reader doesn't stall pushAdminNotification for
// every other subscriber; a full buffer just drops that one subscriber's
// live push, which its next reconnect's ?since= catch-up (notifHistory)
// papers over. Separate mutex from notifMu above: broadcasting must never
// contend with, or be blocked behind, an unrelated history read/write.
type notifSubscriber chan adminNotification

var (
	notifSubMu sync.Mutex
	notifSubs  = map[notifSubscriber]bool{}
)

// notifSubscribe registers a new subscriber — callers MUST defer
// notifUnsubscribe(sub) to avoid leaking the channel and its map entry once
// their connection ends.
func notifSubscribe() notifSubscriber {
	ch := make(notifSubscriber, 8)
	notifSubMu.Lock()
	notifSubs[ch] = true
	notifSubMu.Unlock()
	return ch
}

func notifUnsubscribe(ch notifSubscriber) {
	notifSubMu.Lock()
	delete(notifSubs, ch)
	notifSubMu.Unlock()
	close(ch)
}

// notifBroadcast fans n out to every current subscriber, non-blockingly —
// see notifSubscriber's doc comment for why a full buffer just drops the
// push for that one subscriber rather than blocking every other one.
func notifBroadcast(n adminNotification) {
	notifSubMu.Lock()
	defer notifSubMu.Unlock()
	for ch := range notifSubs {
		select {
		case ch <- n:
		default:
		}
	}
}

// notificationsSince returns every notification with ID > sinceID, oldest
// first.
func notificationsSince(sinceID int64) []adminNotification {
	notifMu.Lock()
	defer notifMu.Unlock()
	out := []adminNotification{}
	for _, n := range notifHistory {
		if n.ID > sinceID {
			out = append(out, n)
		}
	}
	return out
}

// handleAdminNotifications serves ?since=<id> (default 0) as a one-shot
// snapshot of the notification history — see registerRoutes for the
// requireAdminSession gate. Superseded as the Mail tab's live update
// mechanism by handleAdminNotificationsStream below; kept for any caller
// that wants a plain point-in-time poll instead of a persistent connection.
func handleAdminNotifications(w http.ResponseWriter, r *http.Request) {
	since := parseSinceParam(r)
	writeJSON(w, map[string]any{"notifications": notificationsSince(since)})
}

// notifStreamKeepAlive is how often handleAdminNotificationsStream writes an
// SSE comment line on an otherwise-idle connection — long enough to almost
// never fire in practice (real notifications are rare), short enough that
// an intermediary proxy/load balancer with its own idle-connection timeout
// (commonly 60s) never sees a fully silent connection and drops it.
const notifStreamKeepAlive = 25 * time.Second

// parseSinceParam parses the shared ?since=<id> query parameter for both
// notification endpoints — invalid/missing defaults to 0 (the beginning of
// notifHistory), never a request error, since "since" is just a resume
// point, not a required parameter.
func parseSinceParam(r *http.Request) int64 {
	if v := r.URL.Query().Get("since"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

// handleAdminNotificationsStream serves a Server-Sent Events stream: every
// notification queued since ?since=<id> (the reconnect catch-up, from
// notifHistory) followed by every new one as pushAdminNotification produces
// it, live, for as long as the connection stays open — see registerRoutes
// for the requireAdminSession gate. The client (web/app.js's EventSource)
// reconnects automatically on a drop; browsers resend the last received
// event's id as ?since= themselves via EventSource's own Last-Event-ID
// handling if the server sets it, but this endpoint accepts it as an
// explicit query parameter instead (simpler than threading an `id:` field
// through every write path just to enable a header EventSource doesn't
// let JS read anyway) — web/app.js passes lastAdminNotifId itself on every
// (re)connect.
func handleAdminNotificationsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	since := parseSinceParam(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	for _, n := range notificationsSince(since) {
		if !writeSSENotification(w, n) {
			return
		}
	}
	flusher.Flush()

	sub := notifSubscribe()
	defer notifUnsubscribe(sub)

	keepAlive := time.NewTicker(notifStreamKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected (tab closed, navigated away, logout) —
			// nothing to clean up beyond the deferred notifUnsubscribe.
			return
		case n := <-sub:
			if !writeSSENotification(w, n) {
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSENotification writes n as one SSE "data:" event, returning false
// if the write failed (client gone) so the caller can stop rather than keep
// writing to a dead connection.
func writeSSENotification(w http.ResponseWriter, n adminNotification) bool {
	payload, err := json.Marshal(n)
	if err != nil {
		return true // malformed notification, not a dead connection — skip and keep going
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", n.ID, payload)
	return err == nil
}
