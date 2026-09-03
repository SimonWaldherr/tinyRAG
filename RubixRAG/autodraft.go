package main

import (
	"context"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Auto-draft rule engine: on every exchange-graph-sync tick (scheduler.go),
// check each newly-seen message in the previewed batch against
// exchangeGraphConfig.AutoDraftRules and, on a match, generate a grounded
// reply (reusing draft.go's composeDraftReply — the exact same "read the
// knowledge base, propose a reply" logic the Mail tab uses on demand) and
// file it via graphmail.go's createExchangeGraphDraft.
//
// HARD SAFETY INVARIANT (restated once more here, since this is the one
// place a rule match turns into an actual write with zero human involved
// in the loop): this ALWAYS stops at creating a draft. There is no
// "auto-send" mode, no configuration that skips the draft step, and
// nothing here ever calls anything resembling a Graph /send endpoint —
// createExchangeGraphDraft's own doc comment documents the two calls it
// makes and why neither is a send. A human still has to open Outlook,
// review, and press send — exactly like every other draft this codebase
// has ever produced.
//
// Off by default at two levels: exchangeGraphConfig.EnableAutoDraftRules
// (this file's own gate) AND EnableDraftReplies (the underlying write
// capability, graphmail.go) both have to be on — either off and
// runExchangeAutoDraftRules is a no-op, so an existing deployment that
// upgrades to a build containing this code keeps behaving exactly as
// before (read/import only) until an admin deliberately opts in to both.
// ─────────────────────────────────────────────────────────────────────────────

// autoDraftedIDsCap bounds exchangeGraphConfig.AutoDraftedIDs so a
// long-lived mailbox connection doesn't grow this list (persisted in
// settings.json) without bound — same reasoning as
// schedulerHistoryLimit's cap on an unrelated in-memory list. 2000 is
// generous relative to previewExchangeMail's own per-run cap (at most
// importPreviewLimit messages get checked per tick), so it comfortably
// holds many sync cycles' worth of already-seen IDs before anything is
// trimmed off the front.
const autoDraftedIDsCap = 2000

// appendAutoDraftedID appends id to ids, trimming from the front once
// autoDraftedIDsCap is exceeded — oldest-seen IDs age out first, which is
// safe: a trimmed-off ID re-appearing in some future preview batch (it
// would have to still be within previewExchangeMail's "most recent N"
// window, i.e. genuinely recent) just gets re-evaluated once more, at
// worst producing one duplicate draft rather than silently losing dedup
// for a message still actually new.
func appendAutoDraftedID(ids []string, id string) []string {
	ids = append(ids, id)
	if len(ids) > autoDraftedIDsCap {
		ids = ids[len(ids)-autoDraftedIDsCap:]
	}
	return ids
}

// matchAutoDraftRule is the pure rule-matching core — no Graph/RAG/HTTP
// involved, so it's testable as a plain table-driven function. Rules are
// tried in order; the first enabled rule with a non-empty Pattern that
// matches wins (later rules are never consulted for that message).
// PatternField anything other than "subject" (including the empty
// default) is treated as "from" — the common case (an external-sender
// rule) shouldn't require every rule author to type "from" explicitly.
// Matching is case-insensitive substring, and Negate flips the sense (see
// exchangeAutoDraftRule's doc comment for why: it's what makes "sender is
// NOT from rubix.com" expressible).
func matchAutoDraftRule(rules []exchangeAutoDraftRule, from, subject string) (exchangeAutoDraftRule, bool) {
	for _, r := range rules {
		if !r.Enabled || strings.TrimSpace(r.Pattern) == "" {
			continue
		}
		field := from
		if strings.EqualFold(r.PatternField, "subject") {
			field = subject
		}
		contains := strings.Contains(strings.ToLower(field), strings.ToLower(r.Pattern))
		if contains != r.Negate {
			return r, true
		}
	}
	return exchangeAutoDraftRule{}, false
}

// draftExchangeAutoReply fetches item's full body, composes a
// knowledge-base-grounded reply (composeDraftReply — the same function
// the Mail tab's on-demand "Antwortentwurf erstellen" button uses) and
// files it as a Graph draft reply. No tools/nested tool-calling
// (nil/nil + draftNestedToolRounds) — deliberately the most conservative
// option available for a write path nothing reviews before it runs: the
// interactive Mail tab offers the model search/read/Shop/MSSQL/HTTP tools
// because a human is watching and can react to a bad tool call, this
// unattended path is not. deptCode "" + s.DraftPreset's Kinds mirrors
// handleDraftReply's own unauthenticated-caller default (handlers.go) —
// there's no session here to read a department from, same reasoning a
// scheduler-triggered background job has no session anywhere else in this
// codebase either.
func draftExchangeAutoReply(ctx context.Context, rag *ragSystem, s appSettings, cfg exchangeGraphConfig, item graphMailPreviewItem) error {
	token, err := graphAccessToken(ctx, egCreds(cfg))
	if err != nil {
		return err
	}
	m, err := fetchGraphMail(ctx, cfg, token, item.ID)
	if err != nil {
		return fmt.Errorf("fetch message: %w", err)
	}
	mail := graphMailToFields(m)

	preset, _ := findPreset(s.Presets, s.DraftPreset)
	draft, err := composeDraftReply(ctx, rag, s.Ranking, s.activeEmbedModel(), s.DraftChatProfile, s.K, s.SourceAccess, "", "", preset.Kinds, s.PromptsDir, mail, nil, nil, draftNestedToolRounds, "", "", "")
	if err != nil {
		return fmt.Errorf("compose reply: %w", err)
	}
	if strings.TrimSpace(draft.ReplyText) == "" {
		return fmt.Errorf("generated reply was empty, skipped filing a draft")
	}

	if _, err := createExchangeGraphDraft(ctx, cfg, item.ID, draft.ReplyText); err != nil {
		return fmt.Errorf("create draft: %w", err)
	}
	return nil
}

// runExchangeAutoDraftRules is scheduler.go's exchange-graph-sync job's
// entry point into this file: given the same preview batch the job
// already fetched (no extra Graph listing call), it checks every message
// not already in cfg.AutoDraftedIDs against cfg.AutoDraftRules, drafts a
// reply for each match, and returns the updated dedup list (for the
// caller to persist via settings.update — this function itself never
// touches the settings store, keeping it testable without one) alongside
// how many drafts were created and any per-message errors. A no-op
// (returns cfg.AutoDraftedIDs unchanged, 0 drafted, no errors) unless BOTH
// EnableAutoDraftRules and EnableDraftReplies are on.
func runExchangeAutoDraftRules(ctx context.Context, rag *ragSystem, s appSettings, cfg exchangeGraphConfig, preview []graphMailPreviewItem) (updatedIDs []string, drafted int, errs []string) {
	updatedIDs = cfg.AutoDraftedIDs
	if !cfg.EnableAutoDraftRules || !cfg.EnableDraftReplies {
		return updatedIDs, 0, nil
	}
	seen := make(map[string]bool, len(cfg.AutoDraftedIDs))
	for _, id := range cfg.AutoDraftedIDs {
		seen[id] = true
	}
	updatedIDs = append([]string(nil), cfg.AutoDraftedIDs...)

	for _, item := range preview {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		updatedIDs = appendAutoDraftedID(updatedIDs, item.ID)

		if _, ok := matchAutoDraftRule(cfg.AutoDraftRules, item.From, item.Subject); !ok {
			continue
		}
		if err := draftExchangeAutoReply(ctx, rag, s, cfg, item); err != nil {
			errs = append(errs, fmt.Sprintf("auto-draft %s: %v", item.ID, err))
			continue
		}
		drafted++
	}
	return updatedIDs, drafted, errs
}
