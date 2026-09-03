package main

import (
	"strings"
	"testing"
)

func TestSplitParagraphs(t *testing.T) {
	body := "First paragraph.\n\nSecond paragraph,\nstill same one.\n\n\nThird, after extra blank lines.\n"
	got := splitParagraphs(body)
	want := []string{"First paragraph.", "Second paragraph,\nstill same one.", "Third, after extra blank lines."}
	if len(got) != len(want) {
		t.Fatalf("want %d paragraphs, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paragraph %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestNormalizeParagraphCollapsesWhitespaceAndCase(t *testing.T) {
	a := normalizeParagraph("Diese E-Mail enthält   vertrauliche\nInformationen.")
	b := normalizeParagraph("diese e-mail enthält vertrauliche informationen.")
	if a != b {
		t.Fatalf("expected whitespace/case-insensitive match, got %q vs %q", a, b)
	}
}

func TestBoilerplateThresholdScalesWithMessageCount(t *testing.T) {
	if got := boilerplateThreshold(10); got != boilerplateMinCount {
		t.Fatalf("small runs should use the absolute floor (%d), got %d", boilerplateMinCount, got)
	}
	if got := boilerplateThreshold(1000); got != 50 {
		t.Fatalf("want 5%% of 1000 = 50, got %d", got)
	}
}

// TestBoilerplateDetectorFlagsRecurringDisclaimer mirrors the reported
// case: a long bilingual legal disclaimer repeated on every message
// should be detected and stripped, while each message's actual (unique)
// content survives untouched.
func TestBoilerplateDetectorFlagsRecurringDisclaimer(t *testing.T) {
	disclaimer := "Diese E-Mail enthält vertrauliche und/oder rechtlich geschützte Informationen. " +
		"Wenn Sie nicht der richtige Adressat sind oder diese E-Mail irrtümlich erhalten haben, " +
		"informieren Sie bitte sofort den Absender und vernichten Sie diese Mail."

	bodies := []string{
		"Hallo Team,\n\nbitte prüft den Anhang bis Freitag.\n\n" + disclaimer,
		"Guten Tag,\n\nder Liefertermin verschiebt sich auf KW 32.\n\n" + disclaimer,
		"Hi,\n\nkannst du das nochmal gegenlesen?\n\n" + disclaimer,
		"Moin,\n\nRechnung ist raus, bitte Zahlungseingang prüfen.\n\n" + disclaimer,
		"Servus,\n\nMeeting verschoben auf 14 Uhr.\n\n" + disclaimer,
	}

	d := newBoilerplateDetector()
	for _, b := range bodies {
		d.observe(b)
	}
	set := d.boilerplateSet()
	if len(set) != 1 {
		t.Fatalf("want exactly 1 boilerplate paragraph detected, got %d: %+v", len(set), set)
	}
	if !set[normalizeParagraph(disclaimer)] {
		t.Fatalf("expected the disclaimer paragraph to be flagged as boilerplate")
	}

	for _, b := range bodies {
		stripped := stripBoilerplateParagraphs(b, set)
		if stripped == b {
			t.Errorf("expected disclaimer to be stripped from body %q", b)
		}
		if len(stripped) == 0 {
			t.Errorf("stripping the disclaimer should not empty the whole body: %q", b)
		}
		for _, p := range splitParagraphs(stripped) {
			if normalizeParagraph(p) == normalizeParagraph(disclaimer) {
				t.Errorf("disclaimer paragraph survived stripping in %q", stripped)
			}
		}
	}
}

// TestBoilerplateDetectorIgnoresShortRepeatedPhrases guards the length
// floor: a short greeting repeated on every message is ordinary
// conversational language, not a disclaimer/footer, and must never be
// flagged regardless of how often it recurs.
func TestBoilerplateDetectorIgnoresShortRepeatedPhrases(t *testing.T) {
	d := newBoilerplateDetector()
	for i := 0; i < 10; i++ {
		d.observe("Guten Tag,\n\nDanke und viele Grüße.")
	}
	if set := d.boilerplateSet(); len(set) != 0 {
		t.Fatalf("short phrases must never be flagged, got %+v", set)
	}
}

// TestBoilerplateDetectorIgnoresBelowThresholdRepeats checks a paragraph
// that repeats a few times, but not often enough relative to the run
// size, is left alone — real content can legitimately be quoted or
// repeated by a few senders without being boilerplate.
func TestBoilerplateDetectorIgnoresBelowThresholdRepeats(t *testing.T) {
	longEnough := "This paragraph is long enough to pass the length floor easily on its own merits here."
	d := newBoilerplateDetector()
	// Only 2 of 100 messages share this paragraph — well under both the
	// absolute floor and the 5% fraction.
	for i := 0; i < 2; i++ {
		d.observe(longEnough)
	}
	for i := 0; i < 98; i++ {
		d.observe("Some unrelated unique message body number " + string(rune('A'+i%26)) + ".")
	}
	if set := d.boilerplateSet(); len(set) != 0 {
		t.Fatalf("2-of-100 repeats should stay below threshold, got %+v", set)
	}
}

func TestBoilerplateDetectorCountsEachMessageOnceEvenWithDuplicateParagraphs(t *testing.T) {
	repeated := "This exact paragraph appears twice in the very same message body by mistake here."
	d := newBoilerplateDetector()
	d.observe(repeated + "\n\n" + repeated)
	if got := d.counts[normalizeParagraph(repeated)]; got != 1 {
		t.Fatalf("want a paragraph repeated within one message to count once toward that message, got %d", got)
	}
}

// TestBoilerplateStatsReturnsSampleAndCountSortedByFrequency covers the
// debug-mode detail path: stats() must report each detected paragraph's
// original text and occurrence count, most-repeated first, so an operator
// can review what a run's boilerplate filter actually caught.
func TestBoilerplateStatsReturnsSampleAndCountSortedByFrequency(t *testing.T) {
	frequent := "This paragraph recurs on almost every message in the mailbox and is long enough to qualify."
	rarer := "This other paragraph recurs less often but still clears both the length and count thresholds."

	d := newBoilerplateDetector()
	for i := 0; i < 8; i++ {
		d.observe(frequent)
	}
	for i := 0; i < 6; i++ {
		d.observe(rarer)
	}
	set := d.boilerplateSet()
	if len(set) != 2 {
		t.Fatalf("want both paragraphs flagged as boilerplate, got %d: %+v", len(set), set)
	}

	stats := d.stats(set)
	if len(stats) != 2 {
		t.Fatalf("want 2 stats entries, got %d: %+v", len(stats), stats)
	}
	if stats[0].Sample != frequent || stats[0].Count != 8 {
		t.Errorf("want the more frequent paragraph first, got %+v", stats[0])
	}
	if stats[1].Sample != rarer || stats[1].Count != 6 {
		t.Errorf("want the less frequent paragraph second, got %+v", stats[1])
	}
	if stats[0].Chars != len([]rune(frequent)) {
		t.Errorf("want Chars to reflect the untruncated sample length, got %d", stats[0].Chars)
	}
}

// TestBoilerplateStatsTruncatesLongSamples ensures a very long detected
// paragraph doesn't blow up the debug payload — Chars still reports the
// true original length, but Sample is capped.
func TestBoilerplateStatsTruncatesLongSamples(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("A very long recurring disclaimer sentence. ", 20)) // > 300 runes
	d := newBoilerplateDetector()
	for i := 0; i < boilerplateMinCount; i++ {
		d.observe(long)
	}
	set := d.boilerplateSet()
	stats := d.stats(set)
	if len(stats) != 1 {
		t.Fatalf("want 1 stats entry, got %d", len(stats))
	}
	if stats[0].Chars != len([]rune(long)) {
		t.Errorf("want Chars to reflect the untruncated length %d, got %d", len([]rune(long)), stats[0].Chars)
	}
	if got := len([]rune(stats[0].Sample)); got != boilerplateStatSampleMaxChars+1 { // +1 for the trailing "…"
		t.Errorf("want Sample truncated to %d runes plus ellipsis, got %d runes", boilerplateStatSampleMaxChars, got)
	}
}

func TestStripBoilerplateParagraphsNoOpWhenSetEmpty(t *testing.T) {
	body := "Unrelated content.\n\nMore unrelated content."
	if got := stripBoilerplateParagraphs(body, nil); got != body {
		t.Fatalf("empty boilerplate set should leave body unchanged, got %q", got)
	}
}
