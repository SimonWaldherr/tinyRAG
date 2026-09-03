package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

// ─────────────────────────────────────────────────────────────────────────────
// Live, read-only SQL Server access exposed to the chat model as an
// OpenAI-style tool/function call (see llm.go's chatWithTools) — the one
// connector in this file that isn't an import source. Nothing here is ever
// embedded or stored: each query runs fresh against the live database
// exactly when the model asks for one, and only the result of that one
// query is fed back into the conversation.
//
// Safety model, in order of how much it's actually trusted:
//  1. The database login configured in mssqlConfig should itself only have
//     SELECT permission on whatever it's allowed to expose. This is the
//     real boundary — nothing below substitutes for it.
//  2. validateSelectOnly is a best-effort statement blocklist (single
//     SELECT/CTE, no semicolon-chained second statement, no
//     DML/DDL/EXEC/stored-procedure keywords), not a full SQL parser. It
//     exists so a model that's confused about read-only intent gets an
//     immediate, clear rejection instead of a surprising side effect, not
//     to withstand a deliberately adversarial query.
//  3. MaxRows/TimeoutSeconds bound the blast radius of even an allowed
//     query (an unfiltered SELECT * over a huge table can't flood the
//     model's context or hang the request indefinitely).
// ─────────────────────────────────────────────────────────────────────────────

// mssqlDSN builds a sqlserver:// connection string from cfg, applying the
// default port (1433) and requiring both a resolved password and username
// to be present before attempting a connection.
func mssqlDSN(cfg mssqlConfig) (string, error) {
	if cfg.Host == "" || cfg.Database == "" {
		return "", fmt.Errorf("mssql: host/database not configured")
	}
	password := cfg.resolvedPassword()
	if cfg.Username == "" || password == "" {
		return "", fmt.Errorf("mssql: username/password not configured (set password or password_env)")
	}
	port := cfg.Port
	if port <= 0 {
		port = 1433
	}
	q := url.Values{}
	q.Set("database", cfg.Database)
	q.Set("TrustServerCertificate", strconv.FormatBool(cfg.TrustServerCertificate))
	u := url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(cfg.Username, password),
		Host:     fmt.Sprintf("%s:%d", cfg.Host, port),
		RawQuery: q.Encode(),
	}
	return u.String(), nil
}

var (
	// mssqlForbiddenRe matches statement-changing keywords and stored-
	// procedure prefixes as whole words/tokens, case-insensitively —
	// covers both explicit DML/DDL and the sp_/xp_ stored-procedure
	// namespaces (xp_cmdshell being the canonical "this is not a SELECT"
	// example).
	mssqlForbiddenRe = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|DROP|ALTER|CREATE|TRUNCATE|MERGE|GRANT|REVOKE|EXEC|EXECUTE|INTO|WAITFOR|OPENROWSET|OPENDATASOURCE|BULK|BACKUP|RESTORE|DBCC|KILL|SET|USE|sp_\w*|xp_\w*)\b`)
	mssqlLeadingRe   = regexp.MustCompile(`(?is)^\s*(--[^\n]*\n\s*)*(SELECT|WITH)\b`)
)

// validateSelectOnly rejects anything that isn't a single read-only
// SELECT (or a WITH ... SELECT CTE) statement. See the package doc comment
// above for what this is (and isn't) a defense against.
func validateSelectOnly(query string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return fmt.Errorf("empty query")
	}
	// Allow exactly one optional trailing semicolon; anything else
	// containing ';' is a second statement.
	// Ignore comments and quoted literals while inspecting syntax. This
	// avoids rejecting a harmless product name such as 'DELETE filter',
	// while still rejecting keywords that are actually executable SQL.
	code := strings.TrimSpace(sqlCodeOnly(q))
	trimmed := strings.TrimSpace(strings.TrimSuffix(code, ";"))
	if strings.Contains(trimmed, ";") {
		return fmt.Errorf("only a single statement is allowed")
	}
	if !mssqlLeadingRe.MatchString(trimmed) {
		return fmt.Errorf("only SELECT statements are allowed")
	}
	if m := mssqlForbiddenRe.FindString(trimmed); m != "" {
		return fmt.Errorf("query contains a disallowed keyword: %s", m)
	}
	return nil
}

// sqlCodeOnly replaces comments and quoted literals with spaces while
// preserving statement punctuation. It is intentionally not a SQL parser;
// it only gives validateSelectOnly a conservative view of executable tokens.
func sqlCodeOnly(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			b.WriteString("  ")
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				b.WriteByte(' ')
				i++
			}
			if i+1 < len(s) {
				b.WriteString("  ")
				i += 2
			}
			continue
		}
		if s[i] == '\'' {
			b.WriteByte(' ')
			i++
			for i < len(s) {
				if s[i] == '\'' {
					b.WriteByte(' ')
					i++
					if i < len(s) && s[i] == '\'' {
						b.WriteByte(' ')
						i++
						continue
					}
					break
				}
				b.WriteByte(' ')
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// runMSSQLQuery executes a validated read-only query with no parameters
// and renders the result — see runMSSQLQueryArgs, which this is a
// convenience wrapper around for the generic query_mssql tool (below),
// the only caller that never has parameters to bind.
func runMSSQLQuery(ctx context.Context, cfg mssqlConfig, query string) (string, error) {
	return runMSSQLQueryArgs(ctx, cfg, query, nil)
}

// runMSSQLQueryArgs executes a validated read-only query and renders the
// result as a compact pipe-delimited text table, capped at cfg.MaxRows —
// a shape any chat model can read directly without needing JSON parsing
// instructions in the prompt. args, if non-empty, are bound via the
// driver's native parameter support (sql.Named, see
// mssqlTemplateToolExecutor) — query text is never built by
// string-concatenating a value into it, whether the query came from the
// model directly (query_mssql) or from an admin-authored template.
func runMSSQLQueryArgs(ctx context.Context, cfg mssqlConfig, query string, args []any) (string, error) {
	if err := validateSelectOnly(query); err != nil {
		return "", err
	}
	dsn, err := mssqlDSN(cfg)
	if err != nil {
		return "", err
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return "", fmt.Errorf("mssql: open: %w", err)
	}
	defer db.Close()

	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = 10
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("mssql: query failed: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("mssql: read columns: %w", err)
	}

	maxRows := cfg.MaxRows
	if maxRows <= 0 {
		maxRows = 200
	}
	masked := maskedColumnSet(cfg.MaskColumns, cols)

	var b strings.Builder
	b.WriteString(strings.Join(cols, " | "))
	b.WriteByte('\n')

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	n := 0
	truncated := false
	for rows.Next() {
		if n >= maxRows {
			truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("mssql: scan row %d: %w", n, err)
		}
		cells := make([]string, len(cols))
		for i, v := range vals {
			if masked[i] {
				cells[i] = "•••"
				continue
			}
			cells[i] = formatSQLValue(v)
		}
		b.WriteString(strings.Join(cells, " | "))
		b.WriteByte('\n')
		n++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("mssql: read rows: %w", err)
	}
	if n == 0 {
		return "(query returned no rows)", nil
	}
	if truncated {
		fmt.Fprintf(&b, "(truncated at %d rows — narrow the query with WHERE/TOP for a complete result)\n", maxRows)
	}
	return b.String(), nil
}

// maskedColumnSet resolves cfg.MaskColumns (case-insensitive column names)
// against this specific query's actual result columns, returning a
// per-index bool so the row loop can check masked[i] without repeating the
// string comparison for every single row — the name list is small and
// admin-authored, so a linear scan per column here (once per query, not
// once per row) is simpler than building a lookup map for what's normally
// a handful of names.
func maskedColumnSet(maskColumns, cols []string) []bool {
	masked := make([]bool, len(cols))
	if len(maskColumns) == 0 {
		return masked
	}
	for i, col := range cols {
		for _, m := range maskColumns {
			if strings.EqualFold(col, m) {
				masked[i] = true
				break
			}
		}
	}
	return masked
}

// formatSQLValue renders one scanned column value for the pipe-delimited
// result table, decoding []byte (how the driver returns many non-numeric
// types) as text rather than a Go byte-slice literal.
func formatSQLValue(v any) string {
	if v == nil {
		return "NULL"
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// mssqlToolName is the function name advertised to the model and matched
// against incoming tool_calls (see llm.go's chatWithTools).
const mssqlToolName = "query_mssql"

// mssqlToolDef describes the query_mssql tool in OpenAI's function-calling
// schema shape.
func mssqlToolDef(cfg mssqlConfig) toolDef {
	desc := fmt.Sprintf(
		"Run a single read-only SQL SELECT query against the %q SQL Server database and return the result as a text table. "+
			"Use this for live, current data the knowledge base wouldn't have (today's stock level, an order's current status, a running total) — "+
			"NOT for anything already answerable from search_knowledge_base, and NOT for writes of any kind. "+
			"Only SELECT (or WITH ... SELECT) statements are allowed — no INSERT/UPDATE/DELETE/DDL/stored procedures; a write attempt is rejected before it reaches the database. "+
			"Results are capped at %d rows — add your own TOP/WHERE/ORDER BY to narrow down large tables instead of relying on the cap. "+
			"Example: SELECT TOP 20 OrderNumber, Status, OrderDate FROM Orders WHERE CustomerID = 4711 ORDER BY OrderDate DESC.",
		cfg.Database, cfg.MaxRows)
	if len(cfg.MaskColumns) > 0 {
		desc += fmt.Sprintf(" These columns are always masked as \"•••\" in the result, regardless of query: %s — don't bother filtering on or selecting them specifically, the value is never revealed.", strings.Join(cfg.MaskColumns, ", "))
	}
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name:        mssqlToolName,
			Description: desc,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "A single T-SQL SELECT statement, e.g. \"SELECT TOP 10 Name, Stock FROM Products WHERE Category = 'Handschuhe'\". Must start with SELECT or WITH — never INSERT/UPDATE/DELETE/EXEC/DDL.",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// mssqlToolArgs is the shape of the JSON arguments the model sends for a
// query_mssql tool call.
type mssqlToolArgs struct {
	Query string `json:"query"`
}

// mssqlToolExecutor adapts runMSSQLQuery to the generic toolExecutor shape
// (llm.go's chatWithTools) — decode the model's JSON arguments, run the
// query against cfg, and hand the rendered result text back.
func mssqlToolExecutor(cfg mssqlConfig) toolExecutor {
	return func(ctx context.Context, argsJSON string) (string, error) {
		var args mssqlToolArgs
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
		return runMSSQLQuery(ctx, cfg, args.Query)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Admin-curated query templates — the MCP-style alternative to the
// generic query_mssql tool above (see mssqlConfig.QueryTemplates' doc
// comment in settings.go). Each enabled template becomes its own named
// tool with a typed parameter schema; the model can only ever run the
// exact SQL text an admin wrote, with model-supplied values bound as SQL
// parameters (never string-concatenated into the query).
// ─────────────────────────────────────────────────────────────────────────────

// sqlTemplateNameRe restricts template names to safe function-name-like
// identifiers — they become literal tool names advertised to the model
// (like mssqlToolName), so anything a JSON-schema/function-calling API
// would reject is rejected here first, at save time.
var sqlTemplateNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// sqlTemplateParamRefRe finds "{name}"-style placeholders in a template's
// SQL text, used by validateSQLQueryTemplates to check that every
// declared parameter is actually referenced (and, implicitly, that the
// SQL doesn't reference an undeclared one — see the reverse check
// there). Same placeholder syntax as http_tool.go's
// httpTemplatePlaceholderRe — SQL and HTTP templates used to differ
// ("@name" vs "{name}"); unified onto "{name}" (RFC 6570's URI Template
// convention) since "{"/"}" can never appear in a literal SQL identifier
// or URL, making a placeholder unambiguous at a glance in either. This is
// purely an authoring-syntax choice: mssqlTemplateToolExecutor still
// rewrites "{name}" to a REAL "@name" sql.Named bind reference immediately
// before execution (rewriteSQLTemplatePlaceholders) — values are still
// never string-concatenated into the query text, exactly as before this
// changed. migrateLegacySQLTemplateParamSyntax upgrades a settings.json
// predating this change on load, so an existing admin-authored template
// keeps working without manual edits.
var sqlTemplateParamRefRe = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// rewriteSQLTemplatePlaceholders converts every "{name}" placeholder in sql
// to the real T-SQL bind-parameter syntax "@name" the driver expects —
// called once, at execution time (mssqlTemplateToolExecutor), on the
// admin-authored template text only. This substitutes a fixed, already
// name-validated identifier into the query STRING (never a model/user-
// supplied value), so it carries none of the risk string-concatenating an
// actual value would — the values themselves are still bound separately
// via sql.Named, exactly as before {name} replaced @name as the authoring
// syntax.
func rewriteSQLTemplatePlaceholders(sql string) string {
	return sqlTemplateParamRefRe.ReplaceAllString(sql, "@$1")
}

// migrateLegacySQLTemplateParamSyntax rewrites every sqlQueryTemplate's SQL
// text in place, from the pre-unification "@name" bind-parameter syntax to
// the current "{name}" syntax shared with HTTP templates — called once,
// right after unmarshaling settings.json (loadOrCreateSettings), so an
// existing admin-authored template written before this migration keeps
// working without the admin having to edit anything. Scoped to each
// template's own declared Parameters (never a blanket "@word" rewrite
// across the whole SQL text): validateSQLQueryTemplates already rejects
// any saved template where a declared parameter's "@name" appears only
// inside a string literal (e.g. an email address), so every "@name"
// occurrence for an actually-declared parameter in already-persisted,
// already-validated SQL is guaranteed to be a real bind reference, never
// coincidental literal text — nothing here can corrupt an unrelated "@" a
// template's SQL happens to contain.
func migrateLegacySQLTemplateParamSyntax(templates []sqlQueryTemplate) {
	for i := range templates {
		for _, p := range templates[i].Parameters {
			name := strings.TrimSpace(p.Name)
			if name == "" {
				continue
			}
			re := regexp.MustCompile(`@` + regexp.QuoteMeta(name) + `\b`)
			templates[i].SQL = re.ReplaceAllString(templates[i].SQL, "{"+name+"}")
		}
	}
}

// validateSQLQueryTemplates rejects a template set with a name collision,
// an invalid/duplicate parameter, a non-SELECT statement, or a parameter
// that isn't actually referenced in its own SQL (or vice versa) —
// checked once at save time (see handlers.go's handleSettings) rather
// than at every chat request, so a mistake is caught immediately instead
// of silently exposing a broken or dangerous tool to every future
// question.
func validateSQLQueryTemplates(templates []sqlQueryTemplate) error {
	seen := map[string]bool{}
	for i, t := range templates {
		name := strings.TrimSpace(t.Name)
		if !sqlTemplateNameRe.MatchString(name) {
			return fmt.Errorf("template %d: name %q must match %s (letters/digits/underscore, starting with a letter or underscore)", i, t.Name, sqlTemplateNameRe.String())
		}
		if name == mssqlToolName {
			return fmt.Errorf("template %d: name %q collides with the built-in %s tool", i, name, mssqlToolName)
		}
		if seen[name] {
			return fmt.Errorf("template %d: duplicate name %q", i, name)
		}
		seen[name] = true

		if err := validateSelectOnly(t.SQL); err != nil {
			return fmt.Errorf("template %q: %w", name, err)
		}

		// Literal-only occurrences of "{name}" (e.g. "LIKE '%{kunde}%'")
		// never actually bind: mssqlTemplateToolExecutor rewrites {name} to
		// a real @name sql.Named parameter before execution, but SQL
		// Server's parser treats text inside single quotes as a literal,
		// never re-interpreting an "@name" that happens to appear there
		// (post-rewrite) as a parameter reference — so the query always
		// matches the literal characters "{kunde}"/"@kunde", regardless of
		// the value actually supplied, and silently returns no rows for
		// every input. sqlCodeOnly (used by validateSelectOnly above)
		// already blanks out string/comment contents while preserving
		// everything else, so a "{name}" that survives there is a real,
		// bindable reference; one that doesn't was inside a literal.
		// Caught here, at save time, rather than left to look like a data
		// problem on every future query through this template.
		sqlOnly := sqlCodeOnly(t.SQL)

		declared := map[string]bool{}
		for j, p := range t.Parameters {
			pname := strings.TrimSpace(p.Name)
			if pname == "" {
				return fmt.Errorf("template %q, parameter %d: empty name", name, j)
			}
			if declared[pname] {
				return fmt.Errorf("template %q: duplicate parameter %q", name, pname)
			}
			declared[pname] = true
			switch p.Type {
			case "string", "integer", "number", "boolean", "date":
			default:
				return fmt.Errorf("template %q, parameter %q: unknown type %q (want string|integer|number|boolean|date)", name, pname, p.Type)
			}
			if err := validateTemplateParamOptions(p.Options); err != nil {
				return fmt.Errorf("template %q, parameter %q: %w", name, pname, err)
			}
			if !strings.Contains(t.SQL, "{"+pname+"}") {
				return fmt.Errorf("template %q: parameter %q is declared but never referenced in SQL as {%s}", name, pname, pname)
			}
			if !strings.Contains(sqlOnly, "{"+pname+"}") {
				return fmt.Errorf("template %q: parameter %q only appears inside a string literal (e.g. '%%{%s}%%') — SQL Server never substitutes a parameter there, so the query would always return no rows regardless of the value supplied; use string concatenation instead, e.g. '%%' + {%s} + '%%'", name, pname, pname, pname)
			}
		}
		for _, ref := range sqlTemplateParamRefRe.FindAllStringSubmatch(t.SQL, -1) {
			if !declared[ref[1]] {
				return fmt.Errorf("template %q: SQL references {%s}, which isn't declared as a parameter", name, ref[1])
			}
		}
	}
	return nil
}

// jsonSchemaTypeFor maps a sqlQueryParam.Type onto the corresponding
// JSON-schema primitive — everything except integer/number/boolean
// degrades to "string" (there's no native JSON-schema date type; "date"
// params are described in their Description instead and passed through
// as text, which SQL Server parses on the server side).
func jsonSchemaTypeFor(paramType string) string {
	switch paramType {
	case "integer", "number", "boolean":
		return paramType
	default:
		return "string"
	}
}

// mssqlTemplateToolDef describes one query template as an OpenAI-style
// function-calling tool — same shape as mssqlToolDef, but with a
// parameter schema derived from the template instead of a single free-
// form "query" string.
func mssqlTemplateToolDef(t sqlQueryTemplate) toolDef {
	desc, props, required := queryTemplateToolSchema(t.Description, t.Parameters, t.ResultHint)
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name:        t.Name,
			Description: desc,
			Parameters: map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

// convertSQLTemplateParam decodes one JSON argument value according to
// paramType, so it's bound into the query as the right SQL type (e.g. an
// integer parameter never ends up bound as its string representation).
func convertSQLTemplateParam(paramType string, raw json.RawMessage) (any, error) {
	switch paramType {
	case "integer":
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("expected an integer: %w", err)
		}
		return n, nil
	case "number":
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("expected a number: %w", err)
		}
		return n, nil
	case "boolean":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("expected a boolean: %w", err)
		}
		return b, nil
	default: // "string", "date" — SQL Server parses an ISO date string bound as text
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("expected a string: %w", err)
		}
		return s, nil
	}
}

// mssqlTemplateToolExecutor adapts one sqlQueryTemplate to the generic
// toolExecutor shape: decode the model's JSON arguments, convert/bind
// each declared parameter via sql.Named (never string-concatenated into
// the SQL text), rewrite the admin-authored "{name}" placeholders to real
// "@name" bind references (rewriteSQLTemplatePlaceholders), and run the
// resulting query against cfg.
func mssqlTemplateToolExecutor(cfg mssqlConfig, tmpl sqlQueryTemplate) toolExecutor {
	return func(ctx context.Context, argsJSON string) (string, error) {
		var raw map[string]json.RawMessage
		if strings.TrimSpace(argsJSON) != "" {
			if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
				return "", fmt.Errorf("invalid tool arguments: %w", err)
			}
		}
		var sqlArgs []any
		for _, p := range tmpl.Parameters {
			v, ok := raw[p.Name]
			if !ok || string(v) == "null" {
				if p.Required {
					return "", fmt.Errorf("missing required parameter %q", p.Name)
				}
				continue
			}
			converted, err := convertSQLTemplateParam(p.Type, v)
			if err != nil {
				return "", fmt.Errorf("parameter %q: %w", p.Name, err)
			}
			if len(p.Options) > 0 && !templateParamOptionMatches(p.Options, converted) {
				return "", fmt.Errorf("parameter %q: %v is not one of the allowed values (%s)", p.Name, converted, strings.Join(p.Options, ", "))
			}
			sqlArgs = append(sqlArgs, sql.Named(p.Name, converted))
		}
		return runMSSQLQueryArgs(ctx, cfg, rewriteSQLTemplatePlaceholders(tmpl.SQL), sqlArgs)
	}
}
