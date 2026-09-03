package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRestyleDraftTextUnknownStyle(t *testing.T) {
	lm := newLMClientFull("local", "http://unused.invalid", "", "embed", "chat", "")
	if _, err := restyleDraftText(context.Background(), lm, "Guten Tag,\n\nvielen Dank.", "sarkastisch"); err == nil {
		t.Fatal("expected an error for an unrecognized style key")
	}
}

func TestRestyleDraftTextRewritesViaChatStream(t *testing.T) {
	var gotSystem string
	var gotUserMsg string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			if m.Role == "system" {
				gotSystem = m.Content
			}
			if m.Role == "user" {
				gotUserMsg = m.Content
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hallo Team, \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"danke dir!\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	lm := newLMClientFull("local", server.URL, "", "embed", "chat", "")
	original := "Guten Tag,\n\nvielen Dank fuer Ihre Anfrage."
	got, err := restyleDraftText(context.Background(), lm, original, "kollegial")
	if err != nil {
		t.Fatalf("restyleDraftText: %v", err)
	}
	if got != "Hallo Team, danke dir!" {
		t.Fatalf("want the rewritten text, got %q", got)
	}
	if gotUserMsg != original {
		t.Fatalf("want the original draft text passed through verbatim as the user message, got %q", gotUserMsg)
	}
	if !strings.Contains(gotSystem, "kollegial") {
		t.Fatalf("want the style name in the system prompt, got %q", gotSystem)
	}
}

// TestHandleDraftReplyStreamsStepsAndDraft confirms handleDraftReply's new
// NDJSON contract: at least one line decodes, and the last one is
// type="done" carrying the complete draftReply (subject split off, reply
// text populated) — replacing the single buffered JSON object this
// endpoint used to return before Mail was converted to streaming so
// "Thinking"/"Using SQL" can show live (see draftStreamMsg's doc comment,
// handlers.go).
func TestHandleDraftReplyStreamsStepsAndDraft(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One non-streaming decision round with no tool_calls: composeNewMail
		// always offers the knowledge-base tools (buildMailTools), so
		// chatWithToolsBudget's first move is chatOnce, not a raw stream —
		// answering directly here short-circuits it to a single call.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Betreff: Test\n\nHallo, das ist ein Testentwurf."}}]}`))
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", server.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(draftReplyRequest{Brief: "Schreibe eine kurze Testmail."})
	rec := httptest.NewRecorder()
	handleDraftReply(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/draft/reply", bytes.NewReader(body)))

	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("want Content-Type application/x-ndjson, got %q", ct)
	}

	dec := json.NewDecoder(rec.Body)
	var lastMsg draftStreamMsg
	lineCount := 0
	for {
		var msg draftStreamMsg
		if err := dec.Decode(&msg); err != nil {
			break
		}
		lineCount++
		lastMsg = msg
	}
	if lineCount == 0 {
		t.Fatal("expected at least one NDJSON line, got none")
	}
	if lastMsg.Type != "done" {
		t.Fatalf("want the last line to be type=done, got %+v", lastMsg)
	}
	if lastMsg.Draft == nil || strings.TrimSpace(lastMsg.Draft.ReplyText) == "" {
		t.Fatalf("want a non-empty draft on the done line, got %+v", lastMsg.Draft)
	}
	if lastMsg.Draft.Subject != "Test" {
		t.Fatalf("want subject %q, got %q", "Test", lastMsg.Draft.Subject)
	}
	if lastMsg.Draft.ReplyText != "Hallo, das ist ein Testentwurf." {
		t.Fatalf("want the reply body, got %q", lastMsg.Draft.ReplyText)
	}
}

// TestHandleDraftReplyFoldsInOptedInPersonalContext proves the Phase 4
// wiring end-to-end: a logged-in, opted-in user's personal-context fields
// (userprefs.go) reach the model's prompt when generating a mail draft,
// via effectiveInstructions — not a separate code path, so this also pins
// down that req.Instructions and personal context combine rather than one
// overwriting the other.
func TestHandleDraftReplyFoldsInOptedInPersonalContext(t *testing.T) {
	var sawUserContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []chatMsg `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "user" {
				sawUserContent += m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Hallo, danke fuer Ihre Anfrage."}}]}`))
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", server.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.PersonalizeAnswers = true
	withTestGlobalSettings(t, s)
	withTestUserPrefsDB(t)
	if err := userPrefsDB.setPersonalContext("Any User", userPrefs{
		CommunicationStyle: "MARKER-KOMMUNIKATIONSSTIL",
		UsePersonalContext: true,
	}); err != nil {
		t.Fatalf("setPersonalContext: %v", err)
	}

	body, _ := json.Marshal(draftReplyRequest{RawEmail: "Wann kommt meine Bestellung an?", Instructions: "MARKER-SITUATIVER-HINWEIS"})
	r := httptest.NewRequest(http.MethodPost, "/api/draft/reply", bytes.NewReader(body))
	sessW := httptest.NewRecorder()
	issueSession(sessW, &ldapUser{CN: "Any User"})
	for _, c := range sessW.Result().Cookies() {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handleDraftReply(rag)(rec, r)

	if rec.Code != http.StatusOK && rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sawUserContent, "MARKER-SITUATIVER-HINWEIS") {
		t.Fatalf("want the request's own Instructions present in the prompt, got %q", sawUserContent)
	}
	if !strings.Contains(sawUserContent, "MARKER-KOMMUNIKATIONSSTIL") {
		t.Fatalf("want the opted-in personal context present in the prompt, got %q", sawUserContent)
	}
}

// TestHandleDraftReplyOmitsPersonalContextWithoutOptIn is the negative
// counterpart: PersonalizeAnswers alone (without the user's own opt-in)
// must never fold personal-context fields into a draft.
func TestHandleDraftReplyOmitsPersonalContextWithoutOptIn(t *testing.T) {
	var sawUserContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []chatMsg `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "user" {
				sawUserContent += m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Hallo."}}]}`))
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", server.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.PersonalizeAnswers = true
	withTestGlobalSettings(t, s)
	withTestUserPrefsDB(t)
	if err := userPrefsDB.setPersonalContext("Any User", userPrefs{
		CommunicationStyle: "MARKER-KOMMUNIKATIONSSTIL",
		UsePersonalContext: false, // not opted in
	}); err != nil {
		t.Fatalf("setPersonalContext: %v", err)
	}

	body, _ := json.Marshal(draftReplyRequest{RawEmail: "Wann kommt meine Bestellung an?"})
	r := httptest.NewRequest(http.MethodPost, "/api/draft/reply", bytes.NewReader(body))
	sessW := httptest.NewRecorder()
	issueSession(sessW, &ldapUser{CN: "Any User"})
	for _, c := range sessW.Result().Cookies() {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handleDraftReply(rag)(rec, r)

	if strings.Contains(sawUserContent, "MARKER-KOMMUNIKATIONSSTIL") {
		t.Fatalf("must NOT fold personal context in without the user's own opt-in, got %q", sawUserContent)
	}
}

func TestHandleDraftRestyleEndToEnd(t *testing.T) {
	path := withTestAuditLog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Umformuliert.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", server.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	withTestGlobalSettings(t, s)

	handler := handleDraftRestyle(rag)
	body, _ := json.Marshal(draftRestyleRequest{Text: "Alter Text.", Style: "distanziert"})
	r := httptest.NewRequest(http.MethodPost, "/api/draft/restyle", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["text"] != "Umformuliert." {
		t.Fatalf("want the restyled text, got %+v", out)
	}

	events := readAuditEvents(t, path)
	if len(events) != 1 || events[0].Action != "draft_restyle" || events[0].Detail != "style=distanziert" {
		t.Fatalf("want one draft_restyle audit event, got %+v", events)
	}
}

func TestHandleDraftRestyleRejectsUnknownStyle(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)
	handler := handleDraftRestyle(rag)
	body, _ := json.Marshal(draftRestyleRequest{Text: "Text.", Style: "wuetend"})
	r := httptest.NewRequest(http.MethodPost, "/api/draft/restyle", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown style, got %d", w.Code)
	}
}

func TestHandleDraftRestyleRejectsEmptyText(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)
	handler := handleDraftRestyle(rag)
	body, _ := json.Marshal(draftRestyleRequest{Text: "   ", Style: "professionell"})
	r := httptest.NewRequest(http.MethodPost, "/api/draft/restyle", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for empty text, got %d", w.Code)
	}
}
