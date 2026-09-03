package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTracingTransportCapturesAndRedacts is the security-critical
// guarantee of the whole "Details anzeigen" feature: credentials in
// headers and bodies (both directions) must never reach the captured
// exchange, while the surrounding structure stays inspectable.
func TestTracingTransportCapturesAndRedacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"live-token-xyz","expires_in":3600,"note":"hello"}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: tracingTransport{}}
	ctx, trace := withConnTrace(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/rest-api/v1/tokens",
		strings.NewReader(`{"userLogin":"u","password":"super-geheim","clientSecret":"cs-geheim"}`))
	req.Header.Set("Authorization", "Bearer secret-bearer-123")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	ex := trace.snapshot()
	if len(ex) != 1 {
		t.Fatalf("want 1 captured exchange, got %d", len(ex))
	}
	all := ex[0].Request + ex[0].Response
	for _, leaked := range []string{"secret-bearer-123", "super-geheim", "cs-geheim", "live-token-xyz"} {
		if strings.Contains(all, leaked) {
			t.Errorf("credential %q leaked into the captured exchange", leaked)
		}
	}
	if !strings.Contains(ex[0].Request, "userLogin") {
		t.Error("non-secret request content should remain visible")
	}
	if !strings.Contains(ex[0].Response, `"note"`) {
		t.Errorf("non-secret response content should remain visible, got %q", ex[0].Response)
	}
	if !strings.Contains(ex[0].Label, "200") {
		t.Errorf("label should carry the status, got %q", ex[0].Label)
	}
}

// TestTracingTransportPassThroughWithoutTrace guards the zero-overhead
// contract for normal traffic: no trace in the context → nothing captured,
// response fully intact.
func TestTracingTransportPassThroughWithoutTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("plain"))
	}))
	defer srv.Close()

	client := &http.Client{Transport: tracingTransport{}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 16)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "plain" {
		t.Fatalf("body must pass through untouched, got %q", buf[:n])
	}
}

// TestConnTraceFormEncodedRedaction covers graph.go's token request shape
// (form-encoded client_secret) — a different body format than the JSON
// case above.
func TestConnTraceFormEncodedRedaction(t *testing.T) {
	got := redactConnTraceText("grant_type=client_credentials&client_id=abc&client_secret=form-geheim&scope=x")
	if strings.Contains(got, "form-geheim") {
		t.Fatalf("form-encoded client_secret leaked: %q", got)
	}
	if !strings.Contains(got, "client_id=abc") {
		t.Fatalf("non-secret form fields must stay visible: %q", got)
	}
}
