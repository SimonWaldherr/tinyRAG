package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// stagePSTUpload reads the "file" multipart field to a fresh temp dir and
// returns its path — shared by handleImportPSTPreview (the only way to get
// a staging ID) since importing always goes through a preview first.
func stagePSTUpload(r *http.Request) (string, error) {
	if err := r.ParseMultipartForm(2 << 30); err != nil {
		return "", fmt.Errorf("parse form: %w", err)
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return "", fmt.Errorf("missing file: %w", err)
	}
	defer file.Close()

	tmpDir, err := os.MkdirTemp("", "r3-pst-")
	if err != nil {
		return "", err
	}
	tmpPath := filepath.Join(tmpDir, filepath.Base(header.Filename))
	out, err := os.Create(tmpPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.RemoveAll(tmpDir)
		return "", err
	}
	out.Close()
	return tmpPath, nil
}

// handleImportPSTPreview stages a PST or OST upload, returns its folder preview, and
// leaves selection/commit for handleImportPST.
func handleImportPSTPreview(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		tmpPath, err := stagePSTUpload(r)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		preview, err := previewPST(tmpPath)
		if err != nil {
			os.RemoveAll(filepath.Dir(tmpPath))
			writeJSONError(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writePSTPreview(w, stagePST(tmpPath, true, preview.Folders), preview)
	}
}

type pstPreviewPathRequest struct {
	Path string `json:"path"`
}

// handleImportPSTPreviewPath is the server-local-file variant for large PSTs.
func handleImportPSTPreviewPath(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req pstPreviewPathRequest
		if err := decodeJSONBody(r, &req); err != nil || strings.TrimSpace(req.Path) == "" {
			writeJSONError(w, "missing path", http.StatusBadRequest)
			return
		}
		info, err := os.Stat(req.Path)
		if err != nil || info.IsDir() {
			writeJSONError(w, "not a file", http.StatusBadRequest)
			return
		}
		preview, err := previewPST(req.Path)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writePSTPreview(w, stagePST(req.Path, false, preview.Folders), preview)
	}
}

// writePSTPreview keeps the upload and server-path preview responses
// identical, including their staging token.
func writePSTPreview(w http.ResponseWriter, stagingID string, preview pstPreviewResult) {
	writeJSON(w, map[string]any{
		"staging_id":     stagingID,
		"file":           preview.File,
		"folders":        preview.Folders,
		"total":          preview.Total,
		"folders_walked": preview.FoldersWalked,
	})
}

type pstImportRequest struct {
	StagingID string   `json:"staging_id"`
	Folders   []string `json:"folders"`
	DryRun    bool     `json:"dry_run,omitempty"`
	Debug     bool     `json:"debug,omitempty"`
}

// handleImportPST starts a detached job after atomically validating and
// consuming the staged preview.
func handleImportPST(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req pstImportRequest
		if err := decodeJSONBody(r, &req); err != nil || req.StagingID == "" {
			writeJSONError(w, "missing staging_id", http.StatusBadRequest)
			return
		}
		staged, ok, err := takeStagedPST(req.StagingID, req.Folders)
		if !ok {
			writeJSONError(w, "unknown or expired staging_id — please reload the PST and preview again", http.StatusNotFound)
			return
		}
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}

		s := settings.get()
		ctx, cancel := context.WithCancel(context.Background())
		job := registerPSTImportJob(filepath.Base(staged.Path), cancel)
		// The job runs detached from this request (it can easily outlive a
		// closed browser tab, see the package comment) — actor/remoteIP are
		// resolved from r here, while it's still available, and carried
		// into the goroutine as plain strings rather than the *http.Request
		// itself.
		actor, remoteIP := actorFromRequest(r), clientKey(r)
		go runPSTImportJob(ctx, cancel, job, rag, s, staged, req, actor, remoteIP)
		writeJSON(w, map[string]string{"job_id": job.id})
	}
}

func runPSTImportJob(ctx context.Context, cancel context.CancelFunc, job *pstImportJob, rag *ragSystem, s appSettings, staged pstStagingEntry, req pstImportRequest, actor, remoteIP string) {
	if staged.Owned {
		defer os.RemoveAll(filepath.Dir(staged.Path))
	}
	defer cancel()
	res, err := importPST(ctx, rag, s, s.activeEmbedModel(), staged.Path, makeSet(req.Folders), req.DryRun, req.Debug, job.updateProgress)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
	}
	logAuditAs(actor, remoteIP, "import", fmt.Sprintf("connector=pst file=%s chunks=%d skipped=%d errors=%d dry_run=%v",
		filepath.Base(staged.Path), res.Chunks, res.Skipped, len(res.Errors), req.DryRun))
	job.finish(res, err)
}

type pstJobIDRequest struct {
	JobID string `json:"job_id"`
}

func handleImportPSTStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := getPSTImportJob(r.URL.Query().Get("job_id"))
	if !ok {
		writeJSONError(w, "unknown or expired job_id", http.StatusNotFound)
		return
	}
	writeJSON(w, job.status())
}

func handleImportPSTCancel(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req pstJobIDRequest
	if err := decodeJSONBody(r, &req); err != nil || req.JobID == "" {
		writeJSONError(w, "missing job_id", http.StatusBadRequest)
		return
	}
	job, ok := getPSTImportJob(req.JobID)
	if !ok {
		writeJSONError(w, "unknown or expired job_id", http.StatusNotFound)
		return
	}
	job.requestCancel()
	writeJSON(w, map[string]bool{"ok": true})
}

func handleImportPSTJobs(w http.ResponseWriter, r *http.Request) {
	jobs := listPSTImportJobs()
	out := make([]pstJobStatusDTO, len(jobs))
	for i, job := range jobs {
		out[i] = job.status()
	}
	writeJSON(w, out)
}
