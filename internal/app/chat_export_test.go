package app

import (
	"strings"
	"testing"
)

func testConversation() *conversation {
	return &conversation{
		ID:      "chat-1",
		Title:   "Kubernetes Rollback",
		Created: "2026-07-01T10:00:00Z",
		Messages: []chatMessage{
			{Role: "user", Content: "Wie mache ich ein Rollback?", Time: "2026-07-01T10:00:00Z"},
			{Role: "assistant", Content: "Mit `kubectl rollout undo`.", Time: "2026-07-01T10:00:05Z", Model: "test-model"},
		},
	}
}

func TestExportMarkdown(t *testing.T) {
	md := testConversation().exportMarkdown("tinyRAG")
	for _, want := range []string{
		"# Kubernetes Rollback",
		"## Nutzer —",
		"## Assistent —",
		"kubectl rollout undo",
		"*Modell: test-model*",
		"Exportiert aus tinyRAG",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown export missing %q:\n%s", want, md)
		}
	}
}

func TestExportHTMLEscapesContent(t *testing.T) {
	c := testConversation()
	c.Messages[0].Content = `<script>alert("xss")</script>`
	html := c.exportHTML("tinyRAG")
	if strings.Contains(html, "<script>alert") {
		t.Fatal("HTML export must escape message content")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("expected escaped script tag in output")
	}
	if !strings.Contains(html, "<!doctype html>") {
		t.Error("expected standalone HTML document")
	}
}

func TestExportFilename(t *testing.T) {
	c := testConversation()
	if got := exportFilename(c, "md"); got != "Kubernetes_Rollback.md" {
		t.Errorf("exportFilename = %q", got)
	}
	c.Title = `ein/böser\name:mit*zeichen?`
	got := exportFilename(c, "html")
	if strings.ContainsAny(got, `/\:*?<>|"`) {
		t.Errorf("filename contains unsafe characters: %q", got)
	}
	c.Title = ""
	if got := exportFilename(c, "md"); got != "chat-1.md" {
		t.Errorf("empty title should fall back to id, got %q", got)
	}
}
