package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Audit trail — docs/ENTERPRISE_READINESS.md Phase A ("do this first":
// foundational to every other compliance conversation, and doesn't depend
// on any infrastructure decision outside R3's own codebase).
//
// Append-only JSONL, one line per recorded action — same storage shape as
// feedback.go's feedbackLogPath and scheduler.go's run history: one file,
// one mutex-guarded append, no SQL table, no cleanup job. Deliberately no
// admin-UI viewer in this phase either (per the plan): read the file
// directly (tail/grep) until real usage patterns justify building one — a
// queryable store/viewer is a natural upgrade once someone's actually
// asked "show me who deleted X", not before.
//
// Detail is always short and structured (e.g. "source_id=...",
// "connector=sharepoint chunks=12 errors=0") — never the full question/
// answer text, never a secret/credential value, so the audit log itself
// never becomes a new sensitive-data store. Actor is the session's AD
// mail/CN if present, else "anonym" — never a password.
// ─────────────────────────────────────────────────────────────────────────────

// auditLogPath is set once in main() to a file next to whatever -settings
// path was configured, so separate instances (e.g. launch.json's
// r3-verify/r3-verify2, each with its own -settings) don't share one audit
// log — same pattern as feedbackLogPath.
var auditLogPath = "r3-audit.jsonl"

// auditEvent is one recorded action, appended as a single JSON line.
type auditEvent struct {
	Time     int64  `json:"time"` // unix seconds
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Detail   string `json:"detail,omitempty"`
	RemoteIP string `json:"remote_ip,omitempty"`
}

var auditMu sync.Mutex

// appendAudit writes one event as a JSON line to auditLogPath, creating
// the file on first use.
func appendAudit(ev auditEvent) error {
	auditMu.Lock()
	defer auditMu.Unlock()
	f, err := os.OpenFile(auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// actorFromRequest resolves "who's asking" the same way every other
// caller-identity check in this codebase does: the session's AD mail if
// present, else its CN, else "anonym" for no session at all.
func actorFromRequest(r *http.Request) string {
	claims, ok := currentSession(r)
	if !ok {
		return "anonym"
	}
	return sessionActor(claims)
}

// logAudit records one audit event for the current request — best-effort,
// like feedback.go/scheduler.go's own persistence: a write failure (disk
// full, permissions) is logged, never fails the request it's attached to.
// The overwhelming majority of call sites use this directly; logAuditAs
// below exists only for the one case (the detached PST import job) where
// the *http.Request is gone by the time the action actually finishes.
func logAudit(r *http.Request, action, detail string) {
	logAuditAs(actorFromRequest(r), clientKey(r), action, detail)
}

// logAuditAs is logAudit's lower-level counterpart for a caller that
// already resolved actor/remoteIP before the *http.Request stopped being
// available — see handleImportPST's detached background job.
func logAuditAs(actor, remoteIP, action, detail string) {
	ev := auditEvent{
		Time:     time.Now().Unix(),
		Actor:    actor,
		Action:   action,
		Detail:   detail,
		RemoteIP: remoteIP,
	}
	if err := appendAudit(ev); err != nil {
		log.Printf("WARN: audit log write failed: %v", err)
	}
}

// logImportAudit records one "import" event for any connector whose
// result embeds baseImportResult (every connector but PST/upload/folder,
// which have their own result shapes and log directly) — chunks/skipped/
// error counts and the dry-run flag, never any content.
func logImportAudit(r *http.Request, connector string, base baseImportResult) {
	logAudit(r, "import", fmt.Sprintf("connector=%s chunks=%d skipped=%d errors=%d dry_run=%v",
		connector, base.Chunks, base.Skipped, len(base.Errors), base.DryRun))
}
