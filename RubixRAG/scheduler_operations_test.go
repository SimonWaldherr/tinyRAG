package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchedulerOperationsPersistHistoryAndAlerts(t *testing.T) {
	oldHistoryPath, oldAlertsPath := schedulerHistoryPath, schedulerAlertsPath
	schedulerHistoryMu.Lock()
	oldHistory := append([]schedulerRun(nil), schedulerHistory...)
	schedulerHistory = nil
	schedulerHistoryMu.Unlock()
	schedulerAlertMu.Lock()
	oldAlerts := append([]schedulerAlert(nil), schedulerAlerts...)
	oldNextID := schedulerAlertNextID
	schedulerAlerts, schedulerAlertNextID = nil, 0
	schedulerAlertMu.Unlock()
	t.Cleanup(func() {
		schedulerHistoryPath, schedulerAlertsPath = oldHistoryPath, oldAlertsPath
		schedulerHistoryMu.Lock()
		schedulerHistory = oldHistory
		schedulerHistoryMu.Unlock()
		schedulerAlertMu.Lock()
		schedulerAlerts, schedulerAlertNextID = oldAlerts, oldNextID
		schedulerAlertMu.Unlock()
	})

	dir := t.TempDir()
	if err := initSchedulerOperations(filepath.Join(dir, "history.jsonl"), filepath.Join(dir, "alerts.json")); err != nil {
		t.Fatal(err)
	}
	recordSchedulerRun(schedulerRun{Job: "imap-sync:mailbox", StartedAt: 42, OK: true, Trigger: "auto"})

	// Simulate a restart: reset the in-memory views, then load from disk.
	schedulerHistoryMu.Lock()
	schedulerHistory = nil
	schedulerHistoryMu.Unlock()
	if err := loadSchedulerHistory(); err != nil {
		t.Fatal(err)
	}
	if got := schedulerHistorySnapshot(); len(got) != 1 || got[0].Job != "imap-sync:mailbox" {
		t.Fatalf("persisted scheduler history not restored: %#v", got)
	}

	raiseSchedulerAlert("imap-sync:mailbox", "upstream timeout")
	alerts := schedulerAlertsSnapshot()
	if len(alerts) != 1 || alerts[0].ResolvedAt != 0 {
		t.Fatalf("want one active alert, got %#v", alerts)
	}
	if !acknowledgeSchedulerAlert(alerts[0].ID) || !schedulerAlertsSnapshot()[0].Acknowledged {
		t.Fatal("acknowledging alert must persist in the in-memory state")
	}
	resolveSchedulerAlerts("imap-sync:mailbox")
	if schedulerAlertsSnapshot()[0].ResolvedAt == 0 {
		t.Fatal("successful job must resolve its active alert")
	}
}

func TestMetricsRequiresTokenAndExposesSchedulerSeries(t *testing.T) {
	schedulerMetrics.Lock()
	oldRuns := schedulerMetrics.runs
	oldFailures := schedulerMetrics.failures
	oldDurations := schedulerMetrics.durations
	oldRunning := schedulerMetrics.running
	schedulerMetrics.runs = map[string]int64{}
	schedulerMetrics.failures = map[string]int64{}
	schedulerMetrics.durations = map[string]schedulerDurationMetric{}
	schedulerMetrics.running = map[string]bool{}
	schedulerMetrics.Unlock()
	t.Cleanup(func() {
		schedulerMetrics.Lock()
		schedulerMetrics.runs, schedulerMetrics.failures = oldRuns, oldFailures
		schedulerMetrics.durations, schedulerMetrics.running = oldDurations, oldRunning
		schedulerMetrics.Unlock()
	})

	t.Setenv(metricsTokenEnv, "metrics-test-token")
	metricSchedulerStarted("imap-sync:mailbox")
	metricSchedulerFinished("imap-sync:mailbox", 1.25, false)

	denied := httptest.NewRecorder()
	handleMetrics(denied, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("metrics without token status=%d, want 404", denied.Code)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer metrics-test-token")
	handleMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`r3_scheduler_runs_total{job="imap-sync:mailbox"} 1`,
		`r3_scheduler_failures_total{job="imap-sync:mailbox"} 1`,
		`r3_scheduler_run_duration_seconds_count{job="imap-sync:mailbox"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("metrics output missing %q:\n%s", want, rec.Body.String())
		}
	}
}
