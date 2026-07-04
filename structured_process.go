package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"
)

type processRAGOptions struct {
	Enabled bool   `json:"enabled"`
	Query   string `json:"query"`
	K       int    `json:"k"`
}

type processOptions struct {
	ValidateJSON *bool `json:"validate_json"`
	RepairJSON   *bool `json:"repair_json"`
	MaxRetries   int   `json:"max_retries"`
}

type processRequest struct {
	RequestID      string            `json:"request_id"`
	Mode           string            `json:"mode"`
	SystemPrompt   string            `json:"system_prompt"`
	PrePrompt      string            `json:"pre_prompt"`
	Input          any               `json:"input"`
	PostPrompt     string            `json:"post_prompt"`
	ResponseSchema map[string]any    `json:"response_schema"`
	PersonaID      string            `json:"persona_id"`
	RAG            processRAGOptions `json:"rag"`
	Options        processOptions    `json:"options"`
}

type processResponse struct {
	RequestID       string     `json:"request_id"`
	OK              bool       `json:"ok"`
	Mode            string     `json:"mode"`
	ValidJSON       bool       `json:"valid_json"`
	Attempts        int        `json:"attempts"`
	DurationMS      int64      `json:"duration_ms"`
	RAGUsed         bool       `json:"rag_used"`
	RAGQuery        string     `json:"rag_query,omitempty"`
	ContextChars    int        `json:"context_chars,omitempty"`
	Raw             string     `json:"raw,omitempty"`
	Result          any        `json:"result,omitempty"`
	Error           string     `json:"error,omitempty"`
	ValidationError string     `json:"validation_error,omitempty"`
	Retrieval       *debugInfo `json:"retrieval,omitempty"`
}

var fencedJSONBlockRE = regexp.MustCompile("(?is)```(?:json)?\\s*(.*?)\\s*```")

func boolOrDefault(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func normalizeProcessMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "rag":
		return "rag"
	default:
		return "direct"
	}
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func processQueryFromInput(v any) string {
	raw := strings.TrimSpace(compactJSON(v))
	if len(raw) > 1000 {
		raw = raw[:1000]
	}
	return raw
}

func firstNonSpaceIndex(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return i
		}
	}
	return -1
}

func extractBalancedJSON(s string) (string, error) {
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '{' || s[i] == '[' {
			start = i
			break
		}
	}
	if start == -1 {
		return "", fmt.Errorf("no JSON object or array found")
	}
	stack := make([]byte, 0, 8)
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, ch)
		case '}':
			if len(stack) == 0 || stack[len(stack)-1] != '{' {
				return "", fmt.Errorf("invalid JSON structure")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return s[start : i+1], nil
			}
		case ']':
			if len(stack) == 0 || stack[len(stack)-1] != '[' {
				return "", fmt.Errorf("invalid JSON structure")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unterminated JSON structure")
}

func extractFirstJSONValue(text string) (string, error) {
	candidates := []string{strings.TrimSpace(text)}
	matches := fencedJSONBlockRE.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if len(m) > 1 {
			candidates = append(candidates, strings.TrimSpace(m[1]))
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if idx := firstNonSpaceIndex(candidate); idx >= 0 {
			if candidate[idx] == '{' || candidate[idx] == '[' {
				var parsed any
				if err := json.Unmarshal([]byte(candidate[idx:]), &parsed); err == nil {
					return strings.TrimSpace(candidate[idx:]), nil
				}
			}
		}
		if out, err := extractBalancedJSON(candidate); err == nil {
			var parsed any
			if err := json.Unmarshal([]byte(out), &parsed); err == nil {
				return out, nil
			}
		}
	}
	return "", fmt.Errorf("no valid JSON found in model output")
}

func jsonTypeName(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if math.Trunc(v) == v {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func schemaTypeMatches(expected string, value any) bool {
	switch expected {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && math.Trunc(n) == n
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

func validateJSONAgainstSchema(value any, schema map[string]any, path string) error {
	if len(schema) == 0 {
		return nil
	}
	if path == "" {
		path = "$"
	}
	if enumRaw, ok := schema["enum"].([]any); ok && len(enumRaw) > 0 {
		matched := false
		for _, allowed := range enumRaw {
			if reflect.DeepEqual(allowed, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %v is not part of enum", path, value)
		}
	}
	if rawType, ok := schema["type"]; ok {
		switch tv := rawType.(type) {
		case string:
			if !schemaTypeMatches(tv, value) {
				return fmt.Errorf("%s: expected %s, got %s", path, tv, jsonTypeName(value))
			}
		case []any:
			matched := false
			for _, item := range tv {
				if ts, ok := item.(string); ok && schemaTypeMatches(ts, value) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%s: unexpected type %s", path, jsonTypeName(value))
			}
		}
	}

	switch actual := value.(type) {
	case map[string]any:
		if reqRaw, ok := schema["required"].([]any); ok {
			for _, item := range reqRaw {
				key, ok := item.(string)
				if !ok {
					continue
				}
				if _, exists := actual[key]; !exists {
					return fmt.Errorf("%s.%s: required property missing", path, key)
				}
			}
		}
		properties := map[string]any{}
		if raw, ok := schema["properties"].(map[string]any); ok {
			properties = raw
		}
		if rawAP, ok := schema["additionalProperties"].(bool); ok && !rawAP {
			for key := range actual {
				if _, exists := properties[key]; !exists {
					return fmt.Errorf("%s.%s: additional property not allowed", path, key)
				}
			}
		}
		for key, item := range actual {
			childSchema, ok := properties[key].(map[string]any)
			if !ok {
				continue
			}
			if err := validateJSONAgainstSchema(item, childSchema, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		if minItems, ok := schema["minItems"].(float64); ok && len(actual) < int(minItems) {
			return fmt.Errorf("%s: expected at least %d items", path, int(minItems))
		}
		if maxItems, ok := schema["maxItems"].(float64); ok && len(actual) > int(maxItems) {
			return fmt.Errorf("%s: expected at most %d items", path, int(maxItems))
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for i, item := range actual {
				if err := validateJSONAgainstSchema(item, itemSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case string:
		if minLength, ok := schema["minLength"].(float64); ok && len(actual) < int(minLength) {
			return fmt.Errorf("%s: expected minimum length %d", path, int(minLength))
		}
		if maxLength, ok := schema["maxLength"].(float64); ok && len(actual) > int(maxLength) {
			return fmt.Errorf("%s: expected maximum length %d", path, int(maxLength))
		}
	case float64:
		if minimum, ok := schema["minimum"].(float64); ok && actual < minimum {
			return fmt.Errorf("%s: expected minimum %v", path, minimum)
		}
		if maximum, ok := schema["maximum"].(float64); ok && actual > maximum {
			return fmt.Errorf("%s: expected maximum %v", path, maximum)
		}
	}
	return nil
}

func buildStructuredProcessSystemPrompt(s appSettings, personaPrompt, customSystemPrompt, ctxText string, schema map[string]any) string {
	var sb strings.Builder
	sb.WriteString(buildAssistantPolicyPrompt(s))
	sb.WriteString("Du arbeitest als strukturierte JSON-Schnittstelle fuer Backend-Prozesse.\n")
	sb.WriteString("Gib exakt einen JSON-Wert zurueck, der zum angeforderten Schema passt.\n")
	sb.WriteString("Keine Markdown-Codeblocks, keine Kommentare, keine Erklaerungen, keine Vor- oder Nachsaetze.\n")
	sb.WriteString("Wenn Kontext aus der lokalen Wissensbasis vorhanden ist, nutze ihn nur als Zusatzinformation und markiere keine internen Quellen im JSON.\n")
	if personaPrompt != "" {
		sb.WriteString("\n### Persona\n")
		sb.WriteString(personaPrompt)
		sb.WriteString("\n")
	}
	if customSystemPrompt != "" {
		sb.WriteString("\n### System Prompt\n")
		sb.WriteString(customSystemPrompt)
		sb.WriteString("\n")
	}
	if ctxText != "" {
		sb.WriteString("\n### Lokaler Kontext\n")
		sb.WriteString(ctxText)
		sb.WriteString("\n")
	}
	if len(schema) > 0 {
		sb.WriteString("\n### Antwortschema\n")
		sb.WriteString(prettyJSON(schema))
		sb.WriteString("\n")
	}
	return sb.String()
}

func buildStructuredProcessUserPrompt(req processRequest) string {
	var sb strings.Builder
	if strings.TrimSpace(req.PrePrompt) != "" {
		sb.WriteString(strings.TrimSpace(req.PrePrompt))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Input JSON:\n")
	sb.WriteString(prettyJSON(req.Input))
	sb.WriteString("\n\n")
	if len(req.ResponseSchema) > 0 {
		sb.WriteString("Erwartetes JSON-Schema:\n")
		sb.WriteString(prettyJSON(req.ResponseSchema))
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(req.PostPrompt) != "" {
		sb.WriteString(strings.TrimSpace(req.PostPrompt))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Antworte ausschliesslich mit validem JSON.")
	return sb.String()
}

func buildStructuredRepairPrompt(req processRequest, validationErr, raw string) string {
	var sb strings.Builder
	sb.WriteString("Die vorige Antwort war kein gueltiges JSON fuer das angeforderte Schema.\n\n")
	if validationErr != "" {
		sb.WriteString("Validierungsfehler:\n")
		sb.WriteString(validationErr)
		sb.WriteString("\n\n")
	}
	if len(req.ResponseSchema) > 0 {
		sb.WriteString("Schema:\n")
		sb.WriteString(prettyJSON(req.ResponseSchema))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Urspruengliche Modellantwort:\n")
	sb.WriteString(raw)
	sb.WriteString("\n\nGib jetzt nur die reparierte JSON-Antwort zurueck.")
	return sb.String()
}

func processModelOutput(raw string, schema map[string]any, validate bool) (string, any, string, error) {
	cleaned := stripInternalThinking(strings.TrimSpace(raw))
	jsonText, err := extractFirstJSONValue(cleaned)
	if err != nil {
		return "", nil, "", err
	}
	var parsed any
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return jsonText, nil, "", err
	}
	if validate && len(schema) > 0 {
		if err := validateJSONAgainstSchema(parsed, schema, "$"); err != nil {
			return jsonText, parsed, err.Error(), err
		}
	}
	return jsonText, parsed, "", nil
}

func runStructuredProcess(ctx context.Context, rag *ragSystem, s appSettings, personaPrompt string, req processRequest) processResponse {
	start := time.Now()
	resp := processResponse{
		RequestID: req.RequestID,
		Mode:      normalizeProcessMode(req.Mode),
	}
	validateJSON := boolOrDefault(req.Options.ValidateJSON, true)
	repairJSON := boolOrDefault(req.Options.RepairJSON, true)
	maxRetries := req.Options.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries == 0 && repairJSON {
		maxRetries = 2
	}
	var ctxText string
	var retrieval *debugInfo
	if req.RAG.Enabled || resp.Mode == "rag" {
		resp.RAGUsed = true
		resp.Mode = "rag"
		query := strings.TrimSpace(req.RAG.Query)
		if query == "" {
			query = processQueryFromInput(req.Input)
		}
		k := req.RAG.K
		if k <= 0 {
			k = rag.k
		}
		var err error
		ctxText, retrieval, err = rag.prepareDirectContext(query, k)
		if err != nil {
			resp.Attempts = 0
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Error = err.Error()
			resp.RAGQuery = query
			return resp
		}
		resp.RAGQuery = query
		resp.ContextChars = len(ctxText)
		resp.Retrieval = retrieval
	}
	systemPrompt := buildStructuredProcessSystemPrompt(s, personaPrompt, req.SystemPrompt, ctxText, req.ResponseSchema)
	userPrompt := buildStructuredProcessUserPrompt(req)
	msgs := []chatMsg{{Role: "user", Content: userPrompt}}

	var raw string
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		var buf bytes.Buffer
		if err := rag.getLM().chatStream(ctx, systemPrompt, msgs, &buf); err != nil {
			resp.Attempts = attempt
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Error = err.Error()
			resp.Raw = strings.TrimSpace(buf.String())
			return resp
		}
		raw = strings.TrimSpace(buf.String())
		jsonText, parsed, validationErr, err := processModelOutput(raw, req.ResponseSchema, validateJSON)
		if err == nil {
			resp.OK = true
			resp.ValidJSON = true
			resp.Attempts = attempt
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Raw = jsonText
			resp.Result = parsed
			return resp
		}
		resp.Raw = raw
		resp.ValidationError = validationErr
		if attempt > maxRetries || !repairJSON {
			resp.Attempts = attempt
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.Error = err.Error()
			return resp
		}
		msgs = []chatMsg{{
			Role:    "user",
			Content: buildStructuredRepairPrompt(req, validationErr, raw),
		}}
	}
	resp.DurationMS = time.Since(start).Milliseconds()
	resp.Error = "processing failed"
	return resp
}
