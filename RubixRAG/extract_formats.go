package main

// Native parsers and bounded container helpers for formats that are common in
// enterprise exports but do not need a heavyweight Go dependency. Everything
// here returns plain text/Markdown so the normal chunking, redaction and
// content-hash path remains unchanged.

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	archiveMaxEntries      = 2048
	archiveMaxExpandedByte = 128 * 1024 * 1024
	archiveMaxDepth        = 1
)

var (
	subtitleTimeLineRe = regexp.MustCompile(`^\s*(?:\d{1,2}:)?\d{2}:\d{2}[,.]\d{3}\s+-->\s+(?:\d{1,2}:)?\d{2}:\d{2}[,.]\d{3}`)
	subtitleTagRe      = regexp.MustCompile(`<[^>]+>`)
)

func isVideoExtension(ext string) bool {
	switch ext {
	case ".mp4", ".webm", ".mov", ".mkv", ".avi", ".flv":
		return true
	default:
		return false
	}
}

func unfoldRFCLines(raw []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var out []string
	for _, line := range lines {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += strings.TrimLeft(line, " \t")
			continue
		}
		out = append(out, strings.TrimSuffix(line, "\r"))
	}
	return out
}

func extractMBOX(raw []byte) (string, error) {
	reader := bufio.NewReader(bytes.NewReader(raw))
	var message bytes.Buffer
	started := false
	var texts []string
	flush := func() error {
		if message.Len() == 0 {
			return nil
		}
		text, err := extractEML(message.Bytes())
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
		message.Reset()
		return nil
	}
	for {
		line, err := reader.ReadString('\n')
		if strings.HasPrefix(line, "From ") && !strings.HasPrefix(line, "From:") {
			if started {
				if flushErr := flush(); flushErr != nil {
					return "", flushErr
				}
			}
			started = true
		} else if started {
			message.WriteString(line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read mbox: %w", err)
		}
	}
	if err := flush(); err != nil {
		return "", err
	}
	if len(texts) == 0 {
		// A few exporters omit the mbox separator for a one-message file.
		return extractEML(raw)
	}
	var b strings.Builder
	for i, text := range texts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## Nachricht %d\n\n%s", i+1, text)
	}
	return b.String(), nil
}

func extractMHTML(raw []byte) (string, error) {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return "", fmt.Errorf("parse mhtml: %w", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		return htmlToText(string(raw)), nil
	}
	reader := multipartReader(msg.Body, params["boundary"])
	var parts []string
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return "", fmt.Errorf("read mhtml part: %w", nextErr)
		}
		data, readErr := io.ReadAll(io.LimitReader(part, archiveMaxExpandedByte))
		if readErr != nil {
			return "", readErr
		}
		data = decodeTransferEncoding(data, part.Header.Get("Content-Transfer-Encoding"))
		partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		switch {
		case partType == "text/html":
			parts = append(parts, htmlToText(string(data)))
		case partType == "text/plain":
			parts = append(parts, string(data))
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return "", fmt.Errorf("mhtml contains no text/html or text/plain part")
	}
	return text, nil
}

// multipartReader keeps the concrete type out of the parser signatures while
// reusing the MIME handling already exercised by the EML importer.
func multipartReader(body io.Reader, boundary string) *multipart.Reader {
	return multipart.NewReader(body, boundary)
}

func decodeVCardValue(value string, params map[string]string) string {
	encoding := strings.ToLower(params["encoding"])
	switch encoding {
	case "quoted-printable":
		if decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(value))); err == nil {
			value = decodeCharset(decoded, params["charset"])
		}
	case "b", "base64":
		if decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(value), "")); err == nil {
			value = decodeCharset(decoded, params["charset"])
		}
	}
	value = strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\,`, ",", `\;`, ";").Replace(value)
	return strings.TrimSpace(value)
}

func extractVCards(raw []byte) (string, error) {
	lines := unfoldRFCLines(raw)
	var current []string
	var contacts []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		var b strings.Builder
		for _, line := range current {
			if line == "" || strings.HasPrefix(strings.ToUpper(line), "BEGIN:") || strings.HasPrefix(strings.ToUpper(line), "END:") {
				continue
			}
			colon := strings.IndexByte(line, ':')
			if colon <= 0 {
				continue
			}
			left, value := line[:colon], line[colon+1:]
			parts := strings.Split(left, ";")
			name := strings.ToUpper(parts[0])
			params := make(map[string]string)
			for _, p := range parts[1:] {
				kv := strings.SplitN(p, "=", 2)
				if len(kv) == 2 {
					params[strings.ToLower(kv[0])] = strings.Trim(kv[1], `"`)
				}
			}
			label := map[string]string{
				"FN": "Name", "N": "Name", "EMAIL": "E-Mail", "TEL": "Telefon",
				"ORG": "Organisation", "TITLE": "Titel", "ROLE": "Rolle", "ADR": "Adresse",
				"URL": "URL", "NOTE": "Notiz", "BDAY": "Geburtstag", "CATEGORIES": "Kategorien",
			}[name]
			if label == "" {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", label, decodeVCardValue(value, params))
		}
		if text := strings.TrimSpace(b.String()); text != "" {
			contacts = append(contacts, text)
		}
		current = nil
	}
	for _, line := range lines {
		upper := strings.ToUpper(strings.TrimSpace(line))
		switch upper {
		case "BEGIN:VCARD":
			flush()
			current = []string{line}
		case "END:VCARD":
			current = append(current, line)
			flush()
		default:
			if len(current) > 0 {
				current = append(current, line)
			}
		}
	}
	flush()
	if len(contacts) == 0 {
		return "", fmt.Errorf("vcf contains no readable contacts")
	}
	var b strings.Builder
	for i, contact := range contacts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## Kontakt %d\n%s", i+1, contact)
	}
	return b.String(), nil
}

func unescapeCalendarValue(value string) string {
	return strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";").Replace(value)
}

func extractCalendar(raw []byte) (string, error) {
	lines := unfoldRFCLines(raw)
	var current []string
	var events []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		var b strings.Builder
		for _, line := range current {
			colon := strings.IndexByte(line, ':')
			if colon <= 0 {
				continue
			}
			name := strings.ToUpper(strings.SplitN(line[:colon], ";", 2)[0])
			label := map[string]string{
				"SUMMARY": "Titel", "DESCRIPTION": "Beschreibung", "LOCATION": "Ort",
				"DTSTART": "Beginn", "DTEND": "Ende", "DUE": "Fällig",
				"ORGANIZER": "Organisator", "ATTENDEE": "Teilnehmer", "STATUS": "Status",
				"UID": "UID", "URL": "URL", "CATEGORIES": "Kategorien", "COMMENT": "Kommentar",
			}[name]
			if label == "" {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", label, unescapeCalendarValue(line[colon+1:]))
		}
		if text := strings.TrimSpace(b.String()); text != "" {
			events = append(events, text)
		}
		current = nil
	}
	for _, line := range lines {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if upper == "BEGIN:VEVENT" || upper == "BEGIN:VTODO" || upper == "BEGIN:VJOURNAL" {
			flush()
			current = []string{line}
			continue
		}
		if upper == "END:VEVENT" || upper == "END:VTODO" || upper == "END:VJOURNAL" {
			current = append(current, line)
			flush()
			continue
		}
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	flush()
	if len(events) == 0 {
		return "", fmt.Errorf("ics contains no readable calendar entries")
	}
	var b strings.Builder
	for i, event := range events {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## Kalendereintrag %d\n%s", i+1, event)
	}
	return b.String(), nil
}

func extractSubtitles(raw []byte, ext string) string {
	var b strings.Builder
	for _, line := range unfoldRFCLines(raw) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.EqualFold(trimmed, "WEBVTT") || subtitleTimeLineRe.MatchString(trimmed) || isDecimalLine(trimmed) {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "NOTE") && ext == ".vtt" {
			continue
		}
		trimmed = strings.TrimSpace(subtitleTagRe.ReplaceAllString(trimmed, ""))
		if trimmed != "" {
			b.WriteString(trimmed)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

func isDecimalLine(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func safeArchiveName(name string) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	clean := path.Clean(name)
	if name == "" || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(clean, "/") || strings.ContainsRune(name, 0) {
		return "", false
	}
	return clean, true
}

func extractArchive(pathName string, raw []byte, ext string, s appSettings) (string, error) {
	switch ext {
	case ".zip":
		return extractZipArchive(raw, s, 0)
	case ".tar":
		return extractTarArchive(bytes.NewReader(raw), s, 0)
	case ".tar.gz", ".tgz":
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "", fmt.Errorf("open gzip archive: %w", err)
		}
		defer gz.Close()
		return extractTarArchive(gz, s, 0)
	case ".7z":
		return extract7zArchive(pathName, s)
	default:
		return "", fmt.Errorf("unsupported archive type: %s", ext)
	}
}

func extractZipArchive(raw []byte, s appSettings, depth int) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("read zip: %w", err)
	}
	if len(reader.File) > archiveMaxEntries {
		return "", fmt.Errorf("archive contains too many entries (limit %d)", archiveMaxEntries)
	}
	var total int64
	var parts []string
	for _, file := range reader.File {
		name, ok := safeArchiveName(file.Name)
		if !ok || file.FileInfo().IsDir() || file.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if file.UncompressedSize64 > uint64(archiveMaxExpandedByte) {
			return "", fmt.Errorf("archive entry %q exceeds the expanded-size limit", name)
		}
		entry, err := file.Open()
		if err != nil {
			return "", err
		}
		data, readErr := io.ReadAll(io.LimitReader(entry, archiveMaxExpandedByte+1))
		entry.Close()
		if readErr != nil {
			return "", readErr
		}
		if len(data) > archiveMaxExpandedByte || total+int64(len(data)) > archiveMaxExpandedByte {
			return "", fmt.Errorf("archive expanded-size limit exceeded")
		}
		total += int64(len(data))
		text, ok := extractArchiveEntry(data, name, s, depth)
		if ok {
			parts = append(parts, "## "+name+"\n\n"+text)
		}
	}
	return joinArchiveParts(parts)
}

func extractTarArchive(reader io.Reader, s appSettings, depth int) (string, error) {
	tr := tar.NewReader(reader)
	var total int64
	var entries int
	var parts []string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar: %w", err)
		}
		entries++
		if entries > archiveMaxEntries {
			return "", fmt.Errorf("archive contains too many entries (limit %d)", archiveMaxEntries)
		}
		name, ok := safeArchiveName(header.Name)
		if !ok || header.FileInfo().IsDir() || header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			continue
		}
		if header.Size < 0 || header.Size > archiveMaxExpandedByte || total+header.Size > archiveMaxExpandedByte {
			return "", fmt.Errorf("archive expanded-size limit exceeded")
		}
		data, err := io.ReadAll(io.LimitReader(tr, archiveMaxExpandedByte+1))
		if err != nil {
			return "", err
		}
		total += int64(len(data))
		text, ok := extractArchiveEntry(data, name, s, depth)
		if ok {
			parts = append(parts, "## "+name+"\n\n"+text)
		}
	}
	return joinArchiveParts(parts)
}

func extractArchiveEntry(data []byte, name string, s appSettings, depth int) (string, bool) {
	ext := fileExtension(name)
	if archiveExtensions()[ext] {
		if ext == ".7z" && !s.AllowShellExec {
			return "", false
		}
		if depth >= archiveMaxDepth {
			return "", false
		}
		text, err := extractArchive(name, data, ext, s)
		return text, err == nil && strings.TrimSpace(text) != ""
	}
	if !isExtractableDocument(ext) {
		return "", false
	}
	text, err := extractDocumentText(data, name, s.Import, s.AllowShellExec)
	return text, err == nil && strings.TrimSpace(text) != ""
}

func joinArchiveParts(parts []string) (string, error) {
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return "", fmt.Errorf("archive contains no supported text documents")
	}
	return text, nil
}

func extract7zArchive(pathName string, s appSettings) (string, error) {
	bin := strings.TrimSpace(s.Import.SevenZipBin)
	if bin == "" {
		bin = "7z"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("7z binary %q not found in PATH: %w", bin, err)
	}
	dir, err := os.MkdirTemp("", "r3-7z-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "x", "-y", "-bd", "-o"+dir, pathName)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return "", fmt.Errorf("7z extraction failed: %s", strings.TrimSpace(string(output)))
	}
	var parts []string
	var total int64
	entries := 0
	err = filepath.WalkDir(dir, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		entries++
		if entries > archiveMaxEntries {
			return fmt.Errorf("archive contains too many entries (limit %d)", archiveMaxEntries)
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > archiveMaxExpandedByte || total+info.Size() > archiveMaxExpandedByte {
			return fmt.Errorf("archive expanded-size limit exceeded")
		}
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return readErr
		}
		total += int64(len(data))
		rel, relErr := filepath.Rel(dir, filePath)
		if relErr != nil {
			return relErr
		}
		if text, ok := extractArchiveEntry(data, rel, s, 0); ok {
			parts = append(parts, "## "+rel+"\n\n"+text)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return joinArchiveParts(parts)
}

func runVideoToText(ffmpegBin, markItDownBin, pathName string, maxMB int64) (string, error) {
	bin := strings.TrimSpace(ffmpegBin)
	if bin == "" {
		bin = "ffmpeg"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("ffmpeg binary %q not found in PATH: %w", bin, err)
	}
	tmp, err := os.CreateTemp("", "r3-video-*.wav")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	defer os.Remove(tmpPath)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "-nostdin", "-v", "error", "-i", pathName, "-vn", "-ac", "1", "-ar", "16000", "-f", "wav", "-y", tmpPath)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return "", fmt.Errorf("ffmpeg failed: %s", strings.TrimSpace(string(output)))
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return "", err
	}
	limit := maxMB
	if limit <= 0 {
		limit = 25
	}
	if info.Size() > limit*20*1024*1024 {
		return "", fmt.Errorf("decoded audio exceeds the media expansion limit")
	}
	return runMarkItDown(markItDownBin, tmpPath, "")
}

// Keep strconv linked into this file for older exported calendar variants
// whose numeric UTC offsets are occasionally found as bare values. The
// helper is intentionally small and avoids a full iCalendar dependency.
func parseCalendarOffset(raw string) string {
	if n, err := strconv.Atoi(raw); err == nil {
		return fmt.Sprintf("%d", n)
	}
	return raw
}
