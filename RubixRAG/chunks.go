package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Chunk viewer: GET /api/chunks
//
// A debug/browse view over exactly what's stored and searchable — every
// chunk with its full provenance (source, load, timestamps) and a computed
// freshness score, with free-text search, structured filters, sorting and
// pagination. Structured filters (source_kind, embed_model) are pushed to
// SQL (vectorstore_tinysql.go's listChunks, exact-match only); free-text
// search, source_id substring matching, sorting and pagination all happen
// here in Go over that result set — see chunkListCap for why that's an
// acceptable tradeoff for a debug tool rather than a paginated production
// listing.
// ─────────────────────────────────────────────────────────────────────────────

// chunkViewRow is one chunk as rendered by the viewer: the raw stored
// fields plus derived display fields (Freshness, Chars).
type chunkViewRow struct {
	ID         int     `json:"id"`
	SourceID   string  `json:"source_id"`
	SourceKind string  `json:"source_kind"`
	SourceName string  `json:"source_name"`
	LoadID     string  `json:"load_id"`
	LoadedAt   int64   `json:"loaded_at"`
	DocDate    int64   `json:"doc_date"`
	ChunkIdx   int     `json:"chunk_idx"`
	Content    string  `json:"content"`
	EmbedModel string  `json:"embed_model"`
	Freshness  float64 `json:"freshness"` // same recencyScore() formula rankedSearch uses, so this matches why a chunk would actually rank the way it does
	Chars      int     `json:"chars"`
}

type chunksResponse struct {
	Chunks []chunkViewRow `json:"chunks"`
	Total  int            `json:"total"`  // after all filters; a floor, not exact, if Capped
	Capped bool           `json:"capped"` // true if the underlying structured-filtered set exceeded the safety cap
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// handleChunks serves GET /api/chunks: pushes structured filters to the
// store, then applies free-text search, sorting and pagination in Go over
// that result set before returning the page the viewer asked for.
func handleChunks(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		filter := chunkFilter{
			SourceKind: strings.TrimSpace(q.Get("source_kind")),
			EmbedModel: strings.TrimSpace(q.Get("embed_model")),
		}
		rows, capped, err := rag.listChunks(filter)
		if err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}

		search := strings.ToLower(strings.TrimSpace(q.Get("q")))
		sourceIDSub := strings.ToLower(strings.TrimSpace(q.Get("source_id")))
		halfLife := settings.get().Ranking.RecencyHalfLifeDays

		out := make([]chunkViewRow, 0, len(rows))
		for _, row := range rows {
			if sourceIDSub != "" && !strings.Contains(strings.ToLower(row.SourceID), sourceIDSub) {
				continue
			}
			if search != "" &&
				!strings.Contains(strings.ToLower(row.Content), search) &&
				!strings.Contains(strings.ToLower(row.SourceName), search) {
				continue
			}
			out = append(out, chunkViewRow{
				ID: row.ID, SourceID: row.SourceID, SourceKind: row.SourceKind, SourceName: row.SourceName,
				LoadID: row.LoadID, LoadedAt: row.LoadedAt, DocDate: row.DocDate, ChunkIdx: row.ChunkIdx,
				Content: row.Content, EmbedModel: row.EmbedModel,
				Freshness: recencyScore(row.DocDate, row.LoadedAt, halfLife),
				Chars:     len(row.Content),
			})
		}

		sortChunkRows(out, q.Get("sort"), q.Get("order") != "asc")

		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 200 {
			limit = 50
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		if offset < 0 {
			offset = 0
		}

		total := len(out)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}

		writeJSON(w, chunksResponse{
			Chunks: out[offset:end],
			Total:  total,
			Capped: capped,
			Limit:  limit,
			Offset: offset,
		})
	}
}

// sortChunkRows sorts rows in place. Plain insertion sort is fine at the
// chunk-viewer's scale (<=chunkListCap rows, further narrowed by whatever
// filters were applied) and keeps this file free of sort.Slice's reflection
// overhead and comparator-negation pitfalls for what's typically a few
// hundred to a few thousand rows.
func sortChunkRows(rows []chunkViewRow, sortBy string, desc bool) {
	cmp := func(a, b chunkViewRow) int {
		switch sortBy {
		case "doc_date":
			return cmpInt64(a.DocDate, b.DocDate)
		case "freshness":
			return cmpFloat64(a.Freshness, b.Freshness)
		case "chunk_idx":
			return cmpInt(a.ChunkIdx, b.ChunkIdx)
		case "source_name":
			return strings.Compare(strings.ToLower(a.SourceName), strings.ToLower(b.SourceName))
		case "chars":
			return cmpInt(a.Chars, b.Chars)
		default: // "loaded_at"
			return cmpInt64(a.LoadedAt, b.LoadedAt)
		}
	}
	less := func(i, j int) bool {
		c := cmp(rows[i], rows[j])
		if desc {
			return c > 0
		}
		return c < 0
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

// cmpInt64 is sortChunkRows' three-way comparator for int64 fields.
func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cmpInt is sortChunkRows' three-way comparator for int fields.
func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cmpFloat64 is sortChunkRows' three-way comparator for float64 fields
// (currently just Freshness).
func cmpFloat64(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// chunkSearchTestRequest is what the Chunks tab's "Testsuche" sends —
// a query and an optional k (0 = the deployment's own configured default,
// same as askRequest.K).
type chunkSearchTestRequest struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

type chunkSearchTestResponse struct {
	Hits []rankedHit `json:"hits"`
}

// handleChunksSearchTest lets an admin run the SAME hybrid retrieval
// rankedSearch performs for a real question (rank.go) directly from the
// Chunks tab, unrestricted (adminDeptCode, no SourceAccess/preset
// narrowing — same "admin sees everything" posture as GET /api/chunks
// itself). This is the tab's answer to "why doesn't source X show up when
// I search for Y" or "is the keyword/vector/recency weighting tuned
// right" — without it, the only way to see actual retrieval scores was to
// ask a real question in Chat as an admin and read Debug-Modus, which
// buries the same rankedHit data behind an unrelated LLM call and answer.
func handleChunksSearchTest(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req chunkSearchTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "invalid body: "+err.Error(), 400)
			return
		}
		if strings.TrimSpace(req.Query) == "" {
			writeJSONError(w, "missing query", 400)
			return
		}
		s := settings.get()
		k := req.K
		if k <= 0 {
			k = s.K
		}
		hits, err := rag.rankedSearch(req.Query, k, s.Ranking, s.activeEmbedModel(), nil, adminDeptCode, nil)
		if err != nil {
			writeJSONError(w, "search failed: "+err.Error(), 500)
			return
		}
		writeJSON(w, chunkSearchTestResponse{Hits: hits})
	}
}
