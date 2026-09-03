package main

import (
	"regexp"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Cross-message boilerplate detection. A company mailbox export almost
// always carries the same legal-disclaimer/signature footer on every
// message ("Diese E-Mail enthält vertrauliche..."/"This e-mail may
// contain confidential...", often several languages stacked together).
// That text embeds identically into every chunk that includes it,
// diluting retrieval relevance without ever being useful to cite.
//
// Rather than hardcoding known disclaimer wording (brittle, language-
// specific, breaks the moment legal changes the wording), this detects it
// statistically within one import run: any paragraph that recurs
// near-verbatim across many different messages is almost certainly
// boilerplate, not content — a real message body varies message to
// message; a signature/disclaimer footer doesn't. See pst.go's importPST
// for the two-phase scan-then-ingest flow this requires: the boilerplate
// set can only be known once the whole run has been observed, so ingest
// is deferred until after the scan completes.
// ─────────────────────────────────────────────────────────────────────────────

var paragraphSplitRe = regexp.MustCompile(`\r?\n[ \t]*\r?\n`)

// splitParagraphs breaks body into paragraphs on blank-line boundaries,
// trimming each and dropping empty ones — the unit boilerplate detection
// counts and later strips.
func splitParagraphs(body string) []string {
	parts := paragraphSplitRe.Split(body, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var whitespaceRunRe = regexp.MustCompile(`\s+`)

// normalizeParagraph collapses whitespace and case so near-identical
// copies of the same footer (different line wrapping, trailing spaces,
// stray capitalization) hash identically.
func normalizeParagraph(p string) string {
	return strings.ToLower(whitespaceRunRe.ReplaceAllString(strings.TrimSpace(p), " "))
}

// boilerplateMinParagraphLen is the shortest normalized paragraph (in
// runes) ever considered a boilerplate candidate. Short recurring phrases
// ("Danke!", "Guten Tag,") are ordinary conversational language, not the
// multi-sentence disclaimer blocks this targets — never flagged
// regardless of how often they repeat.
const boilerplateMinParagraphLen = 60

// boilerplateMinCount is the absolute floor: a paragraph must recur at
// least this many times before a small import (where 5% of messages is
// only one or two) can flag it as boilerplate.
const boilerplateMinCount = 5

// boilerplateMinFraction: a paragraph must additionally recur across at
// least this fraction of the run's messages — keeps a large import from
// flagging something that happens to repeat a handful of times by
// coincidence (e.g. several people quoting the same short instruction)
// without actually being present on most messages.
const boilerplateMinFraction = 0.05

// boilerplateThreshold returns the minimum occurrence count a paragraph
// needs to qualify as boilerplate for a run of totalMessages messages —
// whichever of boilerplateMinCount/boilerplateMinFraction is higher.
func boilerplateThreshold(totalMessages int) int {
	byFraction := int(float64(totalMessages)*boilerplateMinFraction + 0.999999) // ceil
	if byFraction > boilerplateMinCount {
		return byFraction
	}
	return boilerplateMinCount
}

// boilerplateDetector accumulates paragraph frequency across one import
// run's messages, then reports which paragraphs qualify as boilerplate.
// Not safe for concurrent use — observe is called sequentially during the
// scan pass.
type boilerplateDetector struct {
	counts        map[string]int
	samples       map[string]string // normalized key -> first-seen original (un-normalized) paragraph text
	totalMessages int
}

func newBoilerplateDetector() *boilerplateDetector {
	return &boilerplateDetector{counts: map[string]int{}, samples: map[string]string{}}
}

// observe records one message's body paragraphs — call once per message
// during the scan pass, before any boilerplate set has been decided.
func (d *boilerplateDetector) observe(body string) {
	d.totalMessages++
	seen := map[string]bool{} // a paragraph repeated twice within one message must not count twice
	for _, p := range splitParagraphs(body) {
		if len([]rune(p)) < boilerplateMinParagraphLen {
			continue
		}
		key := normalizeParagraph(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		d.counts[key]++
		if _, ok := d.samples[key]; !ok {
			d.samples[key] = p
		}
	}
}

// boilerplateSet returns the normalized paragraph keys that qualify as
// boilerplate given everything observe has seen so far.
func (d *boilerplateDetector) boilerplateSet() map[string]bool {
	threshold := boilerplateThreshold(d.totalMessages)
	set := make(map[string]bool)
	for key, n := range d.counts {
		if n >= threshold {
			set[key] = true
		}
	}
	return set
}

// boilerplateStatSampleMaxChars caps how much of a detected paragraph's
// original text is included in debug stats — long disclaimer blocks are
// common, but an operator only needs enough to recognize what got
// filtered, not the whole text.
const boilerplateStatSampleMaxChars = 300

// boilerplateStat is one detected-and-filtered paragraph's debug summary:
// how often it recurred across the run and a readable sample of its
// (un-normalized) text, so an operator running debug mode can confirm
// what was actually stripped was signatures/disclaimers, not real content
// that just happened to repeat.
type boilerplateStat struct {
	Sample string `json:"sample"`
	Count  int    `json:"count"`
	Chars  int    `json:"chars"`
}

// stats returns debug detail for every paragraph in set, sorted by
// occurrence count descending so the most-repeated (and thus most
// impactful) strips surface first. Only meant to be called when debug
// output was explicitly requested — see importPST — since samples are
// verbatim message content.
func (d *boilerplateDetector) stats(set map[string]bool) []boilerplateStat {
	out := make([]boilerplateStat, 0, len(set))
	for key := range set {
		sample := d.samples[key]
		runes := []rune(sample)
		chars := len(runes)
		if chars > boilerplateStatSampleMaxChars {
			sample = string(runes[:boilerplateStatSampleMaxChars]) + "…"
		}
		out = append(out, boilerplateStat{Sample: sample, Count: d.counts[key], Chars: chars})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Sample < out[j].Sample
	})
	return out
}

// stripBoilerplateParagraphs removes every paragraph of body whose
// normalized form is in set, rejoining the rest with blank lines — the
// second pass, once the boilerplate set for the whole import run is known.
func stripBoilerplateParagraphs(body string, set map[string]bool) string {
	if len(set) == 0 {
		return body
	}
	paragraphs := splitParagraphs(body)
	kept := make([]string, 0, len(paragraphs))
	for _, p := range paragraphs {
		if set[normalizeParagraph(p)] {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "\n\n")
}
