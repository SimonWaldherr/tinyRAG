package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// feedbackCitationRef deliberately contains only stable source identifiers.
// Feedback collection must not duplicate answer text, questions, URLs, tool
// inputs, or other potentially sensitive prompt material.
type feedbackCitationRef struct {
	DocumentID string `json:"document_id"`
	ChunkID    string `json:"chunk_id,omitempty"`
}

type feedbackRequest struct {
	RequestID string                `json:"request_id"`
	Rating    string                `json:"rating"`
	Citations []feedbackCitationRef `json:"citations"`
}

type feedbackSummary struct {
	DocumentID string `json:"document_id"`
	Votes      int    `json:"votes"`
	NetRating  int    `json:"net_rating"`
}

func feedbackActor(r *http.Request, s appSettings) string {
	if session := sessionFromRequest(r); session != nil && strings.TrimSpace(session.Username) != "" {
		return session.Username
	}
	if apiUser, ok := authenticateAPIUser(r, s.APIUsers); ok && strings.TrimSpace(apiUser.ID) != "" {
		return "api:" + apiUser.ID
	}
	return "anonymous"
}

func normalizeFeedbackRefs(refs []feedbackCitationRef) []feedbackCitationRef {
	seen := make(map[string]bool, len(refs))
	result := make([]feedbackCitationRef, 0, len(refs))
	for _, ref := range refs {
		ref.DocumentID = strings.TrimSpace(ref.DocumentID)
		ref.ChunkID = strings.TrimSpace(ref.ChunkID)
		if ref.DocumentID == "" {
			continue
		}
		// One vote per document avoids overweighting a source merely because
		// its context window contains several adjacent chunks.
		if seen[ref.DocumentID] {
			continue
		}
		seen[ref.DocumentID] = true
		result = append(result, ref)
	}
	return result
}

func feedbackRating(raw string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "up", "positive":
		return 1, true
	case "down", "negative":
		return -1, true
	default:
		return 0, false
	}
}

// visibleFeedbackRefs accepts only documents the active role can retrieve.
// This prevents a caller from attaching feedback to an arbitrary hidden
// document ID. The endpoint still does not use feedback for ranking.
func (r *ragSystem) visibleFeedbackRefs(refs []feedbackCitationRef, role string) []feedbackCitationRef {
	refs = normalizeFeedbackRefs(refs)
	if len(refs) == 0 {
		return nil
	}
	allowed := make([]feedbackCitationRef, 0, len(refs))
	filter := roleAndACLFilterSQL(role)
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	for _, ref := range refs {
		where := fmt.Sprintf("document_id = '%s'", escapeSQ(ref.DocumentID))
		if ref.ChunkID != "" {
			where += fmt.Sprintf(" AND chunk_id = '%s'", escapeSQ(ref.ChunkID))
		}
		stmt, err := tinysql.ParseSQL(fmt.Sprintf("SELECT document_id FROM chunks WHERE %s AND %s LIMIT 1", where, filter))
		if err != nil {
			continue
		}
		rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
		if err == nil && rs != nil && len(rs.Rows) > 0 {
			allowed = append(allowed, ref)
		}
	}
	return allowed
}

// recordFeedback replaces the caller's previous vote for the same request and
// document. It intentionally leaves chunks.feedback_score unchanged: a later,
// explicitly reviewed aggregation step can decide when real feedback volume is
// sufficient to influence retrieval.
func (r *ragSystem) recordFeedback(requestID, actor string, rating int, refs []feedbackCitationRef) (int, error) {
	requestID = strings.TrimSpace(requestID)
	actor = strings.TrimSpace(actor)
	if requestID == "" || actor == "" || (rating != 1 && rating != -1) {
		return 0, fmt.Errorf("invalid feedback event")
	}
	refs = normalizeFeedbackRefs(refs)
	if len(refs) == 0 {
		return 0, fmt.Errorf("feedback requires at least one cited document")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	for _, ref := range refs {
		deleteSQL := fmt.Sprintf(
			"DELETE FROM r3_feedback_events WHERE request_id = '%s' AND actor = '%s' AND document_id = '%s'",
			escapeSQ(requestID), escapeSQ(actor), escapeSQ(ref.DocumentID),
		)
		if stmt, err := tinysql.ParseSQL(deleteSQL); err == nil {
			if _, err := tinysql.Execute(context.Background(), r.db, "default", stmt); err != nil {
				return 0, err
			}
		}
		eventID := stableContentHash(requestID + "\x00" + actor + "\x00" + ref.DocumentID)[:24]
		insertSQL := fmt.Sprintf(
			"INSERT INTO r3_feedback_events VALUES ('%s','%s','%s',%d,'%s','%s','%s')",
			escapeSQ(eventID), escapeSQ(requestID), escapeSQ(actor), rating,
			escapeSQ(ref.DocumentID), escapeSQ(ref.ChunkID), escapeSQ(now),
		)
		stmt, err := tinysql.ParseSQL(insertSQL)
		if err != nil {
			return 0, err
		}
		if _, err := tinysql.Execute(context.Background(), r.db, "default", stmt); err != nil {
			return 0, err
		}
	}
	return len(refs), nil
}

func (r *ragSystem) feedbackSummaries(limit int) ([]feedbackSummary, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	stmt, err := tinysql.ParseSQL(fmt.Sprintf(
		"SELECT document_id, COUNT(*) AS votes, SUM(rating) AS net_rating FROM r3_feedback_events GROUP BY document_id ORDER BY votes DESC LIMIT %d", limit,
	))
	if err != nil {
		return nil, err
	}
	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
	if err != nil {
		return nil, err
	}
	result := make([]feedbackSummary, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		documentID, _ := tinysql.GetVal(row, "document_id")
		votes, _ := tinysql.GetVal(row, "votes")
		netRating, _ := tinysql.GetVal(row, "net_rating")
		result = append(result, feedbackSummary{
			DocumentID: fmt.Sprint(documentID),
			Votes:      intFeedbackValue(votes),
			NetRating:  intFeedbackValue(netRating),
		})
	}
	return result, nil
}

func intFeedbackValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func feedbackHandler(rag *ragSystem, settings *settingsStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req feedbackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.RequestID) == "" {
			http.Error(w, "request_id is required", http.StatusBadRequest)
			return
		}
		rating, ok := feedbackRating(req.Rating)
		if !ok {
			http.Error(w, "rating must be up or down", http.StatusBadRequest)
			return
		}
		if len(req.Citations) > 16 {
			http.Error(w, "too many citations", http.StatusBadRequest)
			return
		}
		s := settings.get()
		refs := rag.visibleFeedbackRefs(req.Citations, s.ActiveRole)
		if len(refs) == 0 {
			http.Error(w, "no accessible cited documents", http.StatusBadRequest)
			return
		}
		actor := feedbackActor(r, s)
		recorded, err := rag.recordFeedback(req.RequestID, actor, rating, refs)
		if err != nil {
			http.Error(w, "could not record feedback", http.StatusInternalServerError)
			return
		}
		rag.logR3Audit(AuditEvent{
			EventType:   "retrieval_feedback",
			Actor:       actor,
			EntityType:  "document",
			EntityID:    refs[0].DocumentID,
			Decision:    req.Rating,
			PolicyClass: "quality_signal",
			Details:     fmt.Sprintf("request=%s documents=%d", strings.TrimSpace(req.RequestID), recorded),
		})
		writeJSON(w, map[string]any{"ok": true, "recorded": recorded})
	}
}
