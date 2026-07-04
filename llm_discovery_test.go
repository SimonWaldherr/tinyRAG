package main

import "testing"

func TestProviderHintFromURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11434":                    "Ollama",
		"http://127.0.0.1:1234":                     "LM Studio",
		"http://localhost:8080":                     "llmster",
		"http://localhost:9999":                     "Local LLM",
		"https://api.openai.com/v1":                 "OpenAI",
		"https://eu.api.openai.com":                 "OpenAI",
		"https://api.anthropic.com":                 "Anthropic",
		"https://api.mistral.ai":                    "Mistral AI",
		"https://api.groq.com":                      "Groq",
		"https://api.deepseek.com":                  "DeepSeek",
		"https://openrouter.ai/api/v1":              "OpenRouter",
		"https://generativelanguage.googleapis.com": "Google Gemini",
		"https://my-remote-server.example.com":      "OpenAI-compatible",
		"http://10.0.0.5:11434":                     "Ollama", // port-based fallback for non-local hosts
		"http://localhost:8000":                     "vLLM",
		"http://127.0.0.1:5000":                     "text-generation-webui",
		"http://localhost:5001":                     "KoboldCpp",
		"http://localhost:1337":                     "Jan",
		"http://10.0.0.5:8000":                      "vLLM", // port-based fallback for non-local hosts
	}
	for in, want := range cases {
		if got := providerHintFromURL(in); got != want {
			t.Errorf("providerHintFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderHintFromURLInvalidInput(t *testing.T) {
	// Must not panic on garbage input; falls back to the generic hint.
	if got := providerHintFromURL("::not a url::"); got == "" {
		t.Errorf("expected a non-empty fallback hint, got %q", got)
	}
}

func TestRecommendModels(t *testing.T) {
	models := []string{
		"text-embedding-nomic-embed-text-v1.5",
		"mistralai/ministral-3-14b-reasoning",
		"bge-large-en",
		"llama-3.1-8b-instruct",
		"random-unrelated-model",
	}
	chat, embed := recommendModels(models)
	if len(embed) != 2 {
		t.Errorf("expected 2 embedding models, got %v", embed)
	}
	if len(chat) != 2 {
		t.Errorf("expected 2 chat models, got %v", chat)
	}
	for _, m := range embed {
		if m == "random-unrelated-model" {
			t.Error("unrelated model must not be classified as embedding")
		}
	}
}

func TestRecommendModelsCapsAtEight(t *testing.T) {
	var models []string
	for i := 0; i < 20; i++ {
		models = append(models, "llama-variant")
	}
	chat, _ := recommendModels(models)
	if len(chat) != 8 {
		t.Errorf("expected chat list capped at 8, got %d", len(chat))
	}
}

func TestIsLocalLLMBase(t *testing.T) {
	if !isLocalLLMBase("http://localhost:1234") {
		t.Error("localhost should be local")
	}
	if !isLocalLLMBase("http://127.0.0.1:11434") {
		t.Error("127.0.0.1 should be local")
	}
	if isLocalLLMBase("https://api.openai.com") {
		t.Error("remote host should not be local")
	}
}

func TestLocalLLMCandidates(t *testing.T) {
	candidates := localLLMCandidates()
	if len(candidates) != 7 {
		t.Fatalf("expected 7 candidates, got %d", len(candidates))
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if !isLocalLLMBase(c) {
			t.Errorf("candidate %q should be recognized as local", c)
		}
		if seen[c] {
			t.Errorf("duplicate candidate %q", c)
		}
		seen[c] = true
		if providerHintFromURL(c) == "Local LLM" {
			t.Errorf("candidate %q should resolve to a specific provider hint, not the generic fallback", c)
		}
	}
}

func TestFirstModelOr(t *testing.T) {
	models := []string{"a", "b", "c"}
	if got := firstModelOr("b", models); got != "b" {
		t.Errorf("existing current model should be kept, got %q", got)
	}
	if got := firstModelOr("not-in-list", models); got != "a" {
		t.Errorf("unknown current model should fall back to first available, got %q", got)
	}
	if got := firstModelOr("keep-me", nil); got != "keep-me" {
		t.Errorf("empty models list should keep current, got %q", got)
	}
	if got := firstModelOr("", nil); got != "" {
		t.Errorf("no current and no models should return empty, got %q", got)
	}
}

func TestMustJSON(t *testing.T) {
	got := mustJSON(map[string]any{"a": 1})
	if got != `{"a":1}` {
		t.Errorf("unexpected mustJSON output: %q", got)
	}
	// Unmarshalable values (channels) should not panic, just yield empty string.
	if got := mustJSON(make(chan int)); got != "" {
		t.Errorf("expected empty string for unmarshalable value, got %q", got)
	}
}
