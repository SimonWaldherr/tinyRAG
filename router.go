package main

// ─────────────────────────────────────────────────────────────────────────────
// Query Router
//
// Routes each user question to one of three answer modes:
//
//   direct_answer   — conceptual / general questions, no external data needed
//   retrieval_answer — product docs, specs, manuals, FAQs, internal knowledge
//   agentic_answer  — orders/customers/business data, calculations, URL fetch,
//                     multi-source requests
//
// The router uses deterministic keyword heuristics as a first pass.
// It is intentionally narrow and does NOT call the LLM for most queries.
// An LLM planner is NOT required for unambiguous cases to keep latency low.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"net/url"
	"strings"
)

// AnswerMode identifies the routing decision for a request.
type AnswerMode string

const (
	// ModeDirect — answer from LLM knowledge alone, no tools needed.
	ModeDirect AnswerMode = "direct_answer"
	// ModeRetrieval — use the RAG knowledge base to retrieve context.
	ModeRetrieval AnswerMode = "retrieval_answer"
	// ModeAgentic — use one or more tools (business, URL, compute, RAG).
	ModeAgentic AnswerMode = "agentic_answer"
)

// RouteDecision captures why a mode was chosen.
type RouteDecision struct {
	Mode   AnswerMode `json:"mode"`
	Reason string     `json:"reason"`
	// Hints carries detected signal keywords for telemetry.
	Hints []string `json:"hints,omitempty"`
}

// NormalizedQuery is the cleaned, lower-cased version of the user question,
// used internally by the router and planner.
type NormalizedQuery struct {
	Original  string   `json:"original"`
	Lowercase string   `json:"lowercase"`
	Words     []string `json:"-"`
}

// normalizeQuery prepares a NormalizedQuery from a raw user question.
func normalizeQuery(question string) NormalizedQuery {
	lc := strings.ToLower(strings.TrimSpace(question))
	words := strings.Fields(lc)
	return NormalizedQuery{
		Original:  question,
		Lowercase: lc,
		Words:     words,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Keyword signal tables
// ─────────────────────────────────────────────────────────────────────────────

// agenticKeywords trigger agentic_answer mode (business/customer/compute/URL).
var agenticKeywords = []string{
	// Business / customer / order data
	"order", "orders", "bestellung", "bestellungen",
	"customer", "customers", "kunde", "kunden",
	"invoice", "invoices", "rechnung", "rechnungen",
	"status", "tracking",
	"account", "konto",
	"contract", "contracts", "vertrag", "verträge",
	// URL signals
	"http://", "https://", "www.",
	// Calculation / transformation
	"calculate", "computation", "compute", "berechne", "berechnung",
	"formula", "formel",
	"convert", "konvertiere", "umrechnen",
	"parse", "transform",
	// Code execution
	"run code", "execute", "ausführen",
	// Multi-source research
	"research", "analyse", "analyze",
	"compare", "vergleiche",
	"latest", "aktuell", "current", "news",
}

// retrievalKeywords trigger retrieval_answer mode (docs/manuals/knowledge base).
var retrievalKeywords = []string{
	// Product / spec / manual signals
	"spec", "specs", "specification", "spezifikation",
	"manual", "handbuch", "documentation", "dokumentation",
	"datasheet", "datenblatt",
	"product", "produkt",
	"compatibility", "kompatibilität",
	"faq", "howto", "how-to",
	"procedure", "prozedur",
	"policy", "richtlinie",
	"guideline", "leitfaden",
	"support", "help",
	"feature", "funktion",
	"release note", "changelog",
	"version",
}

// directKeywords are strong signals for direct (no-tool) answers.
var directKeywords = []string{
	"what is", "was ist", "who is", "wer ist",
	"explain", "erkläre", "erklär",
	"define", "definiere",
	"tell me about",
	"how does", "wie funktioniert",
	"why", "warum",
	"describe", "beschreibe",
	"difference between", "unterschied zwischen",
	"example", "beispiel",
	"concept", "konzept",
	"theory", "theorie",
	"history", "geschichte",
}

// ─────────────────────────────────────────────────────────────────────────────
// Router
// ─────────────────────────────────────────────────────────────────────────────

// routeQuery applies the heuristic router to a normalized query and returns
// a RouteDecision.  The decision is deterministic and does not call the LLM.
func routeQuery(nq NormalizedQuery, hasRAGContext bool) RouteDecision {
	lc := nq.Lowercase
	var hints []string

	// ── 1. URL present → agentic (url_fetch) ─────────────────────────────────
	if containsURL(nq.Original) {
		hints = append(hints, "url_present")
		return RouteDecision{Mode: ModeAgentic, Reason: "URL detected in query", Hints: hints}
	}

	// ── 2. Agentic keyword signals ────────────────────────────────────────────
	for _, kw := range agenticKeywords {
		if strings.Contains(lc, kw) {
			hints = append(hints, kw)
		}
	}
	if len(hints) > 0 {
		return RouteDecision{
			Mode:   ModeAgentic,
			Reason: "agentic keywords: " + strings.Join(hints, ", "),
			Hints:  hints,
		}
	}

	// ── 3. Retrieval keyword signals ──────────────────────────────────────────
	for _, kw := range retrievalKeywords {
		if strings.Contains(lc, kw) {
			hints = append(hints, kw)
		}
	}
	if len(hints) > 0 {
		return RouteDecision{
			Mode:   ModeRetrieval,
			Reason: "retrieval keywords: " + strings.Join(hints, ", "),
			Hints:  hints,
		}
	}

	// ── 4. If RAG context was found, prefer retrieval ─────────────────────────
	if hasRAGContext {
		return RouteDecision{
			Mode:   ModeRetrieval,
			Reason: "rag_context_available",
			Hints:  []string{"rag_context"},
		}
	}

	// ── 5. Direct answer signals ──────────────────────────────────────────────
	for _, kw := range directKeywords {
		if strings.Contains(lc, kw) {
			hints = append(hints, kw)
		}
	}
	if len(hints) > 0 {
		return RouteDecision{
			Mode:   ModeDirect,
			Reason: "direct keywords: " + strings.Join(hints, ", "),
			Hints:  hints,
		}
	}

	// ── 6. Default: retrieval (assume knowledge base may be useful) ───────────
	return RouteDecision{
		Mode:   ModeRetrieval,
		Reason: "default_retrieval",
		Hints:  nil,
	}
}

// containsURL returns true when the string contains an HTTP/HTTPS URL.
func containsURL(s string) bool {
	for _, prefix := range []string{"http://", "https://"} {
		idx := strings.Index(strings.ToLower(s), prefix)
		if idx == -1 {
			continue
		}
		// Quick validation: parse the rest as a URL
		rest := s[idx:]
		end := strings.IndexAny(rest, " \t\n\r\"'<>")
		if end > 0 {
			rest = rest[:end]
		}
		if u, err := url.Parse(rest); err == nil && u.Host != "" {
			return true
		}
	}
	return false
}
