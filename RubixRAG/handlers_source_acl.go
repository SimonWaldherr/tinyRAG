package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// sourceACLRequest is intentionally small and explicit: source ACLs are a
// narrow layer over existing SourceAccess, not an alternate general-purpose
// authorization system. Empty lists remove the per-document rule and make
// the source inherit the source-kind policy again.
type sourceACLRequest struct {
	SourceID    string   `json:"source_id"`
	Departments []string `json:"departments,omitempty"`
	Users       []string `json:"users,omitempty"`
}

func handleSourceACL(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sourceID := strings.TrimSpace(r.URL.Query().Get("source_id"))
			if sourceID == "" {
				writeJSONError(w, "missing source_id", http.StatusBadRequest)
				return
			}
			if _, ok := rag.fetchSourceKind(sourceID); !ok {
				writeJSONError(w, "source not found", http.StatusNotFound)
				return
			}
			rule, configured := rag.sourceACLs.get(sourceID)
			writeJSON(w, map[string]any{"source_id": sourceID, "configured": configured, "acl": rule})
		case http.MethodPost:
			var req sourceACLRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
				return
			}
			req.SourceID = strings.TrimSpace(req.SourceID)
			if req.SourceID == "" {
				writeJSONError(w, "missing source_id", http.StatusBadRequest)
				return
			}
			if _, ok := rag.fetchSourceKind(req.SourceID); !ok {
				writeJSONError(w, "source not found", http.StatusNotFound)
				return
			}
			rule := sourceACL{Departments: req.Departments, Users: req.Users}
			if err := rag.sourceACLs.set(req.SourceID, rule); err != nil {
				writeJSONError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			rule, configured := rag.sourceACLs.get(req.SourceID)
			logAudit(r, "source_acl_save", fmt.Sprintf("source_id=%s departments=%d users=%d configured=%v", req.SourceID, len(rule.Departments), len(rule.Users), configured))
			writeJSON(w, map[string]any{"ok": true, "configured": configured, "acl": rule})
		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSONError(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
