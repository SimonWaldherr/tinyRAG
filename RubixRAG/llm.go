package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// OpenAI-compatible / Azure OpenAI client
//
// R3 talks to two kinds of backend with the same request/response shape:
// a local OpenAI-compatible server (LM Studio, Ollama, vLLM, ...) and Azure
// OpenAI Service. The only real differences are the URL shape, the auth
// header and whether "model" belongs in the request body — so, following
// the pattern used by the sibling llmflow6/promptcron projects, this is one
// client with an isAzure() branch rather than two separate implementations.
// ─────────────────────────────────────────────────────────────────────────────

// lmClient is a small OpenAI/Azure-OpenAI-compatible client used for
// embeddings and chat completions.
type lmClient struct {
	provider   string // local, azure, openai, openrouter, claude or gemini
	base       string // local/OpenAI: e.g. http://localhost:1234 ; Azure: https://<resource>.openai.azure.com
	apiVersion string // Azure only, e.g. "2024-10-21"
	embedModel string // local/OpenAI: model id ; Azure: embeddings deployment name
	chatModel  string // local/OpenAI: model id ; Azure: chat deployment name
	apiKey     string
	http       *http.Client
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls is set on an assistant message that requested one or more
	// tool calls instead of (or alongside) answering directly — only ever
	// populated by chatOnce's response parsing, never set by callers.
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	// ToolCallID/Name identify which tool call a "tool"-role message is
	// answering, per the OpenAI tool-calling message shape.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
	// Parts, when non-empty, carries a vision-capable message's content as
	// the OpenAI/Azure multi-part array shape (text + one or more
	// image_url parts) instead of a plain string — see MarshalJSON below.
	// Every existing call site leaves this nil and is completely
	// unaffected; only buildUserMessage (handlers.go) sets it, and only
	// for a profile whose llmProfile.SupportsVision is true.
	Parts []chatContentPart `json:"-"`
}

// chatContentPart is one element of a vision message's content array, per
// the shape both Azure OpenAI and OpenAI-compatible /chat/completions
// endpoints already accept — no new endpoint or request shape is needed,
// only this payload variant.
type chatContentPart struct {
	Type     string        `json:"type"` // "text" | "image_url"
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"` // "data:<mime>;base64,<...>" or a plain https:// URL
}

// MarshalJSON keeps every pre-existing caller byte-for-byte identical
// (Parts is nil for all of them, so this takes the same path
// encoding/json's default struct marshaling always did) while letting
// buildUserMessage opt in to a multi-part "content" array purely by
// setting Parts — no second message type, no branching at every call
// site that builds a chatMsg.
func (m chatMsg) MarshalJSON() ([]byte, error) {
	type plain chatMsg // same fields/tags, minus this method, to avoid infinite recursion
	if len(m.Parts) == 0 {
		return json.Marshal(plain(m))
	}
	return json.Marshal(struct {
		Role       string            `json:"role"`
		Content    []chatContentPart `json:"content"`
		ToolCalls  []toolCall        `json:"tool_calls,omitempty"`
		ToolCallID string            `json:"tool_call_id,omitempty"`
		Name       string            `json:"name,omitempty"`
	}{
		Role:       m.Role,
		Content:    m.Parts,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	})
}

// toolFunction/toolDef describe one callable tool in OpenAI's
// function-calling schema shape (see mssql.go's mssqlToolDef for the only
// tool currently registered).
type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type toolDef struct {
	Type     string       `json:"type"` // always "function"
	Function toolFunction `json:"function"`
}

// toolCall is one function call the model requested, as returned in an
// assistant message's tool_calls.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded object, per the OpenAI schema
	} `json:"function"`
}

// toolExecutor runs one tool call's JSON-encoded arguments and returns the
// text to feed back to the model as that call's result.
type toolExecutor func(ctx context.Context, argsJSON string) (string, error)

const (
	// toolCallsPerRoundLimit bounds the fan-out a single model response can
	// trigger. Tool calls are server-side actions even when they are read-only;
	// accepting an arbitrary-sized array from an upstream model would otherwise
	// turn one answer into an avoidable burst of database/API work.
	toolCallsPerRoundLimit = 8
	// toolArgumentsMaxBytes caps the JSON argument object before it reaches an
	// executor, audit/debug traces or the per-answer result cache. Individual
	// tools impose narrower semantic limits where useful; this is the common
	// circuit breaker for every provider and every tool surface.
	toolArgumentsMaxBytes = 64 << 10
	// toolCallsParallelism keeps independent lookups concurrent for latency,
	// while avoiding a single answer saturating database/API connection pools.
	toolCallsParallelism = 4
	// llmJSONResponseMaxBytes keeps a faulty or malicious model endpoint from
	// making a decision-round response consume unbounded process memory.
	llmJSONResponseMaxBytes = 8 << 20
)

// validateToolCalls applies transport-independent limits to a model tool-call
// response. It is deliberately called immediately after every provider's
// response is decoded, rather than only by runToolCalls: this avoids allocating
// a large result slice or spawning goroutines for an invalid upstream answer.
func validateToolCalls(msg chatMsg) (chatMsg, error) {
	if len(msg.ToolCalls) > toolCallsPerRoundLimit {
		return chatMsg{}, fmt.Errorf("model requested %d tool calls in one round (limit %d)", len(msg.ToolCalls), toolCallsPerRoundLimit)
	}
	for _, call := range msg.ToolCalls {
		if strings.TrimSpace(call.Function.Name) == "" {
			return chatMsg{}, fmt.Errorf("model requested a tool call without a name")
		}
		if len(call.Function.Arguments) > toolArgumentsMaxBytes {
			return chatMsg{}, fmt.Errorf("tool arguments for %q exceed %d bytes", call.Function.Name, toolArgumentsMaxBytes)
		}
	}
	return msg, nil
}

// readLimitedResponse reads at most maxBytes plus one sentinel byte so callers
// can reject an overlong upstream response instead of silently parsing a
// partial JSON document or allocating without a bound.
func readLimitedResponse(r io.Reader, maxBytes int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBytes)
	}
	return raw, nil
}

// newLMClientFull builds a client from already-resolved fields (no env-var
// lookup, no Azure default api-version) — the shared constructor behind
// newLMClientFromProfile, split out so tests can build a client directly.
func newLMClientFull(provider, base, apiVersion, embedModel, chatModel, apiKey string) *lmClient {
	return &lmClient{
		provider:   strings.ToLower(strings.TrimSpace(provider)),
		base:       normalizeBaseURL(base),
		apiVersion: apiVersion,
		embedModel: embedModel,
		chatModel:  chatModel,
		apiKey:     apiKey,
		// tracingTransport (conntrace.go) is a pass-through unless the
		// request context opted in via withConnTrace — only the Settings
		// connection test does.
		http: &http.Client{Timeout: 120 * time.Second, Transport: tracingTransport{}},
	}
}

// newLMClientFromProfile builds a client from a named settings profile,
// resolving the API key from an environment variable when APIKeyEnv is set
// and no inline key was configured — the same "prefer env var" convention
// llmflow6/promptcron use so secrets don't have to live in settings.json.
func newLMClientFromProfile(p llmProfile) *lmClient {
	key := p.APIKey
	if key == "" && p.APIKeyEnv != "" {
		key = os.Getenv(p.APIKeyEnv)
	}
	apiVersion := p.APIVersion
	if isAzureProvider(p.Provider) && apiVersion == "" {
		apiVersion = "2024-10-21"
	}
	return newLMClientFull(p.Provider, p.BaseURL, apiVersion, p.EmbedModel, p.ChatModel, key)
}

// isAzureProvider reports whether a settings profile's raw provider string
// names Azure, independent of any lmClient instance — used both by
// newLMClientFromProfile (to pick the api-version default) and as the
// standalone form of isAzure.
func isAzureProvider(provider string) bool {
	return strings.ToLower(strings.TrimSpace(provider)) == "azure"
}

// isSupportedChatProfile is the single registry for selectable chat
// backends. Embeddings intentionally have a separate, local-only route (see
// appSettings.activeEmbedModel/buildLLMClients), while all entries here may
// answer chat questions.
func isSupportedChatProfile(profile string) bool {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "local", "azure", "openai", "openrouter", "claude", "gemini":
		return true
	default:
		return false
	}
}

// isAzure reports whether c targets Azure OpenAI rather than a local/
// OpenAI-compatible endpoint — the single branch point most of this file's
// URL-building and auth logic keys off.
func (c *lmClient) isAzure() bool {
	return isAzureProvider(c.provider)
}

// chatCompletionsURL builds the request URL for chat completions, following
// Azure's deployment-scoped shape when this client targets Azure:
//
//	local/OpenAI: {base}/v1/chat/completions
//	Azure:        {base}/openai/deployments/{deployment}/chat/completions?api-version={version}
func (c *lmClient) chatCompletionsURL() string {
	if !c.isAzure() {
		return c.base + "/v1/chat/completions"
	}
	return c.azureDeploymentURL(c.chatModel, "chat/completions")
}

// embeddingsURL mirrors chatCompletionsURL for the embeddings endpoint.
func (c *lmClient) embeddingsURL() string {
	if !c.isAzure() {
		return c.base + "/v1/embeddings"
	}
	return c.azureDeploymentURL(c.embedModel, "embeddings")
}

// azureDeploymentURL builds the deployment-scoped URL Azure OpenAI expects
// for a given action ("chat/completions", "embeddings"), injecting the
// configured api-version (or the 2024-10-21 fallback) if the deployment name
// didn't already carry query parameters of its own.
func (c *lmClient) azureDeploymentURL(deployment, action string) string {
	base := strings.TrimRight(c.base, "/")
	endpoint := fmt.Sprintf("%s/openai/deployments/%s/%s", base, url.PathEscape(deployment), action)
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	q := u.Query()
	if q.Get("api-version") == "" {
		v := c.apiVersion
		if v == "" {
			v = "2024-10-21"
		}
		q.Set("api-version", v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// setAuthHeader applies the provider-appropriate credential: Azure uses
// "api-key", OpenAI-compatible services use Bearer, Claude uses x-api-key
// plus its version header, and Gemini uses x-goog-api-key.
func (c *lmClient) setAuthHeader(req *http.Request) {
	if c.isAzure() {
		if c.apiKey == "" {
			return
		}
		req.Header.Set("api-key", c.apiKey)
		return
	}
	if c.provider == "claude" {
		req.Header.Set("anthropic-version", "2023-06-01")
		if c.apiKey != "" {
			req.Header.Set("x-api-key", c.apiKey)
		}
		return
	}
	if c.provider == "gemini" {
		if c.apiKey != "" {
			req.Header.Set("x-goog-api-key", c.apiKey)
		}
		return
	}
	if c.apiKey == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}

// ping does a cheap reachability check against the configured backend at
// startup so a misconfigured URL surfaces as a log warning instead of a
// silent first-request failure.
func (c *lmClient) ping() error {
	if c.isAzure() {
		// Azure has no unauthenticated /v1/models-style probe scoped the same
		// way across resources; a deployment-scoped request is the closest
		// equivalent, so skip a dedicated health check and let the first
		// real embed/chat call surface connectivity problems.
		return nil
	}
	req, err := http.NewRequest("GET", c.base+"/v1/models", nil)
	if err != nil {
		return err
	}
	c.setAuthHeader(req)
	req.Header.Set("User-Agent", connectorUserAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("LLM endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// listModels returns every model id the local/OpenAI-compatible backend at
// c.base currently reports via the standard GET /v1/models listing (LM
// Studio/Ollama/vLLM all implement this) — lets the Settings UI offer a
// picker instead of an admin having to type an exact model id blind. Azure
// has no equivalent unauthenticated/tenant-scoped listing endpoint (see
// ping's doc comment above), so this is local-only; callers should not
// call it for an Azure-provider client.
func (c *lmClient) listModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	c.setAuthHeader(req)
	req.Header.Set("User-Agent", connectorUserAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readLimitedResponse(resp.Body, llmJSONResponseMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("read models response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models endpoint returned %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// llmMaxRetries bounds how many extra attempts embed/chatOnce make after a
// transient failure (a network-level error, or a 429/5xx response) before
// giving up — mirrors graph.go's graphMaxRetries/graphBackoff so every
// outbound HTTP integration in R3 backs off the same way, rather than
// each one either hammering a momentarily struggling server or
// reimplementing its own backoff schedule. Deliberately smaller than
// graph.go's tolerance: a user is waiting on a chat answer or an import
// step here, not a large unattended background batch.
const llmMaxRetries = 2

// llmPostJSONRetry POSTs body to url with c's auth header, retrying on a
// transient failure per llmMaxRetries — shared by embed and chatOnce so
// both back off the same way instead of duplicating this loop. Not used
// by chatStreamMessages: once tokens have started streaming to the
// caller's io.Writer, retrying the request would duplicate/corrupt
// output, so a streaming failure is always final.
func (c *lmClient) llmPostJSONRetry(ctx context.Context, url string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= llmMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", connectorUserAgent)
		c.setAuthHeader(req)
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if attempt < llmMaxRetries {
				time.Sleep(graphBackoff(attempt, 0))
				continue
			}
			return nil, lastErr
		}
		raw, readErr := readLimitedResponse(resp.Body, llmJSONResponseMaxBytes)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read response: %w", readErr)
		}
		if resp.StatusCode == http.StatusOK {
			return raw, nil
		}
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < llmMaxRetries {
			time.Sleep(graphBackoff(attempt, parseRetryAfter(resp.Header)))
			continue
		}
		return nil, lastErr
	}
	return nil, lastErr
}

type embReq struct {
	Model string   `json:"model,omitempty"`
	Input []string `json:"input"`
}

type embResp struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// embed returns one vector per input text.
func (c *lmClient) embed(texts []string) ([][]float64, error) {
	return c.embedCtx(context.Background(), texts)
}

// embedCtx is embed with a caller-supplied context — used by the
// Settings connection test so its timeout/trace context reaches the
// actual HTTP request; every other caller keeps the plain embed wrapper.
func (c *lmClient) embedCtx(ctx context.Context, texts []string) ([][]float64, error) {
	if verbose {
		log.Printf("[verbose] embed provider=%s model=%s base=%s texts=%d", c.provider, c.embedModel, c.base, len(texts))
	}
	er := embReq{Input: texts}
	if !c.isAzure() {
		er.Model = c.embedModel // Azure implies the model via the deployment in the URL
	}
	body, err := json.Marshal(er)
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}
	raw, err := c.llmPostJSONRetry(ctx, c.embeddingsURL(), body)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	var out embResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	vecs := make([][]float64, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	// Not reached by retrieval-query embeddings today: rankedSearch (rank.go)
	// calls embedSingle, which uses context.Background() rather than
	// threading a request-scoped context — see tokenusage.go's package
	// comment for why that's a deliberate v1 scope line, not an oversight.
	// Reached by any ctx-aware caller (e.g. conntest.go's connection test).
	tokenUsageFromContext(ctx).add(tokenUsageEvent{Provider: c.provider, Model: c.embedModel, PromptTokens: out.Usage.PromptTokens})
	return vecs, nil
}

// embedSingle is the common case of embed with exactly one input text — used
// everywhere a single query or chunk needs a vector rather than a batch.
func (c *lmClient) embedSingle(text string) ([]float64, error) {
	return c.embedSingleCtx(context.Background(), text)
}

// embedSingleCtx: see embedCtx's doc comment.
func (c *lmClient) embedSingleCtx(ctx context.Context, text string) ([]float64, error) {
	vecs, err := c.embedCtx(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return vecs[0], nil
}

type chatReq struct {
	Model    string    `json:"model,omitempty"`
	Messages []chatMsg `json:"messages"`
	Stream   bool      `json:"stream"`
	Tools    []toolDef `json:"tools,omitempty"`
	// StreamOptions requests a trailing usage-bearing SSE chunk — OpenAI's
	// own convention, honored by real OpenAI and most OpenAI-compatible
	// local servers too, though not guaranteed for every one. Only set on
	// a streaming request (chatStreamMessages); omitempty keeps a
	// non-streaming chatOnce request's JSON byte-for-byte unchanged from
	// before this field existed. See chatStreamMessages' scan loop for the
	// character-count fallback when no backend sends this chunk.
	StreamOptions *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatStream streams the assistant's reply for the given system prompt and
// conversation history, writing tokens to w as they arrive.
func (c *lmClient) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	all := make([]chatMsg, 0, len(msgs)+1)
	all = append(all, chatMsg{Role: "system", Content: system})
	all = append(all, msgs...)
	return c.chatStreamMessages(ctx, all, w)
}

// chatStreamMessages is chatStream's implementation, taking the full
// message list (system message already included) so chatWithTools can
// stream a final answer after appending tool-call/tool-result messages
// without re-deriving them from a separate "system" parameter.
func (c *lmClient) chatStreamMessages(ctx context.Context, all []chatMsg, w io.Writer) error {
	if c.provider == "claude" || c.provider == "gemini" {
		msg, err := c.nativeChatOnce(ctx, all, nil)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, msg.Content)
		return err
	}
	if verbose {
		log.Printf("[verbose] chat provider=%s model=%s base=%s messages=%d", c.provider, c.chatModel, c.base, len(all))
	}
	cr := chatReq{Messages: all, Stream: true, StreamOptions: &chatStreamOptions{IncludeUsage: true}}
	if !c.isAzure() {
		cr.Model = c.chatModel
	}
	body, err := json.Marshal(cr)
	if err != nil {
		return fmt.Errorf("marshal chat request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.chatCompletionsURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", connectorUserAgent)
	c.setAuthHeader(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("chat request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chat HTTP %d: %s", resp.StatusCode, string(raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var completion strings.Builder
	usageSeen := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			// Usage: only present on the OpenAI "stream_options.include_usage"
			// trailing chunk this request asked for above — nil on every
			// ordinary token-delta chunk. Not every OpenAI-compatible server
			// honors the request; usageSeen tracks whether one ever arrived.
			// upstreamChatUsage carries the prompt_tokens_details.cached_tokens
			// split too, so a streamed final answer records Azure's automatic
			// prompt-cache read the same disjoint way the non-streaming
			// decision rounds (chatOnce) do.
			Usage *upstreamChatUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			if tok := chunk.Choices[0].Delta.Content; tok != "" {
				completion.WriteString(tok)
				if _, err := io.WriteString(w, tok); err != nil {
					return err
				}
			}
		}
		if chunk.Usage != nil {
			usageSeen = true
			tokenUsageFromContext(ctx).add(chunk.Usage.tokenEvent(c.provider, c.chatModel))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !usageSeen {
		// This backend never sent a usage chunk (stream_options.include_usage
		// isn't universally supported) — fall back to the same character-
		// count heuristic openai_api.go already uses for its own outgoing
		// responses, explicitly flagged as an estimate rather than silently
		// presenting a guess as an exact count.
		var promptText strings.Builder
		for _, m := range all {
			promptText.WriteString(m.Content)
		}
		tokenUsageFromContext(ctx).add(tokenUsageEvent{
			Provider:         c.provider,
			Model:            c.chatModel,
			PromptTokens:     estimateOpenAITokens(promptText.String()),
			CompletionTokens: estimateOpenAITokens(completion.String()),
			Estimated:        true,
		})
	}
	return nil
}

// upstreamChatUsage is the usage shape both the non-streaming (chatOnce) and
// streaming (chatStreamMessages) OpenAI-compatible paths parse. Beyond the
// plain prompt/completion counts it captures prompt_tokens_details.cached_tokens
// — the portion of the prompt Azure OpenAI (and OpenAI) served from their
// AUTOMATIC, server-side prompt cache. That caching is exactly what makes the
// Agent tab's multi-round tool loop cheap on these backends (there is no
// client-side cache_control to set, unlike Claude — see chatWithToolsBudget's
// package-level comment): every round re-sends the same growing-but-unmutated
// prefix, so rounds 2..N are largely cache hits. Local servers (LM Studio,
// Ollama) generally omit prompt_tokens_details, so CachedTokens stays 0 there
// — no cache claimed, nothing recorded.
type upstreamChatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// tokenEvent normalizes an OpenAI-compatible usage report into a
// tokenUsageEvent, recording the cached input tokens DISJOINT from
// PromptTokens — deliberately matching the Claude path (llm_claude.go), where
// usage.input_tokens already excludes cache_read_input_tokens. Azure/OpenAI
// instead report prompt_tokens as the TOTAL (cached + fresh), so the cached
// portion is subtracted out here. That keeps "prompt_tokens" meaning the same
// full-price-input quantity for every provider in the usage analytics
// (tokenusage.go's SUMs stay apples-to-apples), while finally surfacing the
// automatic prompt-cache savings that were previously invisible (counted as
// full-price prompt tokens). Defensively clamps a nonsensical cached count
// (negative, or larger than the prompt) so a quirky backend can never make
// PromptTokens go negative.
func (u upstreamChatUsage) tokenEvent(provider, model string) tokenUsageEvent {
	cached := u.PromptTokensDetails.CachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > u.PromptTokens {
		cached = u.PromptTokens
	}
	return tokenUsageEvent{
		Provider:             provider,
		Model:                model,
		PromptTokens:         u.PromptTokens - cached,
		CompletionTokens:     u.CompletionTokens,
		CacheReadInputTokens: cached,
	}
}

// chatOnce performs a single non-streaming chat completion, returning the
// assistant's message verbatim (including any tool_calls) — used by
// chatWithTools for the "does the model want to call a tool" round, where
// streaming a possibly-empty/partial answer would be worse UX than just
// waiting for the (typically fast) decision.
func (c *lmClient) chatOnce(ctx context.Context, all []chatMsg, tools []toolDef) (chatMsg, error) {
	if c.provider == "claude" || c.provider == "gemini" {
		msg, err := c.nativeChatOnce(ctx, all, tools)
		if err != nil {
			return chatMsg{}, err
		}
		return validateToolCalls(msg)
	}
	if verbose {
		log.Printf("[verbose] chat(once) provider=%s model=%s base=%s messages=%d tools=%d", c.provider, c.chatModel, c.base, len(all), len(tools))
	}
	cr := chatReq{Messages: all, Stream: false, Tools: tools}
	if !c.isAzure() {
		cr.Model = c.chatModel
	}
	body, err := json.Marshal(cr)
	if err != nil {
		return chatMsg{}, fmt.Errorf("marshal chat request: %w", err)
	}
	raw, err := c.llmPostJSONRetry(ctx, c.chatCompletionsURL(), body)
	if err != nil {
		return chatMsg{}, fmt.Errorf("chat request failed: %w", err)
	}
	var out struct {
		Choices []struct {
			Message chatMsg `json:"message"`
		} `json:"choices"`
		Usage upstreamChatUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return chatMsg{}, fmt.Errorf("parse chat response: %w", err)
	}
	if len(out.Choices) == 0 {
		return chatMsg{}, fmt.Errorf("chat response had no choices")
	}
	tokenUsageFromContext(ctx).add(out.Usage.tokenEvent(c.provider, c.chatModel))
	return validateToolCalls(out.Choices[0].Message)
}

// chatWithTools runs at most one tool-calling round-trip before streaming
// the final answer — the original single-round behavior, kept as the
// plain Chat tab's semantics. See chatWithToolsBudget for the multi-round
// loop the Agent tab uses.
func (c *lmClient) chatWithTools(ctx context.Context, system string, msgs []chatMsg, tools []toolDef, executors map[string]toolExecutor, w io.Writer) error {
	return c.chatWithToolsBudget(ctx, system, msgs, tools, executors, w, 1)
}

// clarifyToolName is the fixed tool name chatWithToolsBudget recognizes to
// short-circuit its loop into a clarifying question instead of a normal
// answer — see agent.go's clarifyToolDef for the actual tool schema/
// description offered to the model. This file only needs the name, not
// agent.go's types, so the generic chat client stays independent of the
// Agent tab's specific tool set.
const clarifyToolName = "ask_clarifying_question"

// ErrClarificationNeeded is returned by chatWithToolsBudget instead of
// writing an answer to w when the model calls the clarifyToolName tool —
// signals "stop and ask the user first," not a real failure. Callers
// (handlers.go's handleAsk) check errors.As for this specific type and
// render Question/Options as an interactive affordance (buttons) instead
// of an error message.
type ErrClarificationNeeded struct {
	Question string
	Options  []string
}

func (e *ErrClarificationNeeded) Error() string {
	return fmt.Sprintf("clarification needed: %s", e.Question)
}

// clarificationFromCalls scans one round's tool calls for a
// clarifyToolName invocation and, if found, parses its arguments into an
// *ErrClarificationNeeded — nil if this round contains no such call (the
// overwhelmingly common case). If the model calls it alongside other
// tools in the same round, the clarification wins and those other calls
// are simply never executed: asking the user takes priority over
// continuing to dig with tools whose need may evaporate once the
// ambiguity is resolved. A malformed or empty-question call is ignored
// (falls through to normal tool execution for the round) rather than
// silently ending the conversation on a parse error.
func clarificationFromCalls(calls []toolCall) *ErrClarificationNeeded {
	for _, tc := range calls {
		if tc.Function.Name != clarifyToolName {
			continue
		}
		var args struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			continue
		}
		if strings.TrimSpace(args.Question) == "" {
			continue
		}
		return &ErrClarificationNeeded{Question: args.Question, Options: args.Options}
	}
	return nil
}

// ---- Debug trace (Chat/Agent/Mail "Debug-Modus") ---------------------------

// debugToolCall is one recorded tool invocation for the debug trace below —
// see debugTrace's doc comment.
type debugToolCall struct {
	Round      int    `json:"round"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	// Agent attributes this call to the sub-agent that made it ("" = the
	// top-level orchestrator) — mirrors agentStep.Agent/agentToolRun.Agent
	// below, previously the one place sub-agent attribution was missing:
	// the Debug panel's tool-call list used to read as one flat,
	// unattributed sequence even when delegate_subtasks/web_research were
	// involved, unlike the live step timeline and the audit log, which
	// both already carried this.
	Agent string `json:"agent,omitempty"`
}

// debugTrace collects everything the Debug-Modus UI (Chat/Agent/Mail tabs,
// gated by handlers.go's debugModeAllowed) needs to show exactly what one
// request did: the retrieved RAG context (set directly by the retrieval
// call site — handleAsk/composeDraftReply/composeNewMail — since they
// already have the hits in hand), the final message sequence actually
// sent to the model (including every tool round-trip), and every tool
// call made along the way.
//
// Threaded through via context rather than a new parameter on
// chatWithToolsBudget/runToolCalls/composeDraftReply/composeNewMail, so
// every existing call site — and every existing test — is unaffected
// unless it explicitly opts in with withDebugTrace. Every method here is
// nil-receiver-safe, so callers never need a nil check of their own;
// "no trace in this context" and "trace exists but nothing recorded yet"
// both just work.
type debugTrace struct {
	mu              sync.Mutex
	RetrievedChunks []rankedHit     `json:"retrieved_chunks,omitempty"`
	Messages        []chatMsg       `json:"messages,omitempty"`
	ToolCalls       []debugToolCall `json:"tool_calls,omitempty"`
	RawAnswer       string          `json:"raw_answer,omitempty"`
	// Profile/Preset/PresetKinds/PresetTools/DeptCode describe the request
	// context that produced RetrievedChunks/Messages above — which chat
	// backend answered, which admin-curated preset gated tool/source-kind
	// access, and which department code sourceAccessAllowed/
	// filterByDeptAccess actually used (adminDeptCode for an admin
	// session — see settings.go's resolveDeptCode). Set once by the
	// handler right after these are resolved, before any model call.
	Profile     string    `json:"profile,omitempty"`
	Preset      string    `json:"preset,omitempty"`
	PresetKinds []string  `json:"preset_kinds,omitempty"`
	PresetTools []string  `json:"preset_tools,omitempty"`
	DeptCode    string    `json:"dept_code,omitempty"`
	StartedAt   time.Time `json:"-"`
	TotalMS     int64     `json:"total_ms,omitempty"`
	// RewrittenQuery is set only when query_rewrite.go's
	// rewriteQueryForRetrieval actually changed the retrieval query (see
	// queryRewriteConfig, settings.go) — omitted entirely when the
	// question was used as-is, so a debug trace with this field present
	// unambiguously means the rewrite fired.
	RewrittenQuery string `json:"rewritten_query,omitempty"`
	// SelectedSkills is buildSystemPromptForMode's own return value (the
	// display names of every skill_*.md whose tags matched this question,
	// see skills.go's selectSkills) — previously computed and then only
	// ever written to the server's verbose log, invisible anywhere in the
	// UI even in Debug-Modus. Empty/omitted means no skill matched (or
	// none are configured), same as an empty slice from selectSkills.
	SelectedSkills []string `json:"selected_skills,omitempty"`
}

// finish stamps TotalMS from StartedAt — call once, right before the
// trace is attached to a JSON response. A zero StartedAt (trace created
// without setStarted having been called) leaves TotalMS at 0 rather than
// producing a nonsense huge duration.
func (dt *debugTrace) finish() {
	if dt == nil || dt.StartedAt.IsZero() {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.TotalMS = time.Since(dt.StartedAt).Milliseconds()
}

func (dt *debugTrace) setMessages(msgs []chatMsg) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.Messages = append([]chatMsg{}, msgs...)
}

func (dt *debugTrace) addToolCall(c debugToolCall) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.ToolCalls = append(dt.ToolCalls, c)
}

type debugTraceContextKey struct{}

// withDebugTrace returns ctx augmented with a fresh *debugTrace, plus that
// same trace for the caller to read back once the request completes — see
// handlers.go's handleAsk/handleDraftReply for the only call sites, both
// gated by debugModeAllowed so this never runs for anyone else.
func withDebugTrace(ctx context.Context) (context.Context, *debugTrace) {
	dt := &debugTrace{StartedAt: time.Now()}
	return context.WithValue(ctx, debugTraceContextKey{}, dt), dt
}

// debugTraceFromContext returns nil if ctx carries none — every call site
// below treats that as "debug mode is off for this request", not an error.
func debugTraceFromContext(ctx context.Context) *debugTrace {
	dt, _ := ctx.Value(debugTraceContextKey{}).(*debugTrace)
	return dt
}

// ─────────────────────────────────────────────────────────────────────────────
// Live agent progress — a context-carried event stream (same threading
// pattern as debugTrace above, but pushed live rather than collected and
// returned at the end) so the UI can show the agent working step by step:
// each tool call as it starts/finishes, and each orchestrated sub-agent as
// it spawns/reports. handleAsk's streaming branch wires an emitter that
// turns each step into an NDJSON "step" line; every other caller leaves it
// nil, in which case every method here is a no-op (nil-receiver-safe, like
// debugTrace).
// ─────────────────────────────────────────────────────────────────────────────

type agentStep struct {
	// ID uniquely identifies this tool/sub-agent CALL within one process's
	// lifetime — a "start" and its matching "end" share the same ID (the
	// caller passes the start's returned ID back in when sending the end),
	// so the frontend can match them directly instead of the fragile
	// "phase|agent|tool" composite key it previously had to fall back on
	// (which could collide on two concurrent identical calls, or an
	// out-of-order end with no matching start). Also what a graphical
	// agent/sub-agent/tool-call hierarchy view keys nodes on. Not a UUID —
	// nothing needs cross-process/cross-restart uniqueness, this only ever
	// lives inside one live NDJSON stream.
	ID string `json:"id"`
	// ParentID is the ID of the step this one is nested under: the
	// enclosing subagent_start/tool_start's ID for a step emitted from
	// inside a delegated sub-agent or a nested tool loop (web_research's
	// own fetch_page_with_links calls), "" for a step at the top level.
	// This is what turns the previously flat, Agent-label-only step stream
	// into an actual reconstructable tree.
	ParentID string `json:"parent_id,omitempty"`
	// Type: "tool_start" | "tool_end" | "subagent_start" | "subagent_end".
	Type       string `json:"type"`
	Round      int    `json:"round,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Args       string `json:"args,omitempty"`   // truncated
	Result     string `json:"result,omitempty"` // truncated
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// Agent labels which agent emitted this step: "" for the main
	// orchestrator, a subtask label for a delegated sub-agent — so the UI
	// can indent/group a sub-agent's steps under its parent.
	Agent string `json:"agent,omitempty"`
	// Phase distinguishes the pre-flight tool-router's own tool use
	// (tool_router.go's runToolRouter, "router") from the main call's own
	// native tool-calling ("" — unset, backward compatible with every step
	// emitted before this field existed). Purely a UI hint; both phases use
	// the exact same tool_start/tool_end mechanics.
	Phase string `json:"phase,omitempty"`
	// StartedAt is when THIS event was emitted (unix milliseconds) — lets a
	// timeline/Gantt-style view order and place concurrent steps (several
	// sub-agents' tool calls interleaving) correctly from the record alone,
	// not just from NDJSON arrival order.
	StartedAt int64 `json:"started_at_ms,omitempty"`
}

// agentStepSeq is a process-wide counter for nextAgentStepID — guarded by
// atomic ops rather than agentProgress's own per-instance mu, since sibling
// sub-agents each get their OWN agentProgress instance (see
// withSubAgentLabel) and would otherwise race on a shared plain counter.
var agentStepSeq int64

func nextAgentStepID() string {
	return strconv.FormatInt(atomic.AddInt64(&agentStepSeq, 1), 10)
}

type agentProgress struct {
	mu    sync.Mutex
	emit  func(agentStep)
	label string // non-empty inside a delegated sub-agent
	// parentID is stamped onto every step sent through this instance that
	// doesn't already carry one — the ID of the subagent_start/tool_start
	// step this progress instance's own steps nest under ("" = top level).
	// Set by withSubAgentLabel when a sub-agent scope is created.
	parentID string
}

// send delivers one step to the emitter under lock (tool calls run
// concurrently, so several goroutines may emit at once), stamping the
// current agent label plus a fresh ID/ParentID/timestamp unless the caller
// already set them (a matching "end" event reuses its "start" event's ID
// explicitly, so the two sides of one call share one ID — see
// runToolCalls/subAgentToolExecutor). Returns the step's final ID so a
// caller that needs to parent further nested steps under this one (a
// sub-agent's own tool calls, e.g.) can capture it. Nil-safe.
func (p *agentProgress) send(s agentStep) string {
	if p == nil || p.emit == nil {
		return s.ID
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.Agent == "" {
		s.Agent = p.label
	}
	if s.ID == "" {
		s.ID = nextAgentStepID()
	}
	if s.ParentID == "" {
		s.ParentID = p.parentID
	}
	if s.StartedAt == 0 {
		s.StartedAt = time.Now().UnixMilli()
	}
	p.emit(s)
	return s.ID
}

type agentProgressContextKey struct{}

// withAgentProgress returns ctx carrying a fresh progress emitter plus the
// emitter itself. emit is called once per step; it must be safe to call
// from multiple goroutines only in the sense that send() serializes them.
func withAgentProgress(ctx context.Context, emit func(agentStep)) context.Context {
	return context.WithValue(ctx, agentProgressContextKey{}, &agentProgress{emit: emit})
}

func agentProgressFromContext(ctx context.Context) *agentProgress {
	p, _ := ctx.Value(agentProgressContextKey{}).(*agentProgress)
	return p
}

// subAgentLabelFromContext returns the current sub-agent's label, or "" at
// the top level — used to attribute audited tool runs to the sub-agent
// that made them (agent.go's auditExecutor).
func subAgentLabelFromContext(ctx context.Context) string {
	if p := agentProgressFromContext(ctx); p != nil {
		return p.label
	}
	return ""
}

// withSubAgentLabel derives a context whose progress emitter tags every
// step with label (the delegated sub-agent's name) and parentID (its
// steps' ParentID, tying them into the enclosing subagent_start/tool_start
// node), forwarding to the same underlying emit func as the parent — so a
// sub-agent's tool calls stream live under their sub-agent heading, nested
// under the right node. Returns ctx unchanged if the parent carried no
// progress emitter.
func withSubAgentLabel(ctx context.Context, label, parentID string) context.Context {
	parent := agentProgressFromContext(ctx)
	if parent == nil {
		return ctx
	}
	child := &agentProgress{emit: parent.emit, label: label, parentID: parentID}
	return context.WithValue(ctx, agentProgressContextKey{}, child)
}

// currentStepIDContextKey/withCurrentStepID/currentStepIDFromContext thread
// "the ID of the tool_start step runToolCalls just emitted for the call
// currently in flight" alongside ctx — so a tool executor that itself
// spins up a nested sub-agent scope (webResearchToolExecutor) can parent
// that scope's steps under its OWN tool call instead of as a sibling of it.
// subAgentToolExecutor doesn't need this: delegate_subtasks emits its own
// subagent_start/end pair directly and already has that ID in hand.
type currentStepIDContextKey struct{}

func withCurrentStepID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, currentStepIDContextKey{}, id)
}

func currentStepIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(currentStepIDContextKey{}).(string)
	return id
}

// ─────────────────────────────────────────────────────────────────────────────
// Long-agent-run context compaction (agentConfig.ContextCompaction*,
// settings.go) — a context-carried, nil-safe-default config (same pattern
// as debugTrace/agentProgress above) so only handleAsk (the one caller
// with admin-configurable agentConfig in scope) needs to inject an
// override; every other caller (draft.go's mail drafts, agent.go's
// sub-agents/web-research) gets the built-in defaults below automatically.
//
// The problem this solves: chatWithToolsBudgetDeadline's `all` slice only
// ever grows (see its own doc comment on why — prompt-cache prefix
// stability). Across many rounds, especially with delegate_subtasks
// fanning out several sub-agents whose results all land back in the
// orchestrator's own loop, the accumulated tool-result text can get large
// even though each individual result is already capped (agent.go's
// agentSearchResultChars etc.) — the growth is cumulative across rounds,
// not any one result being oversized. compactOldToolRounds bounds that:
// once the accumulated size crosses a threshold, every "tool"-role result
// message belonging to a round OLDER than the most recent keepRounds is
// replaced with a short deterministic placeholder — no second LLM call,
// no summarization, purely a size cap. Assistant messages (which carry
// the tool_calls the model itself issued) are left untouched, so the
// model still remembers what it already tried; only the raw result
// BODIES from rounds it has, in practice, already moved past get
// shortened.
//
// DELIBERATE TENSION WITH PROMPT CACHING (see that section further below):
// this is the ONE place in this file that mutates an already-sent
// message's Content instead of only ever appending — exactly the thing
// that section's own "what would defeat caching" list warns against.
// That's a real, accepted cost, not an oversight: once compaction fires,
// the round immediately after it pays full price — OpenAI-style
// automatic prefix caching and Claude's cache_control breakpoint both
// stop matching at the first rewritten message, so that one request is a
// cache miss (and, for Claude, a fresh — slightly pricier — cache
// *write*) instead of the usual cache *read* discount. Every round before
// the first compaction, and every round after `all` has gone back to
// pure appends until the next threshold crossing, still caches normally —
// this is a one-time cost per compaction event, not a permanent loss for
// the rest of the conversation. The three-field configurability below
// (disabled/thresholdChars/keepRounds) is how a deployment that leans
// heavily on prompt caching for cost trades this off: raise
// thresholdChars (fewer compaction events, each conversation more likely
// to finish before ever paying the one-time cost) or set disabled=true
// outright if long-enough runs to matter are rare in practice anyway.
// There is no way to compact AND preserve the cached prefix at the same
// time — the whole point is changing bytes the cache already matched on.
// ─────────────────────────────────────────────────────────────────────────────

type contextCompactionContextKey struct{}

// contextCompactionConfig is the resolved (defaults-applied) compaction
// policy for one chatWithToolsBudgetDeadline call.
type contextCompactionConfig struct {
	disabled       bool
	thresholdChars int
	keepRounds     int
}

const (
	// contextCompactionDefaultThresholdChars is generous on purpose: most
	// Chat/Agent answers finish in 1-3 rounds with a handful of already-
	// capped tool results and never come close to this — it's meant to
	// only engage for genuinely long multi-round, sub-agent-heavy runs.
	contextCompactionDefaultThresholdChars = 24000
	// contextCompactionDefaultKeepRounds: the most recent 2 rounds are
	// always left verbatim, regardless of size — compaction only ever
	// touches rounds older than that.
	contextCompactionDefaultKeepRounds = 2
)

// withContextCompaction derives a context carrying cfg as the compaction
// policy for chatWithToolsBudgetDeadline calls made with it — see
// handlers.go's handleAsk, the only caller that currently sets this.
func withContextCompaction(ctx context.Context, cfg contextCompactionConfig) context.Context {
	return context.WithValue(ctx, contextCompactionContextKey{}, cfg)
}

// contextCompactionFromContext returns ctx's carried policy with defaults
// filled in for any zero field, or the all-defaults policy if ctx carries
// none at all — every caller that never calls withContextCompaction (mail
// drafts, sub-agents, web-research, the OpenAI-compatible API) still gets
// sane, always-on-by-default compaction this way, not a foot-gun opt-in.
func contextCompactionFromContext(ctx context.Context) contextCompactionConfig {
	cfg, _ := ctx.Value(contextCompactionContextKey{}).(contextCompactionConfig)
	if cfg.thresholdChars <= 0 {
		cfg.thresholdChars = contextCompactionDefaultThresholdChars
	}
	if cfg.keepRounds <= 0 {
		cfg.keepRounds = contextCompactionDefaultKeepRounds
	}
	return cfg
}

// compactedToolResultPlaceholder is what a compacted tool-result message's
// content becomes — short and generic (not a fabricated summary) so the
// model is never tempted to treat it as real data, but explicit that
// content existed and was already available to it in earlier reasoning.
func compactedToolResultPlaceholder(originalChars int) string {
	return fmt.Sprintf("[gekürzt: %d Zeichen Werkzeug-Ergebnis aus einer früheren Runde — bereits in vorherigen Schritten verarbeitet, siehe neuere Schritte für aktuelle Details]", originalChars)
}

// compactOldToolRounds bounds a long, multi-round agent loop's context
// growth: once the accumulated message content since the last compaction
// pass crosses thresholdChars, every "tool"-role result message belonging
// to a round older than the most recent keepRounds is replaced with
// compactedToolResultPlaceholder. roundStarts holds, per completed round,
// the index in all where that round's messages begin. Returns the
// (possibly mutated in place) all slice and the new compactedUpTo
// watermark — callers must pass that back in on the next call so an
// already-compacted range is never re-scanned or re-touched. A no-op
// (returns all, compactedUpTo unchanged) when there aren't yet more than
// keepRounds completed rounds, or the accumulated size since
// compactedUpTo hasn't crossed thresholdChars.
func compactOldToolRounds(all []chatMsg, roundStarts []int, compactedUpTo, keepRounds, thresholdChars int) ([]chatMsg, int) {
	if len(roundStarts) <= keepRounds {
		return all, compactedUpTo
	}
	total := 0
	for i := compactedUpTo; i < len(all); i++ {
		total += len(all[i].Content)
	}
	if total < thresholdChars {
		return all, compactedUpTo
	}
	cutoff := roundStarts[len(roundStarts)-keepRounds]
	for i := compactedUpTo; i < cutoff; i++ {
		if all[i].Role != "tool" {
			continue
		}
		if n := len(all[i].Content); n > 0 {
			all[i].Content = compactedToolResultPlaceholder(n)
		}
	}
	return all, cutoff
}

// chatWithToolsBudget runs up to maxRounds tool-calling round-trips before
// streaming the final answer to w. Each round the model may request one
// or more tool calls; their results are appended and the model asked
// again — this is what lets an agent search, read the result, and search
// again with better terms instead of being stuck with its first guess.
// When the model answers without requesting tools, that answer is
// written and the loop ends early. When the budget runs out while the
// model still wants tools, a final answer is forced by asking once more
// WITHOUT any tools offered — everything gathered so far is in the
// conversation, so the model can still ground its answer; it just can't
// keep digging forever (the budget is also the cost/latency ceiling).
//
// With no tools configured (or maxRounds <= 0) it behaves exactly like
// chatStream, at the cost of one extra non-streaming round-trip to find
// that out — invisible to the user, since nothing has been written to w
// yet at that point. Many small local models don't support tool calling
// at all; against those, the tools schema is simply ignored and the
// model answers normally, so this is safe to call unconditionally.
func (c *lmClient) chatWithToolsBudget(ctx context.Context, system string, msgs []chatMsg, tools []toolDef, executors map[string]toolExecutor, w io.Writer, maxRounds int) error {
	return c.chatWithToolsBudgetDeadline(ctx, system, msgs, tools, executors, w, maxRounds, time.Time{})
}

// ─────────────────────────────────────────────────────────────────────────────
// Prompt caching (OpenAI-compatible path) — deliberately no client-side code
// here, unlike llm_claude.go's explicit cache_control markers (two of them:
// a static one on the system+tools prefix and a rolling one on the growing
// tool-call transcript — see markLastMessageForCache). OpenAI/Azure
// OpenAI/OpenRouter/most local OpenAI-compatible servers cache a repeated
// request PREFIX automatically, server-side, once it exceeds a length
// threshold (roughly 1024 tokens) — there is no cache_control-style field in
// this wire format to opt in with; the server just recognizes the same bytes
// it saw before.
//
// That only pays off if the prefix this file sends is actually
// byte-identical across a multi-round answer, which is why this matters here
// specifically: chatWithToolsBudgetDeadline below builds `all` ONCE (system
// message + msgs) before the round loop starts, then only ever APPENDS to it
// (the assistant's tool-call message, then the tool results) — every
// chatOnce call in the loop re-sends that same growing-but-never-mutated
// prefix, so the system prompt + "Kontext:" retrieval block (handlers.go/
// openai_api.go — often several thousand tokens) stays identical round over
// round exactly like Claude's would. `tools` is likewise built once per
// request from deterministic sources (settings slices in iteration order,
// map[string]any parameter schemas — encoding/json sorts map keys, so their
// serialization is stable too) and never changes mid-loop. Nothing here
// reads a clock, a random ID, or a mutable global into the prefix.
//
// What WOULD defeat this (audit for these if caching seems to not be
// happening — some of these are already avoided in this codebase, listed so
// a future change doesn't reintroduce them):
//   - Interpolating a timestamp, request ID, or session ID into `system`
//     before the round loop (none of buildSystemPromptForMode's inputs do
//     this today).
//   - Rebuilding `tools`/`executors` mid-loop, or sourcing a tool list from a
//     map iterated directly into request order (Go map iteration order is
//     randomized every run) instead of a slice — appendShopTool/
//     buildAgentTools/the MSSQL loop above all append to a []toolDef in a
//     fixed, deterministic order.
//   - Switching c.chatModel or c.provider mid-answer (this file never does;
//     one lmClient serves one answer end to end).
//   - Any per-round mutation of `all`'s early elements rather than pure
//     appends (runToolCalls writes tool results back into the SAME slice
//     positions their originating tool_calls occupied, so replaying an
//     earlier round's bytes is never altered by later rounds). The ONE
//     deliberate, accepted exception is compactOldToolRounds (below,
//     "Long-agent-run context compaction") — it exists specifically
//     because it mutates early elements, at the known, documented cost of
//     one cache-miss round when it fires; see that section for the
//     tradeoff and how to configure around it.
// ─────────────────────────────────────────────────────────────────────────────

// chatWithToolsBudgetDeadline is chatWithToolsBudget with an optional
// wall-clock deadline: once passed, the round loop stops the same
// graceful way round-exhaustion already does below (one final answer, no
// tools offered) instead of the caller's ctx cancellation turning this
// into a hard error partway through a tool call. A zero-value deadline
// behaves exactly like chatWithToolsBudget — every existing caller uses
// that unchanged entry point, so this only matters for a caller (the
// web-research sub-agent, agent.go) that actually sets one.
func (c *lmClient) chatWithToolsBudgetDeadline(ctx context.Context, system string, msgs []chatMsg, tools []toolDef, executors map[string]toolExecutor, w io.Writer, maxRounds int, deadline time.Time) error {
	dt := debugTraceFromContext(ctx)
	if len(tools) == 0 || maxRounds <= 0 {
		full := make([]chatMsg, 0, len(msgs)+1)
		full = append(full, chatMsg{Role: "system", Content: system})
		full = append(full, msgs...)
		dt.setMessages(full)
		return c.chatStream(ctx, system, msgs, w)
	}

	all := make([]chatMsg, 0, len(msgs)+1)
	all = append(all, chatMsg{Role: "system", Content: system})
	all = append(all, msgs...)

	// resultCache remembers every (tool, arguments) pair already executed
	// across every round of *this* answer — not just within one round —
	// so a model that repeats an identical call (confusion, or re-checking
	// after a few more rounds) gets the same answer back immediately
	// instead of paying for (and waiting on) a redundant search/query/mail
	// draft. Safe by construction: nothing here is time-sensitive within
	// the few seconds a single answer takes to generate, and for a
	// write-ish tool (save_draft_to_mailbox) this is a feature, not a
	// limitation — it stops an identical repeated call from filing a
	// second, duplicate draft.
	resultCache := map[string]string{}

	// See compactOldToolRounds' doc comment: roundStarts/compactedUpTo
	// track, respectively, where each completed round begins in `all` and
	// how far compaction has already progressed, so long multi-round runs
	// don't accumulate unbounded tool-result text.
	ccCfg := contextCompactionFromContext(ctx)
	var roundStarts []int
	compactedUpTo := 0

	for round := 0; round < maxRounds; round++ {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		assistant, err := c.chatOnce(ctx, all, tools)
		if err != nil {
			return err
		}
		if len(assistant.ToolCalls) == 0 {
			dt.setMessages(append(append([]chatMsg{}, all...), assistant))
			_, err := io.WriteString(w, assistant.Content)
			return err
		}
		if clar := clarificationFromCalls(assistant.ToolCalls); clar != nil {
			dt.setMessages(append(append([]chatMsg{}, all...), assistant))
			return clar
		}

		roundStart := len(all)
		all = append(all, assistant)
		all = append(all, c.runToolCalls(ctx, round, assistant.ToolCalls, executors, resultCache)...)
		roundStarts = append(roundStarts, roundStart)
		if !ccCfg.disabled {
			all, compactedUpTo = compactOldToolRounds(all, roundStarts, compactedUpTo, ccCfg.keepRounds, ccCfg.thresholdChars)
		}
	}

	dt.setMessages(all)
	// Budget exhausted (round count or deadline): force the final answer
	// (no tools offered).
	return c.chatStreamMessages(ctx, all, w)
}

// runToolCalls executes every tool call the model requested in one round
// concurrently — not sequentially — since a model turn can (and often
// does) request several independent calls at once (e.g. a knowledge-base
// search alongside a shop lookup), and there's no reason to pay their
// latencies one after another when nothing about them depends on the
// others' results. Each call still gets its own executor-level timeout
// (see agent.go's auditExecutor) and any shared resource they touch
// (tinySQL's own mutex, the shop/Graph token caches) already serializes
// itself safely, so running them concurrently only saves wall-clock time,
// never correctness. Results are written back into the same slot as the
// requesting tool_call, so message order matches assistant.ToolCalls'
// order exactly regardless of which goroutine finishes first — the model
// only correlates by tool_call_id anyway, but a stable, reproducible
// transcript is easier to debug/audit than one whose order depends on a
// race.
func (c *lmClient) runToolCalls(ctx context.Context, round int, calls []toolCall, executors map[string]toolExecutor, resultCache map[string]string) []chatMsg {
	dt := debugTraceFromContext(ctx)
	prog := agentProgressFromContext(ctx)
	out := make([]chatMsg, len(calls))
	parallelism := toolCallsParallelism
	if len(calls) < parallelism {
		parallelism = len(calls)
	}
	// A nil semaphore is unnecessary when there are no calls, but keeping a
	// minimum capacity of one makes this defensive entry point safe for tests
	// and future callers that invoke it directly.
	if parallelism < 1 {
		parallelism = 1
	}
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc toolCall) {
			defer wg.Done()
			start := time.Now()
			result := ""
			cached := false
			var execErr error
			argsTooLarge := len(tc.Function.Arguments) > toolArgumentsMaxBytes
			if argsTooLarge {
				execErr = fmt.Errorf("tool arguments exceed %d bytes", toolArgumentsMaxBytes)
				result = "error: " + execErr.Error()
			} else {
				cacheKey := tc.Function.Name + "\x00" + tc.Function.Arguments
				result, cached = resultCache[cacheKey]
			}
			// delegate_subtasks emits its own subagent_start/subagent_end
			// steps per sub-agent; a tool_start/tool_end pair for the
			// orchestration tool itself would just be noise around them, so
			// suppress the generic pair for it.
			emitGeneric := prog != nil && tc.Function.Name != subAgentToolName
			var stepID string
			execCtx := ctx
			if emitGeneric && !cached {
				stepID = prog.send(agentStep{Type: "tool_start", Round: round + 1, Tool: tc.Function.Name, Args: truncateRunesNote(tc.Function.Arguments, 200)})
				// Lets an executor that itself spins up a nested sub-agent
				// scope (webResearchToolExecutor) parent that scope's own
				// steps under THIS tool call instead of as its sibling —
				// see currentStepIDFromContext's doc comment.
				execCtx = withCurrentStepID(ctx, stepID)
			}
			if !cached && !argsTooLarge {
				finishTracking := trackActiveAgentTool(execCtx, tc.Function.Name)
				defer finishTracking()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
					if exec, ok := executors[tc.Function.Name]; ok {
						r, err := exec(execCtx, tc.Function.Arguments)
						if err != nil {
							execErr = err
							result = "error: " + truncateRunesNote(err.Error(), 4096)
						} else {
							result = r
						}
					} else {
						result = fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
					}
				case <-execCtx.Done():
					execErr = execCtx.Err()
					result = "error: " + truncateRunesNote(execErr.Error(), 4096)
				}
			}
			if emitGeneric {
				step := agentStep{ID: stepID, Type: "tool_end", Round: round + 1, Tool: tc.Function.Name, DurationMS: time.Since(start).Milliseconds()}
				if execErr != nil {
					step.Error = truncateRunesNote(execErr.Error(), 1000)
				} else {
					step.Result = truncateRunesNote(result, 300)
				}
				prog.send(step)
			}
			if verbose {
				note := ""
				if cached {
					note = " (cached, identical repeat call)"
				}
				log.Printf("[verbose] tool call (round %d) %s(%s) -> %d bytes%s", round+1, tc.Function.Name, truncateRunesNote(tc.Function.Arguments, 1000), len(result), note)
			}
			rec := debugToolCall{Round: round + 1, Name: tc.Function.Name, Arguments: truncateRunesNote(tc.Function.Arguments, toolArgumentsMaxBytes), DurationMS: time.Since(start).Milliseconds(), Agent: subAgentLabelFromContext(ctx)}
			if execErr != nil {
				rec.Error = truncateRunesNote(execErr.Error(), 10000)
			} else {
				rec.Result = truncateRunesNote(result, 10000)
			}
			dt.addToolCall(rec)
			out[i] = chatMsg{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: result}
		}(i, tc)
	}
	wg.Wait()
	// Populate the cache after every call in this round has finished
	// (not incrementally as each one lands), so two identical calls
	// requested within the *same* round still both execute — only a
	// repeat across rounds is ever short-circuited. Deduplicating within
	// one round would be observably different behavior (the model asked
	// for both; it should get both, even if redundant) whereas across
	// rounds a repeat is unambiguously the model re-asking after seeing
	// the answer already.
	for i, tc := range calls {
		if len(tc.Function.Arguments) <= toolArgumentsMaxBytes {
			resultCache[tc.Function.Name+"\x00"+tc.Function.Arguments] = out[i].Content
		}
	}
	return out
}
