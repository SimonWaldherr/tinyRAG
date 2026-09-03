package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsSafeWebURLRejectsPrivateAndNonHTTP(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"http://127.0.0.1:8080/x", true},
		{"http://localhost/x", true},
		{"http://169.254.169.254/latest/meta-data", true},
		{"https://user:password@example.org/private", true},
		{"ftp://example.org/x", true},
		{"not a url at all://", true},
	}
	for _, c := range cases {
		if _, err := isSafeWebURL(c.url); (err != nil) != c.wantErr {
			t.Errorf("isSafeWebURL(%q): err=%v, wantErr=%v", c.url, err, c.wantErr)
		}
	}
}

func TestSafeWebFetchRedirectPolicyRejectsPrivateTarget(t *testing.T) {
	client := newSafeWebFetchClient(false)
	privateRedirect := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/private", nil)
	if err := client.CheckRedirect(privateRedirect, nil); err == nil {
		t.Fatal("want a redirect to a private target to be rejected")
	}
}

func TestFetchWebPageRawRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(strings.Repeat("x", 33)))
	}))
	defer server.Close()

	webAllowPrivateHosts = true
	t.Cleanup(func() { webAllowPrivateHosts = false })
	if _, _, err := fetchWebPageRaw(t.Context(), server.URL, 32, false); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want oversized page rejection, got %v", err)
	}
}

func TestImportWebPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head><title>Test Page</title></head><body><p>This is a long enough paragraph of body text to be chunked and embedded.</p></body></html>`))
		case "/badtype":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.4 fake"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	webAllowPrivateHosts = true
	t.Cleanup(func() { webAllowPrivateHosts = false })

	rag, s := newTestRAG(t)
	urls := []string{server.URL + "/ok", server.URL + "/badtype", server.URL + "/missing"}

	var progressCalls int
	res := importWebPages(context.Background(), rag, s, "test-embed", urls, false, func(p webProgress) { progressCalls++ })

	if res.Pages != 1 {
		t.Fatalf("want 1 successfully fetched page, got %d (errors: %v)", res.Pages, res.Errors)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("want 2 errors (bad content-type + 404), got %d: %v", len(res.Errors), res.Errors)
	}
	if res.Chunks == 0 {
		t.Fatalf("expected at least one chunk to be ingested")
	}
	if progressCalls != len(urls) {
		t.Fatalf("want %d progress callbacks, got %d", len(urls), progressCalls)
	}

	sources, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	found := false
	for _, src := range sources {
		if src.SourceID == "web:"+server.URL+"/ok" {
			found = true
			if src.SourceKind != "web_page" {
				t.Errorf("want source_kind web_page, got %s", src.SourceKind)
			}
			if src.SourceName != "Test Page" {
				t.Errorf("want source_name %q (from <title>), got %q", "Test Page", src.SourceName)
			}
		}
	}
	if !found {
		t.Fatalf("expected source web:%s/ok to have been ingested, got %+v", server.URL, sources)
	}
}

func TestExtractHTMLLinks(t *testing.T) {
	base, err := url.Parse("https://example.org/dir/page.html")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	rawHTML := `
		<a href="/absolute-path">A</a>
		<a href="relative.html">B</a>
		<a href="https://example.org/dir/relative.html#frag">B again, with fragment</a>
		<a href="https://other.example.com/x">external</a>
		<a href="mailto:someone@example.com">mail</a>
		<a href="javascript:void(0)">js</a>
		<a href="#just-a-fragment">fragment only</a>
		<a href="">empty</a>
		<a not-href="oops">no href attr</a>
	`
	got := extractHTMLLinks(rawHTML, base)
	want := []string{
		"https://example.org/absolute-path",
		"https://example.org/dir/relative.html",
		"https://other.example.com/x",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d links, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("link %d: want %q, got %q (full: %v)", i, w, got[i], got)
		}
	}
}

func TestCrawlWebPagesRespectsDepthAndHost(t *testing.T) {
	var server *httptest.Server
	page := func(title string, links ...string) string {
		var b strings.Builder
		b.WriteString("<html><head><title>" + title + "</title></head><body>")
		b.WriteString("<p>This is a long enough paragraph of body text to be chunked and embedded properly for the test.</p>")
		for _, l := range links {
			b.WriteString(`<a href="` + l + `">link</a>`)
		}
		b.WriteString("</body></html>")
		return b.String()
	}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/seed":
			_, _ = w.Write([]byte(page("Seed", "/depth1a", "/depth1b", "http://other-host.invalid/external")))
		case "/depth1a":
			_, _ = w.Write([]byte(page("Depth1A", "/depth2")))
		case "/depth1b":
			_, _ = w.Write([]byte(page("Depth1B")))
		case "/depth2":
			_, _ = w.Write([]byte(page("Depth2")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	webAllowPrivateHosts = true
	t.Cleanup(func() { webAllowPrivateHosts = false })

	rag, s := newTestRAG(t)
	seed := server.URL + "/seed"

	// maxDepth=1: seed + its two same-host links, not the depth-2 page,
	// not the external-host link (allowOtherHosts defaults to false).
	res := crawlWebPages(context.Background(), rag, s, "test-embed", []string{seed}, 1, 10, false, false, nil)
	if res.Pages != 3 {
		t.Fatalf("maxDepth=1: want 3 pages (seed + 2 depth-1), got %d (errors: %v)", res.Pages, res.Errors)
	}

	// maxDepth=2: also reaches the depth-2 page via depth1a.
	res = crawlWebPages(context.Background(), rag, s, "test-embed", []string{seed}, 2, 10, false, false, nil)
	if res.Pages != 4 {
		t.Fatalf("maxDepth=2: want 4 pages (seed + 2 depth-1 + 1 depth-2), got %d (errors: %v)", res.Pages, res.Errors)
	}

	// maxPages=2 truncates the crawl before exhausting all discoverable pages.
	res = crawlWebPages(context.Background(), rag, s, "test-embed", []string{seed}, 2, 2, false, false, nil)
	if res.Pages != 2 {
		t.Fatalf("maxPages=2: want exactly 2 pages fetched, got %d (errors: %v)", res.Pages, res.Errors)
	}

	// allowOtherHosts=true: the external-host link is now attempted too
	// (and fails, since other-host.invalid doesn't resolve — that's fine,
	// the point is it was tried, unlike the allowOtherHosts=false cases
	// above where it's never even attempted).
	res = crawlWebPages(context.Background(), rag, s, "test-embed", []string{seed}, 1, 10, true, false, nil)
	foundExternalAttempt := false
	for _, e := range res.Errors {
		if strings.Contains(e, "other-host.invalid") {
			foundExternalAttempt = true
		}
	}
	if !foundExternalAttempt {
		t.Fatalf("allowOtherHosts=true: want an attempt (and failure) to fetch the external-host link, got errors: %v", res.Errors)
	}
}

func TestExtractHTMLTitle(t *testing.T) {
	html := `<html><head><title>  Hello &amp; World  </title></head><body></body></html>`
	if got := extractHTMLTitle(html); got != "Hello & World" {
		t.Fatalf("want %q, got %q", "Hello & World", got)
	}
	if got := extractHTMLTitle("<html><body>no title here</body></html>"); got != "" {
		t.Fatalf("want empty string for no <title>, got %q", got)
	}
}
