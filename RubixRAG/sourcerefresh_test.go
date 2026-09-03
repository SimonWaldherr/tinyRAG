package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFindSharePointConnForSourceIDHandlesColonsInSiteURL confirms the
// prefix-match approach (not a naive strings.Split on ":") correctly
// separates SiteURL from itemPath even though the URL itself contains
// colons ("https://...").
func TestFindSharePointConnForSourceIDHandlesColonsInSiteURL(t *testing.T) {
	conns := []sharePointConfig{
		{Enabled: true, SiteURL: "https://groupiph.sharepoint.com/sites/intranet_de/Support/Logistik"},
	}
	sourceID := "sharepoint:https://groupiph.sharepoint.com/sites/intranet_de/Support/Logistik:Freigegebene Dokumente/FAQ.pdf"
	conn, itemPath, ok := findSharePointConnForSourceID(conns, sourceID)
	if !ok {
		t.Fatal("want a match")
	}
	if conn.SiteURL != conns[0].SiteURL {
		t.Fatalf("unexpected matched connection: %+v", conn)
	}
	if itemPath != "Freigegebene Dokumente/FAQ.pdf" {
		t.Fatalf("unexpected itemPath: %q", itemPath)
	}
}

// TestFindSharePointConnForSourceIDSkipsDisabled confirms a disabled
// connection is never matched, even if its SiteURL would otherwise fit —
// refreshing must fail clearly rather than silently using credentials the
// admin turned off.
func TestFindSharePointConnForSourceIDSkipsDisabled(t *testing.T) {
	conns := []sharePointConfig{
		{Enabled: false, SiteURL: "https://groupiph.sharepoint.com/sites/intranet_de/Support/Logistik"},
	}
	_, _, ok := findSharePointConnForSourceID(conns, "sharepoint:https://groupiph.sharepoint.com/sites/intranet_de/Support/Logistik:a.pdf")
	if ok {
		t.Fatal("want no match for a disabled connection")
	}
}

// TestRefreshSourceUnsupportedKindErrors confirms a source_kind
// refreshSource doesn't know how to re-fetch (e.g. a PST email, which has
// no server-kept original to re-download from) fails clearly rather than
// silently doing nothing or crashing.
func TestRefreshSourceUnsupportedKindErrors(t *testing.T) {
	_, err := refreshSource(context.Background(), nil, appSettings{}, "pst:Postfach.pst:123", "pst_email")
	if err == nil {
		t.Fatal("want an error for an unsupported source_kind")
	}
}

// TestRefreshSharePointFileUpdatesChangedContent drives the full refresh
// path for a sharepoint_file source: an initial import, then a second
// import with DIFFERENT content at the same path (simulating someone
// having edited the file in SharePoint since), then refreshSource — the
// chunks must reflect the NEW content afterwards.
func TestRefreshSharePointFileUpdatesChangedContent(t *testing.T) {
	content := "Version eins des Dokuments, lang genug für einen Chunk beim Import."
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.HasSuffix(r.URL.Path, "/download"):
			_, _ = w.Write([]byte(content))
		default:
			_, _ = w.Write([]byte(`{"value": [{"name": "a.txt", "size": 100, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}]}`))
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}

	sourceID := "sharepoint:" + cfg.SiteURL + ":a.txt"
	if _, err := ingestSharePointFile(rag, s, "test-embed", cfg.SiteURL, "a.txt", []byte(content), false); err != nil {
		t.Fatalf("initial ingest: %v", err)
	}
	before, ok := rag.fetchSourceContent(sourceID)
	if !ok || !strings.Contains(before, "Version eins") {
		t.Fatalf("unexpected content after initial ingest: %q (ok=%v)", before, ok)
	}

	content = "Version zwei des Dokuments — komplett anderer Inhalt, ebenfalls lang genug für einen Chunk."
	outcome, err := refreshSource(context.Background(), rag, s, sourceID, "sharepoint_file")
	if err != nil {
		t.Fatalf("refreshSource: %v", err)
	}
	if outcome.Skipped {
		t.Fatal("want the changed content NOT skipped")
	}
	after, ok := rag.fetchSourceContent(sourceID)
	if !ok {
		t.Fatal("source missing after refresh")
	}
	if !strings.Contains(after, "Version zwei") || strings.Contains(after, "Version eins") {
		t.Fatalf("want refreshed content to replace the old version, got %q", after)
	}
}

// TestRefreshSharePointFileSkipsUnchanged confirms refreshing a source
// whose content hasn't actually changed reports Skipped, matching
// ingestDocument's normal content-hash behavior — not a special case
// refreshSource adds itself.
func TestRefreshSharePointFileSkipsUnchanged(t *testing.T) {
	content := "Unveränderter Inhalt, lang genug für einen Chunk beim Import."
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.HasSuffix(r.URL.Path, "/download"):
			_, _ = w.Write([]byte(content))
		default:
			_, _ = w.Write([]byte(`{"value": [{"name": "a.txt", "size": 100, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}]}`))
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}
	sourceID := "sharepoint:" + cfg.SiteURL + ":a.txt"
	if _, err := ingestSharePointFile(rag, s, "test-embed", cfg.SiteURL, "a.txt", []byte(content), false); err != nil {
		t.Fatalf("initial ingest: %v", err)
	}

	outcome, err := refreshSource(context.Background(), rag, s, sourceID, "sharepoint_file")
	if err != nil {
		t.Fatalf("refreshSource: %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("want unchanged content reported as Skipped")
	}
}

// TestRefreshSharePointFileMissingErrors confirms a file no longer present
// at its old path (renamed/deleted since import) fails clearly instead of
// silently doing nothing.
func TestRefreshSharePointFileMissingErrors(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		default:
			_, _ = w.Write([]byte(`{"value": []}`))
		}
	})
	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}
	sourceID := "sharepoint:" + cfg.SiteURL + ":gone.docx"
	if _, err := refreshSource(context.Background(), rag, s, sourceID, "sharepoint_file"); err == nil {
		t.Fatal("want an error when the file is no longer listed")
	}
}

// TestRefreshSharePointPageRefetchesText drives the page refresh path —
// re-lists pages by name (the page's Graph ID isn't itself stored in
// sourceID), refetches its canvasLayout text, and replaces its chunks.
func TestRefreshSharePointPageRefetchesText(t *testing.T) {
	pageText := "<p>Alter Text, lang genug für einen Chunk.</p>"
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.Contains(r.URL.Path, "/pages/p1/microsoft.graph.sitePage"):
			_, _ = w.Write([]byte(`{"title": "Seite A", "canvasLayout": {"horizontalSections": [{"columns": [{"webparts": [{"innerHtml": "` + pageText + `"}]}]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"value": [{"id": "p1", "name": "a.aspx", "title": "Seite A"}]}`))
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}
	sourceID := "sharepoint:" + cfg.SiteURL + ":SitePages/a.aspx"
	if _, err := ingestSharePointPage(rag, s, "test-embed", cfg.SiteURL, "a.aspx", "Seite A", "Alter Text, lang genug für einen Chunk.", false); err != nil {
		t.Fatalf("initial ingest: %v", err)
	}

	pageText = "<p>Neuer, komplett anderer Text nach der Bearbeitung in SharePoint.</p>"
	outcome, err := refreshSource(context.Background(), rag, s, sourceID, "sharepoint_page")
	if err != nil {
		t.Fatalf("refreshSource: %v", err)
	}
	if outcome.Skipped {
		t.Fatal("want the changed page text NOT skipped")
	}
	after, ok := rag.fetchSourceContent(sourceID)
	if !ok || !strings.Contains(after, "komplett anderer Text") {
		t.Fatalf("want refreshed page text, got %q (ok=%v)", after, ok)
	}
}

// TestRefreshSharePointSharedLinkRefetches drives the share-link refresh
// path — re-resolves the link and re-downloads+re-ingests its current
// content.
func TestRefreshSharePointSharedLinkRefetches(t *testing.T) {
	shareURL := "https://groupiph.sharepoint.com/:b:/s/intranet_de/Allgemein/refresh-me?e=1"
	sharePath := "/shares/" + spEncodeShareURL(shareURL) + "/driveItem"
	content := "Alter Inhalt, lang genug für einen Chunk beim Import."
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == sharePath:
			_, _ = w.Write([]byte(`{"name": "notiz.txt", "size": 50, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download-notiz"}`))
		case strings.HasSuffix(r.URL.Path, "/download-notiz"):
			_, _ = w.Write([]byte(content))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}
	sourceID := "sharepoint_link:" + shareURL
	if _, err := ingestSharePointSharedLink(rag, s, "test-embed", shareURL, "notiz.txt", []byte(content), false); err != nil {
		t.Fatalf("initial ingest: %v", err)
	}

	content = "Neuer Inhalt nach der Bearbeitung, ebenfalls lang genug für einen Chunk."
	outcome, err := refreshSource(context.Background(), rag, s, sourceID, "sharepoint_link")
	if err != nil {
		t.Fatalf("refreshSource: %v", err)
	}
	if outcome.Skipped {
		t.Fatal("want the changed content NOT skipped")
	}
	after, ok := rag.fetchSourceContent(sourceID)
	if !ok || !strings.Contains(after, "Neuer Inhalt") {
		t.Fatalf("want refreshed content, got %q (ok=%v)", after, ok)
	}
}

// TestRefreshSourceWebPage: the Quellen-Übersicht "Neu laden" action must
// re-fetch a web_page source's URL and replace its chunks when the page
// changed — previously every non-SharePoint kind got "not supported".
func TestRefreshSourceWebPage(t *testing.T) {
	content := `<html><head><title>Seite</title></head><body><p>Erste Fassung des Inhalts, lang genug für einen ordentlichen Chunk im Test.</p></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()
	webAllowPrivateHosts = true
	t.Cleanup(func() { webAllowPrivateHosts = false })

	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)
	res := importWebPages(context.Background(), rag, s, "test-embed", []string{server.URL + "/seite"}, false, nil)
	if res.Pages != 1 {
		t.Fatalf("setup import failed: %+v", res)
	}
	sourceID := "web:" + server.URL + "/seite"

	// Unchanged page: refresh reports Skipped.
	out, err := refreshSource(context.Background(), rag, s, sourceID, "web_page")
	if err != nil {
		t.Fatalf("refreshSource (unchanged): %v", err)
	}
	if !out.Skipped {
		t.Fatalf("unchanged page must be hash-skipped, got %+v", out)
	}

	// Changed page: refresh replaces the stored chunks.
	content = `<html><head><title>Seite</title></head><body><p>ZWEITE Fassung mit neuem Inhalt, ebenfalls lang genug für einen Chunk.</p></body></html>`
	out, err = refreshSource(context.Background(), rag, s, sourceID, "web_page")
	if err != nil {
		t.Fatalf("refreshSource (changed): %v", err)
	}
	if out.Skipped || out.Chunks == 0 {
		t.Fatalf("changed page must re-ingest, got %+v", out)
	}
	stored, ok := rag.fetchSourceContent(sourceID)
	if !ok || !strings.Contains(stored, "ZWEITE Fassung") {
		t.Fatalf("stored content must be the new version, got ok=%v %q", ok, stored)
	}

	// A source_id without the web: prefix is rejected clearly.
	if _, err := refreshSource(context.Background(), rag, s, "upload:x.txt", "web_page"); err == nil {
		t.Fatalf("want an error for a non-web source_id")
	}
}
