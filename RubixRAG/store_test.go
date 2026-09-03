package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceFilterMatches(t *testing.T) {
	pdf := sourceInfo{SourceID: "file:/docs/Angebot.pdf", SourceKind: "file", SourceName: "Angebot.pdf"}
	docx := sourceInfo{SourceID: "file:/docs/Vertrag.docx", SourceKind: "file", SourceName: "Vertrag.docx"}
	attachment := sourceInfo{SourceID: "pst:mail.pst:attachment:0:Angebot.pdf", SourceKind: "pst_attachment", SourceName: "Re: Angebot — Anhang: Angebot.pdf"}

	cases := []struct {
		name string
		f    sourceFilter
		src  sourceInfo
		want bool
	}{
		{"empty filter matches everything", sourceFilter{}, pdf, true},
		{"kind match", sourceFilter{Kind: "file"}, pdf, true},
		{"kind mismatch", sourceFilter{Kind: "pst_attachment"}, pdf, false},
		{"extension match", sourceFilter{Extension: ".pdf"}, pdf, true},
		{"extension mismatch", sourceFilter{Extension: ".pdf"}, docx, false},
		{"extension matches attachment by name suffix regardless of kind", sourceFilter{Extension: ".pdf"}, attachment, true},
		{"kind+extension both required (AND)", sourceFilter{Kind: "file", Extension: ".pdf"}, attachment, false},
		{"query matches source name substring", sourceFilter{Query: "angebot"}, pdf, true},
		{"query matches source id substring", sourceFilter{Query: "mail.pst"}, attachment, true},
		{"query mismatch", sourceFilter{Query: "invoice"}, docx, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.matches(c.src); got != c.want {
				t.Errorf("%+v.matches(%+v) = %v, want %v", c.f, c.src, got, c.want)
			}
		})
	}
}

func TestDeleteSourcesByFilterDeletesOnlyMatches(t *testing.T) {
	rag, s := newTestRAG(t)

	docs := []struct{ id, kind, name, text string }{
		{"file:/docs/Angebot.pdf", "file", "Angebot.pdf", "This is a long enough paragraph of body text about an offer to be chunked and embedded."},
		{"file:/docs/Vertrag.docx", "file", "Vertrag.docx", "This is a long enough paragraph of body text about a contract to be chunked and embedded."},
		{"pst:mail.pst:attachment:0:Angebot.pdf", "pst_attachment", "Re: Angebot — Anhang: Angebot.pdf", "This is a long enough paragraph of attachment body text to be chunked and embedded."},
	}
	for _, d := range docs {
		if _, err := ingestDocument(rag, s, "test-embed", d.id, d.kind, d.name, d.text, 0, false); err != nil {
			t.Fatalf("ingestDocument(%s): %v", d.id, err)
		}
	}

	// The dry-run count must agree with what the delete below then does,
	// and must not itself delete anything.
	c, err := rag.countSourcesByFilter(sourceFilter{Extension: ".pdf"})
	if err != nil {
		t.Fatalf("countSourcesByFilter: %v", err)
	}
	if c != 2 {
		t.Fatalf("want count 2 before deleting, got %d", c)
	}
	if all, _ := rag.listSources(); len(all) != 3 {
		t.Fatalf("countSourcesByFilter must not delete — want 3 sources still present, got %d", len(all))
	}

	n, err := rag.deleteSourcesByFilter(sourceFilter{Extension: ".pdf"})
	if err != nil {
		t.Fatalf("deleteSourcesByFilter: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 PDFs deleted (file + pst_attachment), got %d", n)
	}

	remaining, err := rag.listSources()
	if err != nil {
		t.Fatalf("listSources: %v", err)
	}
	if len(remaining) != 1 || remaining[0].SourceID != "file:/docs/Vertrag.docx" {
		t.Fatalf("want only Vertrag.docx left, got %+v", remaining)
	}
}

// TestFetchAttachmentSourceContentsMatchesOnlyOwnAttachments confirms the
// prefix match handleDraftReply relies on (fetchAttachmentSourceContents)
// picks up every attachment ingested under one parent email's source_id
// prefix, in the right shape, and does NOT bleed into an unrelated parent's
// own attachments or the parent email's own body source.
func TestFetchAttachmentSourceContentsMatchesOnlyOwnAttachments(t *testing.T) {
	rag, s := newTestRAG(t)

	const parentA = "pst:mail.pst:folder:msg-1"
	const parentB = "pst:mail.pst:folder:msg-2"
	docs := []struct{ id, kind, name, text string }{
		{parentA, "pst_email", "Anfrage", "This is the parent email's own body text, long enough to be chunked."},
		{parentA + ":attachment:0:invoice.txt", "pst_attachment", "Anfrage — Anhang: invoice.txt", "Anhang \"invoice.txt\" zu E-Mail \"Anfrage\": Rechnungsbetrag 42 EUR, long enough to be chunked."},
		{parentA + ":attachment:1:terms.txt", "pst_attachment", "Anfrage — Anhang: terms.txt", "Anhang \"terms.txt\" zu E-Mail \"Anfrage\": Zahlungsziel 30 Tage, long enough to be chunked."},
		{parentB + ":attachment:0:other.txt", "pst_attachment", "Andere Mail — Anhang: other.txt", "Anhang \"other.txt\" zu einer völlig anderen E-Mail, long enough to be chunked."},
	}
	for _, d := range docs {
		if _, err := ingestDocument(rag, s, "test-embed", d.id, d.kind, d.name, d.text, 0, false); err != nil {
			t.Fatalf("ingestDocument(%s): %v", d.id, err)
		}
	}

	contents := rag.fetchAttachmentSourceContents(parentA)
	if len(contents) != 2 {
		t.Fatalf("want 2 attachments for parentA, got %d: %+v", len(contents), contents)
	}
	joined := strings.Join(contents, "\n")
	if !strings.Contains(joined, "Rechnungsbetrag 42 EUR") || !strings.Contains(joined, "Zahlungsziel 30 Tage") {
		t.Fatalf("want both parentA attachments' content, got %q", joined)
	}
	if strings.Contains(joined, "völlig anderen") {
		t.Fatalf("must not include parentB's attachment content, got %q", joined)
	}
	if strings.Contains(joined, "parent email's own body") {
		t.Fatalf("must not include the parent email's own body source, got %q", joined)
	}

	if none := rag.fetchAttachmentSourceContents("pst:mail.pst:folder:no-such-msg"); len(none) != 0 {
		t.Fatalf("want no attachments for an unknown parent, got %+v", none)
	}
}

// newCountingTestRAG mirrors newTestRAG (imapmail_test.go) but counts every
// input string the fake embedding server is asked to embed, and can be
// switched into a failure mode — the two knobs the replaceSourceChunks
// reuse/ordering tests below need.
func newCountingTestRAG(t *testing.T) (*ragSystem, appSettings, *int, *bool) {
	t.Helper()
	embedCalls := 0
	fail := false
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if fail {
			http.Error(w, "embedding backend down", 500)
			return
		}
		embedCalls += len(req.Input)
		var resp embResp
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float64 `json:"embedding"`
			}{Embedding: []float64{0.1, 0.2, 0.3}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(embedServer.Close)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := newVectorStore(storageSettings{Backend: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatalf("newVectorStore: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(*sqliteStore); ok {
			closer.Close()
		}
	})

	embedClient := newLMClientFull("local", embedServer.URL, "", "test-embed", "test-chat", "")
	rag := newRAG(embedClient, map[string]*lmClient{"local": embedClient}, "local", store)
	if err := rag.init(); err != nil {
		t.Fatalf("rag.init: %v", err)
	}
	return rag, appSettings{ChunkSize: 500, K: 5}, &embedCalls, &fail
}

// TestReplaceSourceChunksReusesUnchangedEmbeddings: re-ingesting a source
// where only one paragraph changed must only embed the changed chunk(s) —
// the unchanged chunks' embeddings are carried over from the stored rows.
func TestReplaceSourceChunksReusesUnchangedEmbeddings(t *testing.T) {
	rag, s, embedCalls, _ := newCountingTestRAG(t)
	s.ChunkSize = 60 // small chunks so the fixture spans several

	paraA := "Erster Absatz mit stabilem Inhalt, unverändert."
	paraB := "Zweiter Absatz, auch stabil und unverändert."
	paraC := "Dritter Absatz, Version eins."
	text := paraA + "\n\n" + paraB + "\n\n" + paraC
	out, err := ingestDocument(rag, s, "test-embed", "doc:reuse", "file", "reuse.txt", text, 0, false)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if out.Chunks < 3 {
		t.Fatalf("fixture must span at least 3 chunks, got %d", out.Chunks)
	}
	firstRun := *embedCalls
	if firstRun != out.Chunks {
		t.Fatalf("first ingest must embed every chunk: %d chunks but %d embed inputs", out.Chunks, firstRun)
	}

	textV2 := paraA + "\n\n" + paraB + "\n\n" + "Dritter Absatz, Version zwei — geändert."
	out2, err := ingestDocument(rag, s, "test-embed", "doc:reuse", "file", "reuse.txt", textV2, 0, false)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	rerun := *embedCalls - firstRun
	if rerun >= out2.Chunks {
		t.Fatalf("re-ingest embedded all %d chunks again (%d embed inputs) — unchanged chunks must be reused", out2.Chunks, rerun)
	}
	if rerun < 1 {
		t.Fatalf("the changed chunk must be re-embedded, got %d embed inputs", rerun)
	}

	// The stored content reflects V2.
	content, ok := rag.fetchSourceContent("doc:reuse")
	if !ok || !strings.Contains(content, "Version zwei") {
		t.Fatalf("stored content must be the new version, got ok=%v content=%q", ok, content)
	}
}

// TestReplaceSourceChunksPreservesOldChunksOnEmbedFailure guards the
// data-loss fix: embedding now happens BEFORE the old chunks are deleted, so
// a failing embedding backend leaves the previous version searchable instead
// of destroying it.
func TestReplaceSourceChunksPreservesOldChunksOnEmbedFailure(t *testing.T) {
	rag, s, _, fail := newCountingTestRAG(t)

	text := "Ursprünglicher Inhalt, lang genug für einen ordentlichen Chunk mit ausreichend Substanz."
	if _, err := ingestDocument(rag, s, "test-embed", "doc:keep", "file", "keep.txt", text, 0, false); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	*fail = true
	_, err := ingestDocument(rag, s, "test-embed", "doc:keep", "file", "keep.txt", text+" Nachtrag, der neu eingebettet werden müsste.", 0, false)
	if err == nil {
		t.Fatalf("want an error while the embedding backend is down")
	}

	content, ok := rag.fetchSourceContent("doc:keep")
	if !ok || !strings.Contains(content, "Ursprünglicher Inhalt") {
		t.Fatalf("old chunks must survive an embed failure, got ok=%v content=%q", ok, content)
	}
	if strings.Contains(content, "Nachtrag") {
		t.Fatalf("the failed new version must not be half-written, got %q", content)
	}
}

// TestStripSectionMarker: fetchSourceContent must remove the per-chunk
// "[Abschnitt: …]" breadcrumb lines chunkText prefixes (chunk.go), and leave
// everything else alone.
func TestStripSectionMarker(t *testing.T) {
	if got := stripSectionMarker("[Abschnitt: A > B]\nInhalt"); got != "Inhalt" {
		t.Fatalf("want breadcrumb stripped, got %q", got)
	}
	if got := stripSectionMarker("Inhalt ohne Marker"); got != "Inhalt ohne Marker" {
		t.Fatalf("marker-free content must pass through, got %q", got)
	}
	// Only a LEADING marker is a breadcrumb — mid-text occurrences stay.
	mid := "Inhalt\n[Abschnitt: X]\nmehr"
	if got := stripSectionMarker(mid); got != mid {
		t.Fatalf("mid-text marker must stay, got %q", got)
	}
}

// TestEmbedQueryCachedAvoidsRepeatEmbedCalls: the second identical query
// must be served from the cache — zero additional embedding inputs.
func TestEmbedQueryCachedAvoidsRepeatEmbedCalls(t *testing.T) {
	rag, s, embedCalls, _ := newCountingTestRAG(t)
	if _, err := ingestDocument(rag, s, "test-embed", "doc:q", "file", "q.txt", "Ein Dokument mit genug Inhalt für einen ordentlichen Chunk im Testindex.", 0, false); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	base := *embedCalls

	if _, err := rag.rankedSearch("Wie lautet der Inhalt?", 3, rankingConfig{}, "test-embed", nil, "", nil); err != nil {
		t.Fatalf("first search: %v", err)
	}
	afterFirst := *embedCalls
	if afterFirst != base+1 {
		t.Fatalf("first search must embed the query once, got %d extra", afterFirst-base)
	}
	if _, err := rag.rankedSearch("Wie lautet der Inhalt?", 3, rankingConfig{}, "test-embed", nil, "", nil); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if *embedCalls != afterFirst {
		t.Fatalf("repeated query must be served from cache, got %d extra embed inputs", *embedCalls-afterFirst)
	}

	// A different query misses the cache.
	if _, err := rag.rankedSearch("Etwas völlig anderes?", 3, rankingConfig{}, "test-embed", nil, "", nil); err != nil {
		t.Fatalf("third search: %v", err)
	}
	if *embedCalls != afterFirst+1 {
		t.Fatalf("a new query must embed once, got %d extra", *embedCalls-afterFirst)
	}

	// setLLM drops the cache (new client may embed differently).
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": rag.getEmbedLM()}, "local")
	if _, err := rag.rankedSearch("Wie lautet der Inhalt?", 3, rankingConfig{}, "test-embed", nil, "", nil); err != nil {
		t.Fatalf("post-setLLM search: %v", err)
	}
	if *embedCalls != afterFirst+2 {
		t.Fatalf("setLLM must invalidate the cache, got %d extra", *embedCalls-afterFirst)
	}
}
