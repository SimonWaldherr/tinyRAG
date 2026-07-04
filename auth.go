package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	bcrypt "golang.org/x/crypto/bcrypt"
)

type adminAPIUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	APIKeyHash  string `json:"api_key_hash,omitempty"`
	APIKeyLast4 string `json:"api_key_last4,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Web-UI authentication types
// ─────────────────────────────────────────────────────────────────────────────

// webUIUser represents a local (non-API) user who can log into the web UI.
type webUIUser struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash,omitempty"` // bcrypt hash
	Role         string `json:"role"`                    // "admin" or "viewer"
	Enabled      bool   `json:"enabled"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// webUISession holds information about an active browser session.
type webUISession struct {
	Token     string
	UserID    string
	Username  string
	Role      string
	ExpiresAt time.Time
}

// sessionStore is an in-memory store of active web-UI sessions keyed by token.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*webUISession
}

func newSessionStore() *sessionStore {
	s := &sessionStore{sessions: make(map[string]*webUISession)}
	go s.sweepLoop()
	return s
}

func (ss *sessionStore) create(userID, username, role string, ttl time.Duration) *webUISession {
	token := generateSessionToken()
	sess := &webUISession{
		Token:     token,
		UserID:    userID,
		Username:  username,
		Role:      role,
		ExpiresAt: time.Now().Add(ttl),
	}
	ss.mu.Lock()
	ss.sessions[token] = sess
	ss.mu.Unlock()
	return sess
}

func (ss *sessionStore) get(token string) (*webUISession, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.sessions[token]
	if !ok || time.Now().After(s.ExpiresAt) {
		delete(ss.sessions, token)
		return nil, false
	}
	return s, true
}

func (ss *sessionStore) delete(token string) {
	ss.mu.Lock()
	delete(ss.sessions, token)
	ss.mu.Unlock()
}

// sweepLoop periodically removes expired sessions.
func (ss *sessionStore) sweepLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ss.mu.Lock()
		now := time.Now()
		for tok, s := range ss.sessions {
			if now.After(s.ExpiresAt) {
				delete(ss.sessions, tok)
			}
		}
		ss.mu.Unlock()
	}
}

// generateSessionToken returns a random 32-byte hex token.
func generateSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("rand.Read: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// hashWebUIPassword returns a bcrypt hash of the password.
func hashWebUIPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// verifyWebUIPassword checks a plaintext password against a stored bcrypt hash.
func verifyWebUIPassword(user webUIUser, password string) bool {
	if user.PasswordHash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) == nil
}

// ldapAuthenticate attempts to authenticate username+password against LDAP.
// Returns the user's inferred role ("admin"/"viewer") and nil error on success.
func ldapAuthenticate(s appSettings, username, password string) (string, error) {
	if !s.LDAPEnabled || s.LDAPServer == "" {
		return "", fmt.Errorf("LDAP not configured")
	}
	addr := fmt.Sprintf("%s:%d", s.LDAPServer, s.LDAPPort)
	if s.LDAPPort == 0 {
		if s.LDAPUseTLS {
			addr = s.LDAPServer + ":636"
		} else {
			addr = s.LDAPServer + ":389"
		}
	}
	var conn *ldap.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: s.LDAPServer} //nolint:gosec // user-configured server; InsecureSkipVerify intentionally left false
	if s.LDAPUseTLS {
		conn, err = ldap.DialTLS("tcp", addr, tlsCfg)
	} else {
		conn, err = ldap.DialURL("ldap://" + addr)
	}
	if err != nil {
		return "", fmt.Errorf("LDAP dial: %w", err)
	}
	defer conn.Close()
	if s.LDAPStartTLS && !s.LDAPUseTLS {
		if err = conn.StartTLS(tlsCfg); err != nil {
			return "", fmt.Errorf("LDAP StartTLS: %w", err)
		}
	}
	// Bind with service account to search for user DN
	if s.LDAPBindDN != "" {
		if err = conn.Bind(s.LDAPBindDN, s.LDAPBindPass); err != nil {
			return "", fmt.Errorf("LDAP service bind: %w", err)
		}
	}
	userAttr := s.LDAPUserAttr
	if userAttr == "" {
		userAttr = "uid"
	}
	filterStr := fmt.Sprintf("(%s=%s)", userAttr, ldap.EscapeFilter(username))
	if s.LDAPFilter != "" {
		filterStr = fmt.Sprintf("(&%s%s)", filterStr, s.LDAPFilter)
	}
	searchReq := ldap.NewSearchRequest(
		s.LDAPBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		2, 10, false, filterStr,
		[]string{"dn", "memberOf"},
		nil,
	)
	res, err := conn.Search(searchReq)
	if err != nil {
		return "", fmt.Errorf("LDAP search: %w", err)
	}
	if len(res.Entries) == 0 {
		return "", fmt.Errorf("LDAP user not found")
	}
	userDN := res.Entries[0].DN
	// Bind as the found user to verify password
	if err = conn.Bind(userDN, password); err != nil {
		return "", fmt.Errorf("LDAP bind as user: %w", err)
	}
	// Determine role from group membership
	role := "viewer"
	if s.LDAPAdminGroup != "" {
		for _, attr := range res.Entries[0].GetAttributeValues("memberOf") {
			if strings.EqualFold(attr, s.LDAPAdminGroup) {
				role = "admin"
				break
			}
		}
	}
	return role, nil
}

type apiRouteRule struct {
	Path        string `json:"path"`
	MatchType   string `json:"match_type"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Public      bool   `json:"public"`
}

func defaultAPIRouteRules() []apiRouteRule {
	return []apiRouteRule{
		{Path: "/api/process", MatchType: "exact", Description: "Structured processing endpoint", Enabled: true, Public: true},
		{Path: "/api/ask", MatchType: "exact", Description: "Chat SSE endpoint", Enabled: true, Public: true},
		{Path: "/api/vision", MatchType: "exact", Description: "Multimodal vision chat (SSE)", Enabled: true, Public: true},
		{Path: "/api/stt", MatchType: "exact", Description: "Speech-to-text (Whisper proxy)", Enabled: true, Public: true},
		{Path: "/api/tts", MatchType: "exact", Description: "Text-to-speech proxy", Enabled: true, Public: true},
		{Path: "/api/search", MatchType: "exact", Description: "Semantic search", Enabled: true, Public: true},
		{Path: "/api/tool/execute", MatchType: "exact", Description: "Execute one tool manually", Enabled: true, Public: true},
		{Path: "/api/add-wiki", MatchType: "exact", Description: "Ingest from Wikipedia", Enabled: true, Public: true},
		{Path: "/api/add-url", MatchType: "exact", Description: "Ingest from URL", Enabled: true, Public: true},
		{Path: "/api/add-folder", MatchType: "exact", Description: "Bulk ingest from folder", Enabled: true, Public: true},
		{Path: "/api/add-text", MatchType: "exact", Description: "Ingest plain text", Enabled: true, Public: true},
		{Path: "/api/upload", MatchType: "exact", Description: "File upload ingest", Enabled: true, Public: true},
		{Path: "/api/modules/run", MatchType: "exact", Description: "Run configured module", Enabled: true, Public: true},
		{Path: "/api/modules/upload", MatchType: "exact", Description: "Module upload bridge", Enabled: true, Public: true},
		{Path: "/api/modules/download", MatchType: "exact", Description: "Module download bridge", Enabled: true, Public: true},
	}
}

func normalizeMatchType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "prefix":
		return "prefix"
	default:
		return "exact"
	}
}

func normalizeAPIRouteRules(rules []apiRouteRule) []apiRouteRule {
	if len(rules) == 0 {
		return defaultAPIRouteRules()
	}
	defaults := defaultAPIRouteRules()
	byPath := make(map[string]apiRouteRule, len(rules))
	for _, rule := range rules {
		rule.Path = strings.TrimSpace(rule.Path)
		if rule.Path == "" {
			continue
		}
		rule.MatchType = normalizeMatchType(rule.MatchType)
		byPath[rule.Path] = rule
	}
	out := make([]apiRouteRule, 0, len(defaults))
	for _, def := range defaults {
		rule, ok := byPath[def.Path]
		if !ok {
			out = append(out, def)
			continue
		}
		if rule.Description == "" {
			rule.Description = def.Description
		}
		if rule.MatchType == "" {
			rule.MatchType = def.MatchType
		}
		out = append(out, rule)
		delete(byPath, def.Path)
	}
	for _, rule := range byPath {
		out = append(out, rule)
	}
	return out
}

func generateAPIToken() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func apiTokenFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func findAPIRouteRule(path string, rules []apiRouteRule) (apiRouteRule, bool) {
	var matched apiRouteRule
	found := false
	longest := -1
	for _, rule := range rules {
		switch normalizeMatchType(rule.MatchType) {
		case "prefix":
			if strings.HasPrefix(path, rule.Path) && len(rule.Path) > longest {
				matched = rule
				found = true
				longest = len(rule.Path)
			}
		default:
			if path == rule.Path {
				return rule, true
			}
		}
	}
	return matched, found
}

func authenticateAPIUser(r *http.Request, users []adminAPIUser) (adminAPIUser, bool) {
	token := apiTokenFromRequest(r)
	if token == "" {
		return adminAPIUser{}, false
	}
	tokenHash := hashAPIToken(token)
	for _, user := range users {
		if !user.Enabled || user.APIKeyHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(user.APIKeyHash), []byte(tokenHash)) == 1 {
			return user, true
		}
	}
	return adminAPIUser{}, false
}

func routePolicyMiddleware(settings *settingsStore, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		rule, ok := findAPIRouteRule(r.URL.Path, s.APIRoutes)
		if !ok {
			next(w, r)
			return
		}
		if !rule.Enabled {
			http.Error(w, "API route disabled by admin policy", 403)
			return
		}
		if rule.Public {
			next(w, r)
			return
		}
		if _, ok := authenticateAPIUser(r, s.APIUsers); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tinyRAG API"`)
			http.Error(w, "valid API key required", 401)
			return
		}
		next(w, r)
	}
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localAdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			http.Error(w, "admin endpoints are only available from localhost", 403)
			return
		}
		next(w, r)
	}
}

// sessionFromRequest extracts the active session from the request's
// "session_token" cookie. Returns nil when not found or expired.
func sessionFromRequest(r *http.Request) *webUISession {
	c, err := r.Cookie("session_token")
	if err != nil || c.Value == "" {
		return nil
	}
	sess, ok := sessions.get(c.Value)
	if !ok {
		return nil
	}
	return sess
}

// isHTTPS reports whether the request was made over a secure (TLS) connection,
// either directly or via a trusted reverse proxy that sets X-Forwarded-Proto.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// newSessionCookie builds a "session_token" cookie with the Secure flag set
// only when the request arrived over HTTPS.
func newSessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     "session_token",
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	}
}

// webUIAuthMiddleware wraps a handler so that it requires a valid web-UI
// session when WebUIAuth is enabled. API requests carrying a Bearer token
// bypass the check. If auth is disabled the handler is called directly.
func webUIAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		if !s.WebUIAuth {
			next(w, r)
			return
		}
		// Allow only VALID API-key authenticated requests through
		if _, ok := authenticateAPIUser(r, s.APIUsers); ok {
			next(w, r)
			return
		}
		if sess := sessionFromRequest(r); sess != nil {
			next(w, r)
			return
		}
		// For non-API paths redirect to login; for API paths return 401
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "login required", 401)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// requireAdminSession checks that the caller has an active admin session
// (role == "admin") when WebUIAuth is enabled. Used for admin-only UI operations.
func requireAdminSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		if !s.WebUIAuth {
			next(w, r)
			return
		}
		sess := sessionFromRequest(r)
		if sess == nil {
			http.Error(w, "login required", 401)
			return
		}
		if sess.Role != "admin" {
			http.Error(w, "admin access required", 403)
			return
		}
		next(w, r)
	}
}
