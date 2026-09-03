package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func plannerTools() []toolDef {
	return []toolDef{
		{Name: "wikipedia", Description: "Wikipedia-Suche", ParamHint: "Artikel"},
		{Name: "calculate", Description: "Rechner", ParamHint: "Ausdruck"},
	}
}

func TestParsePlannedStepsValid(t *testing.T) {
	raw := `Hier der Plan: [{"tool":"wikipedia","query":"Albert Einstein","reason":"Fakten"},{"tool":"calculate","query":"2+2"}]`
	steps, err := parsePlannedSteps(raw, plannerTools(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 2 || steps[0].Tool != "wikipedia" || steps[1].Query != "2+2" {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

func TestParsePlannedStepsStructuredArguments(t *testing.T) {
	tools := []toolDef{{Name: "lookup", InputSchema: &JSONSchema{Type: "object", Required: []string{"id", "region"}}}}
	steps, err := parsePlannedSteps(`[{"tool":"lookup","arguments":{"id":"42","region":"eu"},"reason":"record"}]`, tools, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 1 || steps[0].Arguments["id"] != "42" || steps[0].Query != "" {
		t.Fatalf("unexpected structured plan: %+v", steps)
	}
}

func TestParsePlannedStepsFiltersUnknownAndCaps(t *testing.T) {
	raw := `[{"tool":"hackertool","query":"x"},{"tool":"wikipedia","query":"A"},{"tool":"wikipedia","query":"B"},{"tool":"calculate","query":"C"}]`
	steps, err := parsePlannedSteps(raw, plannerTools(), 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected cap at 2 steps, got %d", len(steps))
	}
	for _, s := range steps {
		if s.Tool == "hackertool" {
			t.Error("unknown tool must be filtered out")
		}
	}
}

func TestParsePlannedStepsEmptyPlan(t *testing.T) {
	steps, err := parsePlannedSteps("[]", plannerTools(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("expected empty plan, got %+v", steps)
	}
}

func TestParsePlannedStepsGarbage(t *testing.T) {
	if _, err := parsePlannedSteps("kein json", plannerTools(), 3); err == nil {
		t.Fatal("expected parse error for garbage input")
	}
}

func TestPlanToolStepsViaMockLM(t *testing.T) {
	lm := &mockLMProvider{response: `[{"tool":"calculate","query":"40+2","reason":"rechnen"}]`}
	steps, err := planToolSteps(context.Background(), lm, "Was ist 40+2?", plannerTools(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 1 || steps[0].Tool != "calculate" {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

func TestPlanToolStepsRejectsOversizedOutput(t *testing.T) {
	lm := &mockLMProvider{response: strings.Repeat("x", maxPlannerOutputBytes+1)}
	if _, err := planToolSteps(context.Background(), lm, "question", plannerTools(), 3); err == nil {
		t.Fatal("planner output over the configured limit must fail closed")
	}
}

func TestBuildPlannerPromptBoundsQuestion(t *testing.T) {
	prompt := buildPlannerPrompt(strings.Repeat("q", maxPlannerQuestionRunes+100), plannerTools(), 3)
	if len([]rune(prompt)) > maxPlannerQuestionRunes+1600 {
		t.Fatalf("planner prompt retained an oversized question: %d runes", len([]rune(prompt)))
	}
}

func TestEnginePlannerPhaseExecutesTools(t *testing.T) {
	// The mock LM returns the plan for the planner call AND the final answer
	// for the streaming round (same canned response, plan is valid JSON and
	// contains no <tool> blocks, so round 0 just streams it as text).
	eng, _ := buildTestEngine(`[{"tool":"calculate","query":"6*7","reason":"rechnen"}]`)

	rec := httptest.NewRecorder()
	rf := &recordingFlusher{rec}
	sw := &sseWriter{w: rec, flusher: rf}
	tel := newRequestTelemetry("test-plan", "chat-1", "Was ist 6*7?")

	req := EngineRequest{
		RequestID: "test-plan",
		Question:  "Was ist 6*7?",
		Messages:  []chatMsg{{Role: "user", Content: "Was ist 6*7?"}},
		PlanFirst: true,
	}
	if _, err := eng.Run(context.Background(), req, sw, tel); err != nil {
		t.Fatalf("engine run failed: %v", err)
	}

	events := collectSSEFromString(rec.Body.String())
	if len(events.events["plan"]) != 1 {
		t.Fatalf("expected one plan event, got %v", events.events["plan"])
	}
	if len(events.events["tool_start"]) != 1 || !strings.Contains(events.events["tool_start"][0], "calculate") {
		t.Fatalf("expected planned calculate tool_start, got %v", events.events["tool_start"])
	}
	if len(events.events["tool_result"]) != 1 {
		t.Fatalf("expected one tool_result, got %v", events.events["tool_result"])
	}
	if len(tel.ToolInvocations) != 1 || tel.ToolInvocations[0].Tool != "calculate" {
		t.Fatalf("expected telemetry record for planned tool, got %+v", tel.ToolInvocations)
	}
}

func TestEnginePlannerEmptyPlanFallsThrough(t *testing.T) {
	eng, _ := buildTestEngine(`[]`)
	rec := httptest.NewRecorder()
	rf := &recordingFlusher{rec}
	sw := &sseWriter{w: rec, flusher: rf}
	tel := newRequestTelemetry("test-noplan", "chat-1", "Hallo")

	req := EngineRequest{
		RequestID: "test-noplan",
		Question:  "Hallo",
		Messages:  []chatMsg{{Role: "user", Content: "Hallo"}},
		PlanFirst: true,
	}
	if _, err := eng.Run(context.Background(), req, sw, tel); err != nil {
		t.Fatalf("engine run failed: %v", err)
	}
	events := collectSSEFromString(rec.Body.String())
	if len(events.events["plan"]) != 0 {
		t.Errorf("empty plan must not emit a plan event, got %v", events.events["plan"])
	}
	if len(events.events["tool_start"]) != 0 {
		t.Errorf("empty plan must not start tools, got %v", events.events["tool_start"])
	}
}
