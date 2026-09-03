package main

import (
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// XML Tool Protocol Tests
// ─────────────────────────────────────────────────────────────────────────────

// ── parseXMLBlock ────────────────────────────────────────────────────────────

func TestParseXMLBlock_ValidQuery(t *testing.T) {
	block := `<tool name="rag_knowledge"><query>product specifications</query></tool>`
	call, ok := parseXMLBlock(block)
	if !ok {
		t.Fatal("expected successful parse")
	}
	if call.Name != "rag_knowledge" {
		t.Errorf("expected name=rag_knowledge, got %q", call.Name)
	}
	if call.Query != "product specifications" {
		t.Errorf("expected query=%q, got %q", "product specifications", call.Query)
	}
	if call.Raw != block {
		t.Errorf("expected Raw to be original block")
	}
	if call.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestParseXMLBlock_URLElement(t *testing.T) {
	block := `<tool name="url_fetch"><url>https://example.com/page</url></tool>`
	call, ok := parseXMLBlock(block)
	if !ok {
		t.Fatal("expected successful parse")
	}
	if call.Name != "url_fetch" {
		t.Errorf("expected name=url_fetch, got %q", call.Name)
	}
	if call.Query != "https://example.com/page" {
		t.Errorf("expected URL in Query, got %q", call.Query)
	}
}

func TestParseXMLBlock_SourceElement(t *testing.T) {
	block := `<tool name="nanogo"><source>fmt.Println(2+2)</source></tool>`
	call, ok := parseXMLBlock(block)
	if !ok {
		t.Fatal("expected successful parse")
	}
	if call.Query != "fmt.Println(2+2)" {
		t.Errorf("expected source in Query, got %q", call.Query)
	}
}

func TestParseXMLBlock_InputElement(t *testing.T) {
	block := `<tool name="customer_lookup"><input>{"id":"42"}</input></tool>`
	call, ok := parseXMLBlock(block)
	if !ok {
		t.Fatal("expected successful parse")
	}
	if call.Query != `{"id":"42"}` {
		t.Errorf("expected input JSON in Query, got %q", call.Query)
	}
}

func TestParseXMLBlock_StructuredArguments(t *testing.T) {
	block := `<tool name="customer_lookup"><arguments>{"customer_id":"42","include_history":true}</arguments></tool>`
	call, ok := parseXMLBlock(block)
	if !ok {
		t.Fatal("expected successful structured-argument parse")
	}
	if got := call.Arguments["customer_id"]; got != "42" {
		t.Fatalf("customer_id = %#v, want 42", got)
	}
	if got, ok := call.Arguments["include_history"].(bool); !ok || !got {
		t.Fatalf("include_history = %#v, want true", call.Arguments["include_history"])
	}
}

func TestParseXMLBlock_RejectsInvalidStructuredArguments(t *testing.T) {
	block := `<tool name="customer_lookup"><arguments>{not-json}</arguments></tool>`
	if _, ok := parseXMLBlock(block); ok {
		t.Fatal("invalid structured arguments must be rejected")
	}
}

func TestParseXMLBlock_EmptyName(t *testing.T) {
	block := `<tool name=""><query>test</query></tool>`
	_, ok := parseXMLBlock(block)
	if ok {
		t.Error("expected parse failure for empty name")
	}
}

func TestParseXMLBlock_EmptyContent(t *testing.T) {
	block := `<tool name="websearch"><query></query></tool>`
	_, ok := parseXMLBlock(block)
	if ok {
		t.Error("expected parse failure for empty content")
	}
}

func TestParseXMLBlock_InvalidXML(t *testing.T) {
	block := `<tool name="test"><query>foo`
	_, ok := parseXMLBlock(block)
	if ok {
		t.Error("expected parse failure for truncated XML")
	}
}

func TestParseXMLBlock_NestedToolRejected(t *testing.T) {
	block := `<tool name="outer"><query>before <tool name="inner"><query>x</query></tool> after</query></tool>`
	_, ok := parseXMLBlock(block)
	if ok {
		t.Error("expected parse failure for nested tool block")
	}
}

func TestParseXMLBlock_Whitespace(t *testing.T) {
	block := `<tool name="  websearch  "><query>  hello world  </query></tool>`
	call, ok := parseXMLBlock(block)
	if !ok {
		t.Fatal("expected parse success")
	}
	if call.Name != "websearch" {
		t.Errorf("expected trimmed name, got %q", call.Name)
	}
	if call.Query != "hello world" {
		t.Errorf("expected trimmed query, got %q", call.Query)
	}
}

// ── XMLParseState.Feed ───────────────────────────────────────────────────────

func TestFeed_PlainText(t *testing.T) {
	p := &XMLParseState{}
	res := p.Feed("Hello, world!")
	if !strings.Contains(res.Visible, "Hello") {
		t.Errorf("expected visible text, got %q", res.Visible)
	}
	if len(res.Calls) != 0 {
		t.Errorf("expected no calls, got %d", len(res.Calls))
	}
}

func TestFeed_SingleCompleteBlock(t *testing.T) {
	p := &XMLParseState{}
	res := p.Feed(`I will search now. <tool name="websearch"><query>tinyRAG</query></tool> Done.`)
	if len(res.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(res.Calls))
	}
	if res.Calls[0].Name != "websearch" {
		t.Errorf("expected websearch, got %q", res.Calls[0].Name)
	}
	if res.Calls[0].Query != "tinyRAG" {
		t.Errorf("expected tinyRAG query, got %q", res.Calls[0].Query)
	}
}

func TestFeed_PartialBlockNotTriggered(t *testing.T) {
	p := &XMLParseState{}
	// Feed only the opening, no closing tag
	res := p.Feed(`Here is some text <tool name="websearch"><query>hello`)
	if len(res.Calls) != 0 {
		t.Errorf("partial block must not trigger execution, got %d calls", len(res.Calls))
	}
	// Complete it in the next Feed
	res2 := p.Feed(`</query></tool> rest`)
	if len(res2.Calls) != 1 {
		t.Fatalf("expected 1 call after completing the block, got %d", len(res2.Calls))
	}
	if res2.Calls[0].Query != "hello" {
		t.Errorf("expected query=hello, got %q", res2.Calls[0].Query)
	}
}

func TestFeed_OversizedPartialBlockIsRejectedAndBounded(t *testing.T) {
	p := &XMLParseState{}
	input := `<tool name="websearch"><query>` + strings.Repeat("x", maxXMLToolBlockBytes)
	res := p.Feed(input)
	if len(res.Calls) != 0 || res.ParseErrors == 0 {
		t.Fatalf("oversized incomplete block must be rejected, got %+v", res)
	}
	if len(p.buf) >= maxXMLToolBlockBytes {
		t.Fatalf("parser retained an oversized block: %d bytes", len(p.buf))
	}
	if !strings.Contains(res.Visible, "Tool-Aufruf abgelehnt") {
		t.Fatalf("expected visible rejection marker, got %q", truncate(res.Visible, 120))
	}
}

func TestFeed_OversizedCompleteBlockIsRejectedAndBounded(t *testing.T) {
	p := &XMLParseState{}
	input := `<tool name="websearch"><query>` + strings.Repeat("x", maxXMLToolBlockBytes) + `</query></tool> after`
	res := p.Feed(input)
	if len(res.Calls) != 0 || res.ParseErrors == 0 {
		t.Fatalf("oversized complete block must be rejected, got %+v", res)
	}
	if len(res.Visible) > maxXMLToolBlockBytes+128 {
		t.Fatalf("oversized complete block leaked %d bytes into visible output", len(res.Visible))
	}
	if !strings.Contains(stripXMLToolCalls(res.Visible), "after") {
		t.Fatalf("rejected block must not hide following answer text: %q", truncate(res.Visible, 120))
	}
}

func TestFeed_PartialTag_HeldBack(t *testing.T) {
	p := &XMLParseState{}
	// Feed text that ends with a partial start tag
	res := p.Feed("some text <too")
	// The "<too" might be start of "<tool" so it could be held back
	// But "some text " should be visible
	if !strings.Contains(res.Visible, "some text") {
		t.Errorf("expected 'some text' in visible, got %q", res.Visible)
	}
}

func TestFeed_MultipleBlocksInOneChunk(t *testing.T) {
	p := &XMLParseState{}
	input := `<tool name="rag_knowledge"><query>specs</query></tool> and <tool name="websearch"><query>news</query></tool>`
	res := p.Feed(input)
	if len(res.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(res.Calls))
	}
	if res.Calls[0].Name != "rag_knowledge" {
		t.Errorf("expected first call rag_knowledge, got %q", res.Calls[0].Name)
	}
	if res.Calls[1].Name != "websearch" {
		t.Errorf("expected second call websearch, got %q", res.Calls[1].Name)
	}
}

func TestFeed_InvalidXMLHandledSafely(t *testing.T) {
	p := &XMLParseState{}
	// A block with empty name attribute triggers parse detection but fails validation.
	// Note: must use "<tool " (with space) for the parser to detect it.
	input := `text <tool name=""><query>test</query></tool> more`
	res := p.Feed(input)
	// Should not panic, should return visible text
	_ = res
	if res.ParseErrors != 1 {
		t.Errorf("expected 1 parse error for empty name attr, got %d", res.ParseErrors)
	}
}

func TestFeed_NestedToolHandledSafely(t *testing.T) {
	p := &XMLParseState{}
	input := `text <tool name="outer"><query>before <tool name="inner"><query>x</query></tool> after</query></tool> more`
	res := p.Feed(input)
	if len(res.Calls) != 0 {
		t.Fatalf("nested tool block must not trigger execution, got %d calls", len(res.Calls))
	}
	if res.ParseErrors == 0 {
		t.Error("expected parse error for nested tool block")
	}
	if !strings.Contains(res.Visible, `<tool name="outer"`) {
		t.Errorf("expected invalid XML to remain visible, got %q", res.Visible)
	}
}

func TestFeed_SplitAcrossChunks(t *testing.T) {
	p := &XMLParseState{}
	chunks := []string{
		"Let me ",
		"use a tool ",
		"<tool name=\"calc",
		"ulate\"><query>",
		"2+2</query>",
		"</tool>",
		" done",
	}
	var totalCalls []XMLToolCall
	var totalVisible string
	for _, c := range chunks {
		res := p.Feed(c)
		totalVisible += res.Visible
		totalCalls = append(totalCalls, res.Calls...)
	}
	if len(totalCalls) != 1 {
		t.Fatalf("expected 1 call after all chunks, got %d", len(totalCalls))
	}
	if totalCalls[0].Name != "calculate" {
		t.Errorf("expected name=calculate, got %q", totalCalls[0].Name)
	}
}

func TestFeed_Flush(t *testing.T) {
	p := &XMLParseState{}
	p.Feed("prefix <tool name=\"ws\"><query>q")
	tail := p.Flush()
	// Remaining buffer should be flushed as visible text
	if tail == "" {
		t.Error("expected non-empty flush output")
	}
}

// ── stripXMLToolCalls ────────────────────────────────────────────────────────

func TestStripXMLToolCalls(t *testing.T) {
	input := `I need to search. <tool name="websearch"><query>test</query></tool> Here is the answer.`
	got := stripXMLToolCalls(input)
	if strings.Contains(got, "<tool") {
		t.Errorf("expected no XML tags in output, got %q", got)
	}
	if !strings.Contains(got, "Here is the answer") {
		t.Errorf("expected answer text preserved, got %q", got)
	}
}

func TestStripXMLToolCalls_NoTools(t *testing.T) {
	input := "A plain answer with no tools."
	got := stripXMLToolCalls(input)
	if got != input {
		t.Errorf("expected unchanged string, got %q", got)
	}
}

// ── extractAllXMLToolCalls ───────────────────────────────────────────────────

func TestExtractAllXMLToolCalls(t *testing.T) {
	input := `<tool name="a"><query>q1</query></tool> text <tool name="b"><url>http://x.com</url></tool>`
	calls := extractAllXMLToolCalls(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "a" || calls[1].Name != "b" {
		t.Errorf("unexpected names: %q, %q", calls[0].Name, calls[1].Name)
	}
}

// ── partialPrefixLen ─────────────────────────────────────────────────────────

func TestPartialPrefixLen(t *testing.T) {
	tests := []struct {
		s      string
		target string
		want   int
	}{
		{"abc", "<tool ", 0},
		{"abc <", "<tool ", 1},
		{"abc <t", "<tool ", 2},
		{"abc <to", "<tool ", 3},
		{"abc <too", "<tool ", 4},
		{"abc <tool", "<tool ", 5},
		{"abc <tool ", "<tool ", 0}, // full match not partial
	}
	for _, tt := range tests {
		got := partialPrefixLen(tt.s, tt.target)
		if got != tt.want {
			t.Errorf("partialPrefixLen(%q, %q) = %d, want %d", tt.s, tt.target, got, tt.want)
		}
	}
}
