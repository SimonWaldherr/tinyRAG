package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type ragPullSource struct {
	ID              string           `json:"id"`
	Kind            string           `json:"kind"`
	Path            string           `json:"path"`
	Recursive       bool             `json:"recursive"`
	Enabled         bool             `json:"enabled"`
	IntervalSeconds int              `json:"interval_seconds"`
	EmbedModel      string           `json:"embed_model,omitempty"`
	Roles           []string         `json:"roles,omitempty"`
	Metadata        R3IngestMetadata `json:"metadata,omitempty"`
}

type ragIngestDocument struct {
	ID         string           `json:"id,omitempty"`
	Source     string           `json:"source,omitempty"`
	Title      string           `json:"title,omitempty"`
	Text       string           `json:"text"`
	EmbedModel string           `json:"embed_model,omitempty"`
	Roles      []string         `json:"roles,omitempty"`
	Metadata   R3IngestMetadata `json:"metadata,omitempty"`
}

type ragPushIngestRequest struct {
	Source     string              `json:"source,omitempty"`
	Title      string              `json:"title,omitempty"`
	Text       string              `json:"text,omitempty"`
	EmbedModel string              `json:"embed_model,omitempty"`
	Roles      []string            `json:"roles,omitempty"`
	Metadata   R3IngestMetadata    `json:"metadata,omitempty"`
	Documents  []ragIngestDocument `json:"documents,omitempty"`
}

type ragIngestDocumentResult struct {
	Source      string `json:"source"`
	Title       string `json:"title"`
	DocumentID  string `json:"document_id"`
	Status      string `json:"status"`
	Chars       int    `json:"chars"`
	Chunks      int    `json:"chunks"`
	Redactions  int    `json:"redactions"`
	ContentHash string `json:"content_hash"`
	Error       string `json:"error,omitempty"`
}

type ragFolderScanRequest struct {
	Path       string           `json:"path"`
	Recursive  bool             `json:"recursive"`
	EmbedModel string           `json:"embed_model"`
	Roles      []string         `json:"roles"`
	Metadata   R3IngestMetadata `json:"metadata"`
}

type ragFolderScanResult struct {
	SourceID     string                    `json:"source_id,omitempty"`
	Path         string                    `json:"path"`
	FilesSeen    int                       `json:"files_seen"`
	FilesChanged int                       `json:"files_changed"`
	FilesSkipped int                       `json:"files_skipped"`
	FilesErrored int                       `json:"files_errored"`
	TotalChars   int                       `json:"total_chars"`
	TotalChunks  int                       `json:"total_chunks"`
	Results      []ragIngestDocumentResult `json:"results"`
	Errors       []string                  `json:"errors,omitempty"`
}

func normalizeIngestUpdateMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "upsert", "update":
		return "upsert"
	case "replace", "overwrite":
		return "replace"
	case "skip", "skip_existing", "":
		return "skip"
	default:
		return "skip"
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func documentFingerprintFromChunks(chunks []string) string {
	parts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		parts = append(parts, stableContentHash(chunk))
	}
	return stableContentHash(strings.Join(parts, "|"))
}

func (r *ragSystem) documentContentHash(documentID, roleScope string) (string, bool, error) {
	q := fmt.Sprintf(
		"SELECT content_hash FROM chunks WHERE document_id = '%s' AND role_scope = '%s' ORDER BY chunk_idx",
		escapeSQ(documentID), escapeSQ(roleScope),
	)
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return "", false, err
	}
	r.dbMu.Lock()
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
	if err != nil {
		return "", false, err
	}
	if rs == nil || len(rs.Rows) == 0 {
		return "", false, nil
	}
	parts := make([]string, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		if v, ok := tinysql.GetVal(row, "content_hash"); ok && v != nil {
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return stableContentHash(strings.Join(parts, "|")), true, nil
}

func (r *ragSystem) deleteDocumentChunks(documentID, roleScope string) error {
	q := fmt.Sprintf(
		"DELETE FROM chunks WHERE document_id = '%s' AND role_scope = '%s'",
		escapeSQ(documentID), escapeSQ(roleScope),
	)
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return err
	}
	r.dbMu.Lock()
	_, err = tinysql.Execute(context.Background(), r.db, "default", stmt)
	r.dbMu.Unlock()
	if err != nil {
		return err
	}
	return r.save()
}

func mergeR3Metadata(base, override R3IngestMetadata) R3IngestMetadata {
	out := base
	if override.DocumentID != "" {
		out.DocumentID = override.DocumentID
	}
	if override.SourceSystem != "" {
		out.SourceSystem = override.SourceSystem
	}
	if override.SourceType != "" {
		out.SourceType = override.SourceType
	}
	if override.SourceTitle != "" {
		out.SourceTitle = override.SourceTitle
	}
	if override.SourceURL != "" {
		out.SourceURL = override.SourceURL
	}
	if override.SourceObjectID != "" {
		out.SourceObjectID = override.SourceObjectID
	}
	if override.SourceVersion != "" {
		out.SourceVersion = override.SourceVersion
	}
	if override.ACLGroups != "" {
		out.ACLGroups = override.ACLGroups
	}
	if override.BusinessOwner != "" {
		out.BusinessOwner = override.BusinessOwner
	}
	if override.Sensitivity != "" {
		out.Sensitivity = override.Sensitivity
	}
	if override.TrustLevel > 0 {
		out.TrustLevel = override.TrustLevel
	}
	if override.SourceQuality > 0 {
		out.SourceQuality = override.SourceQuality
	}
	if override.FreshnessScore > 0 {
		out.FreshnessScore = override.FreshnessScore
	}
	if override.QualityScore > 0 {
		out.QualityScore = override.QualityScore
	}
	if override.FeedbackScore > 0 {
		out.FeedbackScore = override.FeedbackScore
	}
	if !override.ImportedAt.IsZero() {
		out.ImportedAt = override.ImportedAt
	}
	if !override.UpdatedAt.IsZero() {
		out.UpdatedAt = override.UpdatedAt
	}
	if override.OpenLinkAllowedSet {
		out.OpenLinkAllowed = override.OpenLinkAllowed
		out.OpenLinkAllowedSet = true
	}
	if override.Provenance != "" {
		out.Provenance = override.Provenance
	}
	if override.Ownership != "" {
		out.Ownership = override.Ownership
	}
	if override.TrustTier != "" {
		out.TrustTier = override.TrustTier
	}
	if override.Lifecycle != "" {
		out.Lifecycle = override.Lifecycle
	}
	if override.RetentionPolicy != "" {
		out.RetentionPolicy = override.RetentionPolicy
	}
	if override.UpdateMode != "" {
		out.UpdateMode = override.UpdateMode
	}
	return out
}

func ingestDocument(rag *ragSystem, doc ragIngestDocument, fallbackEmbedModel string, fallbackRoles []string, s appSettings) ragIngestDocumentResult {
	source := strings.TrimSpace(doc.Source)
	if source == "" {
		source = strings.TrimSpace(doc.ID)
	}
	if source == "" {
		source = strings.TrimSpace(doc.Title)
	}
	title := strings.TrimSpace(doc.Title)
	if title == "" {
		title = source
	}
	if source == "" {
		source = "push:" + stableContentHash(doc.Text)[:16]
	}
	if title == "" {
		title = source
	}
	meta := doc.Metadata
	if meta.SourceTitle == "" {
		meta.SourceTitle = title
	}
	if meta.SourceObjectID == "" {
		meta.SourceObjectID = source
	}
	if meta.DocumentID == "" {
		meta.DocumentID = stableContentHash(source)
	}
	if meta.UpdateMode == "" {
		meta.UpdateMode = "upsert"
	}
	if meta.SourceSystem == "" {
		meta.SourceSystem = "push"
	}
	if meta.SourceType == "" {
		meta.SourceType = normalizeR3SourceType(source)
	}
	if meta.Provenance == "" {
		meta.Provenance = source
	}
	if meta.OpenLinkAllowedSet == false && meta.SourceURL != "" {
		meta.OpenLinkAllowed = true
		meta.OpenLinkAllowedSet = true
	}
	embedModel := strings.TrimSpace(doc.EmbedModel)
	if embedModel == "" {
		embedModel = fallbackEmbedModel
	}
	roles := doc.Roles
	if len(roles) == 0 {
		roles = fallbackRoles
	}
	chunks, redactions := chunksForIngestWithDoc(doc.Text, s, meta.DocumentID, false)
	writeRes, err := rag.addChunksWithMetadataResult(title, chunks, embedModel, roles, meta)
	out := ragIngestDocumentResult{
		Source:      source,
		Title:       title,
		DocumentID:  meta.DocumentID,
		Chars:       len(doc.Text),
		Chunks:      writeRes.Chunks,
		Redactions:  redactions,
		Status:      writeRes.Status,
		ContentHash: writeRes.ContentHash,
	}
	if writeRes.DocumentID != "" {
		out.DocumentID = writeRes.DocumentID
	}
	if err != nil {
		out.Status = "error"
		out.Error = err.Error()
	}
	return out
}

func scanDirectoryIntoRAG(rag *ragSystem, req ragFolderScanRequest, sourceID string) ragFolderScanResult {
	s := settings.get()
	root := filepath.Clean(req.Path)
	result := ragFolderScanResult{SourceID: sourceID, Path: root}
	embedModel := strings.TrimSpace(req.EmbedModel)
	if embedModel == "" {
		embedModel = s.EmbedModel
	}
	roles := normalizeRoleScopes(req.Roles, s.ActiveRole)
	baseMeta := req.Metadata
	if baseMeta.SourceSystem == "" {
		baseMeta.SourceSystem = "pull:folder"
	}
	if sourceID != "" {
		baseMeta.SourceSystem = "pull:folder:" + sourceID
	}
	if baseMeta.SourceType == "" {
		baseMeta.SourceType = "document"
	}
	if baseMeta.UpdateMode == "" {
		baseMeta.UpdateMode = "upsert"
	}
	if baseMeta.RetentionPolicy == "" {
		baseMeta.RetentionPolicy = "source-sync"
	}
	if baseMeta.OpenLinkAllowedSet == false {
		baseMeta.OpenLinkAllowed = false
		baseMeta.OpenLinkAllowedSet = true
	}

	walkFn := func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors = append(result.Errors, path+": "+walkErr.Error())
			result.FilesErrored++
			return nil
		}
		if d.IsDir() {
			if !req.Recursive && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		text, err := readFileForRAG(path, 5*1024*1024)
		if err != nil {
			return nil
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		result.FilesSeen++
		info, _ := d.Info()
		relPath, _ := filepath.Rel(root, path)
		if relPath == "" || strings.HasPrefix(relPath, "..") {
			relPath = filepath.Base(path)
		}
		source := "folder:" + relPath
		if sourceID != "" {
			source = "pull:" + sourceID + ":" + relPath
		}
		meta := baseMeta
		meta.DocumentID = stableContentHash("folder|" + root + "|" + relPath)
		meta.SourceTitle = relPath
		meta.SourceObjectID = path
		meta.Provenance = path
		if info != nil {
			meta.UpdatedAt = info.ModTime().UTC()
			meta.SourceVersion = fmt.Sprintf("%d:%d", info.ModTime().Unix(), info.Size())
		}
		doc := ragIngestDocument{
			Source:     source,
			Title:      relPath,
			Text:       text,
			EmbedModel: embedModel,
			Roles:      roles,
			Metadata:   meta,
		}
		docRes := ingestDocument(rag, doc, embedModel, roles, s)
		result.Results = append(result.Results, docRes)
		result.TotalChars += docRes.Chars
		result.TotalChunks += docRes.Chunks
		switch docRes.Status {
		case "inserted", "updated":
			result.FilesChanged++
		case "error":
			result.FilesErrored++
			result.Errors = append(result.Errors, relPath+": "+docRes.Error)
		default:
			result.FilesSkipped++
		}
		return nil
	}
	if err := filepath.WalkDir(root, walkFn); err != nil {
		result.Errors = append(result.Errors, err.Error())
		result.FilesErrored++
	}
	return result
}

func startPullScheduler(ctx context.Context, rag *ragSystem, ss *settingsStore) {
	if ss == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		lastRun := map[string]time.Time{}
		runDue := func() {
			now := time.Now().UTC()
			for _, src := range ss.get().PullSources {
				src := normalizePullSource(src)
				if !src.Enabled || src.Kind != "folder" || src.Path == "" {
					continue
				}
				interval := time.Duration(src.IntervalSeconds) * time.Second
				if interval < time.Minute {
					interval = time.Minute
				}
				if prev, ok := lastRun[src.ID]; ok && now.Sub(prev) < interval {
					continue
				}
				lastRun[src.ID] = now
				log.Printf("pull source %s: scanning %s (interval %s)", src.ID, src.Path, interval)
				res := scanDirectoryIntoRAG(rag, ragFolderScanRequest{
					Path:       src.Path,
					Recursive:  src.Recursive,
					EmbedModel: src.EmbedModel,
					Roles:      src.Roles,
					Metadata:   src.Metadata,
				}, src.ID)
				log.Printf("pull source %s: seen=%d changed=%d skipped=%d errors=%d", src.ID, res.FilesSeen, res.FilesChanged, res.FilesSkipped, res.FilesErrored)
			}
		}
		runDue()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runDue()
			}
		}
	}()
}

func normalizePullSource(src ragPullSource) ragPullSource {
	src.ID = strings.TrimSpace(src.ID)
	if src.ID == "" {
		src.ID = stableContentHash(src.Kind + "|" + src.Path)[:12]
	}
	src.Kind = strings.ToLower(strings.TrimSpace(src.Kind))
	if src.Kind == "" {
		src.Kind = "folder"
	}
	if src.IntervalSeconds <= 0 {
		src.IntervalSeconds = 300
	}
	if src.IntervalSeconds < 60 {
		src.IntervalSeconds = 60
	}
	src.Roles = normalizeRoleScopes(src.Roles, "it")
	if src.Metadata.UpdateMode == "" {
		src.Metadata.UpdateMode = "upsert"
	}
	return src
}

func encodeR3MetadataForAudit(meta R3IngestMetadata) string {
	b, err := json.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(b)
}
