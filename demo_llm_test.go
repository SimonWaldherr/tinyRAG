//go:build demo_llm

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDemoLLMModelExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny-model.Q4_K_M.gguf")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	resolved, name, err := resolveDemoLLMModel(path)
	if err != nil {
		t.Fatalf("resolveDemoLLMModel failed: %v", err)
	}
	if resolved != path {
		t.Errorf("expected resolved path %q, got %q", path, resolved)
	}
	if name != "tiny-model.Q4_K_M" {
		t.Errorf("expected name derived from filename, got %q", name)
	}
}

func TestResolveDemoLLMModelMissingFile(t *testing.T) {
	if _, _, err := resolveDemoLLMModel(filepath.Join(t.TempDir(), "does-not-exist.gguf")); err == nil {
		t.Fatal("expected an error for a missing model file")
	}
}

func TestResolveDemoLLMModelEmptySelector(t *testing.T) {
	if _, _, err := resolveDemoLLMModel(""); err == nil {
		t.Fatal("expected an error for an empty selector")
	}
	if _, _, err := resolveDemoLLMModel("   "); err == nil {
		t.Fatal("expected an error for a whitespace-only selector")
	}
}

func TestResolveDemoLLMModelAutoWithNoModels(t *testing.T) {
	// "auto" scans GopherLLM's default model directory; in this sandbox
	// that directory almost certainly has no GGUF models, so we only assert
	// the function fails with a clear, actionable error rather than panicking.
	if _, _, err := resolveDemoLLMModel("auto"); err == nil {
		t.Log("auto-discovery unexpectedly found a usable model in this environment — not a failure, just noting it")
	} else if !strings.Contains(err.Error(), "demo LLM") {
		t.Errorf("expected a demo-LLM-scoped error message, got: %v", err)
	}
}

func TestWaitForDemoLLMTimesOutOnUnreachableAddr(t *testing.T) {
	// Port 1 is a reserved/unlikely-to-be-listening port; use a short
	// timeout so the test itself stays fast.
	err := waitForDemoLLM("127.0.0.1:1", 300_000_000) // 300ms in time.Duration units
	if err == nil {
		t.Fatal("expected a timeout error for an unreachable address")
	}
}
