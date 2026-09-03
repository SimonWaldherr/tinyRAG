package main

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Provenance-aware RAG orchestrator
//
// ragSystem coordinates the LLM clients (embed one profile, chat via a
// per-request-selectable profile — see llm.go/settings.go) and a vectorStore
// (see vectorstore.go) that actually persists/searches chunks. It never
// touches tinySQL (or any other backend) directly — that's the whole point
// of the vectorStore seam: swapping storage backends later only means
// implementing that interface again, not touching this file.
//
// Every chunk row records WHERE it came from (source_id/source_kind/
// source_name), WHICH import run produced it (load_id/loaded_at) and a
// content hash. This lets us:
//   - always cite the originating file/email/module when answering,
//   - detect that a source is unchanged and skip re-embedding it, and
//   - delete every old chunk for a source before inserting the new ones
//     when a source is re-ingested.
// ─────────────────────────────────────────────────────────────────────────────

type ragSystem struct {
	store vectorStore
	// sourceACLs carries optional per-source rules in addition to the
	// deployment-wide source_kind rules in settings.SourceAccess.
	sourceACLs *sourceACLStore

	// lmMu guards embedLM/chatLMs/defaultChatProfile, which are swapped out
	// wholesale whenever settings are saved (see handleSettings).
	lmMu               sync.RWMutex
	embedLM            *lmClient
	chatLMs            map[string]*lmClient
	defaultChatProfile string

	// queryCache holds recent query embeddings (see embedQueryCached) so a
	// repeated question — "Neu generieren", several users asking the same
	// FAQ, the admin search tester — skips the embedding round trip.
	// Replaced wholesale by setLLM, since a new embedding client can put
	// the same text at a different vector.
	queryCache *queryEmbedCache
}

// queryEmbedCacheSize bounds the query-embedding cache; FIFO eviction. Query
// texts are short and vectors small, so this stays a few hundred KB.
const queryEmbedCacheSize = 256

// queryEmbedCache is a small, mutex-guarded FIFO cache of query embeddings.
type queryEmbedCache struct {
	mu    sync.Mutex
	order []string
	vecs  map[string][]float64
}

func newQueryEmbedCache() *queryEmbedCache {
	return &queryEmbedCache{vecs: make(map[string][]float64, queryEmbedCacheSize)}
}

func (c *queryEmbedCache) get(key string) ([]float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	vec, ok := c.vecs[key]
	return vec, ok
}

func (c *queryEmbedCache) put(key string, vec []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.vecs[key]; exists {
		return
	}
	if len(c.order) >= queryEmbedCacheSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.vecs, oldest)
	}
	c.order = append(c.order, key)
	c.vecs[key] = vec
}

// newRAG wires a ragSystem around an already-open vectorStore and the LLM
// clients it needs at startup; use setLLM later to swap clients without
// rebuilding the whole system.
func newRAG(embedLM *lmClient, chatLMs map[string]*lmClient, defaultChatProfile string, store vectorStore) *ragSystem {
	return &ragSystem{
		store:              store,
		embedLM:            embedLM,
		chatLMs:            chatLMs,
		defaultChatProfile: defaultChatProfile,
		queryCache:         newQueryEmbedCache(),
		sourceACLs:         &sourceACLStore{entries: map[string]sourceACL{}},
	}
}

// setLLM atomically swaps the embedding client and the full set of chat
// backends, e.g. after settings.json is updated with new profile details.
// The query-embedding cache is dropped along with the old clients — a new
// embedding endpoint/model can place the same text at a different vector.
func (r *ragSystem) setLLM(embedLM *lmClient, chatLMs map[string]*lmClient, defaultChatProfile string) {
	r.lmMu.Lock()
	defer r.lmMu.Unlock()
	r.embedLM = embedLM
	r.chatLMs = chatLMs
	r.defaultChatProfile = defaultChatProfile
	r.queryCache = newQueryEmbedCache()
}

// embedQueryCached embeds a search query, serving repeats from queryCache.
// Keyed by embed model AND query text; nil-cache-safe for tests that build a
// bare &ragSystem{}. Callers must treat the returned vector as read-only —
// cache hits share one slice.
func (r *ragSystem) embedQueryCached(query, embedModel string) ([]float64, error) {
	key := embedModel + "\x00" + query
	if r.queryCache != nil {
		if vec, ok := r.queryCache.get(key); ok {
			return vec, nil
		}
	}
	vec, err := r.getEmbedLM().embedSingle(query)
	if err != nil {
		return nil, err
	}
	if r.queryCache != nil {
		r.queryCache.put(key, vec)
	}
	return vec, nil
}

// getEmbedLM returns the client used for all embedding calls.
func (r *ragSystem) getEmbedLM() *lmClient {
	r.lmMu.RLock()
	defer r.lmMu.RUnlock()
	return r.embedLM
}

// getChatLM returns the chat client for the requested profile ("local" or
// "azure"), falling back to the configured default profile when profile is
// empty or unknown (e.g. "azure" requested but never configured).
func (r *ragSystem) getChatLM(profile string) *lmClient {
	r.lmMu.RLock()
	defer r.lmMu.RUnlock()
	profile = strings.ToLower(strings.TrimSpace(profile))
	if c, ok := r.chatLMs[profile]; ok {
		return c
	}
	return r.chatLMs[r.defaultChatProfile]
}

// init prepares the underlying vectorStore for use (schema/tables etc.) —
// call once at startup before any query or save.
func (r *ragSystem) init() error {
	return r.store.init()
}

// save flushes the store to durable storage — a no-op for backends that
// write through immediately, but required for tinySQL's "hybrid" mode
// (see docs/VECTOR_DB.md) after a batch of ingested chunks.
func (r *ragSystem) save() error {
	return r.store.save()
}

// lastContentHash returns the content hash stored for sourceID's most
// recent import, letting a re-ingest skip re-embedding when the source
// hasn't actually changed.
func (r *ragSystem) lastContentHash(sourceID string) string {
	return r.store.lastContentHash(sourceID)
}

// deleteSource removes every chunk previously stored for sourceID. Used
// both directly (the "delete this source" UI action) and as the first step
// of replaceSourceChunks below.
func (r *ragSystem) deleteSource(sourceID string) error {
	if err := r.store.deleteSource(sourceID); err != nil {
		return err
	}
	if r.sourceACLs != nil {
		if err := r.sourceACLs.delete(sourceID); err != nil {
			return fmt.Errorf("delete source ACL for %s: %w", sourceID, err)
		}
	}
	return nil
}

// deleteSourcesByKind removes every source whose SourceKind matches kind
// exactly (e.g. every "pst_email", regardless of which PST file it came
// from), returning how many sources were deleted. Composed from
// listSources+deleteSource rather than a new vectorStore method — no
// backend needs to know about this grouping, it's just repeated single
// deletes from the caller's point of view.
func (r *ragSystem) deleteSourcesByKind(kind string) (int, error) {
	sources, err := r.listSources()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, src := range sources {
		if src.SourceKind != kind {
			continue
		}
		if err := r.deleteSource(src.SourceID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// deleteSourcesByPrefix removes every source whose SourceID starts with
// prefix — e.g. "pst:<file>:" deletes every message imported from one
// specific PST file without touching sources from any other import, since
// every importer already encodes its origin as a SourceID prefix (see
// pst.go/ingest.go).
func (r *ragSystem) deleteSourcesByPrefix(prefix string) (int, error) {
	sources, err := r.listSources()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, src := range sources {
		if !strings.HasPrefix(src.SourceID, prefix) {
			continue
		}
		if err := r.deleteSource(src.SourceID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// sourceFilter narrows which sources deleteSourcesByFilter considers a
// match — every non-empty field is ANDed together (e.g. Kind="file" +
// Extension=".pdf" deletes only PDF file-imports, not PDFs that arrived
// as a PST attachment). Callers (handlers.go) must reject an
// all-empty filter before calling deleteSourcesByFilter, since matches
// below would otherwise treat it as "match everything".
type sourceFilter struct {
	Kind      string // exact match against SourceKind, "" = any
	Extension string // case-insensitive suffix match against SourceName, already normalized to start with "." by the caller, "" = any
	Query     string // case-insensitive substring match against SourceName or SourceID, already lowercased by the caller, "" = any
}

// matches reports whether src satisfies every non-empty criterion in f.
func (f sourceFilter) matches(src sourceInfo) bool {
	if f.Kind != "" && src.SourceKind != f.Kind {
		return false
	}
	if f.Extension != "" && !strings.HasSuffix(strings.ToLower(src.SourceName), f.Extension) {
		return false
	}
	if f.Query != "" && !strings.Contains(strings.ToLower(src.SourceName), f.Query) && !strings.Contains(strings.ToLower(src.SourceID), f.Query) {
		return false
	}
	return true
}

// countSourcesByFilter reports how many sources currently match f without
// deleting anything — the dry-run half of deleteSourcesByFilter below, so
// the UI's confirmation dialog can show a server-accurate number instead
// of trusting its own possibly stale client-side row count (a parallel
// import or second admin may have changed the store since the table was
// rendered).
func (r *ragSystem) countSourcesByFilter(f sourceFilter) (int, error) {
	sources, err := r.listSources()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, src := range sources {
		if f.matches(src) {
			n++
		}
	}
	return n, nil
}

// deleteSourcesByFilter removes every source matching f, returning how
// many were deleted — the granular counterpart to
// deleteSourcesByKind/deleteSourcesByPrefix above, for "delete every PDF"
// or "delete everything matching this customer name" without deleting an
// entire source_kind or PST-file prefix. Composed the same way:
// listSources+deleteSource, no dedicated store query.
func (r *ragSystem) deleteSourcesByFilter(f sourceFilter) (int, error) {
	sources, err := r.listSources()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, src := range sources {
		if !f.matches(src) {
			continue
		}
		if err := r.deleteSource(src.SourceID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// listSources returns every source currently in the store, for the UI's
// sources table and for deleteSourcesByKind's bulk-delete grouping.
func (r *ragSystem) listSources() ([]sourceInfo, error) {
	return r.store.listSources()
}

// docCount returns the total number of distinct sources stored, used for
// the admin dashboard's at-a-glance stats.
func (r *ragSystem) docCount() int {
	return r.store.docCount()
}

// sectionMarkerRe matches the "[Abschnitt: …]" breadcrumb line chunkText
// (chunk.go's headingContextLine) prefixes to chunks inside a markdown
// section. Useful signal for embedding/BM25/LLM context, but noise when a
// source's chunks are joined back into one readable text — so
// fetchSourceContent strips it per chunk below.
var sectionMarkerRe = regexp.MustCompile(`^\[Abschnitt: [^\n]*\]\n?`)

// stripSectionMarker removes a chunk's leading breadcrumb line, if any.
func stripSectionMarker(content string) string {
	return sectionMarkerRe.ReplaceAllString(content, "")
}

// fetchSourceContent concatenates every chunk of sourceID (in original
// chunk_idx order) into one string — used by the citation popup (see
// handleSourceContent) to show a source's full text, PST email or
// uploaded document alike, regardless of how many chunks it was split
// into. Per-chunk "[Abschnitt: …]" breadcrumb lines are stripped so the
// reassembled text reads like the original document rather than repeating
// the section marker at every former chunk boundary. Returns ok=false if
// sourceID has no stored chunks at all.
func (r *ragSystem) fetchSourceContent(sourceID string) (string, bool) {
	chunks, err := r.store.fetchSourceChunks(sourceID)
	if err != nil || len(chunks) == 0 {
		return "", false
	}
	parts := make([]string, len(chunks))
	for i, c := range chunks {
		parts[i] = stripSectionMarker(c.Content)
	}
	return strings.Join(parts, "\n"), true
}

// fetchAttachmentSourceContents returns the stored text of every source
// ingested as an attachment of parentSourceID (see ingestEmailAttachment's
// "<parent>:attachment:<idx>:<filename>" source_id scheme). Used by
// handleDraftReply so a reply drafted from an already-imported email
// deterministically sees its own attachments' content, rather than relying
// on rankedSearch happening to surface them via semantic similarity.
func (r *ragSystem) fetchAttachmentSourceContents(parentSourceID string) []string {
	return r.fetchAttachmentSourceContentsForIdentity(parentSourceID, nil, adminDeptCode, "")
}

// fetchAttachmentSourceContentsForIdentity is the access-controlled variant
// used by mail drafting. A readable parent email must not make a separately
// restricted attachment leak into the draft's context.
func (r *ragSystem) fetchAttachmentSourceContentsForIdentity(parentSourceID string, access map[string][]string, deptCode, user string) []string {
	sources, err := r.listSources()
	if err != nil {
		return nil
	}
	prefix := parentSourceID + ":attachment:"
	var contents []string
	for _, src := range sources {
		if !strings.HasPrefix(src.SourceID, prefix) {
			continue
		}
		if !r.sourceAccessAllowed(access, src.SourceID, src.SourceKind, deptCode, user) {
			continue
		}
		if content, ok := r.fetchSourceContent(src.SourceID); ok {
			contents = append(contents, content)
		}
	}
	return contents
}

// fetchSourceKind returns sourceID's source_kind (e.g. "pst_email",
// "freshservice_ticket"), for callers that must check source_access before
// disclosing a source's content — see handleDraftReply. ok is false if
// sourceID has no stored chunks.
func (r *ragSystem) fetchSourceKind(sourceID string) (string, bool) {
	chunks, err := r.store.fetchSourceChunks(sourceID)
	if err != nil || len(chunks) == 0 {
		return "", false
	}
	return chunks[0].SourceKind, true
}

// sourceAccessAllowed applies both layers of read authorization.  The
// source-kind rule is deliberately evaluated first so a document ACL never
// widens an existing department restriction.
func (r *ragSystem) sourceAccessAllowed(access map[string][]string, sourceID, sourceKind, deptCode, user string) bool {
	if !sourceAccessAllowed(access, sourceKind, deptCode) {
		return false
	}
	return r.sourceACLs == nil || r.sourceACLs.allowed(sourceID, deptCode, user)
}

// listChunks delegates to the store's structured-filter query, backing the
// chunk viewer (see chunks.go's handleChunks) — free-text search, sorting
// and pagination all happen on the result set there, not here.
func (r *ragSystem) listChunks(filter chunkFilter) ([]chunkRow, bool, error) {
	return r.store.listChunks(filter)
}

// sourceChunks bundles everything an importer needs to hand a fully-chunked
// document to the store.
type sourceChunks struct {
	SourceID   string
	SourceKind string
	SourceName string
	DocDate    int64 // unix seconds, 0 if unknown
	Chunks     []string
}

// sourceInfo is the row shape returned to the UI for the sources table and
// citation chips. SourceURL is derived at query time from settings.URLMappings
// (never stored in the DB) and may be empty when no mapping matches.
type sourceInfo struct {
	SourceID   string `json:"source_id"`
	SourceKind string `json:"source_kind"`
	SourceName string `json:"source_name"`
	LoadID     string `json:"load_id"`
	LoadedAt   int64  `json:"loaded_at"`
	DocDate    int64  `json:"doc_date"`
	Chunks     int    `json:"chunks"`
	SourceURL  string `json:"source_url,omitempty"`
	// Marker is the 1-based "[Qn]" citation number this source was given
	// in the context assembled for the LLM (see assembleContext in
	// rank.go) — only set on citations returned from /api/ask, zero
	// (omitted) everywhere else sourceInfo is used (sources table, chunk
	// viewer, ...). The frontend resolves inline [Qn] markers against this
	// field rather than the citations array's position, since
	// filterCitations (rank.go) may drop entries — an unused or
	// visibility-hidden source — without renumbering the rest.
	Marker int `json:"marker,omitempty"`
}

// resolveSourceURL derives a web URL for a source_id by checking each
// configured urlMapping in order, returning the first match. Returns ""
// when no mapping applies (uploaded files, unconfigured prefixes, etc.).
func resolveSourceURL(sourceID string, mappings []urlMapping) string {
	for _, m := range mappings {
		if m.Prefix == "" || m.URLPrefix == "" {
			continue
		}
		if strings.HasPrefix(sourceID, m.Prefix) {
			rel := strings.TrimPrefix(sourceID, m.Prefix)
			rel = strings.ReplaceAll(rel, "\\", "/")
			base := strings.TrimRight(m.URLPrefix, "/")
			return base + "/" + rel
		}
	}
	return ""
}

// sourceEmbeddingFetcher is an optional vectorStore capability — implemented
// by both backends (vectorstore_sqlite.go, vectorstore_tinysql.go) —
// returning one source's stored embeddings keyed by each chunk content's
// hash, so replaceSourceChunks below can reuse them for chunks whose text
// didn't change. A backend without it just re-embeds everything, same
// graceful-degrade shape as rank.go's ftsKeywordScorer.
type sourceEmbeddingFetcher interface {
	fetchSourceEmbeddings(sourceID, embedModel string) (map[string][]float64, error)
}

// replaceSourceChunks embeds the given chunks in batches (an LLM concern, so
// it happens here rather than inside vectorStore), then replaces any existing
// chunks for sc.SourceID with the freshly embedded set under a new
// load_id/loaded_at. It is the single write path used by every importer
// (generic files, PST emails, IMAP, SharePoint, ...) so provenance columns
// are always filled in consistently and an update never leaves stale content
// searchable alongside the new version.
//
// Two deliberate properties of this ordering:
//   - Embedding happens BEFORE the old chunks are deleted. A failing
//     embedding call (endpoint down, model unloaded) therefore leaves the
//     source's previous content fully intact and searchable — the old code
//     deleted first and could permanently lose a source to a transient
//     embedding outage.
//   - Chunks whose exact text was already stored for this source reuse their
//     stored embedding (sourceEmbeddingFetcher above) instead of re-paying
//     the embedding call. A re-imported document with one edited paragraph
//     only embeds the chunks around that edit; everything else is carried
//     over. (The whole-source hash-skip in ingestDocument still short-
//     circuits the fully-unchanged case before this function is reached.)
func (r *ragSystem) replaceSourceChunks(sc sourceChunks, embedModel string) (int, error) {
	if len(sc.Chunks) == 0 {
		if err := r.deleteSource(sc.SourceID); err != nil {
			return 0, fmt.Errorf("delete old chunks for %s: %w", sc.SourceID, err)
		}
		return 0, nil
	}

	var reuse map[string][]float64
	if fetcher, ok := r.store.(sourceEmbeddingFetcher); ok {
		var err error
		if reuse, err = fetcher.fetchSourceEmbeddings(sc.SourceID, embedModel); err != nil {
			reuse = nil // best-effort: fall back to embedding everything
		}
	}

	vecs := make([][]float64, len(sc.Chunks))
	var missingIdx []int
	var missing []string
	for i, chunk := range sc.Chunks {
		if v, ok := reuse[contentHash(chunk)]; ok && len(v) > 0 {
			vecs[i] = v
			continue
		}
		missingIdx = append(missingIdx, i)
		missing = append(missing, chunk)
	}

	embedBatch := importEmbedBatchSize(settings.get().Import)
	for i := 0; i < len(missing); i += embedBatch {
		end := i + embedBatch
		if end > len(missing) {
			end = len(missing)
		}
		batchVecs, err := r.getEmbedLM().embed(missing[i:end])
		if err != nil {
			return 0, fmt.Errorf("embed batch %d: %w", i/embedBatch, err)
		}
		if len(batchVecs) != end-i {
			return 0, fmt.Errorf("embed batch %d: got %d vectors for %d inputs", i/embedBatch, len(batchVecs), end-i)
		}
		for j, v := range batchVecs {
			vecs[missingIdx[i+j]] = v
		}
	}

	hash := contentHash(strings.Join(sc.Chunks, "\n"))
	loadID := newRequestID()
	loadedAt := time.Now().Unix()

	// A re-import replaces vector rows but deliberately keeps a document ACL:
	// access rules belong to the stable source_id, not to one particular load.
	if err := r.store.deleteSource(sc.SourceID); err != nil {
		return 0, fmt.Errorf("delete old chunks for %s: %w", sc.SourceID, err)
	}
	inserted, err := r.store.insertChunks(sc, embedModel, vecs, loadID, loadedAt, hash)
	if err != nil {
		return inserted, err
	}
	if err := r.save(); err != nil {
		log.Printf("WARN: save failed: %v", err)
	}
	return inserted, nil
}
