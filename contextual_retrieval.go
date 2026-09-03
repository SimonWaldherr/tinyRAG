package main

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

// Contextual retrieval turns an elliptical follow-up into a standalone search
// query before it reaches the embedding model. It is deliberately separate
// from the visible question: the answer model still receives the user's
// original wording and full conversation history.
const (
	contextualRewriteHistoryMessages = 6
	contextualRewriteMessageRunes    = 800
	contextualRewriteMaxRunes        = 500
	contextualRewriteTimeout         = 8 * time.Second
)

// needsContextualRewrite keeps the extra model call off the ordinary
// single-turn path. The markers are intentionally language-agnostic enough
// for common German and English follow-ups. They are kept explicit so that a
// short but self-contained entity search does not incur another model call.
func needsContextualRewrite(question string, history []chatMessage) bool {
	if len(history) == 0 {
		return false
	}
	q := strings.ToLower(strings.TrimSpace(question))
	if q == "" {
		return false
	}
	if looksTechnicalQuery(q) {
		return false
	}
	markers := []string{
		"das", "diese", "dieser", "dieses", "davon", "dazu", "damit", "dort",
		"und was", "und wie", "mehr dazu", "wie weiter", "noch mehr",
		"this", "that", "those", "there", "them", "about it", "what about",
		"and what", "and how", "more about", "same one",
	}
	for _, marker := range markers {
		if hasQueryPhrase(q, marker) {
			return true
		}
	}
	return false
}

func hasQueryPhrase(query, phrase string) bool {
	if !strings.Contains(query, phrase) {
		return false
	}
	// For single-token markers, compare token boundaries so a word such as
	// "stadt" does not trigger the German marker "das".
	if !strings.Contains(phrase, " ") {
		for _, token := range splitSearchTokens(query) {
			if token == phrase {
				return true
			}
		}
		return false
	}
	return true
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// rewriteRetrievalQuery returns the original question unless a bounded model
// call safely produces a concise standalone query. This makes contextual
// retrieval fail-open: an unavailable or unsuitable chat model never blocks
// ordinary RAG retrieval.
func rewriteRetrievalQuery(ctx context.Context, lm lmProvider, question string, history []chatMessage) (string, bool) {
	question = strings.TrimSpace(question)
	if lm == nil || !needsContextualRewrite(question, history) {
		return question, false
	}

	start := len(history) - contextualRewriteHistoryMessages
	if start < 0 {
		start = 0
	}
	msgs := make([]chatMsg, 0, len(history)-start+1)
	for _, message := range history[start:] {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		msgs = append(msgs, chatMsg{Role: role, Content: truncateRunes(content, contextualRewriteMessageRunes)})
	}
	if len(msgs) == 0 {
		return question, false
	}
	msgs = append(msgs, chatMsg{Role: "user", Content: question})

	system := "Formuliere für eine Wissenssuche eine kurze, eigenständige Suchanfrage. " +
		"Löse Verweise auf vorherige Nachrichten auf, erfinde keine Fakten und gib ausschließlich die Suchanfrage aus. " +
		"Wenn die aktuelle Frage bereits eigenständig ist, gib sie unverändert aus."
	tctx, cancel := context.WithTimeout(ctx, contextualRewriteTimeout)
	defer cancel()
	var output strings.Builder
	if err := lm.chatStream(tctx, system, msgs, &output); err != nil {
		return question, false
	}
	rewritten := strings.Trim(strings.TrimSpace(output.String()), "\"'")
	if rewritten == "" || strings.ContainsAny(rewritten, "\r\n") || utf8.RuneCountInString(rewritten) > contextualRewriteMaxRunes {
		return question, false
	}
	lower := strings.ToLower(rewritten)
	if strings.HasPrefix(lower, "suchanfrage:") || strings.HasPrefix(lower, "query:") || strings.HasPrefix(lower, "search query:") {
		return question, false
	}
	if strings.EqualFold(rewritten, question) {
		return question, false
	}
	return rewritten, true
}
