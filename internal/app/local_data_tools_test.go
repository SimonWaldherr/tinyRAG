package app

import (
	"strings"
	"testing"
	"time"
)

func TestStructuredLocalDataToolsExecute(t *testing.T) {
	settings := appSettings{}
	tests := []struct {
		name    string
		request toolRequest
		want    []string
		source  string
	}{
		{
			name: "json path",
			request: toolRequest{Tool: "json_query", Arguments: map[string]any{
				"json": `{"items":[{"name":"Ada"}]}`,
				"path": "items.0.name",
			}},
			want:   []string{`"Ada"`},
			source: "json_query:local",
		},
		{
			name: "line diff",
			request: toolRequest{Tool: "text_diff", Arguments: map[string]any{
				"before": "alpha\nbeta",
				"after":  "alpha\ngamma\nbeta",
			}},
			want:   []string{"Zeilen-Diff:", "+ gamma"},
			source: "text_diff:local",
		},
		{
			name: "regex capture",
			request: toolRequest{Tool: "regex_extract", Arguments: map[string]any{
				"pattern": `ID-(\d+)`,
				"text":    "ID-42 ID-99",
				"limit":   float64(1),
			}},
			want:   []string{"ID-42", "42"},
			source: "regex_extract:local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, source, err := executeToolRequest(tt.request, settings, nil, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if source != tt.source {
				t.Fatalf("source = %q, want %q", source, tt.source)
			}
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Fatalf("result %q does not contain %q", text, want)
				}
			}
		})
	}
}

func TestStructuredLocalDataToolsAreAutonomousAndBounded(t *testing.T) {
	eng := toolPolicyTestEngine(t, nil)
	settings := eng.settings.get()
	for _, role := range allDemoRoles() {
		for _, tool := range []string{"json_query", "text_diff", "regex_extract"} {
			if !canRoleUseTool(role, tool) {
				t.Fatalf("%s must be available as a local tool for role %s", tool, role)
			}
		}
	}
	calls := []XMLToolCall{
		{Name: "json_query", Arguments: map[string]any{"json": `{"ok":true}`}},
		{Name: "text_diff", Arguments: map[string]any{"before": "old", "after": "new"}},
		{Name: "regex_extract", Arguments: map[string]any{"pattern": "x", "text": "x"}},
	}
	for _, call := range calls {
		admitted, decision := eng.admitToolCall(settings, call, false)
		if !decision.Allowed {
			t.Fatalf("%s should be an autonomous local tool: %+v", call.Name, decision)
		}
		if admitted.Name != call.Name {
			t.Fatalf("admitted tool = %q, want %q", admitted.Name, call.Name)
		}
	}

	_, _, err := executeToolRequest(toolRequest{Tool: "regex_extract", Arguments: map[string]any{
		"pattern": "x", "text": "x", "limit": float64(maxLocalRegexMatches + 1),
	}}, settings, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected bounded regex limit error, got %v", err)
	}
}

func TestDateTimeToolSupportsStructuredTimezone(t *testing.T) {
	text, source, err := executeToolRequest(toolRequest{Tool: "datetime", Arguments: map[string]any{
		"timezone": "UTC",
		"format":   "rfc3339",
	}}, appSettings{}, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(source, ":UTC") {
		t.Fatalf("unexpected source %q", source)
	}
	if _, err := time.Parse(time.RFC3339, text); err != nil {
		t.Fatalf("expected RFC3339 datetime, got %q: %v", text, err)
	}
	_, _, err = executeToolRequest(toolRequest{Tool: "datetime", Arguments: map[string]any{
		"timezone": "Not/AZone",
	}}, appSettings{}, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid timezone") {
		t.Fatalf("expected invalid timezone error, got %v", err)
	}
	_, _, err = executeToolRequest(toolRequest{Tool: "url_fetch", Arguments: map[string]any{
		"url": "http://127.0.0.1/private",
	}}, appSettings{}, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected structured URL safety rejection, got %v", err)
	}
}
