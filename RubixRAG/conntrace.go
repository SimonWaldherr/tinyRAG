package main

import (
	"context"
	"net/http"
	"net/http/httputil"
	"regexp"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// Roh-Mitschnitt für "Verbindung testen": ein kontext-gesteuerter
// http.RoundTripper-Wrapper, der — NUR wenn der Request-Context einen
// *connTrace trägt — den kompletten Request und die komplette Response
// (Header + Body) via httputil dumpt, Secrets redigiert und dem Trace
// anhängt. Für normalen Traffic (kein Trace im Context) ist RoundTrip ein
// reiner Durchgriff ohne Mehrarbeit — dieselbe Opt-in-Mechanik wie
// llm.go's debugTrace.
//
// Der Wrapper wird einmalig um die vier HTTP-Client-Pools gelegt
// (connector.go's connectorHTTPClient, shop.go's shopClient, llm.go's
// lmClient, graph.go's graphHTTPClient); nur die conntest.go-Handler
// erzeugen je Testklick einen Trace-Context. Redaktion passiert VOR dem
// Speichern im Trace, nie erst im Frontend — ein Bearer-Token/Passwort
// darf den Server gar nicht erst in Richtung Browser verlassen.
// ─────────────────────────────────────────────────────────────────────────────

// connExchange is one captured request/response pair, already redacted.
type connExchange struct {
	Label    string `json:"label"` // "POST https://host/path → 200 OK"
	Request  string `json:"request"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"` // network-level failure (no response)
}

// connTrace collects every exchange one connection test performed —
// mutex-guarded since a single test can fan out (token + search, retries).
type connTrace struct {
	mu        sync.Mutex
	Exchanges []connExchange
}

func (t *connTrace) add(e connExchange) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Hard cap: a retry loop must not grow the trace unboundedly.
	if len(t.Exchanges) >= 20 {
		return
	}
	t.Exchanges = append(t.Exchanges, e)
}

func (t *connTrace) snapshot() []connExchange {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]connExchange{}, t.Exchanges...)
}

type connTraceContextKey struct{}

// withConnTrace returns ctx augmented with a fresh trace plus the trace
// itself for reading back after the test ran.
func withConnTrace(ctx context.Context) (context.Context, *connTrace) {
	t := &connTrace{}
	return context.WithValue(ctx, connTraceContextKey{}, t), t
}

func connTraceFromContext(ctx context.Context) *connTrace {
	t, _ := ctx.Value(connTraceContextKey{}).(*connTrace)
	return t
}

// connTraceRedactions strips every credential-shaped value from a dumped
// exchange — headers (Authorization, api-key, cookies) and body fields
// (JSON and form-encoded passwords/secrets/tokens, including tokens the
// SERVER returns, e.g. access_token in a token response). Field-name
// lists mirror isSecretSettingsPath's markers plus the token-response
// shapes shop.go/graph.go actually parse.
var connTraceRedactions = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^(authorization:[ \t]*).+$`),
	regexp.MustCompile(`(?im)^(api-key:[ \t]*).+$`),
	regexp.MustCompile(`(?im)^(cookie:[ \t]*).+$`),
	regexp.MustCompile(`(?im)^(set-cookie:[ \t]*).+$`),
	regexp.MustCompile(`(?i)("(?:password|client_?secret|api_?key|api_?token|access_?token|refresh_?token|token)"\s*:\s*)"(?:[^"\\]|\\.)*"`),
	regexp.MustCompile(`(?i)\b(password|client_secret|api_key|api_token|access_token|refresh_token)=[^&\s]+`),
}

func redactConnTraceText(s string) string {
	for _, re := range connTraceRedactions {
		s = re.ReplaceAllString(s, "${1}[REDIGIERT]")
	}
	return s
}

// connTraceMaxDumpChars caps one dumped request/response — an LLM chat
// body or a big search response stays inspectable without shipping
// megabytes to the admin's browser.
const connTraceMaxDumpChars = 16384

// tracingTransport wraps base and captures exchanges for requests whose
// context opted in via withConnTrace. Base nil falls back to
// http.DefaultTransport, mirroring net/http's own convention.
type tracingTransport struct {
	base http.RoundTripper
}

func (t tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	trace := connTraceFromContext(req.Context())
	if trace == nil {
		return base.RoundTrip(req)
	}

	// DumpRequestOut/DumpResponse both restore the body they read
	// (drainBody), so capturing never consumes the real exchange.
	reqDump, _ := httputil.DumpRequestOut(req, true)
	resp, err := base.RoundTrip(req)
	e := connExchange{
		Label:   req.Method + " " + req.URL.String(),
		Request: redactConnTraceText(truncateRunesNote(string(reqDump), connTraceMaxDumpChars)),
	}
	if err != nil {
		e.Error = err.Error()
		trace.add(e)
		return resp, err
	}
	e.Label += " → " + resp.Status
	respDump, dumpErr := httputil.DumpResponse(resp, true)
	if dumpErr != nil {
		e.Error = "response dump failed: " + dumpErr.Error()
	} else {
		e.Response = redactConnTraceText(truncateRunesNote(string(respDump), connTraceMaxDumpChars))
	}
	trace.add(e)
	return resp, err
}
