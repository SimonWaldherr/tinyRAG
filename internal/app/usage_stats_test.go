package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsageStoreRecordAndSummarize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	us := newUsageStore(path)

	now := time.Now()
	us.record(usageRecord{Time: now, Role: "it", Mode: "rag", DurationMS: 100, TokensStreamed: 50, ToolCalls: 1, Success: true, Tools: []string{"wikipedia"}})
	us.record(usageRecord{Time: now, Role: "hr", Mode: "direct", DurationMS: 300, TokensStreamed: 150, Success: false})
	us.record(usageRecord{Time: now.AddDate(0, 0, -60), Role: "it", Mode: "rag", DurationMS: 100, Success: true}) // outside window

	sum := us.summarize(30)
	if sum.TotalRequests != 2 {
		t.Fatalf("expected 2 requests in window, got %d", sum.TotalRequests)
	}
	if sum.SuccessRate != 0.5 {
		t.Errorf("expected success rate 0.5, got %f", sum.SuccessRate)
	}
	if sum.AvgDurationMS != 200 {
		t.Errorf("expected avg duration 200ms, got %d", sum.AvgDurationMS)
	}
	if sum.TotalTokens != 200 {
		t.Errorf("expected 200 tokens, got %d", sum.TotalTokens)
	}
	if sum.PerRole["it"] != 1 || sum.PerRole["hr"] != 1 {
		t.Errorf("unexpected per-role counts: %v", sum.PerRole)
	}
	if sum.PerTool["wikipedia"] != 1 {
		t.Errorf("expected wikipedia tool count 1, got %v", sum.PerTool)
	}
	if len(sum.PerDay) != 1 || sum.PerDay[0].Requests != 2 || sum.PerDay[0].Errors != 1 {
		t.Errorf("unexpected per-day buckets: %+v", sum.PerDay)
	}
}

func TestUsageStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	us1 := newUsageStore(path)
	us1.record(usageRecord{Time: time.Now(), Role: "it", Success: true})
	us1.record(usageRecord{Time: time.Now(), Role: "it", Success: true})

	us2 := newUsageStore(path)
	sum := us2.summarize(7)
	if sum.TotalRequests != 2 {
		t.Fatalf("expected 2 records after reload, got %d", sum.TotalRequests)
	}
}

func TestUsageStoreNilSafe(t *testing.T) {
	var us *usageStore
	us.record(usageRecord{Time: time.Now()}) // must not panic
	us.recordFromTelemetry(nil, "it")
	sum := us.summarize(7)
	if sum.TotalRequests != 0 {
		t.Error("nil store must summarize to zero")
	}
}

func TestUsageRecordFromTelemetry(t *testing.T) {
	us := newUsageStore("") // memory-only
	tel := newRequestTelemetry("r1", "c1", "frage")
	tel.SelectedMode = "rag"
	tel.TokensStreamed = 42
	tel.Success = true
	tel.TotalMS = 123
	tel.recordTool(ToolInvocationRecord{Tool: "wikipedia"})
	tel.recordTool(ToolInvocationRecord{Tool: "calculate", Deduplicated: true}) // must be skipped
	tel.recordTool(ToolInvocationRecord{Tool: "shell", PolicyDecision: "confirmation_required:code_requires_explicit_user_action"})

	us.recordFromTelemetry(tel, "vertrieb")
	sum := us.summarize(7)
	if sum.TotalRequests != 1 || sum.PerRole["vertrieb"] != 1 {
		t.Fatalf("unexpected summary: %+v", sum)
	}
	if sum.TotalToolCalls != 1 || sum.PerTool["wikipedia"] != 1 {
		t.Errorf("non-executed tools must not count: %+v", sum.PerTool)
	}
}
