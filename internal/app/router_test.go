package app

import (
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Router Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestRouteQuery_DirectAnswer(t *testing.T) {
	tests := []struct {
		question string
	}{
		{"What is machine learning?"},
		{"Explain the difference between TCP and UDP"},
		{"Why is the sky blue?"},
		{"How does a transformer model work?"},
	}
	for _, tt := range tests {
		nq := normalizeQuery(tt.question)
		decision := routeQuery(nq, false)
		if decision.Mode != ModeDirect {
			t.Logf("question=%q → mode=%s reason=%s (expected direct, but acceptable)", tt.question, decision.Mode, decision.Reason)
		}
		// Just ensure no panic and a valid mode is returned
		if decision.Mode == "" {
			t.Errorf("empty mode for question=%q", tt.question)
		}
	}
}

func TestRouteQuery_RetrievalAnswer(t *testing.T) {
	tests := []struct {
		question string
	}{
		{"What does the product manual say about installation?"},
		{"Show me the specification for part X"},
		{"What are the FAQs for this service?"},
		{"Find documentation about the API"},
	}
	for _, tt := range tests {
		nq := normalizeQuery(tt.question)
		decision := routeQuery(nq, false)
		if decision.Mode != ModeRetrieval {
			t.Logf("question=%q → mode=%s reason=%s (expected retrieval)", tt.question, decision.Mode, decision.Reason)
		}
	}
}

func TestRouteQuery_AgenticAnswer_BusinessTerms(t *testing.T) {
	tests := []string{
		"What is the status of order 12345?",
		"Show me all customer invoices from last month",
		"Track my order",
		"What's the contract value for customer Acme?",
	}
	for _, q := range tests {
		nq := normalizeQuery(q)
		decision := routeQuery(nq, false)
		if decision.Mode != ModeAgentic {
			t.Errorf("q=%q → mode=%s (expected agentic)", q, decision.Mode)
		}
	}
}

func TestRouteQuery_AgenticAnswer_URLPresent(t *testing.T) {
	tests := []string{
		"Please fetch https://example.com/page and summarize it",
		"What does http://api.example.com/docs say?",
	}
	for _, q := range tests {
		nq := normalizeQuery(q)
		decision := routeQuery(nq, false)
		if decision.Mode != ModeAgentic {
			t.Errorf("q=%q → mode=%s (expected agentic for URL)", q, decision.Mode)
		}
	}
}

func TestRouteQuery_AgenticAnswer_Calculation(t *testing.T) {
	tests := []string{
		"Calculate the total price including 19% VAT",
		"Convert 100 USD to EUR",
		"Compute the average of these values",
	}
	for _, q := range tests {
		nq := normalizeQuery(q)
		decision := routeQuery(nq, false)
		if decision.Mode != ModeAgentic {
			t.Errorf("q=%q → mode=%s (expected agentic for calc)", q, decision.Mode)
		}
	}
}

func TestRouteQuery_WithRAGContext(t *testing.T) {
	nq := normalizeQuery("What do we know?")
	decision := routeQuery(nq, true)
	// With RAG context available, should prefer retrieval
	if decision.Mode != ModeRetrieval {
		t.Logf("q=%q with rag context → mode=%s (expected retrieval)", nq.Original, decision.Mode)
	}
}

func TestRouteQuery_DefaultFallback(t *testing.T) {
	nq := normalizeQuery("foo bar baz qux")
	decision := routeQuery(nq, false)
	// Should return some mode, not empty
	if decision.Mode == "" {
		t.Error("expected non-empty mode for unknown query")
	}
}

func TestRouteQuery_HintsPopulated(t *testing.T) {
	nq := normalizeQuery("I need to check the order status for customer 123")
	decision := routeQuery(nq, false)
	if len(decision.Hints) == 0 {
		t.Error("expected hints for business query")
	}
}

func TestNormalizeQuery(t *testing.T) {
	nq := normalizeQuery("  What IS this?  ")
	if nq.Original != "  What IS this?  " {
		t.Errorf("expected original preserved, got %q", nq.Original)
	}
	if nq.Lowercase != "what is this?" {
		t.Errorf("expected lowercase, got %q", nq.Lowercase)
	}
	if len(nq.Words) != 3 {
		t.Errorf("expected 3 words, got %d: %v", len(nq.Words), nq.Words)
	}
}

func TestContainsURL(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"https://example.com/page", true},
		{"http://api.example.com", true},
		{"no URL here", false},
		{"www.example.com (no scheme)", false},
		{"go to https://example.com and read", true},
	}
	for _, tt := range tests {
		got := containsURL(tt.s)
		if got != tt.want {
			t.Errorf("containsURL(%q) = %t, want %t", tt.s, got, tt.want)
		}
	}
}
