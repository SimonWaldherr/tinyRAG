package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleChunksSearchTestReturnsRankedHits is the core guarantee behind
// the Chunks tab's "Testsuche": it must run the SAME rankedSearch (rank.go)
// a real question would, unrestricted, and return the actual scored hits —
// not a reimplementation, the real thing. The fake embed server
// (newTestRAG) returns an identical vector for every input, so this seeds
// two sources with different content and relies on keyword overlap to
// distinguish them, confirming the query actually reaches real content
// rather than returning arbitrary/empty results.
func TestHandleChunksSearchTestReturnsRankedHits(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	if _, err := rag.replaceSourceChunks(sourceChunks{
		SourceID: "doc-schweissen", SourceKind: "file", SourceName: "schweissen.md",
		Chunks: []string{"Beim Schweißen ist ein Schweißerschutzschild Pflicht."},
	}, s.activeEmbedModel()); err != nil {
		t.Fatalf("seed doc-schweissen: %v", err)
	}
	if _, err := rag.replaceSourceChunks(sourceChunks{
		SourceID: "doc-urlaub", SourceKind: "file", SourceName: "urlaub.md",
		Chunks: []string{"Urlaubsantrag über das interne Portal stellen."},
	}, s.activeEmbedModel()); err != nil {
		t.Fatalf("seed doc-urlaub: %v", err)
	}

	body, _ := json.Marshal(chunkSearchTestRequest{Query: "Schweißen Schutzausrüstung"})
	rec := httptest.NewRecorder()
	handleChunksSearchTest(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/chunks/search-test", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var res chunkSearchTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("want at least one hit, got none")
	}
	if res.Hits[0].SourceID != "doc-schweissen" {
		t.Fatalf("want the keyword-matching source ranked first, got %+v", res.Hits)
	}
}

// TestHandleChunksSearchTestRejectsEmptyQuery confirms the 400 guard —
// an empty query would just embed "" and return a meaningless top-k.
func TestHandleChunksSearchTestRejectsEmptyQuery(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(chunkSearchTestRequest{Query: "   "})
	rec := httptest.NewRecorder()
	handleChunksSearchTest(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/chunks/search-test", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an empty/whitespace query, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleChunksSearchTestRequiresAdminSession confirms the endpoint is
// wired through requireAdminSession — an unrestricted (adminDeptCode)
// search bypasses every SourceAccess/preset restriction, so it must never
// be reachable by a non-admin session.
func TestHandleChunksSearchTestRequiresAdminSession(t *testing.T) {
	rag, s := newTestRAG(t)
	s.LDAP.Enabled = true
	withTestGlobalSettings(t, s)

	rec := httptest.NewRecorder()
	requireAdminSession(handleChunksSearchTest(rag))(rec, httptest.NewRequest(http.MethodPost, "/api/chunks/search-test", bytes.NewReader([]byte(`{"query":"x"}`))))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without an admin session when LDAP is enabled, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}
