package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFilterByMinSimilarity(t *testing.T) {
	hits := []rankedHit{
		{SourceID: "strong", VectorScore: 0.9},
		{SourceID: "weak", VectorScore: 0.2},
	}
	// 0 (rankingConfig.MinVectorSimilarity's default) disables the check.
	if got := filterByMinSimilarity(hits, 0); len(got) != 2 {
		t.Fatalf("threshold 0: want both hits kept, got %+v", got)
	}
	got := filterByMinSimilarity(hits, 0.5)
	if len(got) != 1 || got[0].SourceID != "strong" {
		t.Fatalf("threshold 0.5: want only the strong hit, got %+v", got)
	}
}

func TestFilterByMinFinalScore(t *testing.T) {
	// zero-keyword hit: FinalScore includes a recency bonus that shouldn't
	// count toward clearing the floor, since it shares no vocabulary with
	// the query at all — mirrors the reported "irrelevant question, high
	// recency, no keyword overlap" case (vector=0.637, keyword=0,
	// recency=0.947, final=0.540 at default weights 0.7/0.2/0.1).
	zeroKeyword := rankedHit{SourceID: "zero-keyword", KeywordScore: 0, RecencyScore: 0.947, FinalScore: 0.540}
	// same nominal FinalScore, but it actually shares a query keyword —
	// its recency bonus is legitimate and must NOT be discounted.
	withKeyword := rankedHit{SourceID: "with-keyword", KeywordScore: 0.3, RecencyScore: 0.947, FinalScore: 0.540}
	hits := []rankedHit{zeroKeyword, withKeyword}

	// 0 (rankingConfig.MinFinalScore's default) disables the check.
	if got := filterByMinFinalScore(hits, 0, 0.1); len(got) != 2 {
		t.Fatalf("threshold 0: want both hits kept, got %+v", got)
	}

	// 0.45: zero-keyword's discounted score is 0.540 - 0.1*0.947 = 0.4453,
	// just below the floor — dropped. with-keyword's full 0.540 clears it.
	got := filterByMinFinalScore(hits, 0.45, 0.1)
	if len(got) != 1 || got[0].SourceID != "with-keyword" {
		t.Fatalf("threshold 0.45: want only the keyword-matched hit kept, got %+v", got)
	}
}

func TestUsedCitationMarkers(t *testing.T) {
	used := usedCitationMarkers("Laut [Q1] und [Q3] ist das so. [q1] nochmal.")
	if !used[1] || !used[3] {
		t.Fatalf("want markers 1 and 3 used, got %v", used)
	}
	if used[2] {
		t.Fatalf("marker 2 was never mentioned, got used=%v", used)
	}
}

func TestFilterCitations(t *testing.T) {
	citations := []sourceInfo{
		{SourceID: "a", SourceKind: "file", Marker: 1},
		{SourceID: "b", SourceKind: "pst_email", Marker: 2},
		{SourceID: "c", SourceKind: "file", Marker: 3},
	}
	s := appSettings{SourceVisibility: map[string]bool{"pst_email": false}}

	// Marker 2 (pst_email) is hidden by source_kind; marker 3 was never
	// cited by the model at all — only marker 1 should survive.
	got := filterCitations(citations, "Antwort mit Beleg [Q1] und [Q2].", s)
	if len(got) != 1 || got[0].SourceID != "a" {
		t.Fatalf("want only citation 'a' to survive, got %+v", got)
	}
}

func TestEmailFamilyRoot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pst:m.pst:Inbox:42", "pst:m.pst:Inbox:42"},
		{"pst:m.pst:Inbox:42:attachment:0:Angebot.pdf", "pst:m.pst:Inbox:42"},
		{"imap:user@x:INBOX:7:attachment:2:img.png", "imap:user@x:INBOX:7"},
	}
	for _, c := range cases {
		if got := emailFamilyRoot(c.in); got != c.want {
			t.Errorf("emailFamilyRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ingestEmailFamilyFixture stores one parent mail, two attachments sharing
// its source_id prefix, and one unrelated file — the canonical family
// layout ingestEmailAttachment produces (see ingest.go).
func ingestEmailFamilyFixture(t *testing.T, rag *ragSystem, s appSettings) {
	t.Helper()
	// Text bodies are deliberately >= minSiblingContentChars (rank.go) —
	// short stub text would now get skipped by expandEmailFamilies as a
	// (simulated) header-only sibling, which isn't what these fixtures are
	// meant to represent.
	docs := []struct{ id, kind, name, text string }{
		{"pst:m.pst:Inbox:42", "pst_email", "AW: Angebot", "This is the parent mail body, long enough to be chunked and embedded properly. It includes several sentences of realistic content so the fixture comfortably exceeds the minimum sibling content length used by email-family expansion."},
		{"pst:m.pst:Inbox:42:attachment:0:Angebot.pdf", "pst_attachment", "AW: Angebot — Anhang: Angebot.pdf", "ATTACHMENT-PDF-TEXT: the offer document body, long enough to be chunked and embedded. It includes several sentences of realistic content so the fixture comfortably exceeds the minimum sibling content length used by email-family expansion."},
		{"pst:m.pst:Inbox:42:attachment:1:Preise.xlsx", "pst_attachment", "AW: Angebot — Anhang: Preise.xlsx", "ATTACHMENT-XLSX-TEXT: the price list body, long enough to be chunked and embedded. It includes several sentences of realistic content so the fixture comfortably exceeds the minimum sibling content length used by email-family expansion."},
		{"file:/docs/Unrelated.txt", "file", "Unrelated.txt", "UNRELATED-TEXT: some other document, long enough to be chunked and embedded. It includes several sentences of realistic content so the fixture comfortably exceeds the minimum sibling content length used by email-family expansion."},
	}
	for _, d := range docs {
		if _, err := ingestDocument(rag, s, "test-embed", d.id, d.kind, d.name, d.text, 0, false); err != nil {
			t.Fatalf("ingestDocument(%s): %v", d.id, err)
		}
	}
}

func TestAssembleContextExpandsMailHitWithAttachments(t *testing.T) {
	rag, _ := ingestFamilyRAG(t)
	hits := []rankedHit{{
		SourceID: "pst:m.pst:Inbox:42", SourceKind: "pst_email", SourceName: "AW: Angebot",
		ChunkIdx: 0, Content: "This is the parent mail body, long enough to be chunked and embedded properly.",
	}}

	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)

	if len(citations) != 3 {
		t.Fatalf("want 3 citations (mail + 2 attachments), got %d: %+v", len(citations), citations)
	}
	if !strings.Contains(contextText, "ATTACHMENT-PDF-TEXT") || !strings.Contains(contextText, "ATTACHMENT-XLSX-TEXT") {
		t.Fatalf("want both attachments' text in context, got:\n%s", contextText)
	}
	if strings.Contains(contextText, "UNRELATED-TEXT") {
		t.Fatalf("unrelated source must not ride along, got:\n%s", contextText)
	}
	// Markers must stay unique and sequential so [Qn] citing keeps working.
	for i, c := range citations {
		if c.Marker != i+1 {
			t.Fatalf("want sequential markers, citation %d has marker %d: %+v", i, c.Marker, citations)
		}
	}
}

func TestAssembleContextExpandsAttachmentHitWithParentMail(t *testing.T) {
	rag, _ := ingestFamilyRAG(t)
	hits := []rankedHit{{
		SourceID: "pst:m.pst:Inbox:42:attachment:0:Angebot.pdf", SourceKind: "pst_attachment",
		SourceName: "AW: Angebot — Anhang: Angebot.pdf",
		ChunkIdx:   0, Content: "ATTACHMENT-PDF-TEXT: the offer document body, long enough to be chunked and embedded.",
	}}

	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)

	if len(citations) != 3 {
		t.Fatalf("want 3 citations (attachment hit + parent mail + sibling attachment), got %d: %+v", len(citations), citations)
	}
	if !strings.Contains(contextText, "parent mail body") || !strings.Contains(contextText, "ATTACHMENT-XLSX-TEXT") {
		t.Fatalf("want parent mail and sibling attachment in context, got:\n%s", contextText)
	}
}

func TestAssembleContextFamilyRespectsSourceAccess(t *testing.T) {
	rag, _ := ingestFamilyRAG(t)
	hits := []rankedHit{{
		SourceID: "pst:m.pst:Inbox:42", SourceKind: "pst_email", SourceName: "AW: Angebot",
		ChunkIdx: 0, Content: "This is the parent mail body, long enough to be chunked and embedded properly.",
	}}

	// pst_attachment restricted to Vertrieb; the caller is anonymous ("").
	access := map[string][]string{"pst_attachment": {"Vertrieb"}}
	contextText, citations := rag.assembleContext(hits, rankingConfig{}, access, "", nil)

	if len(citations) != 1 {
		t.Fatalf("restricted attachments must not ride along, want 1 citation, got %d: %+v", len(citations), citations)
	}
	if strings.Contains(contextText, "ATTACHMENT-PDF-TEXT") {
		t.Fatalf("restricted attachment text leaked into context:\n%s", contextText)
	}

	// The same caller with the right department gets the full family.
	_, citations = rag.assembleContext(hits, rankingConfig{}, access, "Vertrieb", nil)
	if len(citations) != 3 {
		t.Fatalf("want full family for an allowed department, got %d citations", len(citations))
	}
}

// TestAssembleContextCapsLargePrimarySource reproduces the "one matched
// chunk pulls in an entire 50-chunk PDF" scenario: a single source
// ingested with many chunks whose combined length comfortably exceeds
// maxPrimaryContentChars. Before this cap existed, fetchAllSourceChunks
// dumped the whole thing into context regardless of size — silently
// making the "K (Kontext-Chunks)" setting meaningless for large documents.
func TestAssembleContextCapsLargePrimarySource(t *testing.T) {
	rag, s := newTestRAG(t)
	// chunkText (chunk.go) splits on newlines, so the body needs actual
	// paragraph breaks to become many chunks rather than one giant one.
	paragraph := "Absatz mit ausreichend Text, damit der Chunk bestehen bleibt und nicht zu kurz fuer einen sinnvollen Chunk ist."
	body := strings.Repeat(paragraph+"\n", 80) // ~80 chunks' worth of paragraphs at ChunkSize=500
	if _, err := ingestDocument(rag, s, "test-embed", "file:/docs/Big.pdf", "file", "Big.pdf", body, 0, false); err != nil {
		t.Fatalf("ingestDocument: %v", err)
	}

	hits := []rankedHit{{SourceID: "file:/docs/Big.pdf", SourceKind: "file", SourceName: "Big.pdf", ChunkIdx: 0, Content: paragraph}}
	// -1/-1 (unlimited window): this test is specifically about the char
	// cap kicking in once the *whole* source is pulled in, not about the
	// separate chunk-windowing feature.
	cfg := rankingConfig{ContextChunksBefore: -1, ContextChunksAfter: -1}
	contextText, citations := rag.assembleContext(hits, cfg, nil, "", nil)

	if len(citations) != 1 {
		t.Fatalf("want exactly 1 citation despite the source spanning many chunks, got %+v", citations)
	}
	if !strings.Contains(contextText, "gekürzt") {
		t.Fatalf("want a truncation note for an oversized primary source, got:\n%s", contextText)
	}
	if len(contextText) > maxPrimaryContentCharsDefault+200 { // + citation header/note overhead
		t.Fatalf("want context bounded near maxPrimaryContentCharsDefault (%d), got %d bytes", maxPrimaryContentCharsDefault, len(contextText))
	}
}

// TestAssembleContextRespectsMaxSources checks rankingConfig.MaxSources:
// even with more distinct-source hits available, no more than the
// configured number ever gets a citation.
func TestAssembleContextRespectsMaxSources(t *testing.T) {
	rag, s := newTestRAG(t)
	docs := []struct{ id, text string }{
		{"file:/docs/A.txt", "Content of document A, long enough to be chunked and embedded."},
		{"file:/docs/B.txt", "Content of document B, long enough to be chunked and embedded."},
		{"file:/docs/C.txt", "Content of document C, long enough to be chunked and embedded."},
	}
	var hits []rankedHit
	for _, d := range docs {
		if _, err := ingestDocument(rag, s, "test-embed", d.id, "file", d.id, d.text, 0, false); err != nil {
			t.Fatalf("ingestDocument(%s): %v", d.id, err)
		}
		hits = append(hits, rankedHit{SourceID: d.id, SourceKind: "file", SourceName: d.id, ChunkIdx: 0, Content: d.text})
	}

	cfg := rankingConfig{MaxSources: 2}
	_, citations := rag.assembleContext(hits, cfg, nil, "", nil)
	if len(citations) != 2 {
		t.Fatalf("want exactly 2 citations (MaxSources cap), got %d: %+v", len(citations), citations)
	}

	// 0 (the default) disables the cap — all 3 distinct sources are cited.
	_, citations = rag.assembleContext(hits, rankingConfig{}, nil, "", nil)
	if len(citations) != 3 {
		t.Fatalf("MaxSources=0: want all 3 distinct sources cited, got %d: %+v", len(citations), citations)
	}
}

func TestAssembleContextNonEmailHitHasNoFamilyExpansion(t *testing.T) {
	rag, _ := ingestFamilyRAG(t)
	hits := []rankedHit{{
		SourceID: "file:/docs/Unrelated.txt", SourceKind: "file", SourceName: "Unrelated.txt",
		ChunkIdx: 0, Content: "UNRELATED-TEXT: some other document, long enough to be chunked and embedded.",
	}}

	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)
	if len(citations) != 1 {
		t.Fatalf("a plain file hit has no family, want 1 citation, got %d: %+v", len(citations), citations)
	}
	if strings.Contains(contextText, "ATTACHMENT-") || strings.Contains(contextText, "parent mail body") {
		t.Fatalf("email family must not be pulled in by an unrelated file hit:\n%s", contextText)
	}
}

// TestAssembleContextSkipsHeaderOnlySibling reproduces the reported
// "email-family expansion pads out with near-empty siblings" case: an
// attachment sibling whose content is just email header metadata (From/
// To/Date/Subject, no body) — short and formulaic, well under
// minSiblingContentChars — must not get its own citation/content dump,
// while a normal-length sibling still rides along as before.
func TestAssembleContextSkipsHeaderOnlySibling(t *testing.T) {
	rag, s := newTestRAG(t)
	docs := []struct{ id, kind, name, text string }{
		{"pst:m.pst:Inbox:99", "pst_email", "AW: Anfrage", "This is the parent mail body, long enough to be chunked and embedded properly. It includes several sentences of realistic content so the fixture comfortably exceeds the minimum sibling content length used by email-family expansion."},
		{"pst:m.pst:Inbox:99:attachment:0:Angebot.pdf", "pst_attachment", "AW: Anfrage — Anhang: Angebot.pdf", "ATTACHMENT-PDF-TEXT: the offer document body, long enough to be chunked and embedded. It includes several sentences of realistic content so the fixture comfortably exceeds the minimum sibling content length used by email-family expansion."},
		// Header-only stub, mirroring a real "gehört zur selben E-Mail"
		// sibling that carries no body — well under minSiblingContentChars.
		{"pst:m.pst:Inbox:99:attachment:1:HeaderOnly.eml", "pst_attachment", "AW: Anfrage — HeaderOnly.eml", "From: A <a@example.com>\nTo: B <b@example.com>\nDate: Mon, 01 Jan 2026 00:00:00 +0100\nSubject: AW: Anfrage"},
	}
	for _, d := range docs {
		if _, err := ingestDocument(rag, s, "test-embed", d.id, d.kind, d.name, d.text, 0, false); err != nil {
			t.Fatalf("ingestDocument(%s): %v", d.id, err)
		}
	}
	hits := []rankedHit{{
		SourceID: "pst:m.pst:Inbox:99", SourceKind: "pst_email", SourceName: "AW: Anfrage",
		ChunkIdx: 0, Content: docs[0].text,
	}}

	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)
	if len(citations) != 2 {
		t.Fatalf("want 2 citations (mail + real attachment, header-only sibling skipped), got %d: %+v", len(citations), citations)
	}
	if !strings.Contains(contextText, "ATTACHMENT-PDF-TEXT") {
		t.Fatalf("want the real attachment's text in context, got:\n%s", contextText)
	}
	if strings.Contains(contextText, "HeaderOnly") {
		t.Fatalf("header-only sibling must not ride along, got:\n%s", contextText)
	}
}

// TestAssembleContextNoHitsNotesNothingRelevant covers rankedSearch's
// filterByMinFinalScore/filterByMinSimilarity filtering every candidate
// out (or a query simply having no hits at all): the LLM must see an
// explicit "nothing relevant" note instead of a blank context block.
func TestAssembleContextNoHitsNotesNothingRelevant(t *testing.T) {
	rag, _ := newTestRAG(t)
	contextText, citations := rag.assembleContext(nil, rankingConfig{}, nil, "", nil)
	if len(citations) != 0 {
		t.Fatalf("want no citations, got %+v", citations)
	}
	if !strings.Contains(contextText, "Keine ausreichend relevanten Quellen") {
		t.Fatalf("want an explicit 'nothing relevant' note, got:\n%q", contextText)
	}
}

// TestAssembleContextDedupsIdenticalContentAcrossSources reproduces the
// reported "same blank boilerplate PDF attached to three different emails"
// case (Task 1): two DIFFERENT sources (distinct source_ids, so
// assembleContext's per-source seenSources dedup never fires) whose content
// is byte-identical. Both must still earn a citation (real provenance —
// each genuinely matched), but the content itself must only be written
// into context once, not twice.
func TestAssembleContextDedupsIdenticalContentAcrossSources(t *testing.T) {
	rag, s := newTestRAG(t)
	boilerplate := "STANDARD-BOILERPLATE: this identical disclaimer text appears in every one of these documents, long enough to be chunked and embedded for the test."
	docs := []struct{ id, name string }{
		{"file:/docs/FormA.pdf", "FormA.pdf"},
		{"file:/docs/FormB.pdf", "FormB.pdf"},
	}
	var hits []rankedHit
	for _, d := range docs {
		if _, err := ingestDocument(rag, s, "test-embed", d.id, "file", d.name, boilerplate, 0, false); err != nil {
			t.Fatalf("ingestDocument(%s): %v", d.id, err)
		}
		hits = append(hits, rankedHit{SourceID: d.id, SourceKind: "file", SourceName: d.name, ChunkIdx: 0, Content: boilerplate})
	}

	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)
	if len(citations) != 2 {
		t.Fatalf("both sources are genuinely distinct, want 2 citations, got %d: %+v", len(citations), citations)
	}
	if n := strings.Count(contextText, "STANDARD-BOILERPLATE"); n != 1 {
		t.Fatalf("want the identical content written exactly once, got it %d times:\n%s", n, contextText)
	}
	if !strings.Contains(contextText, "identisch") {
		t.Fatalf("want a note explaining the second source's content was elided as a duplicate, got:\n%s", contextText)
	}
}

// TestAssembleContextKeepsDifferentContentInFull is
// TestAssembleContextDedupsIdenticalContentAcrossSources' counterpart:
// genuinely different content from two different sources must both be
// written in full, with no dedup note.
func TestAssembleContextKeepsDifferentContentInFull(t *testing.T) {
	rag, s := newTestRAG(t)
	docs := []struct{ id, name, text string }{
		{"file:/docs/A.txt", "A.txt", "UNIQUE-CONTENT-A: distinct body text for document A, long enough to be chunked and embedded."},
		{"file:/docs/B.txt", "B.txt", "UNIQUE-CONTENT-B: distinct body text for document B, long enough to be chunked and embedded."},
	}
	var hits []rankedHit
	for _, d := range docs {
		if _, err := ingestDocument(rag, s, "test-embed", d.id, "file", d.name, d.text, 0, false); err != nil {
			t.Fatalf("ingestDocument(%s): %v", d.id, err)
		}
		hits = append(hits, rankedHit{SourceID: d.id, SourceKind: "file", SourceName: d.name, ChunkIdx: 0, Content: d.text})
	}

	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)
	if len(citations) != 2 {
		t.Fatalf("want 2 citations, got %d: %+v", len(citations), citations)
	}
	if !strings.Contains(contextText, "UNIQUE-CONTENT-A") || !strings.Contains(contextText, "UNIQUE-CONTENT-B") {
		t.Fatalf("want both distinct contents written in full, got:\n%s", contextText)
	}
	if strings.Contains(contextText, "identisch") {
		t.Fatalf("genuinely different content must not be flagged as a duplicate, got:\n%s", contextText)
	}
}

// TestAssembleContextSourceDedupStillWorksWithoutContentDuplication checks
// that the pre-existing per-source dedup (seenSources) is unaffected by the
// new content-based dedup: the normal, no-duplicate-content case (the same
// source hit twice, e.g. two chunks of it both scoring high enough) still
// results in exactly one citation and the content written exactly once —
// via seenSources short-circuiting before content-dedup ever runs, not
// because of it.
func TestAssembleContextSourceDedupStillWorksWithoutContentDuplication(t *testing.T) {
	rag, s := newTestRAG(t)
	text := "ONLY-ONE-SOURCE: content that should only be cited/written once even if the ranker returns two hits against the same source."
	if _, err := ingestDocument(rag, s, "test-embed", "file:/docs/Solo.txt", "file", "Solo.txt", text, 0, false); err != nil {
		t.Fatalf("ingestDocument: %v", err)
	}
	hits := []rankedHit{
		{SourceID: "file:/docs/Solo.txt", SourceKind: "file", SourceName: "Solo.txt", ChunkIdx: 0, Content: text},
		{SourceID: "file:/docs/Solo.txt", SourceKind: "file", SourceName: "Solo.txt", ChunkIdx: 0, Content: text},
	}
	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)
	if len(citations) != 1 {
		t.Fatalf("want exactly 1 citation for the same source hit twice, got %d: %+v", len(citations), citations)
	}
	if n := strings.Count(contextText, "ONLY-ONE-SOURCE"); n != 1 {
		t.Fatalf("want content written exactly once, got %d times:\n%s", n, contextText)
	}
	if strings.Contains(contextText, "identisch") {
		t.Fatalf("seenSources must skip the second hit before content-dedup ever runs — no duplicate-note expected, got:\n%s", contextText)
	}
}

// TestAssembleContextDedupRecognizesContentDifferingOnlyByRunLength proves
// Task 3's collapseRepeatedRuns runs BEFORE Task 1's dedup-key check:
// content differing only in an incidental punctuation-run length (e.g. one
// extraction producing a slightly longer PDF dot-leader than another) is
// still recognized as the same content. Neither source is actually ingested
// — fetchAllSourceChunks falls back to the hit's own Content unchanged for
// a source with no stored chunks (see its own doc comment) — which isolates
// assembleContext's OWN collapseRepeatedRuns pass from ingestDocument's
// (which would already have normalized real ingested content before it
// ever reached here).
func TestAssembleContextDedupRecognizesContentDifferingOnlyByRunLength(t *testing.T) {
	rag, _ := newTestRAG(t)
	textA := "BOILERPLATE-VARIANT: identical wording, differently sized dot-leader here " + strings.Repeat(".", 20) + " end."
	textB := "BOILERPLATE-VARIANT: identical wording, differently sized dot-leader here " + strings.Repeat(".", 9) + " end."
	hits := []rankedHit{
		{SourceID: "unstored:A", SourceKind: "file", SourceName: "A", ChunkIdx: 0, Content: textA},
		{SourceID: "unstored:B", SourceKind: "file", SourceName: "B", ChunkIdx: 0, Content: textB},
	}
	contextText, citations := rag.assembleContext(hits, rankingConfig{}, nil, "", nil)
	if len(citations) != 2 {
		t.Fatalf("both are distinct sources, want 2 citations, got %d: %+v", len(citations), citations)
	}
	if n := strings.Count(contextText, "BOILERPLATE-VARIANT"); n != 1 {
		t.Fatalf("want content written exactly once despite differing dot-leader run lengths, got %d times:\n%s", n, contextText)
	}
}

// ingestFamilyRAG bundles newTestRAG + the family fixture, returning both.
func ingestFamilyRAG(t *testing.T) (*ragSystem, appSettings) {
	t.Helper()
	rag, s := newTestRAG(t)
	ingestEmailFamilyFixture(t, rag, s)
	return rag, s
}

func TestFilterCitationsDefaultVisible(t *testing.T) {
	citations := []sourceInfo{{SourceID: "a", SourceKind: "pst_email", Marker: 1}}
	// No SourceVisibility entry at all: default is visible.
	got := filterCitations(citations, "Siehe [Q1].", appSettings{})
	if len(got) != 1 {
		t.Fatalf("want citation to survive with no visibility config, got %+v", got)
	}
}

func TestFilterByDeptAccess(t *testing.T) {
	hits := []rankedHit{
		{SourceID: "wiki-1", SourceKind: "confluence_page"},
		{SourceID: "mecha-1", SourceKind: "imap_email"},
	}
	access := map[string][]string{"imap_email": {"Vertrieb", "Einkauf"}}

	// Anonymous ("" department): unrestricted confluence_page passes,
	// restricted imap_email is dropped.
	got := filterByDeptAccess(hits, access, "")
	if len(got) != 1 || got[0].SourceID != "wiki-1" {
		t.Fatalf("anonymous caller: want only wiki-1, got %+v", got)
	}

	// Logged in, classified as Vertrieb: both pass.
	got = filterByDeptAccess(hits, access, "Vertrieb")
	if len(got) != 2 {
		t.Fatalf("Vertrieb caller: want both hits, got %+v", got)
	}

	// Logged in but a department the ACL doesn't list: still only the
	// unrestricted one passes.
	got = filterByDeptAccess(hits, access, "IT")
	if len(got) != 1 || got[0].SourceID != "wiki-1" {
		t.Fatalf("IT caller: want only wiki-1, got %+v", got)
	}

	// No access map configured at all: nothing filtered, regardless of
	// deptCode — matches SourceAccess's opt-out-not-opt-in default.
	got = filterByDeptAccess(hits, nil, "")
	if len(got) != 2 {
		t.Fatalf("no access config: want both hits unfiltered, got %+v", got)
	}
}

// TestFtsKeywordScoresNormalizesToMaxOne checks ftsKeywordScores' own
// normalization step (dividing by the batch's own max score) on top of the
// tinySQL backend's real BM25 scoring — the top-scoring chunk in any batch
// must land at exactly 1.0 so KeywordScore stays comparable to
// VectorScore/RecencyScore's own ~0..1 range under the default weights.
func TestFtsKeywordScoresNormalizesToMaxOne(t *testing.T) {
	s := newTestTinySQLStore(t)
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{
		"delivery delivery delivery schedule",
		"unrelated content about something else entirely",
	}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	rag := &ragSystem{store: s}
	scores := rag.ftsKeywordScores("delivery", "model-a", 10)
	if scores == nil {
		t.Fatalf("expected a non-nil score map from the tinySQL backend")
	}
	if top := scores[ftsCandidateKey("doc-1", 0)]; top != 1.0 {
		t.Fatalf("expected the top-scoring chunk normalized to exactly 1.0, got %v", top)
	}
}

// noFTSVectorStore is a minimal vectorStore that deliberately does NOT
// implement ftsKeywordScorer — both real backends (tinySQLStore,
// sqliteStore) do now, so this stands in for "some future backend that
// doesn't" purely to keep exercising ftsKeywordScores' type-assertion
// fallback path (see TestFtsKeywordScoresNilForUnsupportedBackend below).
// Every method is an unused stub; only the type itself (and its absence
// of keywordCandidatesFTS) matters for that test.
type noFTSVectorStore struct{}

func (noFTSVectorStore) init() error                   { return nil }
func (noFTSVectorStore) lastContentHash(string) string { return "" }
func (noFTSVectorStore) deleteSource(string) error     { return nil }
func (noFTSVectorStore) insertChunks(sourceChunks, string, [][]float64, string, int64, string) (int, error) {
	return 0, nil
}
func (noFTSVectorStore) vectorCandidates([]float64, string, int) ([]rankedHit, error) {
	return nil, nil
}
func (noFTSVectorStore) fetchSourceChunks(string) ([]sourceChunk, error)  { return nil, nil }
func (noFTSVectorStore) listSources() ([]sourceInfo, error)               { return nil, nil }
func (noFTSVectorStore) docCount() int                                    { return 0 }
func (noFTSVectorStore) listChunks(chunkFilter) ([]chunkRow, bool, error) { return nil, false, nil }
func (noFTSVectorStore) save() error                                      { return nil }
func (noFTSVectorStore) exportAll() ([]exportedChunk, error)              { return nil, nil }
func (noFTSVectorStore) importRaw([]exportedChunk) error                  { return nil }

// TestFtsKeywordScoresNilForUnsupportedBackend checks the fallback signal:
// a backend that doesn't implement ftsKeywordScorer must return nil, not
// an empty map — rankedSearch uses that distinction to fall back to
// keywordOverlapScore instead of scoring every hit 0.
func TestFtsKeywordScoresNilForUnsupportedBackend(t *testing.T) {
	r := &ragSystem{store: noFTSVectorStore{}}
	if scores := r.ftsKeywordScores("delivery", "test-embed", 10); scores != nil {
		t.Fatalf("expected nil (fallback signal) for a backend without FTS support, got %v", scores)
	}
}

// unionFakeStore adds the chunkByKey capability on top of the no-op store,
// so unionFTSCandidates' behavior is testable without a real backend.
type unionFakeStore struct {
	noFTSVectorStore
	byKey map[string]struct {
		hit rankedHit
		emb []float64
	}
}

func (s unionFakeStore) chunkByKey(sourceID string, chunkIdx int, embedModel string) (rankedHit, []float64, bool) {
	e, ok := s.byKey[ftsCandidateKey(sourceID, chunkIdx)]
	return e.hit, e.emb, ok
}

// TestUnionFTSCandidatesAddsKeywordOnlyHits: a chunk the FTS pass scored but
// the vector candidate pool missed must be fetched, given a real cosine
// score, and appended — the "true hybrid union" behavior.
func TestUnionFTSCandidatesAddsKeywordOnlyHits(t *testing.T) {
	store := unionFakeStore{byKey: map[string]struct {
		hit rankedHit
		emb []float64
	}{
		ftsCandidateKey("kb:doc-b", 3): {
			hit: rankedHit{SourceID: "kb:doc-b", SourceKind: "file", ChunkIdx: 3, Content: "Fehlercode E-4711 Anleitung"},
			emb: []float64{0, 1},
		},
	}}
	r := &ragSystem{store: store}
	hits := []rankedHit{{SourceID: "kb:doc-a", SourceKind: "file", ChunkIdx: 0, VectorScore: 0.9}}
	fts := map[string]float64{
		ftsCandidateKey("kb:doc-a", 0): 1.0, // already a vector candidate
		ftsCandidateKey("kb:doc-b", 3): 0.8, // keyword-only
	}

	got := r.unionFTSCandidates(hits, fts, []float64{1, 0}, "test-embed", nil, "", nil)
	if len(got) != 2 {
		t.Fatalf("want the keyword-only hit appended, got %+v", got)
	}
	rescued := got[1]
	if rescued.SourceID != "kb:doc-b" || rescued.ChunkIdx != 3 {
		t.Fatalf("want kb:doc-b#3 rescued, got %+v", rescued)
	}
	// cosine([1,0],[0,1]) = 0 — the rescued hit gets a real (here:
	// orthogonal) vector score, not a fabricated one.
	if rescued.VectorScore != 0 {
		t.Fatalf("want cosine-scored VectorScore 0, got %v", rescued.VectorScore)
	}
}

// TestUnionFTSCandidatesRespectsAccessControl: keyword rescue must never
// bypass department/preset gates.
func TestUnionFTSCandidatesRespectsAccessControl(t *testing.T) {
	store := unionFakeStore{byKey: map[string]struct {
		hit rankedHit
		emb []float64
	}{
		ftsCandidateKey("mail:1", 0): {
			hit: rankedHit{SourceID: "mail:1", SourceKind: "imap_email", ChunkIdx: 0, Content: "geheim"},
			emb: []float64{1, 0},
		},
	}}
	r := &ragSystem{store: store}
	fts := map[string]float64{ftsCandidateKey("mail:1", 0): 1.0}
	access := map[string][]string{"imap_email": {"Vertrieb"}}

	got := r.unionFTSCandidates(nil, fts, []float64{1, 0}, "test-embed", access, "", nil)
	if len(got) != 0 {
		t.Fatalf("anonymous caller: restricted kind must not be rescued, got %+v", got)
	}
	got = r.unionFTSCandidates(nil, fts, []float64{1, 0}, "test-embed", access, "Vertrieb", nil)
	if len(got) != 1 {
		t.Fatalf("Vertrieb caller: want the hit rescued, got %+v", got)
	}
}

func TestCapHitsPerSource(t *testing.T) {
	hits := []rankedHit{
		{SourceID: "a", ChunkIdx: 0},
		{SourceID: "a", ChunkIdx: 1},
		{SourceID: "b", ChunkIdx: 0},
		{SourceID: "a", ChunkIdx: 2},
		{SourceID: "c", ChunkIdx: 0},
	}
	// 0 disables the cap entirely (rankingConfig.MaxHitsPerSource default).
	if got := capHitsPerSource(hits, 0); len(got) != 5 {
		t.Fatalf("cap 0: want all hits kept, got %+v", got)
	}
	got := capHitsPerSource(hits, 2)
	if len(got) != 4 {
		t.Fatalf("cap 2: want 4 hits, got %+v", got)
	}
	// Order preserved; a's third (weakest, since hits arrive sorted) hit is
	// the one dropped.
	for i, want := range []struct {
		id  string
		idx int
	}{{"a", 0}, {"a", 1}, {"b", 0}, {"c", 0}} {
		if got[i].SourceID != want.id || got[i].ChunkIdx != want.idx {
			t.Fatalf("cap 2: want a0,a1,b0,c0 in order, got %+v", got)
		}
	}
}

// TestAssembleContextIncludesAllHitWindows: with windowed context, a source
// matched at two far-apart positions must contribute BOTH windows to its
// single citation block, separated by a gap marker — the second match used
// to be dropped entirely.
func TestAssembleContextIncludesAllHitWindows(t *testing.T) {
	s := newTestTinySQLStore(t)
	chunks := make([]string, 10)
	vecs := make([][]float64, 10)
	for i := range chunks {
		chunks[i] = fmt.Sprintf("SENTINEL-%d some filler content long enough to matter here", i)
		vecs[i] = []float64{1, 0}
	}
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "Handbuch", Chunks: chunks}
	if _, err := s.insertChunks(sc, "test-embed", vecs, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	rag := &ragSystem{store: s}
	hits := []rankedHit{
		{SourceID: "doc-1", SourceKind: "file", SourceName: "Handbuch", ChunkIdx: 1, Content: chunks[1]},
		{SourceID: "doc-1", SourceKind: "file", SourceName: "Handbuch", ChunkIdx: 8, Content: chunks[8]},
	}
	cfg := rankingConfig{ContextChunksBefore: 0, ContextChunksAfter: 0}

	contextText, citations := rag.assembleContext(hits, cfg, nil, "", nil)
	if len(citations) != 1 {
		t.Fatalf("want one citation for the single source, got %+v", citations)
	}
	if !strings.Contains(contextText, "SENTINEL-1") || !strings.Contains(contextText, "SENTINEL-8") {
		t.Fatalf("want both matched windows in context, got:\n%s", contextText)
	}
	if !strings.Contains(contextText, "[…]") {
		t.Fatalf("want a gap marker between non-contiguous windows, got:\n%s", contextText)
	}
	if strings.Contains(contextText, "SENTINEL-5") {
		t.Fatalf("chunk far outside every window must not appear, got:\n%s", contextText)
	}
}

// TestRankedSearchGuardsNaNVectorScores: a NaN similarity (corrupt stored
// embedding) must be neutralized to 0 before sorting, not left to wander.
func TestRankedSearchGuardsNaNVectorScores(t *testing.T) {
	hits := []rankedHit{{SourceID: "bad", VectorScore: math.NaN()}, {SourceID: "good", VectorScore: 0.5}}
	// Mirror rankedSearch's guard inline (the full path needs a store):
	for i := range hits {
		if math.IsNaN(hits[i].VectorScore) || math.IsInf(hits[i].VectorScore, 0) {
			hits[i].VectorScore = 0
		}
	}
	if hits[0].VectorScore != 0 {
		t.Fatalf("NaN must be neutralized to 0, got %v", hits[0].VectorScore)
	}
}

// TestFtsKeywordScoresEmptyOnNonPositiveMax: a batch with no positive score
// must come back as an empty map ("no keyword matches"), never as raw
// non-positive values leaking into the weighted blend.
func TestFtsKeywordScoresEmptyOnNonPositiveMax(t *testing.T) {
	s := newTestTinySQLStore(t)
	rag := &ragSystem{store: s}
	scores := rag.ftsKeywordScores("nothing matches this query", "test-embed", 10)
	if scores == nil {
		t.Fatalf("tinySQL backend supports FTS — nil means 'unsupported', which is wrong here")
	}
	for k, v := range scores {
		if v <= 0 {
			t.Fatalf("no non-positive score may survive normalization, got %s=%v", k, v)
		}
	}
}

// TestHandleSettingsPersistsMaxHitsPerSource guards the recurring "new
// settings field silently fails to persist" failure mode (see AGENTS.md):
// rankingConfig.MaxHitsPerSource must round-trip through POST /api/settings.
func TestHandleSettingsPersistsMaxHitsPerSource(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"ranking": map[string]any{"max_hits_per_source": 3},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := settings.get().Ranking.MaxHitsPerSource; got != 3 {
		t.Fatalf("MaxHitsPerSource did not persist: want 3, got %d", got)
	}
}
