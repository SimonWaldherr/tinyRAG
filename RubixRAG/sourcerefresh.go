package main

import (
	"context"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Manual single-source refresh (Quellen-Übersicht's "Neu laden" button) —
// re-fetches ONE already-imported source from its original system and lets
// ingestDocument's existing content-hash check decide whether anything
// actually changed, without re-running that connector's whole
// import/delta-sync. Implemented for the three SharePoint source_kinds
// (sharepoint_file/sharepoint_page/sharepoint_link) and for web_page —
// every other kind returns a clear "not supported yet" error rather than
// pretending to refresh something it can't actually re-fetch (a PST
// mailbox's uploaded file isn't kept server-side after import, for
// instance, so there's nothing to re-download from for a pst_email row).
// ─────────────────────────────────────────────────────────────────────────────

// refreshSource re-fetches sourceID's content given its already-known
// source_kind (the caller looks this up via rag.fetchSourceKind) and
// re-ingests it — same content-hash-compare-and-replace path every normal
// import already uses, so an unchanged file comes back Skipped=true and a
// changed one replaces its chunks.
func refreshSource(ctx context.Context, rag *ragSystem, s appSettings, sourceID, sourceKind string) (ingestOutcome, error) {
	switch sourceKind {
	case "sharepoint_file":
		return refreshSharePointFile(ctx, rag, s, sourceID)
	case "sharepoint_page":
		return refreshSharePointPage(ctx, rag, s, sourceID)
	case "sharepoint_link":
		return refreshSharePointSharedLink(ctx, rag, s, sourceID)
	case "web_page":
		return refreshWebPage(ctx, rag, s, sourceID)
	default:
		return ingestOutcome{}, fmt.Errorf("manuelles Neuladen wird für Quelltyp %q noch nicht unterstützt", sourceKind)
	}
}

// refreshWebPage re-fetches one imported web page's URL (sourceID is
// "web:"+URL, see importWebPages) and re-ingests it — the same
// fetchWebPage→ingestDocument path (including the allowPrivate=false SSRF
// posture) the web importer itself uses, so an unchanged page comes back
// Skipped=true and a changed one replaces its chunks.
func refreshWebPage(ctx context.Context, rag *ragSystem, s appSettings, sourceID string) (ingestOutcome, error) {
	rawURL := strings.TrimPrefix(sourceID, "web:")
	if strings.TrimSpace(rawURL) == "" || rawURL == sourceID {
		return ingestOutcome{}, fmt.Errorf("Quelle %q trägt keine erkennbare Web-URL", sourceID)
	}
	maxBytes := s.Import.MaxFileMB
	if maxBytes <= 0 {
		maxBytes = 25
	}
	maxBytes *= 1024 * 1024
	title, text, err := fetchWebPage(ctx, rawURL, maxBytes, false)
	if err != nil {
		return ingestOutcome{}, err
	}
	return ingestDocument(rag, s, s.activeEmbedModel(), sourceID, "web_page", title, text, 0, false)
}

// findSharePointConnForSourceID matches sourceID (either "sharepoint:<site
// URL>:<itemPath>" for a file/page) against every enabled SharePoint
// connection's own SiteURL, returning the first match plus the itemPath
// with that prefix stripped off. Matching on the known SiteURL directly —
// rather than splitting sourceID on ":" — sidesteps the fact that SiteURL
// itself contains colons ("https://..."), which would make a naive split
// ambiguous.
func findSharePointConnForSourceID(conns []sharePointConfig, sourceID string) (conn sharePointConfig, itemPath string, ok bool) {
	for _, c := range conns {
		if !c.Enabled || c.SiteURL == "" {
			continue
		}
		prefix := "sharepoint:" + c.SiteURL + ":"
		if strings.HasPrefix(sourceID, prefix) {
			return c, strings.TrimPrefix(sourceID, prefix), true
		}
	}
	return sharePointConfig{}, "", false
}

// firstEnabledSharePointConn picks a connection for actions not tied to
// any one specific site — currently just refreshing a sharing-link import,
// whose sourceID (unlike a file/page's) never encoded which connection
// originally resolved it. Mirrors importSharePointShareLinks' own
// UI-selected-connection convention: any enabled connection's app
// credentials work, as long as that app was separately granted access to
// wherever the link's file actually lives.
func firstEnabledSharePointConn(conns []sharePointConfig) (sharePointConfig, bool) {
	for _, c := range conns {
		if c.Enabled {
			return c, true
		}
	}
	return sharePointConfig{}, false
}

// spItemParentFolder returns itemPath's containing folder ("" for a
// root-level item) — spListFolder's folderPath argument, computed the
// same way spItemPath (delta sync) derives paths in the first place, just
// in reverse.
func spItemParentFolder(itemPath string) string {
	idx := strings.LastIndex(itemPath, "/")
	if idx < 0 {
		return ""
	}
	return itemPath[:idx]
}

// refreshSharePointFile re-lists sourceID's parent folder (a drive item's
// @microsoft.graph.downloadUrl is short-lived, so — like
// importSharePointFiles — it can't be reused from whenever this source
// was first imported) and re-ingests the matching item if still present.
func refreshSharePointFile(ctx context.Context, rag *ragSystem, s appSettings, sourceID string) (ingestOutcome, error) {
	conn, itemPath, ok := findSharePointConnForSourceID(s.SharePoint, sourceID)
	if !ok {
		return ingestOutcome{}, fmt.Errorf("keine aktivierte SharePoint-Verbindung passt zu dieser Quelle (Site nicht mehr konfiguriert oder deaktiviert?)")
	}
	items, err := spListFolder(ctx, conn, spItemParentFolder(itemPath))
	if err != nil {
		return ingestOutcome{}, err
	}
	for _, it := range items {
		if it.IsFolder || it.Path != itemPath {
			continue
		}
		data, err := spDownloadItem(ctx, conn, it)
		if err != nil {
			return ingestOutcome{}, err
		}
		return ingestSharePointFile(rag, s, s.activeEmbedModel(), conn.SiteURL, itemPath, data, false)
	}
	return ingestOutcome{}, fmt.Errorf("Datei %q nicht mehr in der Dokumentbibliothek gefunden (umbenannt oder gelöscht?)", itemPath)
}

// refreshSharePointPage re-lists the site's pages to find pageID by name
// (a page's Graph ID, unlike a file's path, isn't itself encoded in
// sourceID — ingestSharePointPage only ever stored the SitePages-relative
// name) and re-fetches+re-ingests its current text.
func refreshSharePointPage(ctx context.Context, rag *ragSystem, s appSettings, sourceID string) (ingestOutcome, error) {
	conn, itemPath, ok := findSharePointConnForSourceID(s.SharePoint, sourceID)
	if !ok {
		return ingestOutcome{}, fmt.Errorf("keine aktivierte SharePoint-Verbindung passt zu dieser Quelle (Site nicht mehr konfiguriert oder deaktiviert?)")
	}
	pageName := strings.TrimPrefix(itemPath, "SitePages/")
	pages, err := spListPages(ctx, conn)
	if err != nil {
		return ingestOutcome{}, err
	}
	for _, p := range pages {
		if p.Name != pageName {
			continue
		}
		title, text, err := spGetPageText(ctx, conn, p.ID)
		if err != nil {
			return ingestOutcome{}, err
		}
		if title == "" {
			title = p.Title
		}
		return ingestSharePointPage(rag, s, s.activeEmbedModel(), conn.SiteURL, p.Name, title, text, false)
	}
	return ingestOutcome{}, fmt.Errorf("Seite %q nicht mehr gefunden (umbenannt oder gelöscht?)", pageName)
}

// refreshSharePointSharedLink re-resolves and re-downloads a sharing
// link's file — the link itself (sourceID's "sharepoint_link:" suffix) is
// what identifies the item, not any one connection's SiteURL, so any
// enabled connection's credentials are used (see
// firstEnabledSharePointConn's doc comment).
func refreshSharePointSharedLink(ctx context.Context, rag *ragSystem, s appSettings, sourceID string) (ingestOutcome, error) {
	shareURL := strings.TrimPrefix(sourceID, "sharepoint_link:")
	conn, ok := firstEnabledSharePointConn(s.SharePoint)
	if !ok {
		return ingestOutcome{}, fmt.Errorf("keine aktivierte SharePoint-Verbindung konfiguriert")
	}
	item, err := spResolveShareLink(ctx, conn, shareURL)
	if err != nil {
		return ingestOutcome{}, err
	}
	if item.IsFolder {
		return ingestOutcome{}, fmt.Errorf("Link zeigt auf einen Ordner, nicht auf eine einzelne Datei")
	}
	data, err := spDownloadFile(ctx, item.DownloadURL)
	if err != nil {
		return ingestOutcome{}, err
	}
	return ingestSharePointSharedLink(rag, s, s.activeEmbedModel(), shareURL, item.Name, data, false)
}
