# Abteilungs-/Zugriffsregeln (`/api/department-rules*`)

**Verbindungsrolle:** Server
**Datenfluss:** gemischt – Lesen ist Pull, `save`/`reset` ist Push
**Schutz:** `requireAdminSession`
**Registrierung:** [handlers.go:193-195](../handlers.go)
**Implementierung:** `department_admin.go`

## Endpunkte

| Pfad | Zweck |
|---|---|
| `/api/department-rules` | Regeln auflisten/aktualisieren |
| `/api/department-rules/save` | Regelsatz speichern |
| `/api/department-rules/reset` | Auf Standard zurücksetzen |

## Zweck

Definiert, welche Abteilung/Nutzergruppe auf welche Quellen zugreifen darf
(`settings.SourceAccess`, `settings.SourceVisibility` – [settings.go:324](../settings.go)).
Wird u. a. von der LDAP-Anmeldung zur Klassifizierung genutzt.

```mermaid
flowchart TD
    Admin[Admin-UI] -->|CRUD Regeln| H[department_admin.go]
    H --> Settings[(settings.json: source_access / source_visibility)]
    LDAPLogin["LDAP-Login (Gruppenmitgliedschaft)"] --> Classify[Abteilungs-Klassifizierung]
    Classify --> Settings
    Content["/api/sources/content, /original"] -->|prüft| Settings
```

## Zusammenhänge

- Durchgesetzt in [sources-chunks.md](sources-chunks.md) (`sourceAccessAllowedForRequest`)
- Klassifizierung erfolgt beim Login: [auth-ldap-login.md](auth-ldap-login.md), [ldap-directory-outgoing.md](ldap-directory-outgoing.md)
