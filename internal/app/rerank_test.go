package app

import (
	"context"
	"testing"
)

func TestNormalizeRerankMode(t *testing.T) {
	cases := map[string]string{
		"":              "lexical",
		"lexical":       "lexical",
		"LLM":           "llm",
		"cross-encoder": "llm",
		"off":           "off",
		"none":          "off",
		"garbage":       "lexical",
	}
	for in, want := range cases {
		if got := normalizeRerankMode(in); got != want {
			t.Errorf("normalizeRerankMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLexicalOverlapScore(t *testing.T) {
	full := lexicalOverlapScore("Kubernetes Deployment Rollback", "Ein Kubernetes Deployment kann per Rollback zurückgesetzt werden.")
	if full < 0.99 {
		t.Errorf("expected full overlap ≈1.0, got %f", full)
	}
	none := lexicalOverlapScore("Kubernetes Deployment Rollback", "Der Kantinenplan für nächste Woche.")
	if none != 0 {
		t.Errorf("expected zero overlap, got %f", none)
	}
	partial := lexicalOverlapScore("Kubernetes Deployment Rollback", "Das Deployment war erfolgreich.")
	if partial <= none || partial >= full {
		t.Errorf("expected partial overlap between 0 and 1, got %f", partial)
	}
	if lexicalOverlapScore("", "content") != 0 {
		t.Error("empty query must score 0")
	}
	// Prefix match (German compounds) counts at half weight.
	prefix := lexicalOverlapScore("Urlaub", "Der Urlaubsantrag wurde genehmigt.")
	if prefix != 0.5 {
		t.Errorf("expected prefix match 0.5, got %f", prefix)
	}
}

func TestRerankLexicalReorders(t *testing.T) {
	hits := []retrievalHit{
		{Article: "a", ChunkIdx: 0, ChunkID: "c1", R3Score: 0.70, Content: "Völlig anderes Thema ohne Bezug."},
		{Article: "b", ChunkIdx: 0, ChunkID: "c2", R3Score: 0.68, Content: "Kubernetes Deployment Rollback Anleitung Schritt für Schritt."},
	}
	out := rerankLexical("Kubernetes Deployment Rollback", hits)
	if out[0].ChunkID != "c2" {
		t.Errorf("expected lexically matching hit first, got %s", out[0].ChunkID)
	}
}

func TestRerankLexicalKeepsOrderWithoutSignal(t *testing.T) {
	hits := []retrievalHit{
		{Article: "a", ChunkIdx: 0, ChunkID: "c1", R3Score: 0.90, Content: "irrelevant eins"},
		{Article: "b", ChunkIdx: 0, ChunkID: "c2", R3Score: 0.50, Content: "irrelevant zwei"},
	}
	out := rerankLexical("Quantenphysik", hits)
	if out[0].ChunkID != "c1" {
		t.Error("without lexical signal the R3 order must be preserved")
	}
}

func TestRerankLLMBlendsGrades(t *testing.T) {
	// Mock LM returns grades: candidate 1 highly relevant, candidate 0 not.
	lm := &mockLMProvider{response: `[{"i":0,"score":1},{"i":1,"score":10}]`}
	hits := []retrievalHit{
		{Article: "a", ChunkIdx: 0, ChunkID: "c1", R3Score: 0.72, Content: "x"},
		{Article: "b", ChunkIdx: 0, ChunkID: "c2", R3Score: 0.70, Content: "y"},
	}
	out := rerankLLM(context.Background(), lm, "frage", hits)
	if out[0].ChunkID != "c2" {
		t.Errorf("expected LLM-graded hit first, got %s", out[0].ChunkID)
	}
}

func TestRerankLLMFallsBackOnError(t *testing.T) {
	lm := &mockLMProvider{response: "kein json hier"}
	hits := []retrievalHit{
		{Article: "a", ChunkIdx: 0, ChunkID: "c1", R3Score: 0.70, Content: "nichts passendes"},
		{Article: "b", ChunkIdx: 0, ChunkID: "c2", R3Score: 0.68, Content: "Kubernetes Deployment Rollback"},
	}
	out := rerankLLM(context.Background(), lm, "Kubernetes Deployment Rollback", hits)
	// Fallback = lexical → c2 wins despite lower R3.
	if out[0].ChunkID != "c2" {
		t.Errorf("expected lexical fallback ordering, got %s first", out[0].ChunkID)
	}
}
