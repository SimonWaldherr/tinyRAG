package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// ragSystem encapsulates the tinyRAG knowledge store, embedding
// functionality and an associated tinySQL database instance.
type ragSystem struct {
	db     *tinysql.DB
	dbPath string
	k      int
	dim    int

	// Storage mode (for display / logging)
	storageMode tinysql.StorageMode

	// chunkStore is the pluggable vector persistence backend.
	// Default: tinySQLChunkStore; future: sqliteVecChunkStore.
	chunkStore vectorChunkStore

	// Settings-sensitive runtime state
	lmMu sync.RWMutex
	lm   lmProvider

	// DB mutex (tinySQL isn't designed for heavy concurrent writes)
	dbMu sync.Mutex

	// Monotonic chunk IDs (avoid collisions even after deletes)
	idMu   sync.Mutex
	nextID int

	// Pre-compiled SQL queries for frequently-called static statements.
	queryCache   *tinysql.QueryCache
	countAllStmt *tinysql.CompiledQuery
	maxIDStmt    *tinysql.CompiledQuery
}

// newRAG initializes a new `ragSystem` backed by a tinySQL DB using
// the provided storage mode and memory constraints.
func newRAG(lm lmProvider, k int, dbPath string, storageMode tinysql.StorageMode, maxMemMB int64) (*ragSystem, error) {
	var db *tinysql.DB
	var err error
	var encryptionKey []byte
	if settings != nil {
		s := settings.get()
		if s.StorageEncryptionEnabled {
			if storageMode == tinysql.ModeMemory || storageMode == tinysql.ModeWAL {
				return nil, fmt.Errorf("storage encryption requires disk, index, or hybrid mode (WAL is not supported)")
			}
			encryptionKey, err = storageEncryptionKey(true)
			if err != nil {
				return nil, err
			}
		}
	}

	switch storageMode {
	case tinysql.ModeMemory:
		// In-memory with optional save-on-close.
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode:          tinysql.ModeMemory,
			Path:          dbPath, // saves GOB on Close if non-empty
			EncryptionKey: encryptionKey,
		})
		if err != nil {
			return nil, fmt.Errorf("open memory db: %w", err)
		}
		if dbPath != "" {
			fmt.Printf("Storage mode: memory (save to %s on exit)\n", dbPath)
		} else {
			fmt.Println("Storage mode: memory (ephemeral, no persistence)")
		}

	case tinysql.ModeWAL:
		if dbPath == "" {
			dbPath = "tinyrag.gob"
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode:          tinysql.ModeWAL,
			Path:          dbPath,
			EncryptionKey: encryptionKey,
		})
		if err != nil {
			return nil, fmt.Errorf("open wal db: %w", err)
		}
		fmt.Printf("Storage mode: WAL (checkpoint to %s)\n", dbPath)

	case tinysql.ModeDisk:
		if dbPath == "" {
			dbPath = "tinyrag.db"
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode:          tinysql.ModeDisk,
			Path:          dbPath,
			EncryptionKey: encryptionKey,
		})
		if err != nil {
			return nil, fmt.Errorf("open disk db: %w", err)
		}
		fmt.Printf("Storage mode: disk (tables in %s/)\n", dbPath)

	case tinysql.ModeIndex:
		if dbPath == "" {
			dbPath = "tinyrag.db"
		}
		mem := maxMemMB * 1024 * 1024
		if mem <= 0 {
			mem = 64 * 1024 * 1024
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode:           tinysql.ModeIndex,
			Path:           dbPath,
			MaxMemoryBytes: mem,
			EncryptionKey:  encryptionKey,
		})
		if err != nil {
			return nil, fmt.Errorf("open index db: %w", err)
		}
		fmt.Printf("Storage mode: index (schemas in RAM, rows on disk in %s/, max %d MB)\n", dbPath, maxMemMB)

	case tinysql.ModeHybrid:
		if dbPath == "" {
			dbPath = "tinyrag.db"
		}
		mem := maxMemMB * 1024 * 1024
		if mem <= 0 {
			mem = 256 * 1024 * 1024
		}
		db, err = tinysql.OpenDB(tinysql.StorageConfig{
			Mode:           tinysql.ModeHybrid,
			Path:           dbPath,
			MaxMemoryBytes: mem,
			EncryptionKey:  encryptionKey,
		})
		if err != nil {
			return nil, fmt.Errorf("open hybrid db: %w", err)
		}
		fmt.Printf("Storage mode: hybrid (LRU cache %d MB, disk in %s/)\n", maxMemMB, dbPath)

	default:
		// Fallback: legacy behaviour (load GOB if exists, else new)
		if dbPath != "" {
			if loaded, loadErr := tinysql.LoadFromFile(dbPath); loadErr == nil {
				db = loaded
				fmt.Printf("Loaded existing database from %s\n", dbPath)
			} else {
				db = tinysql.NewDB()
				fmt.Printf("Creating new database (will save to %s)\n", dbPath)
			}
		} else {
			db = tinysql.NewDB()
		}
	}

	r := &ragSystem{
		db:          db,
		lm:          lm,
		k:           k,
		dbPath:      dbPath,
		storageMode: storageMode,
		queryCache:  tinysql.NewQueryCache(32),
	}

	// Wire up the vector chunk store.  The tinySQLChunkStore shares the
	// ragSystem's DB and mutex so that chunk and R3 operations remain
	// serialized through a single lock.
	vectorBackend := ""
	if settings != nil {
		vectorBackend = settings.get().VectorBackend
	}
	cs, err := newVectorChunkStore(vectorBackend, db, &r.dbMu, dbPath, storageMode)
	if err != nil {
		return nil, fmt.Errorf("init vector store backend %q: %w", vectorBackend, err)
	}
	r.chunkStore = cs
	return r, nil
}

// setLM atomically replaces the runtime `lmClient` used for embeddings
// and chat requests.
func (r *ragSystem) setLM(lm lmProvider) {
	r.lmMu.Lock()
	defer r.lmMu.Unlock()
	r.lm = lm
}

// getLM returns the currently configured `lmClient`.
func (r *ragSystem) getLM() lmProvider {
	r.lmMu.RLock()
	defer r.lmMu.RUnlock()
	return r.lm
}

// getActiveEmbedModel returns the currently configured embedding model name
// used for newly created chunks or for retrieval filtering.
func (r *ragSystem) getActiveEmbedModel() string {
	s := settings.get()
	if s.EmbedModel != "" {
		return s.EmbedModel
	}
	return ""
}

// scoreThreshold returns the configured minimum cosine-similarity score for
// primary retrieval hits. Falls back to 0.60 when unconfigured.
func (r *ragSystem) scoreThreshold() float64 {
	if settings != nil {
		if t := settings.get().VectorSearchThreshold; t > 0 {
			return clampUnitInterval(t)
		}
	}
	return 0.60
}

// save flushes the underlying database to disk or performs a sync
// depending on the configured storage mode.
func (r *ragSystem) save() error {
	if r.dbPath == "" {
		return nil
	}
	r.dbMu.Lock()
	defer r.dbMu.Unlock()

	// For disk-backed modes, Sync flushes dirty tables to disk.
	// For memory mode, this is a no-op (data saved on Close).
	switch r.storageMode {
	case tinysql.ModeDisk, tinysql.ModeHybrid, tinysql.ModeIndex:
		return r.db.Sync()
	default:
		// Legacy / ModeMemory / ModeWAL: full GOB snapshot
		return tinysql.SaveToFile(r.db, r.dbPath)
	}
}

// init creates required DB tables and initializes runtime counters.
func (r *ragSystem) init() error {
	// Delegate chunks table creation/migration to the vector store backend.
	if err := r.chunkStore.init(); err != nil {
		return err
	}

	// Canonical source registry + import jobs + audit log tables.
	// These remain in the tinySQL DB for all vector backends.
	r.dbMu.Lock()
	createR3Tables := []string{
		"CREATE TABLE IF NOT EXISTS r3_sources (document_id TEXT, provenance TEXT, ownership TEXT, trust_tier TEXT, lifecycle TEXT, retention_policy TEXT, acl_metadata TEXT, updated_at TEXT)",
		"CREATE TABLE IF NOT EXISTS r3_import_jobs (job_id TEXT, source_system TEXT, cursor TEXT, status TEXT, processed INT, imported INT, skipped INT, last_error TEXT, last_hash TEXT, started_at TEXT, updated_at TEXT, completed_at TEXT, idempotency_id TEXT)",
		"CREATE TABLE IF NOT EXISTS r3_audit_events (event_id TEXT, event_type TEXT, actor TEXT, entity_type TEXT, entity_id TEXT, decision TEXT, policy_class TEXT, details TEXT, created_at TEXT)",
		"CREATE TABLE IF NOT EXISTS r3_feedback_events (event_id TEXT, request_id TEXT, actor TEXT, rating INT, document_id TEXT, chunk_id TEXT, created_at TEXT)",
	}
	for _, stmtSQL := range createR3Tables {
		if stmt, err := tinysql.ParseSQL(stmtSQL); err == nil {
			_, _ = tinysql.Execute(context.Background(), r.db, "default", stmt)
		}
	}
	r.dbMu.Unlock()

	// Initialize nextID from MAX(id)+1.
	r.idMu.Lock()
	defer r.idMu.Unlock()
	r.nextID = r.chunkStore.maxChunkID() + 1
	return nil
}

// maxChunkIDLocked delegates to the chunkStore.  The "Locked" suffix is kept
// for backward-compatibility; the chunkStore itself manages its own locking.
func (r *ragSystem) maxChunkIDLocked() int {
	return r.chunkStore.maxChunkID()
}

// allocIDs reserves `n` monotonic IDs for new chunks.
func (r *ragSystem) allocIDs(n int) int {
	r.idMu.Lock()
	defer r.idMu.Unlock()
	start := r.nextID
	r.nextID += n
	return start
}

func (r *ragSystem) upsertR3Source(src SourceRegistryRecord) error {
	if strings.TrimSpace(src.DocumentID) == "" {
		return nil
	}
	delSQL := fmt.Sprintf("DELETE FROM r3_sources WHERE document_id = '%s'", escapeSQ(src.DocumentID))
	insSQL := fmt.Sprintf(
		"INSERT INTO r3_sources VALUES ('%s','%s','%s','%s','%s','%s','%s','%s')",
		escapeSQ(src.DocumentID),
		escapeSQ(src.Provenance),
		escapeSQ(src.Ownership),
		escapeSQ(src.TrustTier),
		escapeSQ(src.Lifecycle),
		escapeSQ(src.RetentionPolicy),
		escapeSQ(src.ACLMetadata),
		escapeSQ(time.Now().UTC().Format(time.RFC3339)),
	)
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	if delStmt, err := tinysql.ParseSQL(delSQL); err == nil {
		_, _ = tinysql.Execute(context.Background(), r.db, "default", delStmt)
	}
	insStmt, err := tinysql.ParseSQL(insSQL)
	if err != nil {
		return err
	}
	_, err = tinysql.Execute(context.Background(), r.db, "default", insStmt)
	return err
}

func (r *ragSystem) logR3Audit(evt AuditEvent) {
	if strings.TrimSpace(evt.EventID) == "" {
		evt.EventID = stableContentHash(fmt.Sprintf("%s|%s|%d", evt.EventType, evt.EntityID, time.Now().UnixNano()))[:16]
	}
	if evt.CreatedAt.IsZero() {
		evt.CreatedAt = time.Now().UTC()
	}
	sql := fmt.Sprintf(
		"INSERT INTO r3_audit_events VALUES ('%s','%s','%s','%s','%s','%s','%s','%s','%s')",
		escapeSQ(evt.EventID),
		escapeSQ(evt.EventType),
		escapeSQ(evt.Actor),
		escapeSQ(evt.EntityType),
		escapeSQ(evt.EntityID),
		escapeSQ(evt.Decision),
		escapeSQ(evt.PolicyClass),
		escapeSQ(evt.Details),
		escapeSQ(evt.CreatedAt.UTC().Format(time.RFC3339)),
	)
	stmt, err := tinysql.ParseSQL(sql)
	if err != nil {
		return
	}
	r.dbMu.Lock()
	_, _ = tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
}

func (r *ragSystem) upsertImportJob(job ImportJob) {
	if strings.TrimSpace(job.JobID) == "" {
		return
	}
	delSQL := fmt.Sprintf("DELETE FROM r3_import_jobs WHERE job_id = '%s'", escapeSQ(job.JobID))
	completedAt := ""
	if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
		completedAt = job.CompletedAt.UTC().Format(time.RFC3339)
	}
	insSQL := fmt.Sprintf(
		"INSERT INTO r3_import_jobs VALUES ('%s','%s','%s','%s',%d,%d,%d,'%s','%s','%s','%s','%s','%s')",
		escapeSQ(job.JobID),
		escapeSQ(job.SourceSystem),
		escapeSQ(job.Cursor),
		escapeSQ(string(job.Status)),
		job.Processed,
		job.Imported,
		job.Skipped,
		escapeSQ(job.LastError),
		escapeSQ(job.LastHash),
		escapeSQ(job.StartedAt.UTC().Format(time.RFC3339)),
		escapeSQ(job.UpdatedAt.UTC().Format(time.RFC3339)),
		escapeSQ(completedAt),
		escapeSQ(job.IdempotencyID),
	)
	r.dbMu.Lock()
	defer r.dbMu.Unlock()
	if delStmt, err := tinysql.ParseSQL(delSQL); err == nil {
		_, _ = tinysql.Execute(context.Background(), r.db, "default", delStmt)
	}
	if insStmt, err := tinysql.ParseSQL(insSQL); err == nil {
		_, _ = tinysql.Execute(context.Background(), r.db, "default", insStmt)
	}
}

// addChunks embeds and stores `chunks` for the given `article` into
// the database, performing batched inserts.
func (r *ragSystem) addChunks(article string, chunks []string, embedModel string) error {
	return r.addChunksWithRoles(article, chunks, embedModel, nil)
}

// addChunksWithRoles stores chunks with an explicit role visibility scope.
// If roles are omitted, the current active role is used.
func (r *ragSystem) addChunksWithRoles(article string, chunks []string, embedModel string, roles []string) error {
	return r.addChunksWithMetadata(article, chunks, embedModel, roles, R3IngestMetadata{})
}

func clampUnitInterval(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

type ingestWriteResult struct {
	DocumentID   string `json:"document_id"`
	Status       string `json:"status"`
	Chunks       int    `json:"chunks"`
	ContentHash  string `json:"content_hash"`
	PreviousHash string `json:"previous_hash,omitempty"`
}

// addChunksWithMetadata stores chunks with explicit R3 provenance metadata.
// Existing callers use addChunks/addChunksWithRoles and receive the historical
// defaults; source adapters can use this path to preserve external provenance.
func (r *ragSystem) addChunksWithMetadata(article string, chunks []string, embedModel string, roles []string, meta R3IngestMetadata) error {
	_, err := r.addChunksWithMetadataResult(article, chunks, embedModel, roles, meta)
	return err
}

func (r *ragSystem) addChunksWithMetadataResult(article string, chunks []string, embedModel string, roles []string, meta R3IngestMetadata) (ingestWriteResult, error) {
	if len(chunks) == 0 {
		return ingestWriteResult{Status: "empty"}, nil
	}
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	normRoles := normalizeRoleScopes(roles, activeRole)
	roleScope := serializeRoleScope(normRoles)
	documentID := strings.TrimSpace(meta.DocumentID)
	if documentID == "" {
		documentID = stableContentHash(article)
	}
	contentHash := documentFingerprintFromChunks(chunks)
	now := time.Now().UTC()
	importedAt := now
	if !meta.ImportedAt.IsZero() {
		importedAt = meta.ImportedAt.UTC()
	}
	updatedAt := now
	if !meta.UpdatedAt.IsZero() {
		updatedAt = meta.UpdatedAt.UTC()
	}
	nowTS := importedAt.Format(time.RFC3339)
	updatedTS := updatedAt.Format(time.RFC3339)
	sourceType := strings.TrimSpace(meta.SourceType)
	if sourceType == "" {
		sourceType = normalizeR3SourceType(article)
	}
	sourceSystem := strings.TrimSpace(meta.SourceSystem)
	if sourceSystem == "" {
		sourceSystem = "tinyrag"
	}
	sourceTitle := strings.TrimSpace(meta.SourceTitle)
	if sourceTitle == "" {
		sourceTitle = article
	}
	sourceObjectID := strings.TrimSpace(meta.SourceObjectID)
	if sourceObjectID == "" {
		sourceObjectID = article
	}
	sourceVersion := strings.TrimSpace(meta.SourceVersion)
	if sourceVersion == "" {
		sourceVersion = "v1"
	}
	aclGroups := strings.TrimSpace(meta.ACLGroups)
	if aclGroups == "" {
		aclGroups = roleScope
	}
	businessOwner := strings.TrimSpace(meta.BusinessOwner)
	if businessOwner == "" {
		businessOwner = activeRole
	}
	defaultSourceQuality := sourceTypeQualityDefault(sourceType)
	if meta.SourceQuality > 0 {
		defaultSourceQuality = clampUnitInterval(meta.SourceQuality)
	}
	defaultTrust := 0.65
	if sourceType == "chat" {
		defaultTrust = 0.40
	}
	if sourceType == "ticket" {
		defaultTrust = 0.50
	}
	if meta.TrustLevel > 0 {
		defaultTrust = clampUnitInterval(meta.TrustLevel)
	}
	defaultFresh := 1.0
	if meta.FreshnessScore > 0 {
		defaultFresh = clampUnitInterval(meta.FreshnessScore)
	} else if !meta.UpdatedAt.IsZero() {
		defaultFresh = freshnessDecayScore(updatedAt, now)
	}
	defaultQuality := 0.65
	if meta.QualityScore > 0 {
		defaultQuality = clampUnitInterval(meta.QualityScore)
	}
	defaultFeedback := 0.50
	if meta.FeedbackScore > 0 {
		defaultFeedback = clampUnitInterval(meta.FeedbackScore)
	}
	sensitivity := "internal"
	if (SensitivityPolicy{}).MustPseudonymize(detectSensitivityClass(strings.Join(chunks, "\n"))) {
		sensitivity = "confidential"
	}
	if strings.TrimSpace(meta.Sensitivity) != "" {
		sensitivity = strings.ToLower(strings.TrimSpace(meta.Sensitivity))
	}
	openLinkAllowed := true
	if meta.OpenLinkAllowedSet {
		openLinkAllowed = meta.OpenLinkAllowed
	}
	updateMode := normalizeIngestUpdateMode(meta.UpdateMode)
	existingHash, exists, hashErr := r.documentContentHash(documentID, roleScope)
	if hashErr != nil {
		return ingestWriteResult{}, hashErr
	}
	result := ingestWriteResult{
		DocumentID:   documentID,
		Status:       "inserted",
		Chunks:       len(chunks),
		ContentHash:  contentHash,
		PreviousHash: existingHash,
	}
	if exists {
		switch updateMode {
		case "upsert":
			if existingHash == contentHash {
				result.Status = "skipped_unchanged"
				result.Chunks = 0
				fmt.Printf("skip addChunks: document '%s' unchanged\n", article)
				return result, nil
			}
			result.Status = "updated"
		case "replace":
			result.Status = "updated"
		default:
			result.Status = "skipped_existing"
			result.Chunks = 0
			fmt.Printf("skip addChunks: article '%s' already present\n", article)
			return result, nil
		}
	}
	batchSize := 16
	pending := make([]storedChunk, 0, len(chunks))

	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[i:end]

		// Embed without holding DB lock.
		vecs, err := r.getLM().embed(batch)
		if err != nil {
			return ingestWriteResult{}, fmt.Errorf("embed batch %d: %w", i/batchSize, err)
		}
		if len(vecs) != len(batch) {
			return ingestWriteResult{}, fmt.Errorf("embed batch %d: got %d vectors for %d chunks", i/batchSize, len(vecs), len(batch))
		}
		if r.dim == 0 && len(vecs) > 0 {
			r.dim = len(vecs[0])
		}

		// Allocate IDs for this batch.
		startID := r.allocIDs(len(batch))

		// Build storedChunk slice and delegate persistence to the chunk store.
		sc := make([]storedChunk, len(batch))
		for j, v := range vecs {
			if len(v) == 0 {
				return ingestWriteResult{}, fmt.Errorf("embed batch %d: empty vector for chunk %d", i/batchSize, i+j)
			}
			idx := i + j
			sc[j] = storedChunk{
				ID:              startID + j,
				ChunkIdx:        idx,
				Article:         article,
				Content:         batch[j],
				Embedding:       v,
				EmbedModel:      embedModel,
				RoleScope:       roleScope,
				ChunkID:         fmt.Sprintf("%s:%d", documentID, idx),
				DocumentID:      documentID,
				SourceSystem:    sourceSystem,
				SourceType:      sourceType,
				SourceTitle:     sourceTitle,
				SourceURL:       strings.TrimSpace(meta.SourceURL),
				SourceObjectID:  sourceObjectID,
				SourceVersion:   sourceVersion,
				ACLGroups:       aclGroups,
				BusinessOwner:   businessOwner,
				Sensitivity:     sensitivity,
				TrustLevel:      defaultTrust,
				SourceQuality:   defaultSourceQuality,
				FreshnessScore:  defaultFresh,
				QualityScore:    defaultQuality,
				FeedbackScore:   defaultFeedback,
				ImportedAt:      nowTS,
				UpdatedAt:       updatedTS,
				ContentHash:     stableContentHash(batch[j]),
				OpenLinkAllowed: openLinkAllowed,
			}
		}
		pending = append(pending, sc...)
	}

	// All embeddings are prepared before the store changes. The backend then
	// writes the replacement while preserving the previous document until the
	// new chunk set is complete.
	if err := r.chunkStore.replaceDocumentChunks(documentID, roleScope, pending); err != nil {
		return ingestWriteResult{}, err
	}
	for end := batchSize; end < len(chunks); end += batchSize {
		fmt.Printf("  embedded+stored %d/%d chunks\n", end, len(chunks))
	}
	fmt.Printf("  embedded+stored %d/%d chunks\n", len(chunks), len(chunks))

	if err := r.save(); err != nil {
		log.Printf("WARN: save failed: %v", err)
	}
	provenance := strings.TrimSpace(meta.Provenance)
	if provenance == "" {
		provenance = strings.TrimSpace(meta.SourceURL)
	}
	if provenance == "" {
		provenance = article
	}
	ownership := strings.TrimSpace(meta.Ownership)
	if ownership == "" {
		ownership = businessOwner
	}
	trustTier := strings.TrimSpace(meta.TrustTier)
	if trustTier == "" {
		trustTier = fmt.Sprintf("%.2f", defaultTrust)
	}
	lifecycle := strings.TrimSpace(meta.Lifecycle)
	if lifecycle == "" {
		lifecycle = "active"
	}
	retentionPolicy := strings.TrimSpace(meta.RetentionPolicy)
	if retentionPolicy == "" {
		retentionPolicy = "default"
	}
	_ = r.upsertR3Source(SourceRegistryRecord{
		DocumentID:      documentID,
		Provenance:      provenance,
		Ownership:       ownership,
		TrustTier:       trustTier,
		Lifecycle:       lifecycle,
		RetentionPolicy: retentionPolicy,
		ACLMetadata:     aclGroups,
	})
	return result, nil
}

// docCount returns the total number of stored chunks.
func (r *ragSystem) docCount() int {
	return r.chunkStore.countChunks("")
}

func (r *ragSystem) docCountForRole(role string) int {
	normRole := normalizeDemoRole(role)
	return r.chunkStore.countChunks(roleAndACLFilterSQL(normRole))
}

// searchResult represents a single retrieval hit returned by searchJSON.
type searchResult struct {
	Score      float64  `json:"score"`
	R3Score    float64  `json:"r3_score,omitempty"`
	Content    string   `json:"content"`
	DocumentID string   `json:"document_id,omitempty"`
	ChunkID    string   `json:"chunk_id,omitempty"`
	Citation   Citation `json:"citation,omitempty"`
}

type retrievalHit struct {
	Article  string
	ChunkIdx int
	Content  string
	Score    float64
	// RetrievalScore is the internal semantic score after bounded candidate
	// fusion. Score remains the raw vector similarity for diagnostics.
	RetrievalScore float64
	VectorRank     int
	FullTextRank   int
	R3Score        float64
	Unit           RetrievalUnit
	Citation       Citation
	ChunkID        string
	DocumentID     string
}

// chunkKey identifies a chunk location inside a source document. Titles are
// display metadata and may legitimately collide, so they are only a fallback
// for legacy rows that do not yet carry a document ID.
type chunkKey struct {
	documentID string
	article    string
	chunkIdx   int
}

func makeChunkKey(documentID, article string, chunkIdx int) chunkKey {
	documentID = strings.TrimSpace(documentID)
	if documentID != "" {
		return chunkKey{documentID: documentID, chunkIdx: chunkIdx}
	}
	return chunkKey{article: strings.TrimSpace(article), chunkIdx: chunkIdx}
}

// searchJSON performs an embedding-based vector search for `query`,
// returning up to `k` primary hits along with neighbor chunks.
func (r *ragSystem) searchJSON(query string, k int) ([]searchResult, error) {
	return r.searchJSONContext(context.Background(), query, k)
}

// searchJSONContext is the cancellable variant used by request-scoped tools.
// The legacy wrapper keeps CLI and API callers source-compatible.
func (r *ragSystem) searchJSONContext(ctx context.Context, query string, k int) ([]searchResult, error) {
	candidates, _, _, err := r.searchCandidatesContext(ctx, query, k)
	if err != nil {
		return nil, err
	}

	minScore := r.scoreThreshold()
	results := make([]searchResult, 0, k*3)
	primaryKeys := make(map[chunkKey]bool)
	primaries := make([]retrievalHit, 0, k)
	for _, h := range candidates {
		if len(primaries) >= k {
			break
		}
		if !isRetrievalHitRelevant(query, h, minScore) {
			// R³ ranks eligible evidence but does not replace the configured
			// semantic relevance floor for primary candidates.
			continue
		}
		key := makeChunkKey(h.DocumentID, h.Article, h.ChunkIdx)
		if primaryKeys[key] {
			continue
		}
		primaryKeys[key] = true
		primaries = append(primaries, h)
	}

	// Preserve all selected primary hits and their citation metadata before
	// adding optional narrative neighbors. Otherwise a neighbor from an earlier
	// hit can suppress a later direct retrieval result at the same location.
	for _, h := range primaries {
		results = append(results, searchResult{
			Score:      h.Score,
			R3Score:    h.R3Score,
			Content:    h.Content,
			DocumentID: h.DocumentID,
			ChunkID:    h.ChunkID,
			Citation:   h.Citation,
		})
	}

	neighborKeys := make(map[chunkKey]bool, len(primaryKeys))
	for key := range primaryKeys {
		neighborKeys[key] = true
	}
	for _, h := range primaries {
		if h.ChunkIdx > 0 {
			prevKey := makeChunkKey(h.DocumentID, h.Article, h.ChunkIdx-1)
			if !neighborKeys[prevKey] {
				if prevContent, ok := r.fetchNeighborContent(h.DocumentID, h.Article, h.ChunkIdx-1); ok {
					results = append(results, searchResult{Score: -1, R3Score: -1, Content: prevContent, DocumentID: h.DocumentID})
					neighborKeys[prevKey] = true
				}
			}
		}
		nextKey := makeChunkKey(h.DocumentID, h.Article, h.ChunkIdx+1)
		if !neighborKeys[nextKey] {
			if nextContent, ok := r.fetchNeighborContent(h.DocumentID, h.Article, h.ChunkIdx+1); ok {
				results = append(results, searchResult{Score: -1, R3Score: -1, Content: nextContent, DocumentID: h.DocumentID})
				neighborKeys[nextKey] = true
			}
		}
	}

	return results, nil
}

func candidateLimitForK(k int) int {
	limit := 100
	if k*3 > limit {
		limit = k * 3
	}
	const maxLimit = 1000
	if limit > maxLimit {
		limit = maxLimit
	}
	return limit
}

func (r *ragSystem) searchCandidatesSingle(query string, k int) ([]retrievalHit, int64, int64, error) {
	return r.searchCandidatesSingleContext(context.Background(), query, k)
}

func (r *ragSystem) searchCandidatesSingleContext(ctx context.Context, query string, k int) ([]retrievalHit, int64, int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	t0 := time.Now()
	qvec, err := r.getLM().embedSingle(query)
	if err != nil {
		return nil, 0, 0, err
	}
	embedMs := time.Since(t0).Milliseconds()
	if err := ctx.Err(); err != nil {
		return nil, embedMs, 0, err
	}

	t1 := time.Now()
	rawHits, err := r.chunkStore.searchTopK(qvec, query, r.getActiveEmbedModel(), roleAndACLFilterSQL(activeRole), candidateLimitForK(k))
	if err != nil {
		return nil, embedMs, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, embedMs, time.Since(t1).Milliseconds(), err
	}
	rawHits = r.mergeFullTextCandidates(query, qvec, activeRole, k, rawHits)
	searchMs := time.Since(t1).Milliseconds()

	rankPolicy := defaultRankingPolicy()
	now := time.Now().UTC()
	hits := make([]retrievalHit, 0, len(rawHits))
	for _, h := range rawHits {
		r3Score := rankPolicy.Score(h.Unit, h.rankingScore(), now)
		h.R3Score = r3Score
		h.Citation = buildCitation(h.Unit, r3Score)
		hits = append(hits, h)
	}
	sortHitsDeterministic(hits, func(a, b retrievalHit) bool {
		if a.R3Score != b.R3Score {
			return a.R3Score > b.R3Score
		}
		if a.rankingScore() != b.rankingScore() {
			return a.rankingScore() > b.rankingScore()
		}
		if a.Article != b.Article {
			return a.Article < b.Article
		}
		return a.ChunkIdx < b.ChunkIdx
	})
	hits = r.applyRerankContext(ctx, query, hits)
	return hits, embedMs, searchMs, nil
}

func (r *ragSystem) searchCandidates(query string, k int) ([]retrievalHit, int64, int64, error) {
	return r.searchCandidatesContext(context.Background(), query, k)
}

func (r *ragSystem) searchCandidatesContext(ctx context.Context, query string, k int) ([]retrievalHit, int64, int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	variants := expandRetrievalQueries(query)
	if len(variants) <= 1 {
		return r.searchCandidatesSingleContext(ctx, query, k)
	}

	texts := make([]string, 0, len(variants))
	for _, variant := range variants {
		texts = append(texts, variant.Query)
	}
	t0 := time.Now()
	vecs, err := r.getLM().embed(texts)
	if err != nil {
		return nil, 0, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, time.Since(t0).Milliseconds(), 0, err
	}
	embedMs := time.Since(t0).Milliseconds()

	best := map[string]retrievalHit{}
	var totalSearchMs int64

	rankPolicy := defaultRankingPolicy()
	now := time.Now().UTC()

	for i, vec := range vecs {
		if err := ctx.Err(); err != nil {
			return nil, embedMs, totalSearchMs, err
		}
		if i >= len(variants) {
			break
		}
		t1 := time.Now()
		rawHits, err := r.chunkStore.searchTopK(vec, variants[i].Query, r.getActiveEmbedModel(), roleAndACLFilterSQL(activeRole), candidateLimitForK(k))
		if err != nil {
			return nil, embedMs, totalSearchMs, err
		}
		totalSearchMs += time.Since(t1).Milliseconds()
		if err := ctx.Err(); err != nil {
			return nil, embedMs, totalSearchMs, err
		}

		for rank, h := range rawHits {
			weightedSemantic := h.Score * variants[i].Weight
			h.Score = weightedSemantic
			h.VectorRank = rank + 1
			h.RetrievalScore = weightedSemantic
			key := retrievalHitIdentity(h)
			if prev, ok := best[key]; !ok || h.Score > prev.Score || (h.Score == prev.Score && h.VectorRank < prev.VectorRank) {
				best[key] = h
			}
		}
	}

	hits := make([]retrievalHit, 0, len(best))
	for _, hit := range best {
		hits = append(hits, hit)
	}
	if len(vecs) > 0 {
		t2 := time.Now()
		hits = r.mergeFullTextCandidates(query, vecs[0], activeRole, k, hits)
		totalSearchMs += time.Since(t2).Milliseconds()
	}
	for i := range hits {
		r3Score := rankPolicy.Score(hits[i].Unit, hits[i].rankingScore(), now)
		hits[i].R3Score = r3Score
		hits[i].Citation = buildCitation(hits[i].Unit, r3Score)
	}
	slices.SortFunc(hits, func(a, b retrievalHit) int {
		if a.R3Score != b.R3Score {
			if a.R3Score > b.R3Score {
				return -1
			}
			return 1
		}
		if a.rankingScore() != b.rankingScore() {
			if a.rankingScore() > b.rankingScore() {
				return -1
			}
			return 1
		}
		if a.Article != b.Article {
			return strings.Compare(a.Article, b.Article)
		}
		return a.ChunkIdx - b.ChunkIdx
	})
	hits = r.applyRerankContext(ctx, query, hits)
	return hits, embedMs, totalSearchMs, nil
}

func formatContextChunk(article string, chunkIdx int, content string) string {
	article = strings.TrimSpace(article)
	if article == "" {
		article = "unknown"
	}
	return fmt.Sprintf("[Quelle: %s | Chunk: %d]\n%s", article, chunkIdx, strings.TrimSpace(content))
}

func formatContextChunkWithCitation(article string, chunkIdx int, content string, citation Citation) string {
	article = strings.TrimSpace(article)
	if article == "" {
		article = "unknown"
	}
	title := strings.TrimSpace(citation.Title)
	if title == "" {
		title = article
	}
	stale := ""
	if citation.Stale {
		stale = " | stale"
	}
	link := ""
	if citation.SourceURL != "" {
		link = " | URL: " + citation.SourceURL
	}
	updated := citation.UpdatedAt
	if updated == "" {
		updated = "unknown"
	}
	return fmt.Sprintf("[Quelle: %s | System: %s | Typ: %s | Updated: %s | Trust: %.2f | R3: %.3f%s%s | Chunk: %d]\n%s",
		title,
		citation.SourceSystem,
		citation.SourceType,
		updated,
		citation.TrustLevel,
		citation.R3Score,
		stale,
		link,
		chunkIdx,
		strings.TrimSpace(content),
	)
}

func (r *ragSystem) loadArticleContext(article string, debug bool, embedMs int64) (string, *debugInfo, bool) {
	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}

	rows, err := r.chunkStore.loadArticleChunks(article, roleAndACLFilterSQL(activeRole))
	if err != nil || len(rows) == 0 {
		return "", nil, false
	}

	budget := newContextBudget(assembledContextBudgetChars)
	var dbgChunks []debugChunk
	var citations []Citation
	resolvedArticle := article

	getStr := func(row map[string]any, key string) string {
		if v, ok := row[key]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	getFloat := func(row map[string]any, key string, fb float64) float64 {
		if v, ok := row[key]; ok && v != nil {
			switch tv := v.(type) {
			case float64:
				return tv
			case int:
				return float64(tv)
			case int64:
				return float64(tv)
			}
		}
		return fb
	}
	getInt := func(row map[string]any, key string) int {
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
		return 0
	}
	getBool := func(row map[string]any, key string, fb bool) bool {
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
				return strings.TrimSpace(tv) != "0" && !strings.EqualFold(strings.TrimSpace(tv), "false")
			}
		}
		return fb
	}

	// An exact title can belong to more than one source document. In that
	// ambiguous case, fall back to ranked retrieval rather than injecting an
	// unranked mixture of unrelated same-titled documents.
	sourceIDs := make(map[string]bool)
	for _, row := range rows {
		documentID := strings.TrimSpace(getStr(row, "document_id"))
		if documentID == "" {
			documentID = "legacy:" + strings.ToLower(strings.TrimSpace(getStr(row, "article")))
		}
		sourceIDs[documentID] = true
		if len(sourceIDs) > 1 {
			return "", nil, false
		}
	}

	for _, row := range rows {
		content := getStr(row, "content")
		if content == "" {
			continue
		}
		if artVal := getStr(row, "article"); artVal != "" {
			resolvedArticle = artVal
		}
		idx := getInt(row, "chunk_idx")
		openLinkAllowed := getBool(row, "open_link_allowed", true)
		trust := getFloat(row, "trust_level", 0.5)
		chunkID := getStr(row, "chunk_id")
		documentID := getStr(row, "document_id")
		if documentID == "" {
			documentID = stableContentHash(resolvedArticle)
		}
		citation := Citation{
			ChunkID:       chunkID,
			DocumentID:    documentID,
			Title:         resolvedArticle,
			SourceSystem:  "tinyrag",
			SourceType:    normalizeR3SourceType(resolvedArticle),
			UpdatedAt:     getStr(row, "updated_at"),
			TrustLevel:    trust,
			Sensitivity:   "internal",
			R3Score:       0.75,
			OpenLinkAllow: openLinkAllowed,
		}
		if ss := getStr(row, "source_system"); ss != "" {
			citation.SourceSystem = ss
		}
		if st := getStr(row, "source_type"); st != "" {
			citation.SourceType = st
		}
		if stitle := getStr(row, "source_title"); stitle != "" {
			citation.Title = stitle
		}
		if openLinkAllowed {
			citation.SourceURL = getStr(row, "source_url")
		}
		if sens := getStr(row, "sensitivity"); sens != "" {
			citation.Sensitivity = sens
		}
		part := formatContextChunkWithCitation(resolvedArticle, idx, content, citation)
		if !budget.append(part, true) {
			continue
		}
		citations = append(citations, citation)
		if debug {
			dbgChunks = append(dbgChunks, debugChunk{
				Score:         citation.R3Score,
				SemanticScore: -1,
				R3Score:       citation.R3Score,
				Content:       content,
				Article:       resolvedArticle,
				ChunkIdx:      idx,
				Citation:      citation,
				IsNeighbor:    false,
			})
		}
	}
	di := &debugInfo{
		Chunks:           dbgChunks,
		Citations:        citations,
		EmbedMs:          embedMs,
		SearchMs:         0,
		TotalChunks:      r.docCountForRole(activeRole),
		UsedK:            r.k,
		Decision:         "article_specific",
		RankingModel:     "r3_weighted",
		ContextTruncated: budget.truncated,
	}
	contextText := budget.text()
	if contextText == "" {
		return "", nil, false
	}
	return contextText, di, true
}

func (r *ragSystem) assembleContext(hits []retrievalHit, usedK int, decision string, embedMs, searchMs int64) (string, *debugInfo, error) {
	seen := make(map[chunkKey]bool)
	primaryKeys := make(map[chunkKey]bool, len(hits))
	for _, hit := range hits {
		primaryKeys[makeChunkKey(hit.DocumentID, hit.Article, hit.ChunkIdx)] = true
	}
	budget := newContextBudget(assembledContextBudgetChars)
	var dbgChunks []debugChunk
	var citations []Citation

	appendChunk := func(documentID, article string, idx int, content string, score float64, semantic float64, citation Citation, isNeighbor bool) bool {
		key := makeChunkKey(documentID, article, idx)
		if seen[key] {
			return false
		}
		var part string
		if citation.Title != "" || citation.DocumentID != "" {
			part = formatContextChunkWithCitation(article, idx, content, citation)
		} else {
			part = formatContextChunk(article, idx, content)
		}
		if !budget.append(part, !isNeighbor) {
			return false
		}
		seen[key] = true
		if citation.Title != "" || citation.DocumentID != "" {
			citations = append(citations, citation)
		}
		dbgChunks = append(dbgChunks, debugChunk{
			Score:         score,
			SemanticScore: semantic,
			R3Score:       score,
			Content:       content,
			Article:       article,
			ChunkIdx:      idx,
			Citation:      citation,
			IsNeighbor:    isNeighbor,
		})
		return true
	}

	// Pack direct evidence first. This prevents a long adjacent chunk from
	// crowding out the ranked primary hit that justifies a citation.
	includedPrimaries := make([]retrievalHit, 0, len(hits))
	for _, h := range hits {
		if appendChunk(h.DocumentID, h.Article, h.ChunkIdx, h.Content, h.R3Score, h.Score, h.Citation, false) {
			includedPrimaries = append(includedPrimaries, h)
		}
	}

	// Add context only around retained primary evidence. If an adjacent chunk is
	// itself a primary candidate, its own citation and ranking take precedence.
	for _, h := range includedPrimaries {
		if h.ChunkIdx > 0 {
			prevKey := makeChunkKey(h.DocumentID, h.Article, h.ChunkIdx-1)
			if !primaryKeys[prevKey] {
				if prevContent, ok := r.fetchNeighborContent(h.DocumentID, h.Article, h.ChunkIdx-1); ok {
					appendChunk(h.DocumentID, h.Article, h.ChunkIdx-1, prevContent, -1, -1, Citation{}, true)
				}
			}
		}
		nextKey := makeChunkKey(h.DocumentID, h.Article, h.ChunkIdx+1)
		if primaryKeys[nextKey] {
			continue
		}
		if nextContent, ok := r.fetchNeighborContent(h.DocumentID, h.Article, h.ChunkIdx+1); ok {
			appendChunk(h.DocumentID, h.Article, h.ChunkIdx+1, nextContent, -1, -1, Citation{}, true)
		}
	}

	activeRole := "it"
	if settings != nil {
		activeRole = settings.get().ActiveRole
	}
	di := &debugInfo{
		Chunks:           dbgChunks,
		Citations:        citations,
		EmbedMs:          embedMs,
		SearchMs:         searchMs,
		TotalChunks:      r.docCountForRole(activeRole),
		UsedK:            usedK,
		Decision:         decision,
		RankingModel:     "r3_weighted",
		ContextTruncated: budget.truncated,
	}
	return budget.text(), di, nil
}

// ── Tool / API definitions ─────────────────────────────────────────

// toolDef describes a built-in or custom tool available to the assistant.
