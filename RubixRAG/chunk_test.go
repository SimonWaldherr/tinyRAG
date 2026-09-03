package main

import (
	"strings"
	"testing"
)

func TestChunkTextSplitsOversizedParagraphs(t *testing.T) {
	chunks := chunkText("alpha beta gamma delta epsilon", 10)
	if len(chunks) < 3 {
		t.Fatalf("expected an oversized paragraph to be split, got %q", chunks)
	}
	for _, chunk := range chunks {
		if got := len([]rune(chunk)); got > 10 {
			t.Errorf("chunk %q has %d runes, want at most 10", chunk, got)
		}
	}
	if got := strings.Join(chunks, " "); !strings.Contains(got, "epsilon") {
		t.Errorf("split chunks lost trailing text: %q", got)
	}
}

func TestChunkTextSplitsLongUnbrokenUnicodeText(t *testing.T) {
	chunks := chunkText("äöüß漢字abcdef", 4)
	if len(chunks) < 2 {
		t.Fatalf("expected long unbroken text to be split, got %q", chunks)
	}
	for _, chunk := range chunks {
		if got := len([]rune(chunk)); got > 4 {
			t.Errorf("chunk %q has %d runes, want at most 4", chunk, got)
		}
	}
}

func TestChunkTextKeepsParagraphsTogether(t *testing.T) {
	// Two multi-line paragraphs separated by a blank line: the chunker must
	// prefer the paragraph boundary over a mid-paragraph line boundary.
	text := "Zeile eins des ersten Absatzes.\nZeile zwei des ersten Absatzes.\n\nZeile eins des zweiten Absatzes.\nZeile zwei des zweiten Absatzes."
	chunks := chunkText(text, 70)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks (one per paragraph), got %d: %q", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "Zeile zwei des ersten Absatzes") {
		t.Errorf("first paragraph was split across chunks: %q", chunks)
	}
	if !strings.Contains(chunks[1], "Zeile eins des zweiten Absatzes") {
		t.Errorf("second paragraph missing from second chunk: %q", chunks)
	}
}

func TestChunkTextPropagatesHeadingBreadcrumbs(t *testing.T) {
	text := "## Produkte\n\nAllgemeines zu Produkten, ausführlich genug um zu füllen.\n\n### Preise\n\nDetails zu Preisen und Konditionen, erste Zeile.\n\nNoch mehr Preisdetails in einem weiteren Absatz, der den Chunk sicher überlaufen lässt."
	chunks := chunkText(text, 90)
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks, got %d: %q", len(chunks), chunks)
	}
	var found bool
	for _, c := range chunks[1:] {
		if strings.HasPrefix(c, "[Abschnitt: Produkte > Preise]\n") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no continuation chunk carries the section breadcrumb, got %q", chunks)
	}
}

func TestChunkTextHeadingResetsSiblingSection(t *testing.T) {
	text := "## Alpha\n\nInhalt von Alpha, lang genug für einen eigenen Chunk hier.\n\n## Beta\n\nInhalt von Beta, ebenfalls lang genug für einen eigenen Chunk."
	chunks := chunkText(text, 60)
	for _, c := range chunks {
		if strings.Contains(c, "[Abschnitt: Alpha > Beta]") {
			t.Fatalf("sibling heading must replace, not nest: %q", c)
		}
	}
	var betaCrumb bool
	for _, c := range chunks {
		if strings.HasPrefix(c, "[Abschnitt: Beta]\n") {
			betaCrumb = true
		}
	}
	if !betaCrumb {
		t.Fatalf("no chunk carries the [Abschnitt: Beta] breadcrumb: %q", chunks)
	}
}

func TestChunkTextIgnoresLevelOneHashLines(t *testing.T) {
	// Single-# lines are comment syntax in Python/shell files — they must not
	// become breadcrumbs.
	text := "# init logging\ncode line one here\ncode line two here\n\n# second comment\nmore code follows here and fills space"
	chunks := chunkText(text, 40)
	for _, c := range chunks {
		if strings.Contains(c, "[Abschnitt:") {
			t.Fatalf("level-1 # line must not produce a breadcrumb, got %q", c)
		}
	}
}

func TestChunkTextParagraphBeforeHeadingKeepsOldSection(t *testing.T) {
	// The paragraph under "Alt" must be tagged [Abschnitt: Alt] even though a
	// new heading follows immediately — flushing order matters.
	text := "## Alt\n\nDieser Absatz gehört zum alten Abschnitt und ist lang genug, um einen Flush zu erzwingen bevor der neue Abschnitt beginnt.\n## Neu\n\nInhalt des neuen Abschnitts, ebenfalls ausreichend lang für einen Chunk."
	chunks := chunkText(text, 60)
	for _, c := range chunks {
		if strings.Contains(c, "gehört zum alten Abschnitt") && strings.HasPrefix(c, "[Abschnitt: Neu]") {
			t.Fatalf("old section's paragraph mis-tagged with the new section: %q", c)
		}
	}
}

func TestChunkTextKeepsSmallTableIntact(t *testing.T) {
	table := "| Artikel | Preis |\n|---|---|\n| Schraube | 1,20 |\n| Mutter | 0,80 |"
	text := "Einleitung vor der Tabelle.\n\n" + table + "\n\nText nach der Tabelle."
	chunks := chunkText(text, 120)
	var whole bool
	for _, c := range chunks {
		if strings.Contains(c, "| Schraube | 1,20 |") && strings.Contains(c, "| Mutter | 0,80 |") && strings.Contains(c, "| Artikel | Preis |") {
			whole = true
		}
	}
	if !whole {
		t.Fatalf("small table was split apart, got %q", chunks)
	}
}

func TestChunkTextRepeatsTableHeaderOnSplit(t *testing.T) {
	rows := []string{"| Artikel | Preis |", "|---|---|"}
	for i := 0; i < 12; i++ {
		rows = append(rows, "| Artikelname mit etwas Länge Nr. "+strings.Repeat("x", i)+" | 1,20 |")
	}
	chunks := chunkText(strings.Join(rows, "\n"), 160)
	if len(chunks) < 2 {
		t.Fatalf("want the oversized table split into multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !strings.Contains(c, "| Artikel | Preis |") {
			t.Errorf("chunk %d lost the table header: %q", i, c)
		}
	}
}

func TestSplitLongParagraphPrefersSentenceBoundary(t *testing.T) {
	text := "Der erste Satz ist hier zu Ende. Der zweite Satz geht dann noch ein ganzes Stück weiter und weiter."
	parts := splitLongParagraph(text, 60)
	if len(parts) < 2 {
		t.Fatalf("want a split, got %q", parts)
	}
	if parts[0] != "Der erste Satz ist hier zu Ende." {
		t.Fatalf("want the cut at the sentence boundary, got first part %q", parts[0])
	}
}

func TestSplitLongParagraphFallsBackToWhitespace(t *testing.T) {
	text := "wort " + strings.Repeat("lang ", 30)
	for _, p := range splitLongParagraph(text, 40) {
		if got := len([]rune(p)); got > 40 {
			t.Errorf("part %q has %d runes, want at most 40", p, got)
		}
	}
}

func TestChunkTextMergesTinyTail(t *testing.T) {
	// A ~full chunk followed by a tiny trailing paragraph: the tail must be
	// merged into the previous chunk instead of stored as a noise fragment.
	big := strings.Repeat("wort ", 159) + "wort" // 799 runes
	text := big + "\n\nEnde."
	chunks := chunkText(text, 800)
	if len(chunks) != 1 {
		t.Fatalf("want the tiny tail merged into one chunk, got %d: tail=%q", len(chunks), chunks[len(chunks)-1])
	}
	if !strings.HasSuffix(chunks[0], "Ende.") {
		t.Fatalf("merged chunk lost the tail text: %q", chunks[0][len(chunks[0])-40:])
	}
}

func TestChunkTextTinyTailNotDuplicatedByOverlapCarry(t *testing.T) {
	// When the final chunk consists of a carried overlap unit plus nothing
	// tiny-mergeable, content must not appear twice in one chunk.
	text := strings.Repeat("erste worte hier. ", 30) + "\n\nkurz"
	chunks := chunkText(text, 200)
	last := chunks[len(chunks)-1]
	if strings.Count(last, "kurz") > 1 {
		t.Fatalf("tail duplicated within one chunk: %q", last)
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "kurz") {
		t.Fatalf("tail text lost entirely: %q", joined)
	}
}

func TestHeadingContextLineMatchesSectionMarkerConvention(t *testing.T) {
	// store.go's sectionMarkerRe strips exactly what headingContextLine
	// produces — keep the two halves of the convention in sync.
	line := headingContextLine("Produkte > Preise")
	if !sectionMarkerRe.MatchString(line) {
		t.Fatalf("sectionMarkerRe must match %q", line)
	}
}
