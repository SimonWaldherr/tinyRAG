package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type inferenceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f inferenceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func inferenceTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestNormalizeInferenceAPI(t *testing.T) {
	cases := map[string]string{
		"":                  inferenceAPIAuto,
		"detect":            inferenceAPIAuto,
		"openai-compatible": inferenceAPIOpenAI,
		"v1":                inferenceAPIOpenAI,
		"ollama-native":     inferenceAPIOllama,
		"native":            inferenceAPIOllama,
		"unknown-profile":   inferenceAPIAuto,
	}
	for input, want := range cases {
		if got := normalizeInferenceAPI(input); got != want {
			t.Errorf("normalizeInferenceAPI(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestInferenceAPIInference(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11434":       inferenceAPIOllama,
		"http://ollama:11434":          inferenceAPIOllama,
		"http://localhost:11434/v1":    inferenceAPIOpenAI,
		"http://localhost:8091":        inferenceAPIOpenAI,
		"https://inference.example/v1": inferenceAPIOpenAI,
	}
	for input, want := range cases {
		if got := inferInferenceAPIFromBase(input); got != want {
			t.Errorf("inferInferenceAPIFromBase(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNativeOllamaProtocol(t *testing.T) {
	var gotEmbed bool
	var gotChat bool
	client := newLMClientWithAPI("http://inference.test", "nomic-embed-text", "qwen3:4b", "", inferenceAPIOllama)
	client.http = &http.Client{Transport: inferenceRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/tags":
			return inferenceTestResponse(r, http.StatusOK, `{"models":[{"name":"qwen3:4b"},{"model":"nomic-embed-text"}]}`), nil
		case "/api/embed":
			gotEmbed = true
			var req ollamaEmbedRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			if req.Model != "nomic-embed-text" || len(req.Input) != 2 {
				return nil, io.ErrUnexpectedEOF
			}
			return inferenceTestResponse(r, http.StatusOK, `{"embeddings":[[1,0],[0,1]]}`), nil
		case "/api/chat":
			gotChat = true
			var req ollamaChatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			if req.Model != "qwen3:4b" || !req.Stream || len(req.Messages) != 2 {
				return nil, io.ErrUnexpectedEOF
			}
			return inferenceTestResponse(r, http.StatusOK, `{"message":{"thinking":"check"},"done":false}`+"\n"+
				`{"message":{"content":"Hallo"},"done":false}`+"\n"+
				`{"message":{"content":" Welt"},"done":true}`+"\n"), nil
		default:
			return inferenceTestResponse(r, http.StatusNotFound, ""), nil
		}
	})}
	models, err := client.listModels("")
	if err != nil {
		t.Fatalf("list native Ollama models: %v", err)
	}
	if strings.Join(models, ",") != "nomic-embed-text,qwen3:4b" {
		t.Fatalf("unexpected models: %v", models)
	}
	vecs, err := client.embed([]string{"a", "b"})
	if err != nil || len(vecs) != 2 || len(vecs[0]) != 2 {
		t.Fatalf("native embeddings: vecs=%v err=%v", vecs, err)
	}
	var answer, thinking strings.Builder
	if err := client.chatStreamDetailed(context.Background(), "system", []chatMsg{{Role: "user", Content: "hi"}}, &answer, &thinking); err != nil {
		t.Fatalf("native chat: %v", err)
	}
	if answer.String() != "Hallo Welt" || thinking.String() != "check" {
		t.Fatalf("unexpected native stream answer=%q thinking=%q", answer.String(), thinking.String())
	}
	if !gotEmbed || !gotChat {
		t.Fatalf("expected native embed and chat calls, embed=%v chat=%v", gotEmbed, gotChat)
	}
}

func TestAutoFallsBackToOllamaProtocol(t *testing.T) {
	client := newLMClient("http://inference.test", "embed", "chat", "")
	client.http = &http.Client{Transport: inferenceRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/models":
			return inferenceTestResponse(r, http.StatusNotFound, ""), nil
		case "/api/tags":
			return inferenceTestResponse(r, http.StatusOK, `{"models":[{"name":"local-model"}]}`), nil
		case "/api/embed":
			return inferenceTestResponse(r, http.StatusOK, `{"embeddings":[[0.5,0.5]]}`), nil
		default:
			return inferenceTestResponse(r, http.StatusNotFound, ""), nil
		}
	})}
	models, err := client.listModels("")
	if err != nil {
		t.Fatalf("auto discovery fallback: %v", err)
	}
	if len(models) != 1 || models[0] != "local-model" {
		t.Fatalf("unexpected fallback models: %v", models)
	}
	if got := client.inferenceAPI(); got != inferenceAPIOllama {
		t.Fatalf("detected protocol = %q, want %q", got, inferenceAPIOllama)
	}
	if _, err := client.embed([]string{"x"}); err != nil {
		t.Fatalf("auto native embedding after detection: %v", err)
	}
}
