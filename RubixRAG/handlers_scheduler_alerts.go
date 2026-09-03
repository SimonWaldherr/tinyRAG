package main

import (
	"encoding/json"
	"net/http"
)

func handleSchedulerAlerts(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, schedulerAlertsSnapshot())
}

type schedulerAlertAcknowledgeRequest struct {
	ID int64 `json:"id"`
}

func handleSchedulerAlertAcknowledge(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req schedulerAlertAcknowledgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		writeJSONError(w, "missing alert id", http.StatusBadRequest)
		return
	}
	if !acknowledgeSchedulerAlert(req.ID) {
		writeJSONError(w, "alert not found", http.StatusNotFound)
		return
	}
	logAudit(r, "scheduler_alert_ack", "id="+itoa(int(req.ID)))
	writeJSON(w, map[string]bool{"ok": true})
}
