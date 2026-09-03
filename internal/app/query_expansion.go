package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type weightedSearchQuery struct {
	Query  string
	Weight float64
}

const (
	retrievalPlannerMinK          = 1
	retrievalPlannerMaxK          = 24
	retrievalPlannerMinThreshold  = 0.45
	retrievalPlannerMaxThreshold  = 0.90
	retrievalPlannerQuestionRunes = 1200
	retrievalPlannerSummaryRunes  = 2400
	retrievalPlannerQueryRunes    = 500
)

// retrievalPlannerTimeout keeps the optional planning request bounded. It is
// a variable so tests can validate the fail-open path without waiting for the
// production timeout.
var retrievalPlannerTimeout = 8 * time.Second

type retrievalPlan struct {
	Action    string
	K         int
	Threshold float64
	Query     string
}

func boundedPlannerK(value, fallback int) int {
	if fallback < retrievalPlannerMinK {
		fallback = retrievalPlannerMinK
	}
	if fallback > retrievalPlannerMaxK {
		fallback = retrievalPlannerMaxK
	}
	if value < retrievalPlannerMinK {
		return fallback
	}
	if value > retrievalPlannerMaxK {
		return retrievalPlannerMaxK
	}
	return value
}

func plannerNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func boundedPlannerQuery(value any, fallback string) string {
	query, ok := value.(string)
	if !ok {
		return fallback
	}
	query = strings.Join(strings.Fields(query), " ")
	if query == "" {
		return fallback
	}
	runes := []rune(query)
	if len(runes) > retrievalPlannerQueryRunes {
		query = string(runes[:retrievalPlannerQueryRunes])
	}
	return query
}

// normalizeRetrievalPlan treats model output as a bounded suggestion. The
// configured relevance threshold remains a floor, so an LLM cannot relax a
// caller's retrieval-quality policy.
func normalizeRetrievalPlan(raw map[string]any, fallbackK int, threshold float64, fallbackQuery string) retrievalPlan {
	plan := retrievalPlan{
		Action:    "RETRIEVE_MORE",
		K:         boundedPlannerK(fallbackK, fallbackK),
		Threshold: clampUnitInterval(threshold),
		Query:     fallbackQuery,
	}
	if raw == nil {
		return plan
	}
	if action, ok := raw["action"].(string); ok && strings.EqualFold(strings.TrimSpace(action), "ANSWER_DIRECT") {
		plan.Action = "ANSWER_DIRECT"
	}
	if value, ok := plannerNumber(raw["k"]); ok {
		plan.K = boundedPlannerK(int(value), plan.K)
	}
	if value, ok := plannerNumber(raw["threshold"]); ok &&
		value >= retrievalPlannerMinThreshold && value <= retrievalPlannerMaxThreshold && value > plan.Threshold {
		plan.Threshold = value
	}
	if value, ok := raw["query"]; ok {
		plan.Query = boundedPlannerQuery(value, plan.Query)
	}
	return plan
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

// configuredTerminology returns the operator-configured terminology table,
// or nil if none is configured / settings aren't initialized yet.
func configuredTerminology() []terminologyEntry {
	if settings == nil {
		return nil
	}
	return settings.get().Terminology
}

// expandTerminologyVariants returns additional standalone query variants for
// any configured term (or one of its expansions) found in the query,
// substituting in every OTHER member of that term's equivalence class. This
// lets an operator bridge an abbreviation to its full form (or a domain
// synonym pair) without relying on the embedding model alone to know the
// mapping. Single-word terms must match a whole token (case-insensitive) to
// avoid matching inside an unrelated longer word; multi-word terms match by
// substring against the full query.
func expandTerminologyVariants(base string, tokens []string, terms []terminologyEntry) []string {
	if len(terms) == 0 || len(tokens) == 0 {
		return nil
	}
	lowBase := strings.ToLower(base)
	tokenSet := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		tokenSet[strings.ToLower(t)] = true
	}

	var out []string
	for _, entry := range terms {
		members := append([]string{entry.Term}, entry.Expansions...)
		matched := ""
		for _, m := range members {
			lm := strings.ToLower(strings.TrimSpace(m))
			if lm == "" {
				continue
			}
			if strings.Contains(lm, " ") {
				if strings.Contains(lowBase, lm) {
					matched = m
					break
				}
				continue
			}
			if tokenSet[lm] {
				matched = m
				break
			}
		}
		if matched == "" {
			continue
		}
		for _, m := range members {
			if m == "" || strings.EqualFold(m, matched) {
				continue
			}
			out = append(out, m)
		}
	}
	return out
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

	// Terminology variants come first (after the verbatim query) so a
	// configured abbreviation/synonym bridge survives the cap below even
	// when several of the generic heuristics also fire.
	for _, variant := range expandTerminologyVariants(base, tokens, configuredTerminology()) {
		add(&out, variant, 0.94)
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

	for _, variant := range expandTerminologyVariants(base, tokens, configuredTerminology()) {
		add(&out, variant)
	}

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
	return r.analyzeQuestionContext(context.Background(), question, summary)
}

func (r *ragSystem) analyzeQuestionContext(parent context.Context, question, summary string) (map[string]any, error) {
	if parent == nil {
		parent = context.Background()
	}
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
	question = truncateRunes(strings.TrimSpace(question), retrievalPlannerQuestionRunes)
	summary = truncateRunes(strings.TrimSpace(summary), retrievalPlannerSummaryRunes)
	user := fmt.Sprintf("Question: %s\n\nCandidates: %s", question, summary)
	msgs := []chatMsg{{Role: "user", Content: user}}

	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(parent, retrievalPlannerTimeout)
	defer cancel()
	if err := r.getLM().chatStream(ctx, system, msgs, &buf); err != nil {
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
	jsonText, err := extractFirstJSONValue(out[i:])
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonText), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fetchNeighborContent loads a neighboring chunk from the same source document.
// Article matching remains only as a legacy fallback for rows without a stable
// document ID.
func (r *ragSystem) fetchNeighborContent(documentID, article string, chunkIdx int) (string, bool) {
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	return r.chunkStore.fetchNeighborContent(documentID, article, chunkIdx, roleAndACLFilterSQL(activeRole))
}

// listSources returns metadata about stored source documents and their chunk
// counts. Titles remain presentation metadata, not source identity.
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

// deleteSource removes a legacy article source and persists the change.
func (r *ragSystem) deleteSource(article string) error {
	role := "it"
	if settings != nil {
		role = settings.get().ActiveRole
	}
	return r.deleteSourceForRole("", article, role)
}

func (r *ragSystem) deleteSourceForRole(documentID, article, role string) error {
	if err := r.chunkStore.deleteSource(documentID, article, roleAndACLFilterSQL(role)); err != nil {
		return err
	}
	return r.save()
}
