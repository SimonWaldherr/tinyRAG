package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type rssImportRequest struct {
	URL    string `json:"url"`
	DryRun bool   `json:"dry_run,omitempty"`
}

func handleRSSImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req rssImportRequest
		if err := decodeJSONBody(r, &req); err != nil || strings.TrimSpace(req.URL) == "" {
			writeJSONError(w, "missing RSS/Atom feed URL", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		res, err := importRSSFeed(ctx, rag, settings.get(), strings.TrimSpace(req.URL), req.DryRun)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, res)
	}
}

// connRequest is embedded into every preview/import/delta-sync request
// struct below — Connection names which of a connector type's configured
// connections (settings.go's now-plural SharePoint/OneDrive/ExchangeGraph/
// IMAP/Teams/Confluence/Jira/Freshservice/GitHub/SAPS4 lists) this request targets. May be
// left empty when exactly one connection of that type is configured (see
// requireConn, connruntime.go).
type connRequest struct {
	Connection string `json:"connection,omitempty"`
}

type spPreviewRequest struct {
	connRequest
	Folder string `json:"folder"`
}

func handleSharePointPreview(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spPreviewRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.SharePoint, req.Connection, "SharePoint import")
		if !ok {
			return
		}
		preview, err := previewSharePointFolder(r.Context(), conn, req.Folder)
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, preview)
	}
}

type spImportRequest struct {
	connRequest
	Folder string   `json:"folder"`
	Files  []string `json:"files"`
	DryRun bool     `json:"dry_run,omitempty"`
}
type spStreamMsg struct {
	Type     string         `json:"type"`
	FileName string         `json:"file_name,omitempty"`
	Result   spImportResult `json:"result"`
}

func handleSharePointImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.SharePoint, req.Connection, "SharePoint import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importSharePointFiles(ctx, rag, s, conn, s.activeEmbedModel(), req.Folder, makeSet(req.Files), req.DryRun, func(p spProgress) { emit(spStreamMsg{Type: "progress", FileName: p.FileName, Result: p.Result}) })
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "sharepoint:"+conn.Name, res.baseImportResult)
		emit(spStreamMsg{Type: "done", Result: res})
	}
}

// spDeltaSyncRequest deliberately allows an otherwise-empty body: a plain
// POST (with just an optional "connection") runs a real sync.
type spDeltaSyncRequest struct {
	connRequest
	DryRun bool `json:"dry_run,omitempty"`
}

func handleSharePointDeltaSync(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spDeltaSyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.SharePoint, req.Connection, "SharePoint import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, newDeltaLink, newItemPaths, err := deltaSyncSharePoint(ctx, rag, s, conn, s.activeEmbedModel(), req.DryRun, func(p spProgress) { emit(spStreamMsg{Type: "progress", FileName: p.FileName, Result: p.Result}) })
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		if newDeltaLink != "" && !req.DryRun {
			_ = settings.update(func(cur *appSettings) {
				if c, i, ok := findConnIndex(cur.SharePoint, conn.Name); ok {
					c.DeltaLink = newDeltaLink
					c.ItemPaths = newItemPaths
					cur.SharePoint[i] = c
				}
			})
		}
		logImportAudit(r, "sharepoint_delta_sync:"+conn.Name, res.baseImportResult)
		emit(spStreamMsg{Type: "done", Result: res})
	}
}

// handleOneDriveSync is the manual counterpart to the scheduler's
// onedrive-delta-sync job. Both paths call syncOneDrive and persist the
// Graph cursor only after every item in the returned window succeeded, so a
// file/extraction failure is retried instead of being skipped by a cursor
// that has already moved past it.
func handleOneDriveSync(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spDeltaSyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.OneDrive, req.Connection, "OneDrive import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, cursor, err := syncOneDrive(ctx, rag, s, conn, s.activeEmbedModel(), req.DryRun, func(p spProgress) {
			emit(spStreamMsg{Type: "progress", FileName: p.FileName, Result: p.Result})
		})
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		if cursor != "" && !req.DryRun && len(res.Errors) == 0 {
			_ = settings.update(func(cur *appSettings) {
				if c, i, found := findConnIndex(cur.OneDrive, conn.Name); found {
					c.DeltaLink = cursor
					cur.OneDrive[i] = c
				}
			})
		}
		logImportAudit(r, "onedrive_delta_sync:"+conn.Name, res.baseImportResult)
		emit(spStreamMsg{Type: "done", Result: res})
	}
}

// spPagesPreviewRequest mirrors spPreviewRequest above, minus Folder — the
// Pages API (sharepoint.go's spListPages) always lists a site's whole
// Site Pages library flat, there's no per-folder browsing step.
type spPagesPreviewRequest struct {
	connRequest
}

func handleSharePointPagesPreview(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req spPagesPreviewRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONError(w, "invalid body", http.StatusBadRequest)
		return
	}
	s := settings.get()
	conn, ok := requireConn(w, s.SharePoint, req.Connection, "SharePoint Site-Pages import")
	if !ok {
		return
	}
	pages, err := spListPages(r.Context(), conn)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"pages": pages})
}

type spPagesImportRequest struct {
	connRequest
	Pages  []string `json:"pages"`
	DryRun bool     `json:"dry_run,omitempty"`
}
type spPagesStreamMsg struct {
	Type   string             `json:"type"`
	Name   string             `json:"name,omitempty"`
	Result spPageImportResult `json:"result"`
}

func handleSharePointPagesImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spPagesImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.SharePoint, req.Connection, "SharePoint Site-Pages import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importSharePointPages(ctx, rag, s, conn, s.activeEmbedModel(), makeSet(req.Pages), req.DryRun, func(p spPageProgress) { emit(spPagesStreamMsg{Type: "progress", Name: p.Name, Result: p.Result}) })
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "sharepoint_pages:"+conn.Name, res.baseImportResult)
		emit(spPagesStreamMsg{Type: "done", Result: res})
	}
}

// spShareLinkImportRequest takes one or more sharing links at once (the
// Import tab's textarea accepts one per line) — each resolved and ingested
// independently (sharepoint.go's importSharePointShareLinks), so a typo'd
// or inaccessible link doesn't block the others. Connection only supplies
// which configured app registration's credentials to use for the Graph
// call — see importSharePointShareLinks' doc comment for why the link
// itself, not this connection's own SiteURL, decides which site is
// actually queried.
type spShareLinkImportRequest struct {
	connRequest
	URLs   []string `json:"urls"`
	DryRun bool     `json:"dry_run,omitempty"`
}
type spShareLinkStreamMsg struct {
	Type   string                  `json:"type"`
	Name   string                  `json:"name,omitempty"`
	Result spShareLinkImportResult `json:"result"`
}

func handleSharePointShareLinkImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spShareLinkImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.SharePoint, req.Connection, "SharePoint Freigabe-Link import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importSharePointShareLinks(ctx, rag, s, conn, s.activeEmbedModel(), req.URLs, req.DryRun, func(p spShareLinkProgress) {
			emit(spShareLinkStreamMsg{Type: "progress", Name: p.Name, Result: p.Result})
		})
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "sharepoint_sharelink:"+conn.Name, res.baseImportResult)
		emit(spShareLinkStreamMsg{Type: "done", Result: res})
	}
}

type exchangePreviewRequest struct {
	connRequest
}

func handleExchangeMailPreview(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req exchangePreviewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.ExchangeGraph, req.Connection, "Exchange (Graph) import")
		if !ok {
			return
		}
		preview, err := previewExchangeMail(r.Context(), conn, importPreviewLimit(s.Import))
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, preview)
	}
}

type graphMailImportRequest struct {
	connRequest
	MessageIDs []string `json:"message_ids"`
	DryRun     bool     `json:"dry_run,omitempty"`
}
type graphMailStreamMsg struct {
	Type    string                `json:"type"`
	Subject string                `json:"subject,omitempty"`
	Result  graphMailImportResult `json:"result"`
}

func handleExchangeMailImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req graphMailImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.ExchangeGraph, req.Connection, "Exchange (Graph) import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importExchangeMail(ctx, rag, s, conn, s.activeEmbedModel(), makeSet(req.MessageIDs), req.DryRun, func(p graphMailProgress) {
			emit(graphMailStreamMsg{Type: "progress", Subject: p.Subject, Result: p.Result})
		})
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "exchange_graph:"+conn.Name, res.baseImportResult)
		emit(graphMailStreamMsg{Type: "done", Result: res})
	}
}

type imapStreamMsg struct {
	Type    string           `json:"type"`
	Subject string           `json:"subject,omitempty"`
	Result  imapImportResult `json:"result"`
}
type imapImportRequest struct {
	connRequest
	DryRun bool `json:"dry_run,omitempty"`
}

func handleIMAPImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req imapImportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.IMAP, req.Connection, "IMAP import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importIMAPMessages(ctx, newIMAPClient(conn), rag, s, s.activeEmbedModel(), conn, req.DryRun, func(p imapProgress) { emit(imapStreamMsg{Type: "progress", Subject: p.Subject, Result: p.Result}) })
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		if res.LastUID > conn.LastUID && !req.DryRun {
			_ = settings.update(func(cur *appSettings) {
				if c, i, ok := findConnIndex(cur.IMAP, conn.Name); ok {
					c.LastUID = res.LastUID
					cur.IMAP[i] = c
				}
			})
		}
		logImportAudit(r, "imap:"+conn.Name, res.baseImportResult)
		emit(imapStreamMsg{Type: "done", Result: res})
	}
}

func handleTeamsPreview(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req connRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.Teams, req.Connection, "Teams import")
		if !ok {
			return
		}
		preview, err := previewTeamsMessages(r.Context(), conn, importPreviewLimit(s.Import))
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, preview)
	}
}

type teamsImportRequest struct {
	connRequest
	MessageIDs []string `json:"message_ids"`
	DryRun     bool     `json:"dry_run,omitempty"`
}
type teamsStreamMsg struct {
	Type    string            `json:"type"`
	Subject string            `json:"subject,omitempty"`
	Result  teamsImportResult `json:"result"`
}

func handleTeamsImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req teamsImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.Teams, req.Connection, "Teams import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importTeamsMessages(ctx, rag, s, conn, s.activeEmbedModel(), makeSet(req.MessageIDs), req.DryRun, func(p teamsProgress) { emit(teamsStreamMsg{Type: "progress", Subject: p.Subject, Result: p.Result}) })
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "teams:"+conn.Name, res.baseImportResult)
		emit(teamsStreamMsg{Type: "done", Result: res})
	}
}

func handleConfluencePreview(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req connRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.Confluence, req.Connection, "Confluence import")
		if !ok {
			return
		}
		preview, err := previewConfluencePages(r.Context(), conn, importPreviewLimit(s.Import))
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, preview)
	}
}

type confluenceImportRequest struct {
	connRequest
	PageIDs []string `json:"page_ids"`
	DryRun  bool     `json:"dry_run,omitempty"`
}
type confluenceStreamMsg struct {
	Type   string                 `json:"type"`
	Title  string                 `json:"title,omitempty"`
	Result confluenceImportResult `json:"result"`
}

func handleConfluenceImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req confluenceImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.Confluence, req.Connection, "Confluence import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importConfluencePages(ctx, rag, s, conn, s.activeEmbedModel(), makeSet(req.PageIDs), req.DryRun, func(p confluenceProgress) {
			emit(confluenceStreamMsg{Type: "progress", Title: p.Title, Result: p.Result})
		})
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "confluence:"+conn.Name, res.baseImportResult)
		emit(confluenceStreamMsg{Type: "done", Result: res})
	}
}

func handleJiraPreview(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req connRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.Jira, req.Connection, "Jira import")
		if !ok {
			return
		}
		preview, err := previewJiraIssues(r.Context(), conn, importPreviewLimit(s.Import))
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, preview)
	}
}

type jiraImportRequest struct {
	connRequest
	IssueKeys []string `json:"issue_keys"`
	DryRun    bool     `json:"dry_run,omitempty"`
}
type jiraStreamMsg struct {
	Type   string           `json:"type"`
	Key    string           `json:"key,omitempty"`
	Result jiraImportResult `json:"result"`
}

func handleJiraImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req jiraImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.Jira, req.Connection, "Jira import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importJiraIssues(ctx, rag, s, conn, s.activeEmbedModel(), makeSet(req.IssueKeys), req.DryRun, func(p jiraProgress) { emit(jiraStreamMsg{Type: "progress", Key: p.Key, Result: p.Result}) })
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "jira:"+conn.Name, res.baseImportResult)
		emit(jiraStreamMsg{Type: "done", Result: res})
	}
}

func handleFreshservicePreview(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req connRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.Freshservice, req.Connection, "Freshservice import")
		if !ok {
			return
		}
		preview, err := previewFreshserviceTickets(r.Context(), conn, importPreviewLimit(s.Import))
		if err != nil {
			writeJSONError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, preview)
	}
}

type freshserviceImportRequest struct {
	connRequest
	TicketIDs []int `json:"ticket_ids"`
	DryRun    bool  `json:"dry_run,omitempty"`
}
type freshserviceStreamMsg struct {
	Type   string                   `json:"type"`
	ID     int                      `json:"id,omitempty"`
	Result freshserviceImportResult `json:"result"`
}

func handleFreshserviceImport(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req freshserviceImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, "invalid body", http.StatusBadRequest)
			return
		}
		s := settings.get()
		conn, ok := requireConn(w, s.Freshservice, req.Connection, "Freshservice import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, err := importFreshserviceTickets(ctx, rag, s, conn, s.activeEmbedModel(), makeSet(req.TicketIDs), req.DryRun, func(p freshserviceProgress) {
			emit(freshserviceStreamMsg{Type: "progress", ID: p.ID, Result: p.Result})
		})
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		logImportAudit(r, "freshservice:"+conn.Name, res.baseImportResult)
		emit(freshserviceStreamMsg{Type: "done", Result: res})
	}
}

type githubStreamMsg struct {
	Type   string           `json:"type"`
	Result githubSyncResult `json:"result"`
}

func handleGitHubSync(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spDeltaSyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.GitHub, req.Connection, "GitHub import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, next, err := syncGitHubRepository(ctx, rag, s, conn, s.activeEmbedModel(), req.DryRun)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		if !req.DryRun && len(res.Errors) == 0 {
			_ = settings.update(func(cur *appSettings) {
				if c, i, found := findConnIndex(cur.GitHub, conn.Name); found {
					c.LastSyncedAt = next.LastSyncedAt
					c.CycleStartedAt = next.CycleStartedAt
					c.NextPage = next.NextPage
					cur.GitHub[i] = c
				}
			})
		}
		logImportAudit(r, "github_sync:"+conn.Name, res.baseImportResult)
		emit(githubStreamMsg{Type: "done", Result: res})
	}
}

type sapS4StreamMsg struct {
	Type   string            `json:"type"`
	Result sapS4ImportResult `json:"result"`
}

func handleSAPS4Sync(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req spDeltaSyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s := settings.get()
		conn, ok := requireConn(w, s.SAPS4, req.Connection, "SAP S/4 import")
		if !ok {
			return
		}
		emit := ndjsonStream(w)
		ctx, cancel := context.WithTimeout(r.Context(), conn.effectiveTimeout(30*60))
		defer cancel()
		res, next, err := syncSAPS4(ctx, rag, s, conn, s.activeEmbedModel(), req.DryRun)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		if !req.DryRun && len(res.Errors) == 0 {
			_ = settings.update(func(cur *appSettings) {
				if c, i, found := findConnIndex(cur.SAPS4, conn.Name); found {
					c.DeltaLink = next.DeltaLink
					c.NextLink = next.NextLink
					cur.SAPS4[i] = c
				}
			})
		}
		logImportAudit(r, "sap_s4_sync:"+conn.Name, res.baseImportResult)
		emit(sapS4StreamMsg{Type: "done", Result: res})
	}
}
