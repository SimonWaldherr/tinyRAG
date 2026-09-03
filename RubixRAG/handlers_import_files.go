package main

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
)

// handleUpload accepts one or more files under form field "file" and
// ingests each through the generic multi-format pipeline (extract.go).
func handleUpload(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if err := r.ParseMultipartForm(200 << 20); err != nil {
			writeJSONError(w, "parse form: "+err.Error(), http.StatusBadRequest)
			return
		}
		files := r.MultipartForm.File["file"]
		if len(files) == 0 {
			writeJSONError(w, "missing file(s)", http.StatusBadRequest)
			return
		}
		keepOriginal := r.FormValue("keep_original") == "1"
		dryRun := r.FormValue("dry_run") == "1"
		s := settings.get()
		results := make([]ingestOutcome, 0, len(files))
		for _, header := range files {
			results = append(results, ingestUploadFile(rag, s, header, keepOriginal, dryRun))
		}
		logAudit(r, "import", fmt.Sprintf("connector=upload %s", summarizeIngestOutcomes(results, dryRun)))
		writeJSON(w, results)
	}
}

// ingestUploadFile keeps per-file errors isolated: one bad attachment does
// not discard successful files from the same multipart request.
func ingestUploadFile(rag *ragSystem, s appSettings, header *multipart.FileHeader, keepOriginal, dryRun bool) ingestOutcome {
	file, err := header.Open()
	if err != nil {
		return ingestOutcome{SourceName: header.Filename, Error: err.Error()}
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return ingestOutcome{SourceName: header.Filename, Error: err.Error()}
	}
	out, err := ingestUploadedFile(rag, s, s.activeEmbedModel(), header.Filename, data, keepOriginal, dryRun)
	if err != nil {
		out.Error = err.Error()
	}
	return out
}

type importFolderRequest struct {
	Path   string `json:"path"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// handleImportFolder walks a server-local directory selected by an admin.
func handleImportFolder(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req importFolderRequest
		if err := decodeJSONBody(r, &req); err != nil || strings.TrimSpace(req.Path) == "" {
			writeJSONError(w, "missing path", http.StatusBadRequest)
			return
		}
		info, err := os.Stat(req.Path)
		if err != nil || !info.IsDir() {
			writeJSONError(w, "not a directory", http.StatusBadRequest)
			return
		}
		s := settings.get()
		results, err := ingestFolder(r.Context(), rag, s, s.activeEmbedModel(), req.Path, 0, req.DryRun)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(r, "import", fmt.Sprintf("connector=folder path=%s %s", req.Path, summarizeIngestOutcomes(results, req.DryRun)))
		writeJSON(w, results)
	}
}

type importSelfSourceRequest struct {
	Root   string `json:"root,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// handleImportSelfSource ingests R3's own source tree into its own vector
// store (selfsource.go) — admin-only (requireAdminSession, see the route
// registration in handlers.go) and always an explicit, one-off admin
// action, never automatic on startup or on any schedule. Root defaults to
// "." (the server process's own working directory, which every documented
// way of running R3 — `go run .`/`./R3` from the repo root, see README.md's
// "Quick start" — makes the repo root); an admin can override it only for
// an unusual layout, it is never persisted to settings.json.
func handleImportSelfSource(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req importSelfSourceRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		s := settings.get()
		results, err := ingestR3Source(r.Context(), rag, s, s.activeEmbedModel(), req.Root, req.DryRun)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(r, "import", fmt.Sprintf("connector=self_source %s", summarizeIngestOutcomes(results, req.DryRun)))
		writeJSON(w, results)
	}
}

// summarizeIngestOutcomes renders a short audit detail string across a
// batch of ingestOutcome rows (handleUpload/handleImportFolder, which use
// this per-file shape rather than baseImportResult) — total chunk count
// and how many entries recorded an error, never file names/content.
func summarizeIngestOutcomes(results []ingestOutcome, dryRun bool) string {
	chunks, errCount := 0, 0
	for _, o := range results {
		chunks += o.Chunks
		if o.Error != "" {
			errCount++
		}
	}
	return fmt.Sprintf("files=%d chunks=%d errors=%d dry_run=%v", len(results), chunks, errCount, dryRun)
}
