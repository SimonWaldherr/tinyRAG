package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

type contextualRewriteTestLM struct {
	response string
	calls    int
	msgs     []chatMsg
}

func (m *contextualRewriteTestLM) embed(texts []string) ([][]float64, error)  { return nil, nil }
func (m *contextualRewriteTestLM) embedSingle(text string) ([]float64, error) { return nil, nil }
func (m *contextualRewriteTestLM) chatStream(_ context.Context, _ string, msgs []chatMsg, w io.Writer) error {
	m.calls++
	m.msgs = append([]chatMsg(nil), msgs...)
	_, _ = io.WriteString(w, m.response)
	return nil
}
func (m *contextualRewriteTestLM) chatStreamDetailed(context.Context, string, []chatMsg, io.Writer, io.Writer) error {
	return nil
}
func (m *contextualRewriteTestLM) chatStreamVision(context.Context, string, []visionMsg, io.Writer, io.Writer) error {
	return nil
}

func TestRewriteRetrievalQueryResolvesFollowUp(t *testing.T) {
	lm := &contextualRewriteTestLM{response: "Preis und Verfügbarkeit des Produkts Atlas"}
	history := []chatMessage{{Role: "user", Content: "Erzähl mir etwas über das Produkt Atlas."}, {Role: "assistant", Content: "Atlas ist ein industrielles Messgerät."}}
	got, rewritten := rewriteRetrievalQuery(context.Background(), lm, "Und was kostet das?", history)
	if !rewritten || got != "Preis und Verfügbarkeit des Produkts Atlas" {
		t.Fatalf("rewrite = %q, %v", got, rewritten)
	}
	if lm.calls != 1 || len(lm.msgs) != 3 {
		t.Fatalf("unexpected rewrite invocation: calls=%d messages=%d", lm.calls, len(lm.msgs))
	}
}

func TestRewriteRetrievalQuerySkipsStandaloneQuestion(t *testing.T) {
	lm := &contextualRewriteTestLM{response: "should not be used"}
	question := "Welche Schutzklasse hat das Modell XR-500?"
	got, rewritten := rewriteRetrievalQuery(context.Background(), lm, question, []chatMessage{{Role: "user", Content: "Vorherige Frage"}})
	if rewritten || got != question || lm.calls != 0 {
		t.Fatalf("standalone question was unexpectedly rewritten: %q, %v, calls=%d", got, rewritten, lm.calls)
	}
}

func TestRewriteRetrievalQueryFailsOpen(t *testing.T) {
	lm := &contextualRewriteTestLM{response: strings.Repeat("x", contextualRewriteMaxRunes+1)}
	question := "Was ist damit?"
	got, rewritten := rewriteRetrievalQuery(context.Background(), lm, question, []chatMessage{{Role: "assistant", Content: "Vorheriger Kontext"}})
	if rewritten || got != question {
		t.Fatalf("unsafe rewrite was accepted: %q, %v", got, rewritten)
	}
}

func TestRewriteRetrievalQueryRejectsExplanation(t *testing.T) {
	lm := &contextualRewriteTestLM{response: "Suchanfrage: Preis des Produkts Atlas"}
	question := "Was kostet das?"
	got, rewritten := rewriteRetrievalQuery(context.Background(), lm, question, []chatMessage{{Role: "assistant", Content: "Atlas ist verfügbar."}})
	if rewritten || got != question {
		t.Fatalf("explanatory output was accepted: %q, %v", got, rewritten)
	}
}
