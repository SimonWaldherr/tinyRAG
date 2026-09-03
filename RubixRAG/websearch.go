package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Web search (Tavily): the web_search agent tool — the "discovery" step
// web_research/fetch_url never had (agent.go — both require an
// already-known start URL, with no way to turn a plain-text question into
// a candidate URL). Given a query, returns a short list of results
// (title/URL/content snippet), which the agent can then hand to
// web_research (deep, multi-hop reading of one promising result) or
// fetch_url (single page) for the actual content.
//
// Tavily was chosen (see the research that led here) over Brave/Google/
// Azure-Bing-grounding because it is simultaneously (a) a plain,
// model-agnostic REST/JSON call — required since this app's answering
// model is admin-selectable across six providers (Local/Azure/OpenAI/
// OpenRouter/Claude/Gemini), not tied to whichever vendor's own search
// API happens to be used — and (b) needs no separate cloud project/
// resource provisioning, just an API key. Gemini's "Grounding with Google
// Search" and Azure's "Grounding with Bing Search" were both ruled out
// specifically because neither returns raw search results to the caller
// at all — they're single-vendor black boxes that search AND answer in
// one step, which can't be wired into a tool any of the other five
// backends could also call.
// ─────────────────────────────────────────────────────────────────────────────

// tavilyBaseURL is a var, not a const, so tests can point it at a fake
// server instead of the real Tavily endpoint (same reasoning as graph.go's
// graphBaseURL/graphAuthHost).
var tavilyBaseURL = "https://api.tavily.com"

const (
	webSearchDefaultMaxResults     = 5
	webSearchMaxResultsCeiling     = 20 // Tavily's own documented per-request ceiling
	webSearchDefaultTimeoutSeconds = 15
	webSearchTimeoutCeiling        = 60
	// webSearchContentCharsPerResult caps each result's content excerpt in
	// the text handed to the model — same "bound what one tool call can
	// cost in context" reasoning as agentSearchResultChars.
	webSearchContentCharsPerResult = 500
)

// agentWebSearchMaxResults / agentWebSearchTimeout resolve the configured
// web_search bounds, same clampInt convention as agentWebSearchRounds/
// agentWebSearchTimeout's web_research siblings.
func agentWebSearchMaxResults(cfg agentConfig) int {
	return clampInt(cfg.WebSearchMaxResults, webSearchDefaultMaxResults, webSearchMaxResultsCeiling)
}

func agentWebSearchTimeout(cfg agentConfig) time.Duration {
	secs := clampInt(cfg.WebSearchTimeoutSeconds, webSearchDefaultTimeoutSeconds, webSearchTimeoutCeiling)
	return time.Duration(secs) * time.Second
}

// resolveWebSearchAPIKey prefers WebSearchAPIKeyEnv over the inline
// WebSearchAPIKey, same convention as every other credential pair in this
// codebase (resolveSecret, connector.go).
func resolveWebSearchAPIKey(cfg agentConfig) string {
	return resolveSecret(cfg.WebSearchAPIKey, cfg.WebSearchAPIKeyEnv)
}

// tavilySearchResult is one hit from Tavily's response — only the fields
// this tool actually uses; Tavily's response carries more (images,
// per-request usage/credits, an optional synthesized "answer") that
// aren't needed here.
type tavilySearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type tavilySearchResponse struct {
	Results []tavilySearchResult `json:"results"`
}

// tavilySearch calls Tavily's POST /search endpoint (Bearer-token auth,
// JSON body/response — see https://docs.tavily.com/documentation/api-reference/endpoint/search).
// Deliberately its own small client (like graph.go's/shop.go's own HTTP
// plumbing) rather than the generic REST-connector/http_tool.go
// mechanism: that mechanism is GET + URL-placeholder only, and Tavily
// requires a POST with a JSON body.
func tavilySearch(ctx context.Context, apiKey, query string, maxResults int) (tavilySearchResponse, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query":       query,
		"max_results": maxResults,
	})
	if err != nil {
		return tavilySearchResponse{}, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilyBaseURL+"/search", bytes.NewReader(reqBody))
	if err != nil {
		return tavilySearchResponse{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", connectorUserAgent)

	resp, err := connectorHTTPClient.Do(req)
	if err != nil {
		return tavilySearchResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return tavilySearchResponse{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return tavilySearchResponse{}, fmt.Errorf("%d: %s", resp.StatusCode, truncateRunesNote(string(raw), 300))
	}
	var out tavilySearchResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return tavilySearchResponse{}, fmt.Errorf("parse response: %w", err)
	}
	return out, nil
}

const webSearchToolName = "web_search"

func webSearchToolDef() toolDef {
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name: webSearchToolName,
			Description: "Durchsucht das öffentliche Web nach einer Stichwortfrage und liefert eine kurze Trefferliste zurück (Titel, Kurzauszug, URL) — der Such-/Entdeckungsschritt, den fetch_url/web_research nicht haben (beide brauchen bereits eine bekannte URL). " +
				"Nutze dies zuerst, um passende Seiten zu einem Thema zu finden; für die Volltext-Lektüre eines vielversprechenden Treffers danach fetch_url (eine Seite) oder web_research (mehrseitige Recherche ab dieser URL) aufrufen.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Die Suchanfrage in Stichworten oder als Frage."},
				},
				"required": []string{"query"},
			},
		},
	}
}

// webSearchToolExecutor wraps tavilySearch as a toolExecutor, formatting
// results as a numbered list (title, URL, truncated content excerpt) —
// plain text the model can read directly, not raw JSON.
func webSearchToolExecutor(s appSettings) toolExecutor {
	maxResults := agentWebSearchMaxResults(s.Agent)
	timeout := agentWebSearchTimeout(s.Agent)
	apiKey := resolveWebSearchAPIKey(s.Agent)

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
		if apiKey == "" {
			return "", fmt.Errorf("web_search: kein Such-API-Key konfiguriert (Einstellungen → Agent)")
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := tavilySearch(ctx, apiKey, query, maxResults)
		if err != nil {
			return "", fmt.Errorf("web search failed: %w", err)
		}
		if len(res.Results) == 0 {
			return "Keine Treffer gefunden.", nil
		}

		var b strings.Builder
		for i, r := range res.Results {
			fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, r.Title, r.URL, truncateRunesNote(strings.TrimSpace(r.Content), webSearchContentCharsPerResult))
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}
