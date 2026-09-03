package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// R3's background scheduler: one syncJob per configured, enabled connector
// connection (all multi-instance import connectors — see connruntime.go),
// rebuilt from live settings every tick so settings edits apply without a
// restart. Beyond the periodic loop itself, this file also carries the
// operational surface around it:
//
//   - a shared run registry (schedRunning/schedNextRun) so the Settings
//     dashboard can show "läuft gerade seit …", "nächster Lauf um …" and
//     the last result per job (GET /api/scheduler/status),
//   - ad-hoc runs of any job — including connections with no cycle
//     configured at all (POST /api/scheduler/run),
//   - cancelling a running job mid-flight via its stored context cancel
//     (POST /api/scheduler/cancel) — every import loop already honors
//     ctx between items, so this stops within one item, not instantly,
//   - pausing/resuming a connection's auto-sync without forgetting its
//     configured interval (POST /api/scheduler/pause, persisted in
//     connRuntime.Paused — see its doc comment for why the settings form
//     can't overwrite it).
//
// Deliberately a hand-rolled ticker loop, not github.com/robfig/cron/v3
// (already an indirect dependency, pulled in transitively, but unused by
// any R3 code) — R3's own connectors need only "every N minutes", not cron
// expressions, and a ticker keeps this dependency-free like the rest of the
// connector code.
// ─────────────────────────────────────────────────────────────────────────────

// syncJob is one runnable background task for one connector connection.
// The whole list is rebuilt from settings.get() on every scheduler tick
// (schedulerBuildJobs), so these are cheap value objects — all persistent
// state lives in the registry below (keyed by Name) and in settings.
type syncJob struct {
	// Name uniquely identifies the job across rebuilds:
	// "<kind-prefix>-sync:<connection name>" — the registry/history key.
	Name string
	// Kind is the connector's settings JSON key ("imap", "sharepoint",
	// "exchange_graph", ...) — what handleSchedulerPause needs to find the
	// right settings list to persist Paused into.
	Kind string
	// Conn is the connection's Name within its Kind.
	Conn string
	// IntervalSecs is the configured cycle in seconds; 0 means "no
	// auto-sync configured" — the job still exists so it shows up in the
	// dashboard and can be run ad-hoc, it just never fires on its own.
	IntervalSecs int
	// Paused suspends automatic firing (connRuntime.Paused); ad-hoc runs
	// deliberately ignore it.
	Paused bool
	// Run does the actual work, returning a short human-readable summary
	// (e.g. "12 tickets, 8 chunks, 2 skipped") alongside the usual error —
	// recorded into schedulerHistory below so an admin can see recent
	// unattended runs without grepping the server log.
	Run func(ctx context.Context) (string, error)
}

// schedulerRun is one completed job run, kept in a bounded in-memory view
// and appended to a JSONL operations history for the Jobs UI. The persisted
// log survives restarts; the in-memory cap keeps status rendering cheap.
type schedulerRun struct {
	Job        string `json:"job"`
	StartedAt  int64  `json:"started_at"` // unix seconds
	DurationMS int64  `json:"duration_ms"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
	// Trigger records what started the run: "auto" (the scheduler's own
	// tick) or "manuell" (the dashboard's "Jetzt ausführen").
	Trigger string `json:"trigger"`
}

// schedulerHistoryLimit caps schedulerHistory at a small, fixed size — a
// dashboard only ever needs the last few runs, not an unbounded log that
// would otherwise grow for the lifetime of a long-running process.
const schedulerHistoryLimit = 50

var (
	schedulerHistoryMu sync.Mutex
	schedulerHistory   []schedulerRun
)

// recordSchedulerRun prepends run to the in-memory history (newest first),
// trimming to schedulerHistoryLimit, then atomically persists that same
// bounded view. Keeping the file bounded as well prevents a long-running
// instance from accumulating an unbounded operations log it never shows.
func recordSchedulerRun(run schedulerRun) {
	schedulerHistoryMu.Lock()
	schedulerHistory = append([]schedulerRun{run}, schedulerHistory...)
	if len(schedulerHistory) > schedulerHistoryLimit {
		schedulerHistory = schedulerHistory[:schedulerHistoryLimit]
	}
	persistSchedulerHistoryLocked(schedulerHistory)
	schedulerHistoryMu.Unlock()
}

// schedulerHistorySnapshot returns a copy of the current history so a
// concurrent JSON-encode (handleSchedulerHistory) never races a new run
// being recorded mid-response.
func schedulerHistorySnapshot() []schedulerRun {
	schedulerHistoryMu.Lock()
	defer schedulerHistoryMu.Unlock()
	out := make([]schedulerRun, len(schedulerHistory))
	copy(out, schedulerHistory)
	return out
}

// lastSchedulerRun returns the most recent history entry for one job, nil
// if it hasn't run since the last server start.
func lastSchedulerRun(jobName string) *schedulerRun {
	schedulerHistoryMu.Lock()
	defer schedulerHistoryMu.Unlock()
	for i := range schedulerHistory {
		if schedulerHistory[i].Job == jobName {
			r := schedulerHistory[i]
			return &r
		}
	}
	return nil
}

// ─── Run registry ────────────────────────────────────────────────────────────

// schedRunningInfo is one in-flight job run: its cancel func (what
// handleSchedulerCancel invokes) and when it started (for the dashboard's
// "läuft seit …").
type schedRunningInfo struct {
	cancel    context.CancelFunc
	startedAt time.Time
}

var (
	schedMu      sync.Mutex
	schedRunning = map[string]*schedRunningInfo{}
	schedNextRun = map[string]time.Time{}
	// schedRootCtx is the scheduler's lifetime context (set by
	// startScheduler) — ad-hoc runs launched from an HTTP handler derive
	// from it rather than from the request's context, so the run survives
	// the HTTP response but still dies with the server.
	schedRootCtx context.Context = context.Background()
)

// schedulerLaunch starts job in its own goroutine unless it's already
// running — the single entry point for both tick-triggered ("auto") and
// dashboard-triggered ("manuell") runs, so overlap protection, cancel
// registration, logging and history recording can't drift apart between
// the two paths. Returns false if the job was already running.
func schedulerLaunch(job syncJob, trigger string) bool {
	schedMu.Lock()
	if _, running := schedRunning[job.Name]; running {
		schedMu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(schedRootCtx)
	schedRunning[job.Name] = &schedRunningInfo{cancel: cancel, startedAt: time.Now()}
	schedMu.Unlock()
	metricSchedulerStarted(job.Name)

	go func() {
		defer func() {
			cancel()
			schedMu.Lock()
			delete(schedRunning, job.Name)
			schedMu.Unlock()
		}()
		started := time.Now()
		log.Printf("scheduler: %s starting (%s)", job.Name, trigger)
		detail, err := job.Run(ctx)
		dur := time.Since(started)
		msg := detail
		cancelled := errors.Is(err, context.Canceled)
		if err != nil {
			msg = err.Error()
			// A cancelled run isn't a connector failure — label it as the
			// deliberate stop it was, so the dashboard doesn't read like
			// the connector broke.
			if cancelled {
				msg = "abgebrochen (manuell gestoppt oder Server-Stopp)"
			}
			log.Printf("scheduler: %s failed after %s: %v", job.Name, dur.Round(time.Millisecond), err)
		} else {
			log.Printf("scheduler: %s done in %s: %s", job.Name, dur.Round(time.Millisecond), detail)
		}
		recordSchedulerRun(schedulerRun{
			Job:        job.Name,
			StartedAt:  started.Unix(),
			DurationMS: dur.Milliseconds(),
			OK:         err == nil,
			Detail:     msg,
			Trigger:    trigger,
		})
		// A deliberate cancellation is recorded as an interrupted run for
		// operator context, but must not increment the connector-failure
		// counter or create an operational failure alert.
		metricSchedulerFinished(job.Name, dur.Seconds(), err == nil || cancelled)
		// A manual stop/server shutdown isn't news worth a toast — only
		// push a notification for a job actually finishing (success or
		// real failure). See notifications.go / web/app.js's
		// pollAdminNotifications for the client side.
		if !cancelled && err != nil {
			raiseSchedulerAlert(job.Name, msg)
		} else if err == nil {
			resolveSchedulerAlerts(job.Name)
			pushAdminNotification("import_done", fmt.Sprintf("%s: %s", job.Name, msg))
		}
	}()
	return true
}

// schedulerTick is how often runScheduler checks whether any job is due —
// deliberately much finer than any expected interval so a newly configured
// short interval (testing a 1-minute sync, say) doesn't wait behind a
// coarse outer loop.
const schedulerTick = 30 * time.Second

// runScheduler blocks until ctx is cancelled, firing each due job via
// schedulerLaunch. buildJobs is called fresh at the start of every tick
// (schedulerTick, 30s) rather than once at startup — cheap, since it's
// just building closures over a settings.get() snapshot — so adding,
// renaming, pausing or removing a named connection in the Settings UI
// takes effect on the very next tick, no restart needed. A job that's
// still running when its next tick comes due is simply skipped, not
// queued (and its nextRun is NOT advanced, so it fires again promptly
// once the slow run finishes) — a slow connection can't pile up
// concurrent imports of the same source.
func runScheduler(ctx context.Context, buildJobs func() []syncJob) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, job := range buildJobs() {
				if job.Paused || job.IntervalSecs <= 0 {
					continue
				}
				schedMu.Lock()
				_, running := schedRunning[job.Name]
				next, seen := schedNextRun[job.Name]
				due := !running && (!seen || !now.Before(next))
				if due {
					schedNextRun[job.Name] = now.Add(time.Duration(job.IntervalSecs) * time.Second)
				}
				schedMu.Unlock()
				if due {
					schedulerLaunch(job, "auto")
				}
			}
		}
	}
}

// ─── Job construction ────────────────────────────────────────────────────────

// connJobs expands one connector type's connection list into syncJobs —
// every ENABLED connection gets a job, even with no cycle configured
// (IntervalSecs 0) or while paused: those still appear in the dashboard
// and can be run ad-hoc, they just never fire automatically. run receives
// the connection's name, not its config: each Run body re-resolves the
// connection by name against a fresh settings.get(), so a connection
// edited between "due" and "actually running" is never run with stale
// credentials — one that's disappeared or been disabled by then is a
// no-op, not an error.
func connJobs[T connWithEnabled](kind, jobPrefix string, conns []T, intervalSecs func(T) int, run func(ctx context.Context, connName string) (string, error)) []syncJob {
	var jobs []syncJob
	for _, conn := range conns {
		if !conn.isEnabled() {
			continue
		}
		name := conn.connName()
		jobs = append(jobs, syncJob{
			Name:         jobPrefix + ":" + name,
			Kind:         kind,
			Conn:         name,
			IntervalSecs: intervalSecs(conn),
			Paused:       conn.isPaused(),
			Run:          func(ctx context.Context) (string, error) { return run(ctx, name) },
		})
	}
	return jobs
}

// minutesOf adapts the six SyncIntervalMinutes-based connectors to
// connJobs' seconds-based interval accessor.
func minutesOf(m int) int {
	if m <= 0 {
		return 0
	}
	return m * 60
}

// schedulerBuildJobs expands every enabled connection across all 7
// multi-instance import connectors into one syncJob each — called fresh
// every scheduler tick and by every dashboard endpoint, so it always
// reflects the current settings snapshot.
func schedulerBuildJobs(rag *ragSystem) []syncJob {
	s := settings.get()
	var jobs []syncJob

	jobs = append(jobs, connJobs("freshservice", "freshservice-sync", s.Freshservice,
		func(c freshserviceConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.Freshservice, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			preview, err := previewFreshserviceTickets(ctx, conn, importPreviewLimit(s.Import))
			if err != nil {
				return "", err
			}
			selected := make(map[int]bool, len(preview.Items))
			for _, item := range preview.Items {
				selected[item.ID] = true
			}
			res, err := importFreshserviceTickets(ctx, rag, s, conn, s.activeEmbedModel(), selected, false, nil) // never a dry run: this is the unattended background sync, not an admin testing something
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d tickets, %d chunks, %d skipped, %d errors", res.Tickets, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d ticket(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			return detail, nil
		})...)

	// Wires up mailboxConfig.PollInterval (imap.go) per connection, same
	// field/semantics ("poll every N seconds") the connector has always
	// documented — 0 (default) stays off, so a migrated connection that
	// never set it behaves exactly as before.
	jobs = append(jobs, connJobs("imap", "imap-sync", s.IMAP,
		func(c mailboxConfig) int { return c.PollInterval },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.IMAP, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			res, err := importIMAPMessages(ctx, newIMAPClient(conn), rag, s, s.activeEmbedModel(), conn, false, nil) // never a dry run, same as freshservice-sync above
			// LastUID advances even when some messages errored — res
			// only counts a UID once its message was actually processed,
			// and the manual handler (handleIMAPImport) persists under
			// the same condition, so both paths stay incremental the
			// same way.
			if res.LastUID > conn.LastUID {
				_ = settings.update(func(cur *appSettings) {
					if c, i, ok := findConnIndex(cur.IMAP, name); ok {
						c.LastUID = res.LastUID
						cur.IMAP[i] = c
					}
				})
			}
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d messages, %d attachments, %d chunks, %d skipped, %d errors", res.Messages, res.Attachments, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d message(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			return detail, nil
		})...)

	jobs = append(jobs, connJobs("sharepoint", "sharepoint-delta-sync", s.SharePoint,
		func(c sharePointConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.SharePoint, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			res, newDeltaLink, newItemPaths, err := deltaSyncSharePoint(ctx, rag, s, conn, s.activeEmbedModel(), false, nil)
			// Persist the cursor (and the rename/move-tracking item-path
			// map alongside it) even when the run partially failed —
			// mirrors handleSharePointDeltaSync: a non-empty delta link
			// means Graph considers everything up to it delivered, and
			// re-reading the same window next tick would duplicate work,
			// not recover the failed items.
			if newDeltaLink != "" {
				_ = settings.update(func(cur *appSettings) {
					if c, i, ok := findConnIndex(cur.SharePoint, name); ok {
						c.DeltaLink = newDeltaLink
						c.ItemPaths = newItemPaths
						cur.SharePoint[i] = c
					}
				})
			}
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d files, %d chunks, %d skipped, %d errors", res.Files, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d file(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			return detail, nil
		})...)

	jobs = append(jobs, connJobs("onedrive", "onedrive-delta-sync", s.OneDrive,
		func(c oneDriveConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.OneDrive, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			res, cursor, err := syncOneDrive(ctx, rag, s, conn, s.activeEmbedModel(), false, nil)
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d files, %d chunks, %d skipped, %d errors", res.Files, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d OneDrive item(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			if cursor != "" {
				_ = settings.update(func(cur *appSettings) {
					if c, i, found := findConnIndex(cur.OneDrive, name); found {
						c.DeltaLink = cursor
						cur.OneDrive[i] = c
					}
				})
			}
			return detail, nil
		})...)

	// Exchange/Graph, Teams, Confluence, Jira: each mirrors
	// freshservice-sync above — preview everything, select it all, import.
	jobs = append(jobs, connJobs("exchange_graph", "exchange-graph-sync", s.ExchangeGraph,
		func(c exchangeGraphConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.ExchangeGraph, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()

			// Incremental sync via the receivedDateTime watermark
			// (exchangeGraphConfig.LastSyncedReceived): once set, list
			// FORWARD from the watermark, oldest first — a backlog larger
			// than one run's cap drains across consecutive runs, and a
			// burst of more messages than one preview page can hold is
			// never lost. The very first run (or after a manual watermark
			// reset) bootstraps from the newest-N preview exactly as this
			// job always did, then starts the watermark there. ids stay
			// ordered oldest-first in BOTH paths — the watermark advance
			// below indexes the newest *attempted* message.
			var items []graphMailPreviewItem
			var ids []string
			receivedByID := map[string]string{}
			if conn.LastSyncedReceived != "" {
				msgs, err := listExchangeMailSince(ctx, conn, conn.LastSyncedReceived, conn.effectiveMaxItems(s.Import))
				if err != nil {
					return "", err
				}
				for _, m := range msgs {
					ids = append(ids, m.ID)
					receivedByID[m.ID] = m.ReceivedDateTime
					from := m.From.EmailAddress.Name
					if from == "" {
						from = m.From.EmailAddress.Address
					}
					items = append(items, graphMailPreviewItem{ID: m.ID, Subject: m.Subject, From: from, Received: m.ReceivedDateTime})
				}
			} else {
				preview, err := previewExchangeMail(ctx, conn, importPreviewLimit(s.Import))
				if err != nil {
					return "", err
				}
				items = preview.Items
				// Preview lists newest first — reverse into oldest-first so
				// the watermark logic below works identically here.
				for i := len(items) - 1; i >= 0; i-- {
					ids = append(ids, items[i].ID)
					receivedByID[items[i].ID] = items[i].Received
				}
			}

			// Auto-draft rule engine (autodraft.go) — a no-op unless the
			// connection opted into both EnableAutoDraftRules and
			// EnableDraftReplies. Runs against this same listing batch
			// (no extra Graph listing call) and independently of the
			// import below: a message getting a rule-matched draft reply
			// doesn't depend on it also being successfully imported, and
			// vice versa. HARD SAFETY INVARIANT: this only ever files a
			// DRAFT, never sends — see autodraft.go/graphmail.go's doc
			// comments.
			updatedIDs, drafted, draftErrs := runExchangeAutoDraftRules(ctx, rag, s, conn, items)
			if drafted > 0 || len(draftErrs) > 0 || len(updatedIDs) != len(conn.AutoDraftedIDs) {
				_ = settings.update(func(cur *appSettings) {
					if c, i, ok := findConnIndex(cur.ExchangeGraph, name); ok {
						c.AutoDraftedIDs = updatedIDs
						cur.ExchangeGraph[i] = c
					}
				})
			}

			res, processed, err := importExchangeMailIDs(ctx, rag, s, conn, s.activeEmbedModel(), ids, false, nil)
			// Advance the watermark across every ATTEMPTED message — even
			// when the run was cut short (cap/timeout) or some messages
			// failed individually: attempted means "this run looked at
			// it", and the next run must resume behind it rather than
			// re-listing the same prefix forever. The `ge` filter
			// re-includes the boundary message itself; the content-hash
			// skip makes that free.
			if processed > 0 {
				if wm := receivedByID[ids[processed-1]]; wm != "" {
					_ = settings.update(func(cur *appSettings) {
						if c, i, ok := findConnIndex(cur.ExchangeGraph, name); ok {
							c.LastSyncedReceived = wm
							cur.ExchangeGraph[i] = c
						}
					})
				}
			}
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d messages, %d attachments, %d chunks, %d skipped, %d errors", res.Messages, res.Attachments, res.Chunks, res.Skipped, len(res.Errors))
			if drafted > 0 {
				detail = fmt.Sprintf("%s, %d auto-draft(s)", detail, drafted)
			}
			allErrs := append(append([]string{}, res.Errors...), draftErrs...)
			if len(allErrs) > 0 {
				return detail, fmt.Errorf("%d message(s)/draft(s) failed: %s", len(allErrs), strings.Join(allErrs, "; "))
			}
			return detail, nil
		})...)

	jobs = append(jobs, connJobs("teams", "teams-sync", s.Teams,
		func(c teamsConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.Teams, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			preview, err := previewTeamsMessages(ctx, conn, importPreviewLimit(s.Import))
			if err != nil {
				return "", err
			}
			selected := make(map[string]bool, len(preview.Items))
			for _, item := range preview.Items {
				selected[item.ID] = true
			}
			res, err := importTeamsMessages(ctx, rag, s, conn, s.activeEmbedModel(), selected, false, nil)
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d messages, %d chunks, %d skipped, %d errors", res.Messages, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d message(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			return detail, nil
		})...)

	jobs = append(jobs, connJobs("confluence", "confluence-sync", s.Confluence,
		func(c confluenceConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.Confluence, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			preview, err := previewConfluencePages(ctx, conn, importPreviewLimit(s.Import))
			if err != nil {
				return "", err
			}
			selected := make(map[string]bool, len(preview.Items))
			for _, item := range preview.Items {
				selected[item.ID] = true
			}
			res, err := importConfluencePages(ctx, rag, s, conn, s.activeEmbedModel(), selected, false, nil)
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d pages, %d chunks, %d skipped, %d errors", res.Pages, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d page(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			return detail, nil
		})...)

	jobs = append(jobs, connJobs("jira", "jira-sync", s.Jira,
		func(c jiraConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.Jira, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			preview, err := previewJiraIssues(ctx, conn, importPreviewLimit(s.Import))
			if err != nil {
				return "", err
			}
			selected := make(map[string]bool, len(preview.Items))
			for _, item := range preview.Items {
				selected[item.Key] = true
			}
			res, err := importJiraIssues(ctx, rag, s, conn, s.activeEmbedModel(), selected, false, nil)
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d issues, %d chunks, %d skipped, %d errors", res.Issues, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d issue(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			return detail, nil
		})...)

	// Folder: no preview/select step (ingestFolder already recursively
	// ingests everything under Path, unlike SharePoint/Confluence/etc.),
	// so this job is just ingestFolder itself, capped/paced the same way
	// every other connection is via effectiveMaxItems/effectiveTimeout.
	jobs = append(jobs, connJobs("folder", "folder-sync", s.Folder,
		func(c folderConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.Folder, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			results, err := ingestFolder(ctx, rag, s, s.activeEmbedModel(), conn.Path, conn.effectiveMaxItems(s.Import), false)
			chunks, skipped, errCount := 0, 0, 0
			for _, res := range results {
				chunks += res.Chunks
				if res.Skipped {
					skipped++
				}
				if res.Error != "" {
					errCount++
				}
			}
			detail := fmt.Sprintf("%d files, %d chunks, %d skipped, %d errors", len(results), chunks, skipped, errCount)
			if errCount > 0 {
				return detail, fmt.Errorf("%d file(s) failed", errCount)
			}
			return detail, err
		})...)

	jobs = append(jobs, connJobs("github", "github-sync", s.GitHub,
		func(c githubConfig) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.GitHub, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			res, next, err := syncGitHubRepository(ctx, rag, s, conn, s.activeEmbedModel(), false)
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d issues, %d pull requests, %d README(s), %d chunks, %d skipped, %d errors", res.Issues, res.PullRequests, res.Readmes, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d GitHub document(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			_ = settings.update(func(cur *appSettings) {
				if c, i, found := findConnIndex(cur.GitHub, name); found {
					c.LastSyncedAt = next.LastSyncedAt
					c.CycleStartedAt = next.CycleStartedAt
					c.NextPage = next.NextPage
					cur.GitHub[i] = c
				}
			})
			return detail, nil
		})...)

	jobs = append(jobs, connJobs("sap_s4", "sap-s4-sync", s.SAPS4,
		func(c sapS4Config) int { return minutesOf(c.SyncIntervalMinutes) },
		func(ctx context.Context, name string) (string, error) {
			s := settings.get()
			conn, ok := findConnByName(s.SAPS4, name)
			if !ok || !conn.Enabled {
				return "connection removed/disabled", nil
			}
			ctx, cancel := context.WithTimeout(ctx, conn.effectiveTimeout(30*60))
			defer cancel()
			res, next, err := syncSAPS4(ctx, rag, s, conn, s.activeEmbedModel(), false)
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("%d records, %d deleted, %d chunks, %d skipped, %d errors", res.Records, res.Deleted, res.Chunks, res.Skipped, len(res.Errors))
			if len(res.Errors) > 0 {
				return detail, fmt.Errorf("%d SAP S/4 record(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
			}
			_ = settings.update(func(cur *appSettings) {
				if c, i, found := findConnIndex(cur.SAPS4, name); found {
					c.DeltaLink = next.DeltaLink
					c.NextLink = next.NextLink
					cur.SAPS4[i] = c
				}
			})
			return detail, nil
		})...)

	return jobs
}

// startScheduler runs R3's dynamic job list until ctx is cancelled — call
// as `go startScheduler(ctx, rag)` from main(). schedulerBuildJobs (and
// therefore the settings store) is consulted fresh every tick rather than
// once at startup, so admin-edited settings — including newly added,
// paused or removed named connections — apply without a restart.
func startScheduler(ctx context.Context, rag *ragSystem) {
	schedMu.Lock()
	schedRootCtx = ctx
	schedMu.Unlock()
	runScheduler(ctx, func() []syncJob { return schedulerBuildJobs(rag) })
}

// ─── Dashboard endpoints ─────────────────────────────────────────────────────

// handleSchedulerHistory serves the in-memory run history for every
// background job as a flat, newest-first JSON array, so the Settings UI
// can show "did the last unattended sync actually work" without an admin
// having to go dig through the server log.
func handleSchedulerHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, schedulerHistorySnapshot())
}

// schedulerJobStatus is one job's row in the dashboard: identity,
// schedule, live state and last result.
type schedulerJobStatus struct {
	Job             string        `json:"job"`
	Kind            string        `json:"kind"`
	Connection      string        `json:"connection"`
	IntervalSeconds int           `json:"interval_seconds"` // 0 = manual/ad-hoc only
	Paused          bool          `json:"paused"`
	Running         bool          `json:"running"`
	RunningSince    int64         `json:"running_since,omitempty"` // unix seconds
	NextRun         int64         `json:"next_run,omitempty"`      // unix seconds; 0 = due on the next tick (or not scheduled)
	LastRun         *schedulerRun `json:"last_run,omitempty"`
}

// handleSchedulerStatus lists every job (one per enabled connection,
// including paused and manual-only ones) with its live scheduler state —
// what the dashboard's table renders.
func handleSchedulerStatus(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobs := schedulerBuildJobs(rag)
		out := make([]schedulerJobStatus, 0, len(jobs))
		schedMu.Lock()
		for _, job := range jobs {
			st := schedulerJobStatus{
				Job:             job.Name,
				Kind:            job.Kind,
				Connection:      job.Conn,
				IntervalSeconds: job.IntervalSecs,
				Paused:          job.Paused,
			}
			if info, ok := schedRunning[job.Name]; ok {
				st.Running = true
				st.RunningSince = info.startedAt.Unix()
			}
			if !job.Paused && job.IntervalSecs > 0 {
				if next, ok := schedNextRun[job.Name]; ok {
					st.NextRun = next.Unix()
				}
			}
			out = append(out, st)
		}
		schedMu.Unlock()
		// History lookup outside schedMu — it takes its own lock.
		for i := range out {
			out[i].LastRun = lastSchedulerRun(out[i].Job)
		}
		writeJSON(w, out)
	}
}

// schedulerJobRequest addresses one job by its dashboard name for the
// run/cancel/pause actions below.
type schedulerJobRequest struct {
	Job    string `json:"job"`
	Paused bool   `json:"paused"` // pause endpoint only
}

// findSchedulerJob resolves a job name against the current settings
// snapshot — the same list status serves, so anything visible in the
// dashboard is addressable and nothing else is.
func findSchedulerJob(rag *ragSystem, name string) (syncJob, bool) {
	for _, job := range schedulerBuildJobs(rag) {
		if job.Name == name {
			return job, true
		}
	}
	return syncJob{}, false
}

// handleSchedulerRun starts one job immediately ("Jetzt ausführen") —
// works for any enabled connection, including paused ones and ones with
// no cycle configured: an explicit admin click is deliberate action, not
// the unattended rhythm Paused suspends. The run itself happens in the
// background; this returns as soon as it's launched, and the dashboard's
// status polling shows progress/result.
func handleSchedulerRun(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req schedulerJobRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Job) == "" {
			writeJSONError(w, "missing job name", http.StatusBadRequest)
			return
		}
		job, ok := findSchedulerJob(rag, req.Job)
		if !ok {
			writeJSONError(w, fmt.Sprintf("unknown job %q", req.Job), http.StatusBadRequest)
			return
		}
		if !schedulerLaunch(job, "manuell") {
			writeJSONError(w, fmt.Sprintf("job %q läuft bereits", req.Job), http.StatusConflict)
			return
		}
		logAudit(r, "scheduler_run", req.Job)
		writeJSON(w, map[string]any{"ok": true, "job": req.Job})
	}
}

// handleSchedulerCancel aborts one currently running job via its stored
// context cancel. The import loops check their context between items
// (never mid-request), so the job stops within one item and its history
// entry records the abort — see schedulerLaunch's context.Canceled
// mapping.
func handleSchedulerCancel(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req schedulerJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Job) == "" {
		writeJSONError(w, "missing job name", http.StatusBadRequest)
		return
	}
	schedMu.Lock()
	info, running := schedRunning[req.Job]
	schedMu.Unlock()
	if !running {
		writeJSONError(w, fmt.Sprintf("job %q läuft gerade nicht", req.Job), http.StatusConflict)
		return
	}
	info.cancel()
	logAudit(r, "scheduler_cancel", req.Job)
	writeJSON(w, map[string]any{"ok": true, "job": req.Job})
}

// resetConnCursor clears the named connection's server-managed resume
// cursor (SharePoint's DeltaLink / IMAP's LastUID — see their doc comments
// in settings.go), forcing the next run to do a full walk instead of
// resuming. Needed because both cursors advance past an item even when
// that item's own ingest failed (scheduler.go persists the new cursor
// unconditionally, see the sharepoint/imap job Run funcs above) — a
// transient failure (bad URL, timeout, since-fixed bug) then permanently
// skips that item, since the upstream API won't report it again unless it
// changes once more. Returns false if no such connection exists (anymore),
// or if kind has no resumable cursor at all (every other connector kind
// re-previews everything on each run, so there's nothing to reset).
func resetConnCursor(kind, connName string) bool {
	found := false
	_ = settings.update(func(cur *appSettings) {
		switch kind {
		case "sharepoint":
			if c, i, ok := findConnIndex(cur.SharePoint, connName); ok {
				c.DeltaLink = ""
				cur.SharePoint[i] = c
				found = true
			}
		case "onedrive":
			if c, i, ok := findConnIndex(cur.OneDrive, connName); ok {
				c.DeltaLink = ""
				cur.OneDrive[i] = c
				found = true
			}
		case "sap_s4":
			if c, i, ok := findConnIndex(cur.SAPS4, connName); ok {
				c.DeltaLink = ""
				c.NextLink = ""
				cur.SAPS4[i] = c
				found = true
			}
		case "imap":
			if c, i, ok := findConnIndex(cur.IMAP, connName); ok {
				c.LastUID = 0
				cur.IMAP[i] = c
				found = true
			}
		}
	})
	return found
}

// setConnPaused persists paused onto the named connection of the given
// kind — the settings-side half of handleSchedulerPause. Returns false if
// no such connection exists (anymore).
func setConnPaused(kind, connName string, paused bool) bool {
	found := false
	_ = settings.update(func(cur *appSettings) {
		switch kind {
		case "sharepoint":
			if c, i, ok := findConnIndex(cur.SharePoint, connName); ok {
				c.Paused = paused
				cur.SharePoint[i] = c
				found = true
			}
		case "exchange_graph":
			if c, i, ok := findConnIndex(cur.ExchangeGraph, connName); ok {
				c.Paused = paused
				cur.ExchangeGraph[i] = c
				found = true
			}
		case "onedrive":
			if c, i, ok := findConnIndex(cur.OneDrive, connName); ok {
				c.Paused = paused
				cur.OneDrive[i] = c
				found = true
			}
		case "imap":
			if c, i, ok := findConnIndex(cur.IMAP, connName); ok {
				c.Paused = paused
				cur.IMAP[i] = c
				found = true
			}
		case "teams":
			if c, i, ok := findConnIndex(cur.Teams, connName); ok {
				c.Paused = paused
				cur.Teams[i] = c
				found = true
			}
		case "confluence":
			if c, i, ok := findConnIndex(cur.Confluence, connName); ok {
				c.Paused = paused
				cur.Confluence[i] = c
				found = true
			}
		case "jira":
			if c, i, ok := findConnIndex(cur.Jira, connName); ok {
				c.Paused = paused
				cur.Jira[i] = c
				found = true
			}
		case "freshservice":
			if c, i, ok := findConnIndex(cur.Freshservice, connName); ok {
				c.Paused = paused
				cur.Freshservice[i] = c
				found = true
			}
		case "folder":
			if c, i, ok := findConnIndex(cur.Folder, connName); ok {
				c.Paused = paused
				cur.Folder[i] = c
				found = true
			}
		case "github":
			if c, i, ok := findConnIndex(cur.GitHub, connName); ok {
				c.Paused = paused
				cur.GitHub[i] = c
				found = true
			}
		case "sap_s4":
			if c, i, ok := findConnIndex(cur.SAPS4, connName); ok {
				c.Paused = paused
				cur.SAPS4[i] = c
				found = true
			}
		}
	})
	return found
}

// handleSchedulerPause persists a connection's Paused flag (see
// connRuntime.Paused). Pausing does NOT cancel a run already in flight —
// that's what handleSchedulerCancel is for; the two compose ("pausieren,
// dann laufenden Job abbrechen") without being conflated.
func handleSchedulerPause(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req schedulerJobRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Job) == "" {
			writeJSONError(w, "missing job name", http.StatusBadRequest)
			return
		}
		job, ok := findSchedulerJob(rag, req.Job)
		if !ok {
			writeJSONError(w, fmt.Sprintf("unknown job %q", req.Job), http.StatusBadRequest)
			return
		}
		if !setConnPaused(job.Kind, job.Conn, req.Paused) {
			writeJSONError(w, fmt.Sprintf("connection for job %q not found", req.Job), http.StatusBadRequest)
			return
		}
		action := "scheduler_pause"
		if !req.Paused {
			action = "scheduler_resume"
		}
		logAudit(r, action, req.Job)
		writeJSON(w, map[string]any{"ok": true, "job": req.Job, "paused": req.Paused})
	}
}

// handleSchedulerResetCursor clears a SharePoint/IMAP connection's
// server-managed resume cursor (see resetConnCursor) so its next run — auto
// or manual — does a full walk instead of resuming from a point that may
// have already skipped past items whose ingest failed. Rejected for any
// other connector kind, since those have no cursor to reset in the first
// place.
func handleSchedulerResetCursor(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req schedulerJobRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Job) == "" {
			writeJSONError(w, "missing job name", http.StatusBadRequest)
			return
		}
		job, ok := findSchedulerJob(rag, req.Job)
		if !ok {
			writeJSONError(w, fmt.Sprintf("unknown job %q", req.Job), http.StatusBadRequest)
			return
		}
		if job.Kind != "sharepoint" && job.Kind != "onedrive" && job.Kind != "imap" && job.Kind != "sap_s4" {
			writeJSONError(w, fmt.Sprintf("job %q has no resettable cursor", req.Job), http.StatusBadRequest)
			return
		}
		if !resetConnCursor(job.Kind, job.Conn) {
			writeJSONError(w, fmt.Sprintf("connection for job %q not found", req.Job), http.StatusBadRequest)
			return
		}
		logAudit(r, "scheduler_reset_cursor", req.Job)
		// Also record into schedulerHistory (Jobs tab's "Verlauf" list) —
		// without this the reset succeeded server-side (confirmed by the
		// 200 response the button's own click handler shows) but left NO
		// visible trace anywhere on the dashboard itself, so re-opening or
		// reloading the tab looked exactly like the click had never
		// happened. Not a real "job run" (no connector work happened), but
		// schedulerRun is the dashboard's only "something happened to this
		// job just now" record, so this is the natural place for it.
		recordSchedulerRun(schedulerRun{
			Job:       req.Job,
			StartedAt: time.Now().Unix(),
			OK:        true,
			Detail:    "Resume-Cursor zurückgesetzt — nächster Lauf beginnt von vorne.",
			Trigger:   "manuell",
		})
		writeJSON(w, map[string]any{"ok": true, "job": req.Job})
	}
}
