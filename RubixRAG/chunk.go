package main

import (
	"regexp"
	"strings"
	"unicode"
)

// ─────────────────────────────────────────────────────────────────────────────
// Chunking
//
// chunkText splits extracted text into embeddable chunks of at most maxLen
// runes. It works in two passes:
//
//  1. chunkUnits segments the text into atomic units — paragraphs (runs of
//     non-blank lines), markdown table blocks (runs of |-rows, split with
//     their header row repeated when oversized), and heading lines — each
//     tagged with the markdown section breadcrumb ("A > B") active where the
//     unit starts.
//  2. The packing loop below greedily joins units into chunks, carrying a
//     small trailing unit over as overlap so semantic context survives chunk
//     boundaries (adapted from tinyRAG), and prefixes each chunk with its
//     first unit's breadcrumb as an "[Abschnitt: …]" context line.
//
// The breadcrumb line means a chunk from deep inside a long document still
// tells the embedder/BM25/LLM *which section* it belongs to — without it, a
// chunk under "## Preise" has no marker at all that it concerns pricing.
// fetchSourceContent (store.go) strips these markers again when reassembling
// a source's full text for the citation popup, so readers see clean text
// while search/context keep the extra signal.
// ─────────────────────────────────────────────────────────────────────────────

// chunkUnit is one atomic piece of text the packing loop places into chunks.
type chunkUnit struct {
	text       string
	breadcrumb string // section path active at this unit's start, "" if none
}

// runeLen returns the length of s in runes — chunk budgets are rune-based so
// multi-byte text (umlauts, CJK) is bounded by characters, not bytes.
func runeLen(s string) int {
	return len([]rune(s))
}

// headingLineRe matches markdown headings of level 2-6. Level-1 ("# …") is
// deliberately excluded: single-# lines are also comment syntax in Python/
// shell/YAML files (all importable as native text), and a comment mistaken
// for a section title would pollute every following chunk's breadcrumb. Real
// markdown (including markitdown's DOCX/PDF output) carries its structure in
// "##"+ section headings; the level-1 title is usually the document name,
// which citations already show as source_name.
var headingLineRe = regexp.MustCompile(`^(#{2,6})\s+(.+)$`)

// maxHeadingTitleRunes rejects implausibly long "headings" (more likely a
// commented-out code line or a stray run of #) from breadcrumb tracking.
const maxHeadingTitleRunes = 120

// breadcrumbMaxRunes caps the rendered breadcrumb; deeper paths drop their
// oldest (shallowest) entries first, keeping the most specific context.
const breadcrumbMaxRunes = 160

type headingEntry struct {
	level int
	title string
}

// headingPath tracks the current markdown section path while scanning lines.
type headingPath struct {
	entries []headingEntry
}

// parseHeading reports whether line is a breadcrumb-worthy heading, and
// which — separate from observe so the caller can flush the previous
// section's paragraph *before* the path mutates to the new section.
func parseHeading(line string) (headingEntry, bool) {
	m := headingLineRe.FindStringSubmatch(line)
	if m == nil {
		return headingEntry{}, false
	}
	title := strings.TrimSpace(m[2])
	if title == "" || runeLen(title) > maxHeadingTitleRunes {
		return headingEntry{}, false
	}
	return headingEntry{level: len(m[1]), title: title}, true
}

// observe pushes e onto the path, popping every entry at the same or a
// deeper level first.
func (h *headingPath) observe(e headingEntry) {
	for len(h.entries) > 0 && h.entries[len(h.entries)-1].level >= e.level {
		h.entries = h.entries[:len(h.entries)-1]
	}
	h.entries = append(h.entries, e)
}

// breadcrumb renders the current section path as "A > B > C", dropping the
// shallowest entries when the rendered path exceeds breadcrumbMaxRunes.
func (h *headingPath) breadcrumb() string {
	if len(h.entries) == 0 {
		return ""
	}
	titles := make([]string, len(h.entries))
	for i, e := range h.entries {
		titles[i] = e.title
	}
	for len(titles) > 1 && runeLen(strings.Join(titles, " > ")) > breadcrumbMaxRunes {
		titles = titles[1:]
	}
	crumb := strings.Join(titles, " > ")
	if runeLen(crumb) > breadcrumbMaxRunes {
		crumb = string([]rune(crumb)[:breadcrumbMaxRunes])
	}
	return crumb
}

// headingContextLine renders the "[Abschnitt: …]" context line prefixed to a
// chunk. sectionMarkerRe (store.go's fetchSourceContent) must keep matching
// whatever this produces — they are two halves of the same convention.
func headingContextLine(breadcrumb string) string {
	return "[Abschnitt: " + breadcrumb + "]\n"
}

// isTableLine reports whether a (trimmed) line looks like a markdown table
// row — the unit segmenter keeps runs of these together as one atomic block.
func isTableLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "|")
}

// tableSeparatorRe matches the |---|:---:| style separator row directly
// under a table's header row (tolerant of collapseRepeatedRuns having
// shortened the dash runs at ingest time).
var tableSeparatorRe = regexp.MustCompile(`^\|[\s:|-]*\|?$`)

// chunkUnits segments text into atomic units (see chunkUnit) and tags each
// with the markdown section breadcrumb active at its start.
func chunkUnits(text string, maxLen int) []chunkUnit {
	lines := strings.Split(text, "\n")
	var units []chunkUnit
	var path headingPath

	var para []string  // current paragraph's lines
	var table []string // current table block's rows

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		joined := strings.Join(para, "\n")
		para = nil
		crumb := path.breadcrumb()
		if runeLen(joined) <= maxLen {
			units = append(units, chunkUnit{text: joined, breadcrumb: crumb})
			return
		}
		for _, part := range splitLongParagraph(joined, maxLen) {
			units = append(units, chunkUnit{text: part, breadcrumb: crumb})
		}
	}
	flushTable := func() {
		if len(table) == 0 {
			return
		}
		rows := table
		table = nil
		crumb := path.breadcrumb()
		// A single |-line is just text, not a table — treat it like a
		// paragraph line so stray pipes don't get table handling.
		if len(rows) == 1 {
			for _, part := range splitLongParagraph(rows[0], maxLen) {
				units = append(units, chunkUnit{text: part, breadcrumb: crumb})
			}
			return
		}
		for _, block := range splitTableBlock(rows, maxLen) {
			units = append(units, chunkUnit{text: block, breadcrumb: crumb})
		}
	}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			flushTable()
			flushPara()
			continue
		}
		if isTableLine(line) {
			flushPara()
			table = append(table, line)
			continue
		}
		flushTable()
		if entry, ok := parseHeading(line); ok {
			// A heading closes the previous paragraph (which still belongs
			// to the *old* section — flush before mutating the path) and
			// stands alone as its own unit, tagged with the new section it
			// just opened.
			flushPara()
			path.observe(entry)
			units = append(units, chunkUnit{text: line, breadcrumb: path.breadcrumb()})
			continue
		}
		para = append(para, line)
	}
	flushTable()
	flushPara()
	return units
}

// splitTableBlock splits an oversized markdown table into maxLen-bounded
// pieces, repeating the header row (and its |---| separator) at the top of
// every continuation so each piece stays interpretable on its own — a bare
// run of data rows loses its column meaning entirely. A header too large to
// repeat sensibly (over half the budget) is not repeated.
func splitTableBlock(rows []string, maxLen int) []string {
	whole := strings.Join(rows, "\n")
	if runeLen(whole) <= maxLen {
		return []string{whole}
	}

	var header []string
	body := rows
	if len(rows) >= 2 && tableSeparatorRe.MatchString(rows[1]) {
		header, body = rows[:2], rows[2:]
	} else {
		header, body = rows[:1], rows[1:]
	}
	headerLen := runeLen(strings.Join(header, "\n"))
	if headerLen > maxLen/2 {
		header = nil
		body = rows
		headerLen = 0
	}

	var parts []string
	var cur []string
	curLen := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		piece := append(append([]string{}, header...), cur...)
		parts = append(parts, strings.Join(piece, "\n"))
		cur = nil
		curLen = 0
	}
	for _, row := range body {
		rl := runeLen(row)
		if headerLen+rl+1 > maxLen {
			// One giant row can't fit under the header at all — split the
			// row itself; those pieces forgo the repeated header.
			flush()
			parts = append(parts, splitLongParagraph(row, maxLen)...)
			continue
		}
		if headerLen+curLen+rl+1 > maxLen && len(cur) > 0 {
			flush()
		}
		cur = append(cur, row)
		curLen += rl + 1
	}
	flush()
	return parts
}

// chunkText splits text into heading/table/paragraph-aware units and joins
// them into chunks of at most maxLen runes, retaining a small overlap between
// chunks so semantic context survives chunk boundaries. Chunks inside a
// markdown section are prefixed with an "[Abschnitt: …]" breadcrumb line
// (excluded from the maxLen budget; see the file header for why it exists and
// who strips it again). Adapted from tinyRAG.
func chunkText(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 800
	}
	units := chunkUnits(text, maxLen)

	var chunks []string
	var current []chunkUnit
	currentLen := 0
	// carryDup marks current[0] as a duplicate carried over from the
	// previous chunk for overlap — the tiny-tail merge below must not
	// re-append such a unit to the chunk that already contains it.
	carryDup := false

	render := func(us []chunkUnit) string {
		var b strings.Builder
		if bc := us[0].breadcrumb; bc != "" {
			b.WriteString(headingContextLine(bc))
		}
		for i, u := range us {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(u.text)
		}
		return b.String()
	}

	for _, u := range units {
		uLen := runeLen(u.text)
		if currentLen+uLen > maxLen && len(current) > 0 {
			chunks = append(chunks, render(current))

			last := current[len(current)-1]
			if len(current) > 1 && runeLen(last.text) < maxLen/2 {
				current = []chunkUnit{last}
				currentLen = runeLen(last.text) + 1
				carryDup = true
			} else {
				current = nil
				currentLen = 0
				carryDup = false
			}
		}
		current = append(current, u)
		currentLen += uLen + 1
	}
	if len(current) > 0 {
		// Merge a tiny trailing chunk into its predecessor instead of
		// storing/embedding a noise fragment (BM25's length normalization
		// over-rewards very short chunks, and each fragment still costs an
		// embedding call). Only when the tail holds genuinely new content —
		// a carried overlap unit is already part of the previous chunk.
		tail := render(current)
		if len(chunks) > 0 && !carryDup && runeLen(tail) < maxLen/8 {
			chunks[len(chunks)-1] += "\n" + tail
		} else {
			chunks = append(chunks, tail)
		}
	}
	return chunks
}

// splitLongParagraph breaks one paragraph into maxLen-rune pieces, preferring
// a sentence boundary in the last half of the window, then a line
// break, then any whitespace, before hard-cutting mid-word. It keeps chunking
// robust for minified HTML, long URLs, and other mail text with no convenient
// newline boundaries while still making forward progress on one giant word.
func splitLongParagraph(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 800
	}
	remaining := []rune(strings.TrimSpace(text))
	parts := make([]string, 0, 1+len(remaining)/maxLen)
	for len(remaining) > maxLen {
		cut := bestSplitPoint(remaining, maxLen)
		part := strings.TrimSpace(string(remaining[:cut]))
		if part == "" { // whitespace-only prefix: force progress at maxLen.
			cut = maxLen
			part = string(remaining[:cut])
		}
		parts = append(parts, part)
		remaining = []rune(strings.TrimSpace(string(remaining[cut:])))
	}
	if len(remaining) > 0 {
		parts = append(parts, string(remaining))
	}
	return parts
}

// sentenceEndRunes are the punctuation runes bestSplitPoint treats as a
// sentence boundary when directly followed by whitespace.
var sentenceEndRunes = map[rune]bool{'.': true, '!': true, '?': true, ':': true, ';': true}

// bestSplitPoint picks where to cut the next piece off remaining (which is
// longer than maxLen): the last "sentence end + whitespace" within the final
// half of the window if one exists, otherwise the last newline in that same
// region, otherwise the last whitespace anywhere in the window, otherwise
// exactly maxLen (mid-word, to guarantee forward progress). The half-window
// floor keeps pieces from shrinking too far just to land on a sentence end —
// below it, plain whitespace wins and the piece stays near budget.
func bestSplitPoint(remaining []rune, maxLen int) int {
	minPreferred := maxLen / 2
	whitespaceCut := 0
	newlineCut := 0
	for i := maxLen; i > 0; i-- {
		r := remaining[i-1]
		if !unicode.IsSpace(r) {
			continue
		}
		if whitespaceCut == 0 {
			whitespaceCut = i
		}
		if i > minPreferred {
			if i >= 2 && sentenceEndRunes[remaining[i-2]] {
				return i
			}
			if newlineCut == 0 && r == '\n' {
				newlineCut = i
			}
		} else if whitespaceCut != 0 {
			// Below the preferred region no sentence end can win anymore —
			// the fallback whitespace cut is already recorded.
			break
		}
	}
	if newlineCut != 0 {
		return newlineCut
	}
	if whitespaceCut != 0 {
		return whitespaceCut
	}
	return maxLen
}

var (
	piiEmailRe = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	piiPhoneRe = regexp.MustCompile(`\b\+?[0-9][0-9()\s./-]{6,}[0-9]\b`)
	piiIBANRe  = regexp.MustCompile(`(?i)\b[a-z]{2}[0-9]{2}[a-z0-9]{11,30}\b`)
	piiCardRe  = regexp.MustCompile(`\b(?:[0-9][ -]?){13,19}\b`)
)

// redactPII replaces common personally identifiable patterns with markers.
// Email ingestion involves real names/addresses/phone numbers by nature, so
// this is opt-in via settings rather than always-on.
func redactPII(text string) (string, int) {
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

// chunksForIngest applies the configured PII policy and chunk size to raw
// extracted text, returning ready-to-embed chunks and the redaction count.
func chunksForIngest(text string, s appSettings) ([]string, int) {
	sanitized := text
	redactions := 0
	if s.RedactPII && strings.TrimSpace(text) != "" {
		sanitized, redactions = redactPII(text)
	}
	return chunkText(sanitized, s.ChunkSize), redactions
}
