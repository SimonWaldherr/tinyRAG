package app

// ─────────────────────────────────────────────────────────────────────────────
// Re-Ranking
//
// Second-stage re-ranking applied after vector retrieval and R³ scoring.
// Two modes are available (settings field "rerank_mode"):
//
//   "off"     — keep the R³ ordering untouched.
//   "lexical" — blend the R³ score with a lexical token-overlap score
//               (default; deterministic, no extra LLM calls).
//   "llm"     — ask the chat model to grade the top candidates for relevance
//               and blend that grade into the ordering. Falls back to
//               lexical scoring when the LLM call fails.
//
// The reranker never removes hits — it only reorders them, so downstream
// thresholding and neighbor expansion keep working unchanged.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	rerankModeOff     = "off"
	rerankModeLexical = "lexical"
	rerankModeLLM     = "llm"

	// lexicalBlendWeight is the share of the lexical score in the blended
	// ranking score for "lexical" mode.
	lexicalBlendWeight = 0.25
	// llmBlendWeight is the share of the LLM relevance grade in "llm" mode.
	llmBlendWeight = 0.45
	// llmRerankTopN caps how many candidates are sent to the LLM grader.
	llmRerankTopN = 16
	// llmRerankTimeout bounds the grading call so retrieval latency stays sane.
	llmRerankTimeout = 12 * time.Second
	// maxPreferredHitsPerSource keeps the first context window from being
	// monopolized by several adjacent chunks of one document. Extra chunks are
	// retained and only moved behind evidence from other sources, so a query
	// whose answer genuinely lives in one document still has its full recall.
	maxPreferredHitsPerSource = 2
)

// normalizeRerankMode maps arbitrary input to a valid rerank mode.
// Empty input defaults to "lexical".
func normalizeRerankMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case rerankModeOff, "none", "disabled":
		return rerankModeOff
	case rerankModeLLM, "model", "cross-encoder":
		return rerankModeLLM
	default:
		return rerankModeLexical
	}
}

// lexicalOverlapScore returns a 0..1 score measuring how many distinct
// query tokens occur in the content. Stopwords are ignored so that filler
// words do not dominate; exact token hits and prefix hits both count,
// prefix hits at half weight (helps German compound words).
func lexicalOverlapScore(query, content string) float64 {
	stop := stopwordSet()
	qTokens := splitSearchTokens(strings.ToLower(query))
	terms := make([]string, 0, len(qTokens))
	for _, t := range qTokens {
		if len(t) < 2 || stop[t] {
			continue
		}
		terms = append(terms, t)
	}
	if len(terms) == 0 {
		return 0
	}
	cTokens := splitSearchTokens(strings.ToLower(content))
	if len(cTokens) == 0 {
		return 0
	}
	cSet := make(map[string]bool, len(cTokens))
	for _, t := range cTokens {
		cSet[t] = true
	}
	var score float64
	for _, term := range terms {
		if cSet[term] {
			score += 1.0
			continue
		}
		for ct := range cSet {
			if len(ct) > len(term) && strings.HasPrefix(ct, term) {
				score += 0.5
				break
			}
		}
	}
	return score / float64(len(terms))
}

// rerankKey identifies a hit independently of its slice position. It uses the
// stable source identity so same-titled documents never share a blended score.
type rerankKey string

func makeRerankKey(hit retrievalHit) rerankKey {
	return rerankKey(retrievalHitIdentity(hit))
}

// rerankLexical reorders hits by blending R³ score with lexical overlap.
// The original slice is modified in place and returned.
func rerankLexical(query string, hits []retrievalHit) []retrievalHit {
	if len(hits) < 2 {
		return hits
	}
	blended := make(map[rerankKey]float64, len(hits))
	for _, h := range hits {
		lex := lexicalOverlapScore(query, h.Content)
		blended[makeRerankKey(h)] = h.R3Score*(1-lexicalBlendWeight) + lex*lexicalBlendWeight
	}
	sortHitsDeterministic(hits, func(a, b retrievalHit) bool {
		ba := blended[makeRerankKey(a)]
		bb := blended[makeRerankKey(b)]
		if ba != bb {
			return ba > bb
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Article != b.Article {
			return a.Article < b.Article
		}
		return a.ChunkIdx < b.ChunkIdx
	})
	return hits
}

// llmRerankGrades asks the chat model to grade each candidate 0–10 for
// relevance to the query. Returns a map from candidate index to grade (0..1).
func llmRerankGrades(ctx context.Context, lm lmProvider, query string, hits []retrievalHit) (map[int]float64, error) {
	n := len(hits)
	if n > llmRerankTopN {
		n = llmRerankTopN
	}
	var sb strings.Builder
	sb.WriteString("Bewerte die Relevanz jedes Textausschnitts für die Frage auf einer Skala von 0 (irrelevant) bis 10 (beantwortet die Frage direkt).\n")
	sb.WriteString("Antworte NUR mit einem JSON-Array der Form [{\"i\":0,\"score\":7}, ...] ohne weiteren Text.\n\n")
	sb.WriteString("Frage: " + strings.TrimSpace(query) + "\n\n")
	for i := 0; i < n; i++ {
		c := hits[i].Content
		if len(c) > 600 {
			c = c[:600] + "…"
		}
		fmt.Fprintf(&sb, "### Ausschnitt %d\n%s\n\n", i, c)
	}

	tctx, cancel := context.WithTimeout(ctx, llmRerankTimeout)
	defer cancel()
	var out strings.Builder
	err := lm.chatStream(tctx, "Du bist ein präziser Relevanz-Bewerter. Antworte ausschließlich mit JSON.", []chatMsg{{Role: "user", Content: sb.String()}}, &out)
	if err != nil {
		return nil, err
	}
	raw, err := extractFirstJSONValue(out.String())
	if err != nil {
		return nil, fmt.Errorf("rerank grade parse: %w", err)
	}
	var grades []struct {
		I     int     `json:"i"`
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(raw), &grades); err != nil {
		return nil, fmt.Errorf("rerank grade unmarshal: %w", err)
	}
	res := make(map[int]float64, len(grades))
	for _, g := range grades {
		if g.I < 0 || g.I >= n {
			continue
		}
		s := g.Score / 10.0
		if s < 0 {
			s = 0
		}
		if s > 1 {
			s = 1
		}
		res[g.I] = s
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("rerank: empty grade set")
	}
	return res, nil
}

// rerankLLM reorders the top candidates using LLM relevance grades blended
// with the R³ score. Ungraded hits keep their R³ score. Falls back to
// lexical reranking when the LLM call fails.
func rerankLLM(ctx context.Context, lm lmProvider, query string, hits []retrievalHit) []retrievalHit {
	if len(hits) < 2 || lm == nil {
		return hits
	}
	grades, err := llmRerankGrades(ctx, lm, query, hits)
	if err != nil {
		log.Printf("RERANK llm failed (%v), falling back to lexical", err)
		return rerankLexical(query, hits)
	}
	blended := make(map[rerankKey]float64, len(hits))
	for i, h := range hits {
		k := makeRerankKey(h)
		if g, ok := grades[i]; ok {
			blended[k] = h.R3Score*(1-llmBlendWeight) + g*llmBlendWeight
		} else {
			blended[k] = h.R3Score
		}
	}
	sortHitsDeterministic(hits, func(a, b retrievalHit) bool {
		ba := blended[makeRerankKey(a)]
		bb := blended[makeRerankKey(b)]
		if ba != bb {
			return ba > bb
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Article != b.Article {
			return a.Article < b.Article
		}
		return a.ChunkIdx < b.ChunkIdx
	})
	return hits
}

// applyRerank applies the configured rerank mode to the candidate hits.
// Called at the end of searchCandidatesSingle / searchCandidates.
func (r *ragSystem) applyRerank(query string, hits []retrievalHit) []retrievalHit {
	return r.applyRerankContext(context.Background(), query, hits)
}

func (r *ragSystem) applyRerankContext(ctx context.Context, query string, hits []retrievalHit) []retrievalHit {
	if ctx == nil {
		ctx = context.Background()
	}
	mode := rerankModeLexical
	if settings != nil {
		mode = normalizeRerankMode(settings.get().RerankMode)
	}
	switch mode {
	case rerankModeOff:
		return diversifyRetrievalHits(hits, maxPreferredHitsPerSource)
	case rerankModeLLM:
		hits = rerankLLM(ctx, r.getLM(), query, hits)
	default:
		hits = rerankLexical(query, hits)
	}
	return diversifyRetrievalHits(hits, maxPreferredHitsPerSource)
}

// diversifyRetrievalHits preserves the rank order within each source while
// prioritizing evidence from distinct documents near the front of the list.
// It intentionally does not discard overflow hits: callers can still use more
// than maxPreferredHitsPerSource chunks when the answer requires one source.
func diversifyRetrievalHits(hits []retrievalHit, maxPerSource int) []retrievalHit {
	if len(hits) < 2 || maxPerSource < 1 {
		return hits
	}
	selected := make([]retrievalHit, 0, len(hits))
	overflow := make([]retrievalHit, 0, len(hits))
	perSource := make(map[string]int)
	for _, hit := range hits {
		source := strings.TrimSpace(hit.DocumentID)
		if source == "" {
			source = strings.TrimSpace(hit.Article)
		}
		if source == "" {
			source = "unknown"
		}
		if perSource[source] < maxPerSource {
			selected = append(selected, hit)
			perSource[source]++
			continue
		}
		overflow = append(overflow, hit)
	}
	return append(selected, overflow...)
}
