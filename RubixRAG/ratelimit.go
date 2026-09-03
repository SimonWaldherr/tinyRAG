package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// A minimal in-memory brute-force guard for the two password-check
// endpoints (handleLDAPLogin, handleAdminCheck) — no external dependency,
// no persistence (a restart resets every count, the same tradeoff
// session.go's signing secret already makes). Keyed by client IP rather
// than username/password, since an anonymous caller hasn't proven any
// identity yet; this bounds how many guesses one source can throw at
// either endpoint. It does not protect against a distributed attack
// spread across many IPs — that needs a reverse proxy/WAF in front, same
// as the rate-limiting/CORS gaps docs/API.md already documents.
// ─────────────────────────────────────────────────────────────────────────────

const (
	loginAttemptLimit  = 10
	loginAttemptWindow = 5 * time.Minute
)

type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time // client key -> recent failure timestamps
}

var globalLoginLimiter = &loginLimiter{fails: map[string][]time.Time{}}

// clientKey identifies the caller for rate-limiting purposes. RemoteAddr
// is what net/http gives a handler directly; if R3 ever sits behind a
// reverse proxy that terminates client connections, this would need to
// read X-Forwarded-For instead — not done here since trusting that header
// without also knowing which proxies are allowed to set it would let a
// client just spoof its way around the limiter entirely.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// allow reports whether key may attempt another login right now, pruning
// failures older than loginAttemptWindow (and dropping the key entirely
// once it has none left, so a long-running process doesn't accumulate an
// ever-growing map of one-time visitors).
func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-loginAttemptWindow)
	// Timestamps are appended in ascending order (recordFailure only ever
	// appends time.Now()), so the expired ones are always a prefix — a
	// forward scan to the first survivor plus a reslice drops them without
	// allocating a new backing array every call, unlike rebuilding via
	// append into a fresh slice.
	times := l.fails[key]
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	times = times[i:]
	if len(times) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = times
	}
	return len(times) < loginAttemptLimit
}

// recordFailure logs a failed login attempt for key; allow() consults (and
// prunes) this timestamp list to decide when to start rejecting further
// attempts.
func (l *loginLimiter) recordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.fails[key], time.Now())
}

// recordSuccess clears key's failure count — a correct password should
// immediately restore the full attempt budget, not leave a lingering
// partial lockout from earlier typos.
func (l *loginLimiter) recordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// checkLoginRateLimit enforces globalLoginLimiter for r's caller and, if
// the attempt budget is exhausted, writes the standard 429 response itself
// — shared by handleLDAPLogin and handleAdminCheck (handlers.go), which
// otherwise each repeated the clientKey/allow/429 preamble verbatim.
// Returns the client key (for the caller's later recordSuccess/
// recordFailure call) and whether the caller may proceed.
func checkLoginRateLimit(w http.ResponseWriter, r *http.Request) (key string, ok bool) {
	key = clientKey(r)
	if !globalLoginLimiter.allow(key) {
		writeJSONError(w, "too many failed attempts — try again later", http.StatusTooManyRequests)
		return key, false
	}
	return key, true
}

// ─────────────────────────────────────────────────────────────────────────────
// requestLimiter is a generic per-key sliding-window request counter, used
// by handleAsk to bound how often an anonymous caller may hit /api/ask
// (see docs/TODO.md C6) — unlike loginLimiter above, every call counts
// (not just failures), since the cost here is the LLM/embedding call
// itself succeeding, not a wrong password. Kept as its own type rather
// than generalizing loginLimiter, since the two have different semantics
// (allow-then-optionally-record vs. always-record-on-check).
// ─────────────────────────────────────────────────────────────────────────────

type requestLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var globalAskLimiter = &requestLimiter{hits: map[string][]time.Time{}}

// globalVoiceLimiter is requestLimiter's second instance, guarding
// /api/voice/transcribe (handlers_voice.go) the same way globalAskLimiter
// guards /api/ask — a separate instance (not a shared budget) since the two
// endpoints have independent per-minute caps (apiConfig.
// GuestVoiceRateLimitPerMinute vs GuestAskRateLimitPerMinute).
var globalVoiceLimiter = &requestLimiter{hits: map[string][]time.Time{}}

// allow reports whether key may make another request right now, given at
// most limit requests per window — and records this attempt as one of
// them if so. limit <= 0 always allows (caller's opt-out, matching the
// "0 disables" convention used elsewhere in settings.go).
func (l *requestLimiter) allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-window)
	// Same in-place-reslice pruning as loginLimiter.allow above — hits are
	// appended in ascending order, so the expired ones are always a prefix.
	times := l.hits[key]
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	times = times[i:]
	if len(times) >= limit {
		l.hits[key] = times
		return false
	}
	l.hits[key] = append(times, time.Now())
	return true
}
