package main

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// activeUser is the intentionally small, admin-only representation of a
// current LDAP session. It exposes operationally useful identity and
// organization context, but never memberOf, objectGUID, passwords, or the
// user's questions/content.
type activeUser struct {
	DisplayName string `json:"display_name"`
	User        string `json:"user"`
	AccountName string `json:"account_name,omitempty"`
	Mail        string `json:"mail,omitempty"`
	Department  string `json:"department,omitempty"`
	Title       string `json:"title,omitempty"`
	Office      string `json:"office,omitempty"`
	Company     string `json:"company,omitempty"`
	IsAdmin     bool   `json:"is_admin"`
	Sessions    int    `json:"sessions"`
	LastSeenAt  int64  `json:"last_seen_at"`
	Active      bool   `json:"active"`
}

// sessionPresenceStats distinguishes valid signed-in browser sessions from
// users active during sessionActiveWindow. A cookie remains valid for eight
// hours, so treating those as the same thing would overstate live usage.
type sessionPresenceStats struct {
	SignedInSessions int          `json:"signed_in_sessions"`
	SignedInUsers    int          `json:"signed_in_users"`
	ActiveSessions   int          `json:"active_sessions"`
	ActiveUsers      int          `json:"active_users"`
	ActiveWindowSecs int64        `json:"active_window_seconds"`
	Users            []activeUser `json:"users"`
}

func sessionSubjectKey(claims sessionClaims) string {
	for _, value := range []string{claims.DirectoryID, claims.Mail, claims.UserPrincipalName, claims.AccountName, claims.User} {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			return value
		}
	}
	return "unknown"
}

func sessionPresenceSnapshot() sessionPresenceStats {
	now := time.Now().Unix()
	activeAfter := now - int64(sessionActiveWindow.Seconds())
	byUser := map[string]*activeUser{}
	stats := sessionPresenceStats{ActiveWindowSecs: int64(sessionActiveWindow.Seconds())}

	sessionStoreMu.Lock()
	// The usual lazy sweep happens on login/lookups. Snapshotting is another
	// safe opportunity to avoid reporting an expired browser as signed in.
	sweepExpiredSessionsLocked()
	for _, claims := range sessionStore {
		if now > claims.Expires {
			continue
		}
		stats.SignedInSessions++
		active := claims.LastSeenAt >= activeAfter
		if active {
			stats.ActiveSessions++
		}
		key := sessionSubjectKey(claims)
		entry := byUser[key]
		if entry == nil {
			entry = &activeUser{
				DisplayName: sessionDisplayName(claims),
				User:        claims.User,
				AccountName: claims.AccountName,
				Mail:        claims.Mail,
				Department:  claims.Department,
				Title:       claims.Title,
				Office:      claims.Office,
				Company:     claims.Company,
				IsAdmin:     claims.IsAdmin,
				LastSeenAt:  claims.LastSeenAt,
				Active:      active,
			}
			byUser[key] = entry
		} else if claims.LastSeenAt > entry.LastSeenAt {
			// Prefer the most recently active session's current profile if the
			// same person logged in from two devices around an AD profile edit.
			entry.DisplayName = sessionDisplayName(claims)
			entry.User = claims.User
			entry.AccountName = claims.AccountName
			entry.Mail = claims.Mail
			entry.Department = claims.Department
			entry.Title = claims.Title
			entry.Office = claims.Office
			entry.Company = claims.Company
			entry.IsAdmin = claims.IsAdmin
			entry.LastSeenAt = claims.LastSeenAt
		}
		entry.Sessions++
		entry.Active = entry.Active || active
	}
	sessionStoreMu.Unlock()

	stats.Users = make([]activeUser, 0, len(byUser))
	for _, user := range byUser {
		if user.Active {
			stats.ActiveUsers++
		}
		stats.Users = append(stats.Users, *user)
	}
	stats.SignedInUsers = len(stats.Users)
	sort.Slice(stats.Users, func(i, j int) bool {
		if stats.Users[i].Active != stats.Users[j].Active {
			return stats.Users[i].Active
		}
		if stats.Users[i].LastSeenAt != stats.Users[j].LastSeenAt {
			return stats.Users[i].LastSeenAt > stats.Users[j].LastSeenAt
		}
		return strings.ToLower(stats.Users[i].DisplayName) < strings.ToLower(stats.Users[j].DisplayName)
	})
	return stats
}

// activeAgentProcess describes a running top-level Agent request. It is
// deliberately separate from agentToolRun's history: this is a live gauge,
// not a long-lived transcript or an additional store of user content.
type activeAgentProcess struct {
	ID              string   `json:"id"`
	User            string   `json:"user"`
	Profile         string   `json:"profile"`
	StartedAt       int64    `json:"started_at"`
	ElapsedMS       int64    `json:"elapsed_ms"`
	ActiveSubagents int      `json:"active_subagents"`
	ActiveToolCalls int      `json:"active_tool_calls"`
	ActiveTools     []string `json:"active_tools,omitempty"`
}

type agentActivityStats struct {
	ActiveRuns      int                  `json:"active_runs"`
	ActiveSubagents int                  `json:"active_subagents"`
	ActiveToolCalls int                  `json:"active_tool_calls"`
	StartedTotal    uint64               `json:"started_total"`
	FinishedTotal   uint64               `json:"finished_total"`
	Runs            []activeAgentProcess `json:"runs"`
}

type trackedAgentRun struct {
	ID          string
	User        string
	Profile     string
	StartedAt   time.Time
	Subagents   int
	ActiveTools map[string]int
}

var (
	agentRunSequence uint64
	agentRunRegistry = struct {
		sync.Mutex
		runs          map[string]*trackedAgentRun
		startedTotal  uint64
		finishedTotal uint64
	}{runs: map[string]*trackedAgentRun{}}
)

type agentRunContextKey struct{}

func activeAgentRunID(ctx context.Context) string {
	id, _ := ctx.Value(agentRunContextKey{}).(string)
	return id
}

// beginActiveAgentRun tracks only the full Agent tier. Instant/standard chat
// may use one ordinary live tool, but they are not agent processes and would
// make the operational dashboard misleading.
func beginActiveAgentRun(ctx context.Context, user, profile string) (context.Context, func()) {
	id := "agent-" + strconv.FormatUint(atomic.AddUint64(&agentRunSequence, 1), 10)
	run := &trackedAgentRun{ID: id, User: strings.TrimSpace(user), Profile: strings.TrimSpace(profile), StartedAt: time.Now(), ActiveTools: map[string]int{}}
	if run.User == "" {
		run.User = "anonym"
	}
	agentRunRegistry.Lock()
	agentRunRegistry.runs[id] = run
	agentRunRegistry.startedTotal++
	agentRunRegistry.Unlock()
	log.Printf("agent run started: id=%s user=%q profile=%q", id, run.User, run.Profile)

	finished := false
	finish := func() {
		agentRunRegistry.Lock()
		if finished {
			agentRunRegistry.Unlock()
			return
		}
		finished = true
		current, ok := agentRunRegistry.runs[id]
		if ok {
			delete(agentRunRegistry.runs, id)
			agentRunRegistry.finishedTotal++
		}
		agentRunRegistry.Unlock()
		if ok {
			log.Printf("agent run finished: id=%s user=%q duration_ms=%d", id, current.User, time.Since(current.StartedAt).Milliseconds())
		}
	}
	return context.WithValue(ctx, agentRunContextKey{}, id), finish
}

// trackActiveAgentTool returns a closure so the call site's normal defer-like
// control flow cannot forget to decrement the gauge on a timeout/error.
func trackActiveAgentTool(ctx context.Context, tool string) func() {
	id := activeAgentRunID(ctx)
	if id == "" {
		return func() {}
	}
	tool = strings.TrimSpace(tool)
	agentRunRegistry.Lock()
	if run := agentRunRegistry.runs[id]; run != nil {
		run.ActiveTools[tool]++
	}
	agentRunRegistry.Unlock()
	return func() {
		agentRunRegistry.Lock()
		if run := agentRunRegistry.runs[id]; run != nil {
			if run.ActiveTools[tool] <= 1 {
				delete(run.ActiveTools, tool)
			} else {
				run.ActiveTools[tool]--
			}
		}
		agentRunRegistry.Unlock()
	}
}

func trackActiveSubagent(ctx context.Context) func() {
	id := activeAgentRunID(ctx)
	if id == "" {
		return func() {}
	}
	agentRunRegistry.Lock()
	if run := agentRunRegistry.runs[id]; run != nil {
		run.Subagents++
	}
	agentRunRegistry.Unlock()
	return func() {
		agentRunRegistry.Lock()
		if run := agentRunRegistry.runs[id]; run != nil && run.Subagents > 0 {
			run.Subagents--
		}
		agentRunRegistry.Unlock()
	}
}

func agentActivitySnapshot() agentActivityStats {
	now := time.Now()
	agentRunRegistry.Lock()
	defer agentRunRegistry.Unlock()
	stats := agentActivityStats{
		ActiveRuns:    len(agentRunRegistry.runs),
		StartedTotal:  agentRunRegistry.startedTotal,
		FinishedTotal: agentRunRegistry.finishedTotal,
		Runs:          make([]activeAgentProcess, 0, len(agentRunRegistry.runs)),
	}
	for _, run := range agentRunRegistry.runs {
		process := activeAgentProcess{
			ID:              run.ID,
			User:            run.User,
			Profile:         run.Profile,
			StartedAt:       run.StartedAt.Unix(),
			ElapsedMS:       now.Sub(run.StartedAt).Milliseconds(),
			ActiveSubagents: run.Subagents,
		}
		stats.ActiveSubagents += run.Subagents
		for tool, count := range run.ActiveTools {
			process.ActiveToolCalls += count
			stats.ActiveToolCalls += count
			process.ActiveTools = append(process.ActiveTools, tool)
		}
		sort.Strings(process.ActiveTools)
		stats.Runs = append(stats.Runs, process)
	}
	sort.Slice(stats.Runs, func(i, j int) bool { return stats.Runs[i].StartedAt < stats.Runs[j].StartedAt })
	return stats
}

type operationsStatus struct {
	GeneratedAt int64                `json:"generated_at"`
	Sessions    sessionPresenceStats `json:"sessions"`
	Agents      agentActivityStats   `json:"agents"`
}

// handleOperationsStatus is admin-only at route registration. It combines
// only in-memory, current-process observations; it does not read questions,
// tool arguments/results, AD groups, or raw directory identifiers.
func handleOperationsStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, operationsStatus{
		GeneratedAt: time.Now().Unix(),
		Sessions:    sessionPresenceSnapshot(),
		Agents:      agentActivitySnapshot(),
	})
}
