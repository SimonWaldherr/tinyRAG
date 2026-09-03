package app

// ─────────────────────────────────────────────────────────────────────────────
// Conversation export
//
// Renders a stored conversation as a self-contained Markdown document or a
// printable standalone HTML page (usable for print-to-PDF). No external
// assets are referenced so exports remain portable.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"
)

// exportRoleLabel maps internal role names to display labels.
func exportRoleLabel(role string) string {
	switch role {
	case "user":
		return "Nutzer"
	case "assistant":
		return "Assistent"
	default:
		return role
	}
}

// exportTimestamp formats an RFC3339 timestamp for humans; falls back to raw.
func exportTimestamp(raw string) string {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Format("02.01.2006 15:04")
	}
	return raw
}

var exportUnsafeFilenameRe = regexp.MustCompile(`[^\p{L}\p{N}\-_. ]+`)

// exportFilename derives a safe download filename from the conversation title.
func exportFilename(c *conversation, ext string) string {
	title := strings.TrimSpace(c.Title)
	if title == "" {
		title = c.ID
	}
	title = exportUnsafeFilenameRe.ReplaceAllString(title, "")
	title = strings.TrimSpace(title)
	if len(title) > 60 {
		title = title[:60]
	}
	if title == "" {
		title = "chat"
	}
	return strings.ReplaceAll(title, " ", "_") + "." + ext
}

// exportMarkdown renders the conversation as a Markdown document.
func (c *conversation) exportMarkdown(appName string) string {
	var sb strings.Builder
	title := strings.TrimSpace(c.Title)
	if title == "" {
		title = "Konversation " + c.ID
	}
	fmt.Fprintf(&sb, "# %s\n\n", title)
	fmt.Fprintf(&sb, "> Exportiert aus %s am %s\n", appName, time.Now().Format("02.01.2006 15:04"))
	if c.Created != "" {
		fmt.Fprintf(&sb, "> Erstellt: %s\n", exportTimestamp(c.Created))
	}
	sb.WriteString("\n---\n\n")
	for _, m := range c.Messages {
		fmt.Fprintf(&sb, "## %s — %s\n\n", exportRoleLabel(m.Role), exportTimestamp(m.Time))
		sb.WriteString(strings.TrimSpace(m.Content))
		sb.WriteString("\n\n")
		if m.Model != "" {
			fmt.Fprintf(&sb, "*Modell: %s*\n\n", m.Model)
		}
	}
	return sb.String()
}

// exportHTML renders the conversation as a standalone printable HTML page.
func (c *conversation) exportHTML(appName string) string {
	title := strings.TrimSpace(c.Title)
	if title == "" {
		title = "Konversation " + c.ID
	}
	var sb strings.Builder
	sb.WriteString("<!doctype html>\n<html lang=\"de\">\n<head>\n<meta charset=\"utf-8\">\n")
	fmt.Fprintf(&sb, "<title>%s</title>\n", html.EscapeString(title))
	sb.WriteString(`<style>
body{font-family:-apple-system,"Segoe UI",Roboto,sans-serif;max-width:820px;margin:2rem auto;padding:0 1rem;color:#1a1a1a;line-height:1.5}
h1{font-size:1.5rem;border-bottom:2px solid #ddd;padding-bottom:.5rem}
.meta{color:#666;font-size:.85rem;margin-bottom:2rem}
.msg{margin:1.25rem 0;padding:1rem;border-radius:8px;white-space:pre-wrap;word-wrap:break-word}
.msg.user{background:#eef2f7;border-left:4px solid #4a6fa5}
.msg.assistant{background:#f6f6f2;border-left:4px solid #7a9a65}
.msg .head{font-weight:600;font-size:.85rem;color:#444;margin-bottom:.5rem}
.msg .model{color:#888;font-size:.75rem;margin-top:.5rem}
@media print{body{margin:0;max-width:none}.msg{break-inside:avoid}}
</style>
</head>
<body>
`)
	fmt.Fprintf(&sb, "<h1>%s</h1>\n", html.EscapeString(title))
	fmt.Fprintf(&sb, "<div class=\"meta\">Exportiert aus %s am %s", html.EscapeString(appName), time.Now().Format("02.01.2006 15:04"))
	if c.Created != "" {
		fmt.Fprintf(&sb, " · Erstellt: %s", html.EscapeString(exportTimestamp(c.Created)))
	}
	sb.WriteString("</div>\n")
	for _, m := range c.Messages {
		cls := "assistant"
		if m.Role == "user" {
			cls = "user"
		}
		fmt.Fprintf(&sb, "<div class=\"msg %s\">\n<div class=\"head\">%s — %s</div>\n%s\n",
			cls,
			html.EscapeString(exportRoleLabel(m.Role)),
			html.EscapeString(exportTimestamp(m.Time)),
			html.EscapeString(strings.TrimSpace(m.Content)),
		)
		if m.Model != "" {
			fmt.Fprintf(&sb, "<div class=\"model\">Modell: %s</div>\n", html.EscapeString(m.Model))
		}
		sb.WriteString("</div>\n")
	}
	sb.WriteString("</body>\n</html>\n")
	return sb.String()
}
