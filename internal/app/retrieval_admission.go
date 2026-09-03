package app

import "strings"

// isRetrievalHitRelevant decides whether a candidate has enough direct
// evidence to enter answer context. R³ scores rank already-admitted evidence;
// they intentionally do not replace the configured semantic relevance floor.
//
// Exact full-text matches for a technical identifier are an explicit narrow
// exception. They support part numbers and error codes whose embedding may be
// weak while avoiding admission based on a generic keyword match alone.
func isRetrievalHitRelevant(query string, hit retrievalHit, threshold float64) bool {
	threshold = clampUnitInterval(threshold)
	if hit.Score >= threshold {
		return true
	}
	return hit.FullTextRank > 0 && hasExactTechnicalIdentifier(query, hit.Content)
}

// hasHighConfidenceSemanticMatch is intentionally semantic-only: a literal
// identifier hit is useful evidence, but it should not skip the normal
// retrieval-planning path merely because it matched a code exactly.
func hasHighConfidenceSemanticMatch(hit retrievalHit, threshold float64) bool {
	return hit.Score >= clampUnitInterval(threshold)
}

// selectRelevantHits preserves the existing candidate order while selecting
// only evidence that passes the relevance admission rule.
func selectRelevantHits(query string, hits []retrievalHit, threshold float64, limit int) []retrievalHit {
	if limit < 1 {
		return nil
	}
	selected := make([]retrievalHit, 0, min(limit, len(hits)))
	for _, hit := range hits {
		if !isRetrievalHitRelevant(query, hit, threshold) {
			continue
		}
		selected = append(selected, hit)
		if len(selected) == limit {
			break
		}
	}
	return selected
}

// hasExactTechnicalIdentifier verifies an identifier as a whole (for example
// "XR-500" or "ABC123") rather than treating an FTS match for a broad token
// such as "500" as sufficient evidence. It accepts equivalent punctuation
// layouts such as "XR 500" because source text commonly normalizes separators.
func hasExactTechnicalIdentifier(query, content string) bool {
	contentTokens := make(map[string]bool)
	contentTerms := make(map[string]bool)
	for _, token := range splitSearchTokens(strings.ToLower(content)) {
		terms := asciiSearchTerms(token)
		if len(terms) == 0 {
			continue
		}
		contentTokens[strings.Join(terms, "")] = true
		for _, term := range terms {
			if len(term) >= 2 {
				contentTerms[term] = true
			}
		}
	}

	for _, token := range splitSearchTokens(strings.ToLower(query)) {
		if !isTechnicalIdentifierToken(token) {
			continue
		}
		terms := asciiSearchTerms(token)
		if len(terms) == 0 {
			continue
		}
		if contentTokens[strings.Join(terms, "")] {
			return true
		}

		meaningful := make([]string, 0, len(terms))
		for _, term := range terms {
			if len(term) >= 2 {
				meaningful = append(meaningful, term)
			}
		}
		if len(meaningful) < 2 {
			continue
		}
		allPresent := true
		for _, term := range meaningful {
			if !contentTerms[term] {
				allPresent = false
				break
			}
		}
		if allPresent {
			return true
		}
	}
	return false
}

func isTechnicalIdentifierToken(token string) bool {
	return hasLetter(token) && hasDigit(token)
}
