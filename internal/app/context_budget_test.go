package app

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContextBudgetKeepsPrimaryAndAddsOneOmissionMarker(t *testing.T) {
	budget := newContextBudget(700)
	primary := "[Quelle: Handbook | Chunk: 0]\n" + strings.Repeat("p", 300)
	neighbor := "[Quelle: Handbook | Chunk: 1]\n" + strings.Repeat("n", 500)
	if !budget.append(primary, true) {
		t.Fatal("expected primary evidence to fit")
	}
	if budget.append(neighbor, false) {
		t.Fatal("oversized neighbor should be omitted instead of truncated")
	}
	got := budget.text()
	if len(got) > 700 {
		t.Fatalf("context length = %d, want <= 700", len(got))
	}
	if !strings.Contains(got, "Chunk: 0") {
		t.Fatalf("primary source header missing: %q", got)
	}
	if strings.Count(got, contextOmissionMarker) != 1 {
		t.Fatalf("omission marker count = %d: %q", strings.Count(got, contextOmissionMarker), got)
	}
}

func TestTruncateContextBytesPreservesUTF8(t *testing.T) {
	got := truncateContextBytes("äöüabcdef", 8)
	if !strings.HasSuffix(got, "…") || !utf8.ValidString(got) || len(got) > 8 {
		t.Fatalf("invalid UTF-8-safe truncation %q (%d bytes)", got, len(got))
	}
}

func TestCompactAssembledContextKeepsStructuredEvidence(t *testing.T) {
	input := strings.Join([]string{
		"[Quelle: A | Chunk: 0]\n" + strings.Repeat("a", 400),
		"[Quelle: B | Chunk: 0]\n" + strings.Repeat("b", 400),
	}, contextPartSeparator)
	got := compactAssembledContext(input, 700)
	if len(got) > 700 || !strings.Contains(got, "[Quelle: A") {
		t.Fatalf("compacted context = %q (%d bytes)", got, len(got))
	}
	if strings.Count(got, contextOmissionMarker) != 1 {
		t.Fatalf("expected one omission marker, got %q", got)
	}
}
