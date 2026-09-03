package main

// Deliberate scope note — no prompt caching here, unlike llm_claude.go.
// Gemini's context-caching is not a request-shape opt-in like Claude's
// cache_control marker: it's a separate, standalone CachedContent resource
// (its own create call, its own handle, its own TTL/lifecycle to manage
// alongside every generateContent call that references it). That's a
// materially bigger feature than a marker on an existing request, so it was
// deliberately left out of the prompt-caching pass that added Claude's
// cache_control (llm_claude.go) — a real follow-up, not an oversight.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func (c *lmClient) geminiChatOnce(ctx context.Context, all []chatMsg, tools []toolDef) (chatMsg, error) {
	system, contents := geminiContents(all)
	body := map[string]any{"contents": contents}
	if system != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": system}}}
	}
	if nativeTools := geminiTools(tools); len(nativeTools) > 0 {
		body["tools"] = []map[string]any{{"functionDeclarations": nativeTools}}
	}
	model := strings.TrimPrefix(strings.TrimSpace(c.chatModel), "models/")
	base := strings.TrimSuffix(strings.TrimRight(c.base, "/"), "/v1beta")
	endpoint := base + "/v1beta/models/" + url.PathEscape(model) + ":generateContent"
	raw, err := c.nativePostJSON(ctx, endpoint, body)
	if err != nil {
		return chatMsg{}, err
	}
	var response struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return chatMsg{}, fmt.Errorf("gemini response: %w", err)
	}
	if len(response.Candidates) == 0 {
		return chatMsg{}, fmt.Errorf("gemini response contained no candidates")
	}
	// Gemini always returns usageMetadata, unlike the OpenAI-compatible SSE
	// path (llm.go's chatStreamMessages) — never an estimate here.
	tokenUsageFromContext(ctx).add(tokenUsageEvent{Provider: "gemini", Model: c.chatModel, PromptTokens: response.UsageMetadata.PromptTokenCount, CompletionTokens: response.UsageMetadata.CandidatesTokenCount})
	var text strings.Builder
	var calls []toolCall
	for _, part := range response.Candidates[0].Content.Parts {
		text.WriteString(part.Text)
		if part.FunctionCall != nil && strings.TrimSpace(part.FunctionCall.Name) != "" {
			args, _ := json.Marshal(part.FunctionCall.Args)
			if len(args) == 0 || string(args) == "null" {
				args = []byte("{}")
			}
			calls = append(calls, nativeToolCall("gemini-"+fmt.Sprint(len(calls)+1), part.FunctionCall.Name, string(args)))
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return chatMsg{}, fmt.Errorf("gemini response contained no text or tool call")
	}
	return chatMsg{Role: "assistant", Content: text.String(), ToolCalls: calls}, nil
}

func geminiContents(all []chatMsg) (string, []map[string]any) {
	var system strings.Builder
	contents := make([]map[string]any, 0, len(all))
	for _, message := range all {
		if message.Role == "system" {
			if strings.TrimSpace(message.Content) != "" {
				if system.Len() > 0 {
					system.WriteString("\n")
				}
				system.WriteString(message.Content)
			}
			continue
		}
		role := "user"
		if message.Role == "assistant" {
			role = "model"
		}
		parts := make([]map[string]any, 0, len(message.ToolCalls)+1)
		if message.Content != "" {
			parts = append(parts, map[string]any{"text": message.Content})
		}
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				parts = append(parts, map[string]any{"functionCall": map[string]any{
					"name": call.Function.Name,
					"args": nativeJSONValue(call.Function.Arguments),
				}})
			}
		}
		if message.Role == "tool" {
			role = "user"
			name := message.Name
			if name == "" {
				name = "unknown_tool"
			}
			functionResponse := map[string]any{"functionResponse": map[string]any{
				"name":     name,
				"response": map[string]any{"result": nativeJSONValue(message.Content)},
			}}
			if len(contents) > 0 && contents[len(contents)-1]["role"] == "user" {
				if previous, ok := contents[len(contents)-1]["parts"].([]map[string]any); ok {
					contents[len(contents)-1]["parts"] = append(previous, functionResponse)
					continue
				}
			}
			parts = []map[string]any{functionResponse}
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"text": ""})
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	return system.String(), contents
}

func geminiTools(tools []toolDef) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Function.Name) == "" {
			continue
		}
		result = append(result, map[string]any{
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
			"parameters":  tool.Function.Parameters,
		})
	}
	return result
}
