package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTestFeedbackLog points feedbackLogPath at a fresh temp file for the
// duration of one test and restores the original afterward — every test
// in this file needs its own isolated log so votes from one test never
// leak into another's aggregate.
func withTestFeedbackLog(t *testing.T) {
	t.Helper()
	orig := feedbackLogPath
	feedbackLogPath = filepath.Join(t.TempDir(), "feedback.jsonl")
	t.Cleanup(func() { feedbackLogPath = orig })
}

// TestReadFeedbackStatsMissingFileReturnsZeroValue confirms a fresh
// deployment with no vote cast yet (no log file at all) reports an
// all-zero aggregate, not an error — same "not-yet-populated is not a
// failure" posture as lastContentHash/similar best-effort reads
// elsewhere in this codebase.
func TestReadFeedbackStatsMissingFileReturnsZeroValue(t *testing.T) {
	withTestFeedbackLog(t)
	stats, err := readFeedbackStats(feedbackStatsFilter{})
	if err != nil {
		t.Fatalf("readFeedbackStats: %v", err)
	}
	if stats.Total != 0 || stats.Up != 0 || stats.Down != 0 || stats.DownRate != 0 {
		t.Fatalf("want an all-zero aggregate for a missing log, got %+v", stats)
	}
}

// TestReadFeedbackStatsAggregatesUpDownAndSources confirms the core
// aggregate: total/up/down counts, the derived down rate, and per-source
// up/down tallies built from each vote's Citations.
func TestReadFeedbackStatsAggregatesUpDownAndSources(t *testing.T) {
	withTestFeedbackLog(t)
	votes := []feedbackRecord{
		{Time: 1, User: "a", Question: "q1", Rating: "up", Citations: []string{"doc-1"}},
		{Time: 2, User: "b", Question: "q2", Rating: "down", Citations: []string{"doc-1"}},
		{Time: 3, User: "c", Question: "q3", Rating: "down", Citations: []string{"doc-1"}},
		{Time: 4, User: "d", Question: "q4", Rating: "up", Citations: []string{"doc-2"}},
	}
	for _, v := range votes {
		if err := appendFeedback(v); err != nil {
			t.Fatalf("appendFeedback: %v", err)
		}
	}

	stats, err := readFeedbackStats(feedbackStatsFilter{})
	if err != nil {
		t.Fatalf("readFeedbackStats: %v", err)
	}
	if stats.Total != 4 || stats.Up != 2 || stats.Down != 2 {
		t.Fatalf("want total=4 up=2 down=2, got %+v", stats)
	}
	if stats.DownRate != 0.5 {
		t.Fatalf("want down_rate=0.5, got %v", stats.DownRate)
	}

	bySource := map[string]feedbackSourceStat{}
	for _, s := range stats.WorstSources {
		bySource[s.SourceID] = s
	}
	doc1, ok := bySource["doc-1"]
	if !ok || doc1.Up != 1 || doc1.Down != 2 {
		t.Fatalf("want doc-1 up=1 down=2, got %+v (present=%v)", doc1, ok)
	}
	// doc-2 has 0 downvotes — must not appear in WorstSources at all
	// (only sources with Down > 0 are candidates for review).
	if _, ok := bySource["doc-2"]; ok {
		t.Fatalf("want doc-2 (never downvoted) excluded from WorstSources, got %+v", stats.WorstSources)
	}
}

// TestReadFeedbackStatsSkipsMalformedLines confirms one corrupted line
// (e.g. a partially-written entry from a crash mid-append) doesn't fail
// the whole read — the remaining well-formed lines still count.
func TestReadFeedbackStatsSkipsMalformedLines(t *testing.T) {
	withTestFeedbackLog(t)
	if err := appendFeedback(feedbackRecord{Rating: "up"}); err != nil {
		t.Fatalf("appendFeedback: %v", err)
	}
	f, err := os.OpenFile(feedbackLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for corrupt append: %v", err)
	}
	if _, err := f.WriteString("{not valid json\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	f.Close()
	if err := appendFeedback(feedbackRecord{Rating: "down"}); err != nil {
		t.Fatalf("appendFeedback: %v", err)
	}

	stats, err := readFeedbackStats(feedbackStatsFilter{})
	if err != nil {
		t.Fatalf("readFeedbackStats must tolerate a malformed line, got error: %v", err)
	}
	if stats.Total != 2 {
		t.Fatalf("want the 2 well-formed lines counted (corrupt line skipped), got total=%d", stats.Total)
	}
}

// TestReadFeedbackStatsCapsRecentDownvotesNewestFirst confirms the
// recent-downvotes window stays bounded at feedbackRecentDownvotesCap
// regardless of how many downvotes exist, and reads back newest-first.
func TestReadFeedbackStatsCapsRecentDownvotesNewestFirst(t *testing.T) {
	withTestFeedbackLog(t)
	n := feedbackRecentDownvotesCap + 10
	for i := 0; i < n; i++ {
		if err := appendFeedback(feedbackRecord{Time: int64(i), Rating: "down", Question: "q"}); err != nil {
			t.Fatalf("appendFeedback %d: %v", i, err)
		}
	}

	stats, err := readFeedbackStats(feedbackStatsFilter{})
	if err != nil {
		t.Fatalf("readFeedbackStats: %v", err)
	}
	if stats.Down != n {
		t.Fatalf("want Down=%d (every vote still counted toward the total), got %d", n, stats.Down)
	}
	if len(stats.RecentDownvotes) != feedbackRecentDownvotesCap {
		t.Fatalf("want RecentDownvotes capped at %d, got %d", feedbackRecentDownvotesCap, len(stats.RecentDownvotes))
	}
	if stats.RecentDownvotes[0].Time != int64(n-1) {
		t.Fatalf("want the newest downvote (Time=%d) first, got %d", n-1, stats.RecentDownvotes[0].Time)
	}
}

// TestReadFeedbackStatsFilterByTimeRange confirms From/To narrow which
// records are counted at all — a vote outside the window must not appear
// in Total, Up/Down, WorstSources, or RecentDownvotes.
func TestReadFeedbackStatsFilterByTimeRange(t *testing.T) {
	withTestFeedbackLog(t)
	votes := []feedbackRecord{
		{Time: 100, Rating: "down", Question: "too old", Citations: []string{"doc-old"}},
		{Time: 200, Rating: "down", Question: "in range", Citations: []string{"doc-in"}},
		{Time: 300, Rating: "up", Question: "too new", Citations: []string{"doc-new"}},
	}
	for _, v := range votes {
		if err := appendFeedback(v); err != nil {
			t.Fatalf("appendFeedback: %v", err)
		}
	}

	stats, err := readFeedbackStats(feedbackStatsFilter{From: 150, To: 250})
	if err != nil {
		t.Fatalf("readFeedbackStats: %v", err)
	}
	if stats.Total != 1 {
		t.Fatalf("want only the in-range vote counted, got total=%d", stats.Total)
	}
	if len(stats.RecentDownvotes) != 1 || stats.RecentDownvotes[0].Question != "in range" {
		t.Fatalf("want only the in-range downvote, got %+v", stats.RecentDownvotes)
	}
	bySource := map[string]feedbackSourceStat{}
	for _, s := range stats.WorstSources {
		bySource[s.SourceID] = s
	}
	if _, ok := bySource["doc-old"]; ok {
		t.Fatalf("want the too-old vote's source excluded, got %+v", stats.WorstSources)
	}
	if _, ok := bySource["doc-in"]; !ok {
		t.Fatalf("want the in-range vote's source included, got %+v", stats.WorstSources)
	}
}

// TestReadFeedbackStatsFilterByUser confirms User exact-matches
// feedbackRecord.User, excluding every other user's votes entirely.
func TestReadFeedbackStatsFilterByUser(t *testing.T) {
	withTestFeedbackLog(t)
	votes := []feedbackRecord{
		{User: "alice@rubix.com", Rating: "down", Question: "alice q1"},
		{User: "bob@rubix.com", Rating: "down", Question: "bob q1"},
		{User: "alice@rubix.com", Rating: "up", Question: "alice q2"},
	}
	for _, v := range votes {
		if err := appendFeedback(v); err != nil {
			t.Fatalf("appendFeedback: %v", err)
		}
	}

	stats, err := readFeedbackStats(feedbackStatsFilter{User: "alice@rubix.com"})
	if err != nil {
		t.Fatalf("readFeedbackStats: %v", err)
	}
	if stats.Total != 2 {
		t.Fatalf("want only alice's 2 votes counted, got total=%d", stats.Total)
	}
	if stats.Up != 1 || stats.Down != 1 {
		t.Fatalf("want alice's up=1 down=1, got up=%d down=%d", stats.Up, stats.Down)
	}
}

// TestHandleFeedbackStatsRequiresAdminWhenLDAPEnabled confirms the
// endpoint is wired through requireAdminSession (handlers.go) — with LDAP
// enabled and no session, it must reject rather than leak every rater's
// question text/identity to an anonymous caller.
func TestHandleFeedbackStatsRequiresAdminWhenLDAPEnabled(t *testing.T) {
	_, s := newTestRAG(t)
	s.LDAP.Enabled = true
	withTestGlobalSettings(t, s)

	rec := httptest.NewRecorder()
	requireAdminSession(handleFeedbackStats)(rec, httptest.NewRequest(http.MethodGet, "/api/feedback/stats", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without an admin session when LDAP is enabled, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleFeedbackStatsReturnsAggregate is the end-to-end happy path:
// POST /api/feedback records votes, GET /api/feedback/stats reports them.
func TestHandleFeedbackStatsReturnsAggregate(t *testing.T) {
	withTestFeedbackLog(t)
	if err := appendFeedback(feedbackRecord{Rating: "up", Question: "q1"}); err != nil {
		t.Fatalf("appendFeedback: %v", err)
	}
	if err := appendFeedback(feedbackRecord{Rating: "down", Question: "q2"}); err != nil {
		t.Fatalf("appendFeedback: %v", err)
	}

	rec := httptest.NewRecorder()
	handleFeedbackStats(rec, httptest.NewRequest(http.MethodGet, "/api/feedback/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"total":2`) {
		t.Fatalf("want total=2 in the response, got %s", rec.Body.String())
	}
}

// TestHandleFeedbackStatsAppliesQueryParamFilter confirms the from/to/user
// query params (handleFeedbackStats) actually reach readFeedbackStats'
// filter — end-to-end wiring, not just the filter logic itself (already
// covered by TestReadFeedbackStatsFilterByTimeRange/ByUser above).
func TestHandleFeedbackStatsAppliesQueryParamFilter(t *testing.T) {
	withTestFeedbackLog(t)
	if err := appendFeedback(feedbackRecord{Time: 100, User: "alice@rubix.com", Rating: "down", Question: "old"}); err != nil {
		t.Fatalf("appendFeedback: %v", err)
	}
	if err := appendFeedback(feedbackRecord{Time: 200, User: "alice@rubix.com", Rating: "down", Question: "in range"}); err != nil {
		t.Fatalf("appendFeedback: %v", err)
	}
	if err := appendFeedback(feedbackRecord{Time: 200, User: "bob@rubix.com", Rating: "down", Question: "wrong user"}); err != nil {
		t.Fatalf("appendFeedback: %v", err)
	}

	rec := httptest.NewRecorder()
	handleFeedbackStats(rec, httptest.NewRequest(http.MethodGet, "/api/feedback/stats?from=150&to=250&user=alice%40rubix.com", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"total":1`) {
		t.Fatalf("want only the one matching vote counted, got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"in range"`) {
		t.Fatalf("want the matching vote's question in the response, got %s", rec.Body.String())
	}
}
