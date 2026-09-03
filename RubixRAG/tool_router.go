package main

import (
	"context"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Pre-flight tool router (toolRouterConfig, settings.go)
//
// An optional, additional step ahead of the main Chat/Agent/Mail answer
// call: one cheap, single-round LLM call, offered only the "live" tools
// (buildLiveTools, agent.go — MSSQL generic+templates, Shop, HTTP templates;
// never the knowledge-base/clarify/draft/subagent tools, since RAG
// retrieval and those meta-tools are handled elsewhere), that decides
// whether a tool is needed for the question at hand and, if so, runs it.
// Its result — if any — comes back as a ready-to-prepend context block, to
// be combined with the RAG chunks before the real answer/draft call, same
// as this feature was explicitly requested: a dedicated decision step whose
// output feeds the main request, rather than relying solely on the main
// call's own native tool-calling (which still runs afterward, unchanged, as
// a fallback — this is additive, not a replacement).
//
// Deliberately NOT run for sub-agents (delegate_subtasks) or the nested
// draft_new_mail tool call: those already run their own independent
// tool-calling loop, and adding a second LLM call ahead of each would
// multiply cost with the sub-task count. Only the three top-level entry
// points (Chat/Agent via handleAsk, Mail via handleDraftReply) call this.
// ─────────────────────────────────────────────────────────────────────────────

// toolRouterSystemPrompt instructs the router call to decide-and-act, never
// to answer the question itself — runToolRouter discards any plain-text
// response it produces; only a tool call (if any) matters.
const toolRouterSystemPrompt = `Du bist eine Vorprüfung, kein Antwort-Assistent. Deine einzige Aufgabe: entscheide, ob eines der angebotenen Werkzeuge nötig ist, um die folgende Frage zu beantworten, und rufe es bei Bedarf mit den passenden Parametern auf. Ist kein Werkzeug nötig oder passend, antworte nicht inhaltlich, sondern nur mit dem einzelnen Wort "nein". Deine eigene Textantwort wird nicht verwendet — ausschließlich ein etwaiger Werkzeugaufruf und dessen Ergebnis zählen.`

// runToolRouter runs the pre-flight step and returns a context block to
// prepend to the main call's RAG context — or "" when disabled, when no
// live tools are available for this request, when the router decided no
// tool was needed, or on any error.
//
// Fail-open by design: a broken or slow pre-flight step must never block or
// degrade the main answer. It deliberately bypasses chatWithToolsBudget
// (llm.go) and calls chatOnce/runToolCalls directly — chatWithToolsBudget's
// budget-exhausted path forces one more (unwanted, discarded) LLM call to
// produce a final text answer once maxRounds is used up by a tool call,
// which would silently double this feature's LLM cost for no benefit here.
//
// Every step this call causes (tool_start/tool_end) still flows through
// ctx's existing progress emitter exactly as a main-loop tool call would,
// tagged Phase:"router" so the UI can distinguish "the pre-check used a
// tool" from "the main answer used a tool" (agentStepsPanel, web/app.js).
func runToolRouter(ctx context.Context, lm *lmClient, question string, s appSettings, sess agentSession, preset sourcePreset, mssqlAllowed bool) string {
	if !s.ToolRouter.Enabled || lm == nil || strings.TrimSpace(question) == "" {
		return ""
	}
	tools, executors := buildLiveTools(s, sess, preset, mssqlAllowed)
	if len(tools) == 0 {
		return ""
	}

	// Forward every step this call emits to whatever progress emitter the
	// caller already wired (nil if none, e.g. no session in play — send is
	// nil-safe regardless), stamped Phase:"router" so the UI timeline can
	// tell it apart from the main call's own tool use.
	parent := agentProgressFromContext(ctx)
	routerCtx := withAgentProgress(ctx, func(st agentStep) {
		st.Phase = "router"
		parent.send(st)
	})

	all := []chatMsg{
		{Role: "system", Content: toolRouterSystemPrompt},
		{Role: "user", Content: question},
	}
	assistant, err := lm.chatOnce(routerCtx, all, tools)
	if err != nil || len(assistant.ToolCalls) == 0 {
		return ""
	}
	if clarificationFromCalls(assistant.ToolCalls) != nil {
		// buildLiveTools never offers ask_clarifying_question, so this
		// should be unreachable — defensive only, never let the router
		// itself surface a clarification prompt.
		return ""
	}

	// A fresh, empty cache: this is a standalone mini-loop, not sharing
	// state with the main call's own (separate) resultCache.
	toolMsgs := lm.runToolCalls(routerCtx, 0, assistant.ToolCalls, executors, map[string]string{})
	var results []string
	for _, m := range toolMsgs {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		results = append(results, fmt.Sprintf("[%s]\n%s", m.Name, m.Content))
	}
	if len(results) == 0 {
		return ""
	}
	return "Ergebnisse automatisch ausgeführter Werkzeuge:\n\n" + strings.Join(results, "\n\n") + "\n\n"
}

// resolveRouterProfile picks the chat profile the router call should use:
// the configured override (toolRouterConfig.Profile) if set, else
// mainProfile — the profile the main answer/draft call already resolved
// to, since a cheap pre-check has no default reason to use a different
// (possibly pricier) backend than the answer it's checking for.
func resolveRouterProfile(cfg toolRouterConfig, mainProfile string) string {
	if p := strings.TrimSpace(cfg.Profile); p != "" {
		return p
	}
	return mainProfile
}
