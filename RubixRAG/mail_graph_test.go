package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testInteractiveExchangeConn(allowedUsers ...string) exchangeGraphConfig {
	cfg := testExchangeGraphConfig()
	cfg.InteractiveEnabled = true
	cfg.AllowedUsers = allowedUsers
	return cfg
}

// ---- resolveInteractiveExchangeOption / mailGraphAvailable -----------------

func TestFindInteractiveExchangeConn(t *testing.T) {
	authorized := sessionClaims{User: "j.doe", Mail: "j.doe@rubix.com"}
	unauthorized := sessionClaims{User: "x.other", Mail: "x.other@rubix.com"}
	noMail := sessionClaims{User: "no.mail"}

	t.Run("authorized user gets the connection with mailbox overridden", func(t *testing.T) {
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
		conn, ok := resolveInteractiveExchangeOption(s, authorized, "")
		if !ok {
			t.Fatal("want authorized")
		}
		if conn.Mailbox != "j.doe@rubix.com" {
			t.Fatalf("want Mailbox overridden to the caller's own address, got %q", conn.Mailbox)
		}
	})

	t.Run("case-insensitive match, and matches on User too", func(t *testing.T) {
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("J.DOE@rubix.com")}}
		if _, ok := resolveInteractiveExchangeOption(s, authorized, ""); !ok {
			t.Fatal("want a case-insensitive match on Mail")
		}
		s2 := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe")}}
		if _, ok := resolveInteractiveExchangeOption(s2, authorized, ""); !ok {
			t.Fatal("want a match on User (CN/login) too")
		}
	})

	t.Run("user not in allow-list is rejected", func(t *testing.T) {
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
		if _, ok := resolveInteractiveExchangeOption(s, unauthorized, ""); ok {
			t.Fatal("want an unlisted user rejected")
		}
	})

	t.Run("empty allow-list authorizes nobody", func(t *testing.T) {
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn()}}
		if _, ok := resolveInteractiveExchangeOption(s, authorized, ""); ok {
			t.Fatal("want an empty AllowedUsers to authorize nobody (opt-in, not opt-out)")
		}
	})

	t.Run("InteractiveEnabled off rejects even an allow-listed user", func(t *testing.T) {
		cfg := testInteractiveExchangeConn("j.doe@rubix.com")
		cfg.InteractiveEnabled = false
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{cfg}}
		if _, ok := resolveInteractiveExchangeOption(s, authorized, ""); ok {
			t.Fatal("want InteractiveEnabled=false to reject regardless of AllowedUsers")
		}
	})

	t.Run("connection disabled entirely rejects", func(t *testing.T) {
		cfg := testInteractiveExchangeConn("j.doe@rubix.com")
		cfg.Enabled = false
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{cfg}}
		if _, ok := resolveInteractiveExchangeOption(s, authorized, ""); ok {
			t.Fatal("want Enabled=false to reject")
		}
	})

	t.Run("caller with no mail attribute is rejected regardless of allow-list", func(t *testing.T) {
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("no.mail")}}
		if _, ok := resolveInteractiveExchangeOption(s, noMail, ""); ok {
			t.Fatal("want a caller with no Mail rejected — there is no mailbox to address")
		}
	})

	t.Run("second connection can authorize a user the first doesn't", func(t *testing.T) {
		first := testInteractiveExchangeConn("someone.else@rubix.com")
		first.Name = "first"
		second := testInteractiveExchangeConn("j.doe@rubix.com")
		second.Name = "second"
		s := appSettings{ExchangeGraph: []exchangeGraphConfig{first, second}}
		conn, ok := resolveInteractiveExchangeOption(s, authorized, "")
		if !ok || conn.Name != "second" {
			t.Fatalf("want the second connection to match, got ok=%v conn=%+v", ok, conn)
		}
	})
}

func TestMailGraphAvailable(t *testing.T) {
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}

	t.Run("no session: false", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if mailGraphAvailable(s, r) {
			t.Fatal("want false with no session cookie")
		}
	})

	t.Run("authorized session: true", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		if !mailGraphAvailable(s, r) {
			t.Fatal("want true for an authorized session")
		}
	})

	t.Run("unauthorized session: false", func(t *testing.T) {
		w := httptest.NewRecorder()
		issueSession(w, &ldapUser{CN: "someone.else", Mail: "someone.else@rubix.com"})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		if mailGraphAvailable(s, r) {
			t.Fatal("want false for a session not on the allow-list")
		}
	})
}

// ---- handleMailGraphList / handleMailGraphMessage / handleMailGraphSaveDraft

func sessionCookiesFor(t *testing.T, user *ldapUser) []*http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	issueSession(w, user)
	return w.Result().Cookies()
}

func TestHandleMailGraphListRequiresAuthorization(t *testing.T) {
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
	withTestGlobalSettings(t, s)

	t.Run("no session: 401", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/list", nil)
		w := httptest.NewRecorder()
		handleMailGraphList(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", w.Code)
		}
	})

	t.Run("unauthorized session: 403", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/list", nil)
		for _, c := range sessionCookiesFor(t, &ldapUser{CN: "someone.else", Mail: "someone.else@rubix.com"}) {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handleMailGraphList(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", w.Code)
		}
	})
}

func TestHandleMailGraphListReturnsCallersOwnMailbox(t *testing.T) {
	var sawPath string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[
			{"id":"msg-1","subject":"Anfrage Lieferzeit","receivedDateTime":"2026-07-10T09:30:00Z","from":{"emailAddress":{"name":"Kunde X","address":"kunde@example.com"}}}
		]}`))
	})

	s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{"limit": 10})
	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/list", bytes.NewReader(body))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphList(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	// The request must have gone against the CALLER's own mailbox
	// (j.doe@rubix.com), never the connection's configured shared one
	// (vertrieb@rubix.com, testExchangeGraphConfig's default) — this is
	// the core per-user-override guarantee this feature exists for.
	if !strings.Contains(sawPath, "j.doe@rubix.com") {
		t.Fatalf("want the Graph request scoped to the caller's own mailbox, got path %q", sawPath)
	}
	if strings.Contains(sawPath, "vertrieb@rubix.com") {
		t.Fatalf("must NOT use the connection's fixed shared mailbox, got path %q", sawPath)
	}

	var out mailGraphListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Subject != "Anfrage Lieferzeit" || out.Items[0].From != "Kunde X" {
		t.Fatalf("unexpected items: %+v", out.Items)
	}
}

// TestHandleMailGraphOptionsListsOwnAndSharedMailboxes proves the options
// endpoint surfaces both an "own mailbox" connection and an
// InteractiveShared "team mailbox" connection the caller is authorized for
// — the picker the Mail tab renders instead of assuming one fixed mailbox.
func TestHandleMailGraphOptionsListsOwnAndSharedMailboxes(t *testing.T) {
	personal := testInteractiveExchangeConn("j.doe@rubix.com")
	personal.Name = "personal"
	shared := testInteractiveExchangeConn("j.doe@rubix.com")
	shared.Name = "team"
	shared.InteractiveShared = true
	shared.Mailbox = "test.mechatronics.ki@rubix.com"
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{personal, shared}}
	withTestGlobalSettings(t, s)

	r := httptest.NewRequest(http.MethodGet, "/api/mail/graph/options", nil)
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphOptions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Options []interactiveMailboxOption `json:"options"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Options) != 2 {
		t.Fatalf("want 2 options, got %+v", out.Options)
	}
	if out.Options[1].Label != "test.mechatronics.ki@rubix.com" {
		t.Fatalf("want the shared mailbox's address as its own option's label, got %+v", out.Options)
	}
}

func TestHandleMailGraphOptionsNoSession401(t *testing.T) {
	withTestGlobalSettings(t, appSettings{})
	r := httptest.NewRequest(http.MethodGet, "/api/mail/graph/options", nil)
	w := httptest.NewRecorder()
	handleMailGraphOptions(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

// TestHandleMailGraphListUsesRequestedMailboxKeyAndFolder proves list picks
// the connection matching the request's mailbox_key (not just the first
// authorized option) and, when folder is set, queries that folder instead
// of the connection's configured default.
func TestHandleMailGraphListUsesRequestedMailboxKeyAndFolder(t *testing.T) {
	var sawPath string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[]}`))
	})

	shared := testInteractiveExchangeConn("j.doe@rubix.com")
	shared.Name = "team"
	shared.InteractiveShared = true
	shared.Mailbox = "test.mechatronics.ki@rubix.com"
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{shared}}
	withTestGlobalSettings(t, s)

	claims := sessionClaims{User: "j.doe", Mail: "j.doe@rubix.com"}
	opts := findInteractiveExchangeOptions(s, claims)
	if len(opts) != 1 {
		t.Fatalf("want exactly 1 option, got %d", len(opts))
	}

	body, _ := json.Marshal(map[string]any{"mailbox_key": opts[0].Key, "folder": "drafts"})
	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/list", bytes.NewReader(body))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphList(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(sawPath, "test.mechatronics.ki@rubix.com") {
		t.Fatalf("want the shared mailbox used, got path %q", sawPath)
	}
	if !strings.Contains(sawPath, "/drafts/") {
		t.Fatalf("want the requested folder override used, got path %q", sawPath)
	}
}

// TestHandleMailGraphFolders proves the interactive folders endpoint
// resolves the caller's chosen mailbox the same authorized way as
// list/message/save-draft, then walks its folder tree
// (exchangeDiscoverTree) — the admin-only Settings "Struktur erkunden"
// button's walker, reused here behind the per-caller authorization gate
// instead of the admin-only Discover routes (discover.go).
func TestHandleMailGraphFolders(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/mailFolders/") {
			_, _ = w.Write([]byte(`{"value":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":[
			{"id":"folder-drafts","displayName":"Entwürfe","childFolderCount":0,"totalItemCount":36},
			{"id":"folder-inbox","displayName":"Posteingang","childFolderCount":0,"totalItemCount":36}
		]}`))
	})

	s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{})
	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/folders", bytes.NewReader(body))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphFolders(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var node discoverNode
	if err := json.Unmarshal(w.Body.Bytes(), &node); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(node.Children) != 2 {
		t.Fatalf("want 2 top-level folders, got %+v", node.Children)
	}
}

func TestHandleMailGraphFoldersRequiresAuthorization(t *testing.T) {
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
	withTestGlobalSettings(t, s)

	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/folders", bytes.NewReader([]byte(`{}`)))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "someone.else", Mail: "someone.else@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphFolders(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestHandleMailGraphMessageReturnsRawEmailForDraftReuse(t *testing.T) {
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"subject":"Anfrage Lieferzeit",
			"receivedDateTime":"2026-07-10T09:30:00Z",
			"from":{"emailAddress":{"name":"Kunde X","address":"kunde@example.com"}},
			"toRecipients":[{"emailAddress":{"name":"J. Doe","address":"j.doe@rubix.com"}}],
			"body":{"contentType":"text","content":"Wann kommt meine Bestellung an?"}
		}`))
	})

	s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(mailGraphMessageRequest{ID: "msg-1"})
	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/message", bytes.NewReader(body))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphMessage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out mailGraphMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Subject != "Anfrage Lieferzeit" || !strings.Contains(out.Body, "Wann kommt meine Bestellung an?") {
		t.Fatalf("unexpected message fields: %+v", out)
	}
	// RawEmail must be usable as-is by /api/draft/reply's raw_email field —
	// parseRawEmail (handlers.go) treats it as an opaque body, so the
	// presence of the key facts (subject/from/body) is what matters, not
	// any particular parsed structure.
	if !strings.Contains(out.RawEmail, "Anfrage Lieferzeit") || !strings.Contains(out.RawEmail, "Wann kommt meine Bestellung an?") {
		t.Fatalf("want RawEmail to carry subject+body for reuse as raw_email, got %q", out.RawEmail)
	}
}

// TestHandleMailGraphMessageIncludesAttachments closes the gap the live
// Exchange mailbox reader used to have: a message with hasAttachments=true
// previously surfaced no trace of its attachment(s) at all, not even a
// filename — now the attachment listing is fetched, its content extracted
// (extractAttachmentText, ingest.go) and both structured (Attachments) and
// folded into RawEmail so a drafted reply can reference it deterministically.
func TestHandleMailGraphMessageIncludesAttachments(t *testing.T) {
	attData := base64.StdEncoding.EncodeToString([]byte("Rechnungsbetrag: 1234,56 EUR"))
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/attachments"):
			_, _ = w.Write([]byte(`{"value": [
				{"@odata.type": "#microsoft.graph.fileAttachment", "name": "rechnung.txt", "contentBytes": "` + attData + `", "size": 28}
			]}`))
		default:
			_, _ = w.Write([]byte(`{
				"subject":"Rechnung anbei",
				"receivedDateTime":"2026-07-10T09:30:00Z",
				"hasAttachments": true,
				"from":{"emailAddress":{"name":"Kunde X","address":"kunde@example.com"}},
				"toRecipients":[{"emailAddress":{"name":"J. Doe","address":"j.doe@rubix.com"}}],
				"body":{"contentType":"text","content":"Siehe Anhang."}
			}`))
		}
	})

	s := appSettings{ExchangeGraph: []exchangeGraphConfig{testInteractiveExchangeConn("j.doe@rubix.com")}}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(mailGraphMessageRequest{ID: "msg-2"})
	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/message", bytes.NewReader(body))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphMessage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out mailGraphMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d: %+v", len(out.Attachments), out.Attachments)
	}
	att := out.Attachments[0]
	if att.Filename != "rechnung.txt" {
		t.Fatalf("want filename rechnung.txt, got %q", att.Filename)
	}
	if att.Error != "" {
		t.Fatalf("want no extraction error, got %q", att.Error)
	}
	if !strings.Contains(att.Text, "1234,56 EUR") {
		t.Fatalf("want extracted attachment text, got %q", att.Text)
	}
	if !strings.Contains(out.RawEmail, "1234,56 EUR") {
		t.Fatalf("want attachment content folded into RawEmail for draft generation, got %q", out.RawEmail)
	}
}

func TestHandleMailGraphSaveDraftRequiresEnableDraftReplies(t *testing.T) {
	called := false
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	// InteractiveEnabled+AllowedUsers authorize the user, but
	// EnableDraftReplies (the connection's own write opt-in) is left off —
	// createExchangeGraphDraft must still refuse, exactly as it does for
	// the import/auto-draft path.
	cfg := testInteractiveExchangeConn("j.doe@rubix.com")
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{cfg}}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(mailGraphSaveDraftRequest{OriginalMessageID: "msg-1", Body: "Vielen Dank fuer Ihre Anfrage."})
	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/save-draft", bytes.NewReader(body))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphSaveDraft(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("want a non-200 error when EnableDraftReplies is off, got 200: %s", w.Body.String())
	}
	if called {
		t.Fatal("want no Graph call at all when EnableDraftReplies is off")
	}
}

func TestHandleMailGraphSaveDraftSavesToCallersOwnMailbox(t *testing.T) {
	var sawPaths []string
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawPaths = append(sawPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/createReply"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "draft-xyz"}`))
		case strings.Contains(r.URL.Path, "/messages/draft-xyz"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id": "draft-xyz"}`))
		default:
			t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
	})

	cfg := testInteractiveExchangeConn("j.doe@rubix.com")
	cfg.EnableDraftReplies = true
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{cfg}}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(mailGraphSaveDraftRequest{OriginalMessageID: "msg-1", Body: "Vielen Dank fuer Ihre Anfrage."})
	r := httptest.NewRequest(http.MethodPost, "/api/mail/graph/save-draft", bytes.NewReader(body))
	for _, c := range sessionCookiesFor(t, &ldapUser{CN: "j.doe", Mail: "j.doe@rubix.com"}) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handleMailGraphSaveDraft(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out mailGraphSaveDraftResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.DraftID != "draft-xyz" {
		t.Fatalf("want the created draft id, got %+v", out)
	}
	found := false
	for _, p := range sawPaths {
		if strings.Contains(p, "j.doe@rubix.com") {
			found = true
		}
		if strings.Contains(p, "vertrieb@rubix.com") {
			t.Fatalf("must NOT write to the connection's fixed shared mailbox, got path %q", p)
		}
	}
	if !found {
		t.Fatalf("want at least one call scoped to the caller's own mailbox, got %v", sawPaths)
	}
}
