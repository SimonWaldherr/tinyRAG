package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

const (
	chunkUnitOther = iota
	chunkUnitList
	chunkUnitTable
)

var (
	chunkListLineRe  = regexp.MustCompile(`^(?:[-*+]|\d{1,3}[.)])\s+\S`)
	chunkTableLineRe = regexp.MustCompile(`^\|.*\|$`)
	chunkCodeFenceRe = regexp.MustCompile("^(```|~~~)")
)

// classifyChunkLine reports whether a trimmed, non-empty line looks like a
// list item or a table row, so consecutive lines of the same kind can be
// packed as one atomic unit instead of by raw line.
func classifyChunkLine(line string) int {
	switch {
	case chunkTableLineRe.MatchString(line):
		return chunkUnitTable
	case chunkListLineRe.MatchString(line):
		return chunkUnitList
	default:
		return chunkUnitOther
	}
}

// buildChunkUnits splits text into the atomic units chunkText packs into
// chunks. Ordinary lines remain one unit each — matching chunkText's
// historical, purely line-based behavior — but a run of consecutive list
// items or table rows is merged into a single multi-line unit, and a fenced
// code block is captured whole (with original indentation, not trimmed),
// so a character-budget cut can never land inside a list run, a table, or
// a code block — only between them.
func buildChunkUnits(text string) []string {
	lines := strings.Split(text, "\n")
	var units []string
	var run []string
	runKind := chunkUnitOther
	inCode := false

	flushRun := func() {
		if len(run) > 0 {
			units = append(units, strings.Join(run, "\n"))
			run = nil
		}
	}

	for _, raw := range lines {
		content := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(content)

		if inCode {
			run = append(run, content)
			if chunkCodeFenceRe.MatchString(trimmed) {
				inCode = false
				flushRun()
			}
			continue
		}
		if trimmed == "" {
			flushRun()
			continue
		}
		if chunkCodeFenceRe.MatchString(trimmed) {
			flushRun()
			run = []string{trimmed}
			inCode = true
			continue
		}

		kind := classifyChunkLine(trimmed)
		if kind == chunkUnitList || kind == chunkUnitTable {
			if runKind == kind && len(run) > 0 {
				run = append(run, trimmed)
			} else {
				flushRun()
				run = []string{trimmed}
				runKind = kind
			}
			continue
		}

		flushRun()
		units = append(units, trimmed)
	}
	flushRun()
	return units
}

// splitLongLine breaks a single unbreakable unit (no internal line to fall
// back to) into maxLen-sized pieces, preferring the last whitespace boundary
// before the cut and always cutting on a UTF-8 rune boundary so multi-byte
// characters are never corrupted.
func splitLongLine(s string, maxLen int) []string {
	if maxLen <= 0 {
		return []string{s}
	}
	var out []string
	for len(s) > maxLen {
		cut := maxLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if idx := strings.LastIndexByte(s[:cut], ' '); idx > maxLen/2 {
			cut = idx
		}
		if cut == 0 {
			cut = maxLen // pathological: no valid earlier cut point, force progress
		}
		out = append(out, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// splitOversizedUnit handles a single packable unit that alone exceeds
// maxLen — previously such a unit passed through unsplit. A multi-line unit
// (a list run, table, or code block too big to fit whole) is re-packed by
// its own lines so whole rows/items stay together wherever possible; a
// genuinely unbreakable single line falls back to splitLongLine.
func splitOversizedUnit(u string, maxLen int) []string {
	lines := strings.Split(u, "\n")
	if len(lines) > 1 {
		return packChunkUnits(lines, maxLen)
	}
	return splitLongLine(u, maxLen)
}

// packChunkUnits greedily packs units into chunks bounded by maxLen,
// carrying a small overlap (the last unit of a closed chunk) into the next
// one when it's short enough to be worth the redundancy. This is chunkText's
// original algorithm, generalized from "paragraph" to "unit" so multi-line
// list/table/code units are packed atomically like any other unit.
func packChunkUnits(units []string, maxLen int) []string {
	var chunks []string
	var current []string
	currentLen := 0

	for _, u := range units {
		uLen := len(u)

		if currentLen+uLen > maxLen && len(current) > 0 {
			chunks = append(chunks, strings.Join(current, "\n"))

			// Overlap: retain the last unit if it's not the only one, and if
			// its length isn't excessively large (e.g. < maxLen/2).
			lastU := current[len(current)-1]
			if len(current) > 1 && len(lastU) < maxLen/2 {
				current = []string{lastU}
				currentLen = len(lastU) + 1 // +1 for the newline when joining
			} else {
				current = nil
				currentLen = 0
			}
		}

		if uLen > maxLen {
			if len(current) > 0 {
				chunks = append(chunks, strings.Join(current, "\n"))
				current = nil
				currentLen = 0
			}
			chunks = append(chunks, splitOversizedUnit(u, maxLen)...)
			continue
		}

		current = append(current, u)
		currentLen += uLen + 1
	}
	if len(current) > 0 {
		chunks = append(chunks, strings.Join(current, "\n"))
	}
	return chunks
}

func chunkText(text string, maxLen int) []string {
	return packChunkUnits(buildChunkUnits(text), maxLen)
}

var (
	piiEmailRe = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	piiPhoneRe = regexp.MustCompile(`\b\+?[0-9][0-9()\s./-]{6,}[0-9]\b`)
	piiIBANRe  = regexp.MustCompile(`(?i)\b[a-z]{2}[0-9]{2}[a-z0-9]{11,30}\b`)
	piiCardRe  = regexp.MustCompile(`\b(?:[0-9][ -]?){13,19}\b`)
)

func sanitizeTextForIngest(text string, s appSettings) (string, int) {
	if !s.RedactPII || strings.TrimSpace(text) == "" {
		return text, 0
	}
	out := text
	replacements := 0
	apply := func(re *regexp.Regexp, marker string) {
		m := re.FindAllStringIndex(out, -1)
		if len(m) == 0 {
			return
		}
		replacements += len(m)
		out = re.ReplaceAllString(out, marker)
	}
	apply(piiEmailRe, "[REDACTED_EMAIL]")
	apply(piiPhoneRe, "[REDACTED_PHONE]")
	apply(piiIBANRe, "[REDACTED_IBAN]")
	apply(piiCardRe, "[REDACTED_CARD]")
	return out, replacements
}

func chunksForIngestWithDoc(text string, s appSettings, documentID string, irreversible bool) ([]string, int) {
	sanitized, redactions := sanitizeAndPseudonymize(text, s, documentID, irreversible)
	return chunkText(sanitized, s.ChunkSize), redactions
}

func chunksForIngest(text string, s appSettings) ([]string, int) {
	return chunksForIngestWithDoc(text, s, stableContentHash(text), false)
}

// ─────────────────────────────────────────────────────────────────────────────
// Structured data import helpers (CSV / JSON → RAG chunks via tinySQL)
// ─────────────────────────────────────────────────────────────────────────────

// importDelimitedAsChunks uses tinySQL's ImportCSV to parse CSV/TSV data from
// src into a temporary in-memory database, then converts each row to a
// "key: value, …" text string that can be embedded and stored as RAG chunks.
func importDelimitedAsChunks(ctx context.Context, src io.Reader, source string, s appSettings) (*tinysql.ImportResult, []string, error) {
	tmpDB := tinysql.NewDB()
	tableName := "import_data"
	result, err := tinysql.ImportCSV(ctx, tmpDB, "default", tableName, src, &tinysql.ImportOptions{
		CreateTable:   true,
		TypeInference: false, // keep all values as TEXT for text chunking
	})
	if err != nil {
		return nil, nil, fmt.Errorf("csv parse: %w", err)
	}

	// Query all rows from the temp table.
	stmt, err := tinysql.ParseSQL("SELECT * FROM " + tableName)
	if err != nil {
		return result, nil, fmt.Errorf("csv query build: %w", err)
	}
	rs, err := tinysql.Execute(ctx, tmpDB, "default", stmt)
	if err != nil || rs == nil {
		return result, nil, fmt.Errorf("csv query: %w", err)
	}

	rawTexts := make([]string, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		var parts []string
		for _, col := range result.ColumnNames {
			v, _ := tinysql.GetVal(row, col)
			parts = append(parts, col+": "+fmt.Sprint(v))
		}
		rawTexts = append(rawTexts, strings.Join(parts, ", "))
	}

	// Re-chunk via the normal pipeline so chunk size limits are respected.
	var chunks []string
	for _, text := range rawTexts {
		c, _ := chunksForIngest(text, s)
		chunks = append(chunks, c...)
	}
	return result, chunks, nil
}

// importGeoAsChunks imports supported geodata into a temporary tinySQL table
// and turns each feature into an embeddable text record. It intentionally keeps
// the original geometry fields so location-aware questions retain context.
func importGeoAsChunks(ctx context.Context, src io.Reader, format, source string, s appSettings) (*tinysql.ImportResult, []string, error) {
	tmpDB := tinysql.NewDB()
	tableName := "geo_import"
	var (
		result *tinysql.ImportResult
		err    error
	)
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "geojson", "json":
		result, err = tinysql.ImportGeoJSON(ctx, tmpDB, "default", tableName, src, &tinysql.ImportOptions{CreateTable: true, TypeInference: false})
	case "kml":
		result, err = tinysql.ImportKML(ctx, tmpDB, "default", tableName, src, &tinysql.ImportOptions{CreateTable: true, TypeInference: false})
	case "osm", "xml":
		result, err = tinysql.ImportOSM(ctx, tmpDB, "default", tableName, src, &tinysql.ImportOptions{CreateTable: true, TypeInference: false})
	default:
		return nil, nil, fmt.Errorf("unsupported geo format %q (supported: geojson, kml, osm)", format)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s parse: %w", format, err)
	}
	stmt, err := tinysql.ParseSQL("SELECT * FROM " + tableName)
	if err != nil {
		return result, nil, fmt.Errorf("geo query build: %w", err)
	}
	rs, err := tinysql.Execute(ctx, tmpDB, "default", stmt)
	if err != nil || rs == nil {
		return result, nil, fmt.Errorf("geo query: %w", err)
	}
	chunks := make([]string, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		parts := []string{"source: " + source}
		for _, col := range result.ColumnNames {
			v, _ := tinysql.GetVal(row, col)
			parts = append(parts, col+": "+fmt.Sprint(v))
		}
		text, _ := chunksForIngest(strings.Join(parts, ", "), s)
		chunks = append(chunks, text...)
	}
	return result, chunks, nil
}

// importJSONAsChunks uses tinySQL's ImportJSON to parse a JSON array from src,
// then converts each object to a "key: value, …" text string for RAG ingestion.
func importJSONAsChunks(ctx context.Context, src io.Reader, source string, s appSettings) (*tinysql.ImportResult, []string, error) {
	tmpDB := tinysql.NewDB()
	tableName := "import_data"
	result, err := tinysql.ImportJSON(ctx, tmpDB, "default", tableName, src, &tinysql.ImportOptions{
		CreateTable:   true,
		TypeInference: false,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("json parse: %w", err)
	}

	stmt, err := tinysql.ParseSQL("SELECT * FROM " + tableName)
	if err != nil {
		return result, nil, fmt.Errorf("json query build: %w", err)
	}
	rs, err := tinysql.Execute(ctx, tmpDB, "default", stmt)
	if err != nil || rs == nil {
		return result, nil, fmt.Errorf("json query: %w", err)
	}

	rawTexts := make([]string, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		var parts []string
		for _, col := range result.ColumnNames {
			v, _ := tinysql.GetVal(row, col)
			parts = append(parts, col+": "+fmt.Sprint(v))
		}
		rawTexts = append(rawTexts, strings.Join(parts, ", "))
	}

	var chunks []string
	for _, text := range rawTexts {
		c, _ := chunksForIngest(text, s)
		chunks = append(chunks, c...)
	}
	return result, chunks, nil
}
