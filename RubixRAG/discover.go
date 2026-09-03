package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// "Discover" — a recursive, read-only structure preview for SharePoint,
// Folder and Exchange/Outlook (see tab-settings.html: that connector is
// labeled "Outlook / Exchange Online (Microsoft Graph)"), distinct from
// (and in addition to) each connector's existing one-level-at-a-time
// preview/picker used to actually select what to import. The point of
// Discover is answering "what's even in here, roughly, before I decide
// what to configure" — a folder tree with file/item counts, not a
// file-by-file checklist.
//
// All three share one wire shape (discoverNode) and one walk-budget
// (discoverBudget): best-effort, bounded by depth and total node count, so
// a huge SharePoint library or mailbox can't turn one click into an
// unbounded number of Graph calls. Hitting the budget or a single node's
// own listing failing are both reported IN the tree (Truncated/Error on
// that node) rather than failing the whole request — a partial structure
// is far more useful to an admin deciding where to point a connector than
// an all-or-nothing error. Each connector's own recursive walker lives in
// its own file (sharepoint.go's spDiscoverTree, graphmail.go's
// exchangeDiscoverTree) — only the shared shape, budget, and the three
// HTTP handlers (which follow conntest.go's "decode the not-yet-saved
// config straight from the body" contract, since the Discover button POSTs
// the same not-yet-saved card state as "Verbindung testen") live here.
// ─────────────────────────────────────────────────────────────────────────────

type discoverNode struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// FileCount: SharePoint/Folder only (files directly in this node).
	FileCount int `json:"file_count,omitempty"`
	// ItemCount: Exchange only (Graph's own totalItemCount for this mail folder).
	ItemCount int            `json:"item_count,omitempty"`
	Children  []discoverNode `json:"children,omitempty"`
	// Truncated: depth or total-node budget was hit descending into/from
	// this node — its own Children may be incomplete.
	Truncated bool `json:"truncated,omitempty"`
	// Error: THIS node's own listing call failed (e.g. a permission-scoped
	// subfolder) — recursion into it stops, siblings/ancestors are
	// unaffected. Distinct from Truncated (a budget limit, not a failure).
	Error string `json:"error,omitempty"`
}

// discoverBudget bounds one Discover walk's total fan-out (every visited
// node, i.e. every listing call) and its wall-clock time via ctx — shared
// by all three connectors so "how deep"/"how much total work" is one
// policy, not reinvented per connector.
type discoverBudget struct {
	maxDepth int
	maxNodes int
	seen     int
}

// admit reports whether one more node/listing-call may be made: false on
// either the node cap or ctx being done (deadline/cancel). Both are
// treated identically by callers — stop, mark Truncated, return the
// partial tree built so far, never a hard error. Discover is a best-effort
// preview, not an all-or-nothing operation.
func (b *discoverBudget) admit(ctx context.Context) bool {
	if ctx.Err() != nil || b.seen >= b.maxNodes {
		return false
	}
	b.seen++
	return true
}

const (
	discoverMaxDepthDefault = 4                // levels below the requested starting folder
	discoverMaxNodesDefault = 500              // total nodes visited == total listing calls for SharePoint/Exchange
	discoverTimeout         = 60 * time.Second // generous but finite — a heavily-throttled Graph tenant still returns a partial tree rather than hanging the request
)

func newDiscoverBudget() *discoverBudget {
	return &discoverBudget{maxDepth: discoverMaxDepthDefault, maxNodes: discoverMaxNodesDefault}
}

// --- SharePoint ---

// spDiscoverRequest mirrors handleTestSharePoint's request shape
// (conntest.go) — the full not-yet-saved sharePointConfig, since the
// Discover button POSTs the settings card's current in-memory state
// exactly like "Verbindung testen" does. Folder optionally sets a
// non-root starting point (mirrors previewSharePointFolder's folderPath);
// the Settings-page Discover button itself never sets one, so it always
// starts at the document library root.
func handleDiscoverSharePoint(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		sharePointConfig
		Folder string `json:"folder,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().SharePoint, req.Name)
	req.ClientSecret = resolveTestSecret(req.ClientSecret, saved.ClientSecret)

	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	node, err := spDiscoverTree(ctx, req.sharePointConfig, req.Folder, newDiscoverBudget())
	if err != nil {
		writeJSONError(w, err.Error(), 400)
		return
	}
	writeJSON(w, node)
}

// --- Folder ---

func handleDiscoverFolder(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg folderConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		writeJSONError(w, "Pfad ist leer", 400)
		return
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		writeJSONError(w, "Pfad nicht erreichbar oder kein Ordner", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	node := folderDiscoverTree(ctx, path, 0, newDiscoverBudget())
	writeJSON(w, node)
}

// folderDiscoverTree recursively lists root's structure — plain
// os.ReadDir, no ingestion side effects (unlike ingestFolder). ctx-aware
// because, unlike the unbounded background ingestFolder walk, this is a
// synchronous request a human is waiting on — a stalled network mount
// shouldn't hang it indefinitely.
func folderDiscoverTree(ctx context.Context, root string, depth int, budget *discoverBudget) discoverNode {
	node := discoverNode{Name: filepath.Base(root), Path: root}
	if !budget.admit(ctx) {
		node.Truncated = true
		return node
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		node.Error = err.Error()
		return node
	}
	for _, e := range entries {
		if e.IsDir() {
			if depth >= budget.maxDepth {
				node.Truncated = true
				continue
			}
			node.Children = append(node.Children, folderDiscoverTree(ctx, filepath.Join(root, e.Name()), depth+1, budget))
		} else {
			node.FileCount++
		}
	}
	return node
}

// --- Exchange / Outlook (Microsoft Graph) ---

func handleDiscoverExchange(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var cfg exchangeGraphConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, "invalid body: "+err.Error(), 400)
		return
	}
	saved, _ := findConnByName(settings.get().ExchangeGraph, cfg.Name)
	cfg.ClientSecret = resolveTestSecret(cfg.ClientSecret, saved.ClientSecret)

	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	node, err := exchangeDiscoverTree(ctx, cfg, newDiscoverBudget())
	if err != nil {
		writeJSONError(w, err.Error(), 400)
		return
	}
	writeJSON(w, node)
}
