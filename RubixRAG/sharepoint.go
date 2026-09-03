package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// SharePoint import via Microsoft Graph API, app-only (client-credentials)
// auth — no interactive user, suitable for a background import job. Needs
// an Azure AD app registration with the Sites.Selected *application*
// permission (admin-consented) plus an explicit per-site grant — see
// settings.go's sharePointConfig doc comment for why Sites.Selected and not
// the broader Sites.Read.All, and ANLEITUNG.md's "SharePoint" section for
// the full rollout guidance. Nothing here does anything until
// s.SharePoint.Enabled is true and that app registration/site grant exist.
//
// Token acquisition and authenticated GET are shared with the other
// Graph-backed connectors (graphmail.go, teams.go) — see graph.go.
// ─────────────────────────────────────────────────────────────────────────────

// spCreds adapts sharePointConfig's fields to the shared graphCreds shape
// graph.go's token acquisition expects.
func spCreds(cfg sharePointConfig) graphCreds {
	return graphCreds{
		TenantID:        cfg.TenantID,
		ClientID:        cfg.ClientID,
		ClientSecret:    cfg.ClientSecret,
		ClientSecretEnv: cfg.ClientSecretEnv,
	}
}

// spAccessToken returns a cached app-only Graph token for cfg — a thin
// wrapper so the rest of this file's call sites didn't need to change
// when token handling moved to graph.go.
func spAccessToken(ctx context.Context, cfg sharePointConfig) (string, error) {
	return graphAccessToken(ctx, spCreds(cfg))
}

// spGraphGet is a thin alias for graph.go's graphGet, kept so this file's
// call sites read as SharePoint-specific rather than reaching into graph.go
// directly.
func spGraphGet(ctx context.Context, token, path string) ([]byte, error) {
	return graphGet(ctx, token, path)
}

// spSiteIDCache caches SiteURL → resolved Graph site ID (spSiteID below).
// A site's ID is permanent, but a modest TTL keeps the cache self-healing
// if a site is ever deleted and recreated under the same URL. Keyed by
// graphBaseURL too so tests pointing graphBaseURL at different fake servers
// (newFakeGraphServer) can never bleed a cached ID into each other.
var (
	spSiteIDMu    sync.Mutex
	spSiteIDCache = map[string]spSiteIDEntry{}
)

type spSiteIDEntry struct {
	id      string
	expires time.Time
}

const spSiteIDTTL = time.Hour

// spSiteID resolves cfg.SiteURL (e.g.
// "https://rubix.sharepoint.com/sites/Vertrieb") into the opaque Graph
// site ID the drive endpoints need. Cached (spSiteIDCache above): this is
// called once per imported page and once per delta item whose downloadUrl
// came back empty — without the cache a 200-page import paid ~200 identical
// site-lookup round trips, pure added latency and throttling exposure.
func spSiteID(ctx context.Context, cfg sharePointConfig, token string) (string, error) {
	if cfg.SiteURL == "" {
		return "", fmt.Errorf("sharepoint: site_url not configured")
	}
	key := graphBaseURL + "|" + cfg.SiteURL
	spSiteIDMu.Lock()
	if e, ok := spSiteIDCache[key]; ok && time.Now().Before(e.expires) {
		spSiteIDMu.Unlock()
		return e.id, nil
	}
	spSiteIDMu.Unlock()

	u, err := url.Parse(cfg.SiteURL)
	if err != nil {
		return "", fmt.Errorf("sharepoint: invalid site_url: %w", err)
	}
	sitePath := strings.TrimSuffix(u.Path, "/")
	raw, err := spGraphGet(ctx, token, fmt.Sprintf("/sites/%s:%s", u.Host, sitePath))
	if err != nil {
		return "", err
	}
	var site struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &site); err != nil {
		return "", fmt.Errorf("sharepoint: parse site response: %w", err)
	}
	if site.ID == "" {
		return "", fmt.Errorf("sharepoint: site not found for %s", cfg.SiteURL)
	}
	spSiteIDMu.Lock()
	spSiteIDCache[key] = spSiteIDEntry{id: site.ID, expires: time.Now().Add(spSiteIDTTL)}
	spSiteIDMu.Unlock()
	return site.ID, nil
}

// spDriveItem is one file/folder entry as R3 uses it — Path is the full
// path relative to the document library root, computed from the parent
// folder passed to spListFolder (Graph itself only returns the bare name).
// ID backs spDownloadItem's content-endpoint fallback for when DownloadURL
// comes back empty (see its doc comment) — every driveItem Graph returns
// has one, listing or delta alike.
type spDriveItem struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	IsFolder    bool   `json:"is_folder"`
	ID          string `json:"-"`
	DownloadURL string `json:"-"`
}

type spGraphItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	DownloadURL string `json:"@microsoft.graph.downloadUrl"`
}

// spListFolderIn lists folderPath's immediate children given an
// already-resolved token+siteID — the actual Graph call, factored out of
// spListFolder so a caller making many calls in a row (spDiscoverWalk's
// recursive tree, below) resolves the token and site ID exactly once
// instead of once per folder visited, which would otherwise double Graph
// traffic under a recursive walk of, say, 200 folders.
//
// Follows @odata.nextLink to completion (same pattern as spListPages and
// spDeltaSync) — Graph's default page size for a children listing is far
// below what a real document library can hold, so returning only the
// first page would silently truncate any folder bigger than that page
// size instead of erroring, with nothing telling a caller items are
// missing.
func spListFolderIn(ctx context.Context, token, siteID, folderPath string) ([]spDriveItem, error) {
	folderPath = strings.Trim(folderPath, "/")
	path := fmt.Sprintf("/sites/%s/drive/root/children", siteID)
	if folderPath != "" {
		escaped := (&url.URL{Path: folderPath}).EscapedPath()
		path = fmt.Sprintf("/sites/%s/drive/root:/%s:/children", siteID, escaped)
	}

	items := make([]spDriveItem, 0)
	for path != "" {
		raw, err := spGraphGet(ctx, token, path)
		if err != nil {
			return nil, err
		}
		var listing struct {
			Value    []spGraphItem `json:"value"`
			NextLink string        `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(raw, &listing); err != nil {
			return nil, fmt.Errorf("sharepoint: parse folder listing: %w", err)
		}
		for _, it := range listing.Value {
			itemPath := it.Name
			if folderPath != "" {
				itemPath = folderPath + "/" + it.Name
			}
			items = append(items, spDriveItem{
				Name:        it.Name,
				Path:        itemPath,
				Size:        it.Size,
				IsFolder:    it.Folder != nil,
				ID:          it.ID,
				DownloadURL: it.DownloadURL,
			})
		}
		path = strings.TrimPrefix(listing.NextLink, graphBaseURL)
	}
	return items, nil
}

// spListFolder lists the immediate children of folderPath (relative to
// the site's default document library root, e.g. "General/Vertrieb", ""
// for the root) — not recursive, matching the PST folder picker's flat,
// one-level-at-a-time preview UX.
func spListFolder(ctx context.Context, cfg sharePointConfig, folderPath string) ([]spDriveItem, error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return nil, err
	}
	siteID, err := spSiteID(ctx, cfg, token)
	if err != nil {
		return nil, err
	}
	return spListFolderIn(ctx, token, siteID, folderPath)
}

// spDownloadFile fetches an item's content via its pre-authenticated,
// short-lived @microsoft.graph.downloadUrl (from spListFolder) — that URL
// is already signed, so no Authorization header is needed or wanted here.
// Goes through graphDoWithRetry so a transient 429/503 on this — the
// highest-volume, most payload-heavy call in the whole connector — gets
// the same retry/backoff resilience every metadata call already has,
// instead of permanently failing that file's ingest for the run.
func spDownloadFile(ctx context.Context, downloadURL string) ([]byte, error) {
	if downloadURL == "" {
		return nil, fmt.Errorf("sharepoint: item has no download URL (is it a folder?)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("sharepoint: build download request: %w", err)
	}
	req.Header.Set("User-Agent", connectorUserAgent)
	raw, err := graphDoWithRetry(req)
	if err != nil {
		return nil, fmt.Errorf("sharepoint: download failed: %w", err)
	}
	return raw, nil
}

// spDownloadItemContent downloads itemID's content via Graph's dedicated
// content endpoint (/sites/{site}/drive/items/{id}/content) rather than a
// listing's @microsoft.graph.downloadUrl — spDownloadItem's fallback for
// when Graph omits that field, which it does inconsistently for large/
// nested delta feeds in practice (observed: dozens of real files across
// nested folders coming back with no downloadUrl at all, delta-sync
// failing on every one with "item has no download URL"). This endpoint
// redirects (302) to a signed blob-storage URL; Go's http.Client follows
// the redirect automatically and — this matters — drops the
// Authorization header once the redirect target is a different host
// (standard net/http behavior since Go 1.8), so the bearer token is never
// sent on to blob storage.
func spDownloadItemContent(ctx context.Context, cfg sharePointConfig, itemID string) ([]byte, error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return nil, err
	}
	siteID, err := spSiteID(ctx, cfg, token)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphBaseURL+fmt.Sprintf("/sites/%s/drive/items/%s/content", siteID, itemID), nil)
	if err != nil {
		return nil, fmt.Errorf("sharepoint: build content request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", connectorUserAgent)
	raw, err := graphDoWithRetry(req)
	if err != nil {
		return nil, fmt.Errorf("sharepoint: download content failed: %w", err)
	}
	return raw, nil
}

// spDownloadItem downloads item's content, preferring its listing's
// short-lived downloadUrl (spDownloadFile, no extra Graph round-trip) and
// falling back to spDownloadItemContent's content-endpoint fetch — by
// item ID, which Graph always returns — whenever downloadUrl came back
// empty, OR whenever a populated one still fails at request time. The
// latter matters because downloadUrl is documented to expire within
// minutes: importSharePointFiles/deltaSyncSharePoint both resolve every
// selected item's downloadUrl once up front and then process items
// sequentially (download, ingest, embed), so a large enough backlog can
// easily outlast it for later items even though it was valid when listed.
// See spDownloadItemContent's doc comment for why the fallback endpoint
// exists at all.
func spDownloadItem(ctx context.Context, cfg sharePointConfig, item spDriveItem) ([]byte, error) {
	if item.DownloadURL == "" {
		if item.ID == "" {
			return nil, fmt.Errorf("sharepoint: item has no download URL and no item ID to fall back to (is it a folder?)")
		}
		return spDownloadItemContent(ctx, cfg, item.ID)
	}
	data, err := spDownloadFile(ctx, item.DownloadURL)
	if err == nil || item.ID == "" {
		return data, err
	}
	if data2, err2 := spDownloadItemContent(ctx, cfg, item.ID); err2 == nil {
		return data2, nil
	}
	return nil, err
}

// spSizeExceedsMaxFileMB is the size-only core spItemExceedsMaxFileMB
// wraps for spDriveItem — factored out so importSharePointShareLinks can
// apply the exact same pre-download gate to spSharedItem.Size (a
// differently-typed but equally already-known size from
// spResolveShareLink's response, no download needed to learn it).
func spSizeExceedsMaxFileMB(size int64, s appSettings) bool {
	maxMB := s.Import.MaxFileMB
	if maxMB <= 0 {
		maxMB = 25
	}
	return size > maxMB*1024*1024
}

// spItemExceedsMaxFileMB reports whether item's already-known Size (from
// the listing/delta response, no download needed) exceeds
// s.Import.MaxFileMB — the same limit and 25MB fallback extractText
// applies, just checked before spending a Graph download and holding the
// whole file in memory on something that will be rejected afterwards
// anyway (e.g. a multi-GB video dropped into a synced document library).
func spItemExceedsMaxFileMB(item spDriveItem, s appSettings) bool {
	return spSizeExceedsMaxFileMB(item.Size, s)
}

// spPreviewResult is what handleSharePointPreview returns: the listing
// for one folder, so the browser can show a checklist of files before
// anything is downloaded/embedded — same "preview, then select" UX as the
// PST folder picker (see pst.go's pstPreviewResult).
type spPreviewResult struct {
	Folder string        `json:"folder"`
	Items  []spDriveItem `json:"items"`
}

// previewSharePointFolder lists folderPath and wraps it as an
// spPreviewResult for handleSharePointPreview — no downloading or ingesting,
// just what the browser needs to render the file-picker checklist.
func previewSharePointFolder(ctx context.Context, cfg sharePointConfig, folderPath string) (spPreviewResult, error) {
	items, err := spListFolder(ctx, cfg, folderPath)
	if err != nil {
		return spPreviewResult{}, err
	}
	return spPreviewResult{Folder: folderPath, Items: items}, nil
}

// spDiscoverTree recursively walks cfg's document library starting at
// rootFolder ("" = library root) — see discover.go for the shared
// discoverNode/discoverBudget shape. Resolves token+siteID exactly once
// (unlike calling the public spListFolder in a loop, which would
// re-resolve both on every call), then recurses via spListFolderIn.
func spDiscoverTree(ctx context.Context, cfg sharePointConfig, rootFolder string, budget *discoverBudget) (discoverNode, error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return discoverNode{}, err
	}
	siteID, err := spSiteID(ctx, cfg, token)
	if err != nil {
		return discoverNode{}, err
	}
	return spDiscoverWalk(ctx, token, siteID, rootFolder, 0, budget), nil
}

// spDiscoverWalk lists folderPath's children and recurses into every
// subfolder, bounded by budget. A folder failing to list (e.g. a
// permission-scoped subfolder) is recorded on that node's own Error field
// — siblings/ancestors are unaffected.
func spDiscoverWalk(ctx context.Context, token, siteID, folderPath string, depth int, budget *discoverBudget) discoverNode {
	name := folderPath
	if idx := strings.LastIndex(folderPath, "/"); idx >= 0 {
		name = folderPath[idx+1:]
	}
	if name == "" {
		name = "/"
	}
	node := discoverNode{Name: name, Path: folderPath}
	if !budget.admit(ctx) {
		node.Truncated = true
		return node
	}
	items, err := spListFolderIn(ctx, token, siteID, folderPath)
	if err != nil {
		node.Error = err.Error()
		return node
	}
	for _, it := range items {
		if !it.IsFolder {
			node.FileCount++
			continue
		}
		if depth >= budget.maxDepth {
			node.Truncated = true
			continue
		}
		node.Children = append(node.Children, spDiscoverWalk(ctx, token, siteID, it.Path, depth+1, budget))
	}
	return node
}

// spImportResult/spProgress mirror pst.go's pstImportResult/pstProgress —
// same streaming-progress shape, reused by handleSharePointImport.
type spImportResult struct {
	baseImportResult
	Files int `json:"files"`
}

type spProgress struct {
	Result   spImportResult
	FileName string
}

// importSharePointFiles re-lists folderPath (Graph's downloadUrl is
// short-lived, so a preview from a while ago can't be reused directly)
// and downloads+ingests every item whose Path is in selected.
func importSharePointFiles(ctx context.Context, rag *ragSystem, s appSettings, cfg sharePointConfig, embedModel, folderPath string, selected map[string]bool, dryRun bool, onProgress func(spProgress)) (spImportResult, error) {
	var res spImportResult
	res.DryRun = dryRun
	items, err := spListFolder(ctx, cfg, folderPath)
	if err != nil {
		return res, err
	}
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	if verbose {
		log.Printf("[verbose] sharepoint import: site=%s folder=%s selected=%d dry_run=%v", cfg.SiteURL, folderPath, len(selected), dryRun)
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if item.IsFolder || !selected[item.Path] {
			continue
		}
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			return res, err
		}
		res.Files++

		if spItemExceedsMaxFileMB(item, s) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: file too large (%d bytes)", item.Path, item.Size))
			if onProgress != nil {
				onProgress(spProgress{Result: res, FileName: item.Name})
			}
			continue
		}
		data, err := spDownloadItem(ctx, cfg, item)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.Path, err))
			if onProgress != nil {
				onProgress(spProgress{Result: res, FileName: item.Name})
			}
			continue
		}
		outcome, err := ingestSharePointFile(rag, s, embedModel, cfg.SiteURL, item.Path, data, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.Path, err))
		} else if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
		pacer.count()
		if onProgress != nil {
			onProgress(spProgress{Result: res, FileName: item.Name})
		}
	}
	return res, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Delta sync — "what changed since last time", via Graph's drive delta
// feed, instead of importSharePointFiles' "browse a folder, pick files by
// hand" flow above. See ANLEITUNG.md's "SharePoint" section for why this
// exists: re-listing an entire drive on every sync doesn't scale, and
// Graph already tracks changes for exactly this purpose.
// ─────────────────────────────────────────────────────────────────────────────

// spGraphDeltaItem is one entry in a delta feed page — a superset of
// spGraphItem's fields: it also carries parentReference (to compute a
// full path, since delta entries can be anywhere in the drive, not just
// the one folder spListFolder was pointed at) and deleted (present only
// for removed items).
type spGraphDeltaItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	DownloadURL     string `json:"@microsoft.graph.downloadUrl"`
	ParentReference *struct {
		Path string `json:"path"`
	} `json:"parentReference"`
	Deleted *struct {
		State string `json:"state"`
	} `json:"deleted"`
}

// spDeltaResult is one page of a delta feed response. Exactly one of
// NextLink/DeltaLink is set: NextLink means more pages remain in this
// changeset, DeltaLink means this page was the last one and is what to
// resume from next time.
type spDeltaResult struct {
	Value     []spGraphDeltaItem `json:"value"`
	NextLink  string             `json:"@odata.nextLink"`
	DeltaLink string             `json:"@odata.deltaLink"`
}

// spItemPath computes a delta item's drive-root-relative path from its
// parentReference.path (Graph returns e.g. "/drive/root:/Documents/Sub")
// plus its own name — the same path shape spListFolder produces, so a
// file's sourceID (see ingestSharePointFile) matches whether it was
// imported via the folder browser or via delta sync.
func spItemPath(it spGraphDeltaItem) string {
	parent := ""
	if it.ParentReference != nil {
		parent = it.ParentReference.Path
	}
	if idx := strings.Index(parent, "root:"); idx >= 0 {
		parent = strings.Trim(parent[idx+len("root:"):], "/")
	} else {
		parent = ""
	}
	if parent == "" {
		return it.Name
	}
	return parent + "/" + it.Name
}

// spDeletedItem is one delta-feed deletion: the item's Graph id (always
// present, even on a delete entry) plus its best-effort Path computed
// from this page's own parentReference — deltaSyncSharePoint prefers
// cfg.ItemPaths's remembered path when available, since Graph doesn't
// reliably repopulate name/parentReference on a delete entry (Path may
// come back empty here in that case).
type spDeletedItem struct {
	ID   string
	Path string
}

// spDeltaSync calls cfg's drive delta feed, resuming from cfg.DeltaLink if
// set (otherwise a full initial sync of the whole drive), and follows
// every @odata.nextLink page within this one call so a large changeset
// isn't split across separate "Delta-Sync jetzt" clicks. Folders are
// skipped (same as spListFolder, which only ever lists files for
// import) — only file adds/updates and deletions are reported, deduped
// by item id across the whole walk so only each item's true final state
// (see spDeltaSyncFrom) is returned.
//
// If resuming from cfg.DeltaLink fails with Graph's documented 410 Gone
// (the resume token has been invalidated — error codes like
// resyncChangesApplyDifferences/resyncChangesUploadDifferences), this
// self-heals by restarting from a full walk instead of surfacing the
// failure: without this, every subsequent scheduled or manual run would
// keep re-requesting the exact same expired link and fail identically
// forever, since nothing else in this codebase ever clears DeltaLink on
// error (the caller only ever persists a *new* one, see
// deltaSyncSharePoint's callers in scheduler.go/handlers_import_connectors.go).
func spDeltaSync(ctx context.Context, cfg sharePointConfig) (items []spDriveItem, deleted []spDeletedItem, newDeltaLink string, err error) {
	items, deleted, newDeltaLink, err = spDeltaSyncFrom(ctx, cfg, cfg.DeltaLink)
	if err != nil && cfg.DeltaLink != "" && graphIsGone(err) {
		log.Printf("sharepoint: delta token für %s ungültig geworden (410 Gone) — starte vollständigen Neu-Walk", cfg.SiteURL)
		return spDeltaSyncFrom(ctx, cfg, "")
	}
	return items, deleted, newDeltaLink, err
}

// spDeltaSyncFrom is spDeltaSync's actual worker, parameterized on the
// resume link to use — split out so spDeltaSync can retry once from an
// empty link (full walk) after a 410 without duplicating this loop.
//
// Entries are deduped by item id across every page of this one walk
// (last occurrence wins) before being split into items/deleted: Microsoft's
// own delta-query docs state the same id can legitimately reappear more
// than once within a single walk ("replays"), and only the final
// occurrence reflects the item's true state — e.g. an id added on an
// earlier page and deleted on a later page is, in truth, gone; applying
// both independently (two plain, unrelated slices) would incorrectly
// re-add it.
func spDeltaSyncFrom(ctx context.Context, cfg sharePointConfig, deltaLink string) (items []spDriveItem, deleted []spDeletedItem, newDeltaLink string, err error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return nil, nil, "", err
	}

	path := deltaLink
	if path != "" {
		// A stored delta link is already the complete next request — no
		// need to resolve the site ID again (spListFolder-style callers do,
		// but here the link itself fully encodes where to resume).
		path = strings.TrimPrefix(path, graphBaseURL)
	} else {
		siteID, err := spSiteID(ctx, cfg, token)
		if err != nil {
			return nil, nil, "", err
		}
		path = fmt.Sprintf("/sites/%s/drive/root/delta", siteID)
	}

	type entry struct {
		item    *spDriveItem
		deleted *spDeletedItem
	}
	seen := make(map[string]entry)
	var order []string
	record := func(id string, e entry) {
		if id == "" {
			// Graph's delta feed is documented to always include id, but
			// guard defensively anyway: an empty id must never dedupe
			// against other empty-id entries, which would silently
			// discard unrelated items instead of just forgoing
			// last-occurrence tracking for this one (rare, ill-formed)
			// entry.
			id = fmt.Sprintf("\x00no-id:%d", len(order))
		}
		if _, ok := seen[id]; !ok {
			order = append(order, id)
		}
		seen[id] = e
	}
	flatten := func() ([]spDriveItem, []spDeletedItem) {
		items := make([]spDriveItem, 0, len(order))
		deleted := make([]spDeletedItem, 0, len(order))
		for _, id := range order {
			e := seen[id]
			switch {
			case e.item != nil:
				items = append(items, *e.item)
			case e.deleted != nil:
				deleted = append(deleted, *e.deleted)
			}
		}
		return items, deleted
	}

	// Per-run cap (import_limits.go): an empty DeltaLink means a full
	// initial walk of the entire drive, which could be tens of thousands
	// of files. When the cap is hit mid-enumeration we stop and hand back
	// the current page's nextLink AS the resume cursor — the caller
	// persists it as DeltaLink, so the next run/scheduler tick continues
	// exactly where this one stopped. A Graph delta nextLink is a valid
	// GET target for the next call, same as a deltaLink, so the backlog
	// drains in bounded chunks without missing or re-ingesting anything.
	maxItems := cfg.effectiveMaxItems(settings.get().Import)
	for {
		raw, err := spGraphGet(ctx, token, path)
		if err != nil {
			return nil, nil, "", err
		}
		var page spDeltaResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, nil, "", fmt.Errorf("sharepoint: parse delta response: %w", err)
		}
		for _, it := range page.Value {
			switch {
			case it.Deleted != nil:
				record(it.ID, entry{deleted: &spDeletedItem{ID: it.ID, Path: spItemPath(it)}})
			case it.Folder != nil:
				// folders themselves are never imported, only the files in them
			default:
				record(it.ID, entry{item: &spDriveItem{
					Name:        it.Name,
					Path:        spItemPath(it),
					Size:        it.Size,
					ID:          it.ID,
					DownloadURL: it.DownloadURL,
				}})
			}
		}
		if page.DeltaLink != "" {
			items, deleted = flatten()
			return items, deleted, page.DeltaLink, nil
		}
		if page.NextLink == "" {
			return nil, nil, "", fmt.Errorf("sharepoint: delta response has neither nextLink nor deltaLink")
		}
		// Stop at the cap (deduped count, not raw entries seen), resuming
		// from the very next page next time.
		if len(seen) >= maxItems {
			log.Printf("sharepoint: Delta-Sync bei %d Elementen auf das Import-Limit gestoßen — Rest folgt beim nächsten Lauf", len(seen))
			items, deleted = flatten()
			return items, deleted, page.NextLink, nil
		}
		path = strings.TrimPrefix(page.NextLink, graphBaseURL)
	}
}

// deltaSyncSharePoint is spDeltaSync's ingest/delete counterpart —
// importSharePointFiles' unattended equivalent: Graph itself decides
// what's new/changed/deleted, there's no preview/select step. Returns the
// new deltaLink and the updated item-id->path map alongside the result;
// the caller (handlers.go's handleSharePointDeltaSync, scheduler.go's
// sharepoint-delta-sync job) persists both via settings.update, since
// this function has no access to the settings store itself (same
// separation as every other ingest function in this file).
//
// Reconciles renames/moves using cfg.ItemPaths (see its doc comment in
// settings.go): when an already-known item id now reports a different
// path than last time, the old path's source is deleted once the new
// path has ingested successfully — success-gated deliberately, so a
// failed ingest under the new path never leaves the item with zero
// copies of its content (the old path/mapping is simply left untouched
// for a future run to retry).
func deltaSyncSharePoint(ctx context.Context, rag *ragSystem, s appSettings, cfg sharePointConfig, embedModel string, dryRun bool, onProgress func(spProgress)) (spImportResult, string, map[string]string, error) {
	var res spImportResult
	res.DryRun = dryRun
	items, deleted, newDeltaLink, err := spDeltaSync(ctx, cfg)
	if err != nil {
		return res, "", nil, err
	}
	if verbose {
		log.Printf("[verbose] sharepoint delta-sync: site=%s new=%d deleted=%d dry_run=%v", cfg.SiteURL, len(items), len(deleted), dryRun)
	}

	newItemPaths := make(map[string]string, len(cfg.ItemPaths))
	for id, p := range cfg.ItemPaths {
		newItemPaths[id] = p
	}

	for _, d := range deleted {
		if err := ctx.Err(); err != nil {
			// Fall back to the OLD cursor (cfg.DeltaLink/ItemPaths), not the
			// new one spDeltaSync already computed: that new link/item-paths
			// reflect the *listing* pass only — persisting it here would
			// silently and permanently skip every deletion/item after this
			// point in the batch, since Graph's delta feed never re-surfaces
			// an unchanged item once the cursor has advanced past it.
			// Re-walking the same window next run is safe (idempotent) —
			// ingestDocument's content-hash skip means already-ingested
			// items just resolve as Skipped again, no duplicates.
			return res, cfg.DeltaLink, cfg.ItemPaths, err
		}
		path := d.Path
		if known, ok := newItemPaths[d.ID]; ok {
			// Prefer our own remembered path over the feed's — Graph
			// doesn't reliably repopulate name/parentReference on a
			// delete entry, so d.Path can come back empty/unreliable.
			path = known
		}
		if path == "" {
			if onProgress != nil {
				onProgress(spProgress{Result: res, FileName: d.ID})
			}
			continue
		}
		sourceID := fmt.Sprintf("sharepoint:%s:%s", cfg.SiteURL, path)
		if !dryRun {
			if err := rag.deleteSource(sourceID); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: delete failed: %v", path, err))
			}
			delete(newItemPaths, d.ID)
		}
		if onProgress != nil {
			onProgress(spProgress{Result: res, FileName: path})
		}
	}

	// pacer.wait only — NOT capReached()/count(): items/deleted here are
	// already a bounded batch (spDeltaSyncFrom's own enumeration-side
	// maxItems cap, itself derived from cfg.MaxItemsPerRun as of the fix
	// above), so a second cap-and-break on top of that would reintroduce
	// exactly the "cursor already advanced past unwalked items" bug this
	// function's ctx.Err() handling above was just fixed to avoid — once
	// this bounded batch is fetched, it's always walked to completion (or
	// aborted via ctx.Err(), which correctly declines to advance the
	// cursor). pacer.wait alone is what was actually missing: every other
	// ingest loop in this file paces its requests via
	// Import.RequestDelayMS; this one fired them back-to-back.
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			// Same reasoning as the deleted-items loop above: fall back to
			// the OLD cursor rather than silently dropping the unwalked
			// tail of this batch.
			return res, cfg.DeltaLink, cfg.ItemPaths, err
		}
		if err := pacer.wait(ctx); err != nil {
			return res, cfg.DeltaLink, cfg.ItemPaths, err
		}
		res.Files++
		if spItemExceedsMaxFileMB(item, s) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: file too large (%d bytes)", item.Path, item.Size))
			if onProgress != nil {
				onProgress(spProgress{Result: res, FileName: item.Name})
			}
			continue
		}
		data, err := spDownloadItem(ctx, cfg, item)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.Path, err))
			if onProgress != nil {
				onProgress(spProgress{Result: res, FileName: item.Name})
			}
			continue
		}
		outcome, err := ingestSharePointFile(rag, s, embedModel, cfg.SiteURL, item.Path, data, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.Path, err))
			if onProgress != nil {
				onProgress(spProgress{Result: res, FileName: item.Name})
			}
			continue
		}
		if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
		if oldPath, ok := newItemPaths[item.ID]; ok && oldPath != item.Path && !dryRun {
			oldSourceID := fmt.Sprintf("sharepoint:%s:%s", cfg.SiteURL, oldPath)
			if err := rag.deleteSource(oldSourceID); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: cleanup of renamed/moved item's old path failed: %v", oldPath, err))
			}
		}
		if !dryRun && item.ID != "" {
			newItemPaths[item.ID] = item.Path
		}
		if onProgress != nil {
			onProgress(spProgress{Result: res, FileName: item.Name})
		}
	}
	return res, newDeltaLink, newItemPaths, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SharePoint modern pages ("Site Pages", the .aspx wiki/news/landing pages
// every modern SharePoint site has, distinct from — and never reachable
// through — the drive/document-library API every function above this uses).
// A page's actual body text lives in its canvasLayout web parts, resolved
// via Graph's dedicated Pages API (/sites/{id}/pages), not as a plain
// downloadable file the way a document library item is.
// ─────────────────────────────────────────────────────────────────────────────

// spPageSummary is one modern page, as R3 uses it for the Import tab's
// checklist — parallel to spDriveItem, but for the Pages API rather than
// the drive API.
type spPageSummary struct {
	ID    string `json:"id"`
	Name  string `json:"name"` // e.g. "EI-LO-VE-I-D-.aspx"
	Title string `json:"title"`
}

type spGraphPage struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Title string `json:"title"`
}

type spPagesResult struct {
	Value    []spGraphPage `json:"value"`
	NextLink string        `json:"@odata.nextLink"`
}

// spListPages lists every modern page in cfg's site, following
// @odata.nextLink pages the same way spDeltaSync does above.
func spListPages(ctx context.Context, cfg sharePointConfig) ([]spPageSummary, error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return nil, err
	}
	siteID, err := spSiteID(ctx, cfg, token)
	if err != nil {
		return nil, err
	}
	var out []spPageSummary
	path := fmt.Sprintf("/sites/%s/pages", siteID)
	for path != "" {
		raw, err := spGraphGet(ctx, token, path)
		if err != nil {
			return nil, err
		}
		var page spPagesResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("sharepoint: parse pages listing: %w", err)
		}
		for _, p := range page.Value {
			out = append(out, spPageSummary{ID: p.ID, Name: p.Name, Title: p.Title})
		}
		path = strings.TrimPrefix(page.NextLink, graphBaseURL)
	}
	return out, nil
}

// spCanvasWebPart is one web part inside a page's canvasLayout — InnerHTML
// is only present on text/paragraph web parts (the common case for a
// page's actual body copy); anything else (images, embeds, lists, Yammer
// feeds, ...) has no plain-text representation Graph exposes here and
// simply contributes nothing, the same "best-effort, not exhaustive"
// posture as every other extractor in this codebase.
type spCanvasWebPart struct {
	InnerHTML string `json:"innerHtml"`
}
type spCanvasColumn struct {
	WebParts []spCanvasWebPart `json:"webparts"`
}
type spCanvasSection struct {
	Columns []spCanvasColumn `json:"columns"`
}
type spPageDetail struct {
	Title        string `json:"title"`
	CanvasLayout struct {
		HorizontalSections []spCanvasSection `json:"horizontalSections"`
		// VerticalSection is the page's optional sidebar column — a single
		// section object (not an array) in Graph's canvasLayout. Pages
		// often put contact info, key links or summary text there;
		// dropping it silently lost that content from search entirely.
		VerticalSection *struct {
			WebParts []spCanvasWebPart `json:"webparts"`
		} `json:"verticalSection"`
	} `json:"canvasLayout"`
}

// spGetPageText fetches pageID's full body (Graph's microsoft.graph.
// sitePage cast, expanded to include canvasLayout) and flattens every text
// web part's innerHtml into one plain-text document — good enough for RAG
// chunking, not a faithful re-rendering of the page.
func spGetPageText(ctx context.Context, cfg sharePointConfig, pageID string) (title, text string, err error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return "", "", err
	}
	siteID, err := spSiteID(ctx, cfg, token)
	if err != nil {
		return "", "", err
	}
	raw, err := spGraphGet(ctx, token, fmt.Sprintf("/sites/%s/pages/%s/microsoft.graph.sitePage?$expand=canvasLayout", siteID, pageID))
	if err != nil {
		return "", "", err
	}
	var detail spPageDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return "", "", fmt.Errorf("sharepoint: parse page content: %w", err)
	}
	var body strings.Builder
	writeWebPart := func(wp spCanvasWebPart) {
		if strings.TrimSpace(wp.InnerHTML) == "" {
			return
		}
		if t := strings.TrimSpace(htmlToText(wp.InnerHTML)); t != "" {
			body.WriteString(t)
			body.WriteString("\n\n")
		}
	}
	for _, sec := range detail.CanvasLayout.HorizontalSections {
		for _, col := range sec.Columns {
			for _, wp := range col.WebParts {
				writeWebPart(wp)
			}
		}
	}
	// The sidebar (vertical section) contributes after the main body — see
	// spPageDetail.VerticalSection.
	if vs := detail.CanvasLayout.VerticalSection; vs != nil {
		for _, wp := range vs.WebParts {
			writeWebPart(wp)
		}
	}
	return detail.Title, strings.TrimSpace(body.String()), nil
}

// spPageImportResult/spPageProgress mirror spImportResult/spProgress above
// — same streaming-progress shape, reused by handleSharePointPagesImport.
type spPageImportResult struct {
	baseImportResult
	Pages int `json:"pages"`
}

type spPageProgress struct {
	Result spPageImportResult
	Name   string
}

// importSharePointPages fetches and ingests every page in selected —
// re-lists first (like importSharePointFiles re-lists its folder) so a
// page renamed/removed since the preview is handled the same "just skip
// what's no longer selectable" way.
func importSharePointPages(ctx context.Context, rag *ragSystem, s appSettings, cfg sharePointConfig, embedModel string, selected map[string]bool, dryRun bool, onProgress func(spPageProgress)) (spPageImportResult, error) {
	var res spPageImportResult
	res.DryRun = dryRun
	pages, err := spListPages(ctx, cfg)
	if err != nil {
		return res, err
	}
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	for _, p := range pages {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !selected[p.ID] {
			continue
		}
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			return res, err
		}
		res.Pages++

		title, text, err := spGetPageText(ctx, cfg, p.ID)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p.Name, err))
			if onProgress != nil {
				onProgress(spPageProgress{Result: res, Name: p.Name})
			}
			continue
		}
		if title == "" {
			title = p.Title
		}
		outcome, err := ingestSharePointPage(rag, s, embedModel, cfg.SiteURL, p.Name, title, text, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p.Name, err))
		} else if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
		pacer.count()
		if onProgress != nil {
			onProgress(spPageProgress{Result: res, Name: p.Name})
		}
	}
	return res, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SharePoint/OneDrive sharing links ("Freigabe-Links", the
// https://.../:x:/s/... or :b:/r/... short links a person gets from
// SharePoint's own "Copy link" action) — resolved via Graph's dedicated
// /shares endpoint rather than the drive/folder API above, since a shared
// link can point at a file in ANY site (not necessarily one otherwise
// configured as its own connection), identified only by the link itself.
// Still requires the calling app registration to have been separately
// granted Sites.Selected access to whichever site the link's file actually
// lives in — same per-site consent requirement as every other SharePoint
// access in this file, just not implied by which sharePointConfig the
// caller happened to pick (see handlers_import_connectors.go's
// handleSharePointShareLinkImport doc comment).
// ─────────────────────────────────────────────────────────────────────────────

// spEncodeShareURL implements Microsoft's documented sharing-link encoding
// (https://learn.microsoft.com/graph/api/shares-get): base64-encode the
// URL, make it URL-safe (unpadded, '/' -> '_', '+' -> '-'), prefix "u!".
func spEncodeShareURL(shareURL string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(shareURL))
	b64 = strings.TrimRight(b64, "=")
	b64 = strings.ReplaceAll(b64, "/", "_")
	b64 = strings.ReplaceAll(b64, "+", "-")
	return "u!" + b64
}

// spSharedItem is a sharing link resolved to an actual drive item — enough
// to download and ingest it, plus WebURL for provenance/logging.
type spSharedItem struct {
	Name        string
	Size        int64
	DownloadURL string
	WebURL      string
	IsFolder    bool
}

type spGraphSharedItem struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	WebURL string `json:"webUrl"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	DownloadURL string `json:"@microsoft.graph.downloadUrl"`
}

// spResolveShareLink resolves a sharing link to the drive item it points
// at. cfg only supplies the app credentials (TenantID/ClientID/
// ClientSecret) — the link itself, not cfg.SiteURL, determines which site
// is actually queried.
func spResolveShareLink(ctx context.Context, cfg sharePointConfig, shareURL string) (spSharedItem, error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return spSharedItem{}, err
	}
	encoded := spEncodeShareURL(shareURL)
	raw, err := spGraphGet(ctx, token, fmt.Sprintf("/shares/%s/driveItem", encoded))
	if err != nil {
		return spSharedItem{}, err
	}
	var it spGraphSharedItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return spSharedItem{}, fmt.Errorf("sharepoint: parse shared item: %w", err)
	}
	return spSharedItem{Name: it.Name, Size: it.Size, DownloadURL: it.DownloadURL, WebURL: it.WebURL, IsFolder: it.Folder != nil}, nil
}

// spShareLinkImportResult/spShareLinkProgress mirror spImportResult/
// spProgress above — same streaming-progress shape, reused by
// handleSharePointShareLinkImport.
type spShareLinkImportResult struct {
	baseImportResult
	Links int `json:"links"`
}

type spShareLinkProgress struct {
	Result spShareLinkImportResult
	Name   string
}

// importSharePointShareLinks resolves and ingests every URL in links —
// each independently, so one bad/inaccessible link doesn't abort the rest
// (recorded in res.Errors instead, same as every other per-item failure in
// this file).
func importSharePointShareLinks(ctx context.Context, rag *ragSystem, s appSettings, cfg sharePointConfig, embedModel string, links []string, dryRun bool, onProgress func(spShareLinkProgress)) (spShareLinkImportResult, error) {
	var res spShareLinkImportResult
	res.DryRun = dryRun
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	for _, link := range links {
		link = strings.TrimSpace(link)
		if link == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			return res, err
		}
		res.Links++

		item, err := spResolveShareLink(ctx, cfg, link)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", link, err))
			if onProgress != nil {
				onProgress(spShareLinkProgress{Result: res, Name: link})
			}
			continue
		}
		if item.IsFolder {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: ist ein Ordner — Freigabe-Link-Import unterstützt nur einzelne Dateien", link))
			if onProgress != nil {
				onProgress(spShareLinkProgress{Result: res, Name: item.Name})
			}
			continue
		}
		// Same pre-download size gate importSharePointFiles/deltaSyncSharePoint
		// already apply via spItemExceedsMaxFileMB — previously missing here,
		// so a share link pointing at an oversized file (a multi-GB video, a
		// large backup) was fully downloaded and held in memory before
		// extractText's own after-the-fact check finally rejected it.
		if spSizeExceedsMaxFileMB(item.Size, s) {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: Datei zu groß (%d Bytes)", link, item.Size))
			if onProgress != nil {
				onProgress(spShareLinkProgress{Result: res, Name: item.Name})
			}
			continue
		}
		data, err := spDownloadFile(ctx, item.DownloadURL)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", link, err))
			if onProgress != nil {
				onProgress(spShareLinkProgress{Result: res, Name: item.Name})
			}
			continue
		}
		outcome, err := ingestSharePointSharedLink(rag, s, embedModel, link, item.Name, data, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", link, err))
		} else if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
		pacer.count()
		if onProgress != nil {
			onProgress(spShareLinkProgress{Result: res, Name: item.Name})
		}
	}
	return res, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Live SharePoint search (search_sharepoint tool, agent.go's
// buildLiveTools/sharepointsearch.go) — Microsoft Graph's Search API,
// queried at answer time rather than relying on content already imported
// into the vector store. Distinct from every function above: those all
// pull content INTO R3's own store ahead of time; this reaches out live,
// the same "answer-time, not pre-ingested" shape as mssql.go/shop.go's
// tools. See sharePointConfig.LiveSearchEnabled's doc comment (settings.go)
// for the permission caveat — Microsoft's Search API's exact app-only
// permission behavior can differ from every other SharePoint feature in
// this file.
// ─────────────────────────────────────────────────────────────────────────────

// spSearchMaxResults bounds a single search_sharepoint call — enough for a
// model to judge relevance without flooding its context with hit
// summaries, same reasoning as shopMaxResults.
const spSearchMaxResults = 8

// spSearchHit is one search result, as R3 uses it — enough for a model to
// decide whether it's relevant and mention the link, not the full file
// content (that's what search_knowledge_base is for, once/if the file is
// actually imported).
type spSearchHit struct {
	Name    string
	WebURL  string
	Summary string
}

type spSearchRequestBody struct {
	Requests []spSearchOneRequest `json:"requests"`
}
type spSearchOneRequest struct {
	EntityTypes []string           `json:"entityTypes"`
	Query       spSearchQueryField `json:"query"`
	From        int                `json:"from"`
	Size        int                `json:"size"`
}
type spSearchQueryField struct {
	QueryString string `json:"queryString"`
}

type spSearchResponse struct {
	Value []struct {
		HitsContainers []struct {
			Hits []struct {
				Summary  string `json:"summary"`
				Resource struct {
					Name   string `json:"name"`
					WebURL string `json:"webUrl"`
				} `json:"resource"`
			} `json:"hits"`
		} `json:"hitsContainers"`
	} `json:"value"`
}

// spSearch queries Graph's Search API for query, scoped to cfg.SiteURL via
// a KQL "path:" filter appended to the query string — so a live search
// stays limited to the one consented/relevant site instead of searching
// (or attempting to search) the whole tenant, which the calling app's
// Sites.Selected grant likely can't see anyway.
func spSearch(ctx context.Context, cfg sharePointConfig, query string) ([]spSearchHit, error) {
	token, err := spAccessToken(ctx, cfg)
	if err != nil {
		return nil, err
	}
	kqlQuery := fmt.Sprintf("%s path:%q", query, strings.TrimRight(cfg.SiteURL, "/"))
	reqBody := spSearchRequestBody{Requests: []spSearchOneRequest{{
		EntityTypes: []string{"driveItem"},
		Query:       spSearchQueryField{QueryString: kqlQuery},
		From:        0,
		Size:        spSearchMaxResults,
	}}}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	raw, err := graphWrite(ctx, "POST", token, "/search/query", bodyBytes)
	if err != nil {
		return nil, err
	}
	var resp spSearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("sharepoint: parse search response: %w", err)
	}
	var hits []spSearchHit
	for _, v := range resp.Value {
		for _, hc := range v.HitsContainers {
			for _, h := range hc.Hits {
				hits = append(hits, spSearchHit{Name: h.Resource.Name, WebURL: h.Resource.WebURL, Summary: h.Summary})
			}
		}
	}
	return hits, nil
}

const sharePointSearchToolName = "search_sharepoint"

// sharePointLiveSearchConns filters conns down to just the enabled, opted-
// in ones (sharePointConfig.LiveSearchEnabled) — shared by the tool
// definition (to list valid "site" choices) and appendSharePointSearchTool
// (to decide whether to offer the tool at all).
func sharePointLiveSearchConns(conns []sharePointConfig) []sharePointConfig {
	var out []sharePointConfig
	for _, c := range conns {
		if c.Enabled && c.LiveSearchEnabled {
			out = append(out, c)
		}
	}
	return out
}

// sharePointSearchToolDef describes the search_sharepoint tool in OpenAI's
// function-calling schema shape — a "site" parameter only appears (and is
// required) once more than one connection has opted in; with exactly one,
// it's implied and omitted entirely so the model doesn't have to name it.
func sharePointSearchToolDef(liveConns []sharePointConfig) toolDef {
	names := make([]string, 0, len(liveConns))
	for _, c := range liveConns {
		names = append(names, c.Name)
	}
	props := map[string]any{
		"query": map[string]any{"type": "string", "description": "Suchbegriff(e) — wie eine normale Stichwortsuche, kein ganzer Satz."},
	}
	required := []string{"query"}
	if len(names) > 1 {
		props["site"] = map[string]any{"type": "string", "enum": names, "description": "Welche der konfigurierten SharePoint-Sites durchsucht werden soll."}
		required = append(required, "site")
	}
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name:        sharePointSearchToolName,
			Description: "Durchsucht den Inhalt einer SharePoint-Site LIVE über Microsoft Graph — unabhängig davon, ob eine Datei bereits in die Wissensbasis importiert wurde. Nützlich, wenn eine Datei sich kürzlich geändert haben könnte oder noch gar nicht importiert ist. Liefert nur kurze Treffer (Dateiname, Ausschnitt, Link) zurück, KEINEN vollständigen Text — ist eine Datei bereits importiert, liefert search_knowledge_base den vollständigen, zitierfähigen Inhalt und sollte dafür bevorzugt werden.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

// sharePointSearchToolExecutor adapts spSearch to the generic toolExecutor
// shape — decode the model's JSON arguments, resolve which connection to
// search (the named "site" argument, or the sole opted-in connection if
// there's only one), run the search, render a short text list.
func sharePointSearchToolExecutor(liveConns []sharePointConfig) toolExecutor {
	return func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Query string `json:"query"`
			Site  string `json:"site"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("empty query")
		}
		var conn sharePointConfig
		var ok bool
		if args.Site != "" {
			for _, c := range liveConns {
				if strings.EqualFold(c.Name, args.Site) {
					conn, ok = c, true
					break
				}
			}
		} else if len(liveConns) > 0 {
			conn, ok = liveConns[0], true
		}
		if !ok {
			return "", fmt.Errorf("keine passende, für Live-Suche freigegebene SharePoint-Verbindung gefunden")
		}
		hits, err := spSearch(ctx, conn, args.Query)
		if err != nil {
			// Prefixed distinctly from "(keine Treffer)" below so the model
			// doesn't conflate "the live search is unreachable/misconfigured
			// right now" with "this specific query had no matches" — same
			// reasoning as shopSearchToolExecutor's identical split.
			return "", fmt.Errorf("SharePoint-Live-Suche momentan nicht möglich (%w) — nicht als \"kein Ergebnis\" interpretieren, sondern der Nutzerin mitteilen, dass die Suche gerade fehlschlägt", err)
		}
		if len(hits) == 0 {
			return fmt.Sprintf("(keine Treffer für diese Suche in %q — ggf. mit anderen/allgemeineren Begriffen erneut versuchen)", conn.Name), nil
		}
		var b strings.Builder
		for i, h := range hits {
			fmt.Fprintf(&b, "%d. %s", i+1, h.Name)
			if h.WebURL != "" {
				fmt.Fprintf(&b, " (%s)", h.WebURL)
			}
			b.WriteString("\n")
			if strings.TrimSpace(h.Summary) != "" {
				fmt.Fprintf(&b, "   %s\n", h.Summary)
			}
		}
		return b.String(), nil
	}
}

// appendSharePointSearchTool offers search_sharepoint only once at least
// one connection has opted in (LiveSearchEnabled), the caller's preset
// allows the "sharepoint_search" tool category, AND (per-connection) the
// caller passes that connection's own AccessControl — same "settings AND
// preset both gate it" shape as buildLiveTools' other entries (agent.go),
// plus the per-caller AccessControl check every other live tool
// (MSSQL/Shop/HTTP templates) already applies. Previously missing here
// entirely: any caller whose preset merely listed "sharepoint_search" got
// full live results from EVERY opted-in site regardless of department,
// even when that same site's already-imported content (source_kind
// "sharepoint_file"/"sharepoint_page") was department-restricted via
// SourceAccess — a live-search bypass of a restriction the admin believed
// already covered this site.
func appendSharePointSearchTool(tools []toolDef, executors map[string]toolExecutor, conns []sharePointConfig, presetTools []string, user string, groups []string) []toolDef {
	liveConns := sharePointLiveSearchConns(conns)
	var allowed []sharePointConfig
	for _, c := range liveConns {
		if c.AccessControl.allows(user, groups) {
			allowed = append(allowed, c)
		}
	}
	if len(allowed) > 0 && presetAllowsTool(presetTools, "sharepoint_search") {
		tools = append(tools, sharePointSearchToolDef(allowed))
		executors[sharePointSearchToolName] = sharePointSearchToolExecutor(allowed)
	}
	return tools
}
