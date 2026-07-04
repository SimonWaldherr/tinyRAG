package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func chunkText(text string, maxLen int) []string {
	paragraphs := strings.Split(text, "\n")
	var chunks []string
	var current []string
	currentLen := 0

	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pLen := len(p)

		if currentLen+pLen > maxLen && len(current) > 0 {
			chunks = append(chunks, strings.Join(current, "\n"))

			// Overlap: retain the last paragraph if it's not the only one,
			// and if its length isn't excessively large (e.g. < maxLen/2).
			lastP := current[len(current)-1]
			if len(current) > 1 && len(lastP) < maxLen/2 {
				current = []string{lastP}
				currentLen = len(lastP) + 1 // +1 for the newline when joining
			} else {
				current = nil
				currentLen = 0
			}
		}

		current = append(current, p)
		currentLen += pLen + 1
	}
	if len(current) > 0 {
		chunks = append(chunks, strings.Join(current, "\n"))
	}
	return chunks
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
