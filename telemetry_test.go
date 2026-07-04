package main

import "testing"

func TestNewRequestTelemetryInitializes(t *testing.T) {
	tel := newRequestTelemetry("req-1", "chat-1", "Wie geht's?")
	if tel.RequestID != "req-1" || tel.ChatID != "chat-1" || tel.Question != "Wie geht's?" {
		t.Fatalf("unexpected initial telemetry: %+v", tel)
	}
	if tel.StartTime.IsZero() {
		t.Error("StartTime should be set on creation")
	}
}

func TestRequestTelemetryRecordTool(t *testing.T) {
	tel := newRequestTelemetry("req-1", "chat-1", "q")
	tel.recordTool(ToolInvocationRecord{ID: "t1", Tool: "wikipedia"})
	tel.recordTool(ToolInvocationRecord{ID: "t2", Tool: "calculate", Deduplicated: true})
	if len(tel.ToolInvocations) != 2 {
		t.Fatalf("expected 2 recorded tool invocations, got %d", len(tel.ToolInvocations))
	}
	if tel.ToolInvocations[1].Tool != "calculate" || !tel.ToolInvocations[1].Deduplicated {
		t.Errorf("unexpected second record: %+v", tel.ToolInvocations[1])
	}
}

func TestRequestTelemetryFinalizeSetsFields(t *testing.T) {
	tel := newRequestTelemetry("req-1", "chat-1", "q")
	tel.finalize(true, "")
	if !tel.Success || tel.Error != "" {
		t.Errorf("success finalize mismatch: success=%v error=%q", tel.Success, tel.Error)
	}
	if tel.TotalMS < 0 {
		t.Errorf("TotalMS should be non-negative, got %d", tel.TotalMS)
	}

	tel2 := newRequestTelemetry("req-2", "chat-1", "q")
	tel2.finalize(false, "boom")
	if tel2.Success || tel2.Error != "boom" {
		t.Errorf("failure finalize mismatch: success=%v error=%q", tel2.Success, tel2.Error)
	}
}
