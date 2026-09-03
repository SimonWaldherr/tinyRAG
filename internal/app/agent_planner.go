package app

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
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// plannedStep is one tool call proposed by the planner.
type plannedStep struct {
	Tool      string         `json:"tool"`
	Query     string         `json:"query,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

// plannerTimeout bounds the planning LLM call.
const (
	plannerTimeout          = 15 * time.Second
	maxAgentPlannerSteps    = 5
	maxPlannerReasonRunes   = 600
	maxPlannerQuestionRunes = 4000
	maxPlannerOutputBytes   = 16 * 1024
)

var errPlannerOutputTooLarge = errors.New("planner output exceeds configured limit")

type boundedPlannerBuffer struct {
	b   strings.Builder
	max int
}

func (w *boundedPlannerBuffer) Write(p []byte) (int, error) {
	if w.b.Len()+len(p) > w.max {
		return 0, errPlannerOutputTooLarge
	}
	return w.b.Write(p)
}

func (w *boundedPlannerBuffer) String() string { return w.b.String() }

// buildPlannerPrompt renders the planning instruction for the LLM.
func buildPlannerPrompt(question string, tools []toolDef, maxSteps int) string {
	var sb strings.Builder
	sb.WriteString("Du bist ein Planungsmodul. Analysiere die Frage und entscheide, welche Tools VORAB ausgeführt werden sollten, um sie fundiert zu beantworten.\n\n")
	sb.WriteString("Verfügbare Tools:\n")
	for _, t := range tools {
		fmt.Fprintf(&sb, "- %s: %s (Parameter: %s)\n", t.Name, t.Description, t.ParamHint)
		if t.InputSchema != nil && len(t.InputSchema.Required) > 1 {
			fmt.Fprintf(&sb, "  Strukturierte Pflichtfelder: %s\n", strings.Join(t.InputSchema.Required, ", "))
		}
	}
	fmt.Fprintf(&sb, "\nRegeln:\n")
	fmt.Fprintf(&sb, "- Maximal %d Schritte.\n", maxSteps)
	sb.WriteString("- Nur Tools einplanen, die wirklich nötig sind. Für einfache Fragen: leeres Array.\n")
	sb.WriteString("- Jeder Schritt braucht ein konkretes, eigenständiges Query oder ein strukturiertes `arguments`-Objekt.\n")
	sb.WriteString("- Nutze `arguments` für Tools mit mehreren Pflichtfeldern; erfinde keine Felder.\n")
	sb.WriteString("- Antworte NUR mit einem JSON-Array der Form [{\"tool\":\"name\",\"query\":\"...\",\"arguments\":{...},\"reason\":\"...\"}] ohne weiteren Text.\n\n")
	sb.WriteString("Frage: " + truncate(strings.TrimSpace(question), maxPlannerQuestionRunes) + "\n")
	return sb.String()
}

// planToolSteps asks the LLM for a tool plan. An empty slice (no error) means
// the planner decided no upfront tools are needed.
func planToolSteps(ctx context.Context, lm lmProvider, question string, tools []toolDef, maxSteps int) ([]plannedStep, error) {
	if lm == nil || len(tools) == 0 || maxSteps <= 0 {
		return nil, nil
	}
	maxSteps = boundedAgentPlannerSteps(maxSteps)
	tctx, cancel := context.WithTimeout(ctx, plannerTimeout)
	defer cancel()
	out := &boundedPlannerBuffer{max: maxPlannerOutputBytes}
	err := lm.chatStream(tctx, "Du bist ein präzises Planungsmodul. Antworte ausschließlich mit JSON.",
		[]chatMsg{{Role: "user", Content: buildPlannerPrompt(question, tools, maxSteps)}}, out)
	if err != nil {
		return nil, err
	}
	return parsePlannedSteps(out.String(), tools, maxSteps)
}

// parsePlannedSteps extracts and validates the JSON plan from raw LLM output.
// Unknown tools and empty queries are dropped; the result is capped at maxSteps.
func parsePlannedSteps(raw string, tools []toolDef, maxSteps int) ([]plannedStep, error) {
	maxSteps = boundedAgentPlannerSteps(maxSteps)
	if maxSteps <= 0 {
		return nil, nil
	}
	jsonStr, err := extractFirstJSONValue(raw)
	if err != nil {
		return nil, fmt.Errorf("planner parse: %w", err)
	}
	var steps []plannedStep
	if err := json.Unmarshal([]byte(jsonStr), &steps); err != nil {
		return nil, fmt.Errorf("planner unmarshal: %w", err)
	}
	known := make(map[string]string, len(tools))
	for _, t := range tools {
		known[strings.ToLower(strings.TrimSpace(t.Name))] = t.Name
	}
	valid := make([]plannedStep, 0, len(steps))
	seen := make(map[string]bool, len(steps))
	for _, s := range steps {
		canonicalName, knownTool := known[strings.ToLower(strings.TrimSpace(s.Tool))]
		s.Tool = canonicalName
		s.Query = strings.TrimSpace(s.Query)
		s.Reason = truncate(strings.TrimSpace(s.Reason), maxPlannerReasonRunes)
		if len([]rune(s.Query)) > maxXMLToolQueryRunes {
			continue
		}
		if len(s.Arguments) > 0 {
			encoded, err := json.Marshal(s.Arguments)
			if err != nil || len(encoded) > maxXMLToolArgumentLen {
				continue
			}
		}
		if !knownTool || s.Tool == "" || (s.Query == "" && len(s.Arguments) == 0) {
			continue
		}
		key := canonicalToolCallKey(XMLToolCall{Name: s.Tool, Query: s.Query, Arguments: s.Arguments})
		if seen[key] {
			continue
		}
		seen[key] = true
		valid = append(valid, s)
		if len(valid) >= maxSteps {
			break
		}
	}
	return valid, nil
}

func boundedAgentPlannerSteps(steps int) int {
	if steps <= 0 {
		return 0
	}
	if steps > maxAgentPlannerSteps {
		return maxAgentPlannerSteps
	}
	return steps
}

// availableToolsForPlanning collects the tool definitions visible to the
// current role, mirroring what /api/ask exposes to the answering LLM.
func (e *StreamingEngine) availableToolsForPlanning(s appSettings, autoSearch bool) []toolDef {
	return e.autonomousToolDefs(s, autoSearch)
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
	maxSteps = boundedAgentPlannerSteps(maxSteps)
	if e.cfg.MaxToolsPerRound > 0 && maxSteps > e.cfg.MaxToolsPerRound {
		maxSteps = e.cfg.MaxToolsPerRound
	}
	if remaining := e.cfg.MaxToolsTotal - *totalTools; remaining < maxSteps {
		maxSteps = remaining
	}
	if maxSteps <= 0 {
		return chatMsg{}, false
	}
	tools := e.availableToolsForPlanning(s, req.AutoSearch)
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
		call := XMLToolCall{ID: fmt.Sprintf("plan-%d", i), Name: step.Tool, Query: step.Query, Arguments: step.Arguments, Reason: step.Reason}
		var decision toolPolicyDecision
		call, decision = e.admitToolCall(s, call, req.AutoSearch)
		if !decision.Allowed {
			log.Printf("ENGINE[%s] planner: tool not allowed: %s (%s)", req.RequestID, step.Tool, decision.Reason)
			e.recordToolSkip(sw, tel, call, "plan", decision)
			continue
		}
		key := canonicalToolCallKey(call)
		if seen[key] {
			e.recordToolSkip(sw, tel, call, "plan", denyTool("deny", "duplicate_call", decision.Risk))
			continue
		}
		seen[key] = true
		(*totalTools)++

		startPayload, _ := json.Marshal(map[string]any{
			"id": call.ID, "tool": call.Name, "query": call.Query,
			"arguments": call.Arguments, "phase": "plan",
		})
		sw.event("tool_start", string(startPayload))

		ch := make(chan ToolResult, 1)
		go e.execTool(ctx, call, s, "plan", ch)
		pendings = append(pendings, pending{call: call, res: ch})
	}
	if len(pendings) == 0 {
		return chatMsg{}, false
	}

	results := make([]ToolResult, 0, len(pendings))
	for _, p := range pendings {
		results = append(results, <-p.res)
	}

	evidence := buildToolEvidenceMessage(results, "plan")
	for i, res := range results {
		results[i].EvidenceTruncated = evidence.TruncatedCallIDs[res.Call.ID]
		tel.recordTool(toolInvocationRecordFromResult(results[i]))
		payload, _ := json.Marshal(map[string]any{
			"id": res.Call.ID, "tool": res.Call.Name, "query": res.Call.Query,
			"arguments": res.Call.Arguments, "source": res.Source, "error": errStr(res.Error),
			"result_bytes": len(res.Text), "content_hash": res.ContentHash,
			"evidence_truncated": results[i].EvidenceTruncated, "phase": res.Phase,
		})
		sw.event("tool_result", string(payload))
	}
	return chatMsg{Role: "user", Content: evidence.Content}, true
}
