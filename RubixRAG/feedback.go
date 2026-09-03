package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Per-answer user feedback (thumbs up/down) — docs/TODO.md C5. Minimal by
// design: two buttons under every chat answer, appended to a JSONL log
// file next to settings.json. No evaluation UI yet (see the TODO entry) —
// this only needs to start collecting real signal from day one of a
// pilot; analysis can come later once there's something to analyze.
// ─────────────────────────────────────────────────────────────────────────────

// feedbackLogPath is set once in main() to a file next to whatever
// -settings path was configured, so separate instances (e.g. the
// launch.json r3-verify/r3-verify2 configs, each with its own -settings)
// don't share one feedback log.
var feedbackLogPath = "r3-feedback.jsonl"

// feedbackRecord is one thumbs up/down vote, appended as a single JSON line.
type feedbackRecord struct {
	Time     int64  `json:"time"` // unix seconds
	User     string `json:"user"` // session mail/CN, or "anonym"
	Question string `json:"question"`
	// Answer is the full answer text. Originally this file stored only
	// AnswerHash below to keep the log small — but the Jobs tab's own
	// "RecentDownvotes" panel exists specifically as raw material for
	// building a golden-question eval set from bad answers (see
	// feedbackStats' doc comment), which is impossible from a hash alone:
	// an admin reviewing a downvote needs to see WHAT the bad answer
	// actually said, not just that some answer got downvoted. The log
	// already carries Question text and User identity at the same
	// sensitivity level, so storing Answer too doesn't cross a new
	// privacy threshold.
	Answer string `json:"answer,omitempty"`
	// AnswerHash is kept alongside Answer for continuity with log lines
	// written before Answer existed (readFeedbackStats never needs it for
	// anything itself; it's just carried through).
	AnswerHash string   `json:"answer_hash"`
	Citations  []string `json:"citations,omitempty"`
	Rating     string   `json:"rating"` // "up" | "down"
}

// feedbackRequest is what the browser POSTs when a thumbs button is
// clicked (web/app.js's addFeedbackButtons).
type feedbackRequest struct {
	Question  string   `json:"question"`
	Answer    string   `json:"answer"`
	Citations []string `json:"citations,omitempty"`
	Rating    string   `json:"rating"`
}

// hashAnswer returns a short, stable fingerprint of the answer text —
// enough to correlate repeated feedback on the same answer without storing
// (and thereby duplicating) the full text in the feedback log.
func hashAnswer(answer string) string {
	sum := sha256.Sum256([]byte(answer))
	return hex.EncodeToString(sum[:])[:16]
}

// appendFeedback writes one record as a JSON line to feedbackLogPath,
// creating the file on first use. Best-effort like scheduler.go's history
// persistence — a write failure (disk full, permissions) logs but doesn't
// fail the request, since losing one feedback vote is far less disruptive
// than breaking the chat response the user is looking at.
func appendFeedback(rec feedbackRecord) error {
	f, err := os.OpenFile(feedbackLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// handleFeedback records a thumbs up/down vote for one chat answer.
// Ungated (no session required), same reasoning as handleChatEmail's
// neighbors: an anonymous guest can already ask questions via /api/ask, so
// letting them rate the answer they got isn't a new exposure — the vote
// itself carries no more sensitivity than the question/answer pair
// already visible in that guest's own browser.
func handleFeedback(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid body", 400)
		return
	}
	req.Rating = strings.ToLower(strings.TrimSpace(req.Rating))
	if req.Rating != "up" && req.Rating != "down" {
		writeJSONError(w, `rating must be "up" or "down"`, 400)
		return
	}
	if strings.TrimSpace(req.Answer) == "" {
		writeJSONError(w, "missing answer", 400)
		return
	}
	user := "anonym"
	if claims, ok := currentSession(r); ok {
		user = sessionActor(claims)
	}
	rec := feedbackRecord{
		Time:       time.Now().Unix(),
		User:       user,
		Question:   req.Question,
		Answer:     req.Answer,
		AnswerHash: hashAnswer(req.Answer),
		Citations:  req.Citations,
		Rating:     req.Rating,
	}
	if err := appendFeedback(rec); err != nil {
		writeJSONError(w, "could not record feedback: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// Feedback analysis (docs/TODO.md C5's own "analysis can come later" —
// this is that later): an admin-facing read-side aggregate over the same
// JSONL log appendFeedback writes, answering "is this actually working"
// without grepping a log file by hand. Computed fresh from disk on every
// request rather than kept in memory or a database — the log is small
// (one short JSON line per vote) and this is a Jobs-tab admin panel, not
// a hot path; simplest thing that works, matching this file's original
// "minimal by design" posture.
// ─────────────────────────────────────────────────────────────────────────────

// feedbackSourceStat is one cited source's up/down tally across every
// vote that cited it — the candidate list for "this document keeps
// getting downvoted when cited, maybe it needs re-importing/excluding".
type feedbackSourceStat struct {
	SourceID string `json:"source_id"`
	Up       int    `json:"up"`
	Down     int    `json:"down"`
}

// feedbackStats is the full admin-facing aggregate.
type feedbackStats struct {
	Total int `json:"total"`
	Up    int `json:"up"`
	Down  int `json:"down"`
	// DownRate is Down/Total, left at the zero value when Total is 0
	// (avoids a NaN/Inf ever reaching the JSON response).
	DownRate float64 `json:"down_rate"`
	// WorstSources is sorted by Down descending (ties broken by SourceID
	// for a stable order), capped at feedbackWorstSourcesCap — an admin
	// reviewing quality cares about the handful of worst offenders, not a
	// ranked list of every source ever cited.
	WorstSources []feedbackSourceStat `json:"worst_sources,omitempty"`
	// RecentDownvotes is newest-first, capped at feedbackRecentDownvotesCap
	// regardless of how large the underlying log has grown — raw material
	// for building a golden-question eval set by hand (see the RAG/agent
	// optimization plan this panel is part of), not a full audit trail.
	RecentDownvotes []feedbackRecord `json:"recent_downvotes,omitempty"`
}

const (
	feedbackRecentDownvotesCap = 50
	feedbackWorstSourcesCap    = 20
)

// feedbackStatsFilter narrows readFeedbackStats to a time window and/or one
// user — every field is optional (zero value = unbounded/no filter), so
// the pre-existing "whole log, every time" behavior is just the zero-value
// filter. Exists because an admin often cares about a specific slice — "how
// are things since the last deploy" or "what is this one department
// downvoting" — not always the entire, ever-growing log.
type feedbackStatsFilter struct {
	// From/To bound rec.Time (unix seconds), inclusive on both ends. 0
	// means unbounded on that side.
	From, To int64
	// User exact-matches feedbackRecord.User (session mail/CN, or
	// "anonym") — empty means every user.
	User string
}

// matches reports whether rec falls within f — a record is excluded by an
// unset (zero) From/To/User exactly as if that filter dimension weren't
// applied at all.
func (f feedbackStatsFilter) matches(rec feedbackRecord) bool {
	if f.From > 0 && rec.Time < f.From {
		return false
	}
	if f.To > 0 && rec.Time > f.To {
		return false
	}
	if f.User != "" && rec.User != f.User {
		return false
	}
	return true
}

// readFeedbackStats parses feedbackLogPath line by line — a missing file
// (no vote cast yet) is not an error, just an all-zero feedbackStats — and
// skips any line that fails to parse as JSON rather than failing the
// whole read over one corrupted entry (e.g. a partially-written line from
// a crash mid-append). Memory stays bounded regardless of log size: only
// the running aggregate, one running tally per distinct cited source, and
// the capped recent-downvotes window are ever held at once — the log
// itself is never loaded wholesale into memory. filter narrows which
// records count at all (see feedbackStatsFilter) — applied before every
// other computation below, so Total/Up/Down/WorstSources/RecentDownvotes
// are all already scoped to it.
func readFeedbackStats(filter feedbackStatsFilter) (feedbackStats, error) {
	f, err := os.Open(feedbackLogPath)
	if errors.Is(err, os.ErrNotExist) {
		return feedbackStats{}, nil
	}
	if err != nil {
		return feedbackStats{}, err
	}
	defer f.Close()

	var stats feedbackStats
	sourceStats := map[string]*feedbackSourceStat{}
	var recentDown []feedbackRecord

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec feedbackRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if !filter.matches(rec) {
			continue
		}
		stats.Total++
		switch rec.Rating {
		case "up":
			stats.Up++
		case "down":
			stats.Down++
			recentDown = append(recentDown, rec)
			if len(recentDown) > feedbackRecentDownvotesCap {
				recentDown = recentDown[1:]
			}
		}
		for _, sid := range rec.Citations {
			ss, ok := sourceStats[sid]
			if !ok {
				ss = &feedbackSourceStat{SourceID: sid}
				sourceStats[sid] = ss
			}
			switch rec.Rating {
			case "up":
				ss.Up++
			case "down":
				ss.Down++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return feedbackStats{}, err
	}

	if stats.Total > 0 {
		stats.DownRate = float64(stats.Down) / float64(stats.Total)
	}

	for _, ss := range sourceStats {
		if ss.Down > 0 {
			stats.WorstSources = append(stats.WorstSources, *ss)
		}
	}
	sort.Slice(stats.WorstSources, func(i, j int) bool {
		if stats.WorstSources[i].Down != stats.WorstSources[j].Down {
			return stats.WorstSources[i].Down > stats.WorstSources[j].Down
		}
		return stats.WorstSources[i].SourceID < stats.WorstSources[j].SourceID
	})
	if len(stats.WorstSources) > feedbackWorstSourcesCap {
		stats.WorstSources = stats.WorstSources[:feedbackWorstSourcesCap]
	}

	// recentDown was built oldest-first (each new downvote appended,
	// oldest dropped off the front once over cap) — reverse in place so
	// the response reads newest-first, the natural order for an admin
	// scanning what just happened.
	for i, j := 0, len(recentDown)-1; i < j; i, j = i+1, j-1 {
		recentDown[i], recentDown[j] = recentDown[j], recentDown[i]
	}
	stats.RecentDownvotes = recentDown

	return stats, nil
}

// handleFeedbackStats serves the admin-only aggregate above — admin-gated
// (requireAdminSession, handlers.go) unlike handleFeedback itself, since
// this surfaces every rater's identity/question text in bulk, not just
// one vote a caller already knows the content of. Optional query params
// from/to (unix seconds) and user narrow the aggregate via
// feedbackStatsFilter — all omitted reproduces the previous "whole log"
// behavior exactly.
func handleFeedbackStats(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	var filter feedbackStatsFilter
	if v := strings.TrimSpace(q.Get("from")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.From = n
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.To = n
		}
	}
	filter.User = strings.TrimSpace(q.Get("user"))
	stats, err := readFeedbackStats(filter)
	if err != nil {
		writeJSONError(w, "could not read feedback log: "+err.Error(), 500)
		return
	}
	writeJSON(w, stats)
}
