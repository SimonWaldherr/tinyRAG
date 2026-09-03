package main

import (
	"context"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type retrievalPlannerTestLM struct {
	response string
}

func (m retrievalPlannerTestLM) embed(texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = []float64{1, 0}
	}
	return out, nil
}

func (m retrievalPlannerTestLM) embedSingle(string) ([]float64, error) { return []float64{1, 0}, nil }

func (m retrievalPlannerTestLM) chatStream(_ context.Context, _ string, _ []chatMsg, w io.Writer) error {
	_, err := io.WriteString(w, m.response)
	return err
}

func (m retrievalPlannerTestLM) chatStreamDetailed(context.Context, string, []chatMsg, io.Writer, io.Writer) error {
	return nil
}

func (m retrievalPlannerTestLM) chatStreamVision(context.Context, string, []visionMsg, io.Writer, io.Writer) error {
	return nil
}

type blockingPlannerTestLM struct{ retrievalPlannerTestLM }

func (m blockingPlannerTestLM) chatStream(ctx context.Context, _ string, _ []chatMsg, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}

func newRetrievalPlannerTestRAG(t *testing.T, lm lmProvider, similarity float64) *ragSystem {
	t.Helper()
	previous := settings
	t.Cleanup(func() { settings = previous })
	settings = &settingsStore{s: appSettings{
		EmbedModel:            "test-embed",
		ActiveRole:            "it",
		VectorSearchThreshold: 0.60,
	}}
	r, err := newRAG(lm, 3, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.db.Close() })
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	candidate := storedChunk{
		ID: 0, Article: "Retention Handbook", ChunkIdx: 0,
		Content:   "Die Aufbewahrungsrichtlinie verlangt eine dokumentierte Freigabe.",
		Embedding: []float64{similarity, math.Sqrt(1 - similarity*similarity)}, EmbedModel: "test-embed", RoleScope: "|all|",
		ChunkID: "retention-1:0", DocumentID: "retention-1",
		SourceSystem: "local", SourceType: "official_doc", SourceTitle: "Retention Handbook",
		TrustLevel: 0.9, SourceQuality: 0.9, FreshnessScore: 0.9, QualityScore: 0.9, FeedbackScore: 0.5,
		OpenLinkAllowed: true,
	}
	if err := r.chunkStore.insertChunks([]storedChunk{candidate}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPrepareContextPlannerAnswerDirectKeepsGroundedEvidence(t *testing.T) {
	r := newRetrievalPlannerTestRAG(t, retrievalPlannerTestLM{response: "{\"action\":\"ANSWER_DIRECT\"}"}, 0.70)

	context, debug, err := r.prepareContext("retention", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context, "Aufbewahrungsrichtlinie") || !strings.Contains(context, "[Quelle:") {
		t.Fatalf("ANSWER_DIRECT lost its grounding context:\n%s", context)
	}
	if debug == nil || debug.Decision != "answer_from_candidates" || len(debug.Citations) != 1 {
		t.Fatalf("debug evidence = %#v", debug)
	}
}

func TestPrepareDirectContextRejectsHighQualityButIrrelevantCandidate(t *testing.T) {
	r := newRetrievalPlannerTestRAG(t, retrievalPlannerTestLM{}, 0.05)

	context, debug, err := r.prepareDirectContext("retention", 1)
	if err != nil {
		t.Fatal(err)
	}
	if context != "" {
		t.Fatalf("irrelevant candidate entered context:\n%s", context)
	}
	if debug == nil || debug.Decision != "no_hits" {
		t.Fatalf("debug result = %#v", debug)
	}
}

func TestNormalizeRetrievalPlanBoundsModelSuggestions(t *testing.T) {
	plan := normalizeRetrievalPlan(map[string]any{
		"action":    "RETRIEVE_MORE",
		"k":         float64(999),
		"threshold": float64(0.10),
		"query":     strings.Repeat("x", retrievalPlannerQueryRunes+10),
	}, 3, 0.60, "fallback")
	if plan.K != retrievalPlannerMaxK {
		t.Fatalf("planner k = %d, want %d", plan.K, retrievalPlannerMaxK)
	}
	if plan.Threshold != 0.60 {
		t.Fatalf("planner lowered configured threshold to %.2f", plan.Threshold)
	}
	if len([]rune(plan.Query)) != retrievalPlannerQueryRunes {
		t.Fatalf("planner query length = %d", len([]rune(plan.Query)))
	}

	plan = normalizeRetrievalPlan(map[string]any{"threshold": float64(0.85)}, 3, 0.60, "fallback")
	if plan.Threshold != 0.85 {
		t.Fatalf("valid stricter planner threshold = %.2f", plan.Threshold)
	}
}

func TestAnalyzeQuestionTimesOutAndFailsOpen(t *testing.T) {
	previousTimeout := retrievalPlannerTimeout
	retrievalPlannerTimeout = 10 * time.Millisecond
	t.Cleanup(func() { retrievalPlannerTimeout = previousTimeout })

	r := &ragSystem{lm: blockingPlannerTestLM{}}
	start := time.Now()
	if _, err := r.analyzeQuestion("question", "summary"); err == nil {
		t.Fatal("expected planner timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("planner timeout took too long: %s", elapsed)
	}
}

func TestAnalyzeQuestionHonorsParentCancellation(t *testing.T) {
	r := &ragSystem{lm: blockingPlannerTestLM{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := r.analyzeQuestionContext(ctx, "question", "summary"); err == nil {
		t.Fatal("expected cancelled parent context to stop retrieval planning")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled retrieval planner took too long: %s", elapsed)
	}
}

func TestSearchCandidatesHonorsParentCancellationBeforeEmbedding(t *testing.T) {
	r := &ragSystem{lm: retrievalPlannerTestLM{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := r.searchCandidatesContext(ctx, "cancelled query", 3); err == nil {
		t.Fatal("expected cancelled parent context to stop retrieval before embedding")
	}
}
