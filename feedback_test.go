package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func newFeedbackTestRAG(t *testing.T) *ragSystem {
	t.Helper()
	r, err := newRAG(r3MockLM{}, 3, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	settings = &settingsStore{s: appSettings{EmbedModel: "embed", ActiveRole: "it"}}
	meta := R3IngestMetadata{DocumentID: "feedback-doc", SourceSystem: "test", SourceType: "document", SourceTitle: "Feedback document"}
	if err := r.addChunksWithMetadata("Feedback document", []string{"first", "second"}, "embed", []string{"it"}, meta); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecordFeedbackReplacesVoteAndLeavesRankingSignalUntouched(t *testing.T) {
	r := newFeedbackTestRAG(t)
	refs := []feedbackCitationRef{{DocumentID: "feedback-doc", ChunkID: "feedback-doc:0"}, {DocumentID: "feedback-doc", ChunkID: "feedback-doc:1"}}
	if recorded, err := r.recordFeedback("req-1", "alice", 1, refs); err != nil || recorded != 1 {
		t.Fatalf("record feedback = %d, %v", recorded, err)
	}
	if recorded, err := r.recordFeedback("req-1", "alice", -1, refs); err != nil || recorded != 1 {
		t.Fatalf("replace feedback = %d, %v", recorded, err)
	}
	summary, err := r.feedbackSummaries(10)
	if err != nil || len(summary) != 1 || summary[0].Votes != 1 || summary[0].NetRating != -1 {
		t.Fatalf("feedback summary = %#v, %v", summary, err)
	}
	stmt, _ := tinysql.ParseSQL("SELECT feedback_score FROM chunks WHERE document_id = 'feedback-doc'")
	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
	if err != nil || len(rs.Rows) == 0 {
		t.Fatalf("read feedback score: %v", err)
	}
	if score, _ := tinysql.GetVal(rs.Rows[0], "feedback_score"); score != 0.5 {
		t.Fatalf("feedback collection changed live ranking signal: %#v", score)
	}
}

func TestFeedbackHandlerRejectsInvalidRequest(t *testing.T) {
	r := newFeedbackTestRAG(t)
	body, _ := json.Marshal(feedbackRequest{Rating: "invalid"})
	req := httptest.NewRequest(http.MethodPost, "/api/feedback", bytes.NewReader(body))
	res := httptest.NewRecorder()
	feedbackHandler(r, settings)(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusBadRequest)
	}
}

func TestFeedbackHandlerStoresOnlyVisibleCitations(t *testing.T) {
	r := newFeedbackTestRAG(t)
	visibleBody, _ := json.Marshal(feedbackRequest{
		RequestID: "req-visible",
		Rating:    "up",
		Citations: []feedbackCitationRef{{DocumentID: "feedback-doc", ChunkID: "feedback-doc:0"}},
	})
	visibleReq := httptest.NewRequest(http.MethodPost, "/api/feedback", bytes.NewReader(visibleBody))
	visibleRes := httptest.NewRecorder()
	feedbackHandler(r, settings)(visibleRes, visibleReq)
	if visibleRes.Code != http.StatusOK {
		t.Fatalf("visible feedback status = %d, body=%s", visibleRes.Code, visibleRes.Body.String())
	}

	hiddenMeta := R3IngestMetadata{DocumentID: "hidden-doc", SourceSystem: "test", SourceType: "document", SourceTitle: "Hidden document"}
	if err := r.addChunksWithMetadata("Hidden document", []string{"hidden content"}, "embed", []string{"hr"}, hiddenMeta); err != nil {
		t.Fatal(err)
	}
	hiddenBody, _ := json.Marshal(feedbackRequest{
		RequestID: "req-hidden",
		Rating:    "down",
		Citations: []feedbackCitationRef{{DocumentID: "hidden-doc", ChunkID: "hidden-doc:0"}},
	})
	hiddenReq := httptest.NewRequest(http.MethodPost, "/api/feedback", bytes.NewReader(hiddenBody))
	hiddenRes := httptest.NewRecorder()
	feedbackHandler(r, settings)(hiddenRes, hiddenReq)
	if hiddenRes.Code != http.StatusBadRequest {
		t.Fatalf("hidden feedback status = %d, want %d", hiddenRes.Code, http.StatusBadRequest)
	}

	summary, err := r.feedbackSummaries(10)
	if err != nil || len(summary) != 1 || summary[0].DocumentID != "feedback-doc" {
		t.Fatalf("feedback summary = %#v, %v", summary, err)
	}
}
