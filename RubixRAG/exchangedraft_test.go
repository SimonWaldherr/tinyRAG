package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ─── createExchangeGraphDraft (Task B: draft-only write) ───────────────────

func testExchangeGraphConfigForDrafts() exchangeGraphConfig {
	cfg := testExchangeGraphConfig()
	cfg.EnableDraftReplies = true
	return cfg
}

// TestCreateExchangeGraphDraftRefusesWhenDisabled confirms the function
// itself enforces EnableDraftReplies rather than relying solely on callers
// to check it — no HTTP call is made at all when it's off (the default).
func TestCreateExchangeGraphDraftRefusesWhenDisabled(t *testing.T) {
	called := false
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	cfg := testExchangeGraphConfig() // EnableDraftReplies left false (default)

	_, err := createExchangeGraphDraft(context.Background(), cfg, "msg-1", "Hallo, danke fuer Ihre Anfrage.")
	if err == nil {
		t.Fatal("want an error when EnableDraftReplies is off")
	}
	if called {
		t.Fatal("want no Graph call at all when EnableDraftReplies is off")
	}
}

// TestCreateExchangeGraphDraftOnlyEverCallsDraftEndpoints is the core
// safety-invariant test: it records every path the fake Graph server sees
// and asserts (a) createReply then a PATCH of the resulting draft, in that
// order, (b) the returned draft ID matches createReply's response, and
// (c) at no point does any request path contain "send" — the HARD SAFETY
// INVARIANT this whole feature exists under (R3 never sends mail
// automatically, or at all).
func TestCreateExchangeGraphDraftOnlyEverCallsDraftEndpoints(t *testing.T) {
	var calls []struct {
		method, path string
		body         []byte
	}
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		calls = append(calls, struct {
			method, path string
			body         []byte
		}{r.Method, r.URL.Path, body})

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/createReply"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "draft-abc-123", "subject": "RE: Anfrage"}`))
		case strings.Contains(r.URL.Path, "/messages/draft-abc-123"):
			if r.Method != http.MethodPatch {
				t.Fatalf("want PATCH on the draft, got %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id": "draft-abc-123"}`))
		default:
			t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
	})

	cfg := testExchangeGraphConfigForDrafts()
	draftID, err := createExchangeGraphDraft(context.Background(), cfg, "original-msg-1", "Vielen Dank fuer Ihre Nachricht.")
	if err != nil {
		t.Fatalf("createExchangeGraphDraft: %v", err)
	}
	if draftID != "draft-abc-123" {
		t.Fatalf("want the created draft's id returned, got %q", draftID)
	}

	if len(calls) != 2 {
		t.Fatalf("want exactly 2 Graph calls (createReply, PATCH), got %d: %+v", len(calls), calls)
	}
	if !strings.HasSuffix(calls[0].path, "/messages/original-msg-1/createReply") || calls[0].method != http.MethodPost {
		t.Fatalf("want call 1 = POST .../createReply, got %+v", calls[0])
	}
	if !strings.Contains(calls[1].path, "/messages/draft-abc-123") || calls[1].method != http.MethodPatch {
		t.Fatalf("want call 2 = PATCH the draft, got %+v", calls[1])
	}
	for _, c := range calls {
		if strings.Contains(strings.ToLower(c.path), "send") {
			t.Fatalf("HARD SAFETY INVARIANT VIOLATED: a request path contained \"send\": %+v", c)
		}
	}

	var patchBody struct {
		Body struct {
			ContentType string `json:"contentType"`
			Content     string `json:"content"`
		} `json:"body"`
	}
	if err := json.Unmarshal(calls[1].body, &patchBody); err != nil {
		t.Fatalf("decode PATCH body: %v", err)
	}
	if patchBody.Body.Content != "Vielen Dank fuer Ihre Nachricht." {
		t.Fatalf("want the generated reply text patched into the draft body, got %q", patchBody.Body.Content)
	}
}

// TestCreateExchangeGraphDraftPropagatesCreateReplyError confirms a Graph
// API error on the first call (createReply) is returned to the caller,
// not swallowed — and that the PATCH is never attempted since there's no
// draft ID to patch.
func TestCreateExchangeGraphDraftPropagatesCreateReplyError(t *testing.T) {
	patchCalled := false
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/createReply") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"message": "message not found"}}`))
			return
		}
		patchCalled = true
		w.WriteHeader(http.StatusOK)
	})

	cfg := testExchangeGraphConfigForDrafts()
	_, err := createExchangeGraphDraft(context.Background(), cfg, "missing-msg", "Text")
	if err == nil {
		t.Fatal("want the createReply error propagated, got nil")
	}
	if patchCalled {
		t.Fatal("want no PATCH attempted when createReply itself failed")
	}
}

// TestCreateExchangeGraphDraftPropagatesPatchError confirms a failure on
// the second call (PATCH) is also returned, not swallowed, even though the
// draft itself was already created — the caller needs to know the body
// never actually got set.
func TestCreateExchangeGraphDraftPropagatesPatchError(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/createReply") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "draft-xyz"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "boom"}`))
	})

	orig := graphMaxRetries
	graphMaxRetries = 0 // don't sleep through retries for this deliberately-failing PATCH
	t.Cleanup(func() { graphMaxRetries = orig })

	cfg := testExchangeGraphConfigForDrafts()
	_, err := createExchangeGraphDraft(context.Background(), cfg, "msg-2", "Text")
	if err == nil {
		t.Fatal("want the PATCH error propagated, got nil")
	}
}

// TestCreateExchangeGraphDraftAuditsEveryDraft confirms an audit-log entry
// is recorded for a successfully created draft (audit.go's established
// pattern) — never the draft's actual content, just enough to know what
// happened.
func TestCreateExchangeGraphDraftAuditsEveryDraft(t *testing.T) {
	path := withTestAuditLog(t)
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/createReply") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "draft-999"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id": "draft-999"}`))
	})

	cfg := testExchangeGraphConfigForDrafts()
	if _, err := createExchangeGraphDraft(context.Background(), cfg, "msg-3", "Text"); err != nil {
		t.Fatalf("createExchangeGraphDraft: %v", err)
	}

	events := readAuditEvents(t, path)
	if len(events) != 1 || events[0].Action != "exchange_draft_created" {
		t.Fatalf("want one exchange_draft_created audit event, got %+v", events)
	}
	if !strings.Contains(events[0].Detail, "draft-999") || !strings.Contains(events[0].Detail, "msg-3") {
		t.Fatalf("want the draft/message ids in the audit detail, got %q", events[0].Detail)
	}
	if strings.Contains(events[0].Detail, "Text") {
		t.Fatalf("audit detail must never include the draft's actual body text, got %q", events[0].Detail)
	}
}

// ─── matchAutoDraftRule (Task C: pure rule-matching logic) ─────────────────

func TestMatchAutoDraftRuleExternalSenderClassicCase(t *testing.T) {
	rules := []exchangeAutoDraftRule{
		{PatternField: "from", Pattern: "rubix.com", Negate: true, Enabled: true},
	}
	if _, ok := matchAutoDraftRule(rules, "kunde@example.com", "Frage zu Bestellung"); !ok {
		t.Fatal("want a match: sender does not contain rubix.com")
	}
	if _, ok := matchAutoDraftRule(rules, "kollege@rubix.com", "Interne Frage"); ok {
		t.Fatal("want no match: sender IS from rubix.com")
	}
}

func TestMatchAutoDraftRuleSubjectField(t *testing.T) {
	rules := []exchangeAutoDraftRule{
		{PatternField: "subject", Pattern: "Reklamation", Enabled: true},
	}
	if _, ok := matchAutoDraftRule(rules, "any@example.com", "RE: Reklamation Bestellung 123"); !ok {
		t.Fatal("want a match on subject substring")
	}
	if _, ok := matchAutoDraftRule(rules, "any@example.com", "Allgemeine Frage"); ok {
		t.Fatal("want no match: subject doesn't contain the pattern")
	}
}

func TestMatchAutoDraftRuleCaseInsensitive(t *testing.T) {
	rules := []exchangeAutoDraftRule{{PatternField: "from", Pattern: "RUBIX.COM", Negate: true, Enabled: true}}
	if _, ok := matchAutoDraftRule(rules, "person@Rubix.Com", "x"); ok {
		t.Fatal("want the substring match to be case-insensitive")
	}
}

func TestMatchAutoDraftRuleSkipsDisabledAndEmptyPattern(t *testing.T) {
	rules := []exchangeAutoDraftRule{
		{PatternField: "from", Pattern: "rubix.com", Negate: true, Enabled: false}, // disabled
		{PatternField: "from", Pattern: "", Enabled: true},                         // empty pattern
		{PatternField: "from", Pattern: "example.com", Enabled: true},              // this one should win
	}
	rule, ok := matchAutoDraftRule(rules, "person@example.com", "x")
	if !ok || rule.Pattern != "example.com" {
		t.Fatalf("want the third (valid, enabled) rule to match, got %+v ok=%v", rule, ok)
	}
}

func TestMatchAutoDraftRuleFirstMatchWins(t *testing.T) {
	rules := []exchangeAutoDraftRule{
		{PatternField: "from", Pattern: "example.com", Enabled: true},
		{PatternField: "from", Pattern: "person", Enabled: true},
	}
	rule, ok := matchAutoDraftRule(rules, "person@example.com", "x")
	if !ok || rule.Pattern != "example.com" {
		t.Fatalf("want the FIRST matching rule returned, got %+v", rule)
	}
}

func TestMatchAutoDraftRuleNoRulesNoMatch(t *testing.T) {
	if _, ok := matchAutoDraftRule(nil, "anyone@example.com", "subject"); ok {
		t.Fatal("want no match with an empty rule set")
	}
}

// ─── runExchangeAutoDraftRules integration (Task C) ────────────────────────

// TestRunExchangeAutoDraftRulesOffByDefault confirms the whole engine is a
// no-op — no Graph calls, no drafted count — unless BOTH
// EnableAutoDraftRules and EnableDraftReplies are on, matching every other
// opt-in gate in this codebase.
func TestRunExchangeAutoDraftRulesOffByDefault(t *testing.T) {
	called := false
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rag, s := newTestRAG(t)

	cfg := testExchangeGraphConfig() // both flags left false/default
	cfg.AutoDraftRules = []exchangeAutoDraftRule{{PatternField: "from", Pattern: "rubix.com", Negate: true, Enabled: true}}
	preview := []graphMailPreviewItem{{ID: "m1", From: "kunde@example.com", Subject: "Frage"}}

	updated, drafted, errs := runExchangeAutoDraftRules(context.Background(), rag, s, cfg, preview)
	if drafted != 0 || len(errs) != 0 {
		t.Fatalf("want a no-op when the flags are off, got drafted=%d errs=%v", drafted, errs)
	}
	if len(updated) != 0 {
		t.Fatalf("want AutoDraftedIDs left untouched when disabled, got %+v", updated)
	}
	if called {
		t.Fatal("want zero Graph calls when the engine is disabled")
	}
}

// TestRunExchangeAutoDraftRulesMatchDraftsExactlyOnceNoSend is the
// integration-style test the task calls for: a matching new message
// results in exactly one draft-creation flow (createReply + PATCH) and
// zero calls to anything resembling a send endpoint, driven through the
// same fake Graph transport as the rest of this package's connector tests.
func TestRunExchangeAutoDraftRulesMatchDraftsExactlyOnceNoSend(t *testing.T) {
	var draftCalls, embedCalls int
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(strings.ToLower(r.URL.Path), "send"):
			t.Fatalf("HARD SAFETY INVARIANT VIOLATED: send-like path called: %s", r.URL.Path)
		case strings.HasSuffix(r.URL.Path, "/createReply"):
			draftCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "draft-1"}`))
		case strings.Contains(r.URL.Path, "/messages/draft-1"):
			draftCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id": "draft-1"}`))
		case strings.HasSuffix(r.URL.Path, "/messages/ext-msg-1"):
			// fetchGraphMail: the full message body composeDraftReply grounds on.
			_, _ = w.Write([]byte(`{
				"subject": "Frage zur Lieferung",
				"receivedDateTime": "2026-07-01T10:00:00Z",
				"from": {"emailAddress": {"name": "Kunde", "address": "kunde@example.com"}},
				"toRecipients": [{"emailAddress": {"name": "Vertrieb", "address": "vertrieb@rubix.com"}}],
				"body": {"contentType": "text", "content": "Wann kommt meine Lieferung an?"}
			}`))
		default:
			t.Fatalf("unexpected Graph call: %s", r.URL.Path)
		}
	})

	// composeDraftReply is called with nil tools (draftExchangeAutoReply's
	// deliberate choice — see autodraft.go's doc comment), so
	// chatWithToolsBudget takes the plain chatStream (SSE) path, not the
	// tool-calling chatOnce (JSON) path draft_test.go's other fixtures use.
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Vielen Dank fuer Ihre Anfrage.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(chatServer.Close)

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", chatServer.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")

	cfg := testExchangeGraphConfigForDrafts()
	cfg.EnableAutoDraftRules = true
	cfg.AutoDraftRules = []exchangeAutoDraftRule{{PatternField: "from", Pattern: "rubix.com", Negate: true, Enabled: true}}

	preview := []graphMailPreviewItem{
		{ID: "ext-msg-1", From: "Kunde <kunde@example.com>", Subject: "Frage zur Lieferung"},
		{ID: "int-msg-1", From: "Kollege <kollege@rubix.com>", Subject: "Interne Frage"},
	}

	updatedIDs, drafted, errs := runExchangeAutoDraftRules(context.Background(), rag, s, cfg, preview)
	if len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
	if drafted != 1 {
		t.Fatalf("want exactly 1 draft created (only the external sender matches), got %d", drafted)
	}
	if draftCalls != 2 {
		t.Fatalf("want exactly one createReply + one PATCH (2 calls total), got %d", draftCalls)
	}
	if len(updatedIDs) != 2 {
		t.Fatalf("want both messages recorded as seen (dedup cursor), got %+v", updatedIDs)
	}

	// A second run over the SAME preview batch must not draft again — the
	// message is now in AutoDraftedIDs, so it's dedup-skipped even though
	// the rule would still match.
	cfg.AutoDraftedIDs = updatedIDs
	_, drafted2, errs2 := runExchangeAutoDraftRules(context.Background(), rag, s, cfg, preview)
	if drafted2 != 0 || len(errs2) != 0 {
		t.Fatalf("want no re-drafting of already-seen messages, got drafted=%d errs=%v", drafted2, errs2)
	}
}
