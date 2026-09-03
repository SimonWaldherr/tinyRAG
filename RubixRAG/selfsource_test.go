package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR3SourceSkipDir(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{".git", true},
		{".claude", true},
		{"external", true},
		{"node_modules", true},
		{"docs", true},
		{"bin", true},
		{"dist", true},
		{"build", true},
		{"r3-originals", true},
		{"r3-data", true},
		{"r3-data-preview", true},
		{"verify-tinysql-data", true},
		{"verify2", true},
		{"web", false},
		{"prompts", false},
		{".", false},
	}
	for _, c := range cases {
		if got := r3SourceSkipDir(c.name); got != c.want {
			t.Errorf("r3SourceSkipDir(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestR3SourceSkipFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"main.go", false},
		{"web/app.js", false},
		{"web/style.css", false},
		{"web/templates/tab-chat.html", false},
		{"README.md", false},
		{"whisper_test.go", false},
		// wrong extension entirely
		{"go.mod", true},
		{"go.sum", true},
		{"llms.txt", true},
		{"docs/openapi.json", true},
		// secrets/credentials guard — must hold even if the extension were
		// somehow allowed (defense in depth, see r3SourceExcludeFileRe's
		// doc comment)
		{"settings.json", true},
		{"verify2-settings.json", true},
		{".env", true},
		{"credentials.md", true},
		{"my-secret-notes.md", true},
		{"server.pem", true},
		{"private.key", true},
	}
	for _, c := range cases {
		if got := r3SourceSkipFile(c.path); got != c.want {
			t.Errorf("r3SourceSkipFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestIngestR3SourceExcludesSensitiveFilesAndDirs builds a small fake repo
// layout — a couple of real source files alongside exactly the sensitive/
// excluded shapes production R3 actually has (settings.json with a fake
// secret, a .git dir, an external/ reference project, a docs/ note, an
// r3-data-preview storage dir) — and confirms only the safe source files
// get ingested, with the sensitive content never reaching the store at all
// (not just hidden from citations).
func TestIngestR3SourceExcludesSensitiveFilesAndDirs(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("main.go", "package main\n\nfunc main() {}\n// SENTINEL-MAIN-GO long enough to be a real chunk of source code here.\n")
	write("web/style.css", ".btn { color: red; } /* SENTINEL-CSS long enough to be chunked properly here */\n")
	write("README.md", "# R3\n\nSENTINEL-README long enough to be chunked and embedded properly here.\n")
	write("settings.json", `{"ldap":{"bind_password":"SUPER-SECRET-PASSWORD-MUST-NEVER-BE-INGESTED"}}`)
	write(".git/config", "[core]\nSENTINEL-GITCONFIG\n")
	write("external/zndz/zndz.go", "package main\n// SENTINEL-EXTERNAL should never be ingested by R3's own self-import.\n")
	write("docs/DEPLOYMENT.md", "# SENTINEL-DOCS internal deployment notes, never ingested.\n")
	write("r3-data-preview/chunks.db", "not real db content, just needs to exist")
	write("node_modules/pkg/index.js", "// SENTINEL-NODEMODULES\n")

	rag, s := newTestRAG(t)
	results, err := ingestR3Source(t.Context(), rag, s, "test-embed", root, false)
	if err != nil {
		t.Fatalf("ingestR3Source: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, r := range results {
		gotIDs[r.SourceID] = true
	}
	wantIncluded := []string{"r3source:main.go", "r3source:web/style.css", "r3source:README.md"}
	for _, id := range wantIncluded {
		if !gotIDs[id] {
			t.Errorf("want %s ingested, got results %+v", id, results)
		}
	}
	if len(results) != len(wantIncluded) {
		t.Fatalf("want exactly %d ingested files, got %d: %+v", len(wantIncluded), len(results), results)
	}

	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	for _, src := range sources {
		if src.SourceKind != r3SourceKind {
			t.Errorf("want every ingested source tagged %q, got %q for %s", r3SourceKind, src.SourceKind, src.SourceID)
		}
	}

	// The real assertion that matters: the secret string must never have
	// reached the store at all, under ANY source_id — not just excluded
	// from citations.
	for _, src := range sources {
		content, ok := rag.fetchSourceContent(src.SourceID)
		if ok && strings.Contains(content, "SUPER-SECRET-PASSWORD") {
			t.Fatalf("settings.json secret leaked into the vector store under %s", src.SourceID)
		}
		if ok && (strings.Contains(content, "SENTINEL-GITCONFIG") || strings.Contains(content, "SENTINEL-EXTERNAL") || strings.Contains(content, "SENTINEL-DOCS") || strings.Contains(content, "SENTINEL-NODEMODULES")) {
			t.Fatalf("excluded directory content leaked into the vector store under %s: %q", src.SourceID, content)
		}
	}
}

// TestIngestR3SourceDryRunWritesNothing confirms dry_run behaves exactly
// like every other importer's dry-run contract (ingest.go's ingestDocument
// doc comment): chunk counts are reported but nothing is actually stored.
func TestIngestR3SourceDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n// long enough sentinel content to produce a real chunk here.\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rag, s := newTestRAG(t)
	results, err := ingestR3Source(t.Context(), rag, s, "test-embed", root, true)
	if err != nil {
		t.Fatalf("ingestR3Source: %v", err)
	}
	if len(results) != 1 || !results[0].DryRun {
		t.Fatalf("want one dry-run result, got %+v", results)
	}
	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("dry run must not write anything, got sources %+v", sources)
	}
}

// TestIngestR3SourceRejectsNonDirectory guards the same "not a directory"
// error handleImportFolder already returns for a bogus path.
func TestIngestR3SourceRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "not-a-dir.go")
	if err := os.WriteFile(filePath, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	rag, s := newTestRAG(t)
	if _, err := ingestR3Source(t.Context(), rag, s, "test-embed", filePath, false); err == nil {
		t.Fatal("want an error when root is a file, not a directory")
	}
}

// TestHandleImportSelfSourceEndToEnd drives the actual HTTP handler (not
// just ingestR3Source directly), confirming the wire contract: a POST body
// decodes into importSelfSourceRequest, the response is the same
// []ingestOutcome shape every other file-based importer (upload/folder)
// already returns, and dry_run round-trips correctly.
func TestHandleImportSelfSourceEndToEnd(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n// sentinel content long enough to be chunked and embedded properly here.\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"secret":"must-not-leak"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(importSelfSourceRequest{Root: root, DryRun: false})
	rec := httptest.NewRecorder()
	handleImportSelfSource(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/import/self-source", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var results []ingestOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(results) != 1 || results[0].SourceID != "r3source:main.go" {
		t.Fatalf("want exactly main.go ingested, got %+v", results)
	}

	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("want exactly one stored source, got %+v", sources)
	}
}

// TestHandleImportSelfSourceRejectsWrongMethod confirms the method guard.
func TestHandleImportSelfSourceRejectsWrongMethod(t *testing.T) {
	rag, _ := newTestRAG(t)
	rec := httptest.NewRecorder()
	handleImportSelfSource(rag)(rec, httptest.NewRequest(http.MethodGet, "/api/import/self-source", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}
