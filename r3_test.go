package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type r3MockLM struct{}

func (r3MockLM) embed(texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = []float64{0.1, 0.2, 0.3, 0.4}
	}
	return out, nil
}
func (r3MockLM) embedSingle(text string) ([]float64, error) {
	return []float64{0.1, 0.2, 0.3, 0.4}, nil
}
func (r3MockLM) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	return nil
}
func (r3MockLM) chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error {
	return nil
}
func (r3MockLM) chatStreamVision(ctx context.Context, system string, msgs []visionMsg, w io.Writer, thinkW io.Writer) error {
	return nil
}
func (r3MockLM) ping() error { return nil }

func TestR3RankingCorrectness(t *testing.T) {
	p := defaultRankingPolicy()
	now := time.Now().UTC()
	official := RetrievalUnit{
		SourceType:    "official_doc",
		TrustLevel:    0.95,
		SourceQuality: 0.95,
		QualityScore:  0.85,
		FeedbackScore: 0.70,
		Sensitivity:   "public",
		UpdatedAt:     now.Add(-24 * time.Hour),
	}
	ticket := RetrievalUnit{
		SourceType:    "ticket",
		TrustLevel:    0.45,
		SourceQuality: 0.40,
		QualityScore:  0.30,
		FeedbackScore: 0.20,
		Sensitivity:   "confidential",
		UpdatedAt:     now.Add(-400 * 24 * time.Hour),
		Content:       "closed done without resolution",
	}
	a := p.Score(official, 0.82, now)
	b := p.Score(ticket, 0.85, now)
	if a <= b {
		t.Fatalf("expected official doc score > ticket score, got %.4f <= %.4f", a, b)
	}
}

func TestDiversifyRetrievalHitsPrefersDistinctSources(t *testing.T) {
	hits := []retrievalHit{
		{DocumentID: "a", Article: "A", ChunkIdx: 0},
		{DocumentID: "a", Article: "A", ChunkIdx: 1},
		{DocumentID: "a", Article: "A", ChunkIdx: 2},
		{DocumentID: "b", Article: "B", ChunkIdx: 0},
	}
	got := diversifyRetrievalHits(hits, 2)
	if len(got) != len(hits) {
		t.Fatalf("got %d hits, want %d", len(got), len(hits))
	}
	if got[0].DocumentID != "a" || got[1].DocumentID != "a" || got[2].DocumentID != "b" || got[3].ChunkIdx != 2 {
		t.Fatalf("unexpected source-diverse order: %#v", got)
	}
}

func TestDiversifyRetrievalHitsKeepsSingleSourceRecall(t *testing.T) {
	hits := []retrievalHit{{DocumentID: "a", ChunkIdx: 0}, {DocumentID: "a", ChunkIdx: 1}, {DocumentID: "a", ChunkIdx: 2}}
	got := diversifyRetrievalHits(hits, 1)
	if len(got) != len(hits) {
		t.Fatalf("single-source hits were dropped: %#v", got)
	}
}

func TestR3ACLEnforcement(t *testing.T) {
	p := ACLPolicy{}
	if !p.CanRoleAccess("it", "|it|", "|it|") {
		t.Fatal("expected IT role access")
	}
	if p.CanRoleAccess("sales", "|it|", "|it|") {
		t.Fatal("expected sales role to be denied for IT ACL")
	}
	if !p.CanRoleAccess("hr", "|all|", "|all|") {
		t.Fatal("expected all scope to allow access")
	}
}

func TestCitationIntegrityAndHallucinationPrevention(t *testing.T) {
	c := []Citation{{DocumentID: "doc1", Title: "Policy Handbook", SourceSystem: "confluence", SourceType: "official_doc", TrustLevel: 0.9}}
	answer := ensureCitedAnswer("Antworttext.", c)
	if !strings.Contains(strings.ToLower(answer), "quellenbasis") {
		t.Fatalf("expected appended citations, got: %s", answer)
	}
	if !validateCitationsAgainstSources(answer, c) {
		t.Fatal("expected citation validation to pass")
	}
	if validateCitationsAgainstSources("Antwort ohne bekannte Quelle.", c) {
		t.Fatal("expected hallucination prevention to fail for unknown source answer")
	}
}

func TestPseudonymization(t *testing.T) {
	text := "CEO John Doe from Acme GmbH approved €1,000,000 for jane@example.com"
	got1, n1 := pseudonymizeText(text, "doc-42", false)
	got2, n2 := pseudonymizeText(text, "doc-42", false)
	if n1 == 0 || n2 == 0 {
		t.Fatal("expected pseudonymization replacements")
	}
	for _, needle := range []string{"ROLE_EXECUTIVE_", "USER_", "CUSTOMER_", "HIGH_VALUE_", "EMAIL_"} {
		if !strings.Contains(got1, needle) {
			t.Fatalf("expected %s marker in pseudonymized text: %s", needle, got1)
		}
	}
	if got1 != got2 {
		t.Fatal("expected stable pseudonyms for same document")
	}
}

func TestFreshnessDecay(t *testing.T) {
	now := time.Now().UTC()
	fresh := freshnessDecayScore(now.Add(-2*24*time.Hour), now)
	stale := freshnessDecayScore(now.Add(-500*24*time.Hour), now)
	if fresh <= stale {
		t.Fatalf("expected fresh score > stale score, got %.3f <= %.3f", fresh, stale)
	}
}

func TestToolPersistencePolicy(t *testing.T) {
	p := ToolPersistencePolicy{}
	if got := p.Classify("sql_query", "sql:orders"); got != ToolTransientOnly {
		t.Fatalf("expected transient for sql_query, got %s", got)
	}
	if got := p.Classify("module:module-http-folder", "module:module-http-folder:file"); got != ToolPersistableAfterPolicy {
		t.Fatalf("expected persistable for approved folder ingest, got %s", got)
	}
	if got := p.Classify("hr_lookup", "hr:data"); got != ToolNeverPersist {
		t.Fatalf("expected never_persist for hr lookup, got %s", got)
	}
}

func TestMigrationCompatibility(t *testing.T) {
	r, err := newRAG(r3MockLM{}, 3, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatalf("newRAG failed: %v", err)
	}
	if err := r.init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	s := &settingsStore{}
	s.s.EmbedModel = "embed"
	s.s.ActiveRole = "it"
	settings = s
	if err := r.addChunksWithRoles("doc-a", []string{"hello world"}, "embed", []string{"it"}); err != nil {
		t.Fatalf("addChunksWithRoles failed: %v", err)
	}
	stmt, err := tinysql.ParseSQL("SELECT COUNT(*) AS cnt FROM r3_sources")
	if err != nil {
		t.Fatalf("parse SQL failed: %v", err)
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		t.Fatalf("query r3_sources failed: %v", err)
	}
}

func TestConnectorExecutionSchemaValidation(t *testing.T) {
	schema := JSONSchema{
		Type: "object",
		Properties: map[string]JSONSchemaProperty{
			"id": {Type: "string"},
		},
		Required: []string{"id"},
	}
	if err := validateInput(map[string]any{"id": "42"}, schema); err != nil {
		t.Fatalf("expected schema validation pass, got %v", err)
	}
	if err := validateInput(map[string]any{}, schema); err == nil {
		t.Fatal("expected schema validation failure for missing required field")
	}
}

func TestAuthHardeningRejectsInvalidBearer(t *testing.T) {
	st := &settingsStore{}
	st.s.WebUIAuth = true
	st.s.APIUsers = []adminAPIUser{{
		ID:         "u1",
		Name:       "svc",
		Role:       "admin",
		Enabled:    true,
		APIKeyHash: hashAPIToken("valid-token"),
	}}
	settings = st
	req := httptest.NewRequest(http.MethodGet, "/api/search", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	rec := httptest.NewRecorder()
	called := false
	webUIAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})(rec, req)
	if called {
		t.Fatal("expected middleware to block invalid bearer token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestIsSafeFetchURL(t *testing.T) {
	if err := isSafeFetchURL("http://localhost:8080"); err == nil {
		t.Fatal("expected localhost URL to be blocked")
	}
	if err := isSafeFetchURL("http://127.0.0.1/test"); err == nil {
		t.Fatal("expected loopback IP URL to be blocked")
	}
	if err := isSafeFetchURL("https://example.com/docs"); err != nil {
		t.Fatalf("expected public URL allowed, got %v", err)
	}
}
