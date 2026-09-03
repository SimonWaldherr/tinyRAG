package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// connRuntime is embedded (anonymously) into every multi-instance import
// connector config (mailboxConfig, exchangeGraphConfig, sharePointConfig,
// oneDriveConfig, teamsConfig, confluenceConfig, jiraConfig,
// freshserviceConfig, githubConfig, sapS4Config) — one
// shared shape for "which named connection is this" plus the three
// per-connection knobs an admin can now tune individually instead of only
// via the global importConfig (import_limits.go): Zyklus (SyncIntervalMinutes,
// except IMAP which keeps its own pre-existing PollInterval field), Limit
// (MaxItemsPerRun) and Timeout (TimeoutSeconds).
//
// Zero value for every numeric field means "fall back to the existing
// global/hardcoded default" — the same "0 = default" convention
// importConfig.MaxItemsPerRun and shopConfig.TimeoutSeconds already use, so
// a connection that never touches these fields behaves exactly as before.
// Name has no default: it's the merge/scheduler/UI key (see
// mergeConnList in handlers.go and buildJobs in scheduler.go) and must be
// unique within its connector type; migrateLegacySingularConnectors
// (settings.go) assigns "default" to whatever a pre-multi-connection
// settings.json held.
// ─────────────────────────────────────────────────────────────────────────────

type connRuntime struct {
	// Name identifies this connection within its connector type — shown as
	// the card title in Settings, used as the scheduler job key
	// ("<kind>-sync:<name>"), and as the by-name merge/lookup key for
	// settings saves and preview/import requests.
	Name string `json:"name"`
	// SyncIntervalMinutes, if > 0, has the scheduler (scheduler.go) run
	// this connection's import automatically on that interval, in addition
	// to the on-demand "Import jetzt"/preview button in the Import tab. 0
	// (default) means manual-only. Not used by IMAP, which keeps its own
	// pre-existing PollInterval (seconds, imap.go) instead of switching to
	// minutes on an already-shipped field.
	SyncIntervalMinutes int `json:"sync_interval_minutes,omitempty"`
	// MaxItemsPerRun caps how many items THIS connection ingests per run,
	// overriding importConfig.MaxItemsPerRun (import_limits.go) for this
	// connection only. 0 means "use the global default."
	MaxItemsPerRun int `json:"max_items_per_run,omitempty"`
	// TimeoutSeconds bounds how long this connection's import run (preview,
	// manual import, or scheduled sync) may take in total before it's
	// cancelled. 0 means "use the connector's existing default" (whatever
	// that connector already used before per-connection timeouts existed —
	// see each connector's call site for its specific fallback constant).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Paused suspends this connection's scheduled auto-sync without
	// touching its cycle configuration — flipping it back resumes the old
	// rhythm, unlike zeroing SyncIntervalMinutes/PollInterval, which would
	// forget the configured interval. Server-managed via the scheduler
	// dashboard's Pausieren/Fortsetzen action (POST /api/scheduler/pause),
	// NOT via the settings form — mergeConnRuntime below deliberately
	// preserves the saved value on every settings save, same treatment as
	// LastUID/DeltaLink, so a stale browser tab can't silently un-pause a
	// connector by re-saving old form state. Manual and ad-hoc imports
	// ignore Paused: pausing stops the unattended rhythm, not deliberate
	// human action.
	Paused bool `json:"paused,omitempty"`
}

// connName returns r.Name — the tiny accessor connWithName (handlers.go)
// requires so mergeConnList can find a connector's matching entry by name
// without every embedding struct needing its own boilerplate method.
func (r connRuntime) connName() string { return r.Name }

// isPaused is promoted onto every embedding connector config, so the
// scheduler's generic job builder (connJobs, scheduler.go) can read pause
// state through the connWithEnabled interface.
func (r connRuntime) isPaused() bool { return r.Paused }

// effectiveMaxItems resolves this connection's per-run item cap: its own
// MaxItemsPerRun if set, otherwise the deployment-wide default from
// importConfig (importMaxItems, import_limits.go) — same ceiling-clamping
// behavior either way, since importMaxItems already enforces
// importMaxItemsCeiling.
func (r connRuntime) effectiveMaxItems(global importConfig) int {
	if r.MaxItemsPerRun <= 0 {
		return importMaxItems(global)
	}
	if r.MaxItemsPerRun > importMaxItemsCeiling {
		return importMaxItemsCeiling
	}
	return r.MaxItemsPerRun
}

// effectiveTimeout resolves this connection's run timeout: its own
// TimeoutSeconds if set, otherwise defaultSeconds (the caller's
// pre-existing hardcoded fallback, e.g. testCtx's 20s).
func (r connRuntime) effectiveTimeout(defaultSeconds int) time.Duration {
	secs := r.TimeoutSeconds
	if secs <= 0 {
		secs = defaultSeconds
	}
	return time.Duration(secs) * time.Second
}

// requireConn resolves which of list (a connector type's configured
// connections) a preview/import/delta-sync request targets, and confirms
// it's enabled — the connection-aware counterpart of connector.go's
// requireEnabled, used by every handler in handlers_import_connectors.go.
// An empty name resolves to the sole entry when exactly one connection is
// configured (the common case, including every deployment auto-migrated
// from the old single-object settings.json — see
// migrateLegacySingularConnectors) so existing callers/scripts that never
// learned about multiple connections keep working unchanged; an empty
// name with zero or several configured connections is rejected, since
// there's no unambiguous default to fall back to. Writes the JSON error
// response itself (same contract as requireEnabled) so every call site is
// just `conn, ok := requireConn(...); if !ok { return }`.
func requireConn[T connWithEnabled](w http.ResponseWriter, list []T, name, label string) (T, bool) {
	var zero T
	name = strings.TrimSpace(name)
	if name == "" {
		if len(list) == 1 {
			name = list[0].connName()
		} else {
			writeJSONError(w, label+": connection name required (\"connection\" field) — "+strconv.Itoa(len(list))+" connections configured", http.StatusBadRequest)
			return zero, false
		}
	}
	conn, ok := findConnByName(list, name)
	if !ok {
		writeJSONError(w, label+": connection "+strconv.Quote(name)+" not found", http.StatusBadRequest)
		return zero, false
	}
	if !conn.isEnabled() {
		writeJSONError(w, label+" is not enabled for connection "+strconv.Quote(name), http.StatusBadRequest)
		return zero, false
	}
	return conn, true
}

// maskConnList applies mask (e.g. clearing/placeholder-ing one secret
// field) to a copy of every entry in list — used by maskedSettings
// (handlers.go) so a GET /api/settings never round-trips real secrets for
// ANY of a connector type's connections, not just a single fixed one.
func maskConnList[T any](list []T, mask func(*T)) []T {
	out := make([]T, len(list))
	copy(out, list)
	for i := range out {
		mask(&out[i])
	}
	return out
}

// mergeConnRuntime merges a patch's connRuntime fields onto cur, keeping
// cur's Paused: pause state is managed exclusively by the scheduler
// dashboard (handleSchedulerPause), so a settings save from a form that
// loaded before the pause happened must not silently revert it — the same
// server-managed-field rule as LastUID/DeltaLink.
func mergeConnRuntime(cur, patch connRuntime) connRuntime {
	patch.Paused = cur.Paused
	return patch
}

// mergeConnList applies handleSettings' usual patch semantics to a whole
// list at once, keyed by Name: patch is treated as the complete desired
// list (same "omitted means gone" convention every other list field in
// appSettings already uses, e.g. URLMappings) — an entry whose Name
// matches one in cur is merged field-by-field via mergeOne (preserving
// secrets/server-managed fields cur alone still has); a Name not present
// in cur is a brand-new connection and is added exactly as patch sent it
// (its secrets must be given in full — there's nothing saved yet to fall
// back to, same as adding a new MSSQL/HTTP query template today); a Name
// present in cur but absent from patch was removed by the admin (a
// deleted card) and is dropped.
func mergeConnList[T connWithName](cur, patch []T, mergeOne func(cur, patch T) T) []T {
	out := make([]T, 0, len(patch))
	for _, p := range patch {
		if existing, ok := findConnByName(cur, p.connName()); ok {
			out = append(out, mergeOne(existing, p))
		} else {
			out = append(out, p)
		}
	}
	return out
}

func mergeSharePointConn(cur, patch sharePointConfig) sharePointConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.TenantID != "" {
		cur.TenantID = patch.TenantID
	}
	if patch.ClientID != "" {
		cur.ClientID = patch.ClientID
	}
	if patch.ClientSecret != "" && !strings.Contains(patch.ClientSecret, "***") {
		cur.ClientSecret = patch.ClientSecret
	}
	if patch.ClientSecretEnv != "" {
		cur.ClientSecretEnv = patch.ClientSecretEnv
	}
	if patch.SiteURL != "" {
		cur.SiteURL = patch.SiteURL
	}
	cur.LiveSearchEnabled = patch.LiveSearchEnabled
	// DeltaLink/ItemPaths: server-managed (handleSharePointDeltaSync/
	// deltaSyncSharePoint), never overwritten from a settings patch.
	return cur
}

func mergeOneDriveConn(cur, patch oneDriveConfig) oneDriveConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	scopeChanged := (patch.DriveID != "" && patch.DriveID != cur.DriveID) || patch.FolderPath != cur.FolderPath
	if patch.TenantID != "" {
		cur.TenantID = patch.TenantID
	}
	if patch.ClientID != "" {
		cur.ClientID = patch.ClientID
	}
	if patch.ClientSecret != "" && !strings.Contains(patch.ClientSecret, "***") {
		cur.ClientSecret = patch.ClientSecret
	}
	if patch.ClientSecretEnv != "" {
		cur.ClientSecretEnv = patch.ClientSecretEnv
	}
	if patch.DriveID != "" {
		cur.DriveID = patch.DriveID
	}
	// Empty means whole drive, so it is a deliberate setting rather than
	// an omitted field to preserve.
	cur.FolderPath = patch.FolderPath
	// A Graph delta link is scoped to its drive/folder root. Retaining it
	// after an admin repoints either field would keep syncing the old scope.
	if scopeChanged {
		cur.DeltaLink = ""
	}
	// Otherwise DeltaLink remains server-managed by syncOneDrive.
	return cur
}

func mergeExchangeGraphConn(cur, patch exchangeGraphConfig) exchangeGraphConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.TenantID != "" {
		cur.TenantID = patch.TenantID
	}
	if patch.ClientID != "" {
		cur.ClientID = patch.ClientID
	}
	if patch.ClientSecret != "" && !strings.Contains(patch.ClientSecret, "***") {
		cur.ClientSecret = patch.ClientSecret
	}
	if patch.ClientSecretEnv != "" {
		cur.ClientSecretEnv = patch.ClientSecretEnv
	}
	if patch.Mailbox != "" {
		cur.Mailbox = patch.Mailbox
	}
	if patch.Folder != "" {
		cur.Folder = patch.Folder
	}
	// EnableDraftReplies/EnableAutoDraftRules/AutoDraftRules are ordinary
	// admin-editable settings (unconditional copy, same "false/empty is a
	// deliberate state" reasoning as Enabled/PollInterval above) — unlike
	// AutoDraftedIDs and LastSyncedReceived (the scheduled sync's
	// incremental watermark, graphmail.go/scheduler.go), which are
	// server-managed and deliberately NOT copied here, same treatment as
	// DeltaLink/LastUID.
	cur.EnableDraftReplies = patch.EnableDraftReplies
	cur.EnableAutoDraftRules = patch.EnableAutoDraftRules
	cur.AutoDraftRules = patch.AutoDraftRules
	// InteractiveEnabled/AllowedUsers/AllowedGroups/InteractiveShared: same
	// unconditional-copy reasoning — no secrets, and an emptied
	// AllowedUsers/AllowedGroups is a deliberate "revoke everyone's
	// interactive access", not "not filled in yet".
	cur.InteractiveEnabled = patch.InteractiveEnabled
	cur.AllowedUsers = patch.AllowedUsers
	cur.AllowedGroups = patch.AllowedGroups
	cur.InteractiveShared = patch.InteractiveShared
	return cur
}

func mergeIMAPConn(cur, patch mailboxConfig) mailboxConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.Host != "" {
		cur.Host = patch.Host
	}
	if patch.Port > 0 {
		cur.Port = patch.Port
	}
	cur.UseTLS = patch.UseTLS
	if patch.Username != "" {
		cur.Username = patch.Username
	}
	if patch.Password != "" && !strings.Contains(patch.Password, "***") {
		cur.Password = patch.Password
	}
	if patch.PasswordEnv != "" {
		cur.PasswordEnv = patch.PasswordEnv
	}
	if patch.Mailbox != "" {
		cur.Mailbox = patch.Mailbox
	}
	// Unconditional (0 = deliberate "auto-abruf off"), read by
	// scheduler.go's imap-sync job — same reasoning as every other
	// connector's SyncIntervalMinutes.
	cur.PollInterval = patch.PollInterval
	if patch.DraftsMailbox != "" {
		cur.DraftsMailbox = patch.DraftsMailbox
	}
	// LastUID: server-managed (imapmail.go's import run), never
	// overwritten from a settings patch.
	return cur
}

func mergeTeamsConn(cur, patch teamsConfig) teamsConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.TenantID != "" {
		cur.TenantID = patch.TenantID
	}
	if patch.ClientID != "" {
		cur.ClientID = patch.ClientID
	}
	if patch.ClientSecret != "" && !strings.Contains(patch.ClientSecret, "***") {
		cur.ClientSecret = patch.ClientSecret
	}
	if patch.ClientSecretEnv != "" {
		cur.ClientSecretEnv = patch.ClientSecretEnv
	}
	if patch.TeamID != "" {
		cur.TeamID = patch.TeamID
	}
	if patch.ChannelID != "" {
		cur.ChannelID = patch.ChannelID
	}
	return cur
}

func mergeConfluenceConn(cur, patch confluenceConfig) confluenceConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.BaseURL != "" {
		cur.BaseURL = patch.BaseURL
	}
	if patch.Email != "" {
		cur.Email = patch.Email
	}
	if patch.APIToken != "" && !strings.Contains(patch.APIToken, "***") {
		cur.APIToken = patch.APIToken
	}
	if patch.APITokenEnv != "" {
		cur.APITokenEnv = patch.APITokenEnv
	}
	if patch.SpaceKey != "" {
		cur.SpaceKey = patch.SpaceKey
	}
	return cur
}

func mergeJiraConn(cur, patch jiraConfig) jiraConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.BaseURL != "" {
		cur.BaseURL = patch.BaseURL
	}
	if patch.Email != "" {
		cur.Email = patch.Email
	}
	if patch.APIToken != "" && !strings.Contains(patch.APIToken, "***") {
		cur.APIToken = patch.APIToken
	}
	if patch.APITokenEnv != "" {
		cur.APITokenEnv = patch.APITokenEnv
	}
	if patch.ProjectKey != "" {
		cur.ProjectKey = patch.ProjectKey
	}
	return cur
}

func mergeFreshserviceConn(cur, patch freshserviceConfig) freshserviceConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.BaseURL != "" {
		cur.BaseURL = patch.BaseURL
	}
	if patch.APIKey != "" && !strings.Contains(patch.APIKey, "***") {
		cur.APIKey = patch.APIKey
	}
	if patch.APIKeyEnv != "" {
		cur.APIKeyEnv = patch.APIKeyEnv
	}
	return cur
}

// mergeFolderConn is the smallest of these merge functions — folderConfig
// has no secret field to preserve and no server-managed cursor field.
func mergeFolderConn(cur, patch folderConfig) folderConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.Path != "" {
		cur.Path = patch.Path
	}
	return cur
}

func mergeGitHubConn(cur, patch githubConfig) githubConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	scopeChanged := (patch.BaseURL != "" && patch.BaseURL != cur.BaseURL) || (patch.Owner != "" && patch.Owner != cur.Owner) || (patch.Repository != "" && patch.Repository != cur.Repository)
	if patch.BaseURL != "" {
		cur.BaseURL = patch.BaseURL
	}
	if patch.Token != "" && !strings.Contains(patch.Token, "***") {
		cur.Token = patch.Token
	}
	if patch.TokenEnv != "" {
		cur.TokenEnv = patch.TokenEnv
	}
	if patch.Owner != "" {
		cur.Owner = patch.Owner
	}
	if patch.Repository != "" {
		cur.Repository = patch.Repository
	}
	cur.IncludeReadme = patch.IncludeReadme
	cur.IncludeIssues = patch.IncludeIssues
	cur.IncludePullRequests = patch.IncludePullRequests
	// The page/cycle cursor belongs to exactly one repository endpoint. Reset
	// it when the scope changes; otherwise it remains server-managed.
	if scopeChanged {
		cur.LastSyncedAt = ""
		cur.CycleStartedAt = ""
		cur.NextPage = 0
	}
	return cur
}

func mergeSAPS4Conn(cur, patch sapS4Config) sapS4Config {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	scopeChanged := (patch.BaseURL != "" && patch.BaseURL != cur.BaseURL) || (patch.EntityPath != "" && patch.EntityPath != cur.EntityPath)
	if patch.BaseURL != "" {
		cur.BaseURL = patch.BaseURL
	}
	if patch.AuthType != "" {
		cur.AuthType = patch.AuthType
	}
	if patch.Username != "" {
		cur.Username = patch.Username
	}
	if patch.Password != "" && !strings.Contains(patch.Password, "***") {
		cur.Password = patch.Password
	}
	if patch.PasswordEnv != "" {
		cur.PasswordEnv = patch.PasswordEnv
	}
	if patch.Token != "" && !strings.Contains(patch.Token, "***") {
		cur.Token = patch.Token
	}
	if patch.TokenEnv != "" {
		cur.TokenEnv = patch.TokenEnv
	}
	if patch.HeaderName != "" {
		cur.HeaderName = patch.HeaderName
	}
	cur.Headers = patch.Headers
	if patch.EntityPath != "" {
		cur.EntityPath = patch.EntityPath
	}
	if patch.IDField != "" {
		cur.IDField = patch.IDField
	}
	// Empty title/content fields are meaningful: title can fall back to id,
	// while an empty content list must not accidentally preserve columns an
	// administrator deliberately removed.
	cur.TitleField = patch.TitleField
	cur.ContentFields = patch.ContentFields
	cur.UpdatedAtField = patch.UpdatedAtField
	// OData continuation and delta URLs belong to one entity-set endpoint.
	// Reset them when that endpoint changes; otherwise they remain
	// server-managed by syncSAPS4.
	if scopeChanged {
		cur.DeltaLink = ""
		cur.NextLink = ""
	}
	return cur
}

// mergeRESTConn merges a generic REST connector patch — same masked-secret
// rule as the Basic-auth import connectors (skip a "***"-placeholder
// Password/Token so a settings save from the browser doesn't blank a real
// secret it was never shown), plus the two live-connector fields. Headers is
// replaced wholesale (an emptied map is a deliberate "no extra headers", same
// reasoning as MSSQL.MaskColumns / URLMappings in handleSettings). A REST
// connector has no server-managed cursor field, so nothing is preserved from
// cur beyond the secrets.
func mergeRESTConn(cur, patch restConnectorConfig) restConnectorConfig {
	cur.connRuntime = mergeConnRuntime(cur.connRuntime, patch.connRuntime)
	cur.Enabled = patch.Enabled
	if patch.BaseURL != "" {
		cur.BaseURL = patch.BaseURL
	}
	// AuthType: the form always sends one of the fixed option values
	// (never blank), so merge-if-non-empty is enough — no "reset to unset"
	// case, same as Upload.ImageMode in handleSettings.
	if patch.AuthType != "" {
		cur.AuthType = patch.AuthType
	}
	if patch.Username != "" {
		cur.Username = patch.Username
	}
	if patch.Password != "" && !strings.Contains(patch.Password, "***") {
		cur.Password = patch.Password
	}
	if patch.PasswordEnv != "" {
		cur.PasswordEnv = patch.PasswordEnv
	}
	if patch.Token != "" && !strings.Contains(patch.Token, "***") {
		cur.Token = patch.Token
	}
	if patch.TokenEnv != "" {
		cur.TokenEnv = patch.TokenEnv
	}
	if patch.HeaderName != "" {
		cur.HeaderName = patch.HeaderName
	}
	cur.Headers = patch.Headers
	// AccessControl: unconditional copy, same "emptied is a deliberate
	// revoke-the-restriction" reasoning as Headers above — no secrets.
	cur.AccessControl = patch.AccessControl
	return cur
}

// connWithName is the minimal shape mergeConnList (handlers.go) and
// findConn (handlers_import_connectors.go) need from any multi-instance
// connector config: something to key adds/updates/removes and lookups by.
type connWithName interface {
	connName() string
}

// connWithEnabled additionally exposes Enabled and Paused, for
// firstEnabledConn below and the scheduler's generic job builder
// (connJobs, scheduler.go). isPaused comes for free via the embedded
// connRuntime; isEnabled each config defines itself, since Enabled
// deliberately isn't part of connRuntime.
type connWithEnabled interface {
	connWithName
	isEnabled() bool
	isPaused() bool
}

// findConnByName returns the entry in list whose Name matches name, used
// by handlers_import_connectors.go/conntest.go to resolve which configured
// connection a preview/import/test request targets.
func findConnByName[T connWithName](list []T, name string) (T, bool) {
	for _, c := range list {
		if c.connName() == name {
			return c, true
		}
	}
	var zero T
	return zero, false
}

// findConnIndex is findConnByName plus the index, for callers that need to
// write a server-managed field back into a specific slice element (e.g.
// IMAP.LastUID, SharePoint.DeltaLink) via settings.update.
func findConnIndex[T connWithName](list []T, name string) (T, int, bool) {
	for i, c := range list {
		if c.connName() == name {
			return c, i, true
		}
	}
	var zero T
	return zero, -1, false
}

// firstEnabledConn returns the first Enabled entry in list. Used by the
// handful of call sites (agent.go's mail-draft tool, http_tool.go's
// auth_source resolution) that predate multi-instance connections and
// only ever needed "the" configured connection of a type — until those
// features grow their own explicit connection selection, "first enabled"
// is the least-surprising default: it's exactly what the old single-struct
// field behaved like whenever Enabled was true.
func firstEnabledConn[T connWithEnabled](list []T) (T, bool) {
	for _, c := range list {
		if c.isEnabled() {
			return c, true
		}
	}
	var zero T
	return zero, false
}
