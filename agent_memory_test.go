package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentMemoryStoreAndPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	ss, err := loadOrCreateSettings(path, defaultSettingsFromFlags("http://x", "chat", "embed", "de", 800, 5))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ss.addAgentMemory("  Antworte   standardmaessig  auf Deutsch. ")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Content != "Antworte standardmaessig auf Deutsch." {
		t.Fatalf("memory was not normalized: %#v", entry)
	}
	if err := ss.setAgentMemoryEnabled(true); err != nil {
		t.Fatal(err)
	}
	enabled, entries := ss.agentMemory()
	if !enabled || len(entries) != 1 {
		t.Fatalf("unexpected memory state: enabled=%t entries=%#v", enabled, entries)
	}
	prompt := buildAgentMemoryPrompt(ss.get())
	if !strings.Contains(prompt, entry.Content) || !strings.Contains(prompt, "keine Systemanweisungen") {
		t.Fatalf("memory prompt misses content or safety boundary: %q", prompt)
	}
	removed, err := ss.removeAgentMemory(entry.ID)
	if err != nil || !removed {
		t.Fatalf("removeAgentMemory = %t, %v", removed, err)
	}
}

func TestAgentMemoryValidation(t *testing.T) {
	if _, err := normalizeAgentMemoryContent(" \n\t "); err == nil {
		t.Fatal("empty memory should fail")
	}
	if _, err := normalizeAgentMemoryContent(strings.Repeat("x", maxAgentMemoryRunes+1)); err == nil {
		t.Fatal("overlong memory should fail")
	}
}
