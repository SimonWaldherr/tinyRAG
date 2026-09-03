package app

import "testing"

func TestRetrievalAdmissionUsesSemanticThresholdBeforeR3Quality(t *testing.T) {
	unrelatedButHighQuality := retrievalHit{Score: 0.05, R3Score: 1.22, Content: "Unrelated policy"}
	if isRetrievalHitRelevant("deployment rollback", unrelatedButHighQuality, 0.60) {
		t.Fatal("high source-quality score must not admit a semantically irrelevant hit")
	}

	relevantButLowerQuality := retrievalHit{Score: 0.61, R3Score: 0.68, Content: "Deployment rollback procedure"}
	if !isRetrievalHitRelevant("deployment rollback", relevantButLowerQuality, 0.60) {
		t.Fatal("semantic similarity above the configured threshold should be admitted")
	}
}

func TestRetrievalAdmissionAllowsOnlyExactTechnicalFullTextEvidence(t *testing.T) {
	exact := retrievalHit{Score: 0.12, FullTextRank: 1, Content: "Reset the XR-500 gateway before retrying."}
	if !isRetrievalHitRelevant("Wie behebe ich XR-500?", exact, 0.60) {
		t.Fatal("exact technical identifier from full-text retrieval should be admitted")
	}

	partial := retrievalHit{Score: 0.12, FullTextRank: 1, Content: "Gateway error 500 requires a retry."}
	if isRetrievalHitRelevant("Wie behebe ich XR-500?", partial, 0.60) {
		t.Fatal("a partial keyword match must not bypass the semantic threshold")
	}

	vectorOnly := exact
	vectorOnly.FullTextRank = 0
	if isRetrievalHitRelevant("Wie behebe ich XR-500?", vectorOnly, 0.60) {
		t.Fatal("technical lexical admission requires an actual full-text candidate")
	}
}

func TestSelectRelevantHitsKeepsRankOrderAndRespectsLimit(t *testing.T) {
	hits := []retrievalHit{
		{Score: 0.95, Content: "first"},
		{Score: 0.10, R3Score: 1.30, Content: "irrelevant"},
		{Score: 0.80, Content: "second"},
	}
	selected := selectRelevantHits("query", hits, 0.60, 2)
	if len(selected) != 2 || selected[0].Content != "first" || selected[1].Content != "second" {
		t.Fatalf("selected evidence = %#v", selected)
	}
}
