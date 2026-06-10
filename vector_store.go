package main

// ─────────────────────────────────────────────────────────────────────────────
// Vector Chunk Store — backend abstraction
//
// vectorChunkStore is the single interface that all vector persistence backends
// must satisfy.  The default implementation (tinySQLChunkStore) wraps tinySQL.
// A future sqlite-vec implementation can be compiled in with -tags sqlite_vec.
//
// Switching backends is controlled by the "vector_backend" field in
// settings.json ("tinysql" or "sqlite-vec").
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// ─────────────────────────────────────────────────────────────────────────────
// Interface
// ─────────────────────────────────────────────────────────────────────────────

// vectorChunkStore abstracts the persistence layer for vector chunks.
// Implementations may use tinySQL (default) or sqlite-vec (build tag sqlite_vec).
type vectorChunkStore interface {
	// init creates the chunks table and applies backward-compatible migrations.
	init() error
	// maxChunkID returns the highest stored chunk id, or -1 when the table is empty.
	maxChunkID() int
	// checkArticleExists returns true when the document has already been ingested.
	checkArticleExists(documentID, roleScope string) (bool, error)
	// insertChunks persists a batch of pre-embedded chunks.
	insertChunks(chunks []storedChunk) error
	// searchTopK executes a cosine-similarity search and returns up to limit hits.
	// Returned hits have Score = raw cosine similarity; R3Score is zero and must be
	// filled in by the caller.
	searchTopK(vec []float64, embedModel, roleFilter string, limit int) ([]retrievalHit, error)
	// fetchNeighborContent retrieves the text of a specific chunk by article+index.
	fetchNeighborContent(article string, chunkIdx int, roleFilter string) (string, bool)
	// loadArticleChunks returns all chunks for an article as a generic row map.
	// Keys: content, article, chunk_idx, chunk_id, document_id, source_system,
	// source_type, source_title, source_url, sensitivity, trust_level,
	// updated_at, open_link_allowed.
	loadArticleChunks(article, roleFilter string) ([]map[string]any, error)
	// listSources returns distinct article titles with their chunk counts.
	listSources(roleFilter string) []map[string]any
	// deleteSource removes all chunks that belong to article and match roleFilter.
	deleteSource(article, roleFilter string) error
	// countChunks returns the number of stored chunks visible to roleFilter.
	countChunks(roleFilter string) int
	// save persists the store to disk (no-op for memory-only backends).
	save() error
	// close releases all held resources.
	close() error
}

// ─────────────────────────────────────────────────────────────────────────────
// storedChunk — the data record exchanged between ragSystem and vectorChunkStore
// ─────────────────────────────────────────────────────────────────────────────

// storedChunk holds a fully-populated chunk ready for insertion.
// The caller (ragSystem) is responsible for computing the Embedding and
// assigning the ID before passing the slice to insertChunks.
type storedChunk struct {
	ID         int
	ChunkIdx   int
	Article    string
	Content    string
	Embedding  []float64
	EmbedModel string
	RoleScope  string

	// R3 identity
	ChunkID        string
	DocumentID     string
	SourceSystem   string
	SourceType     string
	SourceTitle    string
	SourceURL      string
	SourceObjectID string
	SourceVersion  string

	// Access control & ownership
	ACLGroups     string
	BusinessOwner string
	Sensitivity   string

	// Quality signals
	TrustLevel     float64
	SourceQuality  float64
	FreshnessScore float64
	QualityScore   float64
	FeedbackScore  float64

	// Timestamps & hash
	ImportedAt      string
	UpdatedAt       string
	ContentHash     string
	OpenLinkAllowed bool
}

// ─────────────────────────────────────────────────────────────────────────────
// tinySQLChunkStore — default backend
// ─────────────────────────────────────────────────────────────────────────────

// tinySQLChunkStore implements vectorChunkStore on top of a tinySQL DB.
// The DB and mutex are shared with the owning ragSystem so that chunk
// operations and R3 metadata operations remain serialized.
type tinySQLChunkStore struct {
	db   *tinysql.DB
	dbMu *sync.Mutex
	path string
	mode tinysql.StorageMode
	qc   *tinysql.QueryCache

	// pre-compiled queries
	cntAll *tinysql.CompiledQuery
	maxID  *tinysql.CompiledQuery
}

// newTinySQLChunkStore creates a tinySQLChunkStore that shares the provided DB
// and mutex with the caller.
func newTinySQLChunkStore(db *tinysql.DB, mu *sync.Mutex, path string, mode tinysql.StorageMode) *tinySQLChunkStore {
	return &tinySQLChunkStore{
		db:   db,
		dbMu: mu,
		path: path,
		mode: mode,
		qc:   tinysql.NewQueryCache(32),
	}
}

func (s *tinySQLChunkStore) init() error {
	q := "CREATE TABLE IF NOT EXISTS chunks (id INT, article TEXT, chunk_idx INT, content TEXT, embedding VECTOR, embed_model TEXT, role_scope TEXT)"
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return err
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if _, err = tinysql.Execute(context.Background(), s.db, "default", stmt); err != nil {
		return err
	}
	// Backward-compatible column additions (ignore "already exists" errors).
	migrations := []string{
		"ALTER TABLE chunks ADD COLUMN embed_model TEXT",
		"ALTER TABLE chunks ADD COLUMN role_scope TEXT",
		"ALTER TABLE chunks ADD COLUMN chunk_id TEXT",
		"ALTER TABLE chunks ADD COLUMN document_id TEXT",
		"ALTER TABLE chunks ADD COLUMN source_system TEXT",
		"ALTER TABLE chunks ADD COLUMN source_type TEXT",
		"ALTER TABLE chunks ADD COLUMN source_title TEXT",
		"ALTER TABLE chunks ADD COLUMN source_url TEXT",
		"ALTER TABLE chunks ADD COLUMN source_object_id TEXT",
		"ALTER TABLE chunks ADD COLUMN source_version TEXT",
		"ALTER TABLE chunks ADD COLUMN acl_groups TEXT",
		"ALTER TABLE chunks ADD COLUMN business_owner TEXT",
		"ALTER TABLE chunks ADD COLUMN sensitivity TEXT",
		"ALTER TABLE chunks ADD COLUMN trust_level FLOAT",
		"ALTER TABLE chunks ADD COLUMN source_quality FLOAT",
		"ALTER TABLE chunks ADD COLUMN freshness_score FLOAT",
		"ALTER TABLE chunks ADD COLUMN quality_score FLOAT",
		"ALTER TABLE chunks ADD COLUMN feedback_score FLOAT",
		"ALTER TABLE chunks ADD COLUMN imported_at TEXT",
		"ALTER TABLE chunks ADD COLUMN updated_at TEXT",
		"ALTER TABLE chunks ADD COLUMN content_hash TEXT",
		"ALTER TABLE chunks ADD COLUMN open_link_allowed INT",
	}
	for _, m := range migrations {
		if st, err := tinysql.ParseSQL(m); err == nil {
			_, _ = tinysql.Execute(context.Background(), s.db, "default", st)
		}
	}
	// Normalize NULL / empty default values.
	normalizations := []string{
		"UPDATE chunks SET role_scope='|all|' WHERE role_scope IS NULL OR role_scope = ''",
		"UPDATE chunks SET acl_groups=role_scope WHERE acl_groups IS NULL OR acl_groups = ''",
		"UPDATE chunks SET source_quality=0.60 WHERE source_quality IS NULL",
		"UPDATE chunks SET trust_level=0.50 WHERE trust_level IS NULL",
		"UPDATE chunks SET freshness_score=0.50 WHERE freshness_score IS NULL",
		"UPDATE chunks SET quality_score=0.50 WHERE quality_score IS NULL",
		"UPDATE chunks SET feedback_score=0.50 WHERE feedback_score IS NULL",
		"UPDATE chunks SET open_link_allowed=1 WHERE open_link_allowed IS NULL",
	}
	for _, n := range normalizations {
		if st, err := tinysql.ParseSQL(n); err == nil {
			_, _ = tinysql.Execute(context.Background(), s.db, "default", st)
		}
	}
	// Pre-compile frequently used static queries.
	s.cntAll, _ = tinysql.Compile(s.qc, "SELECT COUNT(*) AS cnt FROM chunks")
	s.maxID, _ = tinysql.Compile(s.qc, "SELECT MAX(id) AS mid FROM chunks")
	return nil
}

func (s *tinySQLChunkStore) maxChunkID() int {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	var rs *tinysql.ResultSet
	var err error
	if s.maxID != nil {
		rs, err = tinysql.ExecuteCompiled(context.Background(), s.db, "default", s.maxID)
	} else {
		st, e := tinysql.ParseSQL("SELECT MAX(id) AS mid FROM chunks")
		if e != nil {
			return -1
		}
		rs, err = tinysql.Execute(context.Background(), s.db, "default", st)
	}
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return -1
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "mid")
	if !ok || v == nil {
		return -1
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}

func (s *tinySQLChunkStore) checkArticleExists(documentID, roleScope string) (bool, error) {
	q := fmt.Sprintf(
		"SELECT COUNT(*) AS cnt FROM chunks WHERE document_id = '%s' AND role_scope = '%s'",
		escapeSQ(documentID), escapeSQ(roleScope),
	)
	st, err := tinysql.ParseSQL(q)
	if err != nil {
		return false, err
	}
	s.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), s.db, "default", st)
	s.dbMu.Unlock()
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return false, err
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "cnt")
	if !ok || v == nil {
		return false, nil
	}
	switch n := v.(type) {
	case int:
		return n > 0, nil
	case int64:
		return n > 0, nil
	case float64:
		return n > 0, nil
	}
	return false, nil
}

func (s *tinySQLChunkStore) insertChunks(chunks []storedChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	var vals []string
	var legacyVals []string
	for _, c := range chunks {
		openInt := 0
		if c.OpenLinkAllowed {
			openInt = 1
		}
		legacyVals = append(legacyVals, fmt.Sprintf(
			"(%d, '%s', %d, '%s', VEC_FROM_JSON('%s'), '%s', '%s')",
			c.ID, escapeSQ(c.Article), c.ChunkIdx, escapeSQ(c.Content),
			escapeSQ(vecJSON(c.Embedding)), escapeSQ(c.EmbedModel), escapeSQ(c.RoleScope),
		))
		vals = append(vals, fmt.Sprintf(
			"(%d, '%s', %d, '%s', VEC_FROM_JSON('%s'), '%s', '%s', '%s', '%s', 'tinyrag', '%s', '%s', '', '%s', '%s', '%s', '%s', '%s', %.4f, %.4f, %.4f, %.4f, %.4f, '%s', '%s', '%s', %d)",
			c.ID, escapeSQ(c.Article), c.ChunkIdx, escapeSQ(c.Content),
			escapeSQ(vecJSON(c.Embedding)), escapeSQ(c.EmbedModel), escapeSQ(c.RoleScope),
			escapeSQ(c.ChunkID), escapeSQ(c.DocumentID),
			escapeSQ(c.SourceType), escapeSQ(c.SourceTitle),
			escapeSQ(c.SourceObjectID), escapeSQ(c.SourceVersion),
			escapeSQ(c.RoleScope), escapeSQ(c.BusinessOwner), escapeSQ(c.Sensitivity),
			c.TrustLevel, c.SourceQuality, c.FreshnessScore, c.QualityScore, c.FeedbackScore,
			escapeSQ(c.ImportedAt), escapeSQ(c.UpdatedAt), escapeSQ(c.ContentHash),
			openInt,
		))
	}
	q := "INSERT INTO chunks VALUES " + strings.Join(vals, ",")
	st, err := tinysql.ParseSQL(q)
	if err != nil {
		return fmt.Errorf("parse bulk insert: %w", err)
	}
	s.dbMu.Lock()
	_, execErr := tinysql.Execute(context.Background(), s.db, "default", st)
	if execErr != nil && strings.Contains(strings.ToLower(execErr.Error()), "unknown column") {
		// Fallback to legacy schema for storage engines without all R3 columns.
		legacyQ := "INSERT INTO chunks VALUES " + strings.Join(legacyVals, ",")
		if lst, pe := tinysql.ParseSQL(legacyQ); pe == nil {
			_, execErr = tinysql.Execute(context.Background(), s.db, "default", lst)
		}
	}
	s.dbMu.Unlock()
	if execErr != nil {
		return fmt.Errorf("exec bulk insert: %w", execErr)
	}
	return nil
}

// rowToRetrievalHit converts a tinySQL result row to a retrievalHit.
// The R3Score and Citation fields are left at their zero values; the caller
// must compute them using the rankPolicy.
func rowToRetrievalHit(row map[string]any, rawScore float64) retrievalHit {
	getStr := func(key, fallback string) string {
		if v, ok := row[key]; ok && v != nil {
			s := fmt.Sprint(v)
			if s != "" {
				return s
			}
		}
		return fallback
	}
	getFloat := func(key string, fallback float64) float64 {
		if v, ok := row[key]; ok && v != nil {
			switch tv := v.(type) {
			case float64:
				return tv
			case int:
				return float64(tv)
			case int64:
				return float64(tv)
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(tv), 64); err == nil {
					return f
				}
			}
		}
		return fallback
	}
	getInt := func(key string, fallback int) int {
		if v, ok := row[key]; ok && v != nil {
			switch tv := v.(type) {
			case int:
				return tv
			case int64:
				return int(tv)
			case float64:
				return int(tv)
			}
		}
		return fallback
	}
	getBool := func(key string, fallback bool) bool {
		if v, ok := row[key]; ok && v != nil {
			switch tv := v.(type) {
			case bool:
				return tv
			case int:
				return tv != 0
			case int64:
				return tv != 0
			case float64:
				return tv != 0
			case string:
				s := strings.TrimSpace(strings.ToLower(tv))
				return s != "0" && s != "false"
			}
		}
		return fallback
	}
	getTime := func(key string) time.Time {
		raw := strings.TrimSpace(getStr(key, ""))
		if raw == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
		return time.Time{}
	}

	article := getStr("article", "")
	content := getStr("content", "")
	chunkIdx := getInt("chunk_idx", 0)
	chunkID := getStr("chunk_id", "")
	documentID := getStr("document_id", "")
	if documentID == "" {
		documentID = stableContentHash(article)
	}
	if chunkID == "" {
		if v, ok := row["id"]; ok {
			chunkID = fmt.Sprintf("%v", v)
		}
	}

	unit := RetrievalUnit{
		ChunkID:         chunkID,
		DocumentID:      documentID,
		ChunkIdx:        chunkIdx,
		Content:         content,
		SourceSystem:    getStr("source_system", "tinyrag"),
		SourceType:      getStr("source_type", normalizeR3SourceType(article)),
		SourceTitle:     getStr("source_title", article),
		SourceURL:       getStr("source_url", ""),
		SourceObjectID:  getStr("source_object_id", article),
		SourceVersion:   getStr("source_version", "v1"),
		RoleScope:       getStr("role_scope", "|all|"),
		ACLGroups:       getStr("acl_groups", getStr("role_scope", "|all|")),
		BusinessOwner:   getStr("business_owner", "it"),
		Sensitivity:     getStr("sensitivity", "internal"),
		TrustLevel:      getFloat("trust_level", 0.5),
		SourceQuality:   getFloat("source_quality", sourceTypeQualityDefault(getStr("source_type", ""))),
		FreshnessScore:  getFloat("freshness_score", 0.0),
		QualityScore:    getFloat("quality_score", 0.5),
		FeedbackScore:   getFloat("feedback_score", 0.5),
		ImportedAt:      getTime("imported_at"),
		UpdatedAt:       getTime("updated_at"),
		ContentHash:     getStr("content_hash", stableContentHash(content)),
		OpenLinkAllowed: getBool("open_link_allowed", true),
	}

	return retrievalHit{
		Article:    article,
		ChunkIdx:   chunkIdx,
		Content:    content,
		Score:      rawScore,
		Unit:       unit,
		ChunkID:    chunkID,
		DocumentID: documentID,
	}
}

// tinySQLRowToMap extracts selected columns from a tinySQL result row into a
// plain map so that callers do not need to import the tinysql package.
func tinySQLRowToMap(row map[string]any, keys []string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := tinysql.GetVal(row, k); ok {
			out[k] = v
		}
	}
	return out
}

func (s *tinySQLChunkStore) searchTopK(vec []float64, embedModel, roleFilter string, limit int) ([]retrievalHit, error) {
	q := fmt.Sprintf(
		"SELECT id, content, article, chunk_idx, chunk_id, document_id, source_system, source_type,"+
			" source_title, source_url, source_object_id, source_version, role_scope, acl_groups,"+
			" business_owner, sensitivity, trust_level, source_quality, freshness_score, quality_score,"+
			" feedback_score, imported_at, updated_at, content_hash, open_link_allowed,"+
			" VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON('%s')) AS score"+
			" FROM chunks WHERE embed_model = '%s' AND %s ORDER BY score DESC LIMIT %d",
		escapeSQ(vecJSON(vec)), escapeSQ(embedModel), roleFilter, limit,
	)
	st, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil, err
	}
	s.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), s.db, "default", st)
	s.dbMu.Unlock()
	if err != nil {
		return nil, err
	}

	searchKeys := []string{
		"id", "content", "article", "chunk_idx", "chunk_id", "document_id",
		"source_system", "source_type", "source_title", "source_url", "source_object_id",
		"source_version", "role_scope", "acl_groups", "business_owner", "sensitivity",
		"trust_level", "source_quality", "freshness_score", "quality_score", "feedback_score",
		"imported_at", "updated_at", "content_hash", "open_link_allowed", "score",
	}

	hits := make([]retrievalHit, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		m := tinySQLRowToMap(row, searchKeys)
		scoreRaw, _ := m["score"]
		score := 0.0
		switch sv := scoreRaw.(type) {
		case float64:
			score = sv
		case int:
			score = float64(sv)
		case int64:
			score = float64(sv)
		}
		hits = append(hits, rowToRetrievalHit(m, score))
	}
	return hits, nil
}

func (s *tinySQLChunkStore) fetchNeighborContent(article string, chunkIdx int, roleFilter string) (string, bool) {
	q := fmt.Sprintf(
		"SELECT content FROM chunks WHERE article = '%s' AND chunk_idx = %d AND %s",
		escapeSQ(article), chunkIdx, roleFilter,
	)
	st, err := tinysql.ParseSQL(q)
	if err != nil {
		return "", false
	}
	s.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), s.db, "default", st)
	s.dbMu.Unlock()
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return "", false
	}
	c, ok := tinysql.GetVal(rs.Rows[0], "content")
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%v", c), true
}

func (s *tinySQLChunkStore) loadArticleChunks(article, roleFilter string) ([]map[string]any, error) {
	q := fmt.Sprintf(
		"SELECT article, chunk_idx, content, chunk_id, document_id, source_system, source_type,"+
			" source_title, source_url, sensitivity, trust_level, updated_at, open_link_allowed"+
			" FROM chunks WHERE LOWER(article) = LOWER('%s') AND %s ORDER BY chunk_idx",
		escapeSQ(article), roleFilter,
	)
	st, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil, err
	}
	s.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), s.db, "default", st)
	s.dbMu.Unlock()
	if err != nil || rs == nil {
		return nil, err
	}
	keys := []string{
		"article", "chunk_idx", "content", "chunk_id", "document_id",
		"source_system", "source_type", "source_title", "source_url",
		"sensitivity", "trust_level", "updated_at", "open_link_allowed",
	}
	out := make([]map[string]any, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		out = append(out, tinySQLRowToMap(row, keys))
	}
	return out, nil
}

func (s *tinySQLChunkStore) listSources(roleFilter string) []map[string]any {
	q := fmt.Sprintf(
		"SELECT article, COUNT(*) AS cnt FROM chunks WHERE %s GROUP BY article ORDER BY article",
		roleFilter,
	)
	st, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil
	}
	s.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), s.db, "default", st)
	s.dbMu.Unlock()
	if err != nil || rs == nil {
		return nil
	}
	var out []map[string]any
	for _, row := range rs.Rows {
		art, ok1 := tinysql.GetVal(row, "article")
		cnt, ok2 := tinysql.GetVal(row, "cnt")
		if ok1 && ok2 {
			out = append(out, map[string]any{
				"article": fmt.Sprintf("%v", art),
				"chunks":  cnt,
			})
		}
	}
	return out
}

func (s *tinySQLChunkStore) deleteSource(article, roleFilter string) error {
	q := fmt.Sprintf(
		"DELETE FROM chunks WHERE article = '%s' AND %s",
		escapeSQ(article), roleFilter,
	)
	st, err := tinysql.ParseSQL(q)
	if err != nil {
		return err
	}
	s.dbMu.Lock()
	_, err = tinysql.Execute(context.Background(), s.db, "default", st)
	s.dbMu.Unlock()
	return err
}

func (s *tinySQLChunkStore) countChunks(roleFilter string) int {
	var q string
	if roleFilter == "" || roleFilter == "1=1" {
		q = "SELECT COUNT(*) AS cnt FROM chunks"
	} else {
		q = fmt.Sprintf("SELECT COUNT(*) AS cnt FROM chunks WHERE %s", roleFilter)
	}
	s.dbMu.Lock()
	var rs *tinysql.ResultSet
	var err error
	if s.cntAll != nil && (roleFilter == "" || roleFilter == "1=1") {
		rs, err = tinysql.ExecuteCompiled(context.Background(), s.db, "default", s.cntAll)
	} else {
		if st, pe := tinysql.ParseSQL(q); pe == nil {
			rs, err = tinysql.Execute(context.Background(), s.db, "default", st)
		}
	}
	s.dbMu.Unlock()
	if err != nil || rs == nil || len(rs.Rows) == 0 {
		return 0
	}
	v, ok := tinysql.GetVal(rs.Rows[0], "cnt")
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func (s *tinySQLChunkStore) save() error {
	if s.path == "" {
		return nil
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	switch s.mode {
	case tinysql.ModeDisk, tinysql.ModeHybrid, tinysql.ModeIndex:
		return s.db.Sync()
	default:
		return tinysql.SaveToFile(s.db, s.path)
	}
}

func (s *tinySQLChunkStore) close() error { return nil }

// ─────────────────────────────────────────────────────────────────────────────
// sqlite-vec stub
// ─────────────────────────────────────────────────────────────────────────────

// sqliteVecChunkStore is a placeholder for the sqlite-vec backend.
// A real implementation is available when the binary is built with
//
//	go build -tags sqlite_vec …
//
// and the sqlite-vec shared library is present.  Without the build tag this
// stub is compiled in and returns a clear error on creation.
type sqliteVecChunkStore struct{}

func newSQLiteVecChunkStore(_ string) (*sqliteVecChunkStore, error) {
	return nil, fmt.Errorf(
		"sqlite-vec backend is not available in this build; " +
			"rebuild with: go build -tags sqlite_vec ./...",
	)
}

func (s *sqliteVecChunkStore) init() error     { return errSQLiteVecUnavailable }
func (s *sqliteVecChunkStore) maxChunkID() int { return -1 }
func (s *sqliteVecChunkStore) checkArticleExists(_, _ string) (bool, error) {
	return false, errSQLiteVecUnavailable
}
func (s *sqliteVecChunkStore) insertChunks(_ []storedChunk) error {
	return errSQLiteVecUnavailable
}
func (s *sqliteVecChunkStore) searchTopK(_ []float64, _, _ string, _ int) ([]retrievalHit, error) {
	return nil, errSQLiteVecUnavailable
}
func (s *sqliteVecChunkStore) fetchNeighborContent(_ string, _ int, _ string) (string, bool) {
	return "", false
}
func (s *sqliteVecChunkStore) loadArticleChunks(_, _ string) ([]map[string]any, error) {
	return nil, errSQLiteVecUnavailable
}
func (s *sqliteVecChunkStore) listSources(_ string) []map[string]any { return nil }
func (s *sqliteVecChunkStore) deleteSource(_, _ string) error        { return errSQLiteVecUnavailable }
func (s *sqliteVecChunkStore) countChunks(_ string) int              { return 0 }
func (s *sqliteVecChunkStore) save() error                           { return errSQLiteVecUnavailable }
func (s *sqliteVecChunkStore) close() error                          { return nil }

var errSQLiteVecUnavailable = fmt.Errorf(
	"sqlite-vec backend unavailable; rebuild with -tags sqlite_vec",
)

// ─────────────────────────────────────────────────────────────────────────────
// Factory
// ─────────────────────────────────────────────────────────────────────────────

// newVectorChunkStore returns the appropriate vectorChunkStore for the
// requested backend.  Supported values for backend:
//
//   - "" or "tinysql" → tinySQLChunkStore (default)
//   - "sqlite-vec" / "sqlitevec" / "sqlite_vec" → sqliteVecChunkStore
//     (returns an error unless built with -tags sqlite_vec)
func newVectorChunkStore(backend string, db *tinysql.DB, mu *sync.Mutex, path string, mode tinysql.StorageMode) (vectorChunkStore, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "sqlite-vec", "sqlitevec", "sqlite_vec":
		return newSQLiteVecChunkStore(path)
	default: // "tinysql" or empty
		return newTinySQLChunkStore(db, mu, path, mode), nil
	}
}
