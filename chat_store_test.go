package main

import (
	"path/filepath"
	"testing"
)

func TestChatStoreCreateGetList(t *testing.T) {
	cs := newChatStore("")
	c := cs.create("", "persona-default")
	if c.ID == "" {
		t.Fatal("expected a generated chat id")
	}
	got := cs.get(c.ID)
	if got == nil || got.Persona != "persona-default" {
		t.Fatalf("unexpected get result: %+v", got)
	}
	if cs.get("does-not-exist") != nil {
		t.Error("unknown id should return nil")
	}
	list := cs.list()
	if len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("unexpected list result: %+v", list)
	}
}

func TestChatStoreAddMessageSetsTitleFromFirstUserMessage(t *testing.T) {
	cs := newChatStore("")
	c := cs.create("", "")
	cs.addMessage(c.ID, "user", "Wie funktioniert Kubernetes Rollback im Detail und mit Beispielen?")
	updated := cs.get(c.ID)
	if updated.Title == "" {
		t.Fatal("title should be derived from the first user message")
	}
	if len(updated.Title) > 50 {
		t.Errorf("title should be truncated to ~50 chars, got %d: %q", len(updated.Title), updated.Title)
	}
	if len(updated.Messages) != 1 || updated.Messages[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", updated.Messages)
	}
}

func TestChatStoreAddMessageWithMeta(t *testing.T) {
	cs := newChatStore("")
	c := cs.create("Title", "")
	cs.addMessageWithMeta(c.ID, "assistant", "Antworttext", "denke nach...", "gpt-x", map[string]string{"base_url": "http://x"})
	updated := cs.get(c.ID)
	if len(updated.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(updated.Messages))
	}
	msg := updated.Messages[0]
	if msg.Thinking != "denke nach..." || msg.Model != "gpt-x" || msg.ModelMeta["base_url"] != "http://x" {
		t.Errorf("unexpected message metadata: %+v", msg)
	}
}

func TestChatStoreSetPersona(t *testing.T) {
	cs := newChatStore("")
	c := cs.create("", "persona-default")
	cs.setPersona(c.ID, "persona-formal")
	if got := cs.get(c.ID); got.Persona != "persona-formal" {
		t.Errorf("expected updated persona, got %q", got.Persona)
	}
}

func TestChatStoreRemove(t *testing.T) {
	cs := newChatStore("")
	c := cs.create("", "")
	if !cs.remove(c.ID) {
		t.Fatal("remove should report true for an existing chat")
	}
	if cs.get(c.ID) != nil {
		t.Error("chat should be gone after remove")
	}
	if cs.remove("nope") {
		t.Error("removing an unknown id should report false")
	}
}

func TestChatStoreListOrderIsReverseChronological(t *testing.T) {
	cs := newChatStore("")
	first := cs.create("first", "")
	second := cs.create("second", "")
	list := cs.list()
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("expected most-recent-first order, got %+v", list)
	}
}

func TestChatStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats.json")
	cs1 := newChatStore(path)
	c := cs1.create("Persisted Chat", "persona-default")
	cs1.addMessage(c.ID, "user", "Hallo")

	cs2 := newChatStore(path)
	got := cs2.get(c.ID)
	if got == nil || len(got.Messages) != 1 {
		t.Fatalf("expected chat to survive reload, got %+v", got)
	}
}
