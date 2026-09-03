package main

import (
	"testing"
	"time"
)

func TestLoginLimiterLocksOutAfterThreshold(t *testing.T) {
	l := &loginLimiter{fails: map[string][]time.Time{}}
	key := "10.0.0.1"
	for i := 0; i < loginAttemptLimit; i++ {
		if !l.allow(key) {
			t.Fatalf("attempt %d: expected allow, got locked out", i)
		}
		l.recordFailure(key)
	}
	if l.allow(key) {
		t.Fatal("expected lockout after loginAttemptLimit consecutive failures")
	}
}

func TestLoginLimiterSuccessClearsFailures(t *testing.T) {
	l := &loginLimiter{fails: map[string][]time.Time{}}
	key := "10.0.0.2"
	for i := 0; i < loginAttemptLimit; i++ {
		l.recordFailure(key)
	}
	if l.allow(key) {
		t.Fatal("expected lockout before recordSuccess")
	}
	l.recordSuccess(key)
	if !l.allow(key) {
		t.Fatal("expected recordSuccess to clear the failure count")
	}
}

func TestLoginLimiterExpiresOldFailures(t *testing.T) {
	l := &loginLimiter{fails: map[string][]time.Time{}}
	key := "10.0.0.5"
	old := time.Now().Add(-loginAttemptWindow - time.Minute)
	l.fails[key] = make([]time.Time, loginAttemptLimit)
	for i := range l.fails[key] {
		l.fails[key][i] = old
	}
	if !l.allow(key) {
		t.Fatal("failures outside the window should not count toward the lockout")
	}
	if _, exists := l.fails[key]; exists {
		t.Fatal("expired-only entries should be pruned from the map, not kept as an empty slice")
	}
}

func TestLoginLimiterKeysAreIndependent(t *testing.T) {
	l := &loginLimiter{fails: map[string][]time.Time{}}
	for i := 0; i < loginAttemptLimit; i++ {
		l.recordFailure("10.0.0.3")
	}
	if l.allow("10.0.0.3") {
		t.Fatal("10.0.0.3 should be locked out")
	}
	if !l.allow("10.0.0.4") {
		t.Fatal("a different client key should be unaffected")
	}
}
