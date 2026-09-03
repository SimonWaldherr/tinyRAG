package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Settings-Änderungshistorie: bei jedem erfolgreichen Settings-Save wird
// der Feld-Diff (alter Wert → neuer Wert, JSON-Pfad-genau) als eine JSONL-
// Zeile angehängt — dieselbe Storage-Form wie audit.go/feedback.go: eine
// Datei, ein Mutex, kein SQL. Der Admin sieht damit unter Einstellungen →
// Änderungshistorie, wer wann welches Feld geändert hat.
//
// Secrets (Passwörter, API-Keys, Client-Secrets, Tokens) tauchen NIE mit
// Wert auf: für als geheim erkannte Pfade wird nur "Pfad wurde geändert"
// (secret:true) protokolliert, weder alter noch neuer Wert — die Historie
// darf selbst kein neuer Credential-Speicher werden, gleiche Regel wie
// audit.go's Detail-Konvention. Die *_env-Felder (nur Variablennamen,
// keine Werte) gelten dabei nicht als geheim.
// ─────────────────────────────────────────────────────────────────────────────

// settingsHistoryPath is set once in main() to a file next to whatever
// -settings path was configured — same per-instance pattern as
// auditLogPath.
var settingsHistoryPath = "r3-settings-history.jsonl"

type settingsChange struct {
	Path string `json:"path"`
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
	// Secret marks a change whose values were deliberately not recorded —
	// the UI renders it as "(geändert, Wert nicht protokolliert)".
	Secret bool `json:"secret,omitempty"`
}

type settingsHistoryEntry struct {
	Time    int64            `json:"time"` // unix seconds
	Actor   string           `json:"actor"`
	Changes []settingsChange `json:"changes"`
	// Source distinguishes a settings-tab form save (the default, "") from
	// a whole-file "Import Settings" upload ("import") — same underlying
	// POST /api/settings and diff logic either way, just tagged so the
	// Änderungshistorie UI can show an admin *how* dozens of fields changed
	// at once instead of that looking like an unexplained bulk edit.
	Source string `json:"source,omitempty"`
}

var settingsHistoryMu sync.Mutex

// isSecretSettingsPath reports whether the flattened JSON path names a
// credential-bearing field. Matches on the last path segment: anything
// containing password/secret/api_key/api_token/apikey counts — EXCEPT the
// *_env variants, which hold an environment-variable *name* (already
// shown unmasked in the Settings UI itself, see maskedSettings).
func isSecretSettingsPath(path string) bool {
	seg := path
	if i := strings.LastIndexAny(path, "."); i >= 0 {
		seg = path[i+1:]
	}
	seg = strings.ToLower(seg)
	if strings.HasSuffix(seg, "_env") {
		return false
	}
	for _, marker := range []string{"password", "secret", "api_key", "api_token", "apikey"} {
		if strings.Contains(seg, marker) {
			return true
		}
	}
	return false
}

// flattenSettingsJSON walks an unmarshalled JSON value and records every
// leaf under its dotted path ("shop.timeout_seconds", "presets[0].name").
// Rendering leaves as their compact JSON text keeps the diff type-agnostic
// — numbers, bools and strings all compare as strings without a schema.
func flattenSettingsJSON(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flattenSettingsJSON(p, child, out)
		}
	case []any:
		for i, child := range t {
			flattenSettingsJSON(fmt.Sprintf("%s[%d]", prefix, i), child, out)
		}
	default:
		b, err := json.Marshal(v)
		if err != nil {
			out[prefix] = fmt.Sprintf("%v", v)
			return
		}
		out[prefix] = string(b)
	}
}

// diffSettings computes the field-level changes between two settings
// snapshots via their JSON form (so struct tags define the paths, matching
// what's in settings.json itself). Values are truncated so a pathological
// paste (e.g. a huge prompt) doesn't bloat the history file; secret paths
// get no values at all.
func diffSettings(before, after appSettings) []settingsChange {
	flatten := func(s appSettings) map[string]string {
		raw, err := json.Marshal(s)
		if err != nil {
			return nil
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil
		}
		out := map[string]string{}
		flattenSettingsJSON("", v, out)
		return out
	}
	b, a := flatten(before), flatten(after)
	if b == nil || a == nil {
		return nil
	}

	paths := map[string]bool{}
	for p := range b {
		paths[p] = true
	}
	for p := range a {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	var changes []settingsChange
	for _, p := range sorted {
		oldV, newV := b[p], a[p]
		if oldV == newV {
			continue
		}
		if isSecretSettingsPath(p) {
			changes = append(changes, settingsChange{Path: p, Secret: true})
			continue
		}
		changes = append(changes, settingsChange{
			Path: p,
			Old:  truncateRunesNote(oldV, 200),
			New:  truncateRunesNote(newV, 200),
		})
	}
	return changes
}

// appendSettingsHistory writes one entry as a JSON line — best-effort like
// logAudit: a write failure is logged, never fails the save it documents.
func appendSettingsHistory(entry settingsHistoryEntry) {
	settingsHistoryMu.Lock()
	defer settingsHistoryMu.Unlock()
	f, err := os.OpenFile(settingsHistoryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("WARN: settings history write failed: %v", err)
		return
	}
	defer f.Close()
	line, err := json.Marshal(entry)
	if err != nil {
		log.Printf("WARN: settings history encode failed: %v", err)
		return
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Printf("WARN: settings history write failed: %v", err)
	}
}

// summarizeSettingsChanges builds the short audit-log detail for a save:
// just the changed paths (paths are never secret, only values are), capped
// so the audit line stays a line.
func summarizeSettingsChanges(changes []settingsChange) string {
	if len(changes) == 0 {
		return "keine Feldänderungen"
	}
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	return truncateRunesNote(fmt.Sprintf("%d Feld(er): %s", len(changes), strings.Join(paths, ",")), 400)
}

// settingsHistoryMaxEntries bounds how many entries one GET returns —
// newest first; older history stays in the file (grep/tail it directly if
// ever needed, same convention as the audit log).
const settingsHistoryMaxEntries = 200

// handleSettingsHistory returns the most recent history entries, newest
// first. Admin-gated at route registration like every settings endpoint.
func handleSettingsHistory(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	f, err := os.Open(settingsHistoryPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, []settingsHistoryEntry{})
			return
		}
		writeJSONError(w, "read history: "+err.Error(), 500)
		return
	}
	defer f.Close()

	var entries []settingsHistoryEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var e settingsHistoryEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // one corrupt line must not hide the rest
		}
		entries = append(entries, e)
	}
	if len(entries) > settingsHistoryMaxEntries {
		entries = entries[len(entries)-settingsHistoryMaxEntries:]
	}
	// newest first for the UI
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if entries == nil {
		entries = []settingsHistoryEntry{}
	}
	writeJSON(w, entries)
}

// recordSettingsChange is the single hook handleSettings' POST branch
// calls after a successful update: diff, persist, and return the audit
// summary line (so the caller logs exactly what was recorded). source is
// "import" for a Settings-file upload, "" for a normal form save — see
// settingsHistoryEntry.Source.
func recordSettingsChange(r *http.Request, before, after appSettings, source string) string {
	changes := diffSettings(before, after)
	if len(changes) == 0 {
		return summarizeSettingsChanges(nil)
	}
	appendSettingsHistory(settingsHistoryEntry{
		Time:    time.Now().Unix(),
		Actor:   actorFromRequest(r),
		Changes: changes,
		Source:  source,
	})
	return summarizeSettingsChanges(changes)
}
