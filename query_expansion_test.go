package main

import (
	"strings"
	"testing"
)

func TestNormalizeTerminologyDropsIncompleteEntries(t *testing.T) {
	in := []terminologyEntry{
		{Term: "  ", Expansions: []string{"x"}},           // blank term dropped
		{Term: "MTBF", Expansions: nil},                    // no expansions dropped
		{Term: "MTBF", Expansions: []string{"", "  "}},     // only-blank expansions dropped
		{Term: " MTBF ", Expansions: []string{" mean time between failures ", "MTBF"}},
	}
	out := normalizeTerminology(in)
	if len(out) != 1 {
		t.Fatalf("expected exactly 1 surviving entry, got %d: %+v", len(out), out)
	}
	if out[0].Term != "MTBF" {
		t.Errorf("expected trimmed term %q, got %q", "MTBF", out[0].Term)
	}
	if len(out[0].Expansions) != 1 || out[0].Expansions[0] != "mean time between failures" {
		t.Errorf("expected one trimmed, deduplicated expansion, got %+v", out[0].Expansions)
	}
}

func TestExpandTerminologyVariantsSingleWordTermMatchesWholeToken(t *testing.T) {
	terms := []terminologyEntry{{Term: "MTBF", Expansions: []string{"mean time between failures"}}}

	base := "MTBF for pump 7"
	got := expandTerminologyVariants(base, splitSearchTokens(base), terms)
	if len(got) != 1 || got[0] != "mean time between failures" {
		t.Fatalf("expected the full-form expansion, got %+v", got)
	}

	// Must not match as a substring of an unrelated longer token.
	base2 := "MTBFooBar unrelated token"
	got2 := expandTerminologyVariants(base2, splitSearchTokens(base2), terms)
	if len(got2) != 0 {
		t.Errorf("expected no match for a longer token merely containing the term, got %+v", got2)
	}
}

func TestExpandTerminologyVariantsMultiWordTermMatchesBySubstring(t *testing.T) {
	terms := []terminologyEntry{{Term: "MTBF", Expansions: []string{"mean time between failures"}}}
	base := "what is the mean time between failures for this part"
	got := expandTerminologyVariants(base, splitSearchTokens(base), terms)
	if len(got) != 1 || got[0] != "MTBF" {
		t.Fatalf("expected the abbreviation back, got %+v", got)
	}
}

func TestExpandTerminologyVariantsNoConfigIsNoOp(t *testing.T) {
	base := "MTBF"
	if got := expandTerminologyVariants(base, splitSearchTokens(base), nil); got != nil {
		t.Errorf("expected nil with no configured terminology, got %+v", got)
	}
}

func TestExpandRetrievalQueriesIncludesConfiguredTerminology(t *testing.T) {
	previous := settings
	settings = &settingsStore{s: appSettings{
		Terminology: []terminologyEntry{{Term: "MTBF", Expansions: []string{"mean time between failures"}}},
	}}
	t.Cleanup(func() { settings = previous })

	variants := expandRetrievalQueries("MTBF")
	found := false
	for _, v := range variants {
		if strings.EqualFold(v.Query, "mean time between failures") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a terminology-expanded variant among %+v", variants)
	}
}
