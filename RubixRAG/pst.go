package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	pst "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/rotisserie/eris"
)

// errPSTCapReached signals that the per-run item cap (import_limits.go)
// was hit mid-walk, so the folder walk should stop cleanly. It's an
// internal control-flow sentinel, swallowed by importPST — never a
// user-facing failure.
var errPSTCapReached = eris.New("pst: per-run import cap reached")

// ─────────────────────────────────────────────────────────────────────────────
// PST/OST mailbox import (MVP data source #1)
//
// Reads an Outlook .pst or .ost export with github.com/mooijtech/go-pst and ingests
// every mail item as its own cited source, keyed by
// "pst:<file>:<folder>:<message-id>". Re-importing the same export (e.g. a
// refreshed nightly dump) only re-embeds messages whose extracted text
// actually changed — everything else is skipped via the content-hash check
// in ingestDocument.
//
// Appointments/contacts/tasks/notes are walked but skipped for the mail RAG
// MVP; only *properties.Message items are ingested.
// ─────────────────────────────────────────────────────────────────────────────

type pstImportResult struct {
	baseImportResult
	mailAttachmentWarnings
	File        string `json:"file"`
	Folders     int    `json:"folders"`
	Messages    int    `json:"messages"`
	Attachments int    `json:"attachments"`
	// BoilerplateBlocks is how many distinct recurring paragraphs (legal
	// disclaimers, signature footers, ...) were detected across this run
	// and stripped before chunking — see boilerplate.go.
	BoilerplateBlocks int `json:"boilerplate_blocks,omitempty"`
	// BoilerplateSamples is per-paragraph debug detail (a text sample plus
	// how often it recurred) for every paragraph BoilerplateBlocks counted.
	// Only populated when importPST is called with debug=true, since the
	// samples are verbatim message content — a normal import's result
	// shouldn't carry that by default, just the count.
	BoilerplateSamples []boilerplateStat `json:"boilerplate_samples,omitempty"`
}

// pstProgress is a snapshot handed to the onProgress callback while a PST
// walk is in flight, so callers (see handleImportPST) can stream it to the
// browser instead of blocking on one final response.
type pstProgress struct {
	Result  pstImportResult
	Folder  string
	Subject string
	// Phase makes the two-pass import visible to polling clients: scan
	// extracts mail/attachments and learns recurring boilerplate; ingest
	// then chunks, embeds, and writes the cleaned message bodies.
	Phase string
}

// progressEvery caps how often onProgress fires — once per message would
// flood an NDJSON response (and the DOM) on multi-thousand-message
// mailboxes, so we sample every N messages plus once per finished folder.
const progressEvery = 20

// walkPSTFolders recursively visits every folder with its full path
// ("Top of Personal Folders/Posteingang/2024"), unlike go-pst's own
// (*File).WalkFolders which only hands the callback a bare folder.Name —
// insufficient once folders need to be identified unambiguously (folder
// filtering, sourceID, preview) since same-named folders nested under
// different parents are common in real mailboxes.
func walkPSTFolders(root pst.Folder, visit func(path string, folder *pst.Folder) error) error {
	return walkPSTFoldersRec("", &root, visit)
}

// walkPSTFoldersRec is walkPSTFolders' recursive implementation, threading
// the accumulated "parent/child" path down through each level of the
// folder tree so visit always sees a folder's full, unambiguous path.
func walkPSTFoldersRec(parentPath string, folder *pst.Folder, visit func(path string, folder *pst.Folder) error) error {
	path := folder.Name
	if parentPath != "" {
		path = parentPath + "/" + folder.Name
	}
	if err := visit(path, folder); err != nil {
		return err
	}
	subFolders, err := folder.GetSubFolders()
	if err != nil {
		return eris.Wrap(err, "failed to get sub folders")
	}
	for i := range subFolders {
		if err := walkPSTFoldersRec(path, &subFolders[i], visit); err != nil {
			return err
		}
	}
	return nil
}

// pendingPSTMessage is one scanned-but-not-yet-ingested message, buffered
// during importPST's scan pass so its body can be re-stripped of whatever
// the run-wide boilerplate detector found before the ingest pass embeds
// it — see importPST's doc comment.
type pendingPSTMessage struct {
	SourceID   string
	SourceName string
	Fields     emailFields
	DocDate    int64
	Folder     string
}

// importPST ingests every message under the folders named in
// allowedFolders (full paths, as returned by previewPST). A nil map
// imports every folder — used when the caller didn't offer folder
// selection at all, keeping importPST usable standalone.
//
// Runs in two passes rather than ingesting each message as it's walked:
// mailbox exports almost always carry the same legal-disclaimer/signature
// footer on every message, and recognizing that requires having seen how
// often a given paragraph recurs across the *whole* run first (see
// boilerplate.go). Pass 1 walks the PST once, extracting every selected
// message's fields, ingesting attachments immediately (unaffected by
// body-text boilerplate stripping) and feeding each body into a
// boilerplateDetector, but buffers the body-ingest step itself in
// `pending` rather than embedding it right away. Once pass 1 finishes and
// the boilerplate set for this run is known, pass 2 strips it from each
// buffered message's body and only then calls ingestDocument. The
// tradeoff is memory: every message's extracted text is held in `pending`
// for the whole run rather than being freed message-by-message — fine for
// a single admin-triggered import, not something to also do per-message
// on a hot path.
func importPST(ctx context.Context, rag *ragSystem, s appSettings, embedModel, pstPath string, allowedFolders map[string]bool, dryRun, debug bool, onProgress func(pstProgress)) (pstImportResult, error) {
	res := pstImportResult{File: filepath.Base(pstPath)}
	res.DryRun = dryRun
	if verbose {
		log.Printf("[verbose] pst import: file=%s dry_run=%v", pstPath, dryRun)
	}

	reader, err := os.Open(pstPath)
	if err != nil {
		return res, fmt.Errorf("open pst: %w", err)
	}
	defer reader.Close()

	pstFile, err := pst.New(reader)
	if err != nil {
		return res, fmt.Errorf("read pst: %w", err)
	}
	defer pstFile.Cleanup()

	rootFolder, err := pstFile.GetRootFolder()
	if err != nil {
		return res, fmt.Errorf("read root folder: %w", err)
	}

	baseName := filepath.Base(pstPath)
	detector := newBoilerplateDetector()
	var pending []pendingPSTMessage

	// Per-run cap (import_limits.go): a single PST folder can hold 100k+
	// messages. errPSTCapReached stops the folder walk cleanly once the
	// cap is hit; it's swallowed (not surfaced as a failure) after the
	// walk, and a note is added so the admin knows the run was truncated
	// and can raise the limit or import remaining folders separately.
	maxItems := importMaxItems(s.Import)
	capReached := false

	walkErr := walkPSTFolders(rootFolder, func(path string, folder *pst.Folder) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if allowedFolders != nil && !allowedFolders[path] {
			return nil
		}
		res.Folders++
		messageIterator, err := folder.GetMessageIterator()
		if eris.Is(err, pst.ErrMessagesNotFound) {
			return nil
		} else if err != nil {
			return err
		}

		var lastSubject string
		for messageIterator.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(pending) >= maxItems {
				capReached = true
				return errPSTCapReached
			}
			message := messageIterator.Value()
			mp, ok := message.Properties.(*properties.Message)
			if !ok {
				continue
			}
			res.Messages++

			// repairMojibake guards against go-pst's internal codepage
			// handling occasionally mis-detecting a message's codepage
			// (see encoding.go) — outside our control at the source, but
			// cheap and safe to repair defensively once extracted.
			subject := repairMojibake(pstSubject(mp))
			from := repairMojibake(formatAddress(mp.GetSenderName(), mp.GetSenderEmailAddress()))
			body := strings.TrimSpace(mp.GetBody())
			if body == "" {
				body = htmlToText(mp.GetBodyHtml())
			}
			if strings.TrimSpace(body) == "" {
				// Many Outlook-composed messages never populate the plain
				// or HTML body properties at all, only the RTF one
				// (PidTagRtfCompressed) — without this fallback those
				// messages ingest as header-only, bodyless chunks.
				if rtfRaw, rtfErr := message.GetBodyRTF(); rtfErr == nil {
					body = rtfToText(rtfRaw)
				}
			}
			body = repairMojibake(body)

			var docDate int64
			if ns := mp.GetMessageDeliveryTime(); ns > 0 {
				docDate = ns / 1_000_000_000
			} else if ns := mp.GetClientSubmitTime(); ns > 0 {
				docDate = ns / 1_000_000_000
			}

			fields := emailFields{Subject: subject, From: from, To: mp.GetDisplayTo(), Body: body}
			if docDate > 0 {
				fields.Date = time.Unix(docDate, 0)
			}

			sourceID := fmt.Sprintf("pst:%s:%s:%d", baseName, path, message.Identifier)

			detector.observe(body)
			pending = append(pending, pendingPSTMessage{
				SourceID:   sourceID,
				SourceName: formatSourceName(subject, from),
				Fields:     fields,
				DocDate:    docDate,
				Folder:     path,
			})

			importPSTAttachments(rag, s, embedModel, message, sourceID, subject, from, docDate, dryRun, &res)

			lastSubject = subject
			if onProgress != nil && res.Messages%progressEvery == 0 {
				if debug {
					// Provisional: detector has only seen messages up to this
					// point, so this can still change before the scan
					// finishes (a paragraph might not yet have crossed
					// boilerplateThreshold, or might still, as totalMessages
					// grows). Good enough to let an admin watching a
					// half-hour import notice "way too much"/"way too
					// little" getting flagged long before the end, rather
					// than only in the final result — see boilerplate.go.
					// The actual stripping below still recomputes the
					// authoritative set once the full scan is done.
					provisional := detector.boilerplateSet()
					res.BoilerplateBlocks = len(provisional)
					res.BoilerplateSamples = detector.stats(provisional)
				}
				onProgress(pstProgress{Result: res, Folder: path, Subject: lastSubject, Phase: "scan"})
			}
		}
		if onProgress != nil {
			onProgress(pstProgress{Result: res, Folder: path, Phase: "scan"})
		}
		return messageIterator.Err()
	})
	if walkErr != nil && !eris.Is(walkErr, errPSTCapReached) {
		return res, walkErr
	}
	if capReached {
		res.Errors = append(res.Errors, fmt.Sprintf("Import-Limit erreicht (%d Nachrichten pro Lauf) — verbleibende Nachrichten/Ordner separat importieren oder das Limit unter Einstellungen → Import erhöhen.", maxItems))
	}

	boilerplate := detector.boilerplateSet()
	res.BoilerplateBlocks = len(boilerplate)
	if debug {
		res.BoilerplateSamples = detector.stats(boilerplate)
	}
	if verbose && len(boilerplate) > 0 {
		log.Printf("[verbose] pst import: %d recurring boilerplate paragraph(s) detected across %d message(s), stripping before ingest", len(boilerplate), detector.totalMessages)
	}
	if onProgress != nil {
		onProgress(pstProgress{Result: res, Phase: "ingest"})
	}

	for i, m := range pending {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		m.Fields.Body = stripBoilerplateParagraphs(m.Fields.Body, boilerplate)

		outcome, ingestErr := ingestDocument(rag, s, embedModel, m.SourceID, "pst_email", m.SourceName, m.Fields.String(), m.DocDate, dryRun)
		foldIngestOutcome(outcome, ingestErr, &res.Errors, &res.Skipped, &res.Chunks)

		if onProgress != nil && (i+1)%progressEvery == 0 {
			onProgress(pstProgress{Result: res, Folder: m.Folder, Subject: m.Fields.Subject, Phase: "ingest"})
		}
	}
	if onProgress != nil {
		onProgress(pstProgress{Result: res, Phase: "ingest"})
	}

	return res, nil
}

// importPSTAttachments ingests every attachment of message as its own
// cited source (see ingestEmailAttachment), tallying into res so the
// caller's progress reporting/summary just works without change. Errors
// here (an unreadable attachment, a type extractText doesn't support, a
// binary-only property with no retrievable content) are routine — most
// mailboxes are full of inline images/signatures — so they count as
// skipped, never as a reason to fail the whole message.
func importPSTAttachments(rag *ragSystem, s appSettings, embedModel string, message *pst.Message, parentSourceID, parentSubject, parentFrom string, docDate int64, dryRun bool, res *pstImportResult) {
	count, err := message.GetAttachmentCount()
	if err != nil || count == 0 {
		return
	}
	for i := 0; i < count; i++ {
		att, err := message.GetAttachment(i)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: attachment %d: %v", parentSourceID, i, err))
			continue
		}
		filename := repairMojibake(strings.TrimSpace(att.GetAttachLongFilename()))
		if filename == "" {
			filename = repairMojibake(strings.TrimSpace(att.GetAttachFilename()))
		}
		if filename == "" {
			filename = fmt.Sprintf("attachment-%d", i)
		}
		// GetAttachSize is a cheap server-side property check — skip the
		// (potentially large) WriteTo read entirely for an oversized
		// attachment instead of buffering it in memory just to reject it
		// afterwards in ingestEmailAttachment. A reported size of 0 is
		// treated as "unknown" (some PST attachments don't populate it) and
		// falls through to the normal read + post-hoc size check.
		if sz := att.GetAttachSize(); sz > 0 && int64(sz) > emailAttachmentMaxBytes(s) {
			res.Skipped++
			res.AttachmentWarnings = append(res.AttachmentWarnings, fmt.Sprintf("%s — Anhang: %s: zu groß (%d Bytes)", parentSubject, filename, sz))
			continue
		}
		var buf bytes.Buffer
		if _, err := att.WriteTo(&buf); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: attachment %q: %v", parentSourceID, filename, err))
			continue
		}
		outcome, err := ingestEmailAttachment(rag, s, embedModel, parentSourceID, i, "pst_attachment", filename, buf.Bytes(), parentSubject, parentFrom, docDate, dryRun)
		foldAttachmentOutcome(outcome, err, &res.Attachments, &res.Skipped, &res.Chunks, &res.AttachmentWarnings)
	}
}

// pstFolderPreview is one folder's entry in a preview response.
type pstFolderPreview struct {
	Path     string `json:"path"`
	Messages int    `json:"messages"`
}

// pstPreviewResult lists every non-empty folder in a PST without importing
// anything — folder.MessageCount is read directly from the folder header,
// so this is cheap (no per-message parsing) even for large mailboxes.
type pstPreviewResult struct {
	File    string             `json:"file"`
	Folders []pstFolderPreview `json:"folders"`
	Total   int                `json:"total"`
	// FoldersWalked is every folder the walk actually reached, including
	// empty ones — always >= len(Folders). Surfaced mainly for diagnosing
	// "fewer messages than expected" reports: if FoldersWalked is small,
	// the PST itself only contains that much structure (e.g. it was
	// exported scoped to one subfolder, not the whole mailbox) rather than
	// R3 failing to see folders that are actually there.
	FoldersWalked int `json:"folders_walked"`
}

// previewPST lists every non-empty folder in pstPath so the browser can
// show a folder-selection checklist before committing to a (potentially
// large and slow) import.
func previewPST(pstPath string) (pstPreviewResult, error) {
	res := pstPreviewResult{File: filepath.Base(pstPath)}

	reader, err := os.Open(pstPath)
	if err != nil {
		return res, fmt.Errorf("open pst: %w", err)
	}
	defer reader.Close()

	pstFile, err := pst.New(reader)
	if err != nil {
		return res, fmt.Errorf("read pst: %w", err)
	}
	defer pstFile.Cleanup()

	rootFolder, err := pstFile.GetRootFolder()
	if err != nil {
		return res, fmt.Errorf("read root folder: %w", err)
	}

	seenFolders := 0
	walkErr := walkPSTFolders(rootFolder, func(path string, folder *pst.Folder) error {
		seenFolders++
		if verbose {
			// Every folder the walk actually reached, including empty ones —
			// deliberately logged before the MessageCount<=0 filter below, so
			// "the checklist only shows N folders" can be told apart from
			// "the walk itself only ever reached N folders" (e.g. a PST that
			// was exported scoped to a single subfolder rather than the
			// whole mailbox looks identical in the UI either way).
			log.Printf("[verbose] pst folder: %q messages=%d has_subfolders=%v", path, folder.MessageCount, folder.HasSubFolders)
		}
		if folder.MessageCount <= 0 {
			return nil
		}
		res.Folders = append(res.Folders, pstFolderPreview{Path: path, Messages: int(folder.MessageCount)})
		res.Total += int(folder.MessageCount)
		return nil
	})
	res.FoldersWalked = seenFolders
	if verbose {
		log.Printf("[verbose] pst preview: walked %d folder(s) total, %d non-empty, walkErr=%v", seenFolders, len(res.Folders), walkErr)
	}
	return res, walkErr
}

// ─────────────────────────────────────────────────────────────────────────────
// Upload staging: handleImportPSTPreview stages an uploaded .pst to a temp
// file and returns a staging ID instead of importing immediately, so the
// browser can show the folder list and let the user pick which folders to
// import before the (potentially large) mailbox is actually parsed and
// embedded. handleImportPST then takes the staged path by ID rather than
// requiring the file to be uploaded a second time.
//
// Entries are swept lazily (on every stagePST call) rather than via a
// background ticker, since previews are a rare, interactive-only action —
// no goroutine lifecycle to manage at startup/shutdown for that.
// ─────────────────────────────────────────────────────────────────────────────

const pstStagingTTL = 2 * time.Hour

type pstStagingEntry struct {
	Path    string
	Created time.Time
	Folders map[string]bool // non-empty folders returned by the preview that created this entry
	// Owned marks a staged path as a temp file R3 itself created for an
	// uploaded .pst (see stagePSTUpload) — safe to os.RemoveAll(Dir(Path))
	// once done with it. An unowned entry points at a file the operator
	// placed directly on the server's filesystem (see
	// handleImportPSTPreviewPath); its directory is never removed, since
	// that directory may hold other files the operator still needs.
	Owned bool
}

var (
	pstStagingMu sync.Mutex
	pstStaging   = map[string]pstStagingEntry{}
)

// stagePST records a PST's path and its previewed folders under a fresh staging ID, sweeping any
// entries older than pstStagingTTL first so an abandoned preview (upload
// without a follow-up import) doesn't leak its temp file forever. owned
// must be true only for temp files R3 created itself (uploads) — see
// pstStagingEntry.Owned.
func stagePST(path string, owned bool, folders []pstFolderPreview) string {
	pstStagingMu.Lock()
	defer pstStagingMu.Unlock()

	now := time.Now()
	for id, e := range pstStaging {
		if now.Sub(e.Created) > pstStagingTTL {
			if e.Owned {
				os.RemoveAll(filepath.Dir(e.Path))
			}
			delete(pstStaging, id)
		}
	}

	knownFolders := make(map[string]bool, len(folders))
	for _, folder := range folders {
		knownFolders[folder.Path] = true
	}
	id := newRequestID()
	pstStaging[id] = pstStagingEntry{Path: path, Created: now, Owned: owned, Folders: knownFolders}
	return id
}

// takeStagedPST validates the requested folder paths against the preview and
// then removes and returns the entry, so a valid staging ID can only be
// committed once. Invalid requests deliberately leave the entry available so
// the browser can correct its selection without another multi-GB upload.
func takeStagedPST(id string, folders []string) (pstStagingEntry, bool, error) {
	pstStagingMu.Lock()
	defer pstStagingMu.Unlock()
	e, ok := pstStaging[id]
	if !ok {
		return pstStagingEntry{}, false, nil
	}
	if len(folders) == 0 {
		return pstStagingEntry{}, true, fmt.Errorf("select at least one PST folder")
	}
	for _, folder := range folders {
		if !e.Folders[folder] {
			return pstStagingEntry{}, true, fmt.Errorf("folder %q was not part of the PST preview", folder)
		}
	}
	delete(pstStaging, id)
	return e, true, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Background import jobs: importPST for a real mailbox can run half an
// hour or more. handleImportPST used to run it synchronously against
// r.Context() and stream progress as NDJSON on the live response — closing
// the browser tab (or any network blip) cancels r.Context(), which
// importPST's ctx.Err() checks then abort on, killing an otherwise-healthy
// import. pstImportJob detaches execution from any single HTTP request:
// the import runs in its own goroutine against context.Background() (only
// cancelled via requestCancel, never by a client disconnecting), and
// progress/result live in this in-memory registry for any request —
// including a page reload, or a completely different browser tab — to poll
// via job ID. Same "in-memory only, swept by TTL, not an audit trail that
// needs to survive a restart" shape as pstStaging above and scheduler.go's
// schedulerHistory: a full process restart mid-import loses the job's
// progress tracking, but not the work already done (ingestDocument's
// content-hash dedup means re-running the same import afterward just
// skips what's already in, no data loss).
// ─────────────────────────────────────────────────────────────────────────────

// pstJobRetention bounds how long a *finished* job's status stays
// queryable before pstImportJobs' TTL sweep drops it — long enough that an
// admin who was away when a half-hour import finished can still come back
// and see the result, short enough not to leak memory indefinitely on a
// long-running server.
const pstJobRetention = 24 * time.Hour

// pstImportJob tracks one importPST run. mu guards every field the
// background goroutine (updateProgress/finish) and HTTP handlers (status)
// touch concurrently.
type pstImportJob struct {
	mu         sync.Mutex
	id         string
	file       string
	startedAt  time.Time
	finishedAt time.Time
	folder     string
	subject    string
	phase      string
	result     pstImportResult
	done       bool
	cancel     context.CancelFunc
}

// pstJobStatusDTO is the JSON shape returned by the status/jobs endpoints.
// No separate error field: a fatal walk error is folded into
// Result.Errors by finish() below, same as handleImportPST always did
// before this job registry existed — one error channel, not two.
type pstJobStatusDTO struct {
	JobID      string          `json:"job_id"`
	File       string          `json:"file"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at,omitempty"`
	Folder     string          `json:"folder,omitempty"`
	Subject    string          `json:"subject,omitempty"`
	Phase      string          `json:"phase,omitempty"`
	Result     pstImportResult `json:"result"`
	Done       bool            `json:"done"`
}

var (
	pstJobsMu sync.Mutex
	pstJobs   = map[string]*pstImportJob{}
)

// registerPSTImportJob creates and records a new job, sweeping finished
// jobs older than pstJobRetention first — mirrors stagePST's sweep-on-
// access pattern just above.
func registerPSTImportJob(file string, cancel context.CancelFunc) *pstImportJob {
	pstJobsMu.Lock()
	defer pstJobsMu.Unlock()

	now := time.Now()
	for id, j := range pstJobs {
		j.mu.Lock()
		stale := j.done && now.Sub(j.finishedAt) > pstJobRetention
		j.mu.Unlock()
		if stale {
			delete(pstJobs, id)
		}
	}

	j := &pstImportJob{id: newRequestID(), file: file, startedAt: now, cancel: cancel}
	pstJobs[j.id] = j
	return j
}

// getPSTImportJob looks up a job by ID — used by the status and cancel
// endpoints. ok is false for an unknown/expired/already-swept ID (e.g.
// after a server restart), which callers surface as 404.
func getPSTImportJob(id string) (*pstImportJob, bool) {
	pstJobsMu.Lock()
	defer pstJobsMu.Unlock()
	j, ok := pstJobs[id]
	return j, ok
}

// listPSTImportJobs returns every known job (running or recently
// finished), most recently started first — lets a freshly loaded Import
// tab (new browser, or the same one after a reload) discover an
// already-running import without needing any client-side state at all.
func listPSTImportJobs() []*pstImportJob {
	pstJobsMu.Lock()
	defer pstJobsMu.Unlock()
	out := make([]*pstImportJob, 0, len(pstJobs))
	for _, j := range pstJobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].startedAt.After(out[k].startedAt) })
	return out
}

// updateProgress records one onProgress callback's snapshot — passed as
// importPST's onProgress func directly from handleImportPST's goroutine.
func (j *pstImportJob) updateProgress(p pstProgress) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.result = p.Result
	j.folder = p.Folder
	j.subject = p.Subject
	j.phase = p.Phase
}

// finish records the final result, folding a fatal walk error into
// Result.Errors — same place handleImportPST used to fold it before
// streaming the "done" NDJSON line, now done once here instead.
func (j *pstImportJob) finish(res pstImportResult, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
	}
	j.result = res
	j.done = true
	j.finishedAt = time.Now()
}

// requestCancel stops the job's import goroutine — safe to call more than
// once (a cancelled context is idempotent) and safe to call on an already-
// finished job (a no-op, since importPST has already returned).
func (j *pstImportJob) requestCancel() {
	j.mu.Lock()
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// status returns a JSON-ready snapshot under lock.
func (j *pstImportJob) status() pstJobStatusDTO {
	j.mu.Lock()
	defer j.mu.Unlock()
	return pstJobStatusDTO{
		JobID: j.id, File: j.file, StartedAt: j.startedAt, FinishedAt: j.finishedAt,
		Folder: j.folder, Subject: j.subject, Phase: j.phase, Result: j.result, Done: j.done,
	}
}

// pstSubject reconstructs a message's real subject from go-pst's already-
// decoded MAPI properties, instead of the raw mp.GetSubject() value.
// Outlook can store PidTagSubject in "compressed" form — a leading 0x01
// marker byte, one prefix-length byte, then <prefix><normalized subject>
// with no separator (MS-OXCMSG §2.5.3.1.1) — and go-pst's GetSubject()
// passes that raw byte sequence straight through, so any message with a
// prefix like "AW:"/"RE:"/"FW:" shows up with the two leading marker
// bytes leaking into the subject as literal control characters (visible
// as "\x01\x05AW: ..." in the UI). PidTagNormalizedSubject/
// PidTagSubjectPrefix are separate, already-decoded MAPI properties
// carrying the same information without that marker, so reconstructing
// from those sidesteps the compressed byte format entirely instead of
// needing to hand-parse it.
func pstSubject(mp *properties.Message) string {
	if normalized := strings.TrimSpace(mp.GetNormalizedSubject()); normalized != "" {
		return mp.GetSubjectPrefix() + normalized
	}
	// Fallback for the rare message with no PidTagNormalizedSubject set:
	// at least strip the marker+length-byte pair if the raw subject still
	// starts with it, so the visible "\x01\x05" artifact is gone even
	// though the prefix/subject split can't be recovered without the
	// property go-pst didn't populate.
	raw := strings.TrimSpace(mp.GetSubject())
	if len(raw) >= 2 && raw[0] == 0x01 {
		raw = strings.TrimSpace(raw[2:])
	}
	return raw
}

// formatAddress renders a sender as "Name <email>", falling back to
// whichever of the two is actually present — PST messages don't reliably
// populate both fields.
func formatAddress(name, email string) string {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case name != "":
		return name
	default:
		return email
	}
}
