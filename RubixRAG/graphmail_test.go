package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testExchangeGraphConfig() exchangeGraphConfig {
	return exchangeGraphConfig{
		connRuntime: connRuntime{Name: "vertrieb-postfach"},
		Enabled:     true, TenantID: "tenant", ClientID: "client", ClientSecret: "secret",
		Mailbox: "vertrieb@rubix.com",
	}
}

// ─── Discover (Task A) ──────────────────────────────────────────────────────
//
// No test coverage of any kind existed for Exchange's Discover before this
// file — mirroring SharePoint, whose own spDiscoverTree (sharepoint.go) is
// likewise untested today; both share discover.go's discoverNode/
// discoverBudget shape and this file's tests follow sharepoint_test.go's
// established newFakeGraphServer pattern. What these tests CANNOT verify,
// same limitation SharePoint's own connector carries and states plainly
// rather than implying otherwise: real Microsoft Graph response quirks
// (throttling behavior under a real tenant's load, real permission-scoped
// subfolder error shapes, pagination edge cases Graph's actual
// implementation might produce that a hand-written fake server doesn't) —
// there is no Azure AD tenant/mailbox reachable from this environment to
// verify against.

func TestExchangeDiscoverTreeRequiresMailbox(t *testing.T) {
	cfg := testExchangeGraphConfig()
	cfg.Mailbox = ""
	if _, err := exchangeDiscoverTree(context.Background(), cfg, newDiscoverBudget()); err == nil {
		t.Fatal("want an error when Mailbox is unconfigured")
	}
}

// TestExchangeDiscoverTreeListsNestedFolders confirms one level of real
// recursion: a top-level folder with childFolderCount > 0 gets its own
// children listed via a second Graph call, and both totalItemCount values
// surface as ItemCount on the matching node.
func TestExchangeDiscoverTreeListsNestedFolders(t *testing.T) {
	var paths []string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/mailFolders/inbox-id/childFolders"):
			_, _ = w.Write([]byte(`{"value": [
				{"id": "sub1-id", "displayName": "Kunden", "childFolderCount": 0, "totalItemCount": 7}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			_, _ = w.Write([]byte(`{"value": [
				{"id": "inbox-id", "displayName": "Inbox", "childFolderCount": 1, "totalItemCount": 42},
				{"id": "sent-id", "displayName": "Gesendet", "childFolderCount": 0, "totalItemCount": 5}
			]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	node, err := exchangeDiscoverTree(context.Background(), testExchangeGraphConfig(), newDiscoverBudget())
	if err != nil {
		t.Fatalf("exchangeDiscoverTree: %v", err)
	}
	if node.Name != "vertrieb@rubix.com" {
		t.Fatalf("want the mailbox as the synthesized root name, got %q", node.Name)
	}
	if len(node.Children) != 2 {
		t.Fatalf("want 2 top-level folders, got %d: %+v", len(node.Children), node.Children)
	}
	inbox := node.Children[0]
	if inbox.Name != "Inbox" || inbox.ItemCount != 42 {
		t.Fatalf("unexpected Inbox node: %+v", inbox)
	}
	if len(inbox.Children) != 1 || inbox.Children[0].Name != "Kunden" || inbox.Children[0].ItemCount != 7 {
		t.Fatalf("want Inbox's child 'Kunden' (7 items), got %+v", inbox.Children)
	}
	sent := node.Children[1]
	if sent.Name != "Gesendet" || len(sent.Children) != 0 {
		t.Fatalf("Gesendet has no children, want none recursed, got %+v", sent)
	}
}

// TestExchangeDiscoverTreeFollowsNextLink confirms a paginated top-level
// folder listing (@odata.nextLink) is fully consumed in one Discover call,
// mirroring TestSpDeltaSyncInitialFollowsNextLinkThenReturnsDeltaLink's
// SharePoint equivalent (sharepoint_test.go).
func TestExchangeDiscoverTreeFollowsNextLink(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/page2"):
			_, _ = w.Write([]byte(`{"value": [{"id": "b-id", "displayName": "Archiv", "childFolderCount": 0, "totalItemCount": 3}]}`))
		default:
			_, _ = w.Write([]byte(`{
				"value": [{"id": "a-id", "displayName": "Inbox", "childFolderCount": 0, "totalItemCount": 1}],
				"@odata.nextLink": "` + graphBaseURL + `/users/vertrieb%40rubix.com/mailFolders/page2"
			}`))
		}
	})

	node, err := exchangeDiscoverTree(context.Background(), testExchangeGraphConfig(), newDiscoverBudget())
	if err != nil {
		t.Fatalf("exchangeDiscoverTree: %v", err)
	}
	if len(node.Children) != 2 {
		t.Fatalf("want both pages' folders (2 total), got %d: %+v", len(node.Children), node.Children)
	}
	if node.Children[0].Name != "Inbox" || node.Children[1].Name != "Archiv" {
		t.Fatalf("unexpected folder names: %+v", node.Children)
	}
}

// TestExchangeDiscoverTreeIsolatesFailingSubfolder confirms a permission-
// scoped (or otherwise failing) subfolder's error is recorded on its OWN
// node without failing sibling folders or the overall call — the same
// "best-effort, partial tree" contract discover.go's package comment
// documents for all three Discover connectors.
func TestExchangeDiscoverTreeIsolatesFailingSubfolder(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/mailFolders/locked-id/childFolders"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error": "access denied"}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			_, _ = w.Write([]byte(`{"value": [
				{"id": "locked-id", "displayName": "Restricted", "childFolderCount": 1, "totalItemCount": 0},
				{"id": "ok-id", "displayName": "Normal", "childFolderCount": 0, "totalItemCount": 2}
			]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	// graphGet retries 4xx? No — only 429/5xx are retried (graph.go's
	// graphGet), so the 403 above surfaces immediately as this subfolder's
	// own error, not after a long retry loop.
	node, err := exchangeDiscoverTree(context.Background(), testExchangeGraphConfig(), newDiscoverBudget())
	if err != nil {
		t.Fatalf("exchangeDiscoverTree: %v", err)
	}
	if len(node.Children) != 2 {
		t.Fatalf("want both siblings present despite one failing, got %+v", node.Children)
	}
	restricted, normal := node.Children[0], node.Children[1]
	if restricted.Error == "" {
		t.Fatalf("want Restricted's listing failure recorded on its own node, got %+v", restricted)
	}
	if normal.Error != "" {
		t.Fatalf("want Normal unaffected by its sibling's failure, got %+v", normal)
	}
}

// TestExchangeDiscoverTreeTruncatesAtMaxDepth confirms a folder deeper than
// budget.maxDepth is marked Truncated instead of recursed into — using a
// budget of maxDepth 1 (rather than the 4-level production default) so the
// test doesn't need to fabricate 5 levels of fake folders.
func TestExchangeDiscoverTreeTruncatesAtMaxDepth(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/mailFolders/sub1-id/childFolders"):
			t.Fatal("must not descend past maxDepth")
		case strings.Contains(r.URL.Path, "/mailFolders/inbox-id/childFolders"):
			_, _ = w.Write([]byte(`{"value": [{"id": "sub1-id", "displayName": "Sub1", "childFolderCount": 1, "totalItemCount": 3}]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			_, _ = w.Write([]byte(`{"value": [{"id": "inbox-id", "displayName": "Inbox", "childFolderCount": 1, "totalItemCount": 10}]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	budget := &discoverBudget{maxDepth: 1, maxNodes: 500}
	node, err := exchangeDiscoverTree(context.Background(), testExchangeGraphConfig(), budget)
	if err != nil {
		t.Fatalf("exchangeDiscoverTree: %v", err)
	}
	if len(node.Children) != 1 || node.Children[0].Name != "Inbox" {
		t.Fatalf("unexpected top level: %+v", node.Children)
	}
	sub1 := node.Children[0].Children
	if len(sub1) != 1 || sub1[0].Name != "Sub1" {
		t.Fatalf("want Sub1 listed (depth 1, within budget), got %+v", sub1)
	}
	if !sub1[0].Truncated {
		t.Fatalf("want Sub1 marked Truncated (it has children, but depth 1 >= maxDepth 1), got %+v", sub1[0])
	}
}

// TestHandleDiscoverExchangeEndToEnd drives the actual HTTP handler
// (handlers.go registers it at /api/import/exchange/discover) rather than
// just the tree-walker, confirming the wire contract end to end: a POST
// body decodes into exchangeGraphConfig and the not-yet-saved
// ClientSecret is used as-is (nothing saved to compare it against here).
// TestImportExchangeMailHandlesAttachments closes three attachment gaps
// that previously existed in the Exchange/Graph import path: (1) an
// oversized attachment is now rejected via Graph's "size" field BEFORE
// base64-decoding it into memory, (2) a base64 decode failure is recorded
// as a specific warning instead of silently bumping Skipped with no trace,
// and (3) a normal attachment is still ingested exactly as before —
// AttachmentWarnings (mailAttachmentWarnings, connector.go) surfaces (1)
// and (2) instead of them vanishing into an undifferentiated skip count.
func TestImportExchangeMailHandlesAttachments(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/attachments"):
			_, _ = w.Write([]byte(`{"value": [
				{"@odata.type": "#microsoft.graph.fileAttachment", "name": "ok.txt", "contentBytes": "` + base64.StdEncoding.EncodeToString([]byte("Attachment body text long enough to be chunked and embedded.")) + `", "size": 60},
				{"@odata.type": "#microsoft.graph.fileAttachment", "name": "huge.txt", "contentBytes": "` + base64.StdEncoding.EncodeToString([]byte("irrelevant")) + `", "size": 5242880},
				{"@odata.type": "#microsoft.graph.fileAttachment", "name": "corrupt.txt", "contentBytes": "not-valid-base64!!", "size": 20}
			]}`))
		default:
			_, _ = w.Write([]byte(`{
				"subject":"Anfrage mit Anhängen",
				"receivedDateTime":"2026-07-10T09:30:00Z",
				"hasAttachments": true,
				"from":{"emailAddress":{"name":"Kunde X","address":"kunde@example.com"}},
				"toRecipients":[{"emailAddress":{"name":"J. Doe","address":"j.doe@rubix.com"}}],
				"body":{"contentType":"text","content":"Siehe Anhänge."}
			}`))
		}
	})

	rag, s := newTestRAG(t)
	s.Import.MaxFileMB = 1 // 1MB ceiling, so the 5MB-declared attachment is rejected by size alone
	cfg := testExchangeGraphConfig()

	res, err := importExchangeMail(context.Background(), rag, s, cfg, "test-embed", map[string]bool{"msg-1": true}, false, nil)
	if err != nil {
		t.Fatalf("importExchangeMail: %v", err)
	}
	if res.Messages != 1 {
		t.Fatalf("want 1 message, got %d", res.Messages)
	}
	if res.Attachments != 1 {
		t.Fatalf("want 1 attachment actually ingested (ok.txt), got %d", res.Attachments)
	}
	if res.Skipped != 2 {
		t.Fatalf("want 2 attachments skipped (oversized + corrupt base64), got %d", res.Skipped)
	}
	if len(res.AttachmentWarnings) != 2 {
		t.Fatalf("want 2 attachment warnings, got %d: %v", len(res.AttachmentWarnings), res.AttachmentWarnings)
	}
	joined := strings.Join(res.AttachmentWarnings, "\n")
	if !strings.Contains(joined, "huge.txt") || !strings.Contains(joined, "zu groß") {
		t.Errorf("want a 'zu groß' warning naming huge.txt, got %v", res.AttachmentWarnings)
	}
	if !strings.Contains(joined, "corrupt.txt") || !strings.Contains(joined, "base64") {
		t.Errorf("want a base64-decode-failure warning naming corrupt.txt, got %v", res.AttachmentWarnings)
	}
}

func TestHandleDiscoverExchangeEndToEnd(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value": [{"id": "inbox-id", "displayName": "Inbox", "childFolderCount": 0, "totalItemCount": 1}]}`))
	})
	withTestGlobalSettings(t, appSettings{})

	body, _ := json.Marshal(testExchangeGraphConfig())
	r := httptest.NewRequest(http.MethodPost, "/api/import/exchange/discover", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleDiscoverExchange(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var node discoverNode
	if err := json.Unmarshal(w.Body.Bytes(), &node); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(node.Children) != 1 || node.Children[0].Name != "Inbox" {
		t.Fatalf("unexpected discover tree: %+v", node)
	}
}

// TestListExchangeMailSince: the incremental listing must filter by
// receivedDateTime >= watermark, order ascending (oldest first), and follow
// @odata.nextLink across pages — the three properties the scheduler's
// watermark sync depends on.
func TestListExchangeMailSince(t *testing.T) {
	var sawFilter, sawOrder string
	var base string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"value": [
				{"id": "m3", "subject": "Drittes", "receivedDateTime": "2026-07-03T10:00:00Z",
				 "from": {"emailAddress": {"name": "C", "address": "c@example.com"}}}
			]}`))
			return
		}
		sawFilter = r.URL.Query().Get("$filter")
		sawOrder = r.URL.Query().Get("$orderby")
		_, _ = w.Write([]byte(`{"value": [
			{"id": "m1", "subject": "Erstes", "receivedDateTime": "2026-07-01T10:00:00Z",
			 "from": {"emailAddress": {"name": "A", "address": "a@example.com"}}},
			{"id": "m2", "subject": "Zweites", "receivedDateTime": "2026-07-02T10:00:00Z",
			 "from": {"emailAddress": {"name": "B", "address": "b@example.com"}}}
		], "@odata.nextLink": "` + base + `/users/box/mailFolders/inbox/messages?page=2"}`))
	})
	base = graphBaseURL

	msgs, err := listExchangeMailSince(context.Background(), testExchangeGraphConfig(), "2026-07-01T00:00:00Z", 10)
	if err != nil {
		t.Fatalf("listExchangeMailSince: %v", err)
	}
	if sawFilter != "receivedDateTime ge 2026-07-01T00:00:00Z" {
		t.Fatalf("wrong $filter sent: %q", sawFilter)
	}
	if sawOrder != "receivedDateTime asc" {
		t.Fatalf("wrong $orderby sent: %q", sawOrder)
	}
	if len(msgs) != 3 || msgs[0].ID != "m1" || msgs[2].ID != "m3" {
		t.Fatalf("want m1..m3 across both pages in order, got %+v", msgs)
	}

	// The max cap truncates listing (and stops pagination).
	capped, err := listExchangeMailSince(context.Background(), testExchangeGraphConfig(), "2026-07-01T00:00:00Z", 2)
	if err != nil {
		t.Fatalf("listExchangeMailSince capped: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("want listing capped at 2, got %d", len(capped))
	}
}

// TestImportExchangeMailIDsReportsProcessedOnCap: when the per-run cap cuts
// the run short, processed must say exactly how many ids were attempted, in
// the given order — the scheduler advances its watermark by indexing
// ids[processed-1], so an off-by-one here would skip or re-import mail.
func TestImportExchangeMailIDsReportsProcessedOnCap(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"subject":"Nachricht",
			"receivedDateTime":"2026-07-10T09:30:00Z",
			"hasAttachments": false,
			"from":{"emailAddress":{"name":"A","address":"a@example.com"}},
			"toRecipients":[],
			"body":{"contentType":"text","content":"Textkörper lang genug für einen Chunk im Test."}
		}`))
	})

	rag, s := newTestRAG(t)
	cfg := testExchangeGraphConfig()
	cfg.MaxItemsPerRun = 2 // cap below len(ids)

	ids := []string{"id-1", "id-2", "id-3", "id-4"}
	res, processed, err := importExchangeMailIDs(context.Background(), rag, s, cfg, "test-embed", ids, false, nil)
	if err != nil {
		t.Fatalf("importExchangeMailIDs: %v", err)
	}
	if processed != 2 {
		t.Fatalf("want processed=2 at cap, got %d", processed)
	}
	if res.Messages != 2 {
		t.Fatalf("want 2 messages imported, got %d", res.Messages)
	}
	capNoted := false
	for _, e := range res.Errors {
		if strings.Contains(e, "Limit") || strings.Contains(e, "cap") || strings.Contains(e, "Maximal") {
			capNoted = true
		}
	}
	if !capNoted {
		t.Fatalf("want the cap noted in errors, got %v", res.Errors)
	}
}
