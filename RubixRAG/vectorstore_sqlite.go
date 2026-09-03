package main

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// sqliteStore: SQLite-backed vectorStore via modernc.org/sqlite (pure Go,
// no CGO) — the second vectorStore implementation, added exactly the way
// the interface in vectorstore.go was designed for (see its package
// comment): a new file plus one case in newVectorStore, nothing else
// touched.
//
// Similarity search is brute-force cosine computed in Go over every row
// matching embed_model — the same technique tinySQL's own
// VEC_COSINE_SIMILARITY already uses (see docs/VECTOR_DB.md). The point of
// this backend isn't ANN query speed (R3's chunk volume doesn't need that
// yet per VECTOR_DB.md) — it's SQLite's much larger track record,
// WAL-mode concurrency and standard backup tooling. A dedicated vector
// extension (sqlite-vec) was deliberately skipped: the C version needs
// CGO, and the pure-Go reimplementation (viant/sqlite-vec) is a young,
// small-adoption project whose virtual-table/MATCH API buys real
// complexity without buying anything R3 needs at its current scale.
//
// Every value here goes through database/sql's parameterized queries (?
// placeholders), never string-interpolated into SQL text — this sidesteps
// the class of bug tinySQL's own text-based lexer has (see
// vectorstore_tinysql.go's sqlStr doc comment) by construction rather than
// by working around it after the fact.
// ─────────────────────────────────────────────────────────────────────────────

type sqliteStore struct {
	db *sql.DB
}

// ─────────────────────────────────────────────────────────────────────────────
// Embedding encoding
//
// New rows store embeddings as a compact binary blob — one magic byte
// (embeddingBlobMagic, so the format is self-describing) followed by the
// vector as little-endian float32s. Compared to the original float64-JSON
// text this is roughly 5x smaller on disk and decodes without a JSON parse,
// which matters because vectorCandidates decodes every stored row on every
// search. float32 loses ~1e-7 per component — far below what could ever
// reorder a cosine ranking.
//
// decodeEmbedding still reads the legacy JSON form (which always starts with
// '[', a byte the magic prefix deliberately avoids), so databases written
// before this encoding keep working without any migration; their rows simply
// upgrade to the binary form whenever their source is next re-ingested.
// ─────────────────────────────────────────────────────────────────────────────

const embeddingBlobMagic = 0x01

// encodeEmbedding serializes vec into the binary blob format above.
func encodeEmbedding(vec []float64) []byte {
	buf := make([]byte, 1+4*len(vec))
	buf[0] = embeddingBlobMagic
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[1+4*i:], math.Float32bits(float32(v)))
	}
	return buf
}

// decodeEmbedding parses either encoding — binary blob (new) or float64
// JSON array (legacy) — back into a []float64.
func decodeEmbedding(raw []byte) ([]float64, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	if raw[0] == embeddingBlobMagic {
		if (len(raw)-1)%4 != 0 {
			return nil, fmt.Errorf("binary embedding has invalid length %d", len(raw))
		}
		vec := make([]float64, (len(raw)-1)/4)
		for i := range vec {
			vec[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[1+4*i:])))
		}
		return vec, nil
	}
	var vec []float64
	if err := json.Unmarshal(raw, &vec); err != nil {
		return nil, err
	}
	return vec, nil
}

// newSQLiteStore opens (creating if needed) the SQLite file at cfg.Path,
// defaulting to r3-data.db, with WAL mode and a busy timeout so concurrent
// readers don't immediately fail against the single writer.
func newSQLiteStore(cfg storageSettings) (*sqliteStore, error) {
	path := cfg.Path
	if path == "" {
		path = "r3-data.db"
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite (like SQLite itself) only truly supports one
	// writer at a time; keeping the pool to a single connection avoids
	// SQLITE_BUSY races between concurrent requests instead of relying
	// solely on busy_timeout, matching tinySQL's own single-writer model.
	db.SetMaxOpenConns(1)
	fmt.Printf("Storage backend: sqlite (%s)\n", path)
	return &sqliteStore{db: db}, nil
}

// init creates the chunks table and its source_id/embed_model indexes if
// they don't already exist, so it's safe to call on every startup.
//
// chunks_fts is a standalone (not "external content") FTS5 virtual table
// duplicating each chunk's content alongside its source_id/chunk_idx/
// embed_model — deliberately NOT an external-content table referencing
// chunks.id, which would need triggers (or manual INSERT/DELETE INTO
// chunks_fts(chunks_fts, rowid, ...) "special commands") to stay in sync.
// A plain duplicate table is kept in sync with one extra parameterized
// INSERT/DELETE alongside every write to chunks (insertChunks/deleteSource/
// importRaw below) — simpler to reason about, at the cost of storing chunk
// text twice; chunk sizes here (settings.go's ChunkSize, default 800
// runes) make that an acceptable trade. This gives the SQLite backend the
// same real BM25 (term frequency + document-length normalization,
// stemming via FTS5's unicode61 tokenizer) that tinySQL's FTS_SEARCH
// already provides, instead of always falling back to rank.go's cruder
// keywordOverlapScore term-presence fraction — see keywordCandidatesFTS
// below.
func (s *sqliteStore) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS chunks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id TEXT NOT NULL,
			source_kind TEXT NOT NULL,
			source_name TEXT NOT NULL,
			load_id TEXT NOT NULL,
			loaded_at INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			doc_date INTEGER NOT NULL,
			chunk_idx INTEGER NOT NULL,
			content TEXT NOT NULL,
			embedding TEXT NOT NULL,
			embed_model TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_source_id ON chunks(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_embed_model ON chunks(embed_model)`,
		// Composite index so fetchSourceChunks' ORDER BY chunk_idx and
		// chunkByKey's exact (source_id, chunk_idx) lookups resolve without
		// a sort/scan step. idx_chunks_source_id above is technically
		// subsumed by this prefix but stays for databases created before it
		// existed (CREATE INDEX IF NOT EXISTS never drops anything).
		`CREATE INDEX IF NOT EXISTS idx_chunks_source_chunk ON chunks(source_id, chunk_idx)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
			content,
			source_id UNINDEXED,
			chunk_idx UNINDEXED,
			embed_model UNINDEXED
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	return nil
}

// lastContentHash looks up the content_hash of any one stored chunk for
// sourceID (all chunks from the same load share the same hash), returning
// "" if the source has no chunks or the query fails.
func (s *sqliteStore) lastContentHash(sourceID string) string {
	var hash string
	err := s.db.QueryRow(`SELECT content_hash FROM chunks WHERE source_id = ? LIMIT 1`, sourceID).Scan(&hash)
	if err != nil {
		return ""
	}
	return hash
}

// deleteSource removes every chunk row for sourceID (and its chunks_fts
// duplicate — see init's doc comment) in a single transaction, e.g. before
// re-ingesting it under a new load.
func (s *sqliteStore) deleteSource(sourceID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds
	if _, err := tx.Exec(`DELETE FROM chunks WHERE source_id = ?`, sourceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE source_id = ?`, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// insertChunks writes sc.Chunks and their vectors inside a single
// transaction via a prepared, parameterized INSERT (embeddings stored as
// JSON text), so a failure partway through rolls back the whole load
// instead of leaving a half-inserted source behind.
func (s *sqliteStore) insertChunks(sc sourceChunks, embedModel string, vectors [][]float64, loadID string, loadedAt int64, hash string) (int, error) {
	if len(sc.Chunks) != len(vectors) {
		return 0, fmt.Errorf("chunk/vector count mismatch: %d chunks, %d vectors", len(sc.Chunks), len(vectors))
	}
	if len(sc.Chunks) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	stmt, err := tx.Prepare(`INSERT INTO chunks
		(source_id, source_kind, source_name, load_id, loaded_at, content_hash, doc_date, chunk_idx, content, embedding, embed_model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	// See init's doc comment for why chunks_fts is a plain duplicate table
	// kept in sync here rather than an FTS5 external-content table.
	ftsStmt, err := tx.Prepare(`INSERT INTO chunks_fts (content, source_id, chunk_idx, embed_model) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer ftsStmt.Close()

	for j, chunk := range sc.Chunks {
		if _, err := stmt.Exec(sc.SourceID, sc.SourceKind, sc.SourceName, loadID, loadedAt, hash, sc.DocDate, j, chunk, encodeEmbedding(vectors[j]), embedModel); err != nil {
			return j, fmt.Errorf("insert chunk %d: %w", j, err)
		}
		if _, err := ftsStmt.Exec(chunk, sc.SourceID, j, embedModel); err != nil {
			return j, fmt.Errorf("insert fts chunk %d: %w", j, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(sc.Chunks), nil
}

// cosineSimilarity computes cosine similarity between a and b. A length
// mismatch scores 0 rather than silently comparing a truncated prefix — the
// only way dimensions differ in practice is rows embedded by a different
// model generation than the query, and a prefix-cosine over those is
// meaningless noise that could still outrank legitimate matches. Zero-norm
// vectors also score 0 instead of dividing by zero.
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// vectorCandidates loads every chunk for embedModel, scores each against
// queryVec with cosineSimilarity in Go (SQLite has no native vector
// support), then sorts and truncates to limit — a full scan rather than an
// indexed search, fine at R3's current chunk volume (see docs/VECTOR_DB.md).
// A row whose stored embedding fails to unmarshal is silently skipped
// rather than failing the whole search.
func (s *sqliteStore) vectorCandidates(queryVec []float64, embedModel string, limit int) ([]rankedHit, error) {
	rows, err := s.db.Query(`SELECT source_id, source_kind, source_name, load_id, loaded_at, doc_date, chunk_idx, content, embedding
		FROM chunks WHERE embed_model = ?`, embedModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []rankedHit
	for rows.Next() {
		var h rankedHit
		var emb []byte
		if err := rows.Scan(&h.SourceID, &h.SourceKind, &h.SourceName, &h.LoadID, &h.LoadedAt, &h.DocDate, &h.ChunkIdx, &h.Content, &emb); err != nil {
			return nil, err
		}
		vec, err := decodeEmbedding(emb)
		if err != nil {
			continue // skip rows with corrupt/unreadable embeddings rather than fail the whole search
		}
		h.VectorScore = cosineSimilarity(queryVec, vec)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].VectorScore > hits[j].VectorScore })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// keywordCandidatesFTS implements rank.go's optional ftsKeywordScorer
// capability via SQLite's FTS5 bm25() ranking function against the
// chunks_fts duplicate table (see init's doc comment) — giving this
// backend the same real BM25 scoring tinySQL's FTS_SEARCH already
// provides instead of always falling back to rank.go's cruder
// keywordOverlapScore. FTS5's bm25() returns SMALLER (more negative)
// values for BETTER matches; negated here so the result matches every
// other score in this codebase's "higher is better" convention (rank.go's
// ftsKeywordScores normalizes by dividing by the batch max, which assumes
// that direction). The query text is tokenized the same way rank.go's own
// tokenize() does (alphanumeric + German umlauts, >=2 chars) and joined as
// an OR of individually double-quoted terms — a plain recall-oriented
// match, not a pass-through of the caller's raw text as FTS5 MATCH syntax
// (which has its own operators/column-filter/quoting rules a user's
// question could otherwise trip over). embed_model is filtered directly
// in SQL since chunks_fts stores it as a regular (if UNINDEXED) column —
// unlike tinySQL's FTS_SEARCH, which has no WHERE-pushdown at all and
// filters embed_model back in Go.
func (s *sqliteStore) keywordCandidatesFTS(query, embedModel string, limit int) (map[string]float64, error) {
	terms := tokenize(query)
	if len(terms) == 0 {
		return map[string]float64{}, nil
	}
	parts := make([]string, 0, len(terms))
	for t := range terms {
		parts = append(parts, `"`+t+`"`)
	}
	matchQuery := strings.Join(parts, " OR ")

	rows, err := s.db.Query(
		`SELECT source_id, chunk_idx, -bm25(chunks_fts) AS score
		 FROM chunks_fts
		 WHERE chunks_fts MATCH ? AND embed_model = ?
		 ORDER BY bm25(chunks_fts)
		 LIMIT ?`,
		matchQuery, embedModel, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fts5 match: %w", err)
	}
	defer rows.Close()

	scores := make(map[string]float64)
	for rows.Next() {
		var sourceID string
		var chunkIdx int
		var score float64
		if err := rows.Scan(&sourceID, &chunkIdx, &score); err != nil {
			return nil, err
		}
		scores[ftsCandidateKey(sourceID, chunkIdx)] = score
	}
	return scores, rows.Err()
}

// chunkByKey implements rank.go's optional chunkKeyFetcher capability: load
// one specific chunk (with its decoded embedding) by its exact
// (source_id, chunk_idx, embed_model) key, so rankedSearch can pull
// FTS-found candidates that the vector search missed into the scored set.
// ok=false when the row doesn't exist or its embedding is unreadable.
func (s *sqliteStore) chunkByKey(sourceID string, chunkIdx int, embedModel string) (rankedHit, []float64, bool) {
	var h rankedHit
	var emb []byte
	err := s.db.QueryRow(`SELECT source_id, source_kind, source_name, load_id, loaded_at, doc_date, chunk_idx, content, embedding
		FROM chunks WHERE source_id = ? AND chunk_idx = ? AND embed_model = ? LIMIT 1`,
		sourceID, chunkIdx, embedModel).
		Scan(&h.SourceID, &h.SourceKind, &h.SourceName, &h.LoadID, &h.LoadedAt, &h.DocDate, &h.ChunkIdx, &h.Content, &emb)
	if err != nil {
		return rankedHit{}, nil, false
	}
	vec, err := decodeEmbedding(emb)
	if err != nil {
		return rankedHit{}, nil, false
	}
	return h, vec, true
}

// fetchSourceEmbeddings implements store.go's optional
// sourceEmbeddingFetcher capability: every stored chunk embedding of one
// source, keyed by the chunk content's hash — so replaceSourceChunks can
// reuse embeddings for chunks whose text didn't change instead of re-paying
// the embedding call for the whole source. Rows with unreadable embeddings
// are skipped (they'll simply be re-embedded).
func (s *sqliteStore) fetchSourceEmbeddings(sourceID, embedModel string) (map[string][]float64, error) {
	rows, err := s.db.Query(`SELECT content, embedding FROM chunks WHERE source_id = ? AND embed_model = ?`, sourceID, embedModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]float64)
	for rows.Next() {
		var content string
		var emb []byte
		if err := rows.Scan(&content, &emb); err != nil {
			return nil, err
		}
		vec, err := decodeEmbedding(emb)
		if err != nil {
			continue
		}
		out[contentHash(content)] = vec
	}
	return out, rows.Err()
}

// fetchSourceChunks returns every stored chunk of sourceID, ordered by
// chunk_idx, in one query — rather than one round trip per index.
func (s *sqliteStore) fetchSourceChunks(sourceID string) ([]sourceChunk, error) {
	rows, err := s.db.Query(`SELECT chunk_idx, content, source_kind FROM chunks WHERE source_id = ? ORDER BY chunk_idx`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sourceChunk
	for rows.Next() {
		var c sourceChunk
		if err := rows.Scan(&c.ChunkIdx, &c.Content, &c.SourceKind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// listSources groups chunks by their source/load identity to recover one
// summary row per distinct load, newest first.
func (s *sqliteStore) listSources() ([]sourceInfo, error) {
	rows, err := s.db.Query(`SELECT source_id, source_kind, source_name, load_id, loaded_at, doc_date, COUNT(*) AS cnt
		FROM chunks GROUP BY source_id, source_kind, source_name, load_id, loaded_at, doc_date
		ORDER BY loaded_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sourceInfo
	for rows.Next() {
		var si sourceInfo
		if err := rows.Scan(&si.SourceID, &si.SourceKind, &si.SourceName, &si.LoadID, &si.LoadedAt, &si.DocDate, &si.Chunks); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// docCount returns the total chunk row count, or 0 if the query fails.
func (s *sqliteStore) docCount() int {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// listChunks builds a dynamic WHERE clause from filter's exact-match
// fields, queries one row past chunkListCap, and reports capped=true (with
// the extra row trimmed) if that cap was reached — free-text search and
// substring filtering happen in Go over this result (see chunks.go).
func (s *sqliteStore) listChunks(filter chunkFilter) ([]chunkRow, bool, error) {
	var where []string
	var args []any
	if filter.SourceKind != "" {
		where = append(where, "source_kind = ?")
		args = append(args, filter.SourceKind)
	}
	if filter.EmbedModel != "" {
		where = append(where, "embed_model = ?")
		args = append(args, filter.EmbedModel)
	}
	q := `SELECT id, source_id, source_kind, source_name, load_id, loaded_at, doc_date, chunk_idx, content, embed_model FROM chunks`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " LIMIT ?"
	args = append(args, chunkListCap+1)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []chunkRow
	for rows.Next() {
		var r chunkRow
		if err := rows.Scan(&r.ID, &r.SourceID, &r.SourceKind, &r.SourceName, &r.LoadID, &r.LoadedAt, &r.DocDate, &r.ChunkIdx, &r.Content, &r.EmbedModel); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	capped := len(out) > chunkListCap
	if capped {
		out = out[:chunkListCap]
	}
	return out, capped, nil
}

// exportAll dumps every chunk row verbatim, decoding each stored embedding
// back from JSON, for one-shot migration to another backend (see
// migrate.go) — not used on the normal request path.
func (s *sqliteStore) exportAll() ([]exportedChunk, error) {
	rows, err := s.db.Query(`SELECT source_id, source_kind, source_name, load_id, loaded_at, content_hash, doc_date, chunk_idx, content, embedding, embed_model FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []exportedChunk
	for rows.Next() {
		var e exportedChunk
		var emb []byte
		if err := rows.Scan(&e.SourceID, &e.SourceKind, &e.SourceName, &e.LoadID, &e.LoadedAt, &e.ContentHash, &e.DocDate, &e.ChunkIdx, &e.Content, &emb, &e.EmbedModel); err != nil {
			return nil, err
		}
		vec, err := decodeEmbedding(emb)
		if err != nil {
			return nil, fmt.Errorf("decode embedding for %s#%d: %w", e.SourceID, e.ChunkIdx, err)
		}
		e.Embedding = vec
		out = append(out, e)
	}
	return out, rows.Err()
}

// importRaw inserts rows exactly as exported (embeddings re-encoded to
// JSON, original provenance and content_hash preserved) inside a single
// transaction, for one-shot migration from another backend — see exportAll.
func (s *sqliteStore) importRaw(rows []exportedChunk) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	stmt, err := tx.Prepare(`INSERT INTO chunks
		(source_id, source_kind, source_name, load_id, loaded_at, content_hash, doc_date, chunk_idx, content, embedding, embed_model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// See init's doc comment for why chunks_fts is a plain duplicate table
	// kept in sync here rather than an FTS5 external-content table.
	ftsStmt, err := tx.Prepare(`INSERT INTO chunks_fts (content, source_id, chunk_idx, embed_model) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ftsStmt.Close()

	for _, e := range rows {
		if _, err := stmt.Exec(e.SourceID, e.SourceKind, e.SourceName, e.LoadID, e.LoadedAt, e.ContentHash, e.DocDate, e.ChunkIdx, e.Content, encodeEmbedding(e.Embedding), e.EmbedModel); err != nil {
			return fmt.Errorf("insert %s#%d: %w", e.SourceID, e.ChunkIdx, err)
		}
		if _, err := ftsStmt.Exec(e.Content, e.SourceID, e.ChunkIdx, e.EmbedModel); err != nil {
			return fmt.Errorf("insert fts %s#%d: %w", e.SourceID, e.ChunkIdx, err)
		}
	}
	return tx.Commit()
}

// Close releases the underlying database connection. Not part of the
// vectorStore interface — tinySQL manages its own lifecycle without an
// explicit close, and the running server relies on process exit rather
// than a graceful shutdown path. Exposed mainly so tests (and any future
// graceful-shutdown code) can release the file handle deterministically;
// on Windows an open SQLite file handle blocks deleting/renaming it.
func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// save checkpoints the WAL back into the main database file. Not required
// for durability — SQLite's WAL mode already makes every committed
// transaction durable — but it keeps the WAL file bounded and means a
// plain copy of the .db file (VECTOR_DB.md's "backup story" for this
// backend) reflects everything written so far without needing the -wal
// sidecar file too.
func (s *sqliteStore) save() error {
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
