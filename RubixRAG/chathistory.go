package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Server-side, per-user chat history — the login-gated counterpart to the
// older browser-local-only history (web/app.js's HISTORY_KEY/localStorage
// functions, still used unchanged when settings.LDAP.Enabled is off or
// the caller isn't logged in — see settings.go's EnableChatHistory doc
// comment for the full split).
//
// Every method below takes an explicit owner argument (always the
// session's AD username from session.go's currentSession, injected by
// requireSession — never a client-supplied value) and enforces it in the
// SQL itself, not just in application code: one person's conversations
// are never returned, renamed or deleted by another person's session,
// even if they guess/reuse a conversation ID.
//
// A dedicated SQLite file, independent of whichever vectorStore backend
// stores chunk vectors (unrelated data, no reason to couple them) —
// modernc.org/sqlite is already a dependency via vectorstore_sqlite.go,
// so this adds no new one.
// ─────────────────────────────────────────────────────────────────────────────

// chatHistory is the process-wide store, opened once in main() from
// settings.ChatHistoryPath — nil only in tests that don't call
// newChatHistoryStore.
var chatHistory *chatHistoryStore

type chatHistoryStore struct {
	db *sql.DB
}

// newChatHistoryStore opens (creating if needed) the SQLite file at path
// and ensures its schema exists.
func newChatHistoryStore(path string) (*chatHistoryStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("chat history: open %s: %w", path, err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversations (
			id TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			title TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			messages TEXT NOT NULL,
			mode TEXT NOT NULL DEFAULT 'chat'
		);
		CREATE INDEX IF NOT EXISTS idx_conversations_owner ON conversations(owner, updated_at DESC);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("chat history: schema: %w", err)
	}
	// mode ("chat" | "agent" — which tab a conversation came from) is
	// already in the CREATE TABLE text above for a fresh install, but this
	// file has no migrations system (see the package doc comment), so an
	// r3-chathistory.db from before this column existed needs it added via
	// ALTER TABLE — CREATE TABLE IF NOT EXISTS is a no-op once the table
	// is already there, column and all. SQLite has no "ADD COLUMN IF NOT
	// EXISTS", so this always attempts the ALTER and swallows exactly the
	// "duplicate column name" error it returns on a database that already
	// has it (every fresh install, and every restart after the first).
	if _, err := db.Exec(`ALTER TABLE conversations ADD COLUMN mode TEXT NOT NULL DEFAULT 'chat'`); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("chat history: add mode column: %w", err)
		}
	}
	return &chatHistoryStore{db: db}, nil
}

// close releases the underlying database handle — mainly for tests
// (Windows can't delete a still-open SQLite file, which t.TempDir's
// cleanup needs to do); the running server never calls this, matching
// how the vectorStore/settings files are also never explicitly closed
// before process exit.
func (s *chatHistoryStore) close() error { return s.db.Close() }

// chatHistoryMessage mirrors the shape web/app.js already keeps per
// message in its localStorage conversations, so the JSON round-trips
// unchanged regardless of which storage backs a given session's history.
type chatHistoryMessage struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Citations []sourceInfo `json:"citations,omitempty"`
}

// chatConversationMeta is one row's list-view shape (no message bodies)
// — used for GET /api/chat/conversations, kept light since one person
// could have many conversations and the history drawer only needs
// title/recency/mode to render its list.
type chatConversationMeta struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updated_at"`
	// Mode is "chat" or "agent" — which tab produced this conversation,
	// so the client can show a badge and reopen it in the matching tab
	// instead of always assuming Chat. A row saved before this field
	// existed reads back as "" here; normalizeConversationMode below is
	// the single place that turns that into "chat".
	Mode string `json:"mode"`
}

// chatConversation is the full shape (metadata + messages) — used for a
// single conversation's GET/save.
type chatConversation struct {
	ID        string               `json:"id"`
	Title     string               `json:"title"`
	UpdatedAt int64                `json:"updated_at"`
	Mode      string               `json:"mode"`
	Messages  []chatHistoryMessage `json:"messages"`
}

// normalizeConversationMode defaults an empty/unrecognized mode to
// "chat" — both for rows saved before the mode column existed (where the
// SQL-level DEFAULT 'chat' already handles it) and defensively for any
// value that isn't one of the two known modes, so a client bug or a
// hand-edited row can't produce a mode the frontend has no icon/routing
// for.
func normalizeConversationMode(mode string) string {
	if mode == "agent" {
		return "agent"
	}
	return "chat"
}

// list returns owner's conversations, most recently updated first.
func (s *chatHistoryStore) list(owner string) ([]chatConversationMeta, error) {
	rows, err := s.db.Query(`SELECT id, title, updated_at, mode FROM conversations WHERE owner = ? ORDER BY updated_at DESC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []chatConversationMeta{}
	for rows.Next() {
		var m chatConversationMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.UpdatedAt, &m.Mode); err != nil {
			return nil, err
		}
		m.Mode = normalizeConversationMode(m.Mode)
		out = append(out, m)
	}
	return out, rows.Err()
}

// get returns id's full conversation, but only if it belongs to owner —
// ok=false both when the id doesn't exist at all and when it exists but
// belongs to someone else, so a caller can never distinguish "doesn't
// exist" from "not yours" (avoids confirming that some other user's
// conversation ID is even valid).
func (s *chatHistoryStore) get(owner, id string) (conv chatConversation, ok bool, err error) {
	var rowOwner, messagesJSON string
	err = s.db.QueryRow(`SELECT owner, title, updated_at, mode, messages FROM conversations WHERE id = ?`, id).
		Scan(&rowOwner, &conv.Title, &conv.UpdatedAt, &conv.Mode, &messagesJSON)
	if err == sql.ErrNoRows {
		return chatConversation{}, false, nil
	}
	if err != nil {
		return chatConversation{}, false, err
	}
	if rowOwner != owner {
		return chatConversation{}, false, nil
	}
	if err := json.Unmarshal([]byte(messagesJSON), &conv.Messages); err != nil {
		return chatConversation{}, false, fmt.Errorf("chat history: decode messages: %w", err)
	}
	conv.ID = id
	conv.Mode = normalizeConversationMode(conv.Mode)
	return conv, true, nil
}

// save upserts a conversation for owner, setting UpdatedAt (and
// CreatedAt, for a new row) from the server clock — never trusting a
// client-supplied timestamp. If id already exists but belongs to a
// different owner, this fails closed (ok=false) rather than overwriting
// — a client can never hijack another user's conversation ID, even by
// guessing or deliberately reusing one. mode is normalized here too, so a
// malformed/missing value from the client can't get stored verbatim.
func (s *chatHistoryStore) save(owner, id, title, mode string, messages []chatHistoryMessage) (ok bool, err error) {
	if messages == nil {
		messages = []chatHistoryMessage{}
	}
	mode = normalizeConversationMode(mode)
	messagesJSON, err := json.Marshal(messages)
	if err != nil {
		return false, err
	}
	now := time.Now().UnixMilli()

	var existingOwner string
	err = s.db.QueryRow(`SELECT owner FROM conversations WHERE id = ?`, id).Scan(&existingOwner)
	switch {
	case err == sql.ErrNoRows:
		_, err = s.db.Exec(`INSERT INTO conversations (id, owner, title, created_at, updated_at, mode, messages) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, owner, title, now, now, mode, string(messagesJSON))
		return err == nil, err
	case err != nil:
		return false, err
	case existingOwner != owner:
		return false, nil
	default:
		// mode isn't updated here — it's fixed at creation (which tab
		// started this conversation), unlike title, which the client
		// legitimately re-sends on every save as its own text changes.
		_, err = s.db.Exec(`UPDATE conversations SET title = ?, updated_at = ?, messages = ? WHERE id = ? AND owner = ?`,
			title, now, string(messagesJSON), id, owner)
		return err == nil, err
	}
}

// rename updates only a conversation's title, leaving messages/
// updated_at untouched — the history modal's rename action never has
// (and shouldn't need) the full message list on hand just to change a
// title.
func (s *chatHistoryStore) rename(owner, id, title string) (ok bool, err error) {
	res, err := s.db.Exec(`UPDATE conversations SET title = ? WHERE id = ? AND owner = ?`, title, id, owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// delete removes one conversation, only if it belongs to owner — a
// delete for someone else's id (or a nonexistent one) is simply a no-op,
// not an error.
func (s *chatHistoryStore) delete(owner, id string) error {
	_, err := s.db.Exec(`DELETE FROM conversations WHERE id = ? AND owner = ?`, id, owner)
	return err
}

// deleteAll removes every conversation belonging to owner — the "Gesamten
// Verlauf löschen" action.
func (s *chatHistoryStore) deleteAll(owner string) error {
	_, err := s.db.Exec(`DELETE FROM conversations WHERE owner = ?`, owner)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP handlers. Registered in handlers.go's registerRoutes, all wrapped
// in requireSession below — every one of these needs a real, current
// session before touching the store, and gets the resolved username
// handed to it directly rather than re-deriving it, so a handler can't
// forget to check who's asking.
// ─────────────────────────────────────────────────────────────────────────────

// sessionHandlerFunc is an http.HandlerFunc that already knows who's
// asking — see requireSession.
type sessionHandlerFunc func(w http.ResponseWriter, r *http.Request, user string)

// requireSession is requireAdminSession's non-admin counterpart: it
// demands only a valid, current session (any logged-in account, not
// specifically an admin one) — used for the chat-history routes, which
// need to know *who* is asking but don't require admin rights. Like
// requireAdminSession, this needs no explicit "is LDAP even enabled"
// check: without LDAP, no one can ever have a session, so every route
// wrapped in this is simply unreachable (401) in that mode — the same
// graceful-no-op-without-LDAP shape used throughout (SourceAccess,
// PersonalizeAnswers, ...).
func requireSession(next sessionHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := currentSession(r)
		if !ok {
			writeJSONError(w, "login required", http.StatusUnauthorized)
			return
		}
		next(w, r, claims.User)
	}
}

// handleChatHistoryList serves GET /api/chat/conversations: the calling
// user's own conversations, most recent first, metadata only.
func handleChatHistoryList(w http.ResponseWriter, r *http.Request, user string) {
	list, err := chatHistory.list(user)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, list)
}

// handleChatHistoryGet serves GET /api/chat/conversations/get?id=...:
// one conversation's full messages, only if the caller owns it.
func handleChatHistoryGet(w http.ResponseWriter, r *http.Request, user string) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSONError(w, "missing id", 400)
		return
	}
	conv, ok, err := chatHistory.get(user, id)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if !ok {
		writeJSONError(w, "not found", 404)
		return
	}
	writeJSON(w, conv)
}

type saveConversationRequest struct {
	ID       string               `json:"id"`
	Title    string               `json:"title"`
	Mode     string               `json:"mode"`
	Messages []chatHistoryMessage `json:"messages"`
}

// chatConversationTitleMax bounds a stored title's length — a chat
// question can be arbitrarily long, and the title defaults to (a prefix
// of) the first question, so this keeps the history list's rendering
// predictable regardless of how long that question was.
const chatConversationTitleMax = 200

// handleChatHistorySave serves POST /api/chat/conversations/save: create
// (empty/new id) or update (existing id, must be owned by the caller) a
// conversation.
func handleChatHistorySave(w http.ResponseWriter, r *http.Request, user string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req saveConversationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSONError(w, "invalid body: id is required", 400)
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Unbenannt"
	}
	if len(title) > chatConversationTitleMax {
		title = title[:chatConversationTitleMax]
	}
	ok, err := chatHistory.save(user, req.ID, title, req.Mode, req.Messages)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if !ok {
		// Either this id already belongs to someone else, or the insert/
		// update itself failed — either way, never say which.
		writeJSONError(w, "conversation id not available", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type renameConversationRequest struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// handleChatHistoryRename serves POST /api/chat/conversations/rename.
func handleChatHistoryRename(w http.ResponseWriter, r *http.Request, user string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req renameConversationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSONError(w, "invalid body", 400)
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Unbenannt"
	}
	if len(title) > chatConversationTitleMax {
		title = title[:chatConversationTitleMax]
	}
	ok, err := chatHistory.rename(user, req.ID, title)
	if err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	if !ok {
		writeJSONError(w, "not found", 404)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type deleteConversationRequest struct {
	ID string `json:"id"`
}

// handleChatHistoryDelete serves POST /api/chat/conversations/delete.
func handleChatHistoryDelete(w http.ResponseWriter, r *http.Request, user string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req deleteConversationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSONError(w, "invalid body", 400)
		return
	}
	if err := chatHistory.delete(user, req.ID); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleChatHistoryDeleteAll serves POST /api/chat/conversations/delete-all.
func handleChatHistoryDeleteAll(w http.ResponseWriter, r *http.Request, user string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := chatHistory.deleteAll(user); err != nil {
		writeJSONError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
