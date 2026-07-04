# UI Customization, Theming & Headless Usage

tinyRAG is designed as a **base/blueprint for arbitrary RAG frontends**: the
web UI is data-driven and can be reshaped per deployment without touching
HTML/CSS/JS, and the binary doubles as a headless CLI RAG engine for scripts,
pipelines and TUI wrappers.

## 1. UI configuration (`ui` in settings.json)

Everything defaults to *enabled*; you only list what you want to change.

```jsonc
{
  "ui": {
    "default_panel": "chat",            // chat | search | ingest | stats
    "panels":  { "ingest": false },     // hide whole panels
    "modes":   { "offline": false,      // hide chat mode toggles
                 "agent": true },
    "show_persona_picker": true,
    "show_role_picker": false,
    "show_llm_switcher": false,
    "footer_text": "Interne Wissensdatenbank – Antworten prüfen.",
    "suggestions": [                    // replaces the empty-state buttons
      { "label": "Onboarding", "prompt": "Fasse den Onboarding-Prozess zusammen" },
      { "label": "IT-Security", "prompt": "Was sind unsere Passwort-Richtlinien?" }
    ]
  }
}
```

API:

| Endpoint            | Method | Auth  | Purpose                                    |
|---------------------|--------|-------|--------------------------------------------|
| `/api/ui`           | GET    | –     | Full UI bootstrap: config, themes, branding |
| `/api/ui/config`    | POST   | admin | Replace the `ui` block                      |
| `/api/ui/themes`    | POST   | admin | Upsert (`{"theme": {…}}`) or delete (`{"delete": "id"}`) a custom theme |

Notes:

- The default panel is always kept reachable (it cannot be disabled away).
- Suggestions are capped at 8; labels at 40 chars, prompts at 300.
- Branding (`app_name`, `app_logo_url`, `custom_css`) works as before and is
  included in the `/api/ui` payload.

## 2. Themes

Six built-in themes ship in `style.css` (`dark`, `light`, `nord`, `solarized`,
`monokai`, `dracula`). **Custom themes** are CSS-variable maps layered on top
of a built-in base theme:

```jsonc
{
  "custom_themes": [
    {
      "id": "corporate",
      "label": "Corporate Orange",
      "base": "light",
      "vars": {
        "--accent": "#ff6600",
        "--accent-subtle": "rgba(255,102,0,.12)",
        "--bg": "#fafafa",
        "--panel": "#ffffff"
      }
    }
  ]
}
```

Custom themes appear automatically as selectable cards in *Settings →
General* and can be activated via `POST /api/settings/theme`
(`{"theme": "corporate"}`).

Security: variable names must match `--[a-z0-9-]+`; values are rejected if
they contain `{ } ; < > \`, `url(`, `expression`, `@import` or
`javascript:`. On the client the values are applied via
`style.setProperty(…)`, which cannot escape the declaration — the server-side
filter is defense in depth. Available variables: see the `:root` block at the
top of `style.css` (`--bg`, `--panel`, `--text`, `--accent`, `--border`, …).

## 3. Headless / CLI / TUI usage

The same binary is a scriptable RAG engine — the building block for CLI
tools, cron jobs, or your own TUI:

```bash
# One-shot question (streams the answer to stdout)
./tinyRAG -ask "Wie funktioniert der Rollback-Prozess?"

# Machine-readable envelope for pipelines
./tinyRAG -ask "…" -jsonout
# → {"question": "…", "answer": "…", "context_chars": 4813, "chunks_used": 5}

# Semantic search only (no LLM call needed beyond embeddings)
./tinyRAG -searchq "kubernetes deployment" -jsonout

# Interactive REPL (colored; NO_COLOR or -nocolor disables ANSI)
./tinyRAG -web=false
```

REPL commands: `/search`, `/add` (Wikipedia), `/url` (web page), `/sources`,
`/count`, `/role [it|logistik|vertrieb|hr]`, `/stats`, `/help`, `/quit` —
anything else is answered as a RAG question.

`-ask`/`-searchq` take precedence over `-web`, so a pipeline can call the
binary directly without extra flags.

## 4. Putting it together: shipping a differently-shaped RAG product

A minimal "search-only knowledge kiosk" deployment, for example:

```jsonc
{
  "app_name": "ACME Wissensportal",
  "theme": "corporate",
  "custom_themes": [ { "id": "corporate", "…": "…" } ],
  "ui": {
    "default_panel": "search",
    "panels": { "ingest": false, "stats": false },
    "modes": { "deep": false, "offline": false, "agent": false, "debug": false },
    "show_llm_switcher": false,
    "show_persona_picker": false
  }
}
```

The same binary, restyled and reduced to search + chat — or fully headless
behind your own frontend via `/api/ask`, `/api/search` and `/api/process`.
