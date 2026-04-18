package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type moduleConfig struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Description string            `json:"description"`
	Enabled     bool              `json:"enabled"`
	Config      map[string]string `json:"config"`
}

type moduleRunResult struct {
	ModuleID string         `json:"module_id"`
	Action   string         `json:"action"`
	Source   string         `json:"source,omitempty"`
	Summary  string         `json:"summary"`
	Text     string         `json:"text,omitempty"`
	Meta     map[string]any `json:"meta,omitempty"`
}

type moduleStore struct {
	settings *settingsStore
}

func defaultModules() []moduleConfig {
	return []moduleConfig{
		{
			ID:          "module-sql",
			Name:        "SQL Connector",
			Kind:        "sql",
			Description: "Fragt SQLite, MariaDB oder MSSQL ueber deren CLI-Clients ab und kann Resultate als RAG-Quelle ingestieren.",
			Enabled:     false,
			Config: map[string]string{
				"backend":      "sqlite",
				"sqlite_bin":   "sqlite3",
				"sqlite_path":  "./example.db",
				"mariadb_bin":  "mysql",
				"mariadb_host": "127.0.0.1",
				"mariadb_port": "3306",
				"mariadb_user": "root",
				"mariadb_pass": "",
				"mariadb_db":   "example",
				"mssql_bin":    "sqlcmd",
				"mssql_host":   "127.0.0.1",
				"mssql_port":   "1433",
				"mssql_user":   "sa",
				"mssql_pass":   "",
				"mssql_db":     "master",
				"query":        "SELECT 'tinyRAG module ready' AS status;",
			},
		},
		{
			ID:          "module-mail",
			Name:        "Mail Server Connector",
			Kind:        "mail",
			Description: "Liest aktuelle E-Mails ueber POP3/POP3S ein und wandelt Betreff, Header und Text-Body in RAG-faehige Dokumente um.",
			Enabled:     false,
			Config: map[string]string{
				"protocol":     "pop3s",
				"host":         "mail.example.com",
				"port":         "995",
				"username":     "demo@example.com",
				"password":     "",
				"max_messages": "5",
			},
		},
		{
			ID:          "module-http-folder",
			Name:        "HTTP Folder Bridge",
			Kind:        "http-folder",
			Description: "Ermoeglicht HTTP Upload/Download in einen lokalen Ordner und kann Dateien direkt in die Wissensbasis uebernehmen.",
			Enabled:     true,
			Config: map[string]string{
				"root_dir":          "./module-files",
				"ingest_on_upload":  "true",
				"download_ingest":   "false",
				"allow_subfolders":  "true",
				"default_list_path": ".",
			},
		},
	}
}

func normalizeModules(mods []moduleConfig) []moduleConfig {
	if len(mods) == 0 {
		return defaultModules()
	}
	for i := range mods {
		if mods[i].Config == nil {
			mods[i].Config = map[string]string{}
		}
	}

	defaults := defaultModules()
	byID := make(map[string]moduleConfig, len(mods))
	for _, m := range mods {
		byID[m.ID] = m
	}
	for _, d := range defaults {
		if _, ok := byID[d.ID]; ok {
			continue
		}
		mods = append(mods, d)
	}
	return mods
}

func newModuleStore(settings *settingsStore) *moduleStore {
	return &moduleStore{settings: settings}
}

func (s *moduleStore) list() []moduleConfig {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	out := make([]moduleConfig, len(s.settings.s.Modules))
	copy(out, s.settings.s.Modules)
	return out
}

func (s *moduleStore) get(id string) (moduleConfig, bool) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for _, m := range s.settings.s.Modules {
		if m.ID == id {
			return m, true
		}
	}
	return moduleConfig{}, false
}

func (s *moduleStore) upsert(mod moduleConfig) (moduleConfig, error) {
	if strings.TrimSpace(mod.ID) == "" {
		return moduleConfig{}, fmt.Errorf("module id required")
	}
	if strings.TrimSpace(mod.Name) == "" {
		mod.Name = mod.ID
	}
	if mod.Config == nil {
		mod.Config = map[string]string{}
	}

	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	for i, existing := range s.settings.s.Modules {
		if existing.ID == mod.ID {
			s.settings.s.Modules[i] = mod
			return mod, s.settings.saveLocked()
		}
	}
	s.settings.s.Modules = append(s.settings.s.Modules, mod)
	return mod, s.settings.saveLocked()
}

func (s *moduleStore) enabledTools() []toolDef {
	var out []toolDef
	for _, mod := range s.list() {
		if !mod.Enabled {
			continue
		}
		switch mod.Kind {
		case "sql":
			out = append(out, toolDef{
				Name:        "module:" + mod.ID,
				Description: mod.Description + " Query override optional; otherwise the configured query is used.",
				ParamHint:   "SQL query override or empty",
			})
		case "mail":
			out = append(out, toolDef{
				Name:        "module:" + mod.ID,
				Description: mod.Description + " Query can be a number to fetch the latest N messages.",
				ParamHint:   "Number of latest emails to fetch",
			})
		case "http-folder":
			out = append(out, toolDef{
				Name:        "module:" + mod.ID,
				Description: mod.Description + " Query is a relative file path to ingest from the folder.",
				ParamHint:   "Relative file path",
			})
		}
	}
	return out
}

func parseBoolString(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseIntString(v string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func allowedTextExtensions() map[string]bool {
	return map[string]bool{
		".txt": true, ".md": true, ".csv": true, ".json": true,
		".xml": true, ".html": true, ".log": true, ".htm": true,
		".yaml": true, ".yml": true, ".toml": true, ".ini": true,
		".cfg": true, ".conf": true, ".sql": true, ".go": true,
		".py": true, ".js": true, ".ts": true, ".rs": true,
		".c": true, ".h": true, ".cpp": true, ".java": true,
		".eml": true,
	}
}

// maxXMLFileSize is the maximum number of bytes read from a single XML entry
// inside an Office document ZIP archive.
const maxXMLFileSize = 20 * 1024 * 1024

// minTextRunLength is the minimum number of consecutive printable ASCII
// characters required to treat a run as a text fragment during PDF fallback extraction.
const minTextRunLength = 4

func allowedBinaryExtensions() map[string]bool {
	return map[string]bool{
		".pdf":  true,
		".docx": true,
		".odt":  true,
		".pptx": true,
		".odp":  true,
		".xlsx": true,
		".ods":  true,
	}
}

// stripXMLTags removes all XML/HTML tags from s and collapses whitespace,
// returning only the human-readable text content.
func stripXMLTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	// Normalise runs of whitespace
	whitespaceReplacer := strings.NewReplacer("\r\n", "\n", "\r", "\n")
	out := whitespaceReplacer.Replace(b.String())
	// Collapse repeated blank lines
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		lines = append(lines, t)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// extractXMLFromZip opens a ZIP archive from data and returns the concatenated
// text content of all entries whose name matches one of the given paths.
func extractXMLFromZip(data []byte, paths ...string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("not a valid ZIP archive: %w", err)
	}
	pathSet := make(map[string]bool, len(paths))
	for _, p := range paths {
		pathSet[p] = true
	}
	var sb strings.Builder
	for _, f := range zr.File {
		if !pathSet[f.Name] {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		raw, err := io.ReadAll(io.LimitReader(rc, maxXMLFileSize))
		rc.Close()
		if err != nil {
			return "", err
		}
		sb.WriteString(stripXMLTags(string(raw)))
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String()), nil
}

// extractTextFromPDF tries to extract text from a PDF byte slice.
// It first attempts the external pdftotext tool (poppler-utils);
// if unavailable it falls back to a simple heuristic that scans the
// raw PDF binary for printable ASCII runs (adequate for simple PDFs).
func extractTextFromPDF(data []byte) (string, error) {
	// Try pdftotext if available.
	if bin, err := exec.LookPath("pdftotext"); err == nil {
		tmp, err := os.CreateTemp("", "tinyrag-*.pdf")
		if err != nil {
			return "", err
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return "", err
		}
		tmp.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "-nopgbrk", "-enc", "UTF-8", tmpPath, "-").Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}

	// Fallback: scan for printable UTF-8 text runs inside the PDF binary.
	// This works reasonably well for unencrypted PDFs with embedded plain text.
	var sb strings.Builder
	run := make([]byte, 0, 64)
	flush := func() {
		if len(run) >= minTextRunLength {
			sb.Write(run)
			sb.WriteByte('\n')
		}
		run = run[:0]
	}
	for _, b := range data {
		if b >= 0x20 && b < 0x7f {
			run = append(run, b)
		} else {
			flush()
		}
	}
	flush()
	text := sb.String()
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("could not extract text from PDF (install pdftotext for better results)")
	}
	return text, nil
}

// extractTextFromFile extracts readable text from data according to the file
// extension. For plain-text types the bytes are returned as-is; for binary
// document formats (PDF, DOCX, ODT …) specialised parsers are used.
func extractTextFromFile(data []byte, filename string) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		return extractTextFromPDF(data)
	case ".docx", ".pptx", ".xlsx":
		// Office Open XML: a ZIP containing XML parts.
		var xmlPaths []string
		switch ext {
		case ".docx":
			xmlPaths = []string{"word/document.xml"}
		case ".pptx":
			// Collect slide XML files dynamically.
			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				return "", fmt.Errorf("invalid PPTX: %w", err)
			}
			for _, f := range zr.File {
				if strings.HasPrefix(f.Name, "ppt/slides/slide") && strings.HasSuffix(f.Name, ".xml") {
					xmlPaths = append(xmlPaths, f.Name)
				}
			}
		case ".xlsx":
			xmlPaths = []string{"xl/sharedStrings.xml"}
		}
		text, err := extractXMLFromZip(data, xmlPaths...)
		if err != nil {
			return "", fmt.Errorf("could not parse %s: %w", ext, err)
		}
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("no text found in %s document", ext)
		}
		return text, nil
	case ".odt", ".odp", ".ods":
		// OpenDocument: a ZIP containing content.xml.
		text, err := extractXMLFromZip(data, "content.xml")
		if err != nil {
			return "", fmt.Errorf("could not parse %s: %w", ext, err)
		}
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("no text found in %s document", ext)
		}
		return text, nil
	default:
		// Treat as plain text (UTF-8).
		return string(data), nil
	}
}

func readFileForRAG(path string, maxBytes int64) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory")
	}
	if info.Size() > maxBytes {
		return "", fmt.Errorf("file too large (%d bytes)", info.Size())
	}
	ext := strings.ToLower(filepath.Ext(path))
	if !allowedTextExtensions()[ext] && !allowedBinaryExtensions()[ext] {
		return "", fmt.Errorf("unsupported file type: %s", filepath.Ext(path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if allowedBinaryExtensions()[ext] {
		return extractTextFromFile(b, path)
	}
	return string(b), nil
}

func runCommandWithTimeout(name string, args []string, env []string) (string, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("required binary %q not found in PATH", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s command timed out", name)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s failed: %w\n%s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runSQLModule(mod moduleConfig, queryOverride string) (moduleRunResult, error) {
	cfg := mod.Config
	backend := strings.ToLower(strings.TrimSpace(cfg["backend"]))
	query := strings.TrimSpace(queryOverride)
	if query == "" {
		query = strings.TrimSpace(cfg["query"])
	}
	if query == "" {
		return moduleRunResult{}, fmt.Errorf("sql query missing")
	}

	var (
		out    string
		err    error
		source string
	)

	switch backend {
	case "sqlite":
		bin := strings.TrimSpace(cfg["sqlite_bin"])
		if bin == "" {
			bin = "sqlite3"
		}
		dbPath := strings.TrimSpace(cfg["sqlite_path"])
		if dbPath == "" {
			return moduleRunResult{}, fmt.Errorf("sqlite_path missing")
		}
		out, err = runCommandWithTimeout(bin, []string{"-header", "-column", dbPath, query}, nil)
		source = fmt.Sprintf("module:%s:sqlite:%s", mod.ID, filepath.Base(dbPath))
	case "mariadb":
		bin := strings.TrimSpace(cfg["mariadb_bin"])
		if bin == "" {
			bin = "mysql"
		}
		host := strings.TrimSpace(cfg["mariadb_host"])
		port := strings.TrimSpace(cfg["mariadb_port"])
		user := strings.TrimSpace(cfg["mariadb_user"])
		pass := cfg["mariadb_pass"]
		db := strings.TrimSpace(cfg["mariadb_db"])
		args := []string{}
		if host != "" {
			args = append(args, "-h", host)
		}
		if port != "" {
			args = append(args, "-P", port)
		}
		if user != "" {
			args = append(args, "-u", user)
		}
		if db != "" {
			args = append(args, "-D", db)
		}
		args = append(args, "-B", "-r", "-e", query)
		env := []string{}
		if pass != "" {
			env = append(env, "MYSQL_PWD="+pass)
		}
		out, err = runCommandWithTimeout(bin, args, env)
		source = fmt.Sprintf("module:%s:mariadb:%s", mod.ID, db)
	case "mssql":
		bin := strings.TrimSpace(cfg["mssql_bin"])
		if bin == "" {
			bin = "sqlcmd"
		}
		host := strings.TrimSpace(cfg["mssql_host"])
		port := strings.TrimSpace(cfg["mssql_port"])
		user := strings.TrimSpace(cfg["mssql_user"])
		pass := cfg["mssql_pass"]
		db := strings.TrimSpace(cfg["mssql_db"])
		server := host
		if port != "" {
			server = host + "," + port
		}
		args := []string{"-S", server, "-Q", query, "-W"}
		if user != "" {
			args = append(args, "-U", user)
		}
		if pass != "" {
			args = append(args, "-P", pass)
		}
		if db != "" {
			args = append(args, "-d", db)
		}
		out, err = runCommandWithTimeout(bin, args, nil)
		source = fmt.Sprintf("module:%s:mssql:%s", mod.ID, db)
	default:
		return moduleRunResult{}, fmt.Errorf("unsupported sql backend %q", backend)
	}
	if err != nil {
		return moduleRunResult{}, err
	}

	text := fmt.Sprintf("SQL module: %s\nBackend: %s\nQuery:\n%s\n\nResult:\n%s", mod.Name, backend, query, strings.TrimSpace(out))
	return moduleRunResult{
		ModuleID: mod.ID,
		Action:   "query",
		Source:   source,
		Summary:  fmt.Sprintf("%s query finished (%d chars)", backend, len(strings.TrimSpace(out))),
		Text:     text,
		Meta: map[string]any{
			"backend": backend,
			"query":   query,
		},
	}, nil
}

type mailMessage struct {
	Index   int
	Subject string
	From    string
	Date    string
	Body    string
}

type pop3Client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
}

func dialPOP3(protocol, host, port string) (*pop3Client, error) {
	if host == "" {
		return nil, fmt.Errorf("mail host missing")
	}
	if port == "" {
		if strings.Contains(strings.ToLower(protocol), "tls") || strings.HasSuffix(strings.ToLower(protocol), "s") {
			port = "995"
		} else {
			port = "110"
		}
	}
	addr := net.JoinHostPort(host, port)
	var conn net.Conn
	var err error
	if strings.Contains(strings.ToLower(protocol), "tls") || strings.HasSuffix(strings.ToLower(protocol), "s") {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
	}
	if err != nil {
		return nil, err
	}
	c := &pop3Client{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
	line, err := c.readLine()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "+OK") {
		conn.Close()
		return nil, fmt.Errorf("pop3 greeting failed: %s", line)
	}
	return c, nil
}

func (c *pop3Client) close() {
	if c == nil || c.conn == nil {
		return
	}
	_, _ = c.cmd("QUIT")
	_ = c.conn.Close()
}

func (c *pop3Client) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *pop3Client) cmd(command string) (string, error) {
	if _, err := c.w.WriteString(command + "\r\n"); err != nil {
		return "", err
	}
	if err := c.w.Flush(); err != nil {
		return "", err
	}
	line, err := c.readLine()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(line, "+OK") {
		return "", fmt.Errorf("pop3 %s failed: %s", strings.Split(command, " ")[0], line)
	}
	return line, nil
}

func (c *pop3Client) multiline(command string) ([]string, error) {
	if _, err := c.w.WriteString(command + "\r\n"); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "+OK") {
		return nil, fmt.Errorf("pop3 %s failed: %s", strings.Split(command, " ")[0], line)
	}
	var lines []string
	for {
		l, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if l == "." {
			break
		}
		if strings.HasPrefix(l, "..") {
			l = l[1:]
		}
		lines = append(lines, l)
	}
	return lines, nil
}

func fetchMailMessages(mod moduleConfig, limit int) ([]mailMessage, error) {
	cfg := mod.Config
	client, err := dialPOP3(cfg["protocol"], cfg["host"], cfg["port"])
	if err != nil {
		return nil, err
	}
	defer client.close()

	if _, err := client.cmd("USER " + strings.TrimSpace(cfg["username"])); err != nil {
		return nil, err
	}
	if _, err := client.cmd("PASS " + cfg["password"]); err != nil {
		return nil, err
	}
	stat, err := client.cmd("STAT")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(stat)
	if len(fields) < 2 {
		return nil, fmt.Errorf("unexpected STAT response: %s", stat)
	}
	total, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("cannot parse mail count: %w", err)
	}
	if total == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = parseIntString(cfg["max_messages"], 5)
	}
	if limit > total {
		limit = total
	}
	start := total - limit + 1
	var out []mailMessage
	for i := start; i <= total; i++ {
		lines, err := client.multiline(fmt.Sprintf("RETR %d", i))
		if err != nil {
			return nil, err
		}
		raw := strings.Join(lines, "\r\n")
		msg, err := mail.ReadMessage(strings.NewReader(raw))
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(msg.Body, 128*1024))
		if err != nil {
			return nil, err
		}
		out = append(out, mailMessage{
			Index:   i,
			Subject: msg.Header.Get("Subject"),
			From:    msg.Header.Get("From"),
			Date:    msg.Header.Get("Date"),
			Body:    string(body),
		})
	}
	return out, nil
}

func runMailModule(mod moduleConfig, limit int) (moduleRunResult, []mailMessage, error) {
	msgs, err := fetchMailMessages(mod, limit)
	if err != nil {
		return moduleRunResult{}, nil, err
	}
	if len(msgs) == 0 {
		return moduleRunResult{
			ModuleID: mod.ID,
			Action:   "fetch_latest",
			Summary:  "No messages available",
		}, nil, nil
	}
	var parts []string
	for _, msg := range msgs {
		body := strings.TrimSpace(msg.Body)
		if len(body) > 1200 {
			body = body[:1200] + "\n[...]"
		}
		parts = append(parts, fmt.Sprintf("Mail #%d\nFrom: %s\nDate: %s\nSubject: %s\n\n%s", msg.Index, msg.From, msg.Date, msg.Subject, body))
	}
	return moduleRunResult{
		ModuleID: mod.ID,
		Action:   "fetch_latest",
		Source:   "module:" + mod.ID + ":mail",
		Summary:  fmt.Sprintf("%d email(s) fetched", len(msgs)),
		Text:     strings.Join(parts, "\n\n---\n\n"),
		Meta: map[string]any{
			"messages": len(msgs),
		},
	}, msgs, nil
}

func resolveModulePath(root, rel string, allowSubfolders bool) (string, error) {
	if root == "" {
		return "", fmt.Errorf("root_dir missing")
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return cleanRoot, nil
	}
	cleanRel := filepath.Clean(rel)
	if strings.HasPrefix(cleanRel, "..") {
		return "", fmt.Errorf("path escapes module root")
	}
	if !allowSubfolders && strings.Contains(cleanRel, string(os.PathSeparator)) {
		return "", fmt.Errorf("subfolders disabled for this module")
	}
	full := filepath.Join(cleanRoot, cleanRel)
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if fullAbs != cleanRoot && !strings.HasPrefix(fullAbs, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes module root")
	}
	return fullAbs, nil
}

func listFolderModule(mod moduleConfig, rel string) (moduleRunResult, error) {
	root := mod.Config["root_dir"]
	target, err := resolveModulePath(root, rel, parseBoolString(mod.Config["allow_subfolders"]))
	if err != nil {
		return moduleRunResult{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return moduleRunResult{}, err
	}
	if !info.IsDir() {
		return moduleRunResult{}, fmt.Errorf("path is not a directory")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return moduleRunResult{}, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		label := e.Name()
		if e.IsDir() {
			label += "/"
		}
		names = append(names, label)
	}
	sort.Strings(names)
	return moduleRunResult{
		ModuleID: mod.ID,
		Action:   "list",
		Summary:  fmt.Sprintf("%d entries", len(names)),
		Text:     strings.Join(names, "\n"),
		Meta: map[string]any{
			"path": target,
		},
	}, nil
}

func ingestFolderModulePath(mod moduleConfig, rag *ragSystem, embedModel, rel string) (moduleRunResult, error) {
	root := mod.Config["root_dir"]
	target, err := resolveModulePath(root, rel, parseBoolString(mod.Config["allow_subfolders"]))
	if err != nil {
		return moduleRunResult{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return moduleRunResult{}, err
	}
	if info.IsDir() {
		var files, chunksAdded, redactions int
		err := filepath.WalkDir(target, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			text, readErr := readFileForRAG(path, 5*1024*1024)
			if readErr != nil || strings.TrimSpace(text) == "" {
				return nil
			}
			relPath, _ := filepath.Rel(root, path)
			cfg := settings.get()
			chunks, reds := chunksForIngest(text, cfg)
			if err := rag.addChunks("module:"+mod.ID+":folder:"+relPath, chunks, embedModel); err != nil {
				return nil
			}
			files++
			chunksAdded += len(chunks)
			redactions += reds
			return nil
		})
		if err != nil {
			return moduleRunResult{}, err
		}
		return moduleRunResult{
			ModuleID: mod.ID,
			Action:   "ingest",
			Source:   "module:" + mod.ID + ":folder",
			Summary:  fmt.Sprintf("%d files ingested", files),
			Meta: map[string]any{
				"files":      files,
				"chunks":     chunksAdded,
				"path":       target,
				"redactions": redactions,
			},
		}, nil
	}

	text, err := readFileForRAG(target, 5*1024*1024)
	if err != nil {
		return moduleRunResult{}, err
	}
	relPath, _ := filepath.Rel(root, target)
	cfg := settings.get()
	chunks, redactions := chunksForIngest(text, cfg)
	if err := rag.addChunks("module:"+mod.ID+":folder:"+relPath, chunks, embedModel); err != nil {
		return moduleRunResult{}, err
	}
	return moduleRunResult{
		ModuleID: mod.ID,
		Action:   "ingest",
		Source:   "module:" + mod.ID + ":folder:" + relPath,
		Summary:  fmt.Sprintf("File ingested (%d chunks)", len(chunks)),
		Text:     text,
		Meta: map[string]any{
			"path":       target,
			"chunks":     len(chunks),
			"redactions": redactions,
		},
	}, nil
}

func saveUploadedModuleFile(mod moduleConfig, file multipart.File, header *multipart.FileHeader, target string) (string, error) {
	root := mod.Config["root_dir"]
	destPath, err := resolveModulePath(root, target, parseBoolString(mod.Config["allow_subfolders"]))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(destPath)
	if err == nil && info.IsDir() {
		destPath = filepath.Join(destPath, filepath.Base(header.Filename))
	} else if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(target, "/") {
		destPath = filepath.Join(destPath, filepath.Base(header.Filename))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}
	out, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(file, 10*1024*1024)); err != nil {
		return "", err
	}
	return destPath, nil
}

func ingestModuleResult(rag *ragSystem, embedModel string, res moduleRunResult) (int, error) {
	if strings.TrimSpace(res.Text) == "" || strings.TrimSpace(res.Source) == "" {
		return 0, nil
	}
	chunks, _ := chunksForIngest(res.Text, settings.get())
	if err := rag.addChunks(res.Source, chunks, embedModel); err != nil {
		return 0, err
	}
	return len(chunks), nil
}

func executeModuleRun(mod moduleConfig, rag *ragSystem, embedModel, action, arg string, limit int, ingest bool) (moduleRunResult, error) {
	switch mod.Kind {
	case "sql":
		res, err := runSQLModule(mod, arg)
		if err != nil {
			return moduleRunResult{}, err
		}
		if ingest {
			chunks, err := ingestModuleResult(rag, embedModel, res)
			if err != nil {
				return moduleRunResult{}, err
			}
			if res.Meta == nil {
				res.Meta = map[string]any{}
			}
			res.Meta["chunks"] = chunks
		}
		return res, nil
	case "mail":
		res, msgs, err := runMailModule(mod, limit)
		if err != nil {
			return moduleRunResult{}, err
		}
		if ingest && len(msgs) > 0 {
			totalChunks := 0
			for _, msg := range msgs {
				doc := fmt.Sprintf("From: %s\nDate: %s\nSubject: %s\n\n%s", msg.From, msg.Date, msg.Subject, msg.Body)
				source := fmt.Sprintf("module:%s:mail:%d:%s", mod.ID, msg.Index, strings.ReplaceAll(strings.TrimSpace(msg.Subject), "'", ""))
				chunks, _ := chunksForIngest(doc, settings.get())
				if err := rag.addChunks(source, chunks, embedModel); err != nil {
					return moduleRunResult{}, err
				}
				totalChunks += len(chunks)
			}
			if res.Meta == nil {
				res.Meta = map[string]any{}
			}
			res.Meta["chunks"] = totalChunks
		}
		return res, nil
	case "http-folder":
		switch action {
		case "", "list":
			return listFolderModule(mod, arg)
		case "ingest":
			return ingestFolderModulePath(mod, rag, embedModel, arg)
		default:
			return moduleRunResult{}, fmt.Errorf("unsupported http-folder action %q", action)
		}
	default:
		return moduleRunResult{}, fmt.Errorf("unsupported module kind %q", mod.Kind)
	}
}
