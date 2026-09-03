package main

import (
	"context"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Conversation-aware retrieval-query rewrite (queryRewriteConfig, settings.go)
//
// rankedSearch (rank.go) only ever sees req.Question — never req.History
// (handlers.go's askRequest doc comment says so explicitly). That's fine
// for a self-contained question, but a natural follow-up like "und bei
// Kistenpfennig?" or "was kostet das?" carries no useful vocabulary of its
// own: it leans entirely on the preceding turns for what "das"/"davon"
// actually refers to. This file adds an optional, cheap pre-flight LLM
// call — same fail-open, additive shape as tool_router.go's pre-flight
// tool routing — that turns such a follow-up into a self-contained search
// query BEFORE rankedSearch runs, without changing what's shown to the
// user or sent to the main answer call as the "question" itself.
// ─────────────────────────────────────────────────────────────────────────────

// queryRewriteSystemPrompt instructs the rewrite call to return ONLY the
// rewritten query, never a conversational answer — rewriteQueryForRetrieval
// treats anything else (an empty response, an error) as "keep the
// original question", never as a reason to fail the request.
const queryRewriteSystemPrompt = `Du bist eine Suchanfragen-Umschreibung, kein Antwort-Assistent. Anhand des bisherigen Gesprächsverlaufs und der neuesten Frage formulierst du GENAU EINE eigenständige, für die Volltextsuche in einer Wissensdatenbank optimierte Suchanfrage, die auch ohne den Gesprächsverlauf verständlich ist — ersetze Verweise wie "das", "davon", "dort", "die Firma" durch das tatsächlich gemeinte Nomen aus dem Verlauf. Gib AUSSCHLIESSLICH die neue Suchanfrage zurück: keine Anführungszeichen, keine Erklärung, kein Präfix wie "Suchanfrage:". Ist die neueste Frage bereits eigenständig verständlich (kein Bezug auf den Verlauf nötig), gib sie unverändert zurück.`

// queryRewriteMaxChars caps the accepted rewrite's length as a sanity
// check against a misbehaving model echoing back a large chunk of history
// instead of a short search query — rankedSearch would still "work" on an
// oversized query, just badly (embedding/keyword-matching a paragraph
// searches for everything and nothing at once), so this is a defensive
// floor-not-ceiling: comfortably above any real query, well below "the
// model pasted the whole conversation back".
const queryRewriteMaxChars = 500

// rewriteQueryForRetrieval turns question (plus history, most recent
// first as usual for askHistoryTurn) into a self-contained retrieval
// query when cfg.Enabled and history is non-empty — a first question in a
// fresh conversation has nothing to rewrite against, so that case (the
// common one) skips the extra LLM call entirely rather than paying for a
// no-op round-trip. Fail-open by design, mirroring tool_router.go's
// runToolRouter: any error, empty response, or suspiciously long response
// (queryRewriteMaxChars) falls back to returning question unchanged
// rather than failing or degrading the caller's request.
func rewriteQueryForRetrieval(ctx context.Context, lm *lmClient, question string, history []askHistoryTurn, cfg queryRewriteConfig, historyMax int) string {
	if !cfg.Enabled || lm == nil || len(history) == 0 || strings.TrimSpace(question) == "" {
		return question
	}

	msgs := make([]chatMsg, 0, len(history)+2)
	msgs = append(msgs, chatMsg{Role: "system", Content: queryRewriteSystemPrompt})
	msgs = append(msgs, historyToChatMsgs(history, historyMax)...)
	msgs = append(msgs, chatMsg{Role: "user", Content: question})

	assistant, err := lm.chatOnce(ctx, msgs, nil)
	if err != nil {
		return question
	}
	rewritten := strings.TrimSpace(assistant.Content)
	if rewritten == "" || len(rewritten) > queryRewriteMaxChars {
		return question
	}
	return rewritten
}

// resolveQueryRewriteProfile picks the chat profile the rewrite call
// should use: the configured override (queryRewriteConfig.Profile) if
// set, else the deployment's own default chat backend — see
// queryRewriteConfig.Profile's doc comment for why this can't fall back
// to "the main call's own profile" the way resolveRouterProfile does.
func resolveQueryRewriteProfile(cfg queryRewriteConfig, defaultChatProfile string) string {
	if p := strings.TrimSpace(cfg.Profile); p != "" {
		return p
	}
	return defaultChatProfile
}
