package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServeStaticAssetSendsETagAndSupportsConditionalGet guards the fix for
// static embedded assets (style.css/app.js/i18n.js/novapop.js) always being
// re-transferred in full on every page load: a first request must return
// the body with an ETag, and a follow-up request presenting that ETag via
// If-None-Match must get a bodyless 304 instead of the content again.
func TestServeStaticAssetSendsETagAndSupportsConditionalGet(t *testing.T) {
	handler := serveStaticAsset("text/css; charset=utf-8", "body content")

	first := httptest.NewRecorder()
	handler(first, httptest.NewRequest(http.MethodGet, "/style.css", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first request: want 200, got %d", first.Code)
	}
	if first.Body.String() != "body content" {
		t.Fatalf("first request: want the body served, got %q", first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("want a non-empty ETag on the first response")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("want Cache-Control: no-cache, got %q", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	handler(second, req)
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional request: want 304, got %d", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 response must have no body, got %q", second.Body.String())
	}
}

// TestServeStaticAssetETagChangesWithContent confirms two different asset
// bodies get different ETags — otherwise a stale cached copy of a DIFFERENT
// file could be served as a false 304.
func TestServeStaticAssetETagChangesWithContent(t *testing.T) {
	h1 := serveStaticAsset("text/plain", "one")
	h2 := serveStaticAsset("text/plain", "two")

	rec1 := httptest.NewRecorder()
	h1(rec1, httptest.NewRequest(http.MethodGet, "/a", nil))
	rec2 := httptest.NewRecorder()
	h2(rec2, httptest.NewRequest(http.MethodGet, "/b", nil))

	if rec1.Header().Get("ETag") == rec2.Header().Get("ETag") {
		t.Fatal("want different content to produce different ETags")
	}
}
