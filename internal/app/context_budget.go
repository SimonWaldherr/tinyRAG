package app

import "strings"

const (
	defaultAssembledContextBudgetChars = 10000
	contextPartSeparator               = "\n---\n"
	contextOmissionMarker              = "[… weiterer Kontext wurde aus Platzgründen ausgelassen …]"
	contextMinimumPartBytes            = 256
)

// assembledContextBudgetChars bounds local evidence before it reaches prompt
// construction. It is deliberately package-visible so tests can verify the
// packing behavior with small fixtures.
var assembledContextBudgetChars = defaultAssembledContextBudgetChars

type contextBudget struct {
	maxBytes  int
	usedBytes int
	parts     []string
	truncated bool
}

func newContextBudget(maxBytes int) *contextBudget {
	if maxBytes <= 0 {
		maxBytes = defaultAssembledContextBudgetChars
	}
	return &contextBudget{maxBytes: maxBytes}
}

// append adds a complete context part where possible. Primary evidence may be
// shortened as a last resort so its source header survives; neighbor context
// is omitted rather than cut mid-sentence.
func (b *contextBudget) append(part string, allowTruncate bool) bool {
	if part == "" {
		return false
	}
	reserve := len(contextPartSeparator) + len(contextOmissionMarker)
	limit := b.maxBytes - reserve
	if limit < 0 {
		limit = 0
	}
	separatorBytes := 0
	if len(b.parts) > 0 {
		separatorBytes = len(contextPartSeparator)
	}
	if b.usedBytes+separatorBytes+len(part) <= limit {
		b.parts = append(b.parts, part)
		b.usedBytes += separatorBytes + len(part)
		return true
	}

	b.truncated = true
	remaining := limit - b.usedBytes - separatorBytes
	if !allowTruncate || remaining < contextMinimumPartBytes {
		return false
	}
	part = truncateContextBytes(part, remaining)
	if part == "" {
		return false
	}
	b.parts = append(b.parts, part)
	b.usedBytes += separatorBytes + len(part)
	return true
}

func (b *contextBudget) text() string {
	if len(b.parts) == 0 {
		return ""
	}
	text := strings.Join(b.parts, contextPartSeparator)
	if !b.truncated {
		return text
	}
	return text + contextPartSeparator + contextOmissionMarker
}

// compactAssembledContext is a defensive second budget pass for callers that
// must fit a smaller final prompt. It preserves complete source blocks where
// possible instead of slicing raw bytes through a citation header.
func compactAssembledContext(text string, maxBytes int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	budget := newContextBudget(maxBytes)
	for _, part := range strings.Split(text, contextPartSeparator) {
		if strings.TrimSpace(part) == contextOmissionMarker {
			budget.truncated = true
			continue
		}
		budget.append(part, true)
	}
	return budget.text()
}

// truncateContextBytes keeps UTF-8 valid while preserving a visible
// truncation marker. Prompt budgets are measured in bytes, matching len(s).
func truncateContextBytes(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	const suffix = "…"
	if maxBytes <= len(suffix) {
		return ""
	}
	cut := maxBytes - len(suffix)
	for cut > 0 && (text[cut]&0xc0) == 0x80 {
		cut--
	}
	if cut == 0 {
		return ""
	}
	return text[:cut] + suffix
}
