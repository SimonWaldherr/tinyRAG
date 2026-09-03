package app

import (
	"strconv"
	"strings"
)

// fullTextCandidateStore is an optional retrieval capability. Vector backends
// that do not support it keep the established vector-only behavior.
type fullTextCandidateStore interface {
	searchFullTextCandidates(query string, vec []float64, embedModel, roleFilter string, limit int) ([]retrievalHit, error)
}

const (
	fullTextCandidateMinLimit = 64
	fullTextCandidateMaxLimit = 192
	fullTextRankFusionOffset  = 60.0
	fullTextRankFusionBoost   = 0.045
)

// shouldUseFullTextCandidates keeps the extra candidate source focused on the
// cases where literal matching adds the most value: product IDs, error codes,
// protocol versions, and similar mixed letter/number identifiers.
func shouldUseFullTextCandidates(query string) bool {
	if looksTechnicalQuery(query) {
		return true
	}
	for _, token := range splitSearchTokens(query) {
		if hasLetter(token) && hasDigit(token) {
			return true
		}
	}
	return false
}

// shouldSupplementFullTextCandidates avoids running FTS_SEARCH after
// HYBRID_SEARCH. Hybrid retrieval already performs a BM25 pass and supplies
// its fused candidate set, so a second FTS query would add latency without
// improving recall. Scalar and vector retrieval retain the targeted fallback
// for technical identifiers.
func shouldSupplementFullTextCandidates() bool {
	if settings == nil {
		return true
	}
	return normalizeRetrievalMode(settings.get().RetrievalMode) != "hybrid"
}

// buildFullTextCandidateQuery creates a small, literal OR query for tinySQL
// FTS. User text is never used as FTS syntax: boolean operators, quotes, and
// punctuation are discarded before the terms reach the database.
func buildFullTextCandidateQuery(query string) string {
	stop := stopwordSet()
	seen := make(map[string]bool)
	terms := make([]string, 0, 8)
	for _, token := range splitSearchTokens(strings.ToLower(query)) {
		for _, term := range asciiSearchTerms(token) {
			if len(term) < 2 || stop[term] || isFullTextOperator(term) || seen[term] {
				continue
			}
			seen[term] = true
			terms = append(terms, term)
			if len(terms) == 8 {
				return strings.Join(terms, " OR ")
			}
		}
	}
	return strings.Join(terms, " OR ")
}

func isFullTextOperator(term string) bool {
	switch term {
	case "and", "or", "not":
		return true
	default:
		return false
	}
}

// asciiSearchTerms mirrors the tokenizer used by the optional full-text
// engine. Keeping this conversion local makes punctuation-bearing identifiers
// such as "XR-500" searchable as the safe terms "xr OR 500".
func asciiSearchTerms(token string) []string {
	var terms []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		terms = append(terms, current.String())
		current.Reset()
	}
	for _, r := range token {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return terms
}

func fullTextCandidateLimit(k int) int {
	limit := k * 12
	if limit < fullTextCandidateMinLimit {
		limit = fullTextCandidateMinLimit
	}
	if limit > fullTextCandidateMaxLimit {
		limit = fullTextCandidateMaxLimit
	}
	return limit
}

func retrievalHitIdentity(hit retrievalHit) string {
	if id := strings.TrimSpace(hit.ChunkID); id != "" {
		return "chunk:" + id
	}
	if id := strings.TrimSpace(hit.DocumentID); id != "" {
		return "document:" + id + "\x00" + strconv.Itoa(hit.ChunkIdx)
	}
	return "legacy:" + hit.Article + "\x00" + strconv.Itoa(hit.ChunkIdx)
}

// fusedRetrievalScore applies a bounded reciprocal-rank fusion bonus when a
// candidate also appears in full-text search. The raw semantic score remains
// intact; exact-term matches can only resolve close candidates, not replace
// them with a large opaque BM25 score.
func fusedRetrievalScore(semantic float64, vectorRank, fullTextRank int) float64 {
	if fullTextRank < 1 {
		return semantic
	}
	denominator := 1.0 / (fullTextRankFusionOffset + float64(fullTextRank))
	if vectorRank > 0 {
		denominator += 1.0 / (fullTextRankFusionOffset + float64(vectorRank))
	}
	// The normalizer is the ideal score for rank one in both lists. A lexical-
	// only hit is intentionally capped at half the possible bonus.
	normalizer := 2.0 / (fullTextRankFusionOffset + 1)
	signal := denominator / normalizer
	if signal > 1 {
		signal = 1
	}
	return semantic + fullTextRankFusionBoost*signal
}

func (hit retrievalHit) rankingScore() float64 {
	if hit.VectorRank > 0 || hit.FullTextRank > 0 {
		return hit.RetrievalScore
	}
	return hit.Score
}

// mergeFullTextCandidates enriches the vector candidate set without changing
// its availability contract. It applies the same embedding-model and ACL
// constraints in the backend and silently retains vector-only retrieval if the
// optional full-text engine cannot produce candidates.
func (r *ragSystem) mergeFullTextCandidates(query string, queryVector []float64, activeRole string, k int, vectorHits []retrievalHit) []retrievalHit {
	for i := range vectorHits {
		if vectorHits[i].VectorRank < 1 {
			vectorHits[i].VectorRank = i + 1
		}
		vectorHits[i].RetrievalScore = vectorHits[i].Score
	}

	if !shouldUseFullTextCandidates(query) {
		return vectorHits
	}
	ftsQuery := buildFullTextCandidateQuery(query)
	store, ok := r.chunkStore.(fullTextCandidateStore)
	if !ok || ftsQuery == "" {
		return vectorHits
	}
	fullTextHits, err := store.searchFullTextCandidates(
		ftsQuery,
		queryVector,
		r.getActiveEmbedModel(),
		roleAndACLFilterSQL(activeRole),
		fullTextCandidateLimit(k),
	)
	if err != nil {
		return vectorHits
	}

	byIdentity := make(map[string]int, len(vectorHits)+len(fullTextHits))
	for i, hit := range vectorHits {
		byIdentity[retrievalHitIdentity(hit)] = i
	}
	for i := range fullTextHits {
		hit := fullTextHits[i]
		// FTS ranks are recomputed after the outer model/ACL filters. The
		// underlying engine ranks before those constraints are applied.
		hit.FullTextRank = i + 1
		identity := retrievalHitIdentity(hit)
		if existing, found := byIdentity[identity]; found {
			vectorHits[existing].FullTextRank = hit.FullTextRank
			vectorHits[existing].RetrievalScore = fusedRetrievalScore(
				vectorHits[existing].Score,
				vectorHits[existing].VectorRank,
				vectorHits[existing].FullTextRank,
			)
			continue
		}
		hit.RetrievalScore = fusedRetrievalScore(hit.Score, hit.VectorRank, hit.FullTextRank)
		byIdentity[identity] = len(vectorHits)
		vectorHits = append(vectorHits, hit)
	}
	return vectorHits
}
