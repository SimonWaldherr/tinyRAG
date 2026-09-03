package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	ParamHint   string      `json:"param_hint"`
	InputSchema *JSONSchema `json:"input_schema,omitempty"`
}

// builtinTools defines the available built-in tools for the LLM assistant.
var builtinTools = []toolDef{
	{
		Name:        "rag_knowledge",
		Description: "Durchsucht die lokale Wissensbasis (RAG-Datenbank) semantisch. Verwende dies, wenn interne Dokumente, Handbücher oder gespeichertes Wissen relevant ist.",
		ParamHint:   "Suchbegriff für die Vektorsuche",
	},
	{
		Name:        "url_fetch",
		Description: "Lädt eine URL und gibt deren Plaintext zurück. Verwende dies, wenn der Nutzer eine spezifische URL erwähnt oder eine Webseite direkt ausgelesen werden soll.",
		ParamHint:   "Vollständige URL (https://...) oder arguments: {\"url\":\"https://...\"}",
		InputSchema: &JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchemaProperty{
				"url": {Type: "string", Description: "Öffentliche http(s)-URL"},
			},
			Required: []string{"url"},
		},
	},
	{
		Name:        "wikipedia",
		Description: "Sucht einen Wikipedia-Artikel und lädt dessen Volltext. Verwende dies für Fakten über Personen, Orte, Ereignisse, Wissenschaft etc.",
		ParamHint:   "Artikelname (z.B. 'Sonnensystem', 'Albert_Einstein')",
	},
	{
		Name:        "duckduckgo",
		Description: "Durchsucht das Web über DuckDuckGo und liefert eine Kurzantwort. Gut für aktuelle Fakten, Definitionen, kurze Zusammenfassungen.",
		ParamHint:   "Suchbegriff (z.B. 'Hauptstadt von Frankreich')",
	},
	{
		Name:        "wiktionary",
		Description: "Schlägt ein Wort im Wiktionary (Wörterbuch) nach. Liefert Bedeutung, Etymologie, Übersetzungen.",
		ParamHint:   "Einzelnes Wort (z.B. 'Apfel', 'serendipity')",
	},
	{
		Name:        "stackoverflow",
		Description: "Sucht relevante StackOverflow-Fragen und Antworten über die StackExchange-API (gut für Programmierfragen).",
		ParamHint:   "Suchbegriff (z.B. 'go http client timeout')",
	},
	{
		Name:        "websearch",
		Description: "Allgemeine Websuche mit Query-Varianten. Nutzt mehrere Formulierungen und kombiniert DuckDuckGo, Wikidata sowie bei technischen Themen GitHub und StackOverflow.",
		ParamHint:   "Suchbegriff (z.B. 'Wetter Berlin heute')",
	},
	{
		Name:        "news",
		Description: "Sucht nach aktuellen Nachrichten zu einem Thema.",
		ParamHint:   "Thema (z.B. 'Künstliche Intelligenz')",
	},
	{
		Name:        "wikidata",
		Description: "Sucht strukturierte Entitäten und Beschreibungen in Wikidata. Gut für Produkte, Firmen, Standards, technische Begriffe oder bekannte Objekte.",
		ParamHint:   "Entität oder Suchbegriff",
	},
	{
		Name:        "github",
		Description: "Sucht öffentliche GitHub-Repositories. Gut für Libraries, SDKs, Implementierungen und technische Referenzen.",
		ParamHint:   "Repository-, Projekt- oder Library-Suchbegriff",
	},
	{
		Name:        "nanogo",
		Description: "Führt sicheren, interpretierten Go-Code (nanoGo) aus. Verwende dies für Logik, Datenverarbeitung oder wenn du Berechnungen brauchst, die über einfache Arithmetik hinausgehen.",
		ParamHint:   "Go-Quelltext (kurze Snippets)",
	},
	{
		Name:        "calculate",
		Description: "Führt eine sichere Berechnung aus (arithmetische Ausdrücke). Nutzt smallR für schnelle Evaluation.",
		ParamHint:   "Expression (z.B. '3*2+(2^3)')",
	},
	{
		Name:        "json_query",
		Description: "Liest lokal einen Wert aus einem JSON-Dokument über einen Punktpfad. Gut für API-Antworten, Konfigurationen und strukturierte Tool-Ergebnisse.",
		ParamHint:   "arguments: {\"json\":\"{...}\", \"path\":\"items.0.name\"}",
		InputSchema: &JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchemaProperty{
				"json": {Type: "string", Description: "JSON-Dokument als Text"},
				"path": {Type: "string", Description: "Optionaler Punktpfad, z.B. items.0.name"},
			},
			Required: []string{"json"},
		},
	},
	{
		Name:        "text_diff",
		Description: "Vergleicht zwei lokale Texte zeilenweise und gibt nur Hinzufügungen sowie Entfernungen zurück.",
		ParamHint:   "arguments: {\"before\":\"alter Text\", \"after\":\"neuer Text\"}",
		InputSchema: &JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchemaProperty{
				"before": {Type: "string", Description: "Ausgangstext"},
				"after":  {Type: "string", Description: "Vergleichstext"},
			},
			Required: []string{"before", "after"},
		},
	},
	{
		Name:        "regex_extract",
		Description: "Extrahiert Treffer und optionale Capture-Gruppen eines regulären Ausdrucks aus lokalem Text.",
		ParamHint:   "arguments: {\"pattern\":\"ID-(\\\\d+)\", \"text\":\"...\", \"limit\":10}",
		InputSchema: &JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchemaProperty{
				"pattern": {Type: "string", Description: "Regulärer Ausdruck (RE2)"},
				"text":    {Type: "string", Description: "Zu untersuchender Text"},
				"limit":   {Type: "integer", Description: "Optionale maximale Trefferzahl (1 bis 50)"},
			},
			Required: []string{"pattern", "text"},
		},
	},
	{
		Name:        "shell",
		Description: "Führt häufig verwendete Shell-Befehle auf dem Server aus (z.B. 'ls', 'cat', 'curl'). Muss explizit in den Einstellungen aktiviert werden. WARNUNG: Sicherheitsrisiko!",
		ParamHint:   "Shell-Befehl (z.B. 'ls -la')",
	},
	{
		Name:        "local_search",
		Description: "Durchsucht die eigene lokale Wissensbasis (RAG-Datenbank) nach zusätzlichen Informationen. Nutze dies für interaktive Suchen in deinen Daten.",
		ParamHint:   "Suchbegriff für die Vektorsuche",
	},
	{
		Name:        "vector_query",
		Description: "Führt eine präzise Vektorähnlichkeitssuche in der Wissensbasis aus. Gut für semantisch verwandte Konzepte, Synonymsuchen oder wenn `local_search` nicht die gewünschten Treffer liefert. Unterstützt optionale Parameter: 'k:10 threshold:0.7 <Suchbegriff>'.",
		ParamHint:   "[k:N] [threshold:F] Suchbegriff (z.B. 'k:8 threshold:0.65 Lagertemperatur')",
	},
	{
		Name:        "sql_query",
		Description: "Führt eine schreibgeschützte SQL-SELECT-Abfrage direkt auf der tinySQL-Wissensbasis aus. Nützlich für genaue Textsuchen, Filterung nach Artikel/Quelle oder Aggregationen. Nur SELECT erlaubt. Verfügbare Tabelle: chunks (id INT, article TEXT, chunk_idx INT, content TEXT, embed_model TEXT, role_scope TEXT).",
		ParamHint:   "SQL SELECT-Statement (z.B. 'SELECT article, content FROM chunks WHERE content LIKE \"%Temperatur%\" LIMIT 5')",
	},
	{
		Name:        "datetime",
		Description: "Gibt das aktuelle Datum und die Uhrzeit zurück, optional für eine IANA-Zeitzone und ein festes Ausgabeformat.",
		ParamHint:   "'now' oder arguments: {\"timezone\":\"Europe/Berlin\", \"format\":\"rfc3339|date|time|human\"}",
		InputSchema: &JSONSchema{
			Type: "object",
			Properties: map[string]JSONSchemaProperty{
				"timezone": {Type: "string", Description: "Optionale IANA-Zeitzone, z.B. Europe/Berlin"},
				"format":   {Type: "string", Description: "Optional: rfc3339, date, time oder human", Enum: []string{"rfc3339", "date", "time", "human"}},
			},
		},
	},
	{
		Name:        "tinygo",
		Description: "Alias für nanogo - interpretiert Go-Code direkt ohne Kompilierung. Sichere Sandbox-Umgebung für Go-Programme.",
		ParamHint:   "Go-Quelltext (z.B. 'package main; func main() { ... }')",
	},
}

// persona represents a user-selectable assistant persona with a
// pre-prompt that influences system behavior.
type persona struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// toolRequest is the structured marker the assistant can emit to
// request that the frontend run a specific tool with a query.
type toolRequest struct {
	Tool      string         `json:"tool"`
	Query     string         `json:"query,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

func shouldAutoExecuteTool(s appSettings, tr toolRequest, autoSearch bool) bool {
	if !canRoleUseTool(s.ActiveRole, tr.Tool) {
		return false
	}
	if s.UsageProfile == "commercial" {
		switch tr.Tool {
		case "nanogo", "exec_code", "shell", "tinygo":
			return false
		}
	}
	switch tr.Tool {
	case "calculate", "json_query", "text_diff", "regex_extract", "local_search", "rag_knowledge", "datetime", "vector_query", "sql_query":
		return true
	case "url_fetch":
		return autoSearch
	case "nanogo", "exec_code":
		return s.AllowNanoGo || s.AllowCodeExec
	case "shell":
		return s.AllowShellExec
	case "tinygo":
		return s.AllowTinyGo
	case "wikipedia", "duckduckgo", "wiktionary", "stackoverflow", "websearch", "news", "wikidata", "github":
		return autoSearch
	default:
		if strings.HasPrefix(tr.Tool, "module:") {
			return autoSearch
		}
		return autoSearch
	}
}

const (
	maxLocalToolInputRunes  = 6000
	maxLocalToolOutputRunes = 6000
	maxLocalDiffLines       = 200
	maxLocalRegexPattern    = 512
	maxLocalRegexMatches    = 50
)

// localToolStringArgument reads a string from a structured local-tool call.
// Text-diff inputs may intentionally be empty, whereas JSON and patterns may
// not. The legacy scalar query is retained only for json_query.
func localToolStringArgument(tr toolRequest, field string, maxRunes int, allowEmpty bool) (string, error) {
	var text string
	if value, ok := tr.Arguments[field]; ok {
		var isString bool
		text, isString = value.(string)
		if !isString {
			return "", fmt.Errorf("%s: argument %q must be a string", tr.Tool, field)
		}
	} else if field == "json" && len(tr.Arguments) == 0 {
		text = tr.Query
	} else {
		return "", fmt.Errorf("%s: missing argument %q", tr.Tool, field)
	}
	if !allowEmpty && strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s: argument %q must not be empty", tr.Tool, field)
	}
	if len([]rune(text)) > maxRunes {
		return "", fmt.Errorf("%s: argument %q exceeds %d characters", tr.Tool, field, maxRunes)
	}
	return text, nil
}

func localToolOptionalStringArgument(tr toolRequest, field string, maxRunes int) (string, error) {
	value, ok := tr.Arguments[field]
	if !ok {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s: argument %q must be a string", tr.Tool, field)
	}
	if len([]rune(text)) > maxRunes {
		return "", fmt.Errorf("%s: argument %q exceeds %d characters", tr.Tool, field, maxRunes)
	}
	return text, nil
}

func localToolLimitArgument(tr toolRequest, fallback, maximum int) (int, error) {
	value, ok := tr.Arguments["limit"]
	if !ok {
		return fallback, nil
	}
	var limit int
	switch n := value.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("%s: argument \"limit\" must be an integer", tr.Tool)
		}
		limit = int(n)
	case int:
		limit = n
	case int64:
		limit = int(n)
	default:
		return 0, fmt.Errorf("%s: argument \"limit\" must be an integer", tr.Tool)
	}
	if limit < 1 || limit > maximum {
		return 0, fmt.Errorf("%s: argument \"limit\" must be between 1 and %d", tr.Tool, maximum)
	}
	return limit, nil
}

func localToolLines(text string) ([]string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) <= maxLocalDiffLines {
		return lines, false
	}
	return lines[:maxLocalDiffLines], true
}

// localTextDiff computes a bounded line-oriented LCS diff. It intentionally
// emits only changes, keeping the result concise enough for an agent prompt.
func localTextDiff(before, after string) string {
	left, leftTruncated := localToolLines(before)
	right, rightTruncated := localToolLines(after)
	dp := make([][]int, len(left)+1)
	for i := range dp {
		dp[i] = make([]int, len(right)+1)
	}
	for i := len(left) - 1; i >= 0; i-- {
		for j := len(right) - 1; j >= 0; j-- {
			if left[i] == right[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var out strings.Builder
	out.WriteString("Zeilen-Diff:\n")
	changes := 0
	for i, j := 0, 0; i < len(left) || j < len(right); {
		switch {
		case i < len(left) && j < len(right) && left[i] == right[j]:
			i++
			j++
		case i < len(left) && (j == len(right) || dp[i+1][j] >= dp[i][j+1]):
			out.WriteString("- ")
			out.WriteString(left[i])
			out.WriteByte('\n')
			i++
			changes++
		case j < len(right):
			out.WriteString("+ ")
			out.WriteString(right[j])
			out.WriteByte('\n')
			j++
			changes++
		}
	}
	if changes == 0 {
		return "Keine Unterschiede."
	}
	if leftTruncated || rightTruncated {
		out.WriteString("[Vergleich auf die ersten 200 Zeilen je Text begrenzt]\n")
	}
	return truncate(strings.TrimSpace(out.String()), maxLocalToolOutputRunes)
}

// executeToolRequest is retained for explicitly initiated, non-streaming
// workflows. Agent runs use executeToolRequestCtx so cancellable dependencies
// receive the request context.
func executeToolRequest(tr toolRequest, s appSettings, rag *ragSystem, customAPIs *apiStore, modules *moduleStore, connectors *connectorStore, connectorExec *connectorExecutor) (string, string, error) {
	return executeToolRequestWithContext(context.Background(), tr, s, rag, customAPIs, modules, connectors, connectorExec)
}

func executeToolRequestWithContext(ctx context.Context, tr toolRequest, s appSettings, rag *ragSystem, customAPIs *apiStore, modules *moduleStore, connectors *connectorStore, connectorExec *connectorExecutor) (string, string, error) {
	var text string
	var source string
	var fetchErr error

	switch tr.Tool {
	case "rag_knowledge":
		// New canonical name for internal RAG search
		hits, err := rag.searchJSONContext(ctx, tr.Query, s.K)
		if err != nil {
			fetchErr = err
		} else if len(hits) == 0 {
			text = "Keine passenden lokalen Dokumente gefunden."
			source = "rag_knowledge:" + tr.Query
		} else {
			var sb strings.Builder
			for i, h := range hits {
				sb.WriteString(fmt.Sprintf("Treffer %d (Score %.2f):\n%s\n\n", i+1, h.Score, h.Content))
			}
			text = sb.String()
			source = "rag_knowledge:" + tr.Query
		}
	case "url_fetch":
		// Fetch a URL and return plain text (model sees content, not the raw HTTP)
		rawURL := strings.TrimSpace(tr.Query)
		if rawURL == "" {
			var err error
			rawURL, err = localToolStringArgument(tr, "url", maxLocalToolInputRunes, false)
			if err != nil {
				fetchErr = err
				break
			}
			rawURL = strings.TrimSpace(rawURL)
		}
		if rawURL == "" {
			fetchErr = fmt.Errorf("url_fetch: empty URL")
			break
		}
		fetched, err := fetchURLCtx(ctx, rawURL)
		if err != nil {
			fetchErr = fmt.Errorf("url_fetch: %w", err)
		} else {
			// Trim to a safe size to avoid huge prompts
			const maxURLFetchChars = 8000
			if len(fetched) > maxURLFetchChars {
				fetched = fetched[:maxURLFetchChars] + "\n[... Inhalt gekürzt ...]"
			}
			text = fetched
			source = "url_fetch:" + rawURL
		}
	case "local_search":
		hits, err := rag.searchJSONContext(ctx, tr.Query, s.K)
		if err != nil {
			fetchErr = err
		} else if len(hits) == 0 {
			text = "Keine passenden lokalen Dokumente gefunden."
			source = "rag_local:" + tr.Query
		} else {
			var sb strings.Builder
			for i, h := range hits {
				sb.WriteString(fmt.Sprintf("Treffer %d (Score %.2f):\n%s\n\n", i+1, h.Score, h.Content))
			}
			text = sb.String()
			source = "rag_local:" + tr.Query
		}
	case "vector_query":
		// Parse optional k:N and threshold:F prefixes from the query string
		k := s.K
		threshold := rag.scoreThreshold()
		rawQ := strings.TrimSpace(tr.Query)
		for {
			if rest, ok := strings.CutPrefix(rawQ, "k:"); ok {
				fields := strings.Fields(rest)
				if len(fields) > 0 {
					if n, err2 := strconv.Atoi(fields[0]); err2 == nil && n > 0 && n <= maxToolResultRows {
						k = n
						rawQ = strings.TrimSpace(rest[len(fields[0]):])
						continue
					}
				}
			}
			if rest, ok := strings.CutPrefix(rawQ, "threshold:"); ok {
				fields := strings.Fields(rest)
				if len(fields) > 0 {
					if f, err2 := strconv.ParseFloat(fields[0], 64); err2 == nil && f >= 0 && f <= 1 {
						threshold = f
						rawQ = strings.TrimSpace(rest[len(fields[0]):])
						continue
					}
				}
			}
			break
		}
		if rawQ == "" {
			rawQ = tr.Query
		}
		hits, err := rag.searchJSONContext(ctx, rawQ, k)
		if err != nil {
			fetchErr = err
		} else {
			var filtered []searchResult
			for _, h := range hits {
				// searchJSON returns neighbor-context chunks with Score=-1;
				// those are always included regardless of threshold because
				// they provide narrative context for a nearby primary hit.
				if h.Score < 0 || h.Score >= threshold {
					filtered = append(filtered, h)
				}
			}
			if len(filtered) == 0 {
				text = fmt.Sprintf("Keine Treffer über Schwellwert %.2f für: %s", threshold, rawQ)
				source = "vector_query:" + rawQ
			} else {
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("Vektor-Suche '%s' (k=%d, threshold=%.2f): %d Treffer\n\n", rawQ, k, threshold, len(filtered)))
				for i, h := range filtered {
					label := fmt.Sprintf("%.2f", h.Score)
					if h.Score < 0 {
						label = "Kontext" // neighbor chunk, no independent similarity score
					}
					sb.WriteString(fmt.Sprintf("[%d | Score: %s]\n%s\n\n", i+1, label, h.Content))
				}
				text = sb.String()
				source = "vector_query:" + rawQ
			}
		}
	case "sql_query":
		// Strip SQL block comments (/* … */) and line comments (-- …) before checking
		// to prevent bypass via /* INSERT */ or -- INSERT tricks.
		trimmed := stripSQLComments(strings.TrimSpace(tr.Query))
		upper := strings.ToUpper(trimmed)
		if !strings.HasPrefix(upper, "SELECT") {
			fetchErr = fmt.Errorf("sql_query: only SELECT statements are allowed")
			break
		}
		// Block DML/DDL and dangerous administrative keywords even inside SELECT
		// (e.g. subqueries, CTEs that wrap forbidden operations).
		forbidden := []string{
			"INSERT", "UPDATE", "DELETE", "DROP", "CREATE", "ALTER",
			"TRUNCATE", "REPLACE", "ATTACH", "DETACH", "PRAGMA",
		}
		for _, kw := range forbidden {
			// Use word-boundary check: keyword must be followed by whitespace or end
			if idx := strings.Index(upper, kw); idx >= 0 {
				// Verify it is a standalone keyword token (not a prefix of a longer word)
				after := idx + len(kw)
				isToken := after >= len(upper) || !isAlphaNumUnder(rune(upper[after]))
				if isToken {
					fetchErr = fmt.Errorf("sql_query: statement contains disallowed keyword %s", kw)
					break
				}
			}
		}
		if fetchErr != nil {
			break
		}
		rag.dbMu.Lock()
		rs, err := tinysql.ExecSQL(tinysql.WithAuditText(ctx, trimmed), rag.db, "default", trimmed)
		rag.dbMu.Unlock()
		if err != nil {
			fetchErr = fmt.Errorf("sql_query exec: %w", err)
			break
		}
		if rs == nil || len(rs.Rows) == 0 {
			text = "SQL-Abfrage ergab keine Zeilen."
			source = "sql_query"
			break
		}
		// Format result as a simple text table
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("SQL-Ergebnis (%d Zeilen):\n\n", len(rs.Rows)))
		for i, row := range rs.Rows {
			if i >= maxToolResultRows { // cap output for LLM context window safety
				sb.WriteString(fmt.Sprintf("… (%d weitere Zeilen abgeschnitten)\n", len(rs.Rows)-i))
				break
			}
			sb.WriteString(fmt.Sprintf("Zeile %d: %v\n", i+1, row))
		}
		text = sb.String()
		source = "sql_query"
	case "datetime":
		zone := ""
		if len(tr.Arguments) > 0 {
			var err error
			zone, err = localToolOptionalStringArgument(tr, "timezone", 128)
			if err != nil {
				fetchErr = err
				break
			}
		}
		if strings.TrimSpace(zone) == "" {
			legacy := strings.TrimSpace(tr.Query)
			if legacy != "" && !strings.EqualFold(legacy, "now") {
				zone = legacy
			}
		}
		format, err := localToolOptionalStringArgument(tr, "format", 32)
		if err != nil {
			fetchErr = err
			break
		}
		location := time.Local
		if strings.TrimSpace(zone) != "" {
			location, err = time.LoadLocation(strings.TrimSpace(zone))
			if err != nil {
				fetchErr = fmt.Errorf("datetime: invalid timezone %q: %w", zone, err)
				break
			}
		}
		now := time.Now().In(location)
		switch strings.ToLower(strings.TrimSpace(format)) {
		case "", "human":
			text = fmt.Sprintf("Aktuelles Datum und Uhrzeit (%s): %s", location, now.Format("2006-01-02 15:04:05 MST"))
		case "rfc3339":
			text = now.Format(time.RFC3339)
		case "date":
			text = now.Format("2006-01-02")
		case "time":
			text = now.Format("15:04:05 MST")
		default:
			fetchErr = fmt.Errorf("datetime: unsupported format %q", format)
		}
		source = "system:datetime:" + location.String()
	case "wikipedia":
		source = "wiki:" + tr.Query
		text, fetchErr = fetchWikipedia(tr.Query, s.Lang)
	case "duckduckgo":
		source = "ddg:" + tr.Query
		text, fetchErr = fetchDuckDuckGo(tr.Query)
	case "wiktionary":
		source = "wikt:" + tr.Query
		text, fetchErr = fetchWiktionary(tr.Query, s.Lang)
	case "stackoverflow":
		source = "so:" + tr.Query
		text, fetchErr = fetchStackOverflow(tr.Query)
	case "websearch":
		source = "web:" + tr.Query
		text, fetchErr = fetchMultiWebSearch(tr.Query)
	case "news":
		source = "news:" + tr.Query
		text, fetchErr = fetchDuckDuckGo(`news "` + tr.Query + `"`)
	case "wikidata":
		source = "wikidata:" + tr.Query
		text, fetchErr = fetchWikidata(tr.Query)
	case "github":
		source = "github:" + tr.Query
		text, fetchErr = fetchGitHub(tr.Query)
	case "llm":
		var buf bytes.Buffer
		msgs := []chatMsg{{Role: "user", Content: tr.Query}}
		if err := rag.getLM().chatStream(ctx, "", msgs, &buf); err != nil {
			return "", "", fmt.Errorf("LLM error: %w", err)
		}
		text = buf.String()
		source = "llm:prompt"
	case "calculate":
		out, err := execSmallR(tr.Query)
		if err != nil {
			fetchErr = err
		} else {
			text = out
			source = "calc:" + tr.Query
		}
	case "json_query":
		rawJSON, err := localToolStringArgument(tr, "json", maxLocalToolInputRunes, false)
		if err != nil {
			fetchErr = err
			break
		}
		path, err := localToolOptionalStringArgument(tr, "path", maxLocalRegexPattern)
		if err != nil {
			fetchErr = err
			break
		}
		path = strings.TrimSpace(path)
		var document any
		if err := json.Unmarshal([]byte(rawJSON), &document); err != nil {
			fetchErr = fmt.Errorf("json_query: invalid JSON: %w", err)
			break
		}
		value := dotGet(document, path)
		if value == nil && path != "" {
			text = fmt.Sprintf("JSON-Pfad nicht gefunden: %s", path)
		} else if encoded, err := json.MarshalIndent(value, "", "  "); err != nil {
			fetchErr = fmt.Errorf("json_query: encode result: %w", err)
		} else {
			text = truncate(string(encoded), maxLocalToolOutputRunes)
		}
		source = "json_query:local"
	case "text_diff":
		before, err := localToolStringArgument(tr, "before", maxLocalToolInputRunes, true)
		if err != nil {
			fetchErr = err
			break
		}
		after, err := localToolStringArgument(tr, "after", maxLocalToolInputRunes, true)
		if err != nil {
			fetchErr = err
			break
		}
		text = localTextDiff(before, after)
		source = "text_diff:local"
	case "regex_extract":
		pattern, err := localToolStringArgument(tr, "pattern", maxLocalRegexPattern, false)
		if err != nil {
			fetchErr = err
			break
		}
		input, err := localToolStringArgument(tr, "text", maxLocalToolInputRunes, true)
		if err != nil {
			fetchErr = err
			break
		}
		limit, err := localToolLimitArgument(tr, 20, maxLocalRegexMatches)
		if err != nil {
			fetchErr = err
			break
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			fetchErr = fmt.Errorf("regex_extract: invalid pattern: %w", err)
			break
		}
		type regexMatch struct {
			Match  string   `json:"match"`
			Groups []string `json:"groups,omitempty"`
		}
		rawMatches := re.FindAllStringSubmatch(input, limit)
		matches := make([]regexMatch, 0, len(rawMatches))
		for _, match := range rawMatches {
			if len(match) == 0 {
				continue
			}
			matches = append(matches, regexMatch{Match: match[0], Groups: match[1:]})
		}
		if len(matches) == 0 {
			text = "Keine Treffer."
		} else if encoded, err := json.MarshalIndent(matches, "", "  "); err != nil {
			fetchErr = fmt.Errorf("regex_extract: encode result: %w", err)
		} else {
			text = truncate(string(encoded), maxLocalToolOutputRunes)
		}
		source = "regex_extract:local"
	case "nanogo", "exec_code":
		timeout := 5 * time.Second
		out, err := RunSafe(tr.Query, timeout)
		if err != nil {
			fetchErr = err
		} else {
			text = out
			if tr.Tool == "exec_code" {
				source = "code:exec"
			} else {
				source = "nanogo:exec"
			}
		}
	case "shell":
		if !s.AllowShellExec {
			fetchErr = fmt.Errorf("shell execution disabled in settings")
		} else {
			text, fetchErr = execShellCommand(tr.Query)
			if fetchErr == nil {
				source = "shell:exec"
			}
		}
	case "tinygo":
		if !s.AllowTinyGo {
			fetchErr = fmt.Errorf("tinygo execution disabled in settings")
		} else {
			text, fetchErr = execTinyGoProgram(tr.Query)
			if fetchErr == nil {
				source = "tinygo:exec"
			}
		}
	default:
		// Connector capability execution path (schema-validated).
		if connectors != nil && connectorExec != nil {
			if entry, ok := connectors.registryEntry(tr.Tool); ok {
				input := make(map[string]any, len(tr.Arguments))
				for key, value := range tr.Arguments {
					input[key] = value
				}
				rawQuery := strings.TrimSpace(tr.Query)
				if len(input) == 0 && rawQuery != "" {
					if err := json.Unmarshal([]byte(rawQuery), &input); err != nil {
						// Fallback: map scalar query into the first required field.
						if len(entry.Capability.InputSchema.Required) == 1 {
							input[entry.Capability.InputSchema.Required[0]] = rawQuery
						} else {
							fetchErr = fmt.Errorf("connector tool %s requires JSON input matching schema", tr.Tool)
							break
						}
					}
				}
				execRes, err := connectorExec.ExecuteContext(ctx, ConnectorExecRequest{
					ConnectorID: entry.Connector.ID,
					Capability:  entry.Capability.Name,
					Input:       input,
				})
				if err != nil {
					fetchErr = err
					break
				}
				source = execRes.Source
				if source == "" {
					source = "connector:" + entry.Connector.ID + ":" + tr.Tool
				}
				var out strings.Builder
				if len(execRes.Output) > 0 {
					out.WriteString("Connector output:\n")
					j, _ := json.MarshalIndent(execRes.Output, "", "  ")
					out.Write(j)
					out.WriteString("\n")
				}
				text = strings.TrimSpace(out.String())
				if text == "" {
					text = "connector call completed with no mapped output"
				}
				break
			}
		}
		if strings.HasPrefix(tr.Tool, "module:") && modules != nil {
			modID := strings.TrimPrefix(tr.Tool, "module:")
			mod, ok := modules.get(modID)
			if !ok {
				fetchErr = fmt.Errorf("unknown module: %s", modID)
				break
			}
			if !mod.Enabled {
				fetchErr = fmt.Errorf("module %s is disabled", modID)
				break
			}
			action := "query"
			limit := 0
			arg := tr.Query
			if mod.Kind == "mail" {
				action = "query"
				limit = parseIntString(tr.Query, 0)
				arg = ""
			} else if mod.Kind == "http-folder" {
				action = "ingest"
			}
			res, err := executeModuleRun(mod, rag, settings.get().EmbedModel, action, arg, limit, true)
			if err != nil {
				fetchErr = err
				break
			}
			text = res.Text
			source = res.Source
			if source == "" {
				source = "module:" + mod.ID
			}
			break
		}
		if api, ok := customAPIs.get(tr.Tool); ok {
			finalURL := strings.ReplaceAll(api.Template, "$q", url.QueryEscape(tr.Query))
			source = "api:" + api.Name + ":" + tr.Query
			text, fetchErr = fetchURLCtx(ctx, finalURL)
		} else {
			fetchErr = fmt.Errorf("unknown tool: %s", tr.Tool)
		}
	}

	return text, source, fetchErr
}

// executeToolRequestCtx passes cancellation to context-aware executors and
// checks it before and after every call. This bounds the agent-visible result
// even for legacy helpers that do not yet accept a context directly.
func executeToolRequestCtx(ctx context.Context, tr toolRequest, s appSettings, rag *ragSystem, customAPIs *apiStore, modules *moduleStore, connectors *connectorStore, connectorExec *connectorExecutor) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", fmt.Errorf("tool %s: %w", tr.Tool, err)
	}
	text, source, err := executeToolRequestWithContext(ctx, tr, s, rag, customAPIs, modules, connectors, connectorExec)
	if err != nil {
		return text, source, err
	}
	// Check whether the context expired during execution.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", fmt.Errorf("tool %s timed out: %w", tr.Tool, ctxErr)
	}
	return text, source, nil
}

func filterToolsForRole(tools []toolDef, role string) []toolDef {
	out := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		if canRoleUseTool(role, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// ── Custom API store (persisted through settingsStore) ──────────────

// customAPI models a user-added external API template persisted in settings.
type customAPI struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Template string `json:"template"` // URL with $q placeholder
	Desc     string `json:"desc"`
}

// apiStore manages the set of persisted custom APIs through settings.
type apiStore struct {
	mu       sync.Mutex
	settings *settingsStore
}

// newAPIStore creates an apiStore backed by `settings`.
func newAPIStore(settings *settingsStore) *apiStore {
	return &apiStore{settings: settings}
}

// list returns a copy of configured custom APIs.
func (s *apiStore) list() []customAPI {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	out := make([]customAPI, len(s.settings.s.CustomAPIs))
	copy(out, s.settings.s.CustomAPIs)
	return out
}

// add registers a new custom API template and persists settings.
func (s *apiStore) add(name, template, desc string) (customAPI, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	api := customAPI{
		ID:       fmt.Sprintf("api-%d", time.Now().UnixNano()),
		Name:     name,
		Template: template,
		Desc:     desc,
	}
	s.settings.s.CustomAPIs = append(s.settings.s.CustomAPIs, api)
	if err := s.settings.saveLocked(); err != nil {
		return customAPI{}, err
	}
	return api, nil
}

// remove deletes a custom API by id and persists the change.
func (s *apiStore) remove(id string) (bool, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	apis := s.settings.s.CustomAPIs
	for i, a := range apis {
		if a.ID == id {
			s.settings.s.CustomAPIs = append(apis[:i], apis[i+1:]...)
			return true, s.settings.saveLocked()
		}
	}
	return false, nil
}

// ── Persona store (persisted through settingsStore) ───────────────

// personaStore manages persisted personas stored inside settings.
type personaStore struct {
	mu       sync.Mutex
	settings *settingsStore
}

// newPersonaStore constructs a personaStore backed by `settings`.
func newPersonaStore(settings *settingsStore) *personaStore {
	return &personaStore{settings: settings}
}

// list returns a copy of all configured personas.
func (p *personaStore) list() []persona {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	out := make([]persona, len(p.settings.s.Personas))
	copy(out, p.settings.s.Personas)
	return out
}

// defaultID returns the ID of the first persona or an empty string.
func (p *personaStore) defaultID() string {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	if len(p.settings.s.Personas) == 0 {
		return ""
	}
	return p.settings.s.Personas[0].ID
}

// get retrieves a persona by id.
func (p *personaStore) get(id string) (persona, bool) {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	for _, per := range p.settings.s.Personas {
		if per.ID == id {
			return per, true
		}
	}
	return persona{}, false
}

// add creates and persists a new persona with the given name and prompt.
func (p *personaStore) add(name, prompt string) (persona, error) {
	name = strings.TrimSpace(name)
	prompt = strings.TrimSpace(prompt)
	if name == "" {
		return persona{}, fmt.Errorf("name required")
	}
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	per := persona{
		ID:     fmt.Sprintf("persona-%d", time.Now().UnixNano()),
		Name:   name,
		Prompt: prompt,
	}
	p.settings.s.Personas = append(p.settings.s.Personas, per)
	return per, p.settings.saveLocked()
}

// remove deletes a persona by id and persists the change.
func (p *personaStore) remove(id string) (bool, error) {
	p.settings.mu.Lock()
	defer p.settings.mu.Unlock()
	list := p.settings.s.Personas
	for i, per := range list {
		if per.ID == id {
			p.settings.s.Personas = append(list[:i], list[i+1:]...)
			return true, p.settings.saveLocked()
		}
	}
	return false, nil
}

type adminUserStore struct {
	settings *settingsStore
}

func newAdminUserStore(settings *settingsStore) *adminUserStore {
	return &adminUserStore{settings: settings}
}

func sanitizeAdminUser(user adminAPIUser) adminAPIUser {
	user.APIKeyHash = ""
	return user
}

func (s *adminUserStore) list() []adminAPIUser {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	out := make([]adminAPIUser, 0, len(s.settings.s.APIUsers))
	for _, user := range s.settings.s.APIUsers {
		out = append(out, sanitizeAdminUser(user))
	}
	return out
}

func (s *adminUserStore) create(name, role string) (adminAPIUser, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return adminAPIUser{}, "", fmt.Errorf("name required")
	}
	token, err := generateAPIToken()
	if err != nil {
		return adminAPIUser{}, "", err
	}
	user := adminAPIUser{
		ID:          fmt.Sprintf("api-user-%d", time.Now().UnixNano()),
		Name:        name,
		Role:        normalizeDemoRole(role),
		Enabled:     true,
		APIKeyHash:  hashAPIToken(token),
		APIKeyLast4: token[max(len(token)-4, 0):],
		CreatedAt:   time.Now().Format(time.RFC3339),
	}
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	s.settings.s.APIUsers = append(s.settings.s.APIUsers, user)
	if err := s.settings.saveLocked(); err != nil {
		return adminAPIUser{}, "", err
	}
	return sanitizeAdminUser(user), token, nil
}

func (s *adminUserStore) update(id, name, role string, enabled bool) (adminAPIUser, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for i, user := range s.settings.s.APIUsers {
		if user.ID != id {
			continue
		}
		if strings.TrimSpace(name) != "" {
			user.Name = strings.TrimSpace(name)
		}
		user.Role = normalizeDemoRole(role)
		user.Enabled = enabled
		s.settings.s.APIUsers[i] = user
		if err := s.settings.saveLocked(); err != nil {
			return adminAPIUser{}, err
		}
		return sanitizeAdminUser(user), nil
	}
	return adminAPIUser{}, fmt.Errorf("user not found")
}

func (s *adminUserStore) regenerateKey(id string) (adminAPIUser, string, error) {
	token, err := generateAPIToken()
	if err != nil {
		return adminAPIUser{}, "", err
	}
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for i, user := range s.settings.s.APIUsers {
		if user.ID != id {
			continue
		}
		user.APIKeyHash = hashAPIToken(token)
		user.APIKeyLast4 = token[max(len(token)-4, 0):]
		s.settings.s.APIUsers[i] = user
		if err := s.settings.saveLocked(); err != nil {
			return adminAPIUser{}, "", err
		}
		return sanitizeAdminUser(user), token, nil
	}
	return adminAPIUser{}, "", fmt.Errorf("user not found")
}

func (s *adminUserStore) remove(id string) (bool, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	list := s.settings.s.APIUsers
	for i, user := range list {
		if user.ID == id {
			s.settings.s.APIUsers = append(list[:i], list[i+1:]...)
			return true, s.settings.saveLocked()
		}
	}
	return false, nil
}

type apiRouteStore struct {
	settings *settingsStore
}

func newAPIRouteStore(settings *settingsStore) *apiRouteStore {
	return &apiRouteStore{settings: settings}
}

func (s *apiRouteStore) list() []apiRouteRule {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	out := make([]apiRouteRule, len(s.settings.s.APIRoutes))
	copy(out, s.settings.s.APIRoutes)
	return out
}

func (s *apiRouteStore) update(path string, enabled, public bool) (apiRouteRule, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for i, rule := range s.settings.s.APIRoutes {
		if rule.Path != path {
			continue
		}
		rule.Enabled = enabled
		rule.Public = public
		s.settings.s.APIRoutes[i] = rule
		if err := s.settings.saveLocked(); err != nil {
			return apiRouteRule{}, err
		}
		return rule, nil
	}
	return apiRouteRule{}, fmt.Errorf("route not found")
}

// get returns a customAPI by id if it exists.
func (s *apiStore) get(id string) (customAPI, bool) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for _, a := range s.settings.s.CustomAPIs {
		if a.ID == id {
			return a, true
		}
	}
	return customAPI{}, false
}

// allTools returns the union of builtin tools and persisted custom APIs.
func (s *apiStore) allTools() []toolDef {
	all := make([]toolDef, len(builtinTools))
	copy(all, builtinTools)

	if s == nil || s.settings == nil {
		return all
	}
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()

	for _, a := range s.settings.s.CustomAPIs {
		desc := a.Desc
		if desc == "" {
			desc = "Custom API: " + a.Template
		}
		all = append(all, toolDef{
			Name:        a.ID,
			Description: desc,
			ParamHint:   "Suchbegriff (wird in $q eingesetzt)",
		})
	}
	return all
}

func extractToolRequest(text string) (toolRequest, bool) {
	start := strings.Index(text, "[TOOL_REQUEST]")
	if start == -1 {
		return toolRequest{}, false
	}
	end := strings.Index(text[start:], "[/TOOL_REQUEST]")
	var body string
	if end == -1 {
		body = strings.TrimSpace(text[start+len("[TOOL_REQUEST]"):])
	} else {
		body = strings.TrimSpace(text[start+len("[TOOL_REQUEST]") : start+end])
	}
	var tr toolRequest
	if err := json.Unmarshal([]byte(body), &tr); err != nil {
		return toolRequest{}, false
	}
	if strings.TrimSpace(tr.Tool) == "" || (strings.TrimSpace(tr.Query) == "" && len(tr.Arguments) == 0) {
		return toolRequest{}, false
	}
	tr.Tool = strings.TrimSpace(tr.Tool)
	tr.Query = strings.TrimSpace(tr.Query)
	return tr, true
}

func stripToolRequest(text string) string {
	start := strings.Index(text, "[TOOL_REQUEST]")
	if start == -1 {
		return strings.TrimSpace(text)
	}
	end := strings.Index(text[start:], "[/TOOL_REQUEST]")
	if end == -1 {
		return strings.TrimSpace(text[:start])
	}
	end += start + len("[/TOOL_REQUEST]")
	return strings.TrimSpace(text[:start] + text[end:])
}
