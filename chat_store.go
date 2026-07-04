package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Chat history (in-memory)
// ─────────────────────────────────────────────────────────────────────────────

// chatMessage represents a single message in a conversation timeline.
type chatMessage struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking,omitempty"`
	Time     string `json:"time"`
	// Model records which model was used to produce this message (assistant-only).
	Model     string            `json:"model,omitempty"`
	ModelMeta map[string]string `json:"model_meta,omitempty"`
}

// conversation stores metadata and the message history for a chat.
type conversation struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Messages []chatMessage `json:"messages"`
	Created  string        `json:"created"`
	Updated  string        `json:"updated"`
	Persona  string        `json:"persona_id,omitempty"`
}

// chatStore manages in-memory conversations and persists them to disk
// when a path is provided.
type chatStore struct {
	mu    sync.Mutex
	chats map[string]*conversation
	order []string
	path  string
}

// newChatStore initializes a chatStore and loads persisted chats if available.
func newChatStore(path string) *chatStore {
	cs := &chatStore{chats: make(map[string]*conversation), path: path}
	if path != "" {
		if err := cs.load(); err != nil {
			log.Printf("WARN: konnte Chats nicht laden (%v)", err)
		}
	}
	return cs
}

// create makes a new conversation, persists it, and returns it.
func (cs *chatStore) create(title, persona string) *conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := time.Now().Format(time.RFC3339)
	id := fmt.Sprintf("chat-%d", time.Now().UnixNano())
	c := &conversation{ID: id, Title: title, Created: now, Updated: now, Persona: persona}
	cs.chats[id] = c
	cs.order = append(cs.order, id)
	_ = cs.saveLocked()
	return c
}

// get returns a conversation by id or nil if not found.
func (cs *chatStore) get(id string) *conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.chats[id]
}

// addMessage appends a message to the conversation and persists the store.
func (cs *chatStore) addMessage(id, role, content string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.chats[id]
	if !ok {
		return
	}
	now := time.Now().Format(time.RFC3339)
	c.Messages = append(c.Messages, chatMessage{Role: role, Content: content, Time: now})
	c.Updated = now
	if c.Title == "" && role == "user" {
		t := strings.TrimSpace(content)
		t = strings.ReplaceAll(t, "\n", " ")
		if len(t) > 50 {
			t = t[:47] + "..."
		}
		c.Title = t
	}
	_ = cs.saveLocked()
}

// addMessageWithMeta appends a message and also records model metadata for assistant messages.
func (cs *chatStore) addMessageWithMeta(id, role, content, thinking, model string, modelMeta map[string]string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.chats[id]
	if !ok {
		return
	}
	now := time.Now().Format(time.RFC3339)
	msg := chatMessage{Role: role, Content: content, Thinking: thinking, Time: now}
	if model != "" {
		msg.Model = model
	}
	if modelMeta != nil {
		msg.ModelMeta = modelMeta
	}
	c.Messages = append(c.Messages, msg)
	c.Updated = now
	if c.Title == "" && role == "user" {
		t := strings.TrimSpace(content)
		t = strings.ReplaceAll(t, "\n", " ")
		if len(t) > 50 {
			t = t[:47] + "..."
		}
		c.Title = t
	}
	_ = cs.saveLocked()
}

// setPersona assigns a persona to an existing conversation and saves it.
func (cs *chatStore) setPersona(id, persona string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.chats[id]
	if !ok {
		return
	}
	c.Persona = persona
	c.Updated = time.Now().Format(time.RFC3339)
	_ = cs.saveLocked()
}

// list returns conversations in reverse chronological order.
func (cs *chatStore) list() []conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	result := make([]conversation, 0, len(cs.order))
	for i := len(cs.order) - 1; i >= 0; i-- {
		if c, ok := cs.chats[cs.order[i]]; ok {
			result = append(result, *c)
		}
	}
	return result
}

// remove deletes a conversation and persists the updated store.
func (cs *chatStore) remove(id string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.chats[id]; !ok {
		return false
	}
	delete(cs.chats, id)
	for i, oid := range cs.order {
		if oid == id {
			cs.order = append(cs.order[:i], cs.order[i+1:]...)
			break
		}
	}
	_ = cs.saveLocked()
	return true
}

// saveLocked writes the chat store payload to disk and must be called
// with `cs.mu` held.
func (cs *chatStore) saveLocked() error {
	if cs.path == "" {
		return nil
	}
	payload := struct {
		Chats []*conversation `json:"chats"`
	}{}
	for _, id := range cs.order {
		if c, ok := cs.chats[id]; ok {
			payload.Chats = append(payload.Chats, c)
		}
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := cs.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cs.path)
}

// load reads persisted chats from disk into the in-memory store.
func (cs *chatStore) load() error {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var payload struct {
		Chats []conversation `json:"chats"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.chats = make(map[string]*conversation)
	cs.order = nil
	for _, c := range payload.Chats {
		copyC := c
		cs.chats[c.ID] = &copyC
		cs.order = append(cs.order, c.ID)
	}
	return nil
}
