package main

import "strings"

// ─────────────────────────────────────────────────────────────────────────────
// Reasoning / execution tiers (askRequest.Tier) — the ChatGPT-style
// "how hard should this run" selector that generalizes the old binary
// askRequest.Mode ("" plain chat vs "agent"). One request, one endpoint
// (/api/ask), but the caller picks how much machinery runs:
//
//   instant  — pure RAG: the pre-assembled knowledge-base context only,
//              NO tools, NO tool-calling rounds. Lowest latency (the tool
//              loop degenerates to a straight stream, llm.go). Also skips
//              the pre-flight tool router.
//   standard — RAG plus the answer-time LIVE tools (MSSQL/Shop/HTTP
//              templates) with a single tool round. This is exactly the
//              old plain-Chat behavior.
//   agent    — the full agentic tool set (knowledge-base search/read,
//              clarify, mail, fetch_url, sub-agents, …) plus a multi-round
//              budget. This is exactly the old mode:"agent" behavior, incl.
//              the agent.md base prompt and the admin-fixed default preset.
//
// The mapping is a pure function so it's unit-testable without a live chat
// backend (same reasoning as resolveAskProfile/resolveUploadRouting), and
// deliberately backward-compatible: an absent Tier falls back to the legacy
// Mode field, so every existing caller (and the Agent tab's mode:"agent")
// behaves exactly as before.
// ─────────────────────────────────────────────────────────────────────────────

// agentRoundsSentinel is executionTier.Rounds' "use agentMaxRounds(s.Agent)"
// marker — the tier layer stays free of the settings dependency, so
// handleAsk resolves the actual number from the admin config.
const agentRoundsSentinel = -1

// executionTier is the resolved plan handleAsk applies for one request:
// which base prompt/retrieval/preset axis (PromptMode, reused as the old
// "mode" string so buildSystemPromptForMode/baselineRankingConfig/preset
// selection need no change), whether the live and/or agentic tools are
// offered, and the tool-round budget.
type executionTier struct {
	Name       string // canonical tier name (debug/telemetry)
	PromptMode string // "" (index.md) | "agent" (agent.md)
	LiveTools  bool   // offer MSSQL/Shop/HTTP live tools + run the pre-flight tool router
	AgentTools bool   // add buildAgentTools + audit-wrap every executor
	Rounds     int    // tool-round budget; agentRoundsSentinel = agentMaxRounds(s.Agent)
}

var (
	tierInstant  = executionTier{Name: "instant", PromptMode: "", LiveTools: false, AgentTools: false, Rounds: 0}
	tierStandard = executionTier{Name: "standard", PromptMode: "", LiveTools: true, AgentTools: false, Rounds: 1}
	tierAgent    = executionTier{Name: "agent", PromptMode: "agent", LiveTools: true, AgentTools: true, Rounds: agentRoundsSentinel}
)

// resolveExecutionTier maps the request's Tier (preferred) or legacy Mode
// (fallback) onto an executionTier. Unknown/empty Tier degrades to the
// legacy-Mode behavior rather than erroring — an unset or stale selector
// should never break a request, same "degrade, don't fail" posture as
// findPreset.
func resolveExecutionTier(tier, mode string) executionTier {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "instant":
		return tierInstant
	case "standard", "balanced", "auto":
		return tierStandard
	case "agent":
		return tierAgent
	default:
		// No explicit (or an unrecognized) tier: honor the legacy Mode field
		// so the Agent tab (mode:"agent") and every pre-tier caller are
		// unchanged.
		if strings.EqualFold(strings.TrimSpace(mode), "agent") {
			return tierAgent
		}
		return tierStandard
	}
}
