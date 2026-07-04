package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type weightedSearchQuery struct {
	Query  string
	Weight float64
}

var searchTokenSplitter = regexp.MustCompile(`[^\p{L}\p{N}\-_.#+/]+`)

func splitSearchTokens(s string) []string {
	raw := searchTokenSplitter.Split(strings.TrimSpace(s), -1)
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func hasLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			return true
		}
	}
	return false
}

func isAlphaToken(s string) bool {
	return hasLetter(s) && !hasDigit(s)
}

// isAlphaNumUnder reports whether r is a letter, digit, or underscore —
// used to identify whether a SQL keyword is a standalone token.
func isAlphaNumUnder(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') || r == '_'
}

// sqlBlockCommentRe matches /* … */ SQL block comments (non-greedy).
var sqlBlockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

// sqlLineCommentRe matches -- … to end-of-line SQL line comments.
var sqlLineCommentRe = regexp.MustCompile(`--[^\n]*`)

// stripSQLComments removes SQL block comments (/* … */) and line comments (-- …)
// from a SQL string so that hidden keywords cannot bypass validation.
func stripSQLComments(q string) string {
	q = sqlBlockCommentRe.ReplaceAllString(q, " ")
	q = sqlLineCommentRe.ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

func stopwordSet() map[string]bool {
	return map[string]bool{
		"was": true, "weisst": true, "weißt": true, "du": true, "ueber": true, "über": true,
		"wer": true, "ist": true, "erzaehl": true, "erzähl": true, "mir": true, "von": true,
		"tell": true, "me": true, "about": true, "what": true, "who": true,
		"the": true, "ein": true, "eine": true, "und": true, "oder": true, "für": true, "fuer": true,
		"with": true, "mit": true, "der": true, "die": true, "das": true,
	}
}

func looksTechnicalQuery(q string) bool {
	tokens := splitSearchTokens(q)
	techScore := 0
	for _, tok := range tokens {
		if hasDigit(tok) {
			techScore++
		}
		if strings.ContainsAny(tok, "-_/") {
			techScore++
		}
		low := strings.ToLower(tok)
		if strings.HasSuffix(low, "rs") || strings.HasSuffix(low, "zz") || strings.Contains(low, "lager") || strings.Contains(low, "bearing") {
			techScore++
		}
	}
	return techScore >= 2
}

func expandRetrievalQueries(q string) []weightedSearchQuery {
	base := strings.TrimSpace(refineSearchQuery(q))
	if base == "" {
		return nil
	}
	seen := map[string]bool{}
	add := func(out *[]weightedSearchQuery, query string, weight float64) {
		query = strings.TrimSpace(query)
		if query == "" {
			return
		}
		key := strings.ToLower(query)
		if seen[key] {
			return
		}
		seen[key] = true
		*out = append(*out, weightedSearchQuery{Query: query, Weight: weight})
	}

	var out []weightedSearchQuery
	add(&out, base, 1.00)

	tokens := splitSearchTokens(base)
	if len(tokens) == 0 {
		return out
	}

	if len(tokens) >= 2 && (hasDigit(tokens[0]) || hasDigit(tokens[1])) {
		add(&out, tokens[0]+" "+tokens[1], 0.98)
	}
	if hasDigit(tokens[0]) {
		add(&out, tokens[0], 0.95)
	}

	stopwords := stopwordSet()
	var alpha []string
	for _, tok := range tokens {
		low := strings.ToLower(tok)
		if stopwords[low] {
			continue
		}
		if isAlphaToken(tok) {
			alpha = append(alpha, tok)
		}
	}
	if len(alpha) > 0 {
		add(&out, alpha[len(alpha)-1], 0.93)
		if len(alpha) > 1 {
			add(&out, strings.Join(alpha, " "), 0.92)
		}
	}
	if len(tokens) >= 3 && hasDigit(tokens[0]) && hasLetter(tokens[len(tokens)-1]) {
		add(&out, tokens[0]+" "+tokens[len(tokens)-1], 0.91)
	}

	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func expandExternalSearchQueries(q string) []string {
	base := strings.TrimSpace(refineSearchQuery(q))
	if base == "" {
		return []string{q}
	}
	seen := map[string]bool{}
	add := func(out *[]string, query string) {
		query = strings.TrimSpace(query)
		if query == "" {
			return
		}
		key := strings.ToLower(query)
		if seen[key] {
			return
		}
		seen[key] = true
		*out = append(*out, query)
	}

	var out []string
	add(&out, base)
	tokens := splitSearchTokens(base)

	// Exact-phrase variant for short, multi-word queries
	if len(tokens) >= 2 && len(tokens) <= 5 {
		add(&out, `"`+base+`"`)
	}

	// Product-code / part-number: quote the first two tokens if either contains digits
	if len(tokens) >= 2 && (hasDigit(tokens[0]) || hasDigit(tokens[1])) {
		add(&out, `"`+tokens[0]+` `+tokens[1]+`"`)
		add(&out, tokens[0]+" "+tokens[1])
	}

	// Technical queries: add language-appropriate detail/datasheet suffix variants
	if looksTechnicalQuery(base) {
		// Detect query language: German text uses umlauts or common German words
		isGermanQuery := strings.ContainsAny(base, "äöüÄÖÜß") ||
			hasAnyWord(base, []string{"und", "der", "die", "das", "von", "für", "mit"})
		if isGermanQuery {
			add(&out, "Technische Details "+base)
			add(&out, base+" Datenblatt")
		} else {
			add(&out, base+" technical details")
			add(&out, base+" specification")
		}
	}

	// Add keyword-only variant (strip stopwords)
	stopwords := stopwordSet()
	var keywords []string
	for _, tok := range tokens {
		if !stopwords[strings.ToLower(tok)] && len(tok) > 2 {
			keywords = append(keywords, tok)
		}
	}
	if len(keywords) > 0 && len(keywords) < len(tokens) {
		add(&out, strings.Join(keywords, " "))
	}

	// Add retrieval-style variants for good measure
	for _, q := range expandRetrievalQueries(base) {
		add(&out, q.Query)
	}

	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// refineSearchQuery attempts to extract an entity-like phrase from the
// user's question (e.g., "was weißt du über Ettling") to narrow the
// retrieval query. Falls back to the original question.
func refineSearchQuery(q string) string {
	q = strings.TrimSpace(q)
	low := strings.ToLower(q)
	patterns := []string{
		`was weißt du über (.+)`,
		`wer ist (.+)`,
		`erzähl mir von (.+)`,
		`was ist (.+)`,
		`worum geht es bei (.+)`,
		`tell me about (.+)`,
		`who is (.+)`,
		`what is (.+)`,
		`qui est (.+)`,
		`parle-moi de (.+)`,
		`qu[ei]én es (.+)`,
		`háblame de (.+)`,
		`hablame de (.+)`,
		`chi è (.+)`,
		`chi e (.+)`,
		`parlami di (.+)`,
		`quem é (.+)`,
		`quem e (.+)`,
		`fale sobre (.+)`,
		`wie is (.+)`,
		`vertel me over (.+)`,
		`kim jest (.+)`,
		`powiedz mi o (.+)`,
	}
	for _, p := range patterns {
		re := regexp.MustCompile(`(?i)` + p)
		if m := re.FindStringSubmatch(low); len(m) >= 2 {
			candidate := strings.TrimSpace(m[1])
			// restore original casing by finding candidate in original
			idx := strings.Index(strings.ToLower(q), candidate)
			if idx >= 0 {
				return strings.TrimSpace(q[idx : idx+len(candidate)])
			}
			return candidate
		}
	}
	return q
}

// analyzeQuestion asks the LM to decide whether to answer directly or
// to request additional retrieval. It returns a parsed map with at
// least an "action" key (ANSWER_DIRECT or RETRIEVE_MORE) and optional
// parameters (k, threshold, query).
func (r *ragSystem) analyzeQuestion(question, summary string) (map[string]any, error) {
	system := `You are a retrieval-planning agent for a RAG assistant.

Task:
- Decide whether the current retrieval candidates are already sufficient.
- Prefer ANSWER_DIRECT only if the likely answer can be grounded with good confidence from the shown candidates.
- Prefer RETRIEVE_MORE if the question is broad, ambiguous, entity-specific but under-supported, or likely needs fresher/more precise context.
- If useful, suggest a shorter and more retrieval-friendly query.

Rules:
- Return ONLY one JSON object.
- No markdown, no prose, no code fences.
- Allowed actions: "ANSWER_DIRECT", "RETRIEVE_MORE".
- Use conservative judgment. If uncertain, choose RETRIEVE_MORE.
- Keep k between 4 and 24.
- Keep threshold between 0.45 and 0.9.

Examples:
{"action":"ANSWER_DIRECT"}
{"action":"RETRIEVE_MORE","k":8,"threshold":0.6,"query":"Karte.Bayern"}
{"action":"RETRIEVE_MORE","k":12,"threshold":0.55}
`
	user := fmt.Sprintf("Question: %s\n\nCandidates: %s", question, summary)
	msgs := []chatMsg{{Role: "user", Content: user}}

	var buf bytes.Buffer
	if err := r.getLM().chatStream(context.Background(), system, msgs, &buf); err != nil {
		return nil, err
	}
	out := buf.String()
	// Try to find the first JSON object in the output
	i := strings.Index(out, "{")
	if i == -1 {
		// fallback heuristic
		if strings.Contains(strings.ToUpper(out), "ANSWER_DIRECT") {
			return map[string]any{"action": "ANSWER_DIRECT"}, nil
		}
		return map[string]any{"action": "RETRIEVE_MORE", "k": r.k, "threshold": 0.6}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[i:]), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fetchNeighborContent loads the content of a chunk at (article, chunk_idx).
// fetchNeighborContent returns the content for a specific chunk index
// of an article, used to include context neighbors around hits.
func (r *ragSystem) fetchNeighborContent(article string, chunkIdx int) (string, bool) {
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	return r.chunkStore.fetchNeighborContent(article, chunkIdx, roleAndACLFilterSQL(activeRole))
}

// listSources returns distinct article names with their chunk counts
// listSources returns metadata about stored articles and their chunk counts.
func (r *ragSystem) listSources() []map[string]any {
	role := "it"
	if settings != nil {
		role = settings.get().ActiveRole
	}
	return r.listSourcesForRole(role)
}

func (r *ragSystem) listSourcesForRole(role string) []map[string]any {
	return r.chunkStore.listSources(roleAndACLFilterSQL(role))
}

// deleteSource removes all chunks belonging to `article` and persists
// the change.
func (r *ragSystem) deleteSource(article string) error {
	role := "it"
	if settings != nil {
		role = settings.get().ActiveRole
	}
	return r.deleteSourceForRole(article, role)
}

func (r *ragSystem) deleteSourceForRole(article, role string) error {
	if err := r.chunkStore.deleteSource(article, roleAndACLFilterSQL(role)); err != nil {
		return err
	}
	return r.save()
}
