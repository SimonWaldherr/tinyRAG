package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ephemeralCacheControl is the single cache_control marker value used on
// every breakpoint this file sets — one place to change the type/TTL if
// that ever needs tuning.
func ephemeralCacheControl() map[string]any {
	return map[string]any{"type": "ephemeral"}
}

func (c *lmClient) claudeChatOnce(ctx context.Context, all []chatMsg, tools []toolDef) (chatMsg, error) {
	system, messages := claudeMessages(all)
	markLastMessageForCache(messages)
	body := map[string]any{
		"model":      c.chatModel,
		"max_tokens": 4096,
		"messages":   messages,
	}
	if system != "" {
		// Anthropic's Messages API accepts "system" as either a plain string
		// or an array of content blocks — only the array form can carry a
		// cache_control marker. chatWithToolsBudget (llm.go) builds this
		// exact system string once per answer and reuses it unchanged across
		// every tool-calling round-trip (see its own doc comment plus
		// llm.go's package-level prompt-caching comment), so it is exactly
		// the large, stable-within-one-answer prefix prompt caching targets:
		// marking it ephemeral here lets Anthropic serve every round after
		// the first from cache instead of reprocessing the system prompt
		// (and, transitively, the "Kontext:" retrieval block folded into it
		// — see handlers.go/openai_api.go) at full price on every round.
		//
		// This is one of (at most) two cache breakpoints per request: this
		// static system+tools prefix, and — via markLastMessageForCache
		// above — a second, ROLLING one at the end of the accumulated
		// tool-call transcript. Anthropic allows up to four; two is well
		// within budget. The canonical cache-prefix order is tools → system
		// → messages, so this system breakpoint transitively caches the
		// (also stable-per-answer) tools block ahead of it too.
		body["system"] = []map[string]any{
			{
				"type":          "text",
				"text":          system,
				"cache_control": ephemeralCacheControl(),
			},
		}
	}
	if nativeTools := claudeTools(tools); len(nativeTools) > 0 {
		body["tools"] = nativeTools
	}
	raw, err := c.nativePostJSON(ctx, strings.TrimRight(c.base, "/")+"/v1/messages", body)
	if err != nil {
		return chatMsg{}, err
	}
	var response struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			// CacheCreationInputTokens/CacheReadInputTokens are only ever
			// non-zero once the cache_control marker above is actually in
			// effect (prefix ≥ the model's minimum cacheable length, and a
			// prior request already wrote the cache within its TTL) — a
			// cache miss reports both as 0/absent, which is not an error,
			// just "this request wrote a fresh cache entry it didn't read".
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return chatMsg{}, fmt.Errorf("claude response: %w", err)
	}
	// Anthropic always returns usage, unlike the OpenAI-compatible SSE path
	// (llm.go's chatStreamMessages) — never an estimate here.
	tokenUsageFromContext(ctx).add(tokenUsageEvent{
		Provider:                 "claude",
		Model:                    c.chatModel,
		PromptTokens:             response.Usage.InputTokens,
		CompletionTokens:         response.Usage.OutputTokens,
		CacheCreationInputTokens: response.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     response.Usage.CacheReadInputTokens,
	})
	var text strings.Builder
	var calls []toolCall
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args := strings.TrimSpace(string(block.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, nativeToolCall(block.ID, block.Name, args))
		}
	}
	if len(response.Content) == 0 || (text.Len() == 0 && len(calls) == 0) {
		return chatMsg{}, fmt.Errorf("claude response contained no text or tool call")
	}
	return chatMsg{Role: "assistant", Content: text.String(), ToolCalls: calls}, nil
}

func claudeMessages(all []chatMsg) (string, []map[string]any) {
	var system strings.Builder
	messages := make([]map[string]any, 0, len(all))
	for _, message := range all {
		switch message.Role {
		case "system":
			if strings.TrimSpace(message.Content) != "" {
				if system.Len() > 0 {
					system.WriteString("\n")
				}
				system.WriteString(message.Content)
			}
		case "assistant":
			content := make([]map[string]any, 0, len(message.ToolCalls)+1)
			if message.Content != "" {
				content = append(content, map[string]any{"type": "text", "text": message.Content})
			}
			for _, call := range message.ToolCalls {
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Function.Name,
					"input": nativeJSONValue(call.Function.Arguments),
				})
			}
			if len(content) == 0 {
				content = append(content, map[string]any{"type": "text", "text": ""})
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": content})
		case "tool":
			// Claude represents tool results as user messages. A result block
			// can be correlated by tool_use_id and may contain arbitrary JSON.
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": message.ToolCallID,
				"content":     message.Content,
			}
			if len(messages) > 0 && messages[len(messages)-1]["role"] == "user" {
				if blocks, ok := messages[len(messages)-1]["content"].([]map[string]any); ok {
					messages[len(messages)-1]["content"] = append(blocks, block)
					continue
				}
			}
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": []map[string]any{block},
			})
		default:
			messages = append(messages, map[string]any{"role": "user", "content": message.Content})
		}
	}
	return system.String(), messages
}

// markLastMessageForCache adds a rolling cache_control breakpoint to the last
// content block of the last message — the incremental-caching technique
// Anthropic recommends for multi-turn / agentic loops. It complements the
// static system+tools breakpoint (claudeChatOnce above): that one caches the
// large, stable prefix every round shares; this one caches the GROWING part
// (the accumulated assistant tool-call and tool-result transcript), so round
// N reads rounds 1..N-1 from cache instead of reprocessing every prior tool
// result — search hits (~400 chars each), get_source_content (up to 8000),
// fetch_url (6000) — at full input price on every one of the up-to-six agent
// rounds and inside every parallel sub-agent's own loop.
//
// Why it produces cache HITS: each round appends the assistant message plus
// its tool results, then re-sends the whole conversation (llm.go's
// chatWithToolsBudgetDeadline only ever appends to `all`). Round N-1's marked
// position (end of its last tool_result block) is therefore an exact prefix
// of round N's request, which Anthropic serves from cache; round N writes a
// new, one-turn-longer entry for round N+1 to read. The final forced-answer
// call (chatStreamMessages → nativeChatOnce with nil tools) ends on the same
// tool-result transcript and so reads it from cache too.
//
// Deliberately only marks ARRAY-form content (assistant blocks, or a
// tool_result-carrying user message — claudeMessages emits these). A trailing
// plain-string user message (the single-round Chat path, round 1 before any
// tool call, a follow-up chat turn) is left byte-identical and unmarked:
// there is no accumulated transcript worth a second breakpoint there, and not
// touching it keeps that common path exactly as it was. No-op on an empty
// message list or a message whose block list is empty.
func markLastMessageForCache(messages []map[string]any) {
	if len(messages) == 0 {
		return
	}
	last := messages[len(messages)-1]
	blocks, ok := last["content"].([]map[string]any)
	if !ok || len(blocks) == 0 {
		return
	}
	blocks[len(blocks)-1]["cache_control"] = ephemeralCacheControl()
}

func claudeTools(tools []toolDef) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Function.Name) == "" {
			continue
		}
		result = append(result, map[string]any{
			"name":         tool.Function.Name,
			"description":  tool.Function.Description,
			"input_schema": tool.Function.Parameters,
		})
	}
	return result
}
