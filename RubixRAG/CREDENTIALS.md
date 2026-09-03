# Credentials & environment variables — how to set them, and best practice

This document is the practical companion to the `*_env`/`api_key`-style
fields scattered across `settings.go` and `settings.json`: how to actually
set an environment variable on each platform R3 runs on, and which
credential fields should use one instead of the inline, plaintext
alternative.

## Why this matters

`settings.json` is a single JSON file, world-readable to anything with
filesystem access to the process (or a backup of it), and it is the file
`GET /api/settings` reads from and `POST /api/settings` writes back to on
every save. Every credential-shaped field in it comes in a pair:

| Inline field (plaintext in the file) | Env-var-name field | Config struct |
|---|---|---|
| `profiles.<name>.api_key` (`local`/`azure`/`openai`/`openrouter`/`claude`/`gemini`) | `profiles.<name>.api_key_env` | `llmProfile` ([settings.go:26](settings.go)) — see the "LLM profiles" caveat below, it applies to **all six**, not just Azure |
| `exchange_graph[].client_secret` | `exchange_graph[].client_secret_env` | `exchangeGraphConfig` |
| `sharepoint[].client_secret` | `sharepoint[].client_secret_env` | `sharePointConfig` |
| `teams[].client_secret` | `teams[].client_secret_env` | `teamsConfig` |
| `confluence.api_token` | `confluence.api_token_env` | `confluenceConfig` |
| `jira.api_token` | `jira.api_token_env` | `jiraConfig` |
| `freshservice.api_key` | `freshservice.api_key_env` | `freshserviceConfig` |
| `smtp.password` | `smtp.password_env` | `smtpConfig` |
| `mssql.password` | `mssql.password_env` | `mssqlConfig` |
| `shop.password` | `shop.password_env` | `shopConfig` |
| `shop.client_secret` | `shop.client_secret_env` | `shopConfig` (see note below) |
| `rest_connectors[].password` | `rest_connectors[].password_env` | `restConnectorConfig` — generic REST-backend connector (`auth_type: "basic"`), see [interfaces/http-query-tool.md](interfaces/http-query-tool.md) |
| `rest_connectors[].token` | `rest_connectors[].token_env` | `restConnectorConfig` — bearer token or raw header value (`auth_type: "bearer"`/`"header"`) |
| `admin_password_env` only (no inline form) | `admin_password_env` | top-level |

**Rule: whenever an `*_env` field exists for a credential, prefer it over
the inline value and leave the inline field empty.** Every connector
resolves the env-var form first (`resolveSecret` in
[connector.go](connector.go), same priority for MSSQL/Shop/Graph/
SharePoint/Teams/REST-connectors) — **except every `profiles.<name>` LLM
profile** (`local`, `azure`, `openai`, `openrouter`, `claude`, `gemini`),
whose inline `api_key` currently wins over `api_key_env` if both are set
(a known inconsistency in `newLMClientFromProfile`, see the "LLM profiles"
note below — this is not Azure-specific, it's the same code path for all
six chat/embedding profiles). For every other credential, setting the env
var and leaving the JSON field blank is both safer and functionally
equivalent.

Note on `rest_connectors[]` (the generic REST-backend connector, see
[interfaces/http-query-tool.md](interfaces/http-query-tool.md)): unlike the
Folder/RSS import connectors (which need no credentials at all — a
filesystem path or a public feed URL), a REST connector's `auth_type`
(`none`/`basic`/`bearer`/`header`) decides which of `password(_env)`/
`token(_env)` is actually used; `headers{}` (static extra headers) is
**not** the place for the primary credential — those values round-trip to
the browser in clear, unlike `password`/`token`, which are masked like every
other secret field on `GET /api/settings`.

Note on `shop.client_id`/`client_secret`: unlike every other row above,
these aren't a per-account secret — they're de.rubix.com's own shop
frontend's fixed, publicly-observable "browser API client" pair (visible
in that shop's own browser devtools network tab), stored in config rather
than hardcoded only so it isn't a literal secret-shaped string baked into
source (see `shopConfig`'s doc comment, `settings.go`). Still fine to move
to `client_secret_env` for consistency, just not the same risk level as
the other rows.

The one exception with no inline alternative at all is
`admin_password_env` — it only ever names an environment variable; there
is no `admin_password` field to accidentally leave populated.

## Setting environment variables

### Windows (PowerShell) — for a manually started R3

Session-only (lost when the terminal closes):

```powershell
$env:AZURE_OPENAI_API_KEY = "..."
.\R3.exe -addr :8090 -storage-path r3-data -url http://localhost:1234 `
    -chat mistralai/ministral-3-3b -embed text-embedding-nomic-embed-text-v1.5
```

Persisted for the current Windows user (survives reboots, visible to every
process that user starts — set once, not per session):

```powershell
[System.Environment]::SetEnvironmentVariable("AZURE_OPENAI_API_KEY", "...", "User")
```

Persisted machine-wide (requires an elevated/Administrator PowerShell):

```powershell
[System.Environment]::SetEnvironmentVariable("AZURE_OPENAI_API_KEY", "...", "Machine")
```

Either persisted form requires opening a **new** terminal (or restarting
the service) before R3 picks it up — `SetEnvironmentVariable` doesn't
affect already-running processes or the current session.

### Linux — for the running production instance (`/etc/init.d/r3`)

Per ["Running as a service"](README.md#running-as-a-service-etcinitdr3) in
the README, every start-time value for the production instance lives in
`/etc/default/r3`, not in a shell profile — the init script sources this
file before starting R3:

```bash
cp deploy/r3.default /etc/default/r3   # one-time, if not already done
chown root:root /etc/default/r3 && chmod 600 /etc/default/r3
```

Add or edit the variable in `/etc/default/r3`:

```bash
AZURE_OPENAI_API_KEY=...
```

`chmod 600` (owner-read-only) is the point — this file, unlike
`settings.json`, is meant to hold secrets, and its permissions are the
actual boundary protecting them, not just a convention. Apply the same
`chmod 600` to any *other* file used the same way (an `.env` file sourced
manually, a systemd `EnvironmentFile=`, etc.) if the deployment ever moves
off the init.d script.

After editing, restart to pick it up:

```bash
service r3 restart
```

### Ad-hoc / local dev (either platform)

For a quick local test where persistence doesn't matter, prefix the
command directly:

```bash
AZURE_OPENAI_API_KEY=... ./R3 -addr :8090 -storage-path r3-data -url http://localhost:1234
```

## Best practice for credentials in this repo

1. **Prefer the `_env` field over the inline field, everywhere one
   exists.** Set the environment variable via one of the mechanisms
   above, then leave the corresponding `api_key`/`client_secret`/
   `password`/`api_token` field in `settings.json` empty (`""` or
   omitted). Don't set both — inline vs. env-var priority differs by
   connector today (see the Azure caveat below), so having both invites
   confusion about which one is actually in effect even where the
   resolution order is correct.
2. **Rotate a credential by rotating the env var, not by hand-editing
   `settings.json`.** Since the file gets rewritten on every settings
   save (`loadOrCreateSettings`/`saveLocked`, `settings.go`), an inline
   secret can resurface in the file even after being cleared from the UI
   once, if something else re-populates it; an env-var-only credential
   has nothing in the file to resurface.
3. **`settings.json` itself should still be treated as sensitive**, even
   with credentials moved to env vars — it can still contain tenant IDs,
   client IDs, internal hostnames, and mailbox/site addresses that are
   useful reconnaissance on their own. It's already `.gitignore`d and
   should keep restrictive filesystem permissions (`0o600`, which
   `saveLocked` already sets); don't relax that, and don't paste its full
   contents into shared channels, tickets, or chat tools without
   redacting the secret-bearing fields first.
4. **Use `admin_password_env`, not an open admin gate, once LDAP is off.**
   With `ldap.enabled: false`, `/api/admin/check` only compares against
   whatever `admin_password_env` names — if that variable is unset, the
   check "always passes" (see README's "Admin access" section). Set it
   explicitly for any instance reachable by more than just the operator's
   own machine.
5. **Prefer LDAP over the shared admin password where available.** Per
   README's "Admin access" section, `ldap.enabled: true` is the only mode
   where admin routes are actually access-controlled per-user
   (`requireAdminSession` + AD bind); the password-only mode is real MVP
   behavior, not real authorization — none of the admin-only *API
   endpoints* are protected in that mode beyond the login button itself.
6. **Never commit a real credential, even temporarily.** If a real secret
   ever ends up staged in git (e.g. via an accidental `git add
   settings.json` on a machine where the ignore rule was bypassed), treat
   it as compromised — rotate it at the source (Azure AD app secret,
   Atlassian API token, SQL Server login, etc.) rather than relying on
   removing it from a future commit, since it already exists in the
   commit history at that point.
7. **Redact before sharing.** If `settings.json`'s contents ever need to
   be pasted somewhere (a support request, a chat with an AI assistant, a
   ticket) for structural review, replace every credential value with a
   placeholder like `"pw"` first — the field *names* and structure are
   what's usually needed for review, not the actual secret values.

## Known inconsistency: LLM profiles (`local`/`azure`/`openai`/`openrouter`/`claude`/`gemini`)

`newLMClientFromProfile` ([llm.go:153-163](llm.go)) currently reads each
profile's inline `api_key` **before** falling back to its `api_key_env` —
the opposite priority from every other connector's `resolveSecret`-based
resolution ([connector.go](connector.go), which checks the env var first).
This affects **all six** chat/embedding profiles equally (it's not
Azure-specific, even though it was first noticed there) — `openai`,
`openrouter`, `claude` and `gemini` were added after this note was first
written and go through the exact same `newLMClientFromProfile` code path.
Until that's fixed in code, setting e.g. `AZURE_OPENAI_API_KEY`,
`OPENAI_API_KEY`, `OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY` or
`GEMINI_API_KEY` has **no effect** if that profile's inline `api_key` is
also non-empty in `settings.json`. The safe workaround today: leave every
`profiles.<name>.api_key` empty and rely purely on `api_key_env` + the
environment variable, exactly as this document recommends for every other
profile/connector.
