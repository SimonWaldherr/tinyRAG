package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGraphBackoffHonorsRetryAfter(t *testing.T) {
	if got := graphBackoff(0, 5*time.Second); got != 5*time.Second {
		t.Fatalf("want Retry-After to win, got %v", got)
	}
}

func TestGraphBackoffExponentialWithoutRetryAfter(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 500 * time.Millisecond},
		{1, 1 * time.Second},
		{2, 2 * time.Second},
	}
	for _, c := range cases {
		if got := graphBackoff(c.attempt, 0); got != c.want {
			t.Errorf("attempt %d: want %v, got %v", c.attempt, c.want, got)
		}
	}
}

func TestGraphBackoffCapsAt30Seconds(t *testing.T) {
	if got := graphBackoff(10, 0); got != 30*time.Second {
		t.Fatalf("want capped at 30s, got %v", got)
	}
}

// TestGraphBackoffJitterHonorsRetryAfterExactly confirms jitter is never
// added on top of the server's own Retry-After hint — that's an explicit
// server directive, not a value to randomize.
func TestGraphBackoffJitterHonorsRetryAfterExactly(t *testing.T) {
	if got := graphBackoffJitter(0, 5*time.Second); got != 5*time.Second {
		t.Fatalf("want Retry-After honored exactly with no jitter, got %v", got)
	}
}

// TestGraphBackoffJitterAddsBoundedJitter confirms the exponential-backoff
// branch (no Retry-After) adds up to +25% random jitter on top of the base
// delay — so concurrent retries against the same throttled tenant spread
// out instead of firing in lockstep — without ever going below the base.
func TestGraphBackoffJitterAddsBoundedJitter(t *testing.T) {
	base := graphBackoff(2, 0)
	max := base + base/4 + time.Millisecond
	for i := 0; i < 50; i++ {
		got := graphBackoffJitter(2, 0)
		if got < base {
			t.Fatalf("jittered backoff %v should never be less than the base %v", got, base)
		}
		if got > max {
			t.Fatalf("jittered backoff %v exceeds the documented +25%% bound over base %v", got, base)
		}
	}
}

// TestGraphGetRetriesOn429ThenSucceeds confirms a single 429 doesn't fail
// the call outright — the concrete failure mode ANLEITUNG.md's SharePoint
// section flags ("ein großer Indexierungslauf bricht einfach ab").
func TestGraphGetRetriesOn429ThenSucceeds(t *testing.T) {
	var attempts int
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true}`))
	})

	raw, err := graphGet(context.Background(), "fake-token", "/whatever")
	if err != nil {
		t.Fatalf("graphGet: %v", err)
	}
	if !strings.Contains(string(raw), `"ok": true`) {
		t.Fatalf("unexpected body: %s", raw)
	}
	if attempts != 2 {
		t.Fatalf("want exactly 2 attempts (1 retry), got %d", attempts)
	}
}

// TestGraphGetGivesUpAfterMaxRetries lowers graphMaxRetries for the
// duration of this test so it doesn't sleep through the full real backoff
// schedule (graphMaxRetries is a var for exactly this reason).
func TestGraphGetGivesUpAfterMaxRetries(t *testing.T) {
	orig := graphMaxRetries
	graphMaxRetries = 1
	t.Cleanup(func() { graphMaxRetries = orig })

	var attempts int
	newFakeGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := graphGet(context.Background(), "fake-token", "/whatever")
	if err == nil {
		t.Fatalf("want error after exhausting retries, got nil")
	}
	if attempts != graphMaxRetries+1 {
		t.Fatalf("want %d attempts, got %d", graphMaxRetries+1, attempts)
	}
}

// TestGraphAccessTokenRetriesOn429ThenSucceeds confirms the token endpoint
// itself gets the same retry treatment — a busy tenant can throttle logins
// independently of the Graph API proper. newFakeGraphServer's token
// handler always returns a fixed 200, so this test drives its own fake
// server instead, mirroring that helper's setup/teardown shape.
func TestGraphAccessTokenRetriesOn429ThenSucceeds(t *testing.T) {
	var attempts int
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token": "fake-token", "expires_in": 3600}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	origAuth := graphAuthHost
	graphAuthHost = server.URL
	t.Cleanup(func() { graphAuthHost = origAuth })
	graphTokensMu.Lock()
	graphTokens = map[string]*graphTokenCache{}
	graphTokensMu.Unlock()
	t.Cleanup(func() {
		graphTokensMu.Lock()
		graphTokens = map[string]*graphTokenCache{}
		graphTokensMu.Unlock()
	})

	creds := graphCreds{TenantID: "tenant", ClientID: "client", ClientSecret: "secret"}
	token, err := graphAccessToken(context.Background(), creds)
	if err != nil {
		t.Fatalf("graphAccessToken: %v", err)
	}
	if token != "fake-token" {
		t.Fatalf("want fake-token, got %q", token)
	}
	if attempts != 2 {
		t.Fatalf("want exactly 2 attempts (1 retry), got %d", attempts)
	}
}
