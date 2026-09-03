package main

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func handleSources(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := rag.listSources()
		if err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		mappings := settings.get().URLMappings
		for i := range sources {
			sources[i].SourceURL = resolveSourceURL(sources[i].SourceID, mappings)
		}
		writeJSON(w, sources)
	}
}

type sourceDeleteRequest struct {
	SourceID string `json:"source_id"`
}

// handleSourceDelete removes a single source (and its chunks) by
// source_id, persisting the change immediately via rag.save() so the
// deletion survives a restart rather than only living in memory.
func handleSourceDelete(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req sourceDeleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SourceID == "" {
			writeJSONError(w, "missing source_id", 400)
			return
		}
		if _, ok := rag.fetchSourceContent(req.SourceID); !ok {
			writeJSONError(w, "source not found", 404)
			return
		}
		if err := rag.deleteSource(req.SourceID); err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		if err := rag.save(); err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		logAudit(r, "source_delete", fmt.Sprintf("source_id=%s", req.SourceID))
		writeJSON(w, map[string]bool{"ok": true})
	}
}

type sourceRefreshRequest struct {
	SourceID string `json:"source_id"`
}

// handleSourceRefresh re-fetches ONE existing source's content from its
// original system (sourcerefresh.go's refreshSource — currently the three
// SharePoint source_kinds only) and replaces its chunks if the content
// actually changed, without re-running that connector's whole import or
// delta-sync. 404 for an unknown source_id, 400 for a kind refreshSource
// doesn't support (yet) or a live fetch that failed (site/file no longer
// reachable, connection disabled, ...).
func handleSourceRefresh(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req sourceRefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SourceID == "" {
			writeJSONError(w, "missing source_id", 400)
			return
		}
		kind, ok := rag.fetchSourceKind(req.SourceID)
		if !ok {
			writeJSONError(w, "source not found", 404)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		outcome, err := refreshSource(ctx, rag, settings.get(), req.SourceID, kind)
		if err != nil {
			writeJSONError(w, err.Error(), 400)
			return
		}
		logAudit(r, "source_refresh", fmt.Sprintf("source_id=%s kind=%s chunks=%d skipped=%v", req.SourceID, kind, outcome.Chunks, outcome.Skipped))
		writeJSON(w, map[string]any{"ok": true, "chunks": outcome.Chunks, "skipped": outcome.Skipped})
	}
}

type sourceDeleteByKindRequest struct {
	SourceKind string `json:"source_kind"`
}

// handleSourceDeleteByKind deletes every source of one kind (e.g. every
// "pst_email", regardless of which import produced it).
func handleSourceDeleteByKind(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req sourceDeleteByKindRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SourceKind == "" {
			writeJSONError(w, "missing source_kind", 400)
			return
		}
		n, err := rag.deleteSourcesByKind(req.SourceKind)
		if err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		if err := rag.save(); err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		logAudit(r, "source_delete_by_kind", fmt.Sprintf("source_kind=%s deleted=%d", req.SourceKind, n))
		writeJSON(w, map[string]any{"ok": true, "deleted": n})
	}
}

type sourceDeleteByFilterRequest struct {
	SourceKind string `json:"source_kind,omitempty"`
	Extension  string `json:"extension,omitempty"`
	Query      string `json:"query,omitempty"`
	// DryRun counts what WOULD be deleted (server-side, current store
	// state) without deleting anything — the UI calls this first and puts
	// the returned number in its confirmation dialog, so the admin
	// confirms the server's count rather than a possibly stale client-side
	// row count. Same request-level dry-run convention as every import
	// endpoint.
	DryRun bool `json:"dry_run,omitempty"`
}

// handleSourceDeleteByFilter deletes every source matching any combination
// of an exact source_kind, a file extension (source_name suffix, e.g.
// ".pdf"), and a free-text substring (source_name or source_id) — the
// granular alternative to delete-by-kind/delete-by-prefix above, for
// "delete every PDF" or "delete everything matching this customer name"
// without deleting an entire source_kind or PST-file prefix in one go. At
// least one filter field is required so an empty/malformed request can
// never wipe every source (see sourceFilter's doc comment in store.go).
func handleSourceDeleteByFilter(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req sourceDeleteByFilterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "invalid body: "+err.Error(), 400)
			return
		}
		ext := strings.ToLower(strings.TrimSpace(req.Extension))
		if ext != "" && !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		f := sourceFilter{
			Kind:      strings.TrimSpace(req.SourceKind),
			Extension: ext,
			Query:     strings.ToLower(strings.TrimSpace(req.Query)),
		}
		if f.Kind == "" && f.Extension == "" && f.Query == "" {
			writeJSONError(w, "at least one filter (source_kind, extension, query) is required", 400)
			return
		}
		if req.DryRun {
			n, err := rag.countSourcesByFilter(f)
			if err != nil {
				writeJSONError(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true, "matched": n, "dry_run": true})
			return
		}
		n, err := rag.deleteSourcesByFilter(f)
		if err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		if err := rag.save(); err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		logAudit(r, "source_delete_by_filter", fmt.Sprintf("source_kind=%s extension=%s query=%q deleted=%d", f.Kind, f.Extension, f.Query, n))
		writeJSON(w, map[string]any{"ok": true, "deleted": n})
	}
}

type sourceDeleteByPrefixRequest struct {
	Prefix string `json:"prefix"`
}

// handleSourceDeleteByPrefix deletes every source whose source_id starts
// with prefix — e.g. "pst:Postfach.pst:" deletes just that one PST import
// as a block, leaving other PST imports and other source kinds untouched.
func handleSourceDeleteByPrefix(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req sourceDeleteByPrefixRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prefix == "" {
			writeJSONError(w, "missing prefix", 400)
			return
		}
		n, err := rag.deleteSourcesByPrefix(req.Prefix)
		if err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		if err := rag.save(); err != nil {
			writeJSONError(w, err.Error(), 500)
			return
		}
		logAudit(r, "source_delete_by_prefix", fmt.Sprintf("prefix=%s deleted=%d", req.Prefix, n))
		writeJSON(w, map[string]any{"ok": true, "deleted": n})
	}
}

// sourceAccessAllowedForRequest reports whether the requester (identified by
// whatever session cookie, if any, came with r — anonymous if none) may see
// sourceID's content under s.SourceAccess. Shared by handleSourceContent,
// handleSourceOriginal and handleDraftReply so a source_id passed straight
// from the client can never bypass the same department check rankedSearch
// already applies (rank.go's filterByDeptAccess) — content/original/draft-
// reply stay reachable without a login (see registerRoutes' comment) but
// must not thereby become a way around source_access. false also covers
// "no such source", so callers can return one generic 404 either way
// without revealing which case it was.
func sourceAccessAllowedForRequest(r *http.Request, s appSettings, rag *ragSystem, sourceID string) bool {
	kind, ok := rag.fetchSourceKind(sourceID)
	if !ok {
		return false
	}
	deptCode := ""
	user := ""
	if claims, ok := currentSession(r); ok {
		deptCode = resolveDeptCode(claims.IsAdmin, claims.DeptCode)
		user = sessionActor(claims)
	}
	return rag.sourceAccessAllowed(s.SourceAccess, sourceID, kind, deptCode, user)
}

// handleSourceContent powers the citation popup: given a source_id (any
// kind — PST email, uploaded file, folder-imported file, ...), returns its
// full extracted text (every chunk, stitched back into original order —
// see ragSystem.fetchSourceContent) plus whether an original-file download
// is available for it. No session is required, but source_access is still
// enforced (sourceAccessAllowedForRequest) — a department-restricted
// source_kind returns 404 for a requester outside it, same as it would
// never have surfaced as a citation in the first place.
func handleSourceContent(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sourceID := r.URL.Query().Get("source_id")
		if sourceID == "" {
			writeJSONError(w, "missing source_id", 400)
			return
		}
		s := settings.get()
		if !sourceAccessAllowedForRequest(r, s, rag, sourceID) {
			writeJSONError(w, "source not found", 404)
			return
		}
		content, ok := rag.fetchSourceContent(sourceID)
		if !ok {
			writeJSONError(w, "source not found", 404)
			return
		}
		_, err := os.Stat(originalFilePath(originalsDirOrDefault(s), sourceID))
		writeJSON(w, map[string]any{
			"source_id":             sourceID,
			"content":               content,
			"has_original":          err == nil,
			"draft_replies_enabled": s.EnableDraftReplies,
		})
	}
}

// handleSourceOriginal streams the original file for sourceID back if the
// upload that created it asked to keep one (see the "Original behalten"
// checkbox / handleUpload's keep_original field); 404 otherwise, including
// for source kinds (PST emails, folder imports) that never have one, or one
// source_access restricts away from the requester (see handleSourceContent).
func handleSourceOriginal(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sourceID := r.URL.Query().Get("source_id")
		if sourceID == "" {
			writeJSONError(w, "missing source_id", 400)
			return
		}
		s := settings.get()
		if !requireSessionIfLDAP(w, r, s) {
			return
		}
		if !sourceAccessAllowedForRequest(r, s, rag, sourceID) {
			writeJSONError(w, "no original file kept for this source", 404)
			return
		}
		path := originalFilePath(originalsDirOrDefault(s), sourceID)
		if _, err := os.Stat(path); err != nil {
			writeJSONError(w, "no original file kept for this source", 404)
			return
		}
		filename := filepath.Base(strings.TrimPrefix(sourceID, "upload:"))
		ct := mime.TypeByExtension(filepath.Ext(filename))
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		http.ServeFile(w, r, path)
	}
}

// originalsDirOrDefault returns where kept original uploads live on disk,
// falling back to "r3-originals" so handleSourceOriginal has somewhere to
// look even on a fresh config that never set Import.OriginalsDir.
func originalsDirOrDefault(s appSettings) string {
	if s.Import.OriginalsDir != "" {
		return s.Import.OriginalsDir
	}
	return "r3-originals"
}
