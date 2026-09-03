package main

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

// metrics are dependency-free Prometheus/OpenMetrics-compatible text.  The
// endpoint is intentionally disabled until R3_METRICS_TOKEN is set; metrics
// include connector/job names and should not be exposed anonymously.
const metricsTokenEnv = "R3_METRICS_TOKEN"

var schedulerMetrics = struct {
	sync.Mutex
	runs      map[string]int64
	failures  map[string]int64
	durations map[string]schedulerDurationMetric
	running   map[string]bool
}{runs: map[string]int64{}, failures: map[string]int64{}, durations: map[string]schedulerDurationMetric{}, running: map[string]bool{}}

type schedulerDurationMetric struct {
	SumSeconds float64
	Count      int64
}

func metricSchedulerStarted(job string) {
	schedulerMetrics.Lock()
	schedulerMetrics.running[job] = true
	schedulerMetrics.Unlock()
}

func metricSchedulerFinished(job string, seconds float64, ok bool) {
	schedulerMetrics.Lock()
	defer schedulerMetrics.Unlock()
	schedulerMetrics.running[job] = false
	schedulerMetrics.runs[job]++
	if !ok {
		schedulerMetrics.failures[job]++
	}
	d := schedulerMetrics.durations[job]
	d.SumSeconds += seconds
	d.Count++
	schedulerMetrics.durations[job] = d
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	want := os.Getenv(metricsTokenEnv)
	if want == "" || r.Header.Get("Authorization") != "Bearer "+want {
		http.NotFound(w, r)
		return
	}
	schedulerMetrics.Lock()
	jobs := make(map[string]bool)
	for job := range schedulerMetrics.runs {
		jobs[job] = true
	}
	for job := range schedulerMetrics.running {
		jobs[job] = true
	}
	for job := range schedulerMetrics.durations {
		jobs[job] = true
	}
	keys := make([]string, 0, len(jobs))
	for job := range jobs {
		keys = append(keys, job)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# HELP r3_scheduler_runs_total Completed scheduler runs by job.\n# TYPE r3_scheduler_runs_total counter\n")
	for _, job := range keys {
		fmt.Fprintf(&b, "r3_scheduler_runs_total{job=%q} %d\n", job, schedulerMetrics.runs[job])
	}
	b.WriteString("# HELP r3_scheduler_failures_total Failed scheduler runs by job.\n# TYPE r3_scheduler_failures_total counter\n")
	for _, job := range keys {
		fmt.Fprintf(&b, "r3_scheduler_failures_total{job=%q} %d\n", job, schedulerMetrics.failures[job])
	}
	b.WriteString("# HELP r3_scheduler_run_duration_seconds Scheduler run duration.\n# TYPE r3_scheduler_run_duration_seconds summary\n")
	for _, job := range keys {
		d := schedulerMetrics.durations[job]
		fmt.Fprintf(&b, "r3_scheduler_run_duration_seconds_sum{job=%q} %.6f\nr3_scheduler_run_duration_seconds_count{job=%q} %d\n", job, d.SumSeconds, job, d.Count)
	}
	b.WriteString("# HELP r3_scheduler_running Whether a scheduler job is running.\n# TYPE r3_scheduler_running gauge\n")
	for _, job := range keys {
		running := 0
		if schedulerMetrics.running[job] {
			running = 1
		}
		fmt.Fprintf(&b, "r3_scheduler_running{job=%q} %d\n", job, running)
	}
	active := 0
	for _, alert := range schedulerAlertsSnapshot() {
		if alert.ResolvedAt == 0 {
			active++
		}
	}
	fmt.Fprintf(&b, "# HELP r3_scheduler_alerts_active Active scheduler alerts.\n# TYPE r3_scheduler_alerts_active gauge\nr3_scheduler_alerts_active %d\n", active)
	schedulerMetrics.Unlock()

	// Live identity/agent gauges are intentionally aggregate-only. The
	// admin operations endpoint can show names to an authenticated operator;
	// Prometheus must never receive per-person labels (high cardinality and
	// avoidable PII leakage).
	presence := sessionPresenceSnapshot()
	agents := agentActivitySnapshot()
	b.WriteString("# HELP r3_sessions_signed_in Valid browser sessions.\n# TYPE r3_sessions_signed_in gauge\n")
	fmt.Fprintf(&b, "r3_sessions_signed_in %d\n", presence.SignedInSessions)
	b.WriteString("# HELP r3_sessions_active Browser sessions active within the configured activity window.\n# TYPE r3_sessions_active gauge\n")
	fmt.Fprintf(&b, "r3_sessions_active %d\n", presence.ActiveSessions)
	b.WriteString("# HELP r3_users_signed_in Distinct users with a valid browser session.\n# TYPE r3_users_signed_in gauge\n")
	fmt.Fprintf(&b, "r3_users_signed_in %d\n", presence.SignedInUsers)
	b.WriteString("# HELP r3_users_active Distinct users active within the configured activity window.\n# TYPE r3_users_active gauge\n")
	fmt.Fprintf(&b, "r3_users_active %d\n", presence.ActiveUsers)
	b.WriteString("# HELP r3_agent_runs_active Active full Agent-tier requests.\n# TYPE r3_agent_runs_active gauge\n")
	fmt.Fprintf(&b, "r3_agent_runs_active %d\n", agents.ActiveRuns)
	b.WriteString("# HELP r3_agent_subagents_active Active delegated or web-research subagents.\n# TYPE r3_agent_subagents_active gauge\n")
	fmt.Fprintf(&b, "r3_agent_subagents_active %d\n", agents.ActiveSubagents)
	b.WriteString("# HELP r3_agent_tool_calls_active Active tool calls belonging to Agent-tier requests.\n# TYPE r3_agent_tool_calls_active gauge\n")
	fmt.Fprintf(&b, "r3_agent_tool_calls_active %d\n", agents.ActiveToolCalls)
	b.WriteString("# HELP r3_agent_runs_started_total Agent runs started since process start.\n# TYPE r3_agent_runs_started_total counter\n")
	fmt.Fprintf(&b, "r3_agent_runs_started_total %d\n", agents.StartedTotal)
	b.WriteString("# HELP r3_agent_runs_finished_total Agent runs finished since process start.\n# TYPE r3_agent_runs_finished_total counter\n")
	fmt.Fprintf(&b, "r3_agent_runs_finished_total %d\n", agents.FinishedTotal)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
