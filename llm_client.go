package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// OpenAI-compatible client (LM Studio, Ollama, …)
// ─────────────────────────────────────────────────────────────────────────────

// lmClient is a small OpenAI-compatible client used for embeddings
// and chat completions against local or remote LLM endpoints.
type lmClient struct {
	base       string
	embedModel string
	chatModel  string
	apiKey     string
	http       *http.Client
}

func newHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// lmProvider abstracts an LLM client used for embeddings and chat.
type lmProvider interface {
	embed(texts []string) ([][]float64, error)
	embedSingle(text string) ([]float64, error)
	chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error
	chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error
	chatStreamVision(ctx context.Context, system string, msgs []visionMsg, w io.Writer, thinkW io.Writer) error
}

// compositeLM allows routing embeddings and chat to different backends.
type compositeLM struct {
	embedClient lmProvider
	chatClient  lmProvider
}

func (c *compositeLM) embed(texts []string) ([][]float64, error) {
	return c.embedClient.embed(texts)
}
func (c *compositeLM) embedSingle(text string) ([]float64, error) {
	return c.embedClient.embedSingle(text)
}
func (c *compositeLM) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	return c.chatClient.chatStream(ctx, system, msgs, w)
}
func (c *compositeLM) chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error {
	return c.chatClient.chatStreamDetailed(ctx, system, msgs, w, thinkW)
}

func (c *compositeLM) chatStreamVision(ctx context.Context, system string, msgs []visionMsg, w io.Writer, thinkW io.Writer) error {
	return c.chatClient.chatStreamVision(ctx, system, msgs, w, thinkW)
}

// newLMClient constructs an `lmClient` configured for the given
// base URL and model names.
func newLMClient(base, embedModel, chatModel, apiKey string) *lmClient {
	return &lmClient{
		base:       normalizeBaseURL(base),
		embedModel: embedModel,
		chatModel:  chatModel,
		apiKey:     apiKey,
		http:       newHTTPClient(120 * time.Second),
	}
}

// ping checks the LLM endpoint for reachability by requesting
// the list of available models.
func (c *lmClient) ping() error {
	req, err := http.NewRequest("GET", c.base+"/v1/models", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("LLM endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// modelsResp is a helper for parsing the /v1/models response.
type modelsResp struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// listModels queries the LLM endpoint for available model IDs,
// optionally overriding the client's base URL.
func (c *lmClient) listModels(baseOverride string) ([]string, error) {
	base := c.base
	if strings.TrimSpace(baseOverride) != "" {
		base = normalizeBaseURL(baseOverride)
	}
	req, err := http.NewRequest("GET", base+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create models request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := newHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read models response: %w", readErr)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var mr modelsResp
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, err
	}
	var out []string
	for _, d := range mr.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// embReq represents an embeddings request payload.
type embReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embResp represents an embeddings response payload.
type embResp struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// embed sends multiple `texts` to the embedding endpoint and returns
// their vector embeddings.
func (c *lmClient) embed(texts []string) ([][]float64, error) {
	body, err := json.Marshal(embReq{Model: c.embedModel, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embed request: %w", err)
	}
	req, err := http.NewRequest("POST", c.base+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read embeddings response: %w", readErr)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embed %d: %s", resp.StatusCode, string(raw))
	}
	var er embResp
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, err
	}
	vecs := make([][]float64, len(er.Data))
	for i, d := range er.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// embedSingle returns the embedding vector for a single text input.
func (c *lmClient) embedSingle(text string) ([]float64, error) {
	vecs, err := c.embed([]string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return vecs[0], nil
}

// chatReq models the request payload for chat completions.
type chatReq struct {
	Model    string    `json:"model"`
	Messages []chatMsg `json:"messages"`
	Stream   bool      `json:"stream"`
}

// contentPart is a single element in a multimodal message content array,
// used for vision-capable models (text + image_url parts).
type contentPart struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *imageURLContent `json:"image_url,omitempty"`
}

// imageURLContent carries the URL (data URI or https) and optional detail level
// for an image_url content part.
type imageURLContent struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "low", "high", or "auto"
}

// chatMsg represents a single chat message with a role and content.
// Content is a plain string for text-only messages.  When ContentParts is
// non-empty the message is multimodal and its JSON representation uses an
// array of contentPart objects instead of a string, as required by the
// OpenAI vision API.
type chatMsg struct {
	Role         string        `json:"role"`
	Content      string        `json:"content,omitempty"`
	ContentParts []contentPart `json:"-"` // set for vision messages; excluded from JSON struct tags so MarshalJSON controls serialisation
}

// MarshalJSON serialises chatMsg to the wire format expected by
// OpenAI-compatible APIs.  Text-only messages produce {"role":"…","content":"…"};
// multimodal messages produce {"role":"…","content":[…]}.
func (m chatMsg) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role    string      `json:"role"`
		Content interface{} `json:"content"`
	}
	if len(m.ContentParts) > 0 {
		return json.Marshal(wire{Role: m.Role, Content: m.ContentParts})
	}
	return json.Marshal(wire{Role: m.Role, Content: m.Content})
}

// isVisionModel reports whether a model ID is likely to support image inputs.
// The check is heuristic and covers the most common vision-capable families.
func isVisionModel(model string) bool {
	ml := strings.ToLower(model)
	return strings.Contains(ml, "vision") ||
		strings.Contains(ml, "-vl") ||
		strings.Contains(ml, "vl-") ||
		strings.Contains(ml, "llava") ||
		strings.Contains(ml, "pixtral") ||
		strings.Contains(ml, "gpt-4o") ||
		strings.Contains(ml, "gpt-4-turbo") ||
		strings.Contains(ml, "claude-3") ||
		strings.Contains(ml, "claude-opus") ||
		strings.Contains(ml, "claude-sonnet") ||
		strings.Contains(ml, "gemini") ||
		strings.Contains(ml, "internvl") ||
		strings.Contains(ml, "cogvlm") ||
		strings.Contains(ml, "minicpm") ||
		strings.Contains(ml, "moondream") ||
		strings.Contains(ml, "phi-3-vision") ||
		strings.Contains(ml, "phi4") ||
		strings.Contains(ml, "qwen2-vl") ||
		strings.Contains(ml, "smolvlm")
}

// imageDataURI builds a base64-encoded data URI from raw image bytes and a MIME type.
func imageDataURI(data []byte, mimeType string) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// describeImageWithVision calls a vision-capable LLM to generate a text
// description of an image that can be stored in the RAG knowledge base.
func describeImageWithVision(ctx context.Context, lm lmProvider, imageData []byte, mimeType, filename string) (string, error) {
	dataURI := imageDataURI(imageData, mimeType)
	msg := chatMsg{
		Role: "user",
		ContentParts: []contentPart{
			{
				Type: "text",
				Text: fmt.Sprintf(
					"Describe the content of this image (%s) in detail. "+
						"Include all visible text, diagrams, tables, charts, and any other "+
						"information that would be useful in a knowledge base. "+
						"Be thorough and precise.",
					filename,
				),
			},
			{
				Type:     "image_url",
				ImageURL: &imageURLContent{URL: dataURI, Detail: "high"},
			},
		},
	}
	systemPrompt := "You are an image analysis assistant. Describe images accurately and extract all textual and visual information in a structured way."
	var buf bytes.Buffer
	if err := lm.chatStream(ctx, systemPrompt, []chatMsg{msg}, &buf); err != nil {
		return "", fmt.Errorf("vision description failed: %w", err)
	}
	desc := strings.TrimSpace(buf.String())
	if desc == "" {
		return "", fmt.Errorf("vision model returned an empty description for %q", filename)
	}
	return desc, nil
}

// visionContentPart represents a single part of a multimodal message
// (either plain text or an image URL / base64 data URI).
type visionContentPart struct {
	Type     string          `json:"type"`                // "text" or "image_url"
	Text     string          `json:"text,omitempty"`      // set when Type=="text"
	ImageURL *visionImageURL `json:"image_url,omitempty"` // set when Type=="image_url"
}

type visionImageURL struct {
	URL    string `json:"url"`              // data URI or https URL
	Detail string `json:"detail,omitempty"` // "auto", "low", "high"
}

// visionMsg is a chat message whose content is a slice of content parts,
// enabling multimodal (vision) requests.
type visionMsg struct {
	Role    string              `json:"role"`
	Content []visionContentPart `json:"content"`
}

// visionReq is the request body sent to the chat completions endpoint for
// multimodal (vision) conversations.
type visionReq struct {
	Model    string      `json:"model"`
	Messages []visionMsg `json:"messages"`
	Stream   bool        `json:"stream"`
}

// chatStreamVision sends a vision request to the LLM and streams the
// response tokens to w (thinking tokens to thinkW if non-nil).
func (c *lmClient) chatStreamVision(ctx context.Context, system string, msgs []visionMsg, w io.Writer, thinkW io.Writer) error {
	all := make([]visionMsg, 0, len(msgs)+1)
	// Prepend system message as plain text visionMsg.
	all = append(all, visionMsg{
		Role: "system",
		Content: []visionContentPart{
			{Type: "text", Text: system},
		},
	})
	all = append(all, msgs...)
	body, err := json.Marshal(visionReq{Model: c.chatModel, Messages: all, Stream: true})
	if err != nil {
		return fmt.Errorf("failed to marshal vision request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create vision HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vision request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vision HTTP %d: %s", resp.StatusCode, string(raw))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var pending string
	inThink := false
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
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
			tok := chunk.Choices[0].Delta.Content
			if tok != "" {
				if err := streamSplitThinkingChunk(tok, &pending, &inThink, w, thinkW); err != nil {
					return err
				}
			}
		}
	}
	if pending != "" {
		if inThink && thinkW != nil {
			writeStreamChunk(thinkW, pending)
		} else {
			writeStreamChunk(w, pending)
		}
	}
	return scanner.Err()
}

func writeStreamChunk(w io.Writer, s string) error {
	if w == nil || s == "" {
		return nil
	}
	_, err := io.WriteString(w, s)
	return err
}

func partialMarkerPrefixLen(s string, markers []string) int {
	maxLen := 0
	for _, marker := range markers {
		limit := len(marker) - 1
		if limit > len(s) {
			limit = len(s)
		}
		for n := limit; n > 0; n-- {
			if strings.HasSuffix(s, marker[:n]) {
				if n > maxLen {
					maxLen = n
				}
				break
			}
		}
	}
	return maxLen
}

func streamSplitThinkingChunk(tok string, pending *string, inThink *bool, visibleW io.Writer, thinkW io.Writer) error {
	const (
		startXML = "<think>"
		endXML   = "</think>"
		startBB  = "[THINK]"
		endBB    = "[/THINK]"
	)
	startMarkers := []string{startXML, startBB}
	endMarkers := []string{endXML, endBB}

	*pending += tok
	for len(*pending) > 0 {
		if !*inThink {
			nextIdx := -1
			nextMarker := ""
			for _, marker := range startMarkers {
				if idx := strings.Index(*pending, marker); idx != -1 && (nextIdx == -1 || idx < nextIdx) {
					nextIdx = idx
					nextMarker = marker
				}
			}
			if nextIdx == -1 {
				keep := partialMarkerPrefixLen(*pending, startMarkers)
				emitLen := len(*pending) - keep
				if emitLen > 0 {
					if err := writeStreamChunk(visibleW, (*pending)[:emitLen]); err != nil {
						return err
					}
					*pending = (*pending)[emitLen:]
				}
				break
			}
			if nextIdx > 0 {
				if err := writeStreamChunk(visibleW, (*pending)[:nextIdx]); err != nil {
					return err
				}
			}
			*pending = (*pending)[nextIdx+len(nextMarker):]
			*inThink = true
			continue
		}

		nextIdx := -1
		nextMarker := ""
		for _, marker := range endMarkers {
			if idx := strings.Index(*pending, marker); idx != -1 && (nextIdx == -1 || idx < nextIdx) {
				nextIdx = idx
				nextMarker = marker
			}
		}
		if nextIdx == -1 {
			keep := partialMarkerPrefixLen(*pending, endMarkers)
			emitLen := len(*pending) - keep
			if emitLen > 0 {
				if err := writeStreamChunk(thinkW, (*pending)[:emitLen]); err != nil {
					return err
				}
				*pending = (*pending)[emitLen:]
			}
			break
		}
		if nextIdx > 0 {
			if err := writeStreamChunk(thinkW, (*pending)[:nextIdx]); err != nil {
				return err
			}
		}
		*pending = (*pending)[nextIdx+len(nextMarker):]
		*inThink = false
	}
	return nil
}

// chatStream streams tokens from the chat completion endpoint and
// writes them to `w` as they arrive.
func (c *lmClient) chatStream(ctx context.Context, system string, msgs []chatMsg, w io.Writer) error {
	return c.chatStreamDetailed(ctx, system, msgs, w, nil)
}

func (c *lmClient) chatStreamDetailed(ctx context.Context, system string, msgs []chatMsg, w io.Writer, thinkW io.Writer) error {
	all := make([]chatMsg, 0, len(msgs)+1)
	all = append(all, chatMsg{Role: "system", Content: system})
	all = append(all, msgs...)
	body, err := json.Marshal(chatReq{Model: c.chatModel, Messages: all, Stream: true})
	if err != nil {
		return fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("chat request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("chat HTTP %d (failed to read body: %v)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("chat HTTP %d: %s", resp.StatusCode, string(raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var pending string
	inThink := false

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
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
			tok := chunk.Choices[0].Delta.Content
			if tok != "" {
				if err := streamSplitThinkingChunk(tok, &pending, &inThink, w, thinkW); err != nil {
					return err
				}
			}
		}
	}
	if pending != "" {
		if inThink {
			if err := writeStreamChunk(thinkW, pending); err != nil {
				return err
			}
		} else {
			if err := writeStreamChunk(w, pending); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// RAG system
// ─────────────────────────────────────────────────────────────────────────────

// vecJSON marshals a float64 slice into a JSON string for SQL usage.
