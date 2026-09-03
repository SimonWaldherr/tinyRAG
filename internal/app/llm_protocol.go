package app

// Protocol adapters for inference servers.  The application speaks one small
// internal lmProvider interface; this file keeps wire-format differences out
// of retrieval, agent, and UI code.  The OpenAI-compatible profile is the
// broad default (including GopherLLM, RustyLLM, llama.cpp, LM Studio, vLLM,
// Jan, LocalAI and cloud gateways).  The Ollama profile is useful for native
// Ollama-compatible servers that are not exposing the /v1 compatibility layer.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	inferenceAPIAuto   = "auto"
	inferenceAPIOpenAI = "openai"
	inferenceAPIOllama = "ollama"
)

// normalizeInferenceAPI accepts the persisted/UI spelling of an inference
// protocol and collapses common aliases.  Unknown values deliberately fall
// back to auto so a settings typo cannot disable the backend completely.
func normalizeInferenceAPI(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "openai", "openai-compatible", "openai_compatible", "compat", "compatible", "v1":
		return inferenceAPIOpenAI
	case "ollama", "ollama-native", "ollama_native", "native":
		return inferenceAPIOllama
	case "", "auto", "detect", "automatic":
		return inferenceAPIAuto
	default:
		return inferenceAPIAuto
	}
}

// inferInferenceAPIFromBase provides a conservative default when a user has
// not selected a profile.  An explicit /v1 in the URL always means the
// OpenAI-compatible path; the standard Ollama port/hostname prefers native
// endpoints.  All other hosts stay OpenAI-compatible, which is the most
// interoperable choice for local and hosted inference applications.
func inferInferenceAPIFromBase(base string) string {
	raw := strings.TrimSpace(base)
	if raw == "" {
		return inferenceAPIOpenAI
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		parsed, _ = url.Parse("http://" + strings.TrimPrefix(raw, "//"))
	}
	host := strings.ToLower(parsed.Hostname())
	path := strings.ToLower(strings.TrimRight(parsed.EscapedPath(), "/"))
	if strings.HasSuffix(path, "/v1") || strings.Contains(path, "/v1/") {
		return inferenceAPIOpenAI
	}
	if host == "ollama" || strings.HasSuffix(host, ".ollama") || parsed.Port() == "11434" {
		return inferenceAPIOllama
	}
	return inferenceAPIOpenAI
}

func (c *lmClient) configuredInferenceAPI() string {
	c.styleMu.RLock()
	style := c.apiStyle
	c.styleMu.RUnlock()
	return normalizeInferenceAPI(style)
}

func (c *lmClient) currentInferenceAPI(base string) string {
	configured := c.configuredInferenceAPI()
	if configured != inferenceAPIAuto {
		return configured
	}
	c.styleMu.RLock()
	detected := c.detectedStyle
	c.styleMu.RUnlock()
	if detected != "" {
		return detected
	}
	return inferInferenceAPIFromBase(base)
}

func (c *lmClient) rememberInferenceAPI(base, style string) {
	if normalizeInferenceAPI(style) == inferenceAPIAuto || c.configuredInferenceAPI() != inferenceAPIAuto {
		return
	}
	if normalizeBaseURL(base) != c.base {
		return
	}
	c.styleMu.Lock()
	c.detectedStyle = normalizeInferenceAPI(style)
	c.styleMu.Unlock()
}

// inferenceAPI returns the effective style for diagnostics and API responses.
func (c *lmClient) inferenceAPI() string {
	return c.currentInferenceAPI(c.base)
}

func (c *lmClient) inferenceAPIChoices(base string) []string {
	first := c.currentInferenceAPI(base)
	if c.configuredInferenceAPI() != inferenceAPIAuto {
		return []string{first}
	}
	second := inferenceAPIOpenAI
	if first == inferenceAPIOpenAI {
		second = inferenceAPIOllama
	}
	return []string{first, second}
}

func inferenceProtocolBase(base, style string) string {
	base = normalizeBaseURL(base)
	if style == inferenceAPIOllama && strings.HasSuffix(strings.ToLower(base), "/api") {
		return strings.TrimSuffix(base, "/api")
	}
	return base
}

// inferenceHTTPError preserves status information so auto-detection can
// retry a different protocol only for unsupported routes, not on auth or
// server failures where retrying would hide the real problem.
type inferenceHTTPError struct {
	operation string
	status    int
	body      string
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Thinking         string `json:"thinking"`
		} `json:"delta"`
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Thinking         string `json:"thinking"`
		} `json:"message"`
	} `json:"choices"`
}

func parseOpenAIStreamChunk(raw []byte) (content, thinking string, ok bool) {
	var chunk openAIStreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil || len(chunk.Choices) == 0 {
		return "", "", false
	}
	choice := chunk.Choices[0]
	content = choice.Delta.Content
	if content == "" {
		content = choice.Message.Content
	}
	thinking = choice.Delta.ReasoningContent
	if thinking == "" {
		thinking = choice.Delta.Reasoning
	}
	if thinking == "" {
		thinking = choice.Delta.Thinking
	}
	if thinking == "" {
		thinking = choice.Message.ReasoningContent
	}
	if thinking == "" {
		thinking = choice.Message.Reasoning
	}
	if thinking == "" {
		thinking = choice.Message.Thinking
	}
	return content, thinking, true
}

func (e *inferenceHTTPError) Error() string {
	if strings.TrimSpace(e.body) == "" {
		return fmt.Sprintf("%s HTTP %d", e.operation, e.status)
	}
	return fmt.Sprintf("%s HTTP %d: %s", e.operation, e.status, strings.TrimSpace(e.body))
}

func canTryInferenceFallback(err error) bool {
	e, ok := err.(*inferenceHTTPError)
	return ok && (e.status == http.StatusNotFound || e.status == http.StatusMethodNotAllowed || e.status == http.StatusNotImplemented)
}

func authInferenceRequest(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func readInferenceBody(resp *http.Response, operation string) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read response: %w", operation, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &inferenceHTTPError{operation: operation, status: resp.StatusCode, body: string(raw)}
	}
	return raw, nil
}

func (c *lmClient) listModels(baseOverride string) ([]string, error) {
	base := c.base
	if strings.TrimSpace(baseOverride) != "" {
		base = normalizeBaseURL(baseOverride)
	}
	var lastErr error
	for _, style := range c.inferenceAPIChoices(base) {
		models, err := c.listModelsWithStyle(base, style)
		if err == nil {
			c.rememberInferenceAPI(base, style)
			return models, nil
		}
		lastErr = err
		if c.configuredInferenceAPI() != inferenceAPIAuto || !canTryInferenceFallback(err) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no inference protocol available")
	}
	return nil, lastErr
}

func (c *lmClient) listModelsWithStyle(base, style string) ([]string, error) {
	base = inferenceProtocolBase(base, style)
	endpoint := base + "/v1/models"
	if style == inferenceAPIOllama {
		endpoint = base + "/api/tags"
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create models request: %w", err)
	}
	authInferenceRequest(req, c.apiKey)
	client := c.http
	if client == nil {
		client = newHTTPClient(10 * time.Second)
	} else {
		// Chat/embedding calls have a generous generation timeout, while
		// discovery must fail fast so startup does not serially wait on every
		// offline local port.
		clone := *client
		clone.Timeout = 10 * time.Second
		client = &clone
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := readInferenceBody(resp, "models")
	if err != nil {
		return nil, err
	}
	var out []string
	if style == inferenceAPIOllama {
		var tags struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.Unmarshal(raw, &tags); err != nil {
			return nil, fmt.Errorf("models: invalid Ollama response: %w", err)
		}
		for _, model := range tags.Models {
			name := strings.TrimSpace(model.Name)
			if name == "" {
				name = strings.TrimSpace(model.Model)
			}
			if name != "" {
				out = append(out, name)
			}
		}
	} else {
		var mr modelsResp
		if err := json.Unmarshal(raw, &mr); err != nil {
			return nil, fmt.Errorf("models: invalid OpenAI-compatible response: %w", err)
		}
		for _, d := range mr.Data {
			if d.ID != "" {
				out = append(out, d.ID)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
	Embedding  []float64   `json:"embedding"`
}

func (c *lmClient) embed(texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return [][]float64{}, nil
	}
	var lastErr error
	for _, style := range c.inferenceAPIChoices(c.base) {
		var vecs [][]float64
		var err error
		if style == inferenceAPIOllama {
			vecs, err = c.embedOllama(texts)
		} else {
			vecs, err = c.embedOpenAI(texts)
		}
		if err == nil {
			c.rememberInferenceAPI(c.base, style)
			return vecs, nil
		}
		lastErr = err
		if c.configuredInferenceAPI() != inferenceAPIAuto || !canTryInferenceFallback(err) {
			break
		}
	}
	return nil, lastErr
}

func (c *lmClient) embedOpenAI(texts []string) ([][]float64, error) {
	base := inferenceProtocolBase(c.base, inferenceAPIOpenAI)
	body, err := json.Marshal(embReq{Model: c.embedModel, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embed request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	authInferenceRequest(req, c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := readInferenceBody(resp, "embed")
	if err != nil {
		return nil, err
	}
	var er embResp
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("embed: invalid OpenAI-compatible response: %w", err)
	}
	vecs := make([][]float64, len(er.Data))
	for i, d := range er.Data {
		vecs[i] = d.Embedding
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: no embeddings returned")
	}
	return vecs, nil
}

func (c *lmClient) embedOllama(texts []string) ([][]float64, error) {
	base := inferenceProtocolBase(c.base, inferenceAPIOllama)
	body, err := json.Marshal(ollamaEmbedRequest{Model: c.embedModel, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Ollama embed request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	authInferenceRequest(req, c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := readInferenceBody(resp, "Ollama embed")
	if err != nil {
		if canTryInferenceFallback(err) {
			return c.embedOllamaLegacy(texts)
		}
		return nil, err
	}
	var er ollamaEmbedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("Ollama embed: invalid response: %w", err)
	}
	if len(er.Embeddings) > 0 {
		return er.Embeddings, nil
	}
	if len(er.Embedding) > 0 && len(texts) == 1 {
		return [][]float64{er.Embedding}, nil
	}
	return nil, fmt.Errorf("Ollama embed: no embeddings returned")
}

// Older Ollama-compatible servers expose /api/embeddings (one prompt per
// request) instead of the batched /api/embed endpoint.
func (c *lmClient) embedOllamaLegacy(texts []string) ([][]float64, error) {
	base := inferenceProtocolBase(c.base, inferenceAPIOllama)
	vecs := make([][]float64, 0, len(texts))
	for _, text := range texts {
		body, err := json.Marshal(map[string]string{"model": c.embedModel, "prompt": text})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal legacy Ollama embed request: %w", err)
		}
		req, err := http.NewRequest(http.MethodPost, base+"/api/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		authInferenceRequest(req, c.apiKey)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		raw, err := readInferenceBody(resp, "legacy Ollama embed")
		if err != nil {
			return nil, err
		}
		var er ollamaEmbedResponse
		if err := json.Unmarshal(raw, &er); err != nil {
			return nil, fmt.Errorf("legacy Ollama embed: invalid response: %w", err)
		}
		if len(er.Embedding) == 0 {
			if len(er.Embeddings) == 1 {
				vecs = append(vecs, er.Embeddings[0])
				continue
			}
			return nil, fmt.Errorf("legacy Ollama embed: no embedding returned")
		}
		vecs = append(vecs, er.Embedding)
	}
	return vecs, nil
}

type ollamaChatRequest struct {
	Model    string    `json:"model"`
	Messages []chatMsg `json:"messages"`
	Stream   bool      `json:"stream"`
}

type ollamaChatChunk struct {
	Message struct {
		Content  string `json:"content"`
		Thinking string `json:"thinking"`
	} `json:"message"`
	Response string `json:"response"`
	Thinking string `json:"thinking"`
	Done     bool   `json:"done"`
	Error    string `json:"error"`
}

func (c *lmClient) chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	all := make([]chatMsg, 0, len(msgs)+1)
	all = append(all, chatMsg{Role: "system", Content: system})
	all = append(all, msgs...)
	var lastErr error
	for _, style := range c.inferenceAPIChoices(c.base) {
		var err error
		if style == inferenceAPIOllama {
			err = c.chatStreamOllama(ctx, all, w, thinkW)
		} else {
			err = c.chatStreamOpenAI(ctx, system, msgs, w, thinkW)
		}
		if err == nil {
			c.rememberInferenceAPI(c.base, style)
			return nil
		}
		lastErr = err
		if c.configuredInferenceAPI() != inferenceAPIAuto || !canTryInferenceFallback(err) {
			break
		}
	}
	return lastErr
}

func (c *lmClient) chatStreamOllama(ctx context.Context, all []chatMsg, w io.Writer, thinkW io.Writer) error {
	body, err := json.Marshal(ollamaChatRequest{Model: c.chatModel, Messages: all, Stream: true})
	if err != nil {
		return fmt.Errorf("failed to marshal Ollama chat request: %w", err)
	}
	return c.streamOllama(ctx, body, w, thinkW)
}

func (c *lmClient) streamOllama(ctx context.Context, body []byte, w io.Writer, thinkW io.Writer) error {
	base := inferenceProtocolBase(c.base, inferenceAPIOllama)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create Ollama chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	authInferenceRequest(req, c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Ollama chat request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("Ollama chat HTTP %d (failed to read body: %v)", resp.StatusCode, readErr)
		}
		return &inferenceHTTPError{operation: "Ollama chat", status: resp.StatusCode, body: string(raw)}
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var pending string
	inThink := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk ollamaChatChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			// A few gateways prefix NDJSON with SSE's data: marker.
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}
		}
		if chunk.Error != "" {
			return fmt.Errorf("Ollama chat: %s", chunk.Error)
		}
		thinking := chunk.Message.Thinking
		if thinking == "" {
			thinking = chunk.Thinking
		}
		if thinking != "" && thinkW != nil {
			if err := writeStreamChunk(thinkW, thinking); err != nil {
				return err
			}
		}
		tok := chunk.Message.Content
		if tok == "" {
			tok = chunk.Response
		}
		if tok != "" {
			if err := streamSplitThinkingChunk(tok, &pending, &inThink, w, thinkW); err != nil {
				return err
			}
		}
		if chunk.Done {
			break
		}
	}
	if pending != "" {
		if inThink && thinkW != nil {
			if err := writeStreamChunk(thinkW, pending); err != nil {
				return err
			}
		} else if err := writeStreamChunk(w, pending); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type ollamaVisionMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content,omitempty"`
	Images  []string `json:"images,omitempty"`
}

type ollamaVisionRequest struct {
	Model    string                `json:"model"`
	Messages []ollamaVisionMessage `json:"messages"`
	Stream   bool                  `json:"stream"`
}

func (c *lmClient) chatStreamVision(ctx context.Context, system string, msgs []visionMsg, w io.Writer, thinkW io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for _, style := range c.inferenceAPIChoices(c.base) {
		var err error
		if style == inferenceAPIOllama {
			err = c.chatStreamVisionOllama(ctx, system, msgs, w, thinkW)
		} else {
			err = c.chatStreamVisionOpenAI(ctx, system, msgs, w, thinkW)
		}
		if err == nil {
			c.rememberInferenceAPI(c.base, style)
			return nil
		}
		lastErr = err
		if c.configuredInferenceAPI() != inferenceAPIAuto || !canTryInferenceFallback(err) {
			break
		}
	}
	return lastErr
}

func (c *lmClient) chatStreamVisionOllama(ctx context.Context, system string, msgs []visionMsg, w io.Writer, thinkW io.Writer) error {
	all := make([]ollamaVisionMessage, 0, len(msgs)+1)
	all = append(all, ollamaVisionMessage{Role: "system", Content: system})
	for _, msg := range msgs {
		converted := ollamaVisionMessage{Role: msg.Role}
		for _, part := range msg.Content {
			switch part.Type {
			case "text":
				converted.Content += part.Text
			case "image_url":
				if part.ImageURL == nil {
					continue
				}
				value := part.ImageURL.URL
				if idx := strings.Index(value, ","); strings.HasPrefix(strings.ToLower(value), "data:") && idx >= 0 {
					value = value[idx+1:]
				}
				if strings.Contains(value, "://") {
					return &inferenceHTTPError{operation: "Ollama vision", status: http.StatusNotImplemented, body: "remote image URLs require an OpenAI-compatible vision endpoint"}
				}
				if _, err := base64.StdEncoding.DecodeString(value); err != nil {
					return fmt.Errorf("Ollama vision: invalid base64 image: %w", err)
				}
				converted.Images = append(converted.Images, value)
			}
		}
		all = append(all, converted)
	}
	body, err := json.Marshal(ollamaVisionRequest{Model: c.chatModel, Messages: all, Stream: true})
	if err != nil {
		return fmt.Errorf("failed to marshal Ollama vision request: %w", err)
	}
	return c.streamOllama(ctx, body, w, thinkW)
}
