package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Covers accessControl.allows (settings.go) — the generalized per-connector
// user/AD-group allow-list for MSSQL/Shop/REST connectors — and its wiring
// into buildLiveTools/appendShopTool, plus exchangeGraphConfig.AllowedGroups
// (mail_graph.go's findInteractiveExchangeConn), Phase 4 Part B.

func TestAccessControlAllowsEmptyIsUnrestricted(t *testing.T) {
	var ac accessControl
	if !ac.allows("anyone@rubix.com", nil) {
		t.Fatal("want an entirely empty accessControl to allow everyone (backward-compat default)")
	}
	if !ac.allows("", nil) {
		t.Fatal("want an entirely empty accessControl to allow even an anonymous caller")
	}
}

func TestAccessControlAllowsByUser(t *testing.T) {
	ac := accessControl{AllowedUsers: []string{"alice@rubix.com"}}
	if !ac.allows("Alice@Rubix.com", nil) {
		t.Error("want a case-insensitive match on AllowedUsers to allow")
	}
	if ac.allows("bob@rubix.com", nil) {
		t.Error("want a non-listed user to be denied once AllowedUsers is non-empty")
	}
}

func TestAccessControlAllowsByGroup(t *testing.T) {
	ac := accessControl{AllowedGroups: []string{"CN=SQL-Users,OU=Groups,DC=rubix,DC=com"}}
	if !ac.allows("bob@rubix.com", []string{"CN=SQL-Users,OU=Groups,DC=rubix,DC=com"}) {
		t.Error("want a group match to allow")
	}
	if ac.allows("bob@rubix.com", []string{"CN=Other,OU=Groups,DC=rubix,DC=com"}) {
		t.Error("want no group match to deny once AllowedGroups is non-empty")
	}
	if ac.allows("bob@rubix.com", nil) {
		t.Error("want no groups at all to deny once AllowedGroups is non-empty")
	}
}

func TestAccessControlAllowsEitherUserOrGroupMatches(t *testing.T) {
	ac := accessControl{
		AllowedUsers:  []string{"alice@rubix.com"},
		AllowedGroups: []string{"CN=SQL-Users,OU=Groups,DC=rubix,DC=com"},
	}
	if !ac.allows("bob@rubix.com", []string{"CN=SQL-Users,OU=Groups,DC=rubix,DC=com"}) {
		t.Error("want group-only match to still allow when AllowedUsers doesn't match")
	}
	if !ac.allows("alice@rubix.com", nil) {
		t.Error("want user-only match to still allow when AllowedGroups doesn't match")
	}
	if ac.allows("carol@rubix.com", []string{"CN=Other,OU=Groups,DC=rubix,DC=com"}) {
		t.Error("want neither matching to deny")
	}
}

// TestBuildLiveToolsMSSQLRespectsAccessControl proves buildLiveTools narrows
// MSSQL access via s.MSSQL.AccessControl on top of the pre-existing
// mssqlAllowed tier gate — an admin who never configures AccessControl sees
// the tool exactly as before (unrestricted), one who does sees it withheld
// from a non-matching caller even though mssqlAllowed=true.
func TestBuildLiveToolsMSSQLRespectsAccessControl(t *testing.T) {
	s := appSettings{MSSQL: mssqlConfig{
		Enabled:           true,
		AllowGenericQuery: true,
		AccessControl:     accessControl{AllowedUsers: []string{"alice@rubix.com"}},
	}}
	preset := sourcePreset{Tools: nil} // nil/empty Tools = no restriction, see presetAllowsTool

	tools, _ := buildLiveTools(s, agentSession{User: "bob@rubix.com"}, preset, true)
	if len(tools) != 0 {
		t.Fatalf("want MSSQL tool withheld from a non-allow-listed user even with mssqlAllowed=true, got %d tools", len(tools))
	}

	tools, _ = buildLiveTools(s, agentSession{User: "alice@rubix.com"}, preset, true)
	if len(tools) == 0 {
		t.Fatal("want MSSQL tool offered to an allow-listed user")
	}

	tools, _ = buildLiveTools(s, agentSession{User: "bob@rubix.com"}, preset, false)
	if len(tools) != 0 {
		t.Fatal("want AccessControl never able to grant access when the coarser mssqlAllowed gate itself denies")
	}
}

// TestBuildLiveToolsShopRespectsAccessControl mirrors the MSSQL case above
// for Shop via appendShopTool's new user/groups parameters.
func TestBuildLiveToolsShopRespectsAccessControl(t *testing.T) {
	s := appSettings{Shop: shopConfig{
		Enabled:       true,
		AccessControl: accessControl{AllowedGroups: []string{"CN=Shop-Users,DC=rubix,DC=com"}},
	}}
	preset := sourcePreset{}

	tools, _ := buildLiveTools(s, agentSession{User: "bob@rubix.com"}, preset, true)
	if len(tools) != 0 {
		t.Fatalf("want Shop tools withheld from a caller in no allowed group, got %d", len(tools))
	}

	tools, _ = buildLiveTools(s, agentSession{User: "bob@rubix.com", Groups: []string{"CN=Shop-Users,DC=rubix,DC=com"}}, preset, true)
	if len(tools) != 3 {
		t.Fatalf("want all three Shop tools offered to a member of the allowed group, got %d", len(tools))
	}
}

// TestBuildLiveToolsHTTPTemplateRespectsConnectorAccessControl proves an
// HTTP query template borrowing a REST connector (auth_source) inherits
// that connector's AccessControl — a template using a built-in auth source
// with no connector (AuthSource "none") is unaffected.
func TestBuildLiveToolsHTTPTemplateRespectsConnectorAccessControl(t *testing.T) {
	s := appSettings{
		RESTConnectors: []restConnectorConfig{
			{connRuntime: connRuntime{Name: "sap"}, Enabled: true, BaseURL: "https://sap.example.com",
				AccessControl: accessControl{AllowedUsers: []string{"alice@rubix.com"}}},
		},
		HTTPTemplates: []httpQueryTemplate{
			{Name: "get_material", Enabled: true, AuthSource: "sap", URLTemplate: "https://sap.example.com/mara/{id}",
				Parameters: []sqlQueryParam{{Name: "id", Type: "string"}}},
			{Name: "get_public_thing", Enabled: true, AuthSource: "none", URLTemplate: "https://sap.example.com/public/{id}",
				Parameters: []sqlQueryParam{{Name: "id", Type: "string"}}},
		},
	}
	preset := sourcePreset{}

	_, execs := buildLiveTools(s, agentSession{User: "bob@rubix.com"}, preset, true)
	if _, ok := execs["get_material"]; ok {
		t.Error("want get_material withheld from a caller not in the sap connector's AccessControl")
	}
	if _, ok := execs["get_public_thing"]; !ok {
		t.Error("want get_public_thing (auth_source=none, no connector) unaffected by the sap connector's AccessControl")
	}

	_, execs = buildLiveTools(s, agentSession{User: "alice@rubix.com"}, preset, true)
	if _, ok := execs["get_material"]; !ok {
		t.Error("want get_material offered to a caller allow-listed on the sap connector")
	}
}

// TestFindInteractiveExchangeConnAllowedGroups proves the Phase 3
// AllowedUsers-only gate now also honors AllowedGroups (AD memberOf), per
// mail_graph.go's findInteractiveExchangeOptions doc comment.
func TestFindInteractiveExchangeConnAllowedGroups(t *testing.T) {
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{
		{connRuntime: connRuntime{Name: "main"}, Enabled: true, InteractiveEnabled: true,
			AllowedGroups: []string{"CN=Mail-Users,DC=rubix,DC=com"}},
	}}

	claims := sessionClaims{Mail: "bob@rubix.com", User: "bob"}
	if _, ok := resolveInteractiveExchangeOption(s, claims, ""); ok {
		t.Fatal("want no match for a user in neither AllowedUsers nor AllowedGroups")
	}

	claims.Groups = []string{"CN=Mail-Users,DC=rubix,DC=com"}
	conn, ok := resolveInteractiveExchangeOption(s, claims, "")
	if !ok {
		t.Fatal("want a match once the caller's AD group is in AllowedGroups")
	}
	if conn.Mailbox != "bob@rubix.com" {
		t.Errorf("want Mailbox overridden to the caller's own address, got %q", conn.Mailbox)
	}
}

// TestFindInteractiveExchangeOptionsSharedMailbox proves an
// InteractiveShared connection contributes the shared mailbox as-is
// (never overridden to the caller's own address), distinct from and
// selectable alongside an "own mailbox" connection.
func TestFindInteractiveExchangeOptionsSharedMailbox(t *testing.T) {
	s := appSettings{ExchangeGraph: []exchangeGraphConfig{
		{connRuntime: connRuntime{Name: "personal"}, Enabled: true, InteractiveEnabled: true,
			AllowedUsers: []string{"bob@rubix.com"}},
		{connRuntime: connRuntime{Name: "team"}, Enabled: true, InteractiveEnabled: true, InteractiveShared: true,
			Mailbox: "test.mechatronics.ki@rubix.com", AllowedUsers: []string{"bob@rubix.com"}},
	}}
	claims := sessionClaims{Mail: "bob@rubix.com", User: "bob"}

	opts := findInteractiveExchangeOptions(s, claims)
	if len(opts) != 2 {
		t.Fatalf("want 2 options (own + shared), got %d", len(opts))
	}
	if opts[0].Conn.Mailbox != "bob@rubix.com" {
		t.Errorf("want the first (own-mailbox) option's Mailbox overridden to the caller, got %q", opts[0].Conn.Mailbox)
	}
	if opts[1].Conn.Mailbox != "test.mechatronics.ki@rubix.com" {
		t.Errorf("want the second (shared) option's Mailbox used as configured, got %q", opts[1].Conn.Mailbox)
	}
	if opts[1].Label != "test.mechatronics.ki@rubix.com" {
		t.Errorf("want the shared option's Label to be the mailbox address itself, got %q", opts[1].Label)
	}

	// A caller not authorized on the shared connection only ever sees the
	// personal one.
	carol := sessionClaims{Mail: "carol@rubix.com", User: "carol"}
	carolOpts := findInteractiveExchangeOptions(s, carol)
	if len(carolOpts) != 0 {
		t.Fatalf("want 0 options for an unauthorized caller, got %d", len(carolOpts))
	}

	conn, ok := resolveInteractiveExchangeOption(s, claims, opts[1].Key)
	if !ok || conn.Mailbox != "test.mechatronics.ki@rubix.com" {
		t.Fatalf("want resolveInteractiveExchangeOption to re-derive the shared option by key, got conn=%v ok=%v", conn, ok)
	}
	if _, ok := resolveInteractiveExchangeOption(s, carol, opts[1].Key); ok {
		t.Fatal("want a key belonging to bob's options to never resolve for a differently-authorized caller")
	}
}

// TestSessionClaimsCarryGroups proves issueSession persists ldapUser.MemberOf
// into sessionClaims.Groups (session.go) — the plumbing every accessControl/
// AllowedGroups check above depends on at runtime.
func TestSessionClaimsCarryGroups(t *testing.T) {
	groups := []string{"CN=A,DC=rubix,DC=com", "CN=B,DC=rubix,DC=com"}
	w := httptest.NewRecorder()
	issueSession(w, &ldapUser{CN: "bob", MemberOf: groups})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	claims, ok := currentSession(req)
	if !ok {
		t.Fatal("want a valid session")
	}
	if len(claims.Groups) != 2 || claims.Groups[0] != groups[0] || claims.Groups[1] != groups[1] {
		t.Errorf("want claims.Groups = %v, got %v", groups, claims.Groups)
	}
}
