package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withTestSessions(t *testing.T, sessions map[string]sessionClaims) {
	t.Helper()
	sessionStoreMu.Lock()
	previous := sessionStore
	sessionStore = sessions
	sessionStoreMu.Unlock()
	t.Cleanup(func() {
		sessionStoreMu.Lock()
		sessionStore = previous
		sessionStoreMu.Unlock()
	})
}

func resetActiveAgentRuns(t *testing.T) {
	t.Helper()
	agentRunRegistry.Lock()
	previousRuns := agentRunRegistry.runs
	previousStarted := agentRunRegistry.startedTotal
	previousFinished := agentRunRegistry.finishedTotal
	agentRunRegistry.runs = map[string]*trackedAgentRun{}
	agentRunRegistry.startedTotal = 0
	agentRunRegistry.finishedTotal = 0
	agentRunRegistry.Unlock()
	previousSequence := atomic.LoadUint64(&agentRunSequence)
	atomic.StoreUint64(&agentRunSequence, 0)
	t.Cleanup(func() {
		agentRunRegistry.Lock()
		agentRunRegistry.runs = previousRuns
		agentRunRegistry.startedTotal = previousStarted
		agentRunRegistry.finishedTotal = previousFinished
		agentRunRegistry.Unlock()
		atomic.StoreUint64(&agentRunSequence, previousSequence)
	})
}

func TestSessionPresenceGroupsDevicesByDirectoryID(t *testing.T) {
	now := time.Now().Unix()
	withTestSessions(t, map[string]sessionClaims{
		"a": {User: "Ada CN", DisplayName: "Ada Lovelace", AccountName: "ada", Mail: "ada@example.test", Department: "IT", DirectoryID: "same-person", IsAdmin: true, LastSeenAt: now, Expires: now + 3600},
		"b": {User: "Ada Updated CN", DisplayName: "Ada Lovelace", AccountName: "ada", Mail: "ada.new@example.test", DirectoryID: "same-person", LastSeenAt: now - 30, Expires: now + 3600},
		"c": {User: "Grace CN", DisplayName: "Grace Hopper", AccountName: "grace", DirectoryID: "other-person", LastSeenAt: now - int64(sessionActiveWindow.Seconds()) - 1, Expires: now + 3600},
	})

	stats := sessionPresenceSnapshot()
	if stats.SignedInSessions != 3 || stats.SignedInUsers != 2 {
		t.Fatalf("signed-in stats = %+v, want 3 sessions / 2 users", stats)
	}
	if stats.ActiveSessions != 2 || stats.ActiveUsers != 1 {
		t.Fatalf("active stats = %+v, want 2 sessions / 1 user", stats)
	}
	if len(stats.Users) != 2 || stats.Users[0].DisplayName != "Ada Lovelace" || stats.Users[0].Sessions != 2 || !stats.Users[0].Active {
		t.Fatalf("want deduplicated active Ada first, got %+v", stats.Users)
	}
	if strings.Contains(strings.Join([]string{stats.Users[0].User, stats.Users[0].Mail, stats.Users[0].AccountName}, " "), "same-person") {
		t.Fatalf("directory ID must not be exposed in user status: %+v", stats.Users[0])
	}
}

func TestActiveAgentRunTracksSubagentsAndTools(t *testing.T) {
	resetActiveAgentRuns(t)
	ctx, finish := beginActiveAgentRun(context.Background(), "ada@example.test", "local")
	stopTool := trackActiveAgentTool(ctx, "search_knowledge_base")
	stopSubagent := trackActiveSubagent(ctx)

	stats := agentActivitySnapshot()
	if stats.ActiveRuns != 1 || stats.ActiveSubagents != 1 || stats.ActiveToolCalls != 1 || len(stats.Runs) != 1 {
		t.Fatalf("active agent stats = %+v, want one run/subagent/tool", stats)
	}
	if stats.Runs[0].User != "ada@example.test" || stats.Runs[0].Profile != "local" || len(stats.Runs[0].ActiveTools) != 1 || stats.Runs[0].ActiveTools[0] != "search_knowledge_base" {
		t.Fatalf("run details = %+v", stats.Runs[0])
	}

	stopTool()
	stopSubagent()
	finish()
	stats = agentActivitySnapshot()
	if stats.ActiveRuns != 0 || stats.ActiveSubagents != 0 || stats.ActiveToolCalls != 0 || stats.StartedTotal != 1 || stats.FinishedTotal != 1 {
		t.Fatalf("finished agent stats = %+v", stats)
	}
}

func TestOperationsStatusAndMetricsExposeOnlyAggregates(t *testing.T) {
	now := time.Now().Unix()
	withTestSessions(t, map[string]sessionClaims{
		"a": {User: "Ada CN", DisplayName: "Ada Lovelace", AccountName: "ada", DirectoryID: "directory-id-not-public", LastSeenAt: now, Expires: now + 3600},
	})
	resetActiveAgentRuns(t)

	operationsRec := httptest.NewRecorder()
	handleOperationsStatus(operationsRec, httptest.NewRequest(http.MethodGet, "/api/admin/operations", nil))
	if operationsRec.Code != http.StatusOK {
		t.Fatalf("operations status=%d body=%s", operationsRec.Code, operationsRec.Body.String())
	}
	if strings.Contains(operationsRec.Body.String(), "directory-id-not-public") {
		t.Fatalf("operations response leaked directory ID: %s", operationsRec.Body.String())
	}
	if !strings.Contains(operationsRec.Body.String(), `"active_users":1`) {
		t.Fatalf("operations response missing active user aggregate: %s", operationsRec.Body.String())
	}

	t.Setenv(metricsTokenEnv, "test-token")
	metricsRec := httptest.NewRecorder()
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer test-token")
	handleMetrics(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", metricsRec.Code, metricsRec.Body.String())
	}
	for _, want := range []string{"r3_users_active 1", "r3_agent_runs_active 0", "r3_agent_tool_calls_active 0"} {
		if !strings.Contains(metricsRec.Body.String(), want) {
			t.Errorf("metrics missing %q: %s", want, metricsRec.Body.String())
		}
	}
	if strings.Contains(metricsRec.Body.String(), "Ada Lovelace") || strings.Contains(metricsRec.Body.String(), "directory-id-not-public") {
		t.Fatalf("metrics must not include identity data: %s", metricsRec.Body.String())
	}
}
