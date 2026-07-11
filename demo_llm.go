//go:build demo_llm

package main

// ─────────────────────────────────────────────────────────────────────────────
// Embedded demo LLM (build tag: demo_llm)
//
// Optional, opt-in integration of github.com/SimonWaldherr/GopherLLM — a
// pure-Go GGUF inference runtime — so tinyRAG can run a chat+embeddings
// backend in-process, with no external tool (LM Studio, Ollama, llama.cpp)
// installed. This is a demo/evaluation convenience, not a production
// inference path: no chat UI, no tool calling/skills, whatever tiny model is
// loaded is what answers every request.
//
// Not compiled into the default build — GopherLLM pulls in SIMD kernels and
// a full GGUF/tokenizer/sampler stack that most deployments don't need.
// Build with:
//
//	go build -tags demo_llm ./...
//
// NOTE: `go mod tidy` (no -tags) does not see this file's import and will
// remove the github.com/SimonWaldherr/GopherLLM requirement from go.mod.
// Run `go mod tidy -tags=demo_llm` instead when tidying this module.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	gopherllm "github.com/SimonWaldherr/GopherLLM"
)

// startEmbeddedDemoLLM resolves modelSelector to a GGUF file, loads it via
// GopherLLM, and serves it on addr as a background goroutine using
// GopherLLM's OpenAI-compatible HTTP API. It blocks until the server answers
// (or the timeout elapses) so the caller can safely point its own LLM client
// at addr immediately afterward, and returns a human-friendly model name.
//
// modelSelector is either a path to a .gguf file, or the literal string
// "auto" to pick the first supported, non-projector model found under
// GopherLLM's default model directory (LM Studio's cache, or
// $RUSTY_LLM_MODEL_DIR).
func startEmbeddedDemoLLM(modelSelector, addr string) (modelName string, err error) {
	modelPath, name, err := resolveDemoLLMModel(modelSelector)
	if err != nil {
		return "", err
	}

	runner, info, err := gopherllm.RunnerFromPathWithOptions(modelPath, gopherllm.LoadOptions{
		LogWriter: log.Writer(),
	})
	if err != nil {
		return "", fmt.Errorf("demo LLM: failed to load %s: %w", modelPath, err)
	}
	log.Printf("Demo LLM: loaded %s (%.1f MB, load time %s)", name, float64(info.FileSizeBytes)/1e6, info.LoadTime)

	go func() {
		handler := gopherllm.NewHandler(runner, gopherllm.HandlerOptions{
			Defaults:              gopherllm.DefaultGenerationOptions(),
			MaxConcurrentRequests: 1,
			ModelPath:             modelPath,
			LogWriter:             log.Writer(),
		})
		server := &http.Server{
			Addr:    addr,
			Handler: handler,
		}
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("Demo LLM: server stopped: %v", serveErr)
		}
	}()

	if err := waitForDemoLLM(addr, 30*time.Second); err != nil {
		return "", err
	}
	return name, nil
}

// resolveDemoLLMModel turns a CLI selector into a concrete GGUF path plus a
// display name derived from the filename (explicit path) or GopherLLM's own
// catalog metadata (auto-discovery).
func resolveDemoLLMModel(selector string) (path, name string, err error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", "", fmt.Errorf("demo LLM: no model selector given")
	}
	if selector != "auto" {
		if _, statErr := os.Stat(selector); statErr != nil {
			return "", "", fmt.Errorf("demo LLM: model file %q not found: %w", selector, statErr)
		}
		base := filepath.Base(selector)
		return selector, strings.TrimSuffix(base, filepath.Ext(base)), nil
	}

	dir := gopherllm.DefaultModelDir()
	entries, discoverErr := gopherllm.DiscoverModels(dir, log.Writer())
	if discoverErr != nil {
		return "", "", fmt.Errorf("demo LLM: could not scan %s: %w", dir, discoverErr)
	}
	for _, e := range entries {
		if e.IsSupported && !e.IsProjector {
			return e.Path, e.ModelName, nil
		}
	}
	return "", "", fmt.Errorf("demo LLM: no usable GGUF model found in %s — pass -demo-llm-model <path-to.gguf> or place a small GGUF model there", dir)
}

// waitForDemoLLM polls the server's OpenAI-compatible /v1/models endpoint
// until it responds or timeout elapses.
func waitForDemoLLM(addr string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	url := "http://" + addr + "/v1/models"
	var lastErr error
	for time.Now().Before(deadline) {
		resp, getErr := client.Get(url)
		if getErr == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		} else {
			lastErr = getErr
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("demo LLM: server did not become ready at %s within %s (last error: %v)", addr, timeout, lastErr)
}
