package main

// ─────────────────────────────────────────────────────────────────────────────
// Agent planner — multi-step tool planning
//
// When enabled (settings "agent_planner_enabled" or per-request "agent" flag),
// the engine asks the LLM to plan up to N tool calls BEFORE the answer stream
// starts. Planned tools are executed upfront (respecting role permissions,
// dedup and the global tool caps) and their results are injected into the
// conversation, so the answering pass starts with all evidence in hand.
//
// This complements the reactive inline <tool> mechanism: the planner handles
// questions that clearly need multiple lookups (compare X and Y, summarize
// A based on B, ...), while simple questions skip planning entirely because
// the model may return an empty plan.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// plannedStep is one tool call proposed by the planner.
type plannedStep struct {
	Tool   string `json:"tool"`
	Query  string `json:"query"`
	Reason string `json:"reason,omitempty"`
}

// plannerTimeout bounds the planning LLM call.
const plannerTimeout = 15 * time.Second

// buildPlannerPrompt renders the planning instruction for the LLM.
func buildPlannerPrompt(question string, tools []toolDef, maxSteps int) string {
	var sb strings.Builder
	sb.WriteString("Du bist ein Planungsmodul. Analysiere die Frage und entscheide, welche Tools VORAB ausgeführt werden sollten, um sie fundiert zu beantworten.\n\n")
	sb.WriteString("Verfügbare Tools:\n")
	for _, t := range tools {
		fmt.Fprintf(&sb, "- %s: %s (Parameter: %s)\n", t.Name, t.Description, t.ParamHint)
	}
	fmt.Fprintf(&sb, "\nRegeln:\n")
	fmt.Fprintf(&sb, "- Maximal %d Schritte.\n", maxSteps)
	sb.WriteString("- Nur Tools einplanen, die wirklich nötig sind. Für einfache Fragen: leeres Array.\n")
	sb.WriteString("- Jeder Schritt braucht ein konkretes, eigenständiges Query.\n")
	sb.WriteString("- Antworte NUR mit einem JSON-Array der Form [{\"tool\":\"name\",\"query\":\"...\",\"reason\":\"...\"}] ohne weiteren Text.\n\n")
	sb.WriteString("Frage: " + strings.TrimSpace(question) + "\n")
	return sb.String()
}

// planToolSteps asks the LLM for a tool plan. An empty slice (no error) means
// the planner decided no upfront tools are needed.
func planToolSteps(ctx context.Context, lm lmProvider, question string, tools []toolDef, maxSteps int) ([]plannedStep, error) {
	if lm == nil || len(tools) == 0 || maxSteps <= 0 {
		return nil, nil
	}
	tctx, cancel := context.WithTimeout(ctx, plannerTimeout)
	defer cancel()
	var out strings.Builder
	err := lm.chatStream(tctx, "Du bist ein präzises Planungsmodul. Antworte ausschließlich mit JSON.",
		[]chatMsg{{Role: "user", Content: buildPlannerPrompt(question, tools, maxSteps)}}, &out)
	if err != nil {
		return nil, err
	}
	return parsePlannedSteps(out.String(), tools, maxSteps)
}

// parsePlannedSteps extracts and validates the JSON plan from raw LLM output.
// Unknown tools and empty queries are dropped; the result is capped at maxSteps.
func parsePlannedSteps(raw string, tools []toolDef, maxSteps int) ([]plannedStep, error) {
	jsonStr, err := extractFirstJSONValue(raw)
	if err != nil {
		return nil, fmt.Errorf("planner parse: %w", err)
	}
	var steps []plannedStep
	if err := json.Unmarshal([]byte(jsonStr), &steps); err != nil {
		return nil, fmt.Errorf("planner unmarshal: %w", err)
	}
	known := make(map[string]bool, len(tools))
	for _, t := range tools {
		known[strings.ToLower(t.Name)] = true
	}
	valid := make([]plannedStep, 0, len(steps))
	for _, s := range steps {
		s.Tool = strings.ToLower(strings.TrimSpace(s.Tool))
		s.Query = strings.TrimSpace(s.Query)
		if s.Tool == "" || s.Query == "" || !known[s.Tool] {
			continue
		}
		valid = append(valid, s)
		if len(valid) >= maxSteps {
			break
		}
	}
	return valid, nil
}

// availableToolsForPlanning collects the tool definitions visible to the
// current role, mirroring what /api/ask exposes to the answering LLM.
func (e *StreamingEngine) availableToolsForPlanning(s appSettings) []toolDef {
	all := builtinTools
	if e.customAPIs != nil {
		all = e.customAPIs.allTools()
	}
	if e.modules != nil {
		all = append(all, e.modules.enabledTools()...)
	}
	if e.connectors != nil {
		all = append(all, e.connectors.enabledToolDefs()...)
	}
	return filterToolsForRole(all, s.ActiveRole)
}

// runPlannerPhase plans and executes upfront tools. It returns an optional
// user message carrying the tool results, to be appended to the conversation
// before the first answering round. The seen map and totalTools counter are
// shared with the main engine loop so caps and dedup hold across both phases.
func (e *StreamingEngine) runPlannerPhase(
	ctx context.Context,
	req EngineRequest,
	sw *sseWriter,
	tel *RequestTelemetry,
	s appSettings,
	seen map[string]bool,
	totalTools *int,
) (chatMsg, bool) {
	maxSteps := s.AgentMaxPlanSteps
	if maxSteps <= 0 {
		maxSteps = 3
	}
	tools := e.availableToolsForPlanning(s)
	steps, err := planToolSteps(ctx, e.lm, req.Question, tools, maxSteps)
	if err != nil {
		log.Printf("ENGINE[%s] planner failed, continuing without plan: %v", req.RequestID, err)
		return chatMsg{}, false
	}
	if len(steps) == 0 {
		log.Printf("ENGINE[%s] planner: empty plan (direct answer)", req.RequestID)
		return chatMsg{}, false
	}

	planPayload, _ := json.Marshal(steps)
	sw.event("plan", string(planPayload))
	log.Printf("ENGINE[%s] planner: %d steps", req.RequestID, len(steps))

	type pending struct {
		call XMLToolCall
		res  chan ToolResult
	}
	var pendings []pending
	for i, step := range steps {
		if *totalTools >= e.cfg.MaxToolsTotal {
			log.Printf("ENGINE[%s] planner: tool cap reached, dropping remaining steps", req.RequestID)
			break
		}
		if !e.toolAllowed(s, step.Tool, true) {
			log.Printf("ENGINE[%s] planner: tool not allowed: %s", req.RequestID, step.Tool)
			continue
		}
		key := toolDedupKey(step.Tool, step.Query)
		if seen[key] {
			continue
		}
		seen[key] = true
		*totalTools++

		call := XMLToolCall{ID: fmt.Sprintf("plan-%d", i), Name: step.Tool, Query: step.Query}
		startPayload, _ := json.Marshal(map[string]string{
			"id": call.ID, "tool": call.Name, "query": call.Query, "phase": "plan",
		})
		sw.event("tool_start", string(startPayload))

		ch := make(chan ToolResult, 1)
		go e.execTool(ctx, call, s, ch)
		pendings = append(pendings, pending{call: call, res: ch})
	}
	if len(pendings) == 0 {
		return chatMsg{}, false
	}

	results := make([]ToolResult, 0, len(pendings))
	for _, p := range pendings {
		res := <-p.res
		results = append(results, res)

		rec := ToolInvocationRecord{
			ID:         p.call.ID,
			Tool:       p.call.Name,
			Query:      p.call.Query,
			StartTime:  res.StartedAt,
			EndTime:    res.StartedAt.Add(res.Duration),
			DurationMS: res.Duration.Milliseconds(),
		}
		if res.Error != nil {
			rec.Error = res.Error.Error()
		} else {
			rec.ResultBytes = len(res.Text)
		}
		tel.recordTool(rec)

		payload, _ := json.Marshal(map[string]any{
			"id": p.call.ID, "tool": p.call.Name, "query": p.call.Query,
			"source": res.Source, "error": errStr(res.Error),
			"result_bytes": len(res.Text), "phase": "plan",
		})
		sw.event("tool_result", string(payload))
	}

	var sb strings.Builder
	sb.WriteString("Vorab geplante Tools wurden bereits ausgeführt. Nutze die Ergebnisse für deine Antwort.\n\n")
	for _, r := range results {
		fmt.Fprintf(&sb, "### Tool: %s\nQuery: %s\n", r.Call.Name, r.Call.Query)
		if r.Error != nil {
			fmt.Fprintf(&sb, "Fehler: %s\nHinweis: Sage offen, dass dieses Tool fehlgeschlagen ist. Erfinde keine Daten.\n", r.Error.Error())
		} else {
			fmt.Fprintf(&sb, "Ergebnis (%d Zeichen):\n%s\n", len(r.Text), r.Text)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Beantworte jetzt die ursprüngliche Frage vollständig. Emittiere nur dann neue <tool>-Blöcke, wenn wichtige Informationen fehlen.\n")
	return chatMsg{Role: "user", Content: sb.String()}, true
}
