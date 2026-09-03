package main

import (
	"path/filepath"
	"testing"
)

func newTestChatHistoryStore(t *testing.T) *chatHistoryStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chathistory_test.db")
	s, err := newChatHistoryStore(path)
	if err != nil {
		t.Fatalf("newChatHistoryStore: %v", err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

func TestChatHistorySaveAndGetRoundTrips(t *testing.T) {
	s := newTestChatHistoryStore(t)
	msgs := []chatHistoryMessage{
		{Role: "user", Content: "Wie geht es der Firma?"},
		{Role: "assistant", Content: "Laut Q1-Bericht gut.", Citations: []sourceInfo{{SourceID: "a", Marker: 1}}},
	}
	ok, err := s.save("alice", "conv-1", "Firmenfrage", "chat", msgs)
	if err != nil || !ok {
		t.Fatalf("save: ok=%v err=%v", ok, err)
	}
	got, ok, err := s.get("alice", "conv-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Title != "Firmenfrage" || len(got.Messages) != 2 {
		t.Fatalf("unexpected conversation: %+v", got)
	}
	if got.Messages[1].Citations[0].SourceID != "a" {
		t.Fatalf("citations did not round-trip: %+v", got.Messages[1])
	}
	if got.Mode != "chat" {
		t.Fatalf("want mode=chat, got %q", got.Mode)
	}
}

// TestChatHistoryModeRoundTripsAndDefaults covers normalizeConversationMode:
// "agent" is preserved, an empty/garbage value defaults to "chat" (both for
// a pre-mode-column row and for a client sending something unexpected), and
// list() reports the same normalized value get() does.
func TestChatHistoryModeRoundTripsAndDefaults(t *testing.T) {
	s := newTestChatHistoryStore(t)
	if _, err := s.save("alice", "a1", "Agent-Sache", "agent", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save("alice", "c1", "Chat-Sache", "chat", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save("alice", "garbage-mode", "Kaputter Modus", "not-a-real-mode", nil); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.get("alice", "a1")
	if err != nil || got.Mode != "agent" {
		t.Fatalf("want mode=agent, got %q (err=%v)", got.Mode, err)
	}
	got, _, err = s.get("alice", "garbage-mode")
	if err != nil || got.Mode != "chat" {
		t.Fatalf("want an unrecognized mode to default to chat, got %q (err=%v)", got.Mode, err)
	}

	list, err := s.list("alice")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, m := range list {
		byID[m.ID] = m.Mode
	}
	if byID["a1"] != "agent" || byID["c1"] != "chat" || byID["garbage-mode"] != "chat" {
		t.Fatalf("list() mode values don't match get(): %+v", byID)
	}
}

// TestChatHistoryCrossUserIsolation is the critical guarantee behind
// this whole feature: one person must never be able to read, rename or
// delete another person's conversation, even knowing (or guessing) its
// exact ID.
func TestChatHistoryCrossUserIsolation(t *testing.T) {
	s := newTestChatHistoryStore(t)
	if _, err := s.save("alice", "shared-id", "Alice's Sache", "chat", []chatHistoryMessage{{Role: "user", Content: "geheim"}}); err != nil {
		t.Fatalf("alice save: %v", err)
	}

	t.Run("get returns not-found for a non-owner", func(t *testing.T) {
		_, ok, err := s.get("bob", "shared-id")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if ok {
			t.Fatal("bob must not be able to read alice's conversation")
		}
	})

	t.Run("list never includes another user's conversations", func(t *testing.T) {
		list, err := s.list("bob")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("bob's list must be empty, got %+v", list)
		}
	})

	t.Run("save with the same id under a different owner does not hijack or overwrite it", func(t *testing.T) {
		ok, err := s.save("bob", "shared-id", "Bob's Versuch", "chat", []chatHistoryMessage{{Role: "user", Content: "uebernahme"}})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if ok {
			t.Fatal("bob must not be able to claim alice's existing conversation id")
		}
		// Alice's original content must be completely unchanged.
		got, ok, err := s.get("alice", "shared-id")
		if err != nil || !ok {
			t.Fatalf("alice's conversation should still exist: ok=%v err=%v", ok, err)
		}
		if got.Title != "Alice's Sache" || got.Messages[0].Content != "geheim" {
			t.Fatalf("alice's conversation was modified by bob's save attempt: %+v", got)
		}
	})

	t.Run("rename by a non-owner is a no-op, not a takeover", func(t *testing.T) {
		ok, err := s.rename("bob", "shared-id", "Umbenannt von Bob")
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		if ok {
			t.Fatal("bob must not be able to rename alice's conversation")
		}
		got, _, _ := s.get("alice", "shared-id")
		if got.Title != "Alice's Sache" {
			t.Fatalf("alice's title must be unchanged, got %q", got.Title)
		}
	})

	t.Run("delete by a non-owner leaves the conversation intact", func(t *testing.T) {
		if err := s.delete("bob", "shared-id"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		_, ok, _ := s.get("alice", "shared-id")
		if !ok {
			t.Fatal("bob's delete attempt must not remove alice's conversation")
		}
	})

	t.Run("deleteAll for one owner never touches another owner's rows", func(t *testing.T) {
		if err := s.deleteAll("bob"); err != nil {
			t.Fatalf("deleteAll: %v", err)
		}
		_, ok, _ := s.get("alice", "shared-id")
		if !ok {
			t.Fatal("bob's deleteAll must not remove alice's conversation")
		}
	})

	t.Run("the true owner can still read, rename and delete normally", func(t *testing.T) {
		ok, err := s.rename("alice", "shared-id", "Alice's Sache (bearbeitet)")
		if err != nil || !ok {
			t.Fatalf("alice rename: ok=%v err=%v", ok, err)
		}
		if err := s.delete("alice", "shared-id"); err != nil {
			t.Fatalf("alice delete: %v", err)
		}
		_, ok, _ = s.get("alice", "shared-id")
		if ok {
			t.Fatal("alice's own delete should have removed it")
		}
	})
}

func TestChatHistoryListOrderedByRecency(t *testing.T) {
	s := newTestChatHistoryStore(t)
	if _, err := s.save("alice", "c1", "Erste", "chat", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save("alice", "c2", "Zweite", "chat", nil); err != nil {
		t.Fatal(err)
	}
	// Re-saving c1 should bump it back to the front (most recently updated).
	if _, err := s.save("alice", "c1", "Erste (aktualisiert)", "chat", nil); err != nil {
		t.Fatal(err)
	}
	list, err := s.list("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "c1" {
		t.Fatalf("want c1 first after being re-saved, got %+v", list)
	}
}
