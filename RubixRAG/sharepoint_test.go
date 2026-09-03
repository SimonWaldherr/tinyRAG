package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func testSharePointConfig() sharePointConfig {
	return sharePointConfig{
		Enabled: true, TenantID: "tenant", ClientID: "client", ClientSecret: "secret",
		SiteURL: "https://rubix.sharepoint.com/sites/Vertrieb",
	}
}

// TestSpDeltaSyncInitialFollowsNextLinkThenReturnsDeltaLink confirms a
// multi-page delta changeset (an @odata.nextLink page followed by an
// @odata.deltaLink page) is fully consumed in one spDeltaSync call, and
// that folders/deleted items are classified correctly instead of being
// treated as importable files.
func TestSpDeltaSyncInitialFollowsNextLinkThenReturnsDeltaLink(t *testing.T) {
	var calls []string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.Contains(r.URL.Path, "/page2"):
			_, _ = w.Write([]byte(`{
				"value": [
					{"id": "item-b", "name": "b.pdf", "size": 200, "parentReference": {"path": "/drive/root:/Docs"}, "@microsoft.graph.downloadUrl": "https://dl/b"},
					{"id": "item-old", "name": "old.pdf", "deleted": {"state": "deleted"}, "parentReference": {"path": "/drive/root:"}}
				],
				"@odata.deltaLink": "` + graphBaseURL + `/sites/site-1/drive/root/delta?token=resume-here"
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"value": [
					{"id": "item-a", "name": "a.docx", "size": 100, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "https://dl/a"},
					{"id": "item-sub", "name": "Subfolder", "folder": {"childCount": 1}, "parentReference": {"path": "/drive/root:"}}
				],
				"@odata.nextLink": "` + graphBaseURL + `/sites/site-1/drive/root/delta/page2"
			}`))
		}
	})

	items, deleted, newDeltaLink, err := spDeltaSync(context.Background(), testSharePointConfig())
	if err != nil {
		t.Fatalf("spDeltaSync: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 files (folder excluded), got %d: %+v", len(items), items)
	}
	if items[0].Path != "a.docx" || items[1].Path != "Docs/b.pdf" {
		t.Fatalf("unexpected item paths: %+v", items)
	}
	if len(deleted) != 1 || deleted[0].Path != "old.pdf" {
		t.Fatalf("want deleted=[old.pdf], got %+v", deleted)
	}
	if !strings.Contains(newDeltaLink, "token=resume-here") {
		t.Fatalf("want deltaLink to be persisted verbatim, got %q", newDeltaLink)
	}
}

// TestSpDeltaSyncResumesFromStoredDeltaLink confirms a non-empty
// cfg.DeltaLink is used directly as the next request, skipping the
// site-ID lookup's own initial "/root/delta" path.
func TestSpDeltaSyncResumesFromStoredDeltaLink(t *testing.T) {
	var gotPath string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
			return
		}
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value": [], "@odata.deltaLink": "` + graphBaseURL + `/resumed/delta"}`))
	})

	cfg := testSharePointConfig()
	cfg.DeltaLink = graphBaseURL + "/sites/site-1/drive/root/delta?token=old"
	_, _, _, err := spDeltaSync(context.Background(), cfg)
	if err != nil {
		t.Fatalf("spDeltaSync: %v", err)
	}
	if gotPath != "/sites/site-1/drive/root/delta" {
		t.Fatalf("want the stored delta link's path to be requested, got %q", gotPath)
	}
}

// TestSpDeltaSyncDedupesLastOccurrenceWinsWithinOneWalk confirms that when
// the same item id appears twice within a single delta walk — once as an
// add on page 1, then as a delete on page 2 — only the final ("deleted")
// state is reported. Microsoft's own delta-query docs call this replay
// behavior out explicitly and say only the last occurrence should be
// applied; treating the two pages as independent, unrelated slices (the
// pre-fix behavior) would incorrectly re-add an item that is, in truth,
// already gone.
func TestSpDeltaSyncDedupesLastOccurrenceWinsWithinOneWalk(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.Contains(r.URL.Path, "/page2"):
			_, _ = w.Write([]byte(`{
				"value": [
					{"id": "item-a", "name": "a.docx", "deleted": {"state": "deleted"}, "parentReference": {"path": "/drive/root:"}}
				],
				"@odata.deltaLink": "` + graphBaseURL + `/sites/site-1/drive/root/delta?token=resume-here"
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"value": [
					{"id": "item-a", "name": "a.docx", "size": 100, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "https://dl/a"}
				],
				"@odata.nextLink": "` + graphBaseURL + `/sites/site-1/drive/root/delta/page2"
			}`))
		}
	})

	items, deleted, _, err := spDeltaSync(context.Background(), testSharePointConfig())
	if err != nil {
		t.Fatalf("spDeltaSync: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("want the later delete to win over the earlier add, got items=%+v", items)
	}
	if len(deleted) != 1 || deleted[0].ID != "item-a" {
		t.Fatalf("want deleted=[item-a], got %+v", deleted)
	}
}

// TestDeltaSyncSharePointReconcilesRenamedItem confirms that when an
// already-known item id reappears at a different path (a rename or
// move), the old path's source is deleted once the new path has ingested
// successfully, and the returned item-path map reflects only the new
// path — without this, Graph's delta feed never re-emits a delete for
// the item's previous path (nor for any descendant, on a folder
// rename/move), leaving permanent orphaned/duplicated content.
func TestDeltaSyncSharePointReconcilesRenamedItem(t *testing.T) {
	var page int
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.HasSuffix(r.URL.Path, "/download"):
			_, _ = w.Write([]byte("Inhalt lang genug für einen Chunk beim Import, unverändert über beide Läufe."))
		default:
			page++
			if page == 1 {
				_, _ = w.Write([]byte(`{
					"value": [{"id": "item-a", "name": "a.txt", "size": 100, "parentReference": {"path": "/drive/root:/Old"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}],
					"@odata.deltaLink": "` + graphBaseURL + `/delta?token=1"
				}`))
			} else {
				_, _ = w.Write([]byte(`{
					"value": [{"id": "item-a", "name": "a.txt", "size": 100, "parentReference": {"path": "/drive/root:/New"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}],
					"@odata.deltaLink": "` + graphBaseURL + `/delta?token=2"
				}`))
			}
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}

	res1, deltaLink1, itemPaths1, err := deltaSyncSharePoint(t.Context(), rag, s, cfg, "test-embed", false, nil)
	if err != nil {
		t.Fatalf("deltaSyncSharePoint (1st): %v", err)
	}
	if res1.Chunks == 0 {
		t.Fatalf("want the file ingested under its old path, got %+v", res1)
	}
	if itemPaths1["item-a"] != "Old/a.txt" {
		t.Fatalf("want item-a tracked at Old/a.txt, got %+v", itemPaths1)
	}
	oldSourceID := "sharepoint:" + cfg.SiteURL + ":Old/a.txt"
	if sources, _ := rag.listSources(); !containsSourceID(sources, oldSourceID) {
		t.Fatalf("want %s ingested after 1st sync", oldSourceID)
	}

	cfg.DeltaLink = deltaLink1
	cfg.ItemPaths = itemPaths1
	res2, _, itemPaths2, err := deltaSyncSharePoint(t.Context(), rag, s, cfg, "test-embed", false, nil)
	if err != nil {
		t.Fatalf("deltaSyncSharePoint (2nd): %v", err)
	}
	if len(res2.Errors) != 0 {
		t.Fatalf("want no errors reconciling the rename, got %v", res2.Errors)
	}
	if itemPaths2["item-a"] != "New/a.txt" {
		t.Fatalf("want item-a tracked at New/a.txt after the rename, got %+v", itemPaths2)
	}
	newSourceID := "sharepoint:" + cfg.SiteURL + ":New/a.txt"
	sources, _ := rag.listSources()
	if !containsSourceID(sources, newSourceID) {
		t.Fatalf("want %s ingested after the rename, got %+v", newSourceID, sources)
	}
	if containsSourceID(sources, oldSourceID) {
		t.Fatalf("want %s cleaned up after the rename, got %+v", oldSourceID, sources)
	}
}

func containsSourceID(sources []sourceInfo, id string) bool {
	for _, src := range sources {
		if src.SourceID == id {
			return true
		}
	}
	return false
}

// TestDeltaSyncSharePointIngestsAndDeletes drives deltaSyncSharePoint
// end-to-end: a first sync ingests a new file, a second sync (simulating
// Graph reporting that file as deleted) removes its chunks again.
func TestDeltaSyncSharePointIngestsAndDeletes(t *testing.T) {
	deletedNow := false
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.HasSuffix(r.URL.Path, "/download"):
			_, _ = w.Write([]byte("Inhalt der Datei, lang genug für einen Chunk beim Import."))
		default:
			if deletedNow {
				_, _ = w.Write([]byte(`{
					"value": [{"name": "a.txt", "deleted": {"state": "deleted"}, "parentReference": {"path": "/drive/root:"}}],
					"@odata.deltaLink": "` + graphBaseURL + `/delta?token=2"
				}`))
			} else {
				_, _ = w.Write([]byte(`{
					"value": [{"name": "a.txt", "size": 100, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}],
					"@odata.deltaLink": "` + graphBaseURL + `/delta?token=1"
				}`))
			}
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}

	res, deltaLink1, _, err := deltaSyncSharePoint(t.Context(), rag, s, cfg, "test-embed", false, nil)
	if err != nil {
		t.Fatalf("deltaSyncSharePoint (1st): %v", err)
	}
	if res.Files != 1 || res.Chunks == 0 {
		t.Fatalf("want 1 file ingested with chunks, got %+v", res)
	}
	sourceID := "sharepoint:" + cfg.SiteURL + ":a.txt"
	sources, _ := rag.listSources()
	found := false
	for _, src := range sources {
		if src.SourceID == sourceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s to be ingested after 1st sync, got %+v", sourceID, sources)
	}
	if !strings.Contains(deltaLink1, "token=1") {
		t.Fatalf("unexpected deltaLink after 1st sync: %q", deltaLink1)
	}

	cfg.DeltaLink = deltaLink1
	deletedNow = true
	res2, _, _, err := deltaSyncSharePoint(t.Context(), rag, s, cfg, "test-embed", false, nil)
	if err != nil {
		t.Fatalf("deltaSyncSharePoint (2nd): %v", err)
	}
	if len(res2.Errors) != 0 {
		t.Fatalf("expected no errors deleting, got %v", res2.Errors)
	}
	sources, _ = rag.listSources()
	for _, src := range sources {
		if src.SourceID == sourceID {
			t.Fatalf("expected %s to be deleted after 2nd sync, still present: %+v", sourceID, sources)
		}
	}
}

// TestSpListPagesFollowsNextLink mirrors
// TestSpDeltaSyncInitialFollowsNextLinkThenReturnsDeltaLink's pagination
// shape, for the separate Pages API (/sites/{id}/pages) rather than the
// drive delta feed.
func TestSpListPagesFollowsNextLink(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.Contains(r.URL.Path, "/page2"):
			_, _ = w.Write([]byte(`{"value": [{"id": "p2", "name": "b.aspx", "title": "Seite B"}]}`))
		default:
			_, _ = w.Write([]byte(`{
				"value": [{"id": "p1", "name": "a.aspx", "title": "Seite A"}],
				"@odata.nextLink": "` + graphBaseURL + `/sites/site-1/pages/page2"
			}`))
		}
	})

	pages, err := spListPages(context.Background(), testSharePointConfig())
	if err != nil {
		t.Fatalf("spListPages: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("want 2 pages across both pagination pages, got %d: %+v", len(pages), pages)
	}
	if pages[0].Name != "a.aspx" || pages[1].Name != "b.aspx" {
		t.Fatalf("unexpected page names: %+v", pages)
	}
}

// TestSpGetPageTextFlattensCanvasLayout confirms multiple sections/columns/
// web parts are all collected, HTML-stripped, and joined — and that a
// non-text web part (no innerHtml, e.g. an image or embed) contributes
// nothing rather than erroring.
func TestSpGetPageTextFlattensCanvasLayout(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		default:
			_, _ = w.Write([]byte(`{
				"title": "Neue Mailstruktur in der Logistik",
				"canvasLayout": {
					"horizontalSections": [
						{"columns": [{"webparts": [
							{"innerHtml": "<p>Erster Absatz.</p>"},
							{}
						]}]},
						{"columns": [{"webparts": [
							{"innerHtml": "<p>Zweiter Absatz.</p>"}
						]}]}
					]
				}
			}`))
		}
	})

	title, text, err := spGetPageText(context.Background(), testSharePointConfig(), "page-1")
	if err != nil {
		t.Fatalf("spGetPageText: %v", err)
	}
	if title != "Neue Mailstruktur in der Logistik" {
		t.Errorf("unexpected title: %q", title)
	}
	if !strings.Contains(text, "Erster Absatz.") || !strings.Contains(text, "Zweiter Absatz.") {
		t.Fatalf("want both paragraphs' text present, got %q", text)
	}
	if strings.Contains(text, "<p>") {
		t.Fatalf("want HTML tags stripped, got %q", text)
	}
}

// TestImportSharePointPagesIngestsSelected drives importSharePointPages
// end-to-end: only the selected page is fetched/ingested, the other is
// listed but skipped.
func TestImportSharePointPagesIngestsSelected(t *testing.T) {
	var fetchedDetail int
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.Contains(r.URL.Path, "/pages/p1/microsoft.graph.sitePage"):
			fetchedDetail++
			_, _ = w.Write([]byte(`{"title": "Seite A", "canvasLayout": {"horizontalSections": [{"columns": [{"webparts": [{"innerHtml": "<p>Inhalt A, lang genug für einen Chunk beim Import.</p>"}]}]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"value": [{"id": "p1", "name": "a.aspx", "title": "Seite A"}, {"id": "p2", "name": "b.aspx", "title": "Seite B"}]}`))
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}

	res, err := importSharePointPages(context.Background(), rag, s, cfg, "test-embed", map[string]bool{"p1": true}, false, nil)
	if err != nil {
		t.Fatalf("importSharePointPages: %v", err)
	}
	if res.Pages != 1 || res.Chunks == 0 {
		t.Fatalf("want 1 page ingested with chunks, got %+v", res)
	}
	if fetchedDetail != 1 {
		t.Fatalf("want the unselected page's detail never fetched, got %d detail fetches", fetchedDetail)
	}
	sourceID := "sharepoint:" + cfg.SiteURL + ":SitePages/a.aspx"
	sources, _ := rag.listSources()
	found := false
	for _, src := range sources {
		if src.SourceID == sourceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s to be ingested, got %+v", sourceID, sources)
	}
}

// TestSpEncodeShareURLRoundTrips checks the documented Microsoft Graph
// sharing-link encoding (base64, URL-safe, unpadded, "u!" prefix) without
// depending on a specific memorized golden value: decoding the result back
// (reversing the URL-safe substitution and padding) must recover the
// original URL exactly, and the encoded form itself must contain none of
// the characters the URL-safe step is supposed to have replaced.
func TestSpEncodeShareURLRoundTrips(t *testing.T) {
	shareURL := "https://groupiph.sharepoint.com/:b:/s/intranet_de/Allgemein/EX3jFDr8IVZJkLfoBee4TQ8BsADLO_TFle53lJa_rg599w?e=6hQ9on"
	encoded := spEncodeShareURL(shareURL)
	if !strings.HasPrefix(encoded, "u!") {
		t.Fatalf("want a \"u!\" prefix, got %q", encoded)
	}
	body := strings.TrimPrefix(encoded, "u!")
	if strings.ContainsAny(body, "/+=") {
		t.Fatalf("want no raw base64 '/', '+', or '=' left in the encoded form, got %q", encoded)
	}
	std := strings.ReplaceAll(strings.ReplaceAll(body, "_", "/"), "-", "+")
	for len(std)%4 != 0 {
		std += "="
	}
	decoded, err := base64.StdEncoding.DecodeString(std)
	if err != nil {
		t.Fatalf("decoding back: %v", err)
	}
	if string(decoded) != shareURL {
		t.Fatalf("round-trip mismatch: got %q, want %q", decoded, shareURL)
	}
}

// TestSpResolveShareLinkUsesEncodedID confirms spResolveShareLink calls
// Graph's /shares/{encoded}/driveItem with exactly the encoding
// spEncodeShareURL produces, and parses the resolved item back correctly.
func TestSpResolveShareLinkUsesEncodedID(t *testing.T) {
	shareURL := "https://groupiph.sharepoint.com/:b:/s/intranet_de/Allgemein/EX3jFDr8IVZJkLfoBee4TQ8BsADLO_TFle53lJa_rg599w?e=6hQ9on"
	wantPath := "/shares/" + spEncodeShareURL(shareURL) + "/driveItem"
	var gotPath string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name": "Rubix Werte.pdf", "size": 4096, "webUrl": "https://groupiph.sharepoint.com/sites/intranet_de/Allgemein/Freigegebene%20Dokumente/Rubix%20Werte.pdf", "@microsoft.graph.downloadUrl": "https://dl/rubix-werte"}`))
	})

	item, err := spResolveShareLink(context.Background(), testSharePointConfig(), shareURL)
	if err != nil {
		t.Fatalf("spResolveShareLink: %v", err)
	}
	if gotPath != wantPath {
		t.Fatalf("want Graph called at %q, got %q", wantPath, gotPath)
	}
	if item.Name != "Rubix Werte.pdf" || item.DownloadURL != "https://dl/rubix-werte" || item.IsFolder {
		t.Fatalf("unexpected resolved item: %+v", item)
	}
}

// TestImportSharePointShareLinksIngestsEach drives
// importSharePointShareLinks end-to-end over two links, one resolving to a
// real file (ingested) and one failing to resolve (recorded as an error,
// not aborting the other).
func TestImportSharePointShareLinksIngestsEach(t *testing.T) {
	goodURL := "https://groupiph.sharepoint.com/:b:/s/intranet_de/Allgemein/good?e=1"
	badURL := "https://groupiph.sharepoint.com/:b:/s/intranet_de/Allgemein/bad?e=2"
	goodPath := "/shares/" + spEncodeShareURL(goodURL) + "/driveItem"

	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == goodPath:
			_, _ = w.Write([]byte(`{"name": "notiz.txt", "size": 50, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download-notiz"}`))
		case strings.HasSuffix(r.URL.Path, "/download-notiz"):
			_, _ = w.Write([]byte("Inhalt der Notiz, lang genug für einen Chunk beim Import."))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"code": "itemNotFound", "message": "not found"}}`))
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}

	res, err := importSharePointShareLinks(context.Background(), rag, s, cfg, "test-embed", []string{goodURL, badURL}, false, nil)
	if err != nil {
		t.Fatalf("importSharePointShareLinks: %v", err)
	}
	if res.Links != 2 {
		t.Fatalf("want both links counted as attempted, got %+v", res)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], badURL) {
		t.Fatalf("want exactly one error mentioning the bad URL, got %+v", res.Errors)
	}
	if res.Chunks == 0 {
		t.Fatalf("want the good link's file ingested with chunks, got %+v", res)
	}
	sourceID := "sharepoint_link:" + goodURL
	sources, _ := rag.listSources()
	found := false
	for _, src := range sources {
		if src.SourceID == sourceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s to be ingested, got %+v", sourceID, sources)
	}
}

// TestSpSearchParsesHitsAndScopesToSite confirms spSearch sends Graph's
// documented search/query request shape (POST, entityTypes=[driveItem]),
// includes a "path:" KQL filter scoping the query to cfg.SiteURL, and
// correctly flattens the nested value/hitsContainers/hits response shape.
func TestSpSearchParsesHitsAndScopesToSite(t *testing.T) {
	var gotBody map[string]any
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/search/query" {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{
				"value": [{"hitsContainers": [{"hits": [
					{"summary": "…Speisekarte für die Kantine…", "resource": {"name": "Speisekarte.pdf", "webUrl": "https://rubix.sharepoint.com/sites/Vertrieb/Speisekarte.pdf"}}
				]}]}]
			}`))
			return
		}
		_, _ = w.Write([]byte(`{"id": "site-1"}`))
	})

	hits, err := spSearch(context.Background(), testSharePointConfig(), "Speisekarte")
	if err != nil {
		t.Fatalf("spSearch: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "Speisekarte.pdf" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if !strings.Contains(hits[0].Summary, "Kantine") {
		t.Errorf("want the summary preserved, got %q", hits[0].Summary)
	}

	reqs, _ := gotBody["requests"].([]any)
	if len(reqs) != 1 {
		t.Fatalf("want exactly 1 search request, got %+v", gotBody)
	}
	first, _ := reqs[0].(map[string]any)
	query, _ := first["query"].(map[string]any)
	qs, _ := query["queryString"].(string)
	if !strings.Contains(qs, "Speisekarte") || !strings.Contains(qs, testSharePointConfig().SiteURL) {
		t.Fatalf("want the query string to contain both the search term and a site path filter, got %q", qs)
	}
}

// TestAppendSharePointSearchToolOffersNothingWithoutOptIn confirms the
// tool is entirely absent when no connection has LiveSearchEnabled — the
// preset/settings gate, not just an empty result at call time.
func TestAppendSharePointSearchToolOffersNothingWithoutOptIn(t *testing.T) {
	conns := []sharePointConfig{{connRuntime: connRuntime{Name: "default"}, Enabled: true, SiteURL: "https://rubix.sharepoint.com/sites/Vertrieb"}}
	tools := appendSharePointSearchTool(nil, map[string]toolExecutor{}, conns, nil, "", nil)
	if len(tools) != 0 {
		t.Fatalf("want no tools offered without LiveSearchEnabled, got %+v", tools)
	}
}

// TestAppendSharePointSearchToolSingleConnNoSiteParam confirms a single
// opted-in connection makes "site" absent from the schema entirely (only
// one sensible choice), while two connections make it required.
func TestAppendSharePointSearchToolSingleConnNoSiteParam(t *testing.T) {
	conns := []sharePointConfig{{connRuntime: connRuntime{Name: "default"}, Enabled: true, LiveSearchEnabled: true, SiteURL: "https://rubix.sharepoint.com/sites/Vertrieb"}}
	tools := appendSharePointSearchTool(nil, map[string]toolExecutor{}, conns, nil, "", nil)
	if len(tools) != 1 {
		t.Fatalf("want exactly 1 tool, got %d", len(tools))
	}
	props, _ := tools[0].Function.Parameters["properties"].(map[string]any)
	if _, hasSite := props["site"]; hasSite {
		t.Error("want no \"site\" parameter with only one opted-in connection")
	}

	conns = append(conns, sharePointConfig{connRuntime: connRuntime{Name: "logistik"}, Enabled: true, LiveSearchEnabled: true, SiteURL: "https://rubix.sharepoint.com/sites/Logistik"})
	tools = appendSharePointSearchTool(nil, map[string]toolExecutor{}, conns, nil, "", nil)
	props, _ = tools[0].Function.Parameters["properties"].(map[string]any)
	if _, hasSite := props["site"]; !hasSite {
		t.Error("want a \"site\" parameter once more than one connection is opted in")
	}
	required, _ := tools[0].Function.Parameters["required"].([]string)
	found2 := false
	for _, r := range required {
		if r == "site" {
			found2 = true
		}
	}
	if !found2 {
		t.Error("want \"site\" to be required once it exists as a parameter")
	}
}

// TestSharePointSearchToolExecutorDefaultsToSoleConnection confirms the
// executor searches the only opted-in connection when the model omits
// "site" entirely (the schema doesn't even offer it in that case).
func TestSharePointSearchToolExecutorDefaultsToSoleConnection(t *testing.T) {
	var gotSitePath string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/search/query" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			reqs, _ := body["requests"].([]any)
			first, _ := reqs[0].(map[string]any)
			query, _ := first["query"].(map[string]any)
			gotSitePath, _ = query["queryString"].(string)
			_, _ = w.Write([]byte(`{"value": []}`))
			return
		}
		_, _ = w.Write([]byte(`{"id": "site-1"}`))
	})

	conns := []sharePointConfig{testSharePointConfig()}
	conns[0].LiveSearchEnabled = true
	executor := sharePointSearchToolExecutor(conns)
	out, err := executor(context.Background(), `{"query":"Urlaubsantrag"}`)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	if !strings.Contains(out, "keine Treffer") {
		t.Fatalf("want a no-hits message, got %q", out)
	}
	if !strings.Contains(gotSitePath, conns[0].SiteURL) {
		t.Fatalf("want the sole connection's site to be searched, got query %q", gotSitePath)
	}
}

// TestSharePointSearchToolExecutorRejectsUnknownSite confirms naming a
// "site" that isn't among the opted-in connections fails clearly rather
// than silently searching something else.
func TestSharePointSearchToolExecutorRejectsUnknownSite(t *testing.T) {
	conns := []sharePointConfig{{connRuntime: connRuntime{Name: "default"}, Enabled: true, LiveSearchEnabled: true, SiteURL: "https://rubix.sharepoint.com/sites/Vertrieb"}}
	executor := sharePointSearchToolExecutor(conns)
	if _, err := executor(context.Background(), `{"query":"x","site":"nonexistent"}`); err == nil {
		t.Fatal("want an error for an unknown site name")
	}
}

// TestDeltaSyncSharePointFallsBackToContentEndpointWhenDownloadURLMissing
// reproduces a real production failure: Microsoft Graph's delta feed
// inconsistently omits @microsoft.graph.downloadUrl for real (non-folder)
// items — observed as dozens of files across nested folders all failing
// delta-sync with "item has no download URL (is it a folder?)". Confirms
// spDownloadItem's fallback (the item's own id, via
// /drive/items/{id}/content) actually recovers the file instead.
func TestDeltaSyncSharePointFallsBackToContentEndpointWhenDownloadURLMissing(t *testing.T) {
	const content = "Inhalt ohne downloadUrl im Delta-Feed, lang genug für einen Chunk."
	var gotAuthHeader string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case r.URL.Path == "/sites/site-1/drive/items/item-1/content":
			gotAuthHeader = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(content))
		default:
			w.Header().Set("Content-Type", "application/json")
			// Deliberately no "@microsoft.graph.downloadUrl" — the exact
			// shape Graph sends for the items this bug affects.
			_, _ = w.Write([]byte(`{
				"value": [{"id": "item-1", "name": "a.txt", "size": 100, "parentReference": {"path": "/drive/root:"}}],
				"@odata.deltaLink": "` + graphBaseURL + `/delta?token=1"
			}`))
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	s.SharePoint = []sharePointConfig{cfg}

	res, _, _, err := deltaSyncSharePoint(t.Context(), rag, s, cfg, "test-embed", false, nil)
	if err != nil {
		t.Fatalf("deltaSyncSharePoint: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("want no errors (fallback should have recovered the file), got %v", res.Errors)
	}
	if res.Files != 1 || res.Chunks == 0 {
		t.Fatalf("want 1 file ingested with chunks, got %+v", res)
	}
	if gotAuthHeader == "" || !strings.HasPrefix(gotAuthHeader, "Bearer ") {
		t.Errorf("want the content-endpoint request to carry a Bearer token, got %q", gotAuthHeader)
	}
	sourceID := "sharepoint:" + cfg.SiteURL + ":a.txt"
	sources, _ := rag.listSources()
	found := false
	for _, src := range sources {
		if src.SourceID == sourceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s to be ingested, got %+v", sourceID, sources)
	}
}

// TestSpDeltaSyncSelfHealsOn410Gone confirms that when Graph invalidates a
// stored delta cursor — the documented 410 Gone response for an expired
// resume token (error codes like resyncChangesApplyDifferences) — the
// very next run automatically restarts from a full walk instead of
// re-requesting and failing on the same expired link forever (nothing
// else in this codebase ever clears DeltaLink on error).
func TestSpDeltaSyncSelfHealsOn410Gone(t *testing.T) {
	var fullWalkRequested bool
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case r.URL.RawQuery == "token=expired":
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error": {"code": "resyncChangesApplyDifferences", "message": "token expired"}}`))
		default:
			fullWalkRequested = true
			_, _ = w.Write([]byte(`{
				"value": [{"name": "a.docx", "size": 100, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "https://dl/a"}],
				"@odata.deltaLink": "` + graphBaseURL + `/sites/site-1/drive/root/delta?token=fresh"
			}`))
		}
	})

	cfg := testSharePointConfig()
	cfg.DeltaLink = graphBaseURL + "/sites/site-1/drive/root/delta?token=expired"
	items, _, newDeltaLink, err := spDeltaSync(context.Background(), cfg)
	if err != nil {
		t.Fatalf("spDeltaSync: %v", err)
	}
	if !fullWalkRequested {
		t.Fatalf("want a full walk to have been requested after the 410, got none")
	}
	if len(items) != 1 || items[0].Name != "a.docx" {
		t.Fatalf("want the full walk's item recovered, got %+v", items)
	}
	if !strings.Contains(newDeltaLink, "token=fresh") {
		t.Fatalf("want the fresh deltaLink persisted, got %q", newDeltaLink)
	}
}

// TestSpListFolderInFollowsNextLink confirms a folder listing spanning
// multiple pages is fully collected instead of silently returning only
// the first page — the same nextLink-following pattern spDeltaSync/
// spListPages already use. Before this fix, any folder bigger than
// Graph's default page size would have every browse/import/discovery
// path silently see only its first page, with nothing signaling missing
// items.
func TestSpListFolderInFollowsNextLink(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/page2"):
			_, _ = w.Write([]byte(`{"value": [{"id": "2", "name": "b.pdf", "size": 200}]}`))
		default:
			_, _ = w.Write([]byte(`{
				"value": [{"id": "1", "name": "a.docx", "size": 100}],
				"@odata.nextLink": "` + graphBaseURL + `/sites/site-1/drive/root/children/page2"
			}`))
		}
	})

	items, err := spListFolderIn(context.Background(), "fake-token", "site-1", "")
	if err != nil {
		t.Fatalf("spListFolderIn: %v", err)
	}
	if len(items) != 2 || items[0].Name != "a.docx" || items[1].Name != "b.pdf" {
		t.Fatalf("want both pages' items collected, got %+v", items)
	}
}

// TestSpDownloadItemFallsBackToContentEndpointWhenDownloadURLRequestFails
// confirms a populated-but-broken/expired downloadUrl is retried via the
// content endpoint (keyed on the item's own ID) instead of failing the
// item outright — Graph's downloadUrl is documented to expire within
// minutes, and a long sequential import run can easily outlast it for
// later items even though it was valid when the folder was listed.
func TestSpDownloadItemFallsBackToContentEndpointWhenDownloadURLRequestFails(t *testing.T) {
	const content = "Frischer Inhalt über den Content-Endpoint nach abgelaufener downloadUrl."
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case r.URL.Path == "/sites/site-1/drive/items/item-1/content":
			_, _ = w.Write([]byte(content))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})

	expiredDownloadURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(expiredDownloadURL.Close)

	cfg := testSharePointConfig()
	item := spDriveItem{Name: "a.txt", Path: "a.txt", ID: "item-1", DownloadURL: expiredDownloadURL.URL}
	data, err := spDownloadItem(context.Background(), cfg, item)
	if err != nil {
		t.Fatalf("spDownloadItem: %v", err)
	}
	if string(data) != content {
		t.Fatalf("want content-endpoint fallback content, got %q", data)
	}
}

// TestSpItemExceedsMaxFileMB confirms the pre-download size gate uses the
// configured MaxFileMB (falling back to extractText's own 25MB default
// when unset) — the same limit, just checked before spending a Graph
// download on a file that would be rejected afterwards anyway.
func TestSpItemExceedsMaxFileMB(t *testing.T) {
	configured := appSettings{Import: importConfig{MaxFileMB: 1}}
	if spItemExceedsMaxFileMB(spDriveItem{Size: 500 * 1024}, configured) {
		t.Fatalf("500KB should not exceed a configured 1MB limit")
	}
	if !spItemExceedsMaxFileMB(spDriveItem{Size: 2 * 1024 * 1024}, configured) {
		t.Fatalf("2MB should exceed a configured 1MB limit")
	}

	unset := appSettings{}
	if spItemExceedsMaxFileMB(spDriveItem{Size: 24 * 1024 * 1024}, unset) {
		t.Fatalf("24MB should be within extractText's 25MB default")
	}
	if !spItemExceedsMaxFileMB(spDriveItem{Size: 26 * 1024 * 1024}, unset) {
		t.Fatalf("26MB should exceed extractText's 25MB default")
	}
}

// TestDeltaSyncSharePointSkipsOversizedFileWithoutDownloading confirms an
// oversized item (per its already-known delta-feed Size) is rejected
// before any download request is made, not after downloading it in full —
// a download-endpoint hit here would fail the test outright.
func TestDeltaSyncSharePointSkipsOversizedFileWithoutDownloading(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.HasSuffix(r.URL.Path, "/download"):
			t.Fatalf("oversized item should never be downloaded, got a request for %s", r.URL.Path)
		default:
			_, _ = w.Write([]byte(`{
				"value": [{"name": "huge.mp4", "size": 999999999, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}],
				"@odata.deltaLink": "` + graphBaseURL + `/delta?token=1"
			}`))
		}
	})

	rag, s := newTestRAG(t)
	s.Import.MaxFileMB = 1
	cfg := testSharePointConfig()
	res, _, _, err := deltaSyncSharePoint(context.Background(), rag, s, cfg, "test-embed", false, nil)
	if err != nil {
		t.Fatalf("deltaSyncSharePoint: %v", err)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "too large") {
		t.Fatalf("want a single 'too large' error, got %+v", res.Errors)
	}
}

// TestDeltaSyncSharePointCancellationPreservesOldCursor confirms a run
// that's cut short by ctx cancellation (the scheduler dashboard's Cancel
// button, or a connection's own timeout) returns the OLD DeltaLink/
// ItemPaths, not the new ones spDeltaSync's listing pass already computed
// — persisting the new cursor here would silently and permanently skip
// every item after the cancellation point, since Graph's delta feed never
// re-surfaces an unchanged item once the cursor has advanced past it.
func TestDeltaSyncSharePointCancellationPreservesOldCursor(t *testing.T) {
	var ctx context.Context
	var cancel context.CancelFunc
	var downloadCalls int32
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sites/rubix.sharepoint.com:"):
			_, _ = w.Write([]byte(`{"id": "site-1"}`))
		case strings.HasSuffix(r.URL.Path, "/download"):
			n := atomic.AddInt32(&downloadCalls, 1)
			if n == 1 {
				// Cancel right after the first item's own download responds,
				// but before this handler returns — deterministic (no sleep/
				// race): the loop's ctx.Err() check for the SECOND item is
				// guaranteed to see it as already cancelled.
				cancel()
			} else {
				t.Fatalf("want only the first item downloaded before cancellation is observed, got a request for item %d", n)
			}
			_, _ = w.Write([]byte("file content"))
		default:
			_, _ = w.Write([]byte(`{
				"value": [
					{"id": "item-1", "name": "a.txt", "size": 10, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"},
					{"id": "item-2", "name": "b.txt", "size": 10, "parentReference": {"path": "/drive/root:"}, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}
				],
				"@odata.deltaLink": "` + graphBaseURL + `/delta?token=NEW"
			}`))
		}
	})

	rag, s := newTestRAG(t)
	cfg := testSharePointConfig()
	cfg.DeltaLink = graphBaseURL + "/delta?token=OLD"
	cfg.ItemPaths = map[string]string{"item-0": "already/known.txt"}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel() // belt-and-suspenders: the fake handler above already calls it, but every path must
	res, newDeltaLink, newItemPaths, err := deltaSyncSharePoint(ctx, rag, s, cfg, "test-embed", false, nil)
	if err == nil {
		t.Fatal("want a context-cancellation error, got nil")
	}
	if newDeltaLink != cfg.DeltaLink {
		t.Fatalf("want the OLD delta link preserved on cancellation (%q), got %q", cfg.DeltaLink, newDeltaLink)
	}
	if len(newItemPaths) != 1 || newItemPaths["item-0"] != "already/known.txt" {
		t.Fatalf("want the OLD item-paths map preserved unmodified on cancellation, got %+v", newItemPaths)
	}
	if res.Files != 1 {
		t.Fatalf("want exactly the first item counted before cancellation, got Files=%d", res.Files)
	}
}

// TestImportSharePointShareLinksSkipsOversizedWithoutDownloading confirms
// a share link resolving to an oversized file is rejected using its
// already-known Size (from spResolveShareLink's response) before any
// download request — mirroring
// TestDeltaSyncSharePointSkipsOversizedFileWithoutDownloading's guarantee
// for the folder-import/delta-sync paths, previously missing for share
// links (which used to download the full file unconditionally first).
func TestImportSharePointShareLinksSkipsOversizedWithoutDownloading(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/driveItem"):
			_, _ = w.Write([]byte(`{"name": "huge.mp4", "size": 999999999, "@microsoft.graph.downloadUrl": "` + graphBaseURL + `/download"}`))
		case strings.HasSuffix(r.URL.Path, "/download"):
			t.Fatalf("oversized shared item should never be downloaded, got a request for %s", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	})

	rag, s := newTestRAG(t)
	s.Import.MaxFileMB = 1
	cfg := testSharePointConfig()
	res, err := importSharePointShareLinks(context.Background(), rag, s, cfg, "test-embed", []string{"https://rubix.sharepoint.com/:v:/s/team/abc123"}, false, nil)
	if err != nil {
		t.Fatalf("importSharePointShareLinks: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("want the oversized link counted as skipped, got %+v", res)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "zu groß") {
		t.Fatalf("want a single 'zu groß' (too large) error, got %+v", res.Errors)
	}
}

// TestAppendSharePointSearchToolRespectsAccessControl confirms a
// connection's AccessControl actually restricts search_sharepoint to
// permitted callers — previously this tool had NO per-caller/department
// gate at all (only the coarse, deployment-wide "sharepoint_search" preset
// category), so any caller whose preset allowed that category got live
// results from every opted-in site regardless of who was asking.
func TestAppendSharePointSearchToolRespectsAccessControl(t *testing.T) {
	conns := []sharePointConfig{{
		connRuntime: connRuntime{Name: "vertrieb"}, Enabled: true, LiveSearchEnabled: true,
		SiteURL:       "https://rubix.sharepoint.com/sites/Vertrieb",
		AccessControl: accessControl{AllowedUsers: []string{"alice@rubix.com"}},
	}}

	// A caller not in AllowedUsers/AllowedGroups must get no tool at all.
	tools := appendSharePointSearchTool(nil, map[string]toolExecutor{}, conns, nil, "bob@rubix.com", nil)
	if len(tools) != 0 {
		t.Fatalf("want no tool offered to a user outside AccessControl, got %+v", tools)
	}

	// The allowed user gets the tool.
	tools = appendSharePointSearchTool(nil, map[string]toolExecutor{}, conns, nil, "alice@rubix.com", nil)
	if len(tools) != 1 {
		t.Fatalf("want the tool offered to a user AccessControl allows, got %d tools", len(tools))
	}

	// A member of an allowed group also gets it, even without being listed
	// individually.
	conns[0].AccessControl = accessControl{AllowedGroups: []string{"CN=Vertrieb,OU=Groups,DC=rubix,DC=com"}}
	tools = appendSharePointSearchTool(nil, map[string]toolExecutor{}, conns, nil, "carol@rubix.com", []string{"CN=Vertrieb,OU=Groups,DC=rubix,DC=com"})
	if len(tools) != 1 {
		t.Fatalf("want the tool offered to a member of an allowed group, got %d tools", len(tools))
	}

	// Empty AccessControl (the zero value, unset) stays unrestricted —
	// same "empty = unrestricted" convention every other AccessControl
	// field uses.
	conns[0].AccessControl = accessControl{}
	tools = appendSharePointSearchTool(nil, map[string]toolExecutor{}, conns, nil, "anyone@rubix.com", nil)
	if len(tools) != 1 {
		t.Fatalf("want an unset AccessControl to mean unrestricted, got %d tools", len(tools))
	}
}

// TestSpDeltaSyncFromRespectsConnectionMaxItemsOverride confirms the
// delta feed's own per-run enumeration cap honors THIS connection's
// MaxItemsPerRun override rather than always falling back to the
// deployment-wide default, regardless of what's configured per-connection.
func TestSpDeltaSyncFromRespectsConnectionMaxItemsOverride(t *testing.T) {
	// The cap is only ever checked BETWEEN pages (spDeltaSyncFrom returns
	// immediately once a page carries @odata.deltaLink, before any cap
	// check) — so this needs two pages to actually exercise it: page 1
	// alone already reaches the (overridden) cap of 2, and page 2 must
	// never be fetched.
	var page2Requested bool
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/page2") {
			page2Requested = true
			_, _ = w.Write([]byte(`{
				"value": [{"id": "3", "name": "c.txt", "size": 1, "parentReference": {"path": "/drive/root:"}}],
				"@odata.deltaLink": "` + graphBaseURL + `/delta?token=done"
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"value": [
				{"id": "1", "name": "a.txt", "size": 1, "parentReference": {"path": "/drive/root:"}},
				{"id": "2", "name": "b.txt", "size": 1, "parentReference": {"path": "/drive/root:"}}
			],
			"@odata.nextLink": "` + graphBaseURL + `/page2"
		}`))
	})

	cfg := testSharePointConfig()
	cfg.MaxItemsPerRun = 2 // tighter than the global default (500)
	items, _, newLink, err := spDeltaSyncFrom(context.Background(), cfg, graphBaseURL+"/some/delta/link")
	if err != nil {
		t.Fatalf("spDeltaSyncFrom: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want the connection's own MaxItemsPerRun=2 override honored, got %d items: %+v", len(items), items)
	}
	if page2Requested {
		t.Fatal("want enumeration to stop at the connection's own cap, page 2 should never have been requested")
	}
	if !strings.Contains(newLink, "/page2") {
		t.Fatalf("want the resume cursor to be page 2's own link so the next run continues there, got %q", newLink)
	}
}
