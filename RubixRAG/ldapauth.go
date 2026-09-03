package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// ─────────────────────────────────────────────────────────────────────────────
// LDAP/Active Directory login, adapted from ndz/lib/ldap.go's Auth/xAuth
// (the same pattern already used elsewhere for Zitec/Rubix AD logins).
// Changes from that original:
//   - the sAMAccountName search filter is escaped via ldap.EscapeFilter
//     before being interpolated — the original code built the filter with
//     a raw fmt.Sprintf, which is an LDAP filter injection vector for any
//     caller-supplied username (e.g. a username containing "*)(...)").
//   - membership in settings.LDAP.RequiredGroupDN no longer gates whether
//     login succeeds at all — it only decides IsAdmin below. Login is now
//     also how a non-admin employee identifies themselves for
//     department-restricted content (settings.SourceAccess) and answer
//     personalization (settings.PersonalizeAnswers), so a successful AD
//     bind must always produce a session; RequiredGroupDN membership is
//     solely what unlocks the admin UI (see handlers.go's
//     requireAdminSession, which checks IsAdmin, not just "has a session").
//   - Title/Office (AD "title"/"physicalDeliveryOfficeName") and a
//     classified DeptCode (department.go) are fetched/computed here too,
//     so session.go can cache them in the signed cookie without a repeat
//     LDAP round trip on every request.
// ─────────────────────────────────────────────────────────────────────────────

// ldapUser is the subset of AD attributes R3 needs after a successful bind.
type ldapUser struct {
	// CN stays the backwards-compatible owner key for existing chat history
	// and personal preferences. The more precise account/display fields below
	// are deliberately additive, so an upgrade never strands existing data.
	CN                string
	DisplayName       string
	AccountName       string // AD sAMAccountName: stable, short login name
	UserPrincipalName string // canonical UPN; useful when mail is unset
	Mail              string
	Department        string
	Title             string
	Office            string
	Company           string
	// DirectoryID is the base64url objectGUID. It never leaves the server;
	// operations telemetry uses it only to deduplicate sessions for the same
	// person even if their display name, mail address, or CN later changes.
	DirectoryID string
	MemberOf    []string
	// IsAdmin reports whether cfg.RequiredGroupDN membership was
	// satisfied — the sole gate for R3's admin UI/API (requireAdminSession).
	// Any AD account that can bind gets a session regardless of this
	// value; only IsAdmin controls admin access.
	IsAdmin bool
	// DeptCode is classifyDepartment(Department, Title) — see
	// department.go — resolved once here rather than per request.
	DeptCode string
}

// ldapAuthenticate binds as user/password against cfg.URL and, if
// cfg.RequiredGroupDN is set, checks whether the account is a member of
// that group (see ldapUser.IsAdmin). A successful bind alone only proves
// the credentials are valid — see RequiredGroupDN's doc comment in
// settings.go for why that's not the same as "should have R3 admin
// access". deptRulesDir is settings.PromptsDir, passed through to
// department.go's departmentRulesOrDefault so an admin-edited
// department_rules.json is honored without this function needing to know
// where that file lives.
func ldapAuthenticate(cfg ldapConfig, deptRulesDir, user, password string) (*ldapUser, error) {
	user = strings.TrimSpace(user)
	if user == "" || password == "" {
		return nil, fmt.Errorf("username and password required")
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("LDAP is not configured (missing url)")
	}

	// The domain prefix (DOMAIN\) only makes sense in front of a bare
	// sAMAccountName. A caller may instead type their email address /
	// userPrincipalName (simon.waldherr@rubix.com) or already include a
	// domain (ZITEC\waldherr) — AD accepts both of those as a bind name
	// as-is, and prepending DOMAIN\ to an "@" address would just break
	// the bind. Nothing here can be derived from the account's CN/surname
	// (usernames aren't guaranteed to follow that pattern), so both forms
	// have to be accepted verbatim from whatever the user typed.
	bindUser := user
	if cfg.DomainPrefix != "" && !strings.Contains(user, "\\") && !strings.Contains(user, "@") {
		bindUser = cfg.DomainPrefix + "\\" + user
	}

	// ldapTLSConfig (ldaptls.go) trusts Rubix's internal CA in addition to
	// the host OS's own roots, so this dial doesn't depend on the runtime
	// host already trusting inf-pla-04.zitec-intern.de's issuing CA.
	conn, err := ldap.DialURL(cfg.URL, ldap.DialWithTLSConfig(ldapTLSConfig()))
	if err != nil {
		return nil, fmt.Errorf("ldap connect: %w", err)
	}
	defer conn.Close()

	if err := conn.Bind(bindUser, password); err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}

	bareUser := user
	if _, after, ok := strings.Cut(user, "\\"); ok {
		bareUser = after
	}
	// bareUser may be a sAMAccountName, a mail address, or a
	// userPrincipalName depending on what the caller typed above — match
	// on all three rather than guessing which one it is.
	escaped := ldap.EscapeFilter(bareUser)
	filter := fmt.Sprintf(
		"(&(objectClass=organizationalPerson)(|(sAMAccountName=%s)(mail=%s)(userPrincipalName=%s)))",
		escaped, escaped, escaped,
	)
	searchReq := ldap.NewSearchRequest(
		cfg.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter,
		// Purpose-limited profile: display/account identity for the UI and
		// access checks, organization fields for optional answer
		// personalization, and objectGUID only for internal session
		// deduplication. Deliberately no employee ID, phone number, manager or
		// address attributes are fetched or persisted.
		[]string{"cn", "displayName", "givenName", "sn", "sAMAccountName", "userPrincipalName", "mail", "department", "title", "physicalDeliveryOfficeName", "company", "objectGUID", "memberOf"},
		nil,
	)
	sr, err := conn.Search(searchReq)
	if err != nil {
		return nil, fmt.Errorf("ldap search: %w", err)
	}
	if len(sr.Entries) != 1 {
		return nil, fmt.Errorf("user not found or ambiguous (%d entries)", len(sr.Entries))
	}
	entry := sr.Entries[0]
	u := &ldapUser{
		CN:                entry.GetAttributeValue("cn"),
		DisplayName:       entry.GetAttributeValue("displayName"),
		AccountName:       entry.GetAttributeValue("sAMAccountName"),
		UserPrincipalName: entry.GetAttributeValue("userPrincipalName"),
		Mail:              entry.GetAttributeValue("mail"),
		Department:        entry.GetAttributeValue("department"),
		Title:             entry.GetAttributeValue("title"),
		Office:            entry.GetAttributeValue("physicalDeliveryOfficeName"),
		Company:           entry.GetAttributeValue("company"),
		MemberOf:          entry.GetAttributeValues("memberOf"),
	}
	if rawGUID := entry.GetRawAttributeValue("objectGUID"); len(rawGUID) > 0 {
		u.DirectoryID = base64.RawURLEncoding.EncodeToString(rawGUID)
	}
	if u.CN == "" {
		u.CN = user
	}
	if u.AccountName == "" {
		u.AccountName = bareUser
	}
	if u.UserPrincipalName == "" {
		u.UserPrincipalName = u.Mail
	}
	if u.DisplayName == "" {
		u.DisplayName = strings.TrimSpace(entry.GetAttributeValue("givenName") + " " + entry.GetAttributeValue("sn"))
		if u.DisplayName == "" {
			u.DisplayName = u.CN
		}
	}
	u.DeptCode = classifyDepartment(departmentRulesOrDefault(deptRulesDir), u.Department, u.Title)

	// Every other verbose log in this codebase covers "which model/endpoint
	// handled this call" (llm.go) — this is the LDAP equivalent: exactly
	// what R3 read back from AD for this account, which is what you need
	// to see to debug a wrong department classification or to pick the
	// right required_group_dn (compare against memberOf here). Gated by
	// -verbose/R3_VERBOSE like everything else in this file's log output,
	// since it's mildly sensitive (department/title/office, full group DN
	// list) and not something a quiet production log should carry by default.
	if verbose {
		log.Printf("[verbose] ldap user resolved: cn=%q display_name=%q account=%q upn=%q mail=%q department=%q title=%q office=%q company=%q deptCode=%q memberOf=%v",
			u.CN, u.DisplayName, u.AccountName, u.UserPrincipalName, u.Mail, u.Department, u.Title, u.Office, u.Company, u.DeptCode, u.MemberOf)
	}

	// admin_users (a fixed allow-list, see settings.go's ldapConfig doc
	// comment) and required_group_dn each independently grant admin access
	// — either is enough, neither is required if the other already
	// matches. bareUser/user cover whatever the caller actually typed
	// (email, sAMAccountName, DOMAIN\\user); u.Mail/u.CN cover what AD
	// reports back, in case the admin listed a different form of the same
	// identity than the one used to log in.
	adminByList := ldapMatchesAdminUser(cfg.AdminUsers, u.CN, u.AccountName, u.UserPrincipalName, u.Mail, bareUser, user)

	if cfg.RequiredGroupDN == "" && len(cfg.AdminUsers) == 0 {
		log.Printf("WARN: neither ldap.required_group_dn nor ldap.admin_users is set — granting admin access to %q on bind alone; set one of them under Einstellungen -> LDAP to restrict admin access", u.CN)
		u.IsAdmin = true
		return u, nil
	}
	u.IsAdmin = adminByList
	if !u.IsAdmin && cfg.RequiredGroupDN != "" {
		u.IsAdmin = ldapIsMemberOf(u.MemberOf, cfg.RequiredGroupDN)
	}
	if !u.IsAdmin {
		log.Printf("LDAP login: %q authenticated but is not an admin (not in required_group_dn=%q, not in admin_users) — session created without admin access", u.CN, cfg.RequiredGroupDN)
	}
	return u, nil
}

// ldapMatchesAdminUser reports whether any of candidates (whatever forms
// of the account's identity are available — AD's own cn/mail, and
// whatever the caller actually typed to log in) case-insensitively
// matches an entry in adminUsers. Empty candidates are skipped so an
// unset AD attribute (e.g. no "mail") can't accidentally match an empty
// adminUsers entry.
func ldapMatchesAdminUser(adminUsers []string, candidates ...string) bool {
	for _, want := range adminUsers {
		if want == "" {
			continue
		}
		for _, have := range candidates {
			if have != "" && strings.EqualFold(have, want) {
				return true
			}
		}
	}
	return false
}

// ldapIsMemberOf checks membership by comparing DNs case-insensitively,
// since AD/LDAP DN casing isn't guaranteed consistent between a user's
// memberOf attribute and the configured required_group_dn.
func ldapIsMemberOf(memberOf []string, groupDN string) bool {
	for _, dn := range memberOf {
		if strings.EqualFold(dn, groupDN) {
			return true
		}
	}
	return false
}
