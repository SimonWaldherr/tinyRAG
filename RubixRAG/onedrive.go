package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"path"
	"strings"
)

// OneDrive uses exactly the same app-only Graph token plumbing as
// SharePoint, but deliberately addresses one configured drive directly.
// This avoids the ambiguous /me endpoint, which is unavailable to a daemon
// application and would make the imported scope dependent on a person.
func oneDriveCreds(cfg oneDriveConfig) graphCreds {
	return graphCreds{
		TenantID: cfg.TenantID, ClientID: cfg.ClientID,
		ClientSecret: cfg.ClientSecret, ClientSecretEnv: cfg.ClientSecretEnv,
	}
}

func oneDriveAccessToken(ctx context.Context, cfg oneDriveConfig) (string, error) {
	return graphAccessToken(ctx, oneDriveCreds(cfg))
}

func oneDriveInitialDeltaPath(ctx context.Context, cfg oneDriveConfig, token string) (string, error) {
	driveID := strings.TrimSpace(cfg.DriveID)
	if driveID == "" {
		return "", fmt.Errorf("onedrive: drive_id not configured")
	}
	folder := strings.Trim(strings.TrimSpace(cfg.FolderPath), "/")
	drive := url.PathEscape(driveID)
	if folder == "" {
		return "/drives/" + drive + "/root/delta", nil
	}
	escaped := (&url.URL{Path: folder}).EscapedPath()
	// Graph documents delta for a drive root or a drive-item ID, not a
	// path-shaped delta endpoint. Resolve the configured human-friendly path
	// first, then use its immutable item ID as the delta root.
	raw, err := graphGet(ctx, token, fmt.Sprintf("/drives/%s/root:/%s", drive, escaped))
	if err != nil {
		return "", fmt.Errorf("onedrive: resolve folder_path: %w", err)
	}
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", fmt.Errorf("onedrive: parse folder response: %w", err)
	}
	if item.ID == "" {
		return "", fmt.Errorf("onedrive: folder_path did not resolve to a drive item")
	}
	return fmt.Sprintf("/drives/%s/items/%s/delta", drive, url.PathEscape(item.ID)), nil
}

// oneDriveGraphPathFromLink accepts a Graph next/delta link only when it
// still points at the configured Graph endpoint. Although Graph normally
// returns its own URL, this check keeps a malformed/stored continuation
// response from becoming a bearer-token request to another host.
func oneDriveGraphPathFromLink(link string) (string, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return "", nil
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("onedrive: invalid delta link: %w", err)
	}
	if !u.IsAbs() {
		if !strings.HasPrefix(u.Path, "/") {
			return "", fmt.Errorf("onedrive: delta link must be an absolute URL or path")
		}
		return u.RequestURI(), nil
	}
	base, err := url.Parse(graphBaseURL)
	if err != nil || !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return "", fmt.Errorf("onedrive: delta link host does not match Microsoft Graph")
	}
	return u.RequestURI(), nil
}

func oneDriveSourceID(cfg oneDriveConfig, itemID string) string {
	// A driveItem ID survives rename and move, unlike an item path. It is the
	// source identity, while the current filename remains SourceName for the
	// UI/citation. This also lets a delete delta remove the exact source even
	// when Graph omits parent/name details from the tombstone.
	return fmt.Sprintf("onedrive:%s:%s", strings.TrimSpace(cfg.DriveID), strings.TrimSpace(itemID))
}

func oneDriveDownloadItem(ctx context.Context, cfg oneDriveConfig, token string, item spDriveItem) ([]byte, error) {
	if item.DownloadURL != "" {
		if raw, err := spDownloadFile(ctx, item.DownloadURL); err == nil {
			return raw, nil
		}
	}
	if item.ID == "" {
		return nil, fmt.Errorf("onedrive: item has no id/content URL")
	}
	return graphGet(ctx, token, fmt.Sprintf("/drives/%s/items/%s/content", url.PathEscape(cfg.DriveID), url.PathEscape(item.ID)))
}

// oneDriveDeltaSyncFrom lists one bounded Graph delta window. Its shape
// deliberately mirrors spDeltaSyncFrom: a nextLink is persisted when the
// configured item cap is hit, so a large initial drive is drained safely
// across later runs instead of silently being truncated.
func oneDriveDeltaSyncFrom(ctx context.Context, cfg oneDriveConfig, deltaLink string) ([]spDriveItem, []spDeletedItem, string, error) {
	token, err := oneDriveAccessToken(ctx, cfg)
	if err != nil {
		return nil, nil, "", err
	}
	pathToFetch, err := oneDriveGraphPathFromLink(deltaLink)
	if err != nil {
		return nil, nil, "", err
	}
	if pathToFetch == "" {
		pathToFetch, err = oneDriveInitialDeltaPath(ctx, cfg, token)
		if err != nil {
			return nil, nil, "", err
		}
	}

	type entry struct {
		item    *spDriveItem
		deleted *spDeletedItem
	}
	seen := map[string]entry{}
	var order []string
	record := func(id string, e entry) {
		if id == "" {
			id = fmt.Sprintf("\x00no-id:%d", len(order))
		}
		if _, ok := seen[id]; !ok {
			order = append(order, id)
		}
		seen[id] = e // Graph may replay an item; final state wins.
	}
	flatten := func() ([]spDriveItem, []spDeletedItem) {
		items := make([]spDriveItem, 0, len(order))
		deleted := make([]spDeletedItem, 0, len(order))
		for _, id := range order {
			e := seen[id]
			if e.item != nil {
				items = append(items, *e.item)
			} else if e.deleted != nil {
				deleted = append(deleted, *e.deleted)
			}
		}
		return items, deleted
	}

	maxItems := cfg.effectiveMaxItems(settings.get().Import)
	for {
		raw, err := graphGet(ctx, token, pathToFetch)
		if err != nil {
			return nil, nil, "", err
		}
		var page spDeltaResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, nil, "", fmt.Errorf("onedrive: parse delta response: %w", err)
		}
		for _, it := range page.Value {
			switch {
			case it.Deleted != nil:
				record(it.ID, entry{deleted: &spDeletedItem{ID: it.ID, Path: spItemPath(it)}})
			case it.Folder != nil:
				// Folders are represented by their files, never as documents.
			default:
				record(it.ID, entry{item: &spDriveItem{ID: it.ID, Name: it.Name, Path: spItemPath(it), Size: it.Size, DownloadURL: it.DownloadURL}})
			}
		}
		if page.DeltaLink != "" {
			items, deleted := flatten()
			return items, deleted, page.DeltaLink, nil
		}
		if page.NextLink == "" {
			return nil, nil, "", fmt.Errorf("onedrive: delta response has neither nextLink nor deltaLink")
		}
		if len(seen) >= maxItems {
			items, deleted := flatten()
			return items, deleted, page.NextLink, nil
		}
		pathToFetch, err = oneDriveGraphPathFromLink(page.NextLink)
		if err != nil {
			return nil, nil, "", err
		}
	}
}

func oneDriveDeltaSync(ctx context.Context, cfg oneDriveConfig) ([]spDriveItem, []spDeletedItem, string, error) {
	items, deleted, cursor, err := oneDriveDeltaSyncFrom(ctx, cfg, cfg.DeltaLink)
	if err != nil && cfg.DeltaLink != "" && graphIsGone(err) {
		log.Printf("onedrive: delta token for drive %s expired (410 Gone); starting a full re-sync", cfg.DriveID)
		return oneDriveDeltaSyncFrom(ctx, cfg, "")
	}
	return items, deleted, cursor, err
}

// syncOneDrive applies one delta window. Drive-item IDs are stable source
// identifiers, so a rename/move simply replaces the existing source rather
// than requiring path-based cleanup bookkeeping.
func syncOneDrive(ctx context.Context, rag *ragSystem, s appSettings, cfg oneDriveConfig, embedModel string, dryRun bool, onProgress func(spProgress)) (spImportResult, string, error) {
	var res spImportResult
	res.DryRun = dryRun
	items, deleted, cursor, err := oneDriveDeltaSync(ctx, cfg)
	if err != nil {
		return res, "", err
	}
	for _, d := range deleted {
		if err := ctx.Err(); err != nil {
			return res, cfg.DeltaLink, err
		}
		itemPath := d.Path
		if d.ID != "" && !dryRun {
			if err := rag.deleteSource(oneDriveSourceID(cfg, d.ID)); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: delete failed: %v", d.ID, err))
			}
		}
		if onProgress != nil {
			onProgress(spProgress{Result: res, FileName: itemPath})
		}
	}

	token, err := oneDriveAccessToken(ctx, cfg)
	if err != nil {
		return res, cfg.DeltaLink, err
	}
	pacer := newImportPacer(s.Import, cfg.MaxItemsPerRun)
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return res, cfg.DeltaLink, err
		}
		if err := pacer.wait(ctx); err != nil {
			return res, cfg.DeltaLink, err
		}
		res.Files++
		if spSizeExceedsMaxFileMB(item.Size, s) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: file too large (%d bytes)", item.Path, item.Size))
			continue
		}
		data, err := oneDriveDownloadItem(ctx, cfg, token, item)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.Path, err))
			continue
		}
		if item.ID == "" {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: item has no stable id", item.Path))
			continue
		}
		outcome, err := ingestRemoteFile(rag, s, embedModel, oneDriveSourceID(cfg, item.ID), "onedrive_file", path.Base(item.Path), data, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.Path, err))
			continue
		}
		if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
		if onProgress != nil {
			onProgress(spProgress{Result: res, FileName: item.Name})
		}
	}
	return res, cursor, nil
}
