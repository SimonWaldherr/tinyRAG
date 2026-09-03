package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Scheduler history used to be a restart-local dashboard convenience. It is
// now a bounded JSONL snapshot so an operator can distinguish a fresh restart
// from a connector that has simply never run, without an unbounded file on a
// long-lived server. Alerts use a compact JSON snapshot so acknowledgement/
// resolution state can be updated atomically.
var (
	schedulerHistoryPath string
	schedulerAlertsPath  string
	schedulerAlertMu     sync.Mutex
	schedulerAlerts      []schedulerAlert
	schedulerAlertNextID int64
)

type schedulerAlert struct {
	ID             int64  `json:"id"`
	Job            string `json:"job"`
	Severity       string `json:"severity"`
	Message        string `json:"message"`
	CreatedAt      int64  `json:"created_at"`
	Acknowledged   bool   `json:"acknowledged"`
	AcknowledgedAt int64  `json:"acknowledged_at,omitempty"`
	ResolvedAt     int64  `json:"resolved_at,omitempty"`
}

func initSchedulerOperations(historyPath, alertsPath string) error {
	schedulerHistoryPath = historyPath
	schedulerAlertsPath = alertsPath
	if err := loadSchedulerHistory(); err != nil {
		return err
	}
	return loadSchedulerAlerts()
}

func loadSchedulerHistory() error {
	if schedulerHistoryPath == "" {
		return nil
	}
	f, err := os.Open(schedulerHistoryPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open scheduler history: %w", err)
	}
	defer f.Close()
	var runs []schedulerRun
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var run schedulerRun
		if json.Unmarshal(scanner.Bytes(), &run) == nil && run.Job != "" {
			runs = append(runs, run)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read scheduler history: %w", err)
	}
	if len(runs) > schedulerHistoryLimit {
		runs = runs[len(runs)-schedulerHistoryLimit:]
	}
	// JSONL is chronological; the in-memory UI uses newest-first.
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	schedulerHistoryMu.Lock()
	schedulerHistory = runs
	schedulerHistoryMu.Unlock()
	return nil
}

// persistSchedulerHistoryLocked atomically replaces the bounded JSONL history.
// schedulerHistory is newest-first in memory, while JSONL remains chronological
// so it can also be inspected comfortably outside the UI. The caller holds
// schedulerHistoryMu, ensuring concurrent jobs cannot overwrite each other's
// just-recorded runs.
func persistSchedulerHistoryLocked(runs []schedulerRun) {
	if schedulerHistoryPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(schedulerHistoryPath), 0o755); err != nil {
		log.Printf("WARN: scheduler history directory: %v", err)
		return
	}
	var b strings.Builder
	for i := len(runs) - 1; i >= 0; i-- {
		line, err := json.Marshal(runs[i])
		if err != nil {
			log.Printf("WARN: marshal scheduler history: %v", err)
			return
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := schedulerHistoryPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		log.Printf("WARN: scheduler history write: %v", err)
		return
	}
	if err := os.Rename(tmp, schedulerHistoryPath); err != nil {
		log.Printf("WARN: scheduler history replace: %v", err)
	}
}

func loadSchedulerAlerts() error {
	if schedulerAlertsPath == "" {
		return nil
	}
	b, err := os.ReadFile(schedulerAlertsPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read scheduler alerts: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	var alerts []schedulerAlert
	if err := json.Unmarshal(b, &alerts); err != nil {
		return fmt.Errorf("parse scheduler alerts: %w", err)
	}
	var maxID int64
	for _, a := range alerts {
		if a.ID > maxID {
			maxID = a.ID
		}
	}
	schedulerAlertMu.Lock()
	schedulerAlerts, schedulerAlertNextID = alerts, maxID
	schedulerAlertMu.Unlock()
	return nil
}

func schedulerAlertsSnapshot() []schedulerAlert {
	schedulerAlertMu.Lock()
	defer schedulerAlertMu.Unlock()
	out := append([]schedulerAlert(nil), schedulerAlerts...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func raiseSchedulerAlert(job, message string) {
	if strings.TrimSpace(job) == "" || strings.TrimSpace(message) == "" {
		return
	}
	now := time.Now().Unix()
	schedulerAlertMu.Lock()
	// One active alert per job keeps a retry storm from producing hundreds
	// of identical red banners. The latest failure remains visible.
	for i := range schedulerAlerts {
		a := &schedulerAlerts[i]
		if a.Job == job && a.ResolvedAt == 0 {
			a.Message, a.CreatedAt, a.Acknowledged, a.AcknowledgedAt = message, now, false, 0
			saveSchedulerAlertsLocked()
			schedulerAlertMu.Unlock()
			pushAdminNotification("scheduler_alert", job+": "+message)
			return
		}
	}
	schedulerAlertNextID++
	schedulerAlerts = append(schedulerAlerts, schedulerAlert{ID: schedulerAlertNextID, Job: job, Severity: "error", Message: message, CreatedAt: now})
	saveSchedulerAlertsLocked()
	schedulerAlertMu.Unlock()
	pushAdminNotification("scheduler_alert", job+": "+message)
}

func resolveSchedulerAlerts(job string) {
	now := time.Now().Unix()
	changed := false
	schedulerAlertMu.Lock()
	for i := range schedulerAlerts {
		if schedulerAlerts[i].Job == job && schedulerAlerts[i].ResolvedAt == 0 {
			schedulerAlerts[i].ResolvedAt = now
			changed = true
		}
	}
	if changed {
		saveSchedulerAlertsLocked()
	}
	schedulerAlertMu.Unlock()
}

func acknowledgeSchedulerAlert(id int64) bool {
	schedulerAlertMu.Lock()
	defer schedulerAlertMu.Unlock()
	for i := range schedulerAlerts {
		if schedulerAlerts[i].ID == id {
			if !schedulerAlerts[i].Acknowledged {
				schedulerAlerts[i].Acknowledged = true
				schedulerAlerts[i].AcknowledgedAt = time.Now().Unix()
				saveSchedulerAlertsLocked()
			}
			return true
		}
	}
	return false
}

func saveSchedulerAlertsLocked() {
	if schedulerAlertsPath == "" {
		return
	}
	b, err := json.MarshalIndent(schedulerAlerts, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(schedulerAlertsPath), 0o755); err != nil {
		log.Printf("WARN: scheduler alerts directory: %v", err)
		return
	}
	tmp := schedulerAlertsPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		if err := os.Rename(tmp, schedulerAlertsPath); err != nil {
			log.Printf("WARN: scheduler alerts write: %v", err)
		}
	}
}
