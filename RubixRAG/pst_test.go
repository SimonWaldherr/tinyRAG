package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mooijtech/go-pst/v6/pkg/properties"
)

func strPtr(s string) *string { return &s }

func TestPstSubjectPrefersNormalizedSubject(t *testing.T) {
	// The common case go-pst decodes correctly: PidTagNormalizedSubject and
	// PidTagSubjectPrefix are both populated, so pstSubject must reconstruct
	// the full "AW: ..." subject from those rather than the raw (possibly
	// marker-prefixed) PidTagSubject.
	mp := &properties.Message{
		Subject:           strPtr("\x01\x05AW: 1127968;22581305 ZAED;ID23829: Angebot vom 23.06.2"),
		NormalizedSubject: strPtr("1127968;22581305 ZAED;ID23829: Angebot vom 23.06.2"),
		SubjectPrefix:     strPtr("AW: "),
	}
	want := "AW: 1127968;22581305 ZAED;ID23829: Angebot vom 23.06.2"
	if got := pstSubject(mp); got != want {
		t.Errorf("pstSubject: got %q, want %q", got, want)
	}
}

func TestPstSubjectFallsBackWithoutNormalizedSubject(t *testing.T) {
	// A message missing PidTagNormalizedSubject (older/malformed PST) still
	// must not leak the raw compressed-subject marker bytes into the
	// visible subject, even though the prefix/subject split can't be
	// recovered without that property.
	mp := &properties.Message{
		Subject: strPtr("\x01\x05AW: 1127968;22581305 ZAED;ID23829: Angebot vom 23.06.2"),
	}
	want := "AW: 1127968;22581305 ZAED;ID23829: Angebot vom 23.06.2"
	if got := pstSubject(mp); got != want {
		t.Errorf("pstSubject: got %q, want %q", got, want)
	}
}

func TestPstSubjectPlainSubjectUnaffected(t *testing.T) {
	// A subject with no prefix at all (no compressed marker present) must
	// pass through unchanged.
	mp := &properties.Message{
		Subject: strPtr("Quarterly report"),
	}
	want := "Quarterly report"
	if got := pstSubject(mp); got != want {
		t.Errorf("pstSubject: got %q, want %q", got, want)
	}
}

func TestTakeStagedPSTValidatesFolderSelectionWithoutConsumingPreview(t *testing.T) {
	pstStagingMu.Lock()
	original := pstStaging
	pstStaging = map[string]pstStagingEntry{}
	pstStagingMu.Unlock()
	defer func() {
		pstStagingMu.Lock()
		pstStaging = original
		pstStagingMu.Unlock()
	}()

	id := stagePST("mailbox.pst", false, []pstFolderPreview{{Path: "Inbox", Messages: 2}})
	if _, ok, err := takeStagedPST(id, nil); !ok || err == nil {
		t.Fatalf("empty selection: want known staging entry and validation error, got ok=%v err=%v", ok, err)
	}
	if _, ok, err := takeStagedPST(id, []string{"Unknown"}); !ok || err == nil {
		t.Fatalf("unknown folder: want known staging entry and validation error, got ok=%v err=%v", ok, err)
	}
	entry, ok, err := takeStagedPST(id, []string{"Inbox"})
	if !ok || err != nil || entry.Path != "mailbox.pst" {
		t.Fatalf("valid selection: want consumed staging entry, got entry=%+v ok=%v err=%v", entry, ok, err)
	}
	if _, ok, err := takeStagedPST(id, []string{"Inbox"}); ok || err != nil {
		t.Fatalf("want consumed staging entry to be gone, got ok=%v err=%v", ok, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// pstImportJob registry (see pst.go's "Background import jobs" section) —
// detaches a PST import from any single HTTP request so closing the
// browser tab mid-import doesn't cancel it.
// ─────────────────────────────────────────────────────────────────────────────

// TestRegisterPSTImportJobAssignsUniqueIDs guards the basic registry
// contract: every registered job gets its own ID and is immediately
// findable via getPSTImportJob.
func TestRegisterPSTImportJobAssignsUniqueIDs(t *testing.T) {
	j1 := registerPSTImportJob("a.pst", func() {})
	j2 := registerPSTImportJob("b.pst", func() {})
	if j1.id == "" || j2.id == "" {
		t.Fatalf("want non-empty job IDs, got %q and %q", j1.id, j2.id)
	}
	if j1.id == j2.id {
		t.Fatalf("want distinct job IDs, both got %q", j1.id)
	}

	got, ok := getPSTImportJob(j1.id)
	if !ok || got != j1 {
		t.Fatalf("want getPSTImportJob to return the same job for %q", j1.id)
	}
	if _, ok := getPSTImportJob("does-not-exist"); ok {
		t.Fatalf("want ok=false for an unknown job id")
	}
}

// TestPSTImportJobStatusReflectsProgressAndFinish covers the read path a
// polling client actually sees: status() before any update, after
// updateProgress, and after finish.
func TestPSTImportJobStatusReflectsProgressAndFinish(t *testing.T) {
	j := registerPSTImportJob("mailbox.pst", func() {})

	initial := j.status()
	if initial.JobID != j.id || initial.File != "mailbox.pst" || initial.Done {
		t.Fatalf("want fresh job status {id, file, done=false}, got %+v", initial)
	}

	j.updateProgress(pstProgress{
		Result:  pstImportResult{baseImportResult: baseImportResult{Chunks: 3}, Messages: 5},
		Folder:  "Inbox",
		Subject: "Hallo",
		Phase:   "scan",
	})
	mid := j.status()
	if mid.Folder != "Inbox" || mid.Subject != "Hallo" || mid.Phase != "scan" || mid.Result.Messages != 5 || mid.Done {
		t.Fatalf("want status to reflect the latest progress snapshot, got %+v", mid)
	}

	j.finish(pstImportResult{baseImportResult: baseImportResult{Chunks: 7}, Messages: 10}, nil)
	final := j.status()
	if !final.Done {
		t.Fatalf("want Done=true after finish, got %+v", final)
	}
	if final.Result.Messages != 10 || final.Result.Chunks != 7 {
		t.Fatalf("want the final result to replace the last progress snapshot, got %+v", final)
	}
	if final.FinishedAt.IsZero() {
		t.Errorf("want FinishedAt to be set once the job is done")
	}
}

// TestPSTImportJobFinishFoldsErrorIntoResultErrors mirrors what
// handleImportPST used to do itself before finish() existed: a fatal walk
// error (e.g. a corrupt PST, or a cancelled context) must show up in
// Result.Errors, not a separate error channel the frontend would have to
// know about too.
func TestPSTImportJobFinishFoldsErrorIntoResultErrors(t *testing.T) {
	j := registerPSTImportJob("broken.pst", func() {})
	j.finish(pstImportResult{Messages: 2}, errors.New("read pst: unexpected EOF"))

	status := j.status()
	if len(status.Result.Errors) != 1 || status.Result.Errors[0] != "read pst: unexpected EOF" {
		t.Fatalf("want the error folded into Result.Errors, got %+v", status.Result.Errors)
	}
	if !status.Done {
		t.Errorf("want Done=true even when the run ended in error")
	}
}

// TestPSTImportJobRequestCancelInvokesStoredCancelFunc covers the
// background-job equivalent of the old "abort the fetch" mechanism: the
// context.CancelFunc importPST is actually running against must be called
// — and calling it twice (e.g. a doubled click) must not panic.
func TestPSTImportJobRequestCancelInvokesStoredCancelFunc(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	calls := 0
	j := registerPSTImportJob("cancel-me.pst", func() { calls++; cancel() })

	j.requestCancel()
	if calls != 1 {
		t.Fatalf("want the stored cancel func called once, got %d calls", calls)
	}
	j.requestCancel() // must not panic
	if calls != 2 {
		t.Fatalf("want a second requestCancel to call it again (idempotent at the context level), got %d calls", calls)
	}
}

// TestListPSTImportJobsOrdersByMostRecentFirst covers the "reattach from a
// fresh browser tab with no local state" path (handleImportPSTJobs): the
// most recently started job should be easiest to find.
func TestListPSTImportJobsOrdersByMostRecentFirst(t *testing.T) {
	pstJobsMu.Lock()
	pstJobs = map[string]*pstImportJob{} // isolate from other tests in this package
	pstJobsMu.Unlock()

	first := registerPSTImportJob("first.pst", func() {})
	first.startedAt = time.Now().Add(-time.Minute)
	second := registerPSTImportJob("second.pst", func() {})

	jobs := listPSTImportJobs()
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(jobs))
	}
	if jobs[0].id != second.id || jobs[1].id != first.id {
		t.Fatalf("want most-recently-started job first, got order %q, %q", jobs[0].id, jobs[1].id)
	}
}
