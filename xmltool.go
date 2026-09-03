package main

// ─────────────────────────────────────────────────────────────────────────────
// XML Tool Protocol
//
// Defines the inline XML tool-call protocol used by the streaming answer
// engine.  The LLM emits self-contained XML blocks inside its streamed text:
//
//   <tool name="TOOL_NAME"><query>CONTENT</query></tool>
//
// Variants:
//   <tool name="rag_knowledge"><query>search terms</query></tool>
//   <tool name="url_fetch"><url>https://example.com/page</url></tool>
//   <tool name="nanogo"><source>fmt.Println(2+2)</source></tool>
//
// Rules enforced here:
//   - Partial XML must NOT trigger execution (only complete blocks).
//   - Invalid XML is safely emitted as visible text and logged.
//   - The same block (same ID) must not be executed twice (dedup via ID map).
//   - Nested <tool> tags are not supported and are treated as invalid.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"strings"
	"time"
)

// xmlToolOpen is the prefix we look for to detect a potential tool block.
const xmlToolOpen = "<tool "

// xmlToolClose is the suffix that terminates a tool block.
const xmlToolClose = "</tool>"

// Tool-call payloads originate from a model and are therefore bounded before
// they reach an executor. The XML parser also limits an unfinished block so a
// malformed stream cannot make its buffer grow without bound.
const (
	maxXMLToolBlockBytes  = 16 * 1024
	maxXMLToolQueryRunes  = 4096
	maxXMLToolArgumentLen = 8 * 1024
)

// XMLToolCall represents a fully parsed, validated inline tool invocation.
type XMLToolCall struct {
	// Name is the tool identifier (e.g. "rag_knowledge", "url_fetch").
	Name string
	// Query is the primary input argument (content of <query> element).
	// For url_fetch the <url> element is mapped here.
	// For nanogo the <source> element is mapped here.
	Query string
	// Arguments carries structured JSON inputs for tools that need more than a
	// single free-text query. It is optional so existing <query>-based calls
	// remain fully compatible.
	Arguments map[string]any
	// Reason is planner-only metadata. It is never accepted from streamed XML
	// and is included solely to make planned tool runs explainable.
	Reason string
	// Raw holds the original XML text as emitted by the model.
	Raw string
	// ID is a unique call identifier used for deduplication.
	ID string
}

// xmlToolRaw is the internal struct used for XML decoding.
type xmlToolRaw struct {
	XMLName   xml.Name `xml:"tool"`
	Name      string   `xml:"name,attr"`
	Query     string   `xml:"query"`
	URL       string   `xml:"url"`
	Source    string   `xml:"source"`
	Input     string   `xml:"input"`
	Args      string   `xml:"args"`
	Arguments string   `xml:"arguments"`
}

// parseXMLBlock attempts to parse a complete `<tool …>…</tool>` block.
// It returns the parsed call and true on success, or zero value and false
// on any parse/validation failure.
func parseXMLBlock(block string) (XMLToolCall, bool) {
	if len(block) > maxXMLToolBlockBytes {
		log.Printf("xmltool: oversized tool block rejected (%d bytes)", len(block))
		return XMLToolCall{}, false
	}
	if strings.Contains(strings.TrimPrefix(block, xmlToolOpen), xmlToolOpen) {
		log.Printf("xmltool: nested tool block rejected: %q", truncate(block, 120))
		return XMLToolCall{}, false
	}

	var raw xmlToolRaw
	if err := xml.Unmarshal([]byte(block), &raw); err != nil {
		log.Printf("xmltool: parse error (%v) for block: %q", err, truncate(block, 120))
		return XMLToolCall{}, false
	}
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		log.Printf("xmltool: block has empty name: %q", truncate(block, 120))
		return XMLToolCall{}, false
	}
	// Parse optional structured arguments before selecting the legacy input
	// element. This lets connector tools keep their native input schemas while
	// the established single-query protocol stays valid.
	argsText := strings.TrimSpace(raw.Arguments)
	if argsText == "" {
		argsText = strings.TrimSpace(raw.Args)
	}
	var args map[string]any
	if argsText != "" {
		if len(argsText) > maxXMLToolArgumentLen {
			log.Printf("xmltool: oversized arguments rejected for %q", name)
			return XMLToolCall{}, false
		}
		if err := json.Unmarshal([]byte(argsText), &args); err != nil || len(args) == 0 {
			log.Printf("xmltool: invalid structured arguments for %q", name)
			return XMLToolCall{}, false
		}
	}

	// Choose the appropriate content element.
	query := strings.TrimSpace(raw.Query)
	if query == "" {
		query = strings.TrimSpace(raw.URL)
	}
	if query == "" {
		query = strings.TrimSpace(raw.Source)
	}
	if query == "" {
		query = strings.TrimSpace(raw.Input)
	}
	if query == "" {
		query = toolQueryFromArguments(args)
	}
	if len([]rune(query)) > maxXMLToolQueryRunes {
		log.Printf("xmltool: oversized query rejected for %q", name)
		return XMLToolCall{}, false
	}
	if query == "" && len(args) == 0 {
		log.Printf("xmltool: block has empty content: %q", truncate(block, 120))
		return XMLToolCall{}, false
	}
	return XMLToolCall{
		Name:      name,
		Query:     query,
		Arguments: args,
		Raw:       block,
		ID:        newXMLCallID(),
	}, true
}

// toolQueryFromArguments provides a backwards-compatible scalar view for
// built-in tools when an XML caller chose the structured form. Connector
// tools receive the full Arguments object directly.
func toolQueryFromArguments(args map[string]any) string {
	for _, key := range []string{"query", "url", "source", "input"} {
		if value, ok := args[key]; ok {
			if text, ok := value.(string); ok {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func rejectedToolBlockPreview(block string) string {
	if len(block) > maxXMLToolBlockBytes {
		block = block[:maxXMLToolBlockBytes]
	}
	// Avoid leaving an unmatched literal <tool marker in the transcript: the
	// final answer cleaner intentionally removes incomplete tool blocks.
	return strings.Replace(block, xmlToolOpen, "&lt;tool ", 1) + "\n[Tool-Aufruf abgelehnt: zu groß]\n"
}

// newXMLCallID generates a short random identifier for a tool call.
// Falls back to a hex-encoded current time nanoseconds on crypto/rand error.
func newXMLCallID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err == nil {
		return "tc-" + hex.EncodeToString(b)
	}
	return fmt.Sprintf("tc-%016x", time.Now().UnixNano())
}

// truncate returns s truncated to at most n runes, with "…" appended if truncated.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ─────────────────────────────────────────────────────────────────────────────
// XMLParseState — incremental streaming parser
// ─────────────────────────────────────────────────────────────────────────────

// XMLParseState holds the incremental parse state for a single streaming
// response.  Callers feed token chunks via Feed() and receive visible text
// (to be forwarded to the user) and any completed XMLToolCall objects.
//
// Thread-safety: XMLParseState is NOT goroutine-safe.  It must be used by
// a single goroutine (the stream reader goroutine in the engine).
type XMLParseState struct {
	buf string // accumulated unprocessed bytes
}

// FeedResult is returned by XMLParseState.Feed.
type FeedResult struct {
	// Visible contains text that should be forwarded to the user immediately.
	Visible string
	// Calls contains any tool calls whose XML blocks just completed.
	Calls []XMLToolCall
	// ParseErrors counts invalid XML blocks encountered (logged internally).
	ParseErrors int
}

// Feed processes an incoming chunk of streamed LLM output.
// It returns visible text to be forwarded to the client, plus any
// completed tool call blocks that were found in the chunk.
//
// Invariant: normal input eventually appears either in Visible output or in a
// completed XMLToolCall.Raw. Oversized rejected tool blocks are intentionally
// shortened to their hard safety limit.
func (s *XMLParseState) Feed(chunk string) FeedResult {
	var res FeedResult
	s.buf += chunk

	for {
		// ── Look for the start of a tool block ───────────────────────────────
		start := strings.Index(s.buf, xmlToolOpen)
		if start == -1 {
			// No tool tag in buffer.  Emit everything safe (hold back a small
			// suffix that might be the start of a future <tool tag).
			keep := partialPrefixLen(s.buf, xmlToolOpen)
			emit := len(s.buf) - keep
			if emit > 0 {
				res.Visible += s.buf[:emit]
				s.buf = s.buf[emit:]
			}
			return res
		}

		// Emit text that precedes the tool tag.
		if start > 0 {
			res.Visible += s.buf[:start]
			s.buf = s.buf[start:]
		}

		// ── Look for the end tag ─────────────────────────────────────────────
		end := strings.Index(s.buf, xmlToolClose)
		if end == -1 {
			if len(s.buf) > maxXMLToolBlockBytes {
				// Do not retain an unbounded incomplete block. Emit the rejected
				// prefix as plain text, then continue processing the remaining
				// stream normally; it cannot be executed later.
				res.Visible += rejectedToolBlockPreview(s.buf)
				s.buf = s.buf[maxXMLToolBlockBytes:]
				res.ParseErrors++
				continue
			}
			// Block not yet complete – keep in buffer.
			return res
		}
		end += len(xmlToolClose)

		// We have a complete `<tool …>…</tool>` block.
		block := s.buf[:end]
		s.buf = s.buf[end:]
		if len(block) > maxXMLToolBlockBytes {
			res.Visible += rejectedToolBlockPreview(block)
			res.ParseErrors++
			continue
		}

		if call, ok := parseXMLBlock(block); ok {
			res.Calls = append(res.Calls, call)
			// Pass the raw XML through as visible text so the frontend can
			// render it as a status card.  The frontend strips / formats it.
			res.Visible += block
		} else {
			// Invalid block – emit as plain text so nothing is hidden.
			res.Visible += block
			res.ParseErrors++
		}
	}
}

// Flush returns any remaining buffered text (called after the stream ends).
// Any incomplete <tool block left in the buffer is flushed as visible text.
func (s *XMLParseState) Flush() string {
	remaining := s.buf
	s.buf = ""
	return remaining
}

// partialPrefixLen returns the length of the longest suffix of s that is a
// prefix of target.  This prevents the parser from prematurely emitting text
// that might be the beginning of a tool tag.
func partialPrefixLen(s, target string) int {
	maxKeep := len(target) - 1
	if maxKeep > len(s) {
		maxKeep = len(s)
	}
	for n := maxKeep; n > 0; n-- {
		if strings.HasSuffix(s, target[:n]) {
			return n
		}
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// Helper: strip XML tool blocks from a string (for saving to chat history).
// ─────────────────────────────────────────────────────────────────────────────

// stripXMLToolCalls removes all `<tool …>…</tool>` blocks from text and
// returns the cleaned string.
func stripXMLToolCalls(text string) string {
	var sb strings.Builder
	remaining := text
	for {
		start := strings.Index(remaining, xmlToolOpen)
		if start == -1 {
			sb.WriteString(remaining)
			break
		}
		sb.WriteString(remaining[:start])
		remaining = remaining[start:]
		end := strings.Index(remaining, xmlToolClose)
		if end == -1 {
			// Truncated block - drop it
			break
		}
		remaining = remaining[end+len(xmlToolClose):]
	}
	return strings.TrimSpace(sb.String())
}

// extractAllXMLToolCalls parses all complete XML tool calls from a full text
// string (used post-stream for dedup and logging).
func extractAllXMLToolCalls(text string) []XMLToolCall {
	var calls []XMLToolCall
	remaining := text
	for {
		start := strings.Index(remaining, xmlToolOpen)
		if start == -1 {
			break
		}
		remaining = remaining[start:]
		end := strings.Index(remaining, xmlToolClose)
		if end == -1 {
			break
		}
		end += len(xmlToolClose)
		block := remaining[:end]
		remaining = remaining[end:]
		if call, ok := parseXMLBlock(block); ok {
			calls = append(calls, call)
		}
	}
	return calls
}
