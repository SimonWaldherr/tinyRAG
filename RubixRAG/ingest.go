package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ingestOutcome reports what happened for one source during an ingest run.
type ingestOutcome struct {
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
	Chunks     int    `json:"chunks"`
	Skipped    bool   `json:"skipped"` // true when content hash was unchanged since the last load
	Redactions int    `json:"redactions"`
	// DryRun mirrors the dryRun flag ingestDocument was called with: true
	// means Chunks is what *would* have been written (extraction/chunking/
	// hash-compare all ran for real) but nothing was actually embedded or
	// stored — see ingestDocument's doc comment.
	DryRun bool   `json:"dry_run,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ingestDocument is the single write path every importer (file upload,
// folder walk, PST email, IMAP/Exchange/Teams/Confluence/Jira/Freshservice/
// web import) funnels through. It:
//  1. skips re-embedding if the extracted text is byte-identical to the
//     last load for this exact source_id (cheap idempotency),
//  2. otherwise deletes every previously stored chunk for that source_id
//     and inserts the freshly chunked, freshly embedded replacement, tagged
//     with a new load_id/loaded_at so citations always show current
//     provenance.
//
// dryRun runs extraction, chunking and the hash-skip comparison exactly as
// normal (so the reported chunk count/skip decision reflect what would
// really happen) but returns before the one step with real side effects —
// rag.replaceSourceChunks, which both calls the embedding API and writes to
// the vector store. Every importer threads its own dryRun flag through to
// here from its own request, so a dry run never touches settings.json or
// any global state — it's a per-call choice, not a mode the server is "in".
func ingestDocument(rag *ragSystem, s appSettings, embedModel string, sourceID, sourceKind, sourceName, text string, docDate int64, dryRun bool) (ingestOutcome, error) {
	out := ingestOutcome{SourceID: sourceID, SourceName: sourceName}
	if strings.TrimSpace(text) == "" {
		return out, fmt.Errorf("no text extracted")
	}

	// Every extractor (markitdown for PDF/DOCX/PPTX/XLSX, htmlToText,
	// rtfToText, plain text, .eml, ...) converges here before chunking — the
	// one place to collapse excessive repeated punctuation/whitespace runs
	// (see collapseRepeatedRuns in extract.go) universally, rather than
	// duplicating that cleanup per extractor. Applied once, at import time,
	// so future embeddings/chunks benefit; content already ingested before
	// this existed is instead cleaned up on the fly at context-assembly time
	// (rank.go's assembleContext/expandEmailFamilies).
	text = collapseRepeatedRuns(text)

	chunks, redactions := chunksForIngest(text, s)
	out.Redactions = redactions

	hash := contentHash(strings.Join(chunks, "\n"))
	skip := hash == rag.lastContentHash(sourceID)
	if verbose {
		log.Printf("[verbose] ingest %s (kind=%s): %d chunk(s), %d redaction(s), skip=%v, dry_run=%v", sourceID, sourceKind, len(chunks), redactions, skip, dryRun)
	}
	if skip {
		out.Skipped = true
		return out, nil
	}

	if dryRun {
		out.Chunks = len(chunks)
		out.DryRun = true
		return out, nil
	}

	n, err := rag.replaceSourceChunks(sourceChunks{
		SourceID:   sourceID,
		SourceKind: sourceKind,
		SourceName: sourceName,
		DocDate:    docDate,
		Chunks:     chunks,
	}, embedModel)
	out.Chunks = n
	if verbose {
		log.Printf("[verbose] ingest %s: wrote %d chunk(s), err=%v", sourceID, n, err)
	}
	return out, err
}

// ingestFile extracts and ingests a single file on disk. source_id is
// derived from the absolute path so re-ingesting the same path (e.g. after
// editing the file) replaces its old chunks rather than duplicating them.
func ingestFile(rag *ragSystem, s appSettings, embedModel, path string, dryRun bool) (ingestOutcome, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	text, err := extractText(path, s)
	if err != nil {
		return ingestOutcome{SourceID: "file:" + abs, SourceName: filepath.Base(path)}, err
	}
	var docDate int64
	if info, statErr := os.Stat(path); statErr == nil {
		docDate = info.ModTime().Unix()
	}
	return ingestDocument(rag, s, embedModel, "file:"+abs, "file", filepath.Base(path), text, docDate, dryRun)
}

// ingestFolder recursively ingests every supported file under root.
// maxItems, if > 0, stops the walk (via filepath.SkipAll — WalkDir then
// returns nil, not an error, same as every other connector's "rest follows
// next run" cap) after that many files have been processed; 0 means
// unbounded, preserving the one-shot manual importer's original behavior.
// ctx is checked between files so a scheduled run can be cancelled/timed
// out like every other connector's import loop.
func ingestFolder(ctx context.Context, rag *ragSystem, s appSettings, embedModel, root string, maxItems int, dryRun bool) ([]ingestOutcome, error) {
	var results []ingestOutcome
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if maxItems > 0 && len(results) >= maxItems {
			return filepath.SkipAll
		}
		ext := fileExtension(path)
		// Images join the walk only when OCR can actually run
		// (AllowShellExec) — otherwise a folder full of photos would
		// produce one noisy "requires allow_shell_exec" error per file
		// instead of being skipped like any other unsupported type.
		if !isExtractableDocument(ext) && !(s.AllowShellExec && imageExtensions()[ext]) {
			return nil
		}
		res, err := ingestFile(rag, s, embedModel, path, dryRun)
		if err != nil {
			res.Error = err.Error()
		}
		results = append(results, res)
		return nil
	})
	if verbose {
		log.Printf("[verbose] ingestFolder %s: %d file(s) processed, dry_run=%v", root, len(results), dryRun)
	}
	return results, err
}

// ingestUploadedFile is used by the /api/upload handler: the browser sends
// bytes directly rather than a filesystem path, so we stage the upload to a
// temp file (extractText/markitdown need a real path) and derive a stable
// source_id from the *filename* rather than a throwaway temp path — so
// re-uploading "Q3-Report.docx" replaces the prior version's chunks instead
// of accumulating duplicates.
//
// keepOriginal persists a copy of the raw bytes under s.Import.OriginalsDir
// (opt-in per upload, see the "Original behalten" checkbox) so the
// citation popup can later offer a download of the exact original file
// alongside its extracted text — see handleSourceOriginal. Never persisted
// during a dry run — dryRun means *zero* side effects, not just skipping
// the embedding call.
func ingestUploadedFile(rag *ragSystem, s appSettings, embedModel, filename string, data []byte, keepOriginal, dryRun bool) (ingestOutcome, error) {
	tmpDir, err := os.MkdirTemp("", "r3-upload-")
	if err != nil {
		return ingestOutcome{SourceName: filename}, err
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, filepath.Base(filename))
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return ingestOutcome{SourceName: filename}, err
	}

	sourceID := "upload:" + filename
	text, err := extractText(tmpPath, s)
	if err != nil {
		return ingestOutcome{SourceID: sourceID, SourceName: filename}, err
	}
	out, err := ingestDocument(rag, s, embedModel, sourceID, "file", filename, text, 0, dryRun)
	if err != nil {
		return out, err
	}
	if keepOriginal && !dryRun {
		if err := saveOriginalFile(s, sourceID, data); err != nil {
			// The chunks are already ingested successfully — losing the
			// original copy is worth surfacing but not worth failing the
			// whole upload over.
			out.Error = fmt.Sprintf("ingested, but failed to keep original: %v", err)
		}
	}
	return out, nil
}

// ingestSharePointFile mirrors ingestUploadedFile for a file downloaded
// from SharePoint (see sharepoint.go/spDownloadFile): stage to a temp
// file, extract, ingest under a source_id that encodes the site and full
// path so re-importing the same file replaces its chunks rather than
// duplicating them, same as every other importer.
func ingestSharePointFile(rag *ragSystem, s appSettings, embedModel, siteURL, itemPath string, data []byte, dryRun bool) (ingestOutcome, error) {
	filename := filepath.Base(itemPath)
	sourceID := fmt.Sprintf("sharepoint:%s:%s", siteURL, itemPath)
	return ingestRemoteFile(rag, s, embedModel, sourceID, "sharepoint_file", filename, data, dryRun)
}

// ingestRemoteFile is the common byte-to-document path for cloud-drive
// connectors. The source ID and kind remain caller-owned (so a OneDrive
// document can never overwrite a same-named SharePoint document), while the
// extraction and size rules intentionally match the established SharePoint
// behavior exactly.
func ingestRemoteFile(rag *ragSystem, s appSettings, embedModel, sourceID, sourceKind, filename string, data []byte, dryRun bool) (ingestOutcome, error) {
	out := ingestOutcome{SourceID: sourceID, SourceName: filename}
	// Callers already pre-check this via spItemExceedsMaxFileMB before
	// ever downloading — this is a defensive backstop, and the one real
	// size guard for the image-OCR branch below, which (unlike the
	// non-image branch, which still goes through extractText's own MaxMB
	// check) has none of its own.
	if maxBytes := emailAttachmentMaxBytes(s); int64(len(data)) > maxBytes {
		return out, fmt.Errorf("file too large (%d bytes, limit %d MB)", len(data), maxBytes/(1024*1024))
	}
	// extractAttachmentText (not extractText directly) so a SharePoint
	// image file — a photographed whiteboard, a screenshot dropped into a
	// document library — is content-sniffed via http.DetectContentType
	// and OCR'd like every other mail-shaped attachment, instead of
	// falling through extractText's plain extension dispatch (which has no
	// image entry) and being rejected as "unsupported file type".
	text, err := extractAttachmentText(s, 0, filename, data)
	if err != nil {
		return out, err
	}
	return ingestDocument(rag, s, embedModel, sourceID, sourceKind, filename, text, 0, dryRun)
}

// ingestSharePointPage ingests one already-extracted modern SharePoint
// page (sharepoint.go's spGetPageText already did the actual text
// extraction — pages have no downloadable file content to stage/extractText
// the way ingestSharePointFile's items do, since a page's body lives in
// its canvasLayout web parts, not a document library item). sourceID uses
// the same "sharepoint:<site>:<path>" shape as ingestSharePointFile, with
// pageName's SitePages-relative path standing in for a regular item's
// itemPath — so one urlMapping entry (settings.go's urlMapping,
// store.go's resolveSourceURL) can cover both files and pages for a
// site's citations, and re-importing the same page replaces its chunks
// instead of duplicating them.
func ingestSharePointPage(rag *ragSystem, s appSettings, embedModel, siteURL, pageName, title, text string, dryRun bool) (ingestOutcome, error) {
	itemPath := "SitePages/" + pageName
	sourceID := fmt.Sprintf("sharepoint:%s:%s", siteURL, itemPath)
	name := title
	if strings.TrimSpace(name) == "" {
		name = pageName
	}
	if strings.TrimSpace(text) == "" {
		return ingestOutcome{SourceID: sourceID, SourceName: name}, fmt.Errorf("page has no extractable text content (no text web parts, or all empty)")
	}
	return ingestDocument(rag, s, embedModel, sourceID, "sharepoint_page", name, text, 0, dryRun)
}

// ingestSharePointSharedLink mirrors ingestSharePointFile for a file
// resolved via a sharing link (sharepoint.go's spResolveShareLink) rather
// than browsed from a configured connection's own document library —
// distinct source_kind ("sharepoint_link") so an admin can independently
// toggle visibility/access for individually-shared files versus
// systematically-synced libraries, and sourceID keyed on the link itself
// (not a site+path, which an ad-hoc resolved link doesn't cleanly have)
// so re-importing the same link updates the same source.
func ingestSharePointSharedLink(rag *ragSystem, s appSettings, embedModel, shareURL, filename string, data []byte, dryRun bool) (ingestOutcome, error) {
	sourceID := "sharepoint_link:" + shareURL
	out := ingestOutcome{SourceID: sourceID, SourceName: filename}
	// See ingestSharePointFile's identical check for why this stays even
	// though importSharePointShareLinks (sharepoint.go) now also checks
	// item.Size before ever downloading — this is the backstop for the
	// image-OCR branch below, which has no size guard of its own.
	if maxBytes := emailAttachmentMaxBytes(s); int64(len(data)) > maxBytes {
		return out, fmt.Errorf("file too large (%d bytes, limit %d MB)", len(data), maxBytes/(1024*1024))
	}
	// extractAttachmentText — see ingestSharePointFile's identical reasoning.
	text, err := extractAttachmentText(s, 0, filename, data)
	if err != nil {
		return out, err
	}
	return ingestDocument(rag, s, embedModel, sourceID, "sharepoint_link", filename, text, 0, dryRun)
}

// emailAttachmentMaxBytes resolves the same MaxFileMB-based size ceiling
// extractText applies to on-disk files (extract.go), but usable in-memory
// before any temp file is written — shared by both branches of
// ingestEmailAttachment below, and by callers (PST/IMAP/Exchange) that can
// check an attachment's size before reading its full bytes at all.
func emailAttachmentMaxBytes(s appSettings) int64 {
	maxMB := s.Import.MaxFileMB
	if maxMB <= 0 {
		maxMB = 25
	}
	return maxMB * 1024 * 1024
}

// extractAttachmentText turns one raw attachment (data, already
// size-checked by the caller) into plain text, sniffing content via
// http.DetectContentType (not just filename extension — a mislabeled or
// missing extension shouldn't hide an actual image) to route image
// attachments through the same OCR path (extractImageTextOCR) chatimages.go
// already uses for Chat/Agent uploads: previously EVERY image attachment
// across all three mail-shaped importers (PST/IMAP/Exchange) fell through
// the extension-only dispatch below, which has no image entry at all, and
// was silently rejected as "unsupported" — so a scanned invoice or a
// screenshot attached to a real customer email was simply never
// searchable, no matter how it arrived. Everything else keeps the
// original extension-based dispatch (native text/HTML/.eml/markitdown).
//
// Shared by ingestEmailAttachment (import-time, persists a source) and the
// live Exchange mailbox reader (mail_graph.go, read-only preview) so both
// paths see the exact same attachment content instead of import getting
// OCR/markitdown and the live view silently seeing nothing.
func extractAttachmentText(s appSettings, idx int, filename string, data []byte) (string, error) {
	ext := fileExtension(filename)
	if strings.HasPrefix(http.DetectContentType(data), "image/") {
		if !s.AllowShellExec {
			return "", fmt.Errorf("image attachment %q: OCR deaktiviert (Einstellungen → Import → „markitdown-/tesseract-Aufrufe erlauben“)", filename)
		}
		return extractImageTextOCR(data, filename, s.Import, s.AllowShellExec)
	}

	if !nativeTextExtensions()[ext] && !nativeHTMLExtensions()[ext] && ext != ".eml" && !markItDownExtensions()[ext] {
		return "", fmt.Errorf("unsupported attachment type %q", ext)
	}

	tmpDir, err := os.MkdirTemp("", "r3-attachment-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	base := filepath.Base(filename)
	if strings.TrimSpace(base) == "" || base == "." || base == string(filepath.Separator) {
		base = fmt.Sprintf("attachment-%d%s", idx, ext)
	}
	tmpPath := filepath.Join(tmpDir, base)
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return "", err
	}
	return extractText(tmpPath, s)
}

// ingestEmailAttachment extracts and ingests one email attachment as its
// own cited source, linked to the parent email by sharing its source_id
// prefix (so deleting the parent PST/mailbox/message also deletes every
// attachment ingested from it — see ragSystem.deleteSourcesByPrefix) and by
// a short header naming the parent email in the embedded text itself.
// idx (the attachment's position within the message) keeps the source_id
// unique even when several attachments share a filename (e.g. repeated
// "image001.png" signature images). Unsupported file types, oversized and
// empty attachments are reported as an error for the caller to count as
// skipped, not as an ingest failure — most real mailboxes carry plenty of
// inline images/signatures no RAG answer would ever need to cite.
func ingestEmailAttachment(rag *ragSystem, s appSettings, embedModel, sourceIDPrefix string, idx int, sourceKind, filename string, data []byte, parentSubject, parentFrom string, docDate int64, dryRun bool) (ingestOutcome, error) {
	sourceID := fmt.Sprintf("%s:attachment:%d:%s", sourceIDPrefix, idx, filename)
	out := ingestOutcome{SourceID: sourceID, SourceName: filename}
	if len(data) == 0 {
		return out, fmt.Errorf("empty attachment")
	}
	if maxBytes := emailAttachmentMaxBytes(s); int64(len(data)) > maxBytes {
		return out, fmt.Errorf("attachment too large (%d bytes, limit %d MB)", len(data), maxBytes/(1024*1024))
	}

	text, err := extractAttachmentText(s, idx, filename, data)
	if err != nil {
		return out, err
	}

	body := fmt.Sprintf("Anhang %q zu E-Mail %q (von %s):\n\n%s", filename, parentSubject, parentFrom, strings.TrimSpace(text))
	sourceName := fmt.Sprintf("%s — Anhang: %s", parentSubject, filename)
	return ingestDocument(rag, s, embedModel, sourceID, sourceKind, sourceName, body, docDate, dryRun)
}

// originalFilePath returns the on-disk path a source's original file is
// stored at, keyed by a hash of sourceID rather than the raw filename
// (source_id/filenames can contain characters unsafe as a path component)
// — deterministic, so handleSourceOriginal can recompute the same path
// from source_id alone without a separate on-disk index.
func originalFilePath(dir, sourceID string) string {
	return filepath.Join(dir, contentHash(sourceID)+filepath.Ext(sourceID))
}

// saveOriginalFile persists data under originalFilePath's deterministic,
// hash-keyed name so handleSourceOriginal can later recompute the same path
// from source_id alone.
func saveOriginalFile(s appSettings, sourceID string, data []byte) error {
	dir := originalsDirOrDefault(s)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(originalFilePath(dir, sourceID), data, 0o644)
}
