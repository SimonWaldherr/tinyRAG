package app

// ─────────────────────────────────────────────────────────────────────────────
// Usage statistics
//
// Persistent, lightweight usage accounting for the dashboard. Every completed
// /api/ask request is appended as one JSON line to the usage file; aggregates
// are computed on demand for GET /api/stats/usage.
//
// The store keeps a bounded in-memory window (maxUsageRecords) so long-running
// installations do not grow memory without bound; the on-disk file is
// append-only and survives restarts.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

// maxUsageRecords bounds the in-memory record window.
const maxUsageRecords = 50000

// usageRecord captures the billable/observable footprint of one request.
type usageRecord struct {
	Time           time.Time `json:"time"`
	Role           string    `json:"role"`
	Mode           string    `json:"mode"`
	DurationMS     int64     `json:"duration_ms"`
	TokensStreamed int       `json:"tokens_streamed"`
	VisibleChars   int       `json:"visible_chars"`
	ContextChars   int       `json:"context_chars"`
	ToolCalls      int       `json:"tool_calls"`
	Continuations  int       `json:"continuations"`
	Success        bool      `json:"success"`
	Tools          []string  `json:"tools,omitempty"`
}

// usageStore persists usage records as JSONL and serves aggregates.
type usageStore struct {
	mu   sync.Mutex
	path string
	recs []usageRecord
}

// package-level usage store, initialized in main(). May stay nil in CLI mode.
var usageStats *usageStore

// newUsageStore loads existing records from path (missing file is fine).
func newUsageStore(path string) *usageStore {
	us := &usageStore{path: path}
	if path == "" {
		return us
	}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: usage stats load failed: %v", err)
		}
		return us
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec usageRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err == nil && !rec.Time.IsZero() {
			us.recs = append(us.recs, rec)
		}
	}
	if len(us.recs) > maxUsageRecords {
		us.recs = us.recs[len(us.recs)-maxUsageRecords:]
	}
	return us
}

// record appends one usage record in memory and to the JSONL file.
func (us *usageStore) record(rec usageRecord) {
	if us == nil {
		return
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	us.recs = append(us.recs, rec)
	if len(us.recs) > maxUsageRecords {
		us.recs = us.recs[len(us.recs)-maxUsageRecords:]
	}
	if us.path == "" {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(us.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("WARN: usage stats append failed: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// recordFromTelemetry converts a finalized RequestTelemetry into a usageRecord.
func (us *usageStore) recordFromTelemetry(tel *RequestTelemetry, role string) {
	if us == nil || tel == nil {
		return
	}
	tools := make([]string, 0, len(tel.ToolInvocations))
	for _, ti := range tel.ToolInvocations {
		if ti.Deduplicated || (ti.PolicyDecision != "" && ti.PolicyDecision != "allow") {
			continue
		}
		tools = append(tools, ti.Tool)
	}
	us.record(usageRecord{
		Time:           tel.StartTime,
		Role:           role,
		Mode:           tel.SelectedMode,
		DurationMS:     tel.TotalMS,
		TokensStreamed: tel.TokensStreamed,
		VisibleChars:   tel.VisibleChars,
		ContextChars:   tel.ContextChars,
		ToolCalls:      len(tools),
		Continuations:  tel.ContinuationCount,
		Success:        tel.Success,
		Tools:          tools,
	})
}

// usageDayBucket is one per-day aggregate row.
type usageDayBucket struct {
	Day      string `json:"day"` // YYYY-MM-DD
	Requests int    `json:"requests"`
	Tokens   int    `json:"tokens"`
	Errors   int    `json:"errors"`
}

// usageSummary is the response payload for GET /api/stats/usage.
type usageSummary struct {
	TotalRequests  int              `json:"total_requests"`
	SuccessRate    float64          `json:"success_rate"`
	AvgDurationMS  int64            `json:"avg_duration_ms"`
	TotalTokens    int              `json:"total_tokens"`
	TotalToolCalls int              `json:"total_tool_calls"`
	PerDay         []usageDayBucket `json:"per_day"`
	PerRole        map[string]int   `json:"per_role"`
	PerMode        map[string]int   `json:"per_mode"`
	PerTool        map[string]int   `json:"per_tool"`
	WindowDays     int              `json:"window_days"`
}

// summarize aggregates all records newer than now-windowDays.
func (us *usageStore) summarize(windowDays int) usageSummary {
	if windowDays <= 0 {
		windowDays = 30
	}
	if windowDays > 365 {
		windowDays = 365
	}
	sum := usageSummary{
		PerRole:    map[string]int{},
		PerMode:    map[string]int{},
		PerTool:    map[string]int{},
		WindowDays: windowDays,
	}
	if us == nil {
		return sum
	}
	cutoff := time.Now().AddDate(0, 0, -windowDays)

	us.mu.Lock()
	defer us.mu.Unlock()

	days := map[string]*usageDayBucket{}
	var totalDur int64
	success := 0
	for _, rec := range us.recs {
		if rec.Time.Before(cutoff) {
			continue
		}
		sum.TotalRequests++
		totalDur += rec.DurationMS
		if rec.Success {
			success++
		}
		sum.TotalTokens += rec.TokensStreamed
		sum.TotalToolCalls += rec.ToolCalls
		if rec.Role != "" {
			sum.PerRole[rec.Role]++
		}
		if rec.Mode != "" {
			sum.PerMode[rec.Mode]++
		}
		for _, tool := range rec.Tools {
			sum.PerTool[tool]++
		}
		day := rec.Time.Format("2006-01-02")
		b, ok := days[day]
		if !ok {
			b = &usageDayBucket{Day: day}
			days[day] = b
		}
		b.Requests++
		b.Tokens += rec.TokensStreamed
		if !rec.Success {
			b.Errors++
		}
	}
	if sum.TotalRequests > 0 {
		sum.SuccessRate = float64(success) / float64(sum.TotalRequests)
		sum.AvgDurationMS = totalDur / int64(sum.TotalRequests)
	}
	for _, b := range days {
		sum.PerDay = append(sum.PerDay, *b)
	}
	sort.Slice(sum.PerDay, func(i, j int) bool { return sum.PerDay[i].Day < sum.PerDay[j].Day })
	return sum
}
