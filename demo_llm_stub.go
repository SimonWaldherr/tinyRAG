//go:build !demo_llm

package main

import "fmt"

// startEmbeddedDemoLLM is the stub used when tinyRAG is built without the
// demo_llm tag (the default). See demo_llm.go for the real implementation.
func startEmbeddedDemoLLM(modelSelector, addr string) (modelName string, err error) {
	return "", fmt.Errorf("embedded demo LLM support not compiled in; rebuild with: go build -tags demo_llm")
}
