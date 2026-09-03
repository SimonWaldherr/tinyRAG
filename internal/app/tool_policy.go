package app

import (
	"encoding/json"
	"strings"
)

// Tool execution is intentionally capability-based rather than name-based.
// A model may suggest any identifier, but only registered, read-only
// capabilities can run autonomously. Actions that can change data, ingest
// content, or execute code stay available to explicitly initiated workflows
// and are never silently promoted into an agent run.
type toolRisk string

const (
	toolRiskLocalRead    toolRisk = "local_read"
	toolRiskExternalRead toolRisk = "external_read"
	toolRiskCompute      toolRisk = "compute"
	toolRiskCode         toolRisk = "code"
	toolRiskMutating     toolRisk = "mutating"
	toolRiskManual       toolRisk = "manual_only"
)

type toolPolicyDecision struct {
	Allowed bool
	Mode    string
	Reason  string
	Risk    toolRisk
}

func allowTool(risk toolRisk) toolPolicyDecision {
	return toolPolicyDecision{Allowed: true, Mode: "auto", Reason: "read_only", Risk: risk}
}

func denyTool(mode, reason string, risk toolRisk) toolPolicyDecision {
	return toolPolicyDecision{Mode: mode, Reason: reason, Risk: risk}
}

// allKnownToolDefs returns the deterministic catalog visible to this engine.
// Built-ins win name collisions so a connector cannot shadow a privileged
// local capability.
func (e *StreamingEngine) allKnownToolDefs() []toolDef {
	var all []toolDef
	if e.customAPIs != nil {
		all = e.customAPIs.allTools()
	} else {
		all = append(all, builtinTools...)
	}
	if e.modules != nil {
		all = append(all, e.modules.enabledTools()...)
	}
	if e.connectors != nil {
		all = append(all, e.connectors.enabledToolDefs()...)
	}

	seen := make(map[string]bool, len(all))
	out := make([]toolDef, 0, len(all))
	for _, def := range all {
		key := strings.ToLower(strings.TrimSpace(def.Name))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, def)
	}
	return out
}

func (e *StreamingEngine) lookupToolDef(name string) (toolDef, bool) {
	needle := strings.ToLower(strings.TrimSpace(name))
	if needle == "" {
		return toolDef{}, false
	}
	for _, def := range e.allKnownToolDefs() {
		if strings.EqualFold(def.Name, needle) {
			return def, true
		}
	}
	return toolDef{}, false
}

func (e *StreamingEngine) connectorEntry(name string) (connectorRegistryEntry, bool) {
	if e.connectors == nil {
		return connectorRegistryEntry{}, false
	}
	needle := strings.TrimSpace(name)
	for registeredName, entry := range e.connectors.registry() {
		if strings.EqualFold(registeredName, needle) {
			return entry, true
		}
	}
	return connectorRegistryEntry{}, false
}

func builtinToolRisk(name string) (toolRisk, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "rag_knowledge", "local_search", "vector_query":
		return toolRiskLocalRead, true
	case "calculate", "datetime", "json_query", "text_diff", "regex_extract":
		return toolRiskCompute, true
	case "url_fetch", "wikipedia", "duckduckgo", "wiktionary", "stackoverflow", "websearch", "news", "wikidata", "github":
		return toolRiskExternalRead, true
	case "nanogo", "exec_code", "shell", "tinygo":
		return toolRiskCode, true
	case "sql_query":
		// Raw SQL is intentionally not part of autonomous execution: it has a
		// broader data-access surface than retrieval and must not bypass ACLs.
		return toolRiskManual, true
	default:
		return "", false
	}
}

// autonomousReadOnlySQL is deliberately narrower than the connector's
// compatibility parser. A configured SQL connector may support richer,
// manually initiated read workflows, but agent automation accepts only a
// single SELECT statement without mutation-related keywords.
func autonomousReadOnlySQL(query string) bool {
	trimmed := stripSQLComments(strings.TrimSpace(query))
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT") || strings.Contains(upper, ";") {
		return false
	}
	for _, keyword := range []string{
		"INSERT", "UPDATE", "DELETE", "DROP", "CREATE", "ALTER",
		"TRUNCATE", "REPLACE", "ATTACH", "DETACH", "PRAGMA", "INTO",
	} {
		for offset := 0; ; {
			index := strings.Index(upper[offset:], keyword)
			if index < 0 {
				break
			}
			index += offset
			before := index == 0 || !isAlphaNumUnder(rune(upper[index-1]))
			afterIndex := index + len(keyword)
			after := afterIndex >= len(upper) || !isAlphaNumUnder(rune(upper[afterIndex]))
			if before && after {
				return false
			}
			offset = afterIndex
		}
	}
	return true
}

func (e *StreamingEngine) riskForTool(def toolDef) toolRisk {
	if risk, ok := builtinToolRisk(def.Name); ok {
		return risk
	}
	if strings.HasPrefix(strings.ToLower(def.Name), "module:") {
		return toolRiskMutating
	}
	if entry, ok := e.connectorEntry(def.Name); ok {
		if entry.Capability.Type == CapabilityTypeIngest {
			return toolRiskMutating
		}
		switch entry.Connector.Type {
		case ConnectorTypeHTTP:
			method := strings.ToUpper(strings.TrimSpace(entry.Capability.Method))
			if method == "" {
				method = "GET"
			}
			if (method == "GET" || method == "HEAD") && strings.TrimSpace(entry.Capability.BodyTemplate) == "" {
				return toolRiskExternalRead
			}
			return toolRiskMutating
		case ConnectorTypeSQL:
			if autonomousReadOnlySQL(entry.Capability.Query) {
				return toolRiskExternalRead
			}
			return toolRiskManual
		case ConnectorTypeRPC:
			// RPC calls are POST requests, so the protocol itself reveals nothing
			// about side effects. Only an operator-declared read-only method may
			// be part of an autonomous run.
			if entry.Capability.ReadOnly && strings.TrimSpace(entry.Capability.RPCMethod) != "" {
				return toolRiskExternalRead
			}
			return toolRiskMutating
		default:
			return toolRiskManual
		}
	}
	// Registered custom APIs use a fixed HTTP template and are therefore
	// external reads under the same auto-search consent as web tools.
	return toolRiskExternalRead
}

// evaluateToolPolicy centralizes the admission decision for planner and
// inline calls. It is deliberately stricter than the legacy settings helper:
// a setting can enable a manual capability, but not grant a model unrestricted
// autonomous side effects.
func (e *StreamingEngine) evaluateToolPolicy(s appSettings, call XMLToolCall, autoSearch bool) toolPolicyDecision {
	def, ok := e.lookupToolDef(call.Name)
	if !ok {
		return denyTool("deny", "unknown_tool", toolRiskManual)
	}
	if !canRoleUseTool(s.ActiveRole, def.Name) {
		return denyTool("deny", "role_not_allowed", toolRiskManual)
	}

	risk := e.riskForTool(def)
	switch risk {
	case toolRiskLocalRead, toolRiskCompute:
		if !shouldAutoExecuteTool(s, toolRequest{Tool: def.Name}, autoSearch) {
			return denyTool("deny", "settings_not_allowed", risk)
		}
		return allowTool(risk)
	case toolRiskExternalRead:
		if !autoSearch {
			return denyTool("confirmation_required", "auto_search_disabled", risk)
		}
		if !shouldAutoExecuteTool(s, toolRequest{Tool: def.Name}, autoSearch) {
			return denyTool("deny", "settings_not_allowed", risk)
		}
		return allowTool(risk)
	case toolRiskCode:
		return denyTool("confirmation_required", "code_requires_explicit_user_action", risk)
	case toolRiskMutating:
		return denyTool("confirmation_required", "mutating_action_requires_explicit_user_action", risk)
	default:
		return denyTool("manual_only", "not_available_for_autonomous_execution", risk)
	}
}

// admitToolCall returns a canonical registered tool identifier plus its policy
// decision. It also rejects oversized model-controlled inputs before they can
// reach an executor.
func (e *StreamingEngine) admitToolCall(s appSettings, call XMLToolCall, autoSearch bool) (XMLToolCall, toolPolicyDecision) {
	def, ok := e.lookupToolDef(call.Name)
	if !ok {
		return call, denyTool("deny", "unknown_tool", toolRiskManual)
	}
	call.Name = def.Name
	call.Query = strings.TrimSpace(call.Query)
	if call.Query == "" && len(call.Arguments) > 0 {
		call.Query = toolQueryFromArguments(call.Arguments)
	}
	if len([]rune(call.Query)) > maxXMLToolQueryRunes {
		return call, denyTool("deny", "query_too_large", e.riskForTool(def))
	}
	if len(call.Arguments) > 0 {
		encoded, err := json.Marshal(call.Arguments)
		if err != nil || len(encoded) > maxXMLToolArgumentLen {
			return call, denyTool("deny", "arguments_too_large_or_invalid", e.riskForTool(def))
		}
	}
	if call.Query == "" && len(call.Arguments) == 0 {
		return call, denyTool("deny", "missing_input", e.riskForTool(def))
	}
	if _, builtin := builtinToolRisk(def.Name); builtin && call.Query == "" && (def.InputSchema == nil || len(call.Arguments) == 0) {
		return call, denyTool("deny", "missing_scalar_input", e.riskForTool(def))
	}
	return call, e.evaluateToolPolicy(s, call, autoSearch)
}

// autonomousToolDefs is used for both the agent planner and the model prompt.
// Listing only executable tools prevents the model from spending turns on an
// action that the engine would have to reject afterwards.
func (e *StreamingEngine) autonomousToolDefs(s appSettings, autoSearch bool) []toolDef {
	all := e.allKnownToolDefs()
	out := make([]toolDef, 0, len(all))
	for _, def := range all {
		if e.evaluateToolPolicy(s, XMLToolCall{Name: def.Name, Query: "_"}, autoSearch).Allowed {
			out = append(out, def)
		}
	}
	return out
}

// canonicalToolCallKey deduplicates structured calls by canonical JSON and
// legacy calls by whitespace-normalized input. It never mutates the input that
// reaches the tool.
func canonicalToolCallKey(call XMLToolCall) string {
	name := strings.ToLower(strings.TrimSpace(call.Name))
	if len(call.Arguments) > 0 {
		if encoded, err := json.Marshal(call.Arguments); err == nil {
			return toolDedupKey(name, "arguments:"+string(encoded))
		}
	}
	return toolDedupKey(name, strings.Join(strings.Fields(call.Query), " "))
}
