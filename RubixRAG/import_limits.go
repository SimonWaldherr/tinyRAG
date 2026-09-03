package main

import (
	"context"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Import-Drosselung: gemeinsame Limits + ein kleiner "Pacer", den jeder
// Import-Loop durchreicht, damit kein Connector unkontrolliert (z. B. 100k
// Tickets/Mails) auf einen Schlag zieht und R3 die Gegenstelle nicht so
// schnell wie möglich mit Requests bombardiert.
//
// Zwei Stellschrauben (importConfig, settings.go), beide global statt pro
// Connector — genau wie MaxFileMB, und weil "wie schnell/wie viel darf ein
// Import ziehen" eine deployment-weite Richtlinie ist, keine pro-Connector-
// Eigenheit:
//
//   - MaxItemsPerRun: harte Obergrenze pro Import-Lauf. Resumierbare
//     Connectoren (IMAP über LastUID, SharePoint über Delta-Link) machen
//     beim nächsten Lauf/Scheduler-Tick weiter — der Cap zerlegt einen
//     großen Rückstand also in Häppchen, statt etwas zu verwerfen.
//   - RequestDelayMS: Pause zwischen den Einzel-Requests eines Imports
//     (proaktives Throttling; das reaktive 429-Backoff in connector.go
//     bleibt als zweite Verteidigungslinie bestehen).
//
// PreviewLimit (wie viele Kandidaten die "Vorschau, dann auswählen"-
// Listen holen) lebt aus Nähe-Gründen ebenfalls hier.
// ─────────────────────────────────────────────────────────────────────────────

const (
	// importMaxItemsDefault bounds a run when the admin hasn't set an
	// explicit cap — generous enough for normal use, small enough that a
	// first-time IMAP/SharePoint full sync can't ingest an entire multi-
	// year mailbox/drive in one uninterrupted run.
	importMaxItemsDefault = 500
	// importPreviewDefault preserves the previous hard-coded listing size.
	importPreviewDefault = 50
	// importMaxItemsCeiling / importPreviewCeiling are sanity ceilings so a
	// fat-fingered config can't reintroduce the very "unbounded" behavior
	// this file exists to prevent.
	importMaxItemsCeiling = 100000
	importPreviewCeiling  = 1000
	// importEmbedBatchSizeDefault preserves store.go's previous hard-coded
	// batch size (replaceSourceChunks).
	importEmbedBatchSizeDefault = 16
	// importEmbedBatchSizeCeiling caps how many chunks go into one embed()
	// call — most embedding APIs (local or cloud) reject an oversized batch
	// outright, so this bounds the blast radius of a fat-fingered config to
	// "one failed batch", not "provider request rejected for the entire
	// import".
	importEmbedBatchSizeCeiling = 256
)

// importEmbedBatchSize resolves how many chunks replaceSourceChunks embeds
// per call to the embedding backend: imp.EmbedBatchSize when set (clamped
// to the ceiling above), else importEmbedBatchSizeDefault.
func importEmbedBatchSize(imp importConfig) int {
	n := imp.EmbedBatchSize
	if n <= 0 {
		return importEmbedBatchSizeDefault
	}
	if n > importEmbedBatchSizeCeiling {
		return importEmbedBatchSizeCeiling
	}
	return n
}

func importMaxItems(imp importConfig) int {
	n := imp.MaxItemsPerRun
	if n <= 0 {
		return importMaxItemsDefault
	}
	if n > importMaxItemsCeiling {
		return importMaxItemsCeiling
	}
	return n
}

func importPreviewLimit(imp importConfig) int {
	n := imp.PreviewLimit
	if n <= 0 {
		return importPreviewDefault
	}
	if n > importPreviewCeiling {
		return importPreviewCeiling
	}
	return n
}

// clampPerPage fits a desired listing size into one API page: at least 1,
// at most the connector's documented per_page/maxResults ceiling. Preview
// stays a single request (no server-side pagination loop, which is exactly
// the unbounded behavior to avoid) — an admin wanting to review more than
// one page picks from the newest apiMax candidates, then re-previews after
// importing, same "preview, then select" model as before.
func clampPerPage(desired, apiMax int) int {
	if desired < 1 {
		desired = importPreviewDefault
	}
	if desired > apiMax {
		return apiMax
	}
	return desired
}

func importRequestDelay(imp importConfig) time.Duration {
	if imp.RequestDelayMS <= 0 {
		return 0
	}
	return time.Duration(imp.RequestDelayMS) * time.Millisecond
}

// importPacer bounds and paces one import run. Usage in a loop:
//
//	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
//	for _, item := range items {
//	    if pacer.capReached() { res.Errors = append(res.Errors, pacer.capNote()); break }
//	    if err := pacer.wait(ctx); err != nil { return res, err } // pace + honor cancel
//	    ... do the one outbound request / ingest ...
//	    pacer.count()
//	}
//
// A zero-value pacer (never constructed) is unusable on purpose — always
// go through newImportPacer.
type importPacer struct {
	max     int
	delay   time.Duration
	seen    int
	stalled bool // set once the cap was hit, so capNote reads naturally
}

// newImportPacer builds a pacer for one connection's import run. connMax is
// that connection's own connRuntime.MaxItemsPerRun (0 = "use imp's global
// importMaxItems default", see connRuntime.effectiveMaxItems) — every
// connector's import function passes its own cfg.MaxItemsPerRun here so a
// single connection can be throttled tighter (or looser, up to
// importMaxItemsCeiling) than the deployment-wide default without
// affecting any other connection.
func newImportPacer(imp importConfig, connMax int) *importPacer {
	max := connRuntime{MaxItemsPerRun: connMax}.effectiveMaxItems(imp)
	return &importPacer{max: max, delay: importRequestDelay(imp)}
}

// capReached reports whether the per-run item cap is already used up.
func (p *importPacer) capReached() bool {
	if p.seen >= p.max {
		p.stalled = true
		return true
	}
	return false
}

// count records that one more item was processed toward the cap.
func (p *importPacer) count() { p.seen++ }

// wait sleeps the configured inter-request delay, returning early (with
// the context error) if the run was cancelled mid-pause — so a long delay
// never blocks a shutdown/abort. A zero delay is a no-op except for the
// cancellation check.
func (p *importPacer) wait(ctx context.Context) error {
	if p.delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(p.delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// capNote is the human-readable line an import appends to its Errors when
// it stopped early because of the cap — phrased so the admin knows it's a
// deliberate throttle, not a failure, and that re-running continues.
func (p *importPacer) capNote() string {
	return "Import-Limit erreicht (" + itoa(p.max) + " Elemente pro Lauf) — weitere Elemente beim nächsten Lauf; Limit unter Einstellungen → Import anpassbar."
}

// capHit reports whether the run stopped because of the cap (for callers
// that want to surface it outside the Errors list, e.g. in a log line).
func (p *importPacer) capHit() bool { return p.stalled }

// itoa avoids pulling strconv into call sites that only need this one
// small conversion for capNote.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
