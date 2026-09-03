package main

import (
	"encoding/json"
	"log"
	"net/url"
	"os"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Web server + helper endpoints
// ─────────────────────────────────────────────────────────────────────────────

// mustJSON encodes `v` to a compact JSON string, ignoring errors.
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// llmCheckReq is the request structure for model/endpoint validation.
type llmCheckReq struct {
	BaseURL      string `json:"base_url"`
	InferenceAPI string `json:"inference_api"`
	OpenAIKey    string `json:"openai_api_key"`
}

// llmCheckResp is the response structure returned when validating an LLM endpoint.
type llmCheckResp struct {
	OK             bool     `json:"ok"`
	BaseURL        string   `json:"base_url"`
	APIStyle       string   `json:"api_style,omitempty"`
	ProviderHint   string   `json:"provider_hint"`
	Error          string   `json:"error,omitempty"`
	Models         []string `json:"models,omitempty"`
	RecommendChat  []string `json:"recommend_chat,omitempty"`
	RecommendEmbed []string `json:"recommend_embed,omitempty"`
}

// providerHintFromURL returns a human-friendly hint about the LLM
// provider based on the base URL, using proper hostname matching where possible.
func providerHintFromURL(base string) string {
	parsed, parseErr := url.Parse(strings.TrimSpace(base))
	if parseErr != nil {
		parsed = &url.URL{}
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()

	// Local endpoints identified by known host + port combinations.
	// Note: several local runners default to the same port (e.g. llama.cpp's
	// llama-server, LocalAI and llmster all commonly use 8080), so the hint
	// is a best-effort guess — the user-selected provider in the UI (or the
	// -url flag) is the source of truth, this is only used to pre-select a
	// sensible default.
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		switch port {
		case "11434":
			return "Ollama"
		case "1234":
			return "LM Studio"
		case "8080":
			return "llmster"
		case "8000":
			return "vLLM"
		case "5000":
			return "text-generation-webui"
		case "5001":
			return "KoboldCpp"
		case "1337":
			return "Jan"
		case "8091":
			return "GopherLLM"
		}
		return "Local LLM"
	}

	// Remote providers matched by hostname suffix.
	switch {
	case strings.Contains(host, "gopherllm"):
		return "GopherLLM"
	case strings.Contains(host, "rustyllm"):
		return "RustyLLM"
	case host == "api.openai.com" || strings.HasSuffix(host, ".openai.com"):
		return "OpenAI"
	case host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com"):
		return "Anthropic"
	case strings.HasSuffix(host, ".googleapis.com") || host == "generativelanguage.googleapis.com":
		return "Google Gemini"
	case host == "api.mistral.ai" || strings.HasSuffix(host, ".mistral.ai"):
		return "Mistral AI"
	case host == "api.groq.com" || strings.HasSuffix(host, ".groq.com"):
		return "Groq"
	case host == "api.deepseek.com" || strings.HasSuffix(host, ".deepseek.com"):
		return "DeepSeek"
	case strings.HasSuffix(host, ".together.xyz") || strings.HasSuffix(host, ".together.ai"):
		return "Together AI"
	case host == "api.x.ai" || strings.HasSuffix(host, ".x.ai"):
		return "xAI"
	case strings.HasSuffix(host, ".cohere.com") || strings.HasSuffix(host, ".cohere.ai"):
		return "Cohere"
	case strings.HasSuffix(host, ".perplexity.ai"):
		return "Perplexity"
	case host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai"):
		return "OpenRouter"
	}

	// Fallback: port-based hints for non-standard local deployments
	// (e.g. a LAN IP or hostname running one of the same local servers).
	switch port {
	case "11434":
		return "Ollama"
	case "1234":
		return "LM Studio"
	case "8080":
		return "llmster"
	case "8000":
		return "vLLM"
	case "5000":
		return "text-generation-webui"
	case "5001":
		return "KoboldCpp"
	case "1337":
		return "Jan"
	case "8091":
		return "GopherLLM"
	}

	return "OpenAI-compatible"
}

// recommendModels heuristically selects likely chat and embedding models
// from a list of available model IDs.
func recommendModels(models []string) (chat []string, embed []string) {
	// Heuristics only: highlight likely candidates.
	for _, m := range models {
		ml := strings.ToLower(m)
		if strings.Contains(ml, "embed") || strings.Contains(ml, "embedding") ||
			strings.Contains(ml, "e5-") || strings.Contains(ml, "bge-") ||
			strings.Contains(ml, "minilm") || strings.Contains(ml, "nomic") ||
			strings.Contains(ml, "gte-") || strings.Contains(ml, "jina-embed") ||
			strings.Contains(ml, "paraphrase") || strings.Contains(ml, "multilingual") ||
			strings.Contains(ml, "mpnet") || strings.Contains(ml, "sentence-transformers") ||
			strings.Contains(ml, "sentence_transformers") {
			embed = append(embed, m)
		}
		// Common chat-ish hints
		if strings.Contains(ml, "llama") ||
			strings.Contains(ml, "mistral") ||
			strings.Contains(ml, "mixtral") ||
			strings.Contains(ml, "qwen") ||
			strings.Contains(ml, "gemma") ||
			strings.Contains(ml, "phi") ||
			strings.Contains(ml, "gpt") ||
			strings.Contains(ml, "ministral") ||
			strings.Contains(ml, "claude") ||
			strings.Contains(ml, "gemini") ||
			strings.Contains(ml, "command") ||
			strings.Contains(ml, "deepseek") ||
			strings.Contains(ml, "hermes") ||
			strings.Contains(ml, "zephyr") ||
			strings.Contains(ml, "falcon") ||
			strings.Contains(ml, "vicuna") ||
			strings.Contains(ml, "wizard") ||
			strings.Contains(ml, "solar") ||
			strings.Contains(ml, "openchat") ||
			strings.Contains(ml, "dolphin") ||
			strings.Contains(ml, "nous") ||
			strings.Contains(ml, "llava") ||
			strings.Contains(ml, "pixtral") ||
			strings.Contains(ml, "smolvlm") ||
			strings.Contains(ml, "internvl") ||
			strings.Contains(ml, "moondream") {
			chat = append(chat, m)
		}
	}
	// Keep lists short
	if len(chat) > 8 {
		chat = chat[:8]
	}
	if len(embed) > 8 {
		embed = embed[:8]
	}
	return
}

// discoverCandidate contains information about a discovered LLM endpoint.
type discoverCandidate struct {
	BaseURL        string   `json:"base_url"`
	ProviderHint   string   `json:"provider_hint"`
	OK             bool     `json:"ok"`
	Error          string   `json:"error,omitempty"`
	Models         []string `json:"models,omitempty"`
	RecommendChat  []string `json:"recommend_chat,omitempty"`
	RecommendEmbed []string `json:"recommend_embed,omitempty"`
}

// discoverResp is returned from the /api/discover endpoint.
type discoverResp struct {
	Candidates []discoverCandidate `json:"candidates"`
}

func isLocalLLMBase(base string) bool {
	u := strings.ToLower(strings.TrimSpace(base))
	return strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1")
}

// localLLMCandidates lists base URLs for common local model runners, probed
// in order by maybePreferOfflineLLM on startup when the configured endpoint
// is unreachable. Covers LM Studio, llmster/llama.cpp/LocalAI (8080),
// Ollama, vLLM, text-generation-webui, KoboldCpp, Jan and GopherLLM/RustyLLM.
func localLLMCandidates() []string {
	return []string{
		"http://localhost:1234",
		"http://localhost:8080",
		"http://localhost:11434",
		"http://localhost:8000",
		"http://localhost:5000",
		"http://localhost:5001",
		"http://localhost:1337",
		"http://localhost:8091",
	}
}

func probeLLMCandidate(base, apiKey string) (discoverCandidate, error) {
	c := discoverCandidate{BaseURL: base, ProviderHint: providerHintFromURL(base)}
	tmp := newLMClient(base, "x", "x", apiKey)
	models, err := tmp.listModels(base)
	if err != nil {
		c.OK = false
		c.Error = err.Error()
		return c, err
	}
	c.OK = true
	c.Models = models
	c.RecommendChat, c.RecommendEmbed = recommendModels(models)
	return c, nil
}

func firstModelOr(current string, models []string) string {
	if current != "" {
		for _, m := range models {
			if m == current {
				return current
			}
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return current
}

func maybePreferOfflineLLM(settings *settingsStore) appSettings {
	s := settings.get()
	apiKey := s.OpenAIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	currentBase := s.ChatBase
	if currentBase == "" {
		currentBase = s.BaseURL
	}
	if isLocalLLMBase(currentBase) {
		if _, err := probeLLMCandidate(currentBase, apiKey); err == nil {
			return s
		}
	}

	var preferred discoverCandidate
	found := false
	for _, base := range localLLMCandidates() {
		c, err := probeLLMCandidate(base, apiKey)
		if err == nil && c.OK {
			preferred = c
			found = true
			break
		}
	}
	if !found {
		return s
	}

	chatModel := firstModelOr(s.ChatModel, preferred.RecommendChat)
	if chatModel == "" {
		chatModel = firstModelOr(s.ChatModel, preferred.Models)
	}
	embedModel := firstModelOr(s.EmbedModel, preferred.RecommendEmbed)
	if embedModel == "" {
		embedModel = firstModelOr(s.EmbedModel, preferred.Models)
	}

	settings.mu.Lock()
	settings.s.BaseURL = preferred.BaseURL
	settings.s.ChatBase = preferred.BaseURL
	settings.s.EmbedBase = preferred.BaseURL
	settings.s.ChatModel = chatModel
	settings.s.EmbedModel = embedModel
	_ = settings.saveLocked()
	settings.mu.Unlock()

	log.Printf("LLM preference: switched to local provider %s (%s)", preferred.ProviderHint, preferred.BaseURL)
	return settings.get()
}
