package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Azure "Grounding with Bing Search": the azure_bing_search agent tool —
// implemented via Azure OpenAI's Responses API `web_search` tool (built on
// Grounding with Bing Search under the hood), a DIFFERENT API surface than
// the Chat Completions endpoint this app's normal chat/embedding calls use
// (llm.go's chatCompletionsURL/azureDeploymentURL). See
// https://learn.microsoft.com/en-us/azure/ai-foundry/openai/how-to/web-search.
//
// Unlike web_search (Tavily, websearch.go), Azure's own web-search tool
// never hands raw search results back to the caller — it searches AND
// synthesizes a cited answer together, inside one Azure-hosted model call.
// This still fits R3's toolExecutor shape (a function returning one string
// result) because it's exactly ONE independent, self-contained call — the
// same "nested call, return a string" shape web_research/delegate_subtasks
// already use, just backed by a different endpoint/protocol instead of a
// nested chatWithToolsBudget loop. The grounded text + cited URLs this
// returns are handed back as plain text to whichever backend is actually
// running the OUTER agent loop, so Azure is used purely as a
// grounded-search microservice here — reachable regardless of which
// profile is answering the overall request, the same role Tavily plays
// for web_search. Reuses appSettings.Profiles.Azure's existing
// BaseURL/APIKey/ChatModel (no separate credential set): if that profile
// is already configured with a GPT-4-class deployment (for regular chat,
// or purely for this), this tool works with no further setup beyond the
// opt-in flag below.
// ─────────────────────────────────────────────────────────────────────────────

const (
	azureResponsesAPIPath = "/openai/v1/responses"

	azureBingSearchDefaultTimeoutSeconds = 30
	azureBingSearchTimeoutCeiling        = 120
	// azureBingSearchContentChars caps the returned (already-synthesized,
	// already concise) answer text — a generous ceiling since this is one
	// coherent answer, not a raw page dump.
	azureBingSearchContentChars = 4000
)

// agentAzureBingSearchTimeout resolves the configured azure_bing_search
// timeout, same clampInt convention as every other tool budget in agent.go.
func agentAzureBingSearchTimeout(cfg agentConfig) time.Duration {
	secs := clampInt(cfg.AzureBingSearchTimeoutSeconds, azureBingSearchDefaultTimeoutSeconds, azureBingSearchTimeoutCeiling)
	return time.Duration(secs) * time.Second
}

// azureResponsesRequest is the (small) subset of the Responses API request
// body this tool needs.
type azureResponsesRequest struct {
	Model string                     `json:"model"`
	Tools []azureResponsesToolConfig `json:"tools"`
	Input string                     `json:"input"`
}

type azureResponsesToolConfig struct {
	Type string `json:"type"` // "web_search"
}

// azureResponsesOutputItem is one entry of the Responses API's "output"
// array — a union type (`web_search_call` | `message` | `reasoning`);
// Content is only populated for `message`, which is all this tool reads.
type azureResponsesOutputItem struct {
	Type    string                         `json:"type"`
	Content []azureResponsesMessageContent `json:"content,omitempty"`
}

type azureResponsesMessageContent struct {
	Type        string                     `json:"type"` // "output_text"
	Text        string                     `json:"text"`
	Annotations []azureResponsesAnnotation `json:"annotations,omitempty"`
}

// azureResponsesAnnotation is one `url_citation` annotation on a message's
// output text — the grounded source R3 surfaces to the model, since Azure
// never returns raw search results (see this file's package comment).
type azureResponsesAnnotation struct {
	Type  string `json:"type"` // "url_citation"
	URL   string `json:"url"`
	Title string `json:"title"`
}

type azureResponsesResponse struct {
	Output []azureResponsesOutputItem `json:"output"`
}

// azureResponsesURL builds the Responses API endpoint URL — intentionally
// NOT azureDeploymentURL (llm.go): this "v1" surface has no
// /openai/deployments/{name}/ segment and no api-version query parameter
// at all (it's Azure's newer, versionless API shape), unlike Chat
// Completions/embeddings.
func (c *lmClient) azureResponsesURL() string {
	return strings.TrimRight(c.base, "/") + azureResponsesAPIPath
}

// azureBingSearch calls this client's Azure OpenAI deployment via the
// Responses API's built-in "web_search" tool — a single, independent call,
// separate from and never mixed into this client's normal
// chatCompletionsURL()-based chat/tool loop. Reuses llmPostJSONRetry for
// the actual HTTP round-trip, so auth header, User-Agent, Content-Type and
// 429/5xx retry behavior exactly match every other call this client makes.
// Returns the model's synthesized, grounded answer text plus the distinct
// cited source URLs (title+URL, de-duplicated, first-seen order). Errors
// if c isn't an Azure client — this tool only exists on Azure's Responses
// API, there's no equivalent for any other provider.
func (c *lmClient) azureBingSearch(ctx context.Context, query string) (text string, citations []azureResponsesAnnotation, err error) {
	if !c.isAzure() {
		return "", nil, fmt.Errorf("azure_bing_search requires an Azure OpenAI profile")
	}
	reqBody, err := json.Marshal(azureResponsesRequest{
		Model: c.chatModel,
		Tools: []azureResponsesToolConfig{{Type: "web_search"}},
		Input: query,
	})
	if err != nil {
		return "", nil, fmt.Errorf("encode request: %w", err)
	}
	raw, err := c.llmPostJSONRetry(ctx, c.azureResponsesURL(), reqBody)
	if err != nil {
		return "", nil, err
	}
	var parsed azureResponsesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", nil, fmt.Errorf("parse response: %w", err)
	}

	var textOut strings.Builder
	seen := map[string]bool{}
	for _, item := range parsed.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			textOut.WriteString(c.Text)
			for _, a := range c.Annotations {
				if a.Type == "url_citation" && a.URL != "" && !seen[a.URL] {
					seen[a.URL] = true
					citations = append(citations, a)
				}
			}
		}
	}
	return strings.TrimSpace(textOut.String()), citations, nil
}

const azureBingSearchToolName = "azure_bing_search"

func azureBingSearchToolDef() toolDef {
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name: azureBingSearchToolName,
			Description: "Beantwortet eine Frage mit aktuellen Web-Informationen über Azure OpenAI's Grounding with Bing Search — anders als web_search/fetch_url liefert dies KEINE Rohtreffer, sondern direkt eine fertig formulierte, mit Quellen-URLs belegte Antwort in einem Schritt. " +
				"Gut für eine einzelne, klar formulierbare Faktenfrage zu aktuellen Web-Inhalten; für eine Trefferliste zum selbst Weiterlesen stattdessen web_search nutzen.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Die Frage, die mit aktuellen Web-Informationen beantwortet werden soll."},
				},
				"required": []string{"query"},
			},
		},
	}
}

// azureBingSearchToolExecutor wraps azureBingSearch as a toolExecutor,
// formatting the grounded answer plus a "Quellen:" list of cited URLs —
// plain text the model can read directly, not raw JSON.
func azureBingSearchToolExecutor(rag *ragSystem, s appSettings) toolExecutor {
	timeout := agentAzureBingSearchTimeout(s.Agent)

	return func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return "", fmt.Errorf("empty query")
		}

		client := rag.getChatLM("azure")
		if client == nil || !client.isAzure() {
			return "", fmt.Errorf("azure_bing_search: kein Azure-OpenAI-Profil konfiguriert (Einstellungen → LLM-Backends → Azure)")
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		text, citations, err := client.azureBingSearch(ctx, query)
		if err != nil {
			return "", fmt.Errorf("azure bing search failed: %w", err)
		}
		if text == "" {
			return "Keine Antwort erhalten.", nil
		}

		out := truncateRunesNote(text, azureBingSearchContentChars)
		if len(citations) > 0 {
			var b strings.Builder
			b.WriteString(out)
			b.WriteString("\n\nQuellen:\n")
			for _, c := range citations {
				title := c.Title
				if title == "" {
					title = c.URL
				}
				fmt.Fprintf(&b, "- %s: %s\n", title, c.URL)
			}
			out = strings.TrimRight(b.String(), "\n")
		}
		return out, nil
	}
}
