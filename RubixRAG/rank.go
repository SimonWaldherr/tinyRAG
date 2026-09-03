package main

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ─────────────────────────────────────────────────────────────────────────────
// "Ranked RAG" retrieval
//
// Plain vector search over-indexes on semantic similarity alone, which lets
// an old, superseded email or a stale spec sheet outrank a recent, exact
// keyword match. R3 asks tinySQL for a wider candidate set by cosine
// similarity, then re-scores every candidate with a weighted blend of:
//
//   - vector similarity  (semantic closeness to the query)
//   - keyword overlap    (cheap BM25-lite recall boost for exact terms)
//   - recency            (exponential decay favoring newer documents/emails)
//
// The blended score decides final ranking and is returned to the caller so
// citations can show *why* a chunk was picked, not just that it was.
// ─────────────────────────────────────────────────────────────────────────────

// rankedHit's JSON tags exist because /api/search (handlers.go) returns
// these directly to external API clients — snake_case matches every other
// JSON shape in this API (sourceInfo, askJSONResponse, ...) rather than
// Go's default PascalCase field-name serialization.
type rankedHit struct {
	SourceID     string  `json:"source_id"`
	SourceKind   string  `json:"source_kind"`
	SourceName   string  `json:"source_name"`
	LoadID       string  `json:"load_id"`
	LoadedAt     int64   `json:"loaded_at"`
	DocDate      int64   `json:"doc_date"`
	ChunkIdx     int     `json:"chunk_idx"`
	Content      string  `json:"content"`
	VectorScore  float64 `json:"vector_score"`
	KeywordScore float64 `json:"keyword_score"`
	RecencyScore float64 `json:"recency_score"`
	FinalScore   float64 `json:"final_score"`
}

// candidateLimitForK decides how many candidates to pull from the vector
// store before re-ranking down to k: at least configured (defaulting to 80)
// and at least 4x k, so a small k still gets a wide enough pool for the
// keyword/recency re-scoring to matter, capped at maxLimit to bound cost.
func candidateLimitForK(k, configured int) int {
	limit := configured
	if limit <= 0 {
		limit = 80
	}
	if k*4 > limit {
		limit = k * 4
	}
	const maxLimit = 1000
	if limit > maxLimit {
		limit = maxLimit
	}
	return limit
}

// ftsKeywordScorer is an optional vectorStore capability — implemented by
// both backends: tinySQLStore (vectorstore_tinysql.go) via tinySQL's
// FTS_SEARCH (upstream since v0.19.0), and sqliteStore
// (vectorstore_sqlite.go) via a duplicate FTS5 virtual table and its
// bm25() ranking function — scoring candidates with a real BM25 rank
// (term frequency + document-length normalization, stemming/stop-word
// handling per backend) instead of keywordOverlapScore's cruder
// term-presence fraction below. Any future backend that doesn't implement
// this falls back to keywordOverlapScore unchanged, same "graceful
// degrade" shape as vectorCandidatesIndexed's own HNSW-then-scan
// fallback.
type ftsKeywordScorer interface {
	keywordCandidatesFTS(query, embedModel string, limit int) (map[string]float64, error)
}

// ftsCandidateKey identifies one chunk in keywordCandidatesFTS's result map
// — source_id alone isn't unique (a source has many chunks).
func ftsCandidateKey(sourceID string, chunkIdx int) string {
	return fmt.Sprintf("%s\x00%d", sourceID, chunkIdx)
}

// parseFTSCandidateKey inverts ftsCandidateKey, so unionFTSCandidates can
// fetch the chunk a keyword-only score refers to.
func parseFTSCandidateKey(key string) (sourceID string, chunkIdx int, ok bool) {
	i := strings.LastIndex(key, "\x00")
	if i < 0 {
		return "", 0, false
	}
	idx, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return "", 0, false
	}
	return key[:i], idx, true
}

// ftsKeywordScores tries the backend's real BM25 scorer (ftsKeywordScorer
// above), normalizing raw scores to (0,1] by the batch's own max so they
// stay comparable to VectorScore/RecencyScore under the existing
// KeywordWeight calibration (settings.go's default 0.2 assumes roughly a
// 0..1 range, not raw unbounded BM25 — an unnormalized score could
// otherwise let one strong keyword hit swamp semantic similarity
// entirely). Returns nil — not an empty map — on an unsupported backend or
// a failed query, so the caller can tell "use the fallback" apart from "no
// keyword matches at all" (a genuinely empty but non-nil map).
func (r *ragSystem) ftsKeywordScores(query, embedModel string, limit int) map[string]float64 {
	scorer, ok := r.store.(ftsKeywordScorer)
	if !ok {
		return nil
	}
	raw, err := scorer.keywordCandidatesFTS(query, embedModel, limit)
	if err != nil {
		log.Printf("WARN: FTS_SEARCH keyword scoring failed, falling back to term-overlap: %v", err)
		return nil
	}
	max := 0.0
	for _, v := range raw {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		// No positive score in the whole batch (empty result, or a
		// degenerate backend answer): return an empty-but-non-nil map — "no
		// keyword matches" — rather than leaking raw non-positive values
		// into the weighted blend below.
		return map[string]float64{}
	}
	for k, v := range raw {
		raw[k] = v / max
	}
	return raw
}

// chunkKeyFetcher is an optional vectorStore capability — implemented by
// both backends — loading one chunk (with its embedding) by exact
// (source_id, chunk_idx, embed_model) key. unionFTSCandidates below uses it
// to turn keyword-only matches into real scored candidates; a backend
// without it simply keeps the previous vector-pool-only behavior, same
// graceful-degrade shape as ftsKeywordScorer above.
type chunkKeyFetcher interface {
	chunkByKey(sourceID string, chunkIdx int, embedModel string) (rankedHit, []float64, bool)
}

// ftsUnionCap bounds how many keyword-only candidates unionFTSCandidates
// fetches per query — each costs one indexed single-row store lookup, and
// only the strongest few could plausibly out-rank the vector pool anyway.
const ftsUnionCap = 20

// unionFTSCandidates makes hybrid retrieval a true union: any chunk the FTS
// pass scored that ISN'T already among the vector candidates is fetched by
// key, given a real cosine VectorScore against qvec, and appended — so an
// exact keyword match (an error code, a part number, a person's name) is
// reachable even when its embedding lands outside the vector top-N.
// MinVectorSimilarity is deliberately NOT applied to these: they earn their
// slot through keywords precisely because their semantic similarity is weak;
// the blended-score floor (filterByMinFinalScore) still applies to them like
// to every other hit. Department/preset gates apply unchanged — keyword
// matches must not bypass access control.
func (r *ragSystem) unionFTSCandidates(hits []rankedHit, ftsScores map[string]float64, qvec []float64, embedModel string, access map[string][]string, deptCode string, presetKinds []string) []rankedHit {
	return r.unionFTSCandidatesForIdentity(hits, ftsScores, qvec, embedModel, access, deptCode, "", presetKinds)
}

func (r *ragSystem) unionFTSCandidatesForIdentity(hits []rankedHit, ftsScores map[string]float64, qvec []float64, embedModel string, access map[string][]string, deptCode, user string, presetKinds []string) []rankedHit {
	if len(ftsScores) == 0 {
		return hits
	}
	fetcher, ok := r.store.(chunkKeyFetcher)
	if !ok {
		return hits
	}
	present := make(map[string]bool, len(hits))
	for _, h := range hits {
		present[ftsCandidateKey(h.SourceID, h.ChunkIdx)] = true
	}
	type extra struct {
		key   string
		score float64
	}
	var extras []extra
	for key, score := range ftsScores {
		if !present[key] {
			extras = append(extras, extra{key: key, score: score})
		}
	}
	if len(extras) == 0 {
		return hits
	}
	sort.Slice(extras, func(i, j int) bool {
		if extras[i].score != extras[j].score {
			return extras[i].score > extras[j].score
		}
		return extras[i].key < extras[j].key // deterministic tie-break
	})
	if len(extras) > ftsUnionCap {
		extras = extras[:ftsUnionCap]
	}
	for _, e := range extras {
		sourceID, chunkIdx, ok := parseFTSCandidateKey(e.key)
		if !ok {
			continue
		}
		h, emb, ok := fetcher.chunkByKey(sourceID, chunkIdx, embedModel)
		if !ok {
			continue
		}
		if !r.sourceAccessAllowed(access, h.SourceID, h.SourceKind, deptCode, user) || !presetAllowsKind(presetKinds, h.SourceKind) {
			continue
		}
		h.VectorScore = cosineSimilarity(qvec, emb)
		hits = append(hits, h)
	}
	return hits
}

var tokenRe = regexp.MustCompile(`[a-zA-Z0-9äöüÄÖÜß]+`)

// tokenize lowercases and splits s into a set of distinct alphanumeric
// (plus German umlaut/ß) terms of at least 2 characters, the shared term
// representation keywordOverlapScore compares query against content with.
func tokenize(s string) map[string]bool {
	toks := tokenRe.FindAllString(strings.ToLower(s), -1)
	set := make(map[string]bool, len(toks))
	for _, t := range toks {
		if len(t) >= 2 {
			set[t] = true
		}
	}
	return set
}

// keywordOverlapScore is a cheap BM25-lite recall signal: the fraction of
// distinct query terms that literally appear in the candidate chunk.
func keywordOverlapScore(queryTerms map[string]bool, content string) float64 {
	if len(queryTerms) == 0 {
		return 0
	}
	contentTerms := tokenize(content)
	if len(contentTerms) == 0 {
		return 0
	}
	matched := 0
	for t := range queryTerms {
		if contentTerms[t] {
			matched++
		}
	}
	return float64(matched) / float64(len(queryTerms))
}

// recencyScore applies exponential decay based on the document's own date
// (falling back to ingest time when the source has no intrinsic date, e.g.
// a plain text paste). halfLifeDays <= 0 disables recency weighting.
func recencyScore(docDate, loadedAt int64, halfLifeDays float64) float64 {
	if halfLifeDays <= 0 {
		return 0
	}
	ts := docDate
	if ts <= 0 {
		ts = loadedAt
	}
	if ts <= 0 {
		return 0
	}
	ageDays := time.Since(time.Unix(ts, 0)).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	return math.Pow(0.5, ageDays/halfLifeDays)
}

// rankedSearch embeds the query, pulls a wide candidate set by cosine
// similarity, re-scores with the hybrid formula and returns the top k
// hits. access/deptCode (settings.SourceAccess and the caller's
// classified department — "" for an anonymous caller, see department.go)
// enforce per-source-kind access control: matching candidates are dropped
// immediately after the vector search, before scoring/sorting, so a
// denied chunk never reaches assembleContext or the LLM's context at all
// — unlike SourceVisibility/filterCitations below, which only controls
// what's disclosed in an already-generated answer.
func (r *ragSystem) rankedSearch(query string, k int, cfg rankingConfig, embedModel string, access map[string][]string, deptCode string, presetKinds []string) ([]rankedHit, error) {
	return r.rankedSearchForIdentity(query, k, cfg, embedModel, access, deptCode, "", presetKinds)
}

// rankedSearchForIdentity is rankedSearch with the caller's identity.  The
// public wrapper above deliberately retains the old shape for internal/test
// callers, while HTTP and MCP entry points pass the identity needed by a
// document ACL's explicit user allow-list.
func (r *ragSystem) rankedSearchForIdentity(query string, k int, cfg rankingConfig, embedModel string, access map[string][]string, deptCode, user string, presetKinds []string) ([]rankedHit, error) {
	qvec, err := r.embedQueryCached(query, embedModel)
	if err != nil {
		return nil, err
	}
	limit := candidateLimitForK(k, cfg.CandidateLimit)
	if len(access) > 0 || len(presetKinds) > 0 {
		// Some candidates will be dropped by filterByDeptAccess/
		// filterByPresetKinds below; over-fetch further so a k-sized
		// result is still likely even after restricted kinds are excluded.
		limit *= 3
		if limit > 1000 {
			limit = 1000
		}
	}
	hits, err := r.store.vectorCandidates(qvec, embedModel, limit)
	if err != nil {
		return nil, err
	}
	hits = filterByMinSimilarity(hits, cfg.MinVectorSimilarity)
	hits = r.filterByAccess(hits, access, deptCode, user)
	// Preset axis (appSettings.Presets, preset.go) — orthogonal to the
	// department check above, same "drop before scoring" placement.
	hits = filterByPresetKinds(hits, presetKinds)

	// Real BM25 (tinySQL's FTS_SEARCH) when the backend supports it; nil
	// means "unsupported or failed", not "no matches" — see
	// ftsKeywordScores's doc comment — so that case alone falls back to
	// the cruder term-overlap fraction below.
	ftsScores := r.ftsKeywordScores(query, embedModel, limit)
	// True hybrid union: keyword matches the vector top-N missed become
	// real candidates too (see unionFTSCandidates) — before this, FTS
	// scores could only re-rank chunks the vector search already found.
	hits = r.unionFTSCandidatesForIdentity(hits, ftsScores, qvec, embedModel, access, deptCode, user, presetKinds)
	queryTerms := tokenize(query)
	for i := range hits {
		// A non-finite similarity (a NaN/Inf embedding slipped into the
		// store) must not poison the sort below — NaN compares false
		// against everything, leaving such hits randomly placed.
		if math.IsNaN(hits[i].VectorScore) || math.IsInf(hits[i].VectorScore, 0) {
			hits[i].VectorScore = 0
		}
		if ftsScores != nil {
			hits[i].KeywordScore = ftsScores[ftsCandidateKey(hits[i].SourceID, hits[i].ChunkIdx)]
		} else {
			hits[i].KeywordScore = keywordOverlapScore(queryTerms, hits[i].Content)
		}
		hits[i].RecencyScore = recencyScore(hits[i].DocDate, hits[i].LoadedAt, cfg.RecencyHalfLifeDays)
		hits[i].FinalScore = cfg.VectorWeight*hits[i].VectorScore +
			cfg.KeywordWeight*hits[i].KeywordScore +
			cfg.RecencyWeight*hits[i].RecencyScore
	}

	// Simple insertion-sort-by-score is fine at candidate-list scale (<=1000).
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j-1].FinalScore < hits[j].FinalScore; j-- {
			hits[j-1], hits[j] = hits[j], hits[j-1]
		}
	}
	hits = filterByMinFinalScore(hits, cfg.MinFinalScore, cfg.RecencyWeight)
	hits = capHitsPerSource(hits, cfg.MaxHitsPerSource)
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

func (r *ragSystem) filterByAccess(hits []rankedHit, access map[string][]string, deptCode, user string) []rankedHit {
	out := make([]rankedHit, 0, len(hits))
	for _, h := range hits {
		if r.sourceAccessAllowed(access, h.SourceID, h.SourceKind, deptCode, user) {
			out = append(out, h)
		}
	}
	return out
}

// capHitsPerSource keeps at most max hits of any one source_id (order
// preserved, so the strongest hits of each source survive) — a diversity
// guard so one long document can't monopolize every one of the K context
// slots. 0 (rankingConfig.MaxHitsPerSource's default) disables the cap,
// preserving previous behavior.
func capHitsPerSource(hits []rankedHit, max int) []rankedHit {
	if max <= 0 {
		return hits
	}
	perSource := make(map[string]int, len(hits))
	out := make([]rankedHit, 0, len(hits))
	for _, h := range hits {
		if perSource[h.SourceID] >= max {
			continue
		}
		perSource[h.SourceID]++
		out = append(out, h)
	}
	return out
}

// filterByMinFinalScore drops hits whose relevance-for-filtering score
// falls below min — a no-op when min <= 0 (rankingConfig.MinFinalScore's
// default), same convention as filterByMinSimilarity above. See
// MinFinalScore's doc comment (settings.go) for why a zero-KeywordScore
// hit has its RecencyWeight contribution excluded from this check
// specifically. h.FinalScore itself is never modified — only this
// pass/fail decision uses the discounted value.
func filterByMinFinalScore(hits []rankedHit, min float64, recencyWeight float64) []rankedHit {
	if min <= 0 {
		return hits
	}
	out := make([]rankedHit, 0, len(hits))
	for _, h := range hits {
		score := h.FinalScore
		if h.KeywordScore == 0 {
			score -= recencyWeight * h.RecencyScore
		}
		if score >= min {
			out = append(out, h)
		}
	}
	return out
}

// filterByMinSimilarity drops every hit whose VectorScore falls below min —
// a no-op when min <= 0 (rankingConfig.MinVectorSimilarity's default,
// disabling the check so a sparse knowledge base's weakest-available match
// still gets used, exactly as before this setting existed).
func filterByMinSimilarity(hits []rankedHit, min float64) []rankedHit {
	if min <= 0 {
		return hits
	}
	out := make([]rankedHit, 0, len(hits))
	for _, h := range hits {
		if h.VectorScore >= min {
			out = append(out, h)
		}
	}
	return out
}

// filterByDeptAccess drops candidate hits whose source_kind is restricted
// (settings.SourceAccess) to department codes that don't include
// deptCode — see sourceAccessAllowed (settings.go) for the actual match
// logic. This is rankedSearch's enforcement point, run before scoring so
// a denied chunk is never even a candidate for the final top-k.
func filterByDeptAccess(hits []rankedHit, access map[string][]string, deptCode string) []rankedHit {
	if len(access) == 0 {
		return hits
	}
	out := make([]rankedHit, 0, len(hits))
	for _, h := range hits {
		if sourceAccessAllowed(access, h.SourceKind, deptCode) {
			out = append(out, h)
		}
	}
	return out
}

// assembleContext turns ranked hits into an LLM-ready context block plus a
// parallel citation list the frontend renders next to the answer. The
// first time a source is cited, every chunk belonging to it (not just the
// one that matched the query) is pulled in and stitched back into
// original chunk order — a query matching, say, the middle of an email
// shouldn't cut the LLM off from the rest of that same email. A second
// hit against an already-cited source contributes nothing further (its
// content is already in context) and isn't cited again.
//
// The same completeness idea extends across source boundaries for emails:
// a mail and its attachments are separate sources (each its own
// source_id, see ingestEmailAttachment in ingest.go), so a query matching
// only the mail body would otherwise never surface the offer PDF attached
// to it — or, matching only the attachment, lose the mail that explains
// its context. expandEmailFamilies below pulls those siblings in as
// additional, individually cited context blocks. access/deptCode gate the
// siblings exactly like rankedSearch gated the hits themselves: an
// attachment whose source_kind is department-restricted must not ride
// into context on the coattails of its unrestricted parent mail.
func (r *ragSystem) assembleContext(hits []rankedHit, cfg rankingConfig, access map[string][]string, deptCode string, presetKinds []string) (string, []sourceInfo) {
	return r.assembleContextForIdentity(hits, cfg, access, deptCode, "", presetKinds)
}

// assembleContextForIdentity keeps speculative email-family expansion under
// the same document ACL as the ranked hits.  Without this, an allowed parent
// mail could accidentally pull a more restricted attachment into context.
func (r *ragSystem) assembleContextForIdentity(hits []rankedHit, cfg rankingConfig, access map[string][]string, deptCode, user string, presetKinds []string) (string, []sourceInfo) {
	var b strings.Builder
	seenSources := map[string]bool{}
	// seenContent dedups by NORMALIZED CONTENT, on top of seenSources'
	// per-SOURCE dedup above — catches the "same blank boilerplate PDF
	// attached to three different emails" case: three distinct source_ids
	// (so seenSources never fires), byte-identical (after
	// collapseRepeatedRuns/whitespace trimming) content. Each such source
	// still earns its own citation (real provenance, e.g. "this exact form
	// was attached to email X too") via writeCitedContent below — only the
	// repeated content text itself, which adds no new information the
	// second/third time, is elided.
	seenContent := map[string]bool{}
	citations := make([]sourceInfo, 0, len(hits))
	maxSources := cfg.MaxSources // 0 = unlimited, checked below

	// All hits of one source are gathered up front so its single citation
	// block can cover EVERY matched position: with windowed context
	// (ContextChunksBefore/After >= 0), a query matching both the intro and
	// a section deep inside the same long document gets a window around
	// each match — the deeper match used to be dropped entirely because the
	// source was already "seen" by the time it came up.
	matchedBySource := make(map[string][]rankedHit, len(hits))
	for _, h := range hits {
		matchedBySource[h.SourceID] = append(matchedBySource[h.SourceID], h)
	}

	for _, h := range hits {
		if seenSources[h.SourceID] {
			continue
		}
		if maxSources > 0 && len(citations) >= maxSources {
			break
		}
		seenSources[h.SourceID] = true
		marker := len(citations) + 1
		citations = append(citations, sourceInfo{
			SourceID:   h.SourceID,
			SourceKind: h.SourceKind,
			SourceName: h.SourceName,
			LoadID:     h.LoadID,
			LoadedAt:   h.LoadedAt,
			DocDate:    h.DocDate,
			Marker:     marker,
		})

		// The label must equal marker exactly — the model is instructed
		// (prompts/index.md) to cite inline as "[Qn]" using this same
		// numbering, and filterCitations below matches those markers back
		// to this Marker field, not to array position (citations entries
		// can be dropped by filterCitations without renumbering the rest).
		fmt.Fprintf(&b, "[Quelle %d: %s]\n", marker, h.SourceName)
		var content strings.Builder
		for _, c := range r.fetchAllSourceChunks(h.SourceID, matchedBySource[h.SourceID], cfg.ContextChunksBefore, cfg.ContextChunksAfter) {
			content.WriteString(c)
			content.WriteString("\n")
		}
		// collapseRepeatedRuns first (Task 3): normalizes away incidental
		// whitespace/punctuation-run-length differences from the source
		// document itself, so writeCitedContent's dedup key (Task 1) below
		// recognizes two chunks as the same content even when one extraction
		// happened to produce a slightly longer dot-leader/dash-run than the
		// other.
		writeCitedContent(&b, seenContent, collapseRepeatedRuns(content.String()), maxPrimaryContentCharsOrDefault(cfg))
	}

	// Every hit got filtered out upstream (rankedSearch's
	// filterByMinFinalScore/filterByMinSimilarity), or there were no hits
	// at all — say so explicitly rather than handing the model a blank
	// "Kontext:\n" with nothing after it. Shorter than dumping weak
	// sources would have been, so this is a token saving too, not just a
	// clarity improvement.
	if len(citations) == 0 {
		b.WriteString("(Keine ausreichend relevanten Quellen in der Wissensbasis gefunden.)\n")
	}

	citations = r.expandEmailFamilies(&b, seenSources, seenContent, citations, cfg, access, deptCode, user, presetKinds)
	return b.String(), citations
}

// contentDedupKey returns the content-dedup comparison key for one chunk of
// context text: the trimmed content, hashed via the same sha256 contentHash
// helper ingestDocument (ingest.go) already uses for its own
// unchanged-since-last-load check. Cheap, and avoids keeping every
// already-written content block's full text around just to compare against
// — see writeCitedContent below, the only caller.
func contentDedupKey(content string) string {
	return contentHash(strings.TrimSpace(content))
}

// writeCitedContent writes one cited source/sibling's content into b,
// truncated to maxChars — unless its normalized content (content must
// already be collapseRepeatedRuns'd by the caller, see assembleContext/
// expandEmailFamilies) already matches something written earlier in this
// same context-assembly call (seenContent, populated here). This is the
// content-based dedup on top of assembleContext's existing per-SOURCE dedup
// (seenSources): two different sources whose content is byte-identical
// (or identical modulo incidental whitespace/punctuation-run length, thanks
// to the caller's collapseRepeatedRuns pass) each still earn their citation
// slot above — dropping the citation itself would lose real provenance
// ("this exact boilerplate form was also attached to this other email") —
// but only the FIRST occurrence's content is actually written; later
// occurrences get a short note instead, saving the tokens a byte-identical
// repeat would otherwise cost with zero added information.
func writeCitedContent(b *strings.Builder, seenContent map[string]bool, content string, maxChars int) {
	key := contentDedupKey(content)
	if seenContent[key] {
		b.WriteString("(Inhalt identisch mit einer bereits oben zitierten Quelle — nicht erneut ausgegeben.)\n")
		return
	}
	seenContent[key] = true
	b.WriteString(truncateContentWithNote(content, maxChars))
	b.WriteString("\n")
}

// emailFamilyKinds are the source kinds whose sources come in families —
// one parent mail plus zero or more attachments sharing its source_id
// prefix (":attachment:<idx>:<filename>" suffix, ingestEmailAttachment).
// Every other kind (files, wiki pages, tickets, ...) is a standalone
// source with no siblings to expand.
var emailFamilyKinds = map[string]bool{
	"pst_email": true, "pst_attachment": true,
	"imap_email": true, "imap_attachment": true,
	"outlook_email": true, "outlook_attachment": true,
}

// emailFamilyRoot returns the parent mail's source_id for any member of an
// email family — an attachment's ":attachment:..." suffix is stripped, a
// mail's own id passes through unchanged.
func emailFamilyRoot(sourceID string) string {
	if i := strings.Index(sourceID, ":attachment:"); i >= 0 {
		return sourceID[:i]
	}
	return sourceID
}

// Budget caps for expandEmailFamilies: attachments can be arbitrarily
// large (a matched mail might carry a 200-page PDF), and unlike the
// ranked hits themselves — which earned their context slot by score —
// siblings are speculative additions. Cap both how many ride along in
// total and how much text each may contribute, so family expansion can
// widen context but never flood it. Each has a rankingConfig field
// (settings.go) that overrides it per deployment — these consts are only
// the "0 means unset" fallback.
const (
	maxFamilySiblingsDefault = 6
	maxSiblingCharsDefault   = 4000
	// maxPrimaryContentCharsDefault caps how much text a single cited
	// (matched) source may contribute to context. Without this, a hit
	// against one chunk of a large multi-chunk source (a long SharePoint
	// file, a PST attachment PDF/XLSX split into dozens of chunks) pulled
	// in the ENTIRE source unconditionally via fetchAllSourceChunks below —
	// making the "K (Kontext-Chunks)" setting misleading: K only bounds how
	// many *hits* are picked, not how much text each hit's full-source
	// expansion actually contributes. A large document matching several of
	// the K hits could balloon context to hundreds of chunks' worth of
	// text. Deliberately more generous than maxSiblingCharsDefault (this is
	// the source that actually matched the query, not a speculative
	// sibling), while still bounding the worst case to a predictable size
	// per hit. rankingConfig.ContextChunksBefore/After (also configurable)
	// bounds this from the other direction — fewer chunks pulled in means
	// less to truncate here in the first place.
	maxPrimaryContentCharsDefault = 6000
	// minSiblingContentChars skips an email-family sibling whose content,
	// after trimming, is shorter than this — a bare header block (From/
	// To/Date/Subject, no body) is short and formulaic; not worth its own
	// citation slot and content dump just to say "this belongs to the
	// same email". Applies only to speculative siblings
	// (expandEmailFamilies), not the K hits themselves — those earned
	// their slot by score regardless of length.
	minSiblingContentChars = 200
)

// maxPrimaryContentCharsOrDefault resolves rankingConfig.MaxPrimaryContentChars.
func maxPrimaryContentCharsOrDefault(cfg rankingConfig) int {
	if cfg.MaxPrimaryContentChars > 0 {
		return cfg.MaxPrimaryContentChars
	}
	return maxPrimaryContentCharsDefault
}

// maxSiblingCharsOrDefault resolves rankingConfig.MaxSiblingChars.
func maxSiblingCharsOrDefault(cfg rankingConfig) int {
	if cfg.MaxSiblingChars > 0 {
		return cfg.MaxSiblingChars
	}
	return maxSiblingCharsDefault
}

// maxFamilySiblingsOrDefault resolves rankingConfig.MaxFamilySiblings.
func maxFamilySiblingsOrDefault(cfg rankingConfig) int {
	if cfg.MaxFamilySiblings > 0 {
		return cfg.MaxFamilySiblings
	}
	return maxFamilySiblingsDefault
}

// truncateContentWithNote cuts s at a rune boundary once it exceeds max
// bytes, appending a note so the model (and a human reading the context
// dump) knows content was cut rather than mistaking the cutoff for the
// document's natural end. Shared by assembleContext's per-hit cap above
// and expandEmailFamilies' sibling cap below.
func truncateContentWithNote(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… [gekürzt — vollständiger Inhalt über die Quellenansicht]"
}

// expandEmailFamilies appends the not-yet-cited siblings of every
// email-family hit (the attachments of a matched mail; the parent mail —
// and its other attachments — of a matched attachment) as additional
// cited context blocks. Returns the extended citations list; b, seenSources
// and seenContent (the content-based dedup set — see writeCitedContent) are
// all extended in place. Siblings get real markers so the model can cite
// them like any other source, and filterCitations treats them identically
// downstream. Uses one listSources scan, and only when at least one hit is
// an email-family kind at all — a question over files/wiki/tickets never
// pays for this.
func (r *ragSystem) expandEmailFamilies(b *strings.Builder, seenSources map[string]bool, seenContent map[string]bool, citations []sourceInfo, cfg rankingConfig, access map[string][]string, deptCode, user string, presetKinds []string) []sourceInfo {
	roots := map[string]bool{}
	for _, c := range citations {
		if emailFamilyKinds[c.SourceKind] {
			roots[emailFamilyRoot(c.SourceID)] = true
		}
	}
	if len(roots) == 0 {
		return citations
	}
	sources, err := r.listSources()
	if err != nil {
		return citations // best-effort: the ranked hits themselves are already in context
	}

	maxSiblings := maxFamilySiblingsOrDefault(cfg)
	added := 0
	for _, src := range sources {
		if added >= maxSiblings {
			break
		}
		if cfg.MaxSources > 0 && len(citations) >= cfg.MaxSources {
			break
		}
		if seenSources[src.SourceID] || !roots[emailFamilyRoot(src.SourceID)] {
			continue
		}
		if !r.sourceAccessAllowed(access, src.SourceID, src.SourceKind, deptCode, user) || !presetAllowsKind(presetKinds, src.SourceKind) {
			continue
		}
		content, ok := r.fetchSourceContent(src.SourceID)
		trimmed := strings.TrimSpace(content)
		if !ok || len(trimmed) < minSiblingContentChars {
			continue
		}
		// collapseRepeatedRuns before the dedup-key check (Task 3 before
		// Task 1, same reasoning as assembleContext's primary-content path
		// above): two siblings differing only in an incidental
		// whitespace/punctuation-run length still get recognized as the
		// same content.
		content = collapseRepeatedRuns(content)

		seenSources[src.SourceID] = true
		added++
		marker := len(citations) + 1
		src.Marker = marker
		citations = append(citations, src)
		fmt.Fprintf(b, "[Quelle %d: %s — gehört zur selben E-Mail wie eine der obigen Quellen]\n", marker, src.SourceName)
		writeCitedContent(b, seenContent, content, maxSiblingCharsOrDefault(cfg))
		b.WriteString("\n")
	}
	return citations
}

// citeMarkerRe matches the inline "[Qn]" citation markers the system
// prompt (prompts/index.md) instructs the model to produce — the same
// numbering assembleContext handed it via "[Quelle N: ...]" blocks.
var citeMarkerRe = regexp.MustCompile(`\[Q(\d+)\]`)

// usedCitationMarkers returns the set of citation numbers that actually
// appear as a "[Qn]" marker in answerText, so filterCitations can tell a
// source that genuinely grounded the answer from one the vector search
// merely retrieved as a candidate.
func usedCitationMarkers(answerText string) map[int]bool {
	used := map[int]bool{}
	for _, m := range citeMarkerRe.FindAllStringSubmatch(answerText, -1) {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil {
			used[n] = true
		}
	}
	return used
}

// filterCitations narrows citations down to the ones actually worth
// returning to the client: cited by marker in the model's own answer text
// AND not configured as hidden for their source_kind (settings.
// SourceVisibility, see settings.go). Both retrieved-but-unused candidates
// and visibility-hidden kinds (e.g. "pst_email" grounding an answer
// without being named as a source) are dropped — the underlying chunks
// already did their job informing the answer via assembleContext; this
// only controls what's disclosed afterward. Order and each surviving
// entry's Marker are preserved so the frontend can still resolve inline
// [Qn] markers for whichever citations remain.
func filterCitations(citations []sourceInfo, answerText string, s appSettings) []sourceInfo {
	used := usedCitationMarkers(answerText)
	out := make([]sourceInfo, 0, len(citations))
	for _, c := range citations {
		if !used[c.Marker] {
			continue
		}
		if !s.citationsVisible(c.SourceKind) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// fetchAllSourceChunks returns sourceID's chunks in original chunk_idx
// order, fetched in a single store round trip. matched holds every ranked
// hit against this source — each hit's already-scored Content is
// substituted back in at its position instead of trusting the freshly
// fetched copy, so a concurrent write between the ranker's search and this
// call can't make a cited source's context disagree with what was scored.
//
// before/after (rankingConfig.ContextChunksBefore/After) bound the window
// of neighboring chunks (by chunk_idx) returned around EACH matched index,
// instead of the source's entire chunk list — e.g. before=1, after=1 with
// matches at chunk 2 and chunk 40 returns chunks 1-3 and 39-41, separated
// by a "[…]" gap marker so the model knows unrelated middle content was
// omitted rather than mistaking the jump for continuous text. Either side
// negative disables windowing entirely (the pre-existing "whole source"
// behavior) — see rankingConfig.ContextChunksBefore's doc comment for why
// a mismatched pair doesn't get a partial interpretation.
func (r *ragSystem) fetchAllSourceChunks(sourceID string, matched []rankedHit, before, after int) []string {
	matchedContent := make(map[int]string, len(matched))
	matchedIdxs := make([]int, 0, len(matched))
	for _, m := range matched {
		if _, dup := matchedContent[m.ChunkIdx]; !dup {
			matchedIdxs = append(matchedIdxs, m.ChunkIdx)
		}
		matchedContent[m.ChunkIdx] = m.Content
	}
	chunks, err := r.store.fetchSourceChunks(sourceID)
	if err != nil || len(chunks) == 0 {
		// Store unavailable/raced: fall back to the matched chunks
		// themselves, in original chunk order.
		sort.Ints(matchedIdxs)
		out := make([]string, 0, len(matchedIdxs))
		for _, idx := range matchedIdxs {
			out = append(out, matchedContent[idx])
		}
		return out
	}
	windowed := before >= 0 && after >= 0
	inWindow := func(idx int) bool {
		for _, m := range matchedIdxs {
			if idx >= m-before && idx <= m+after {
				return true
			}
		}
		return false
	}
	out := make([]string, 0, len(chunks))
	prevIdx := -1 // last included chunk_idx, for gap markers between windows
	for _, c := range chunks {
		if windowed && !inWindow(c.ChunkIdx) {
			continue
		}
		if windowed && prevIdx >= 0 && c.ChunkIdx > prevIdx+1 {
			out = append(out, "[…]")
		}
		if content, ok := matchedContent[c.ChunkIdx]; ok {
			out = append(out, content)
		} else {
			out = append(out, c.Content)
		}
		prevIdx = c.ChunkIdx
	}
	return out
}
