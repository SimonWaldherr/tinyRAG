package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// R3 self-ingestion — the admin-only, opt-in "yo dawg, I heard you like
// RAG" feature: load R3's own source code into R3's own vector store, so
// it can answer questions about its own implementation ("how does query
// rewriting work?", "which file handles the SharePoint delta sync?") the
// same way it answers a question about any other imported document —
// citations included, pointing back at the actual source file.
//
// Deliberately narrower than ingestFolder (which happily walks anything an
// admin points it at, extension list included): this walks the server's
// OWN working directory (where the R3 process itself is running from —
// see README.md's "Quick start", the documented invocation is always
// `./R3`/`go run .` from the repo root) and applies a hard-coded allow
// list of source-code extensions plus an exclude list of directories/
// filenames that must never end up in a searchable, citable vector store.
// Most importantly: settings.json holds live secrets in a real deployment
// (LDAP bind credentials, API keys, OAuth client secrets — see settings.
// go's own "***" masking convention for why this data is sensitive) and
// must never be embedded where a chat citation could surface it verbatim.
// ─────────────────────────────────────────────────────────────────────────────

// r3SourceKind is the source_kind every file ingested by ingestR3Source is
// tagged with — a normal, citable kind (not hidden by default) since
// showing "this answer is grounded in agent.go" is the entire point of
// the feature, not something to obscure. An admin who wants it hidden can
// still do so via the existing settings.source_visibility mechanism, same
// as any other source_kind.
const r3SourceKind = "r3_source"

// r3SourceExtensions is the allow list of source file types this feature
// ingests — just the languages/markup R3 is actually written in, not
// every extension the generic importer recognizes. Notably excludes
// .json: settings.json (and this repo's own verify*-settings.json test
// fixtures) carry either live secrets or pure noise, never something
// worth citing.
func r3SourceExtensions() map[string]bool {
	return map[string]bool{
		".go": true, ".js": true, ".css": true, ".html": true, ".md": true,
	}
}

// r3SourceExcludeDirNames skips these directory names anywhere in the
// walk (not just at the root): VCS metadata, vendored/reference code
// (external/ — see AGENTS.md, "kein Teil von R3"), agent session/worktree
// scratch space, locally-sensitive working notes (docs/ — see README.md's
// own ".gitignore'd ... local working notes" doc comment), and build
// output.
var r3SourceExcludeDirNames = map[string]bool{
	".git": true, ".claude": true, "external": true, "node_modules": true,
	"docs": true, "bin": true, "dist": true, "build": true,
	"r3-originals": true,
}

// r3SourceExcludeDirPrefixes skips any directory whose name starts with
// one of these — covers storage.path's default "r3-data" plus every
// differently-named verify*/*-data fixture this repo's own tooling
// creates (see .claude/launch.json's r3-verify/r3-verify2/
// r3-verify-tinysql configs), without needing to enumerate each by name.
var r3SourceExcludeDirPrefixes = []string{"r3-data", "verify"}

// r3SourceExcludeFileRe matches filenames (checked against the basename
// only, case-insensitively) that must never be ingested regardless of
// extension — settings.json and its verify*-settings.json siblings are
// already excluded by extension (.json isn't in r3SourceExtensions), but
// this is deliberate defense-in-depth: it also catches a stray
// "*secret*"/"*credentials*"-named file with an otherwise-allowed
// extension, and stays correct if the extension allow-list ever changes.
var r3SourceExcludeFileRe = regexp.MustCompile(`(?i)(settings.*\.json|\.env$|credentials|secret|\.key$|\.pem$)`)

// r3SourceSkipDir reports whether dirName (one path segment, not a full
// path) should be pruned entirely from the walk.
func r3SourceSkipDir(dirName string) bool {
	if r3SourceExcludeDirNames[dirName] {
		return true
	}
	lower := strings.ToLower(dirName)
	for _, prefix := range r3SourceExcludeDirPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// r3SourceSkipFile reports whether one candidate file should be excluded —
// wrong extension, or matches the credential/secret filename guard.
func r3SourceSkipFile(path string) bool {
	if !r3SourceExtensions()[strings.ToLower(filepath.Ext(path))] {
		return true
	}
	return r3SourceExcludeFileRe.MatchString(strings.ToLower(filepath.Base(path)))
}

// ingestR3Source recursively imports R3's own source tree (rooted at root,
// typically ".") into the vector store as source_kind r3SourceKind — see
// the file header for why this is deliberately narrower than the generic
// ingestFolder. Each file's source_id is "r3source:<repo-relative path>"
// (forward-slash normalized, stable across OSes) so re-running this after
// a code change replaces exactly that file's chunks via the same
// content-hash skip/replace path (ingestDocument) every other importer
// uses — nothing duplicates, nothing goes stale.
func ingestR3Source(ctx context.Context, rag *ragSystem, s appSettings, embedModel, root string, dryRun bool) ([]ingestOutcome, error) {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", root)
	}

	var results []ingestOutcome
	walkErr := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry (permissions, race with a concurrent delete): skip, don't abort the whole walk
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if path != absRoot && r3SourceSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if r3SourceSkipFile(path) {
			return nil
		}
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		sourceID := "r3source:" + rel

		text, extractErr := extractText(path, s)
		if extractErr != nil {
			results = append(results, ingestOutcome{SourceID: sourceID, SourceName: rel, Error: extractErr.Error()})
			return nil
		}
		var docDate int64
		if fi, statErr := d.Info(); statErr == nil {
			docDate = fi.ModTime().Unix()
		}
		out, ingestErr := ingestDocument(rag, s, embedModel, sourceID, r3SourceKind, rel, text, docDate, dryRun)
		if ingestErr != nil {
			out.Error = ingestErr.Error()
		}
		results = append(results, out)
		return nil
	})
	if verbose {
		log.Printf("[verbose] ingestR3Source %s: %d file(s) processed, dry_run=%v", absRoot, len(results), dryRun)
	}
	return results, walkErr
}
