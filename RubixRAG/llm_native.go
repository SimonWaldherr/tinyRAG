package main

// Native chat adapters for providers whose APIs are not OpenAI-compatible.
// The adapters deliberately translate only the wire format. Tool execution
// stays in llm.go's runToolCalls, so SQL/HTTP/shop tools keep RubixRAG's
// existing authorization, auditing and caching semantics for every provider.

import (
	"context"
	"encoding/json"
	"fmt"
)

func (c *lmClient) nativeChatOnce(ctx context.Context, all []chatMsg, tools []toolDef) (chatMsg, error) {
	if c.chatModel == "" {
		return chatMsg{}, fmt.Errorf("%s: chat model not configured", c.provider)
	}
	switch c.provider {
	case "claude":
		return c.claudeChatOnce(ctx, all, tools)
	case "gemini":
		return c.geminiChatOnce(ctx, all, tools)
	default:
		return chatMsg{}, fmt.Errorf("unsupported native provider %q", c.provider)
	}
}

func nativeToolCall(id, name, arguments string) toolCall {
	var call toolCall
	call.ID = id
	call.Type = "function"
	call.Function.Name = name
	call.Function.Arguments = arguments
	return call
}

func nativeJSONValue(raw string) any {
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		return value
	}
	return raw
}

func (c *lmClient) nativePostJSON(ctx context.Context, endpoint string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", c.provider, err)
	}
	raw, err := c.llmPostJSONRetry(ctx, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("%s chat request failed: %w", c.provider, err)
	}
	return raw, nil
}
