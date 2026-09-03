package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestShopAccessTokenCachesAcrossCalls covers the token-cache contract
// (mirrors graph.go's graphAccessToken): a second call within the token's
// lifetime must reuse the cached token instead of hitting the server
// again.
func TestShopAccessTokenCachesAcrossCalls(t *testing.T) {
	var tokenRequests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest-api/v1/tokens" {
			t.Errorf("want POST to /rest-api/v1/tokens, got %s %s", r.Method, r.URL.Path)
		}
		atomic.AddInt32(&tokenRequests, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "expires_in": 3600})
	}))
	defer srv.Close()

	cfg := shopConfig{BaseURL: srv.URL, Username: "svc-user", Password: "secret", ClientID: "test-client", ClientSecret: "test-secret"}

	session1, err := shopAccessToken(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if session1.token != "tok-1" {
		t.Fatalf("want token %q, got %q", "tok-1", session1.token)
	}

	session2, err := shopAccessToken(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if session2.token != session1.token {
		t.Fatalf("want cached token reused, got %q then %q", session1.token, session2.token)
	}
	if got := atomic.LoadInt32(&tokenRequests); got != 1 {
		t.Fatalf("want exactly 1 token request (second call served from cache), got %d", got)
	}
}

// TestShopAccessTokenRefreshesAfterExpiry covers the other half: once the
// cached token's expiry (minus the 60s safety buffer) has passed, the next
// call must fetch a fresh one rather than returning the stale value.
func TestShopAccessTokenRefreshesAfterExpiry(t *testing.T) {
	var tokenRequests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&tokenRequests, 1)
		w.Header().Set("Content-Type", "application/json")
		// expires_in of 61 is the smallest value that clears the "<=60 use
		// the 1800s fallback" guard in shopAccessToken while still expiring
		// (after the 60s buffer) almost immediately, so the test doesn't
		// need to sleep a long time.
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "expires_in": 61})
	}))
	defer srv.Close()

	cfg := shopConfig{BaseURL: srv.URL, Username: "expiry-user", Password: "secret", ClientID: "test-client", ClientSecret: "test-secret"}

	session1, err := shopAccessToken(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Force the cached entry into the past directly, rather than sleeping
	// out a real ~1s window — same "reach into the cache" approach as
	// pst_test.go's TestListPSTImportJobsOrdersByMostRecentFirst.
	shopTokensMu.Lock()
	cache := shopTokens[shopCacheKey(cfg)]
	shopTokensMu.Unlock()
	cache.mu.Lock()
	cache.expires = time.Now().Add(-time.Second)
	cache.mu.Unlock()

	session2, err := shopAccessToken(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if session2.token == session1.token {
		t.Fatalf("want a refreshed token after forced expiry, got the same %q twice", session1.token)
	}
	if got := atomic.LoadInt32(&tokenRequests); got != 2 {
		t.Fatalf("want exactly 2 token requests (one per fetch), got %d", got)
	}
}

// TestShopAccessTokenRequiresCredentials guards the two "not configured"
// error paths — no network call should even be attempted.
func TestShopAccessTokenRequiresCredentials(t *testing.T) {
	if _, err := shopAccessToken(context.Background(), shopConfig{}); err == nil {
		t.Fatal("want an error when username is empty")
	}
	if _, err := shopAccessToken(context.Background(), shopConfig{Username: "u"}); err == nil {
		t.Fatal("want an error when password is empty")
	}
}

// shopSearchRealCaptureJSON is a real, complete /rest-api/v4/search-items
// response — captured live from an authenticated de.rubix.com session
// (searchText=schraube, pageSize=3), not synthesized — see shop.go's
// package comment. Used to verify parseShopSearchItems against the actual
// confirmed shape rather than a plausible-looking guess.
const shopSearchRealCaptureJSON = `{"facets":[{"code":"categoryLevel3","name":"Produktgruppe","priority":300,"type":"STRING","isMultiSelect":false,"values":[{"code":"50-20-10","name":"Bolzen","count":9653,"isSelected":false}]}],"pageNumber":0,"pageSize":3,"itemsTotalCount":394,"items":[{"erpSkuId":"3378205","slug":"selbstbohrende-schrauben-mit-zylindrischem-kopf-form-m","brand":{"code":"2871","slug":"spartex","brandName":"Spartex","productName":"Schraube, selbstbohrend Spartex VAPTF4,8X38TXEFA2, Durchmesser, metrisch: M4 | Länge: 38 mm | Norm: DIN 7504","productReference":"VAPTF4,8X38TXEFA2","secondaryProductReference":"75042OT48 38"},"images":[],"id":"G5020059865","ean":"4043377240040","perimeter":"DEFAULT","range":{"name":"Selbstbohrende Schrauben mit zylindrischem Kopf, Form M","slug":"selbstbohrende-schrauben-mit-zylindrischem-kopf-form-m","id":"PR_G5020059025","skusTotalCount":1218},"attributes":[{"name":"Gewindedurchmesser","classification":"MANDATORY","type":"STRING","value":"4,8","unit":"mm"},{"name":"Länge","classification":"MANDATORY","type":"NUMERIC","value":"38","unit":"mm"},{"name":"Material/Verarbeitung","classification":"MANDATORY","type":"STRING","value":"edelstahl a2"}],"categoryPath":[{"code":"50"}]},{"erpSkuId":"853724","slug":"sicherheitsschraube-tc-nippel-metrisches-gewinde","brand":{"code":"2871","slug":"spartex","brandName":"Spartex","productName":"Metallschraube mit metrischem Gewinde Spartex VSTB6X16TXEFA2, Durchmesser, metrisch: M6 | Länge: 16 mm | Norm: DIN 7380","productReference":"VSTB6X16TXEFA2"},"images":[],"id":"G5025013234","ean":"4043377132031","perimeter":"DEFAULT","range":{"name":"Sicherheitsschraube TC Nippel, metrisches Gewinde","slug":"sicherheitsschraube-tc-nippel-metrisches-gewinde","id":"PR_G5025013177","skusTotalCount":71},"attributes":[{"name":"Gewindedurchmesser","classification":"MANDATORY","type":"NUMERIC","value":"6","unit":"mm"},{"name":"Länge","classification":"MANDATORY","type":"NUMERIC","value":"16","unit":"mm"},{"name":"Material/Verarbeitung","classification":"MANDATORY","type":"STRING","value":"edelstahl a2"}],"categoryPath":[{"code":"50"}]},{"erpSkuId":"3040320","slug":"set-th-schrauben-mit-vollgewinde-stahl-verzinkt","brand":{"code":"2871","slug":"spartex","brandName":"Spartex","productName":"Sortiment Kiste Spartex 855181, Typ: Verzinktem Stahl | Anzahl Elemente: 400","productReference":"855181"},"images":[],"id":"G1408001822","ean":"3660338106453","perimeter":"DEFAULT","range":{"name":"Set TH-Schrauben mit Vollgewinde Stahl verzinkt.","slug":"set-th-schrauben-mit-vollgewinde-stahl-verzinkt","id":"PR_G1408001822","skusTotalCount":1},"attributes":[{"name":"Typ","classification":"MANDATORY","type":"STRING","value":"verzinktem stahl"}],"categoryPath":[{"code":"50"}]}],"searchText":"schraube","conditions":[{"code":"context","values":["AUTOSUGGEST"]}]}`

// TestParseShopSearchItemsRealCapture verifies parsing against the actual
// confirmed response shape (shopSearchRealCaptureJSON) — erpSkuId (not
// "id", a different unrelated catalog identifier) as the SKU, the product
// name from brand.productName (not any top-level "name" field, which
// doesn't exist in the real response), manufacturer from brand.brandName,
// and only MANDATORY-classified attributes carried through.
func TestParseShopSearchItemsRealCapture(t *testing.T) {
	items, total, err := parseShopSearchItems([]byte(shopSearchRealCaptureJSON), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 394 {
		t.Errorf("want itemsTotalCount 394 surfaced, got %d", total)
	}
	if len(items) != 3 {
		t.Fatalf("want all 3 items parsed, got %d: %+v", len(items), items)
	}
	first := items[0]
	if first.ErpSkuID != "3378205" {
		t.Errorf("want ErpSkuID 3378205 (not the unrelated top-level \"id\" G5020059865), got %q", first.ErpSkuID)
	}
	if first.ProductID != "G5020059865" {
		t.Errorf("want ProductID to still carry the top-level \"id\", got %q", first.ProductID)
	}
	if first.Name != "Schraube, selbstbohrend Spartex VAPTF4,8X38TXEFA2, Durchmesser, metrisch: M4 | Länge: 38 mm | Norm: DIN 7504" {
		t.Errorf("want Name from brand.productName, got %q", first.Name)
	}
	if first.Manufacturer != "Spartex" {
		t.Errorf("want Manufacturer from brand.brandName, got %q", first.Manufacturer)
	}
	if first.EAN != "4043377240040" {
		t.Errorf("want EAN populated, got %q", first.EAN)
	}
	if first.RangeName != "Selbstbohrende Schrauben mit zylindrischem Kopf, Form M" {
		t.Errorf("want RangeName from range.name, got %q", first.RangeName)
	}
	if len(first.Attributes) != 3 {
		t.Fatalf("want all 3 MANDATORY attributes carried through, got %+v", first.Attributes)
	}
	if first.Attributes[0].Name != "Gewindedurchmesser" || first.Attributes[0].Value != "4,8" || first.Attributes[0].Unit != "mm" {
		t.Errorf("want first attribute populated, got %+v", first.Attributes[0])
	}
	// The third item's attribute has no "unit" key at all in the real
	// capture (a plain STRING classification, "Typ": "verzinktem stahl") —
	// must not error or panic on an absent field.
	third := items[2]
	if len(third.Attributes) != 1 || third.Attributes[0].Value != "verzinktem stahl" || third.Attributes[0].Unit != "" {
		t.Errorf("want the unitless attribute parsed with an empty Unit, got %+v", third.Attributes)
	}
}

// TestParseShopSearchItemsUnrecognizedShapeReturnsEmpty covers a response
// that doesn't match the confirmed shape at all: must return an empty
// slice with no error, not crash — an unexpected shape should degrade to
// "no results", not break the whole chat/agent/mail turn.
func TestParseShopSearchItemsUnrecognizedShapeReturnsEmpty(t *testing.T) {
	raw := []byte(`{"somethingElseEntirely": {"nested": true}}`)
	items, total, err := parseShopSearchItems(raw, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 || total != 0 {
		t.Fatalf("want an empty result for an unrecognized shape, got items=%+v total=%d", items, total)
	}
}

// TestParseShopSearchItemsRespectsLimit ensures the limit is actually
// enforced client-side too, not just via the pageSize request parameter.
func TestParseShopSearchItemsRespectsLimit(t *testing.T) {
	raw := []byte(`{"itemsTotalCount": 3, "items": [
		{"erpSkuId": "A", "brand": {"productName": "Item A"}},
		{"erpSkuId": "B", "brand": {"productName": "Item B"}},
		{"erpSkuId": "C", "brand": {"productName": "Item C"}}
	]}`)
	items, total, err := parseShopSearchItems(raw, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want limit of 2 respected, got %d items", len(items))
	}
	if total != 3 {
		t.Errorf("want itemsTotalCount surfaced regardless of the client-side limit, got %d", total)
	}
}

// TestShopTokenRequestBypassesCache covers the "Login testen" button's
// underlying call (handleTestShopLogin, conntest.go): unlike
// shopAccessToken, shopTokenRequest must hit the server fresh every time,
// never returning a cached token — an admin re-clicking "Login testen"
// after fixing credentials must see the *new* attempt's result, not a
// stale cached one.
func TestShopTokenRequestBypassesCache(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 3600})
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "u"}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}

	if _, err := shopTokenRequest(context.Background(), cfg, "pw", jar); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := shopTokenRequest(context.Background(), cfg, "pw", jar); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("want 2 real server hits (no caching in shopTokenRequest), got %d", got)
	}
}

// TestShopTokenRequestReturnsRawBodyOnFailure covers what handleTestShopLogin
// actually shows the admin: a non-200 response's status and raw body must
// both come back, not just a generic error, so the raw (unconfirmed)
// response shape is visible for debugging.
func TestShopTokenRequestReturnsRawBodyOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"UNAUTHORIZED","message":"bad credentials"}`))
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "u"}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}

	attempt, err := shopTokenRequest(context.Background(), cfg, "wrong-pw", jar)
	if err != nil {
		t.Fatalf("want no Go-level error for a well-formed 401 response, got %v", err)
	}
	if attempt.status != http.StatusUnauthorized {
		t.Errorf("want status 401, got %d", attempt.status)
	}
	if !strings.Contains(string(attempt.raw), "UNAUTHORIZED") {
		t.Errorf("want the raw response body preserved, got %q", string(attempt.raw))
	}
}

// TestShopTokenRequestReportsContentTypeAndFinalURL covers the diagnostic
// fields added alongside status/raw: an HTTP 200 with an empty body is
// otherwise indistinguishable from "field names changed" (both currently
// fail parseShopTokenResponse the same way) — Content-Type and the final
// (post-redirect) URL are what let handleTestShopLogin tell those apart.
func TestShopTokenRequestReportsContentTypeAndFinalURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// Deliberately empty body — the "request never reached the real
		// login handler" case this test exists to cover.
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "u"}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}

	attempt, err := shopTokenRequest(context.Background(), cfg, "pw", jar)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempt.status != http.StatusOK {
		t.Errorf("want status 200, got %d", attempt.status)
	}
	if len(attempt.raw) != 0 {
		t.Errorf("want an empty body, got %q", attempt.raw)
	}
	if attempt.contentType != "text/html; charset=utf-8" {
		t.Errorf("want the response Content-Type surfaced, got %q", attempt.contentType)
	}
	if attempt.finalURL != srv.URL+"/rest-api/v1/tokens" {
		t.Errorf("want the final request URL surfaced (no redirect here), got %q", attempt.finalURL)
	}
	if attempt.cookiesSet {
		t.Errorf("want cookiesSet false when the server sets no Set-Cookie header")
	}
}

// TestShopTokenRequestDetectsCookieSession covers the other empty-body
// case: no JSON token field, but the server set session cookies (and a
// Userid header) anyway — the actual de.rubix.com behavior this feature
// was added for, distinct from the "request never reached the login
// handler" case above.
func TestShopTokenRequestDetectsCookieSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SESSION=abc123; Path=/; HttpOnly")
		w.Header().Set("Userid", "6310657212929")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "u"}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}

	attempt, err := shopTokenRequest(context.Background(), cfg, "pw", jar)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !attempt.cookiesSet {
		t.Errorf("want cookiesSet true when the server sets Set-Cookie")
	}
	if attempt.userID != "6310657212929" {
		t.Errorf("want the Userid header surfaced, got %q", attempt.userID)
	}
}

// TestParseShopTokenResponseRejectsUnrecognizedShape covers the "200 but no
// recognizable token field" case handleTestShopLogin reports distinctly
// from an outright HTTP failure.
func TestParseShopTokenResponseRejectsUnrecognizedShape(t *testing.T) {
	_, _, err := parseShopTokenResponse([]byte(`{"somethingElse": "value"}`))
	if err == nil {
		t.Fatal("want an error when no candidate token field is present")
	}
}

func TestParseShopTokenResponseExtractsToken(t *testing.T) {
	token, expiresIn, err := parseShopTokenResponse([]byte(`{"access_token":"abc123","expires_in":7200}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "abc123" || expiresIn != 7200 {
		t.Errorf("want token=abc123 expiresIn=7200, got token=%q expiresIn=%v", token, expiresIn)
	}
}

// TestShopAccessTokenAndSearchUseCookieSession covers the full cookie-
// session path end to end (shopAccessToken's fallback plus
// searchShopItemsCached using the resulting session): a login response
// with no JSON body but real Set-Cookie headers must still yield a usable
// session — the search request must carry that cookie and must NOT send
// an Authorization header (there is no bearer token in this flow).
func TestShopAccessTokenAndSearchUseCookieSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			w.Header().Set("Set-Cookie", "SESSION=abc123; Path=/; HttpOnly")
			w.Header().Set("Userid", "6310657212929")
			w.WriteHeader(http.StatusOK)
		case "/rest-api/v4/search-items":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("want no Authorization header in a cookie-session search, got %q", got)
			}
			cookie, cookieErr := r.Cookie("SESSION")
			if cookieErr != nil || cookie.Value != "abc123" {
				t.Errorf("want the login's session cookie sent on the search request, got err=%v cookie=%v", cookieErr, cookie)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"itemsTotalCount": 1, "items": []map[string]any{{"erpSkuId": "SKU-1", "brand": map[string]any{"productName": "Schraube"}}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "cookie-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	session, err := shopAccessToken(context.Background(), cfg)
	if err != nil {
		t.Fatalf("shopAccessToken: %v", err)
	}
	if session.token != "" {
		t.Errorf("want no bearer token in a cookie-session login, got %q", session.token)
	}
	if session.jar == nil {
		t.Fatal("want a cookie jar on a cookie-session login")
	}

	items, _, err := searchShopItemsCached(context.Background(), cfg, "schraube", 5, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 || items[0].ErpSkuID != "SKU-1" {
		t.Fatalf("want the search to still succeed over the cookie session, got %+v", items)
	}
}

// shopSearchTestServer builds a test server serving both the token and
// search-items endpoints, counting search-items requests separately so
// caching/retry tests can assert exactly how many real searches happened.
// failFirstN search requests get a 503 before the handler starts
// succeeding — 0 to always succeed.
func shopSearchTestServer(t *testing.T, searchAttempts *int32, failFirstN int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case "/rest-api/v4/search-items":
			n := atomic.AddInt32(searchAttempts, 1)
			if n <= failFirstN {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"itemsTotalCount": 1, "items": []map[string]any{{"erpSkuId": "SKU-1", "brand": map[string]any{"productName": "Schraube"}}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

// TestSearchShopItemsReauthenticatesOnceOn401 covers the "cached session
// looked valid but the server rejected it anyway" case: a search that gets
// a 401 despite a not-yet-expired cached session must invalidate that
// session, log in again exactly once, and retry — not fail outright, and
// not loop forever on a genuinely bad account either.
func TestSearchShopItemsReauthenticatesOnceOn401(t *testing.T) {
	var tokenRequests, searchAttempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			n := atomic.AddInt32(&tokenRequests, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "expires_in": 3600})
		case "/rest-api/v4/search-items":
			n := atomic.AddInt32(&searchAttempts, 1)
			if n == 1 {
				// First attempt's token looked cached-valid client-side but
				// is rejected server-side — the case this feature exists for.
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer tok-2" {
				t.Errorf("want the retry to use the freshly re-issued token, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"itemsTotalCount": 1, "items": []map[string]any{{"erpSkuId": "SKU-1", "brand": map[string]any{"productName": "Schraube"}}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "reauth-test-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	items, _, err := searchShopItemsCached(context.Background(), cfg, "schraube", 5, false)
	if err != nil {
		t.Fatalf("want the 401 to trigger a reauth-and-retry, not a hard failure: %v", err)
	}
	if len(items) != 1 || items[0].ErpSkuID != "SKU-1" {
		t.Fatalf("want 1 item after the reauth retry, got %+v", items)
	}
	if got := atomic.LoadInt32(&tokenRequests); got != 2 {
		t.Fatalf("want exactly 2 token requests (initial + one forced re-login), got %d", got)
	}
	if got := atomic.LoadInt32(&searchAttempts); got != 2 {
		t.Fatalf("want exactly 2 search attempts (401 + successful retry), got %d", got)
	}
}

// TestSearchShopItemsFailsAfterSecondConsecutive401 guards the "bounded to
// one retry" half: a search that keeps returning 401 even after the forced
// re-login must fail, not loop forever re-authenticating.
func TestSearchShopItemsFailsAfterSecondConsecutive401(t *testing.T) {
	var searchAttempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case "/rest-api/v4/search-items":
			atomic.AddInt32(&searchAttempts, 1)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "always-401-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	if _, _, err := searchShopItemsCached(context.Background(), cfg, "schraube", 5, false); err == nil {
		t.Fatal("want an error when the search still 401s after the one forced re-login")
	}
	if got := atomic.LoadInt32(&searchAttempts); got != 2 {
		t.Fatalf("want exactly 2 search attempts (initial + one retry, then give up), got %d", got)
	}
}

// TestSearchShopItemsCachesRepeatedQuery covers the new short-TTL result
// cache: an identical (base URL + username + query + limit) call within
// the cache window must be served from memory instead of hitting the
// search endpoint again — relevant since a chat/agent turn can invoke the
// same tool call more than once.
func TestSearchShopItemsCachesRepeatedQuery(t *testing.T) {
	var searchAttempts int32
	srv := shopSearchTestServer(t, &searchAttempts, 0)
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "cache-test-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	items1, total1, err := searchShopItems(context.Background(), cfg, "schraube", 5)
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	items2, total2, err := searchShopItems(context.Background(), cfg, "schraube", 5)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if got := atomic.LoadInt32(&searchAttempts); got != 1 {
		t.Fatalf("want exactly 1 real search request (second served from cache), got %d", got)
	}
	if len(items1) != 1 || len(items2) != 1 || items1[0].ErpSkuID != items2[0].ErpSkuID {
		t.Fatalf("want the cached result identical to the first, got %+v / %+v", items1, items2)
	}
	if total1 != total2 {
		t.Fatalf("want the cached total identical to the first, got %d / %d", total1, total2)
	}
}

// TestSearchShopItemsCachedBypassesCacheWhenDisabled covers
// handleTestShop's contract (conntest.go): "Verbindung + Suche testen"
// must always be a fresh live request, never served from the same cache
// searchShopItems (the tool-call path) uses.
func TestSearchShopItemsCachedBypassesCacheWhenDisabled(t *testing.T) {
	var searchAttempts int32
	srv := shopSearchTestServer(t, &searchAttempts, 0)
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "no-cache-test-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	if _, _, err := searchShopItemsCached(context.Background(), cfg, "schraube", 5, false); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if _, _, err := searchShopItemsCached(context.Background(), cfg, "schraube", 5, false); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if got := atomic.LoadInt32(&searchAttempts); got != 2 {
		t.Fatalf("want 2 real search requests with caching disabled, got %d", got)
	}
}

// TestSearchShopItemsRetriesOnServerError covers the retry-on-5xx
// behavior added to the search-items request itself (previously only the
// token request retried) — a single transient hiccup must not fail the
// whole tool call.
func TestSearchShopItemsRetriesOnServerError(t *testing.T) {
	origRetries := shopMaxRetries
	shopMaxRetries = 1
	defer func() { shopMaxRetries = origRetries }()

	var searchAttempts int32
	srv := shopSearchTestServer(t, &searchAttempts, 1)
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "retry-test-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	items, _, err := searchShopItemsCached(context.Background(), cfg, "schraube", 5, false)
	if err != nil {
		t.Fatalf("want the retried request to succeed, got %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item after retry, got %+v", items)
	}
	if got := atomic.LoadInt32(&searchAttempts); got != 2 {
		t.Fatalf("want exactly 2 attempts (1 failure + 1 retry), got %d", got)
	}
}

// TestShopSearchToolExecutorRejectsEmptyQuery covers the tool-arguments
// validation path without needing a live shop connection.
func TestShopSearchToolExecutorRejectsEmptyQuery(t *testing.T) {
	exec := shopSearchToolExecutor(shopConfig{})
	if _, err := exec(context.Background(), `{"query":""}`); err == nil {
		t.Fatal("want an error for an empty query")
	}
	if _, err := exec(context.Background(), `not json`); err == nil {
		t.Fatal("want an error for invalid JSON arguments")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Live stock/price (commerce-sku-details) — the second, separately-callable
// tool (package comment, shop.go).
// ─────────────────────────────────────────────────────────────────────────────

// shopSkuDetailsRealCaptureJSON is a real, complete /rest-api/v3/
// commerce-sku-details response for 6 erpSkuIds — captured live, not
// synthesized (see shop.go's package comment). Includes both an in-stock
// item with full availability/price fields and an out-of-stock item
// (3152325) whose availabilities entry omits stockLevel/leadTime/
// addToCartMaximumQuantity entirely (absent, not zero) — parseShopSkuDetails
// must handle that gracefully.
const shopSkuDetailsRealCaptureJSON = `{"items":[{"id":"844821","product":{"canBeAddedToCart":true,"isSellout":false,"needLogInToGetPrice":false,"canSubscribeToStockNotification":false},"stock":{"addToCartMaximumQuantity":114,"availabilities":[{"source":"NATIONAL_STOCK","leadTimeMinimum":1,"leadTimeMaximum":2,"stockLevel":114,"unit":{"code":"ST","nameSingular":"Stück","namePlural":"Stück"},"availabilityStatus":"IN_STOCK"}]},"price":{"isPriceOnRequest":false,"supplementaryTaxes":[],"unit":{"code":"ST","nameSingular":"Stück","namePlural":"Stück"},"volumes":[{"basePrice":1.38,"basePriceIncludingTaxes":1.64,"soldByPrice":1.38,"soldByPriceIncludingTaxes":1.64,"unitPrice":1.38,"unitPriceIncludingTaxes":1.64,"minimumQuantity":1}]}},{"id":"3152325","product":{"canBeAddedToCart":false,"isSellout":false,"needLogInToGetPrice":false,"canSubscribeToStockNotification":false},"stock":{"availabilities":[{"source":"NATIONAL_STOCK","unit":{"code":"ST","nameSingular":"Stück","namePlural":"Stück"},"availabilityStatus":"OUT_OF_STOCK"}]},"price":{"isPriceOnRequest":false,"supplementaryTaxes":[],"unit":{"code":"ST","nameSingular":"Stück","namePlural":"Stück"},"volumes":[{"basePrice":5.02,"basePriceIncludingTaxes":5.97,"soldByPrice":5.02,"soldByPriceIncludingTaxes":5.97,"unitPrice":5.02,"unitPriceIncludingTaxes":5.97,"minimumQuantity":1}]}}],"notFoundItems":["9999999"]}`

// TestParseShopSkuDetailsRealCapture verifies parsing against the actual
// confirmed response shape, including the out-of-stock item whose
// availabilities entry omits several fields entirely rather than zeroing
// them, and the notFoundItems list.
func TestParseShopSkuDetailsRealCapture(t *testing.T) {
	details, notFound, err := parseShopSkuDetails([]byte(shopSkuDetailsRealCaptureJSON), "EUR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("want 2 items parsed, got %d: %+v", len(details), details)
	}
	inStock := details[0]
	if inStock.ErpSkuID != "844821" || !inStock.CanBeAddedToCart {
		t.Errorf("want first item's id/cart-eligibility populated, got %+v", inStock)
	}
	if inStock.AvailabilityStatus != "IN_STOCK" || inStock.StockLevel != 114 || inStock.Unit != "Stück" {
		t.Errorf("want first item's stock fields populated, got %+v", inStock)
	}
	if inStock.LeadTimeMinimumDays != 1 || inStock.LeadTimeMaximumDays != 2 {
		t.Errorf("want lead time populated, got %+v", inStock)
	}
	if inStock.Price != 1.64 || inStock.Currency != "EUR" {
		t.Errorf("want basePriceIncludingTaxes (gross) as Price with the given currency, got price=%v currency=%q", inStock.Price, inStock.Currency)
	}

	outOfStock := details[1]
	if outOfStock.ErpSkuID != "3152325" || outOfStock.CanBeAddedToCart {
		t.Errorf("want the out-of-stock item's cart-eligibility false, got %+v", outOfStock)
	}
	if outOfStock.AvailabilityStatus != "OUT_OF_STOCK" {
		t.Errorf("want OUT_OF_STOCK status, got %q", outOfStock.AvailabilityStatus)
	}
	if outOfStock.StockLevel != 0 || outOfStock.LeadTimeMinimumDays != 0 {
		t.Errorf("want absent stockLevel/leadTime fields to default to zero, not error, got %+v", outOfStock)
	}
	// Price is still present for an out-of-stock item (it's still
	// purchasable-when-restocked, just not right now) — must not be
	// dropped just because availability is zero.
	if outOfStock.Price != 5.97 {
		t.Errorf("want the out-of-stock item's price still populated, got %v", outOfStock.Price)
	}

	if len(notFound) != 1 || notFound[0] != "9999999" {
		t.Errorf("want notFoundItems surfaced verbatim, got %+v", notFound)
	}
}

// TestFetchShopSkuDetailsUsesCommaSeparatedIdsAndCurrencyHeader covers the
// request shape (erpSkuIds joined with commas, matching the endpoint's own
// batch convention) and that the Currency response header (not a body
// field — de.rubix.com signals it via HTTP header, confirmed on both
// endpoints) is picked up and applied to every parsed item.
func TestFetchShopSkuDetailsUsesCommaSeparatedIdsAndCurrencyHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case "/rest-api/v3/commerce-sku-details":
			if got := r.URL.Query().Get("erpSkuIds"); got != "111,222,333" {
				t.Errorf("want comma-separated erpSkuIds, got %q", got)
			}
			w.Header().Set("Currency", "EUR")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"items":         []map[string]any{{"id": "111", "product": map[string]any{"canBeAddedToCart": true}}},
				"notFoundItems": []string{},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "u", Password: "pw", ClientID: "c", ClientSecret: "s"}

	details, notFound, err := fetchShopSkuDetails(context.Background(), cfg, []string{"111", "222", "333"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(details) != 1 || details[0].Currency != "EUR" {
		t.Fatalf("want the Currency header applied to the parsed item, got %+v", details)
	}
	if len(notFound) != 0 {
		t.Errorf("want no not-found items, got %+v", notFound)
	}
}

// TestFetchShopSkuDetailsCurrencyFallsBackWhenHeaderAbsent covers the
// shopDefaultCurrency fallback for a response that (unlike every real
// capture so far) omits the Currency header entirely.
func TestFetchShopSkuDetailsCurrencyFallsBackWhenHeaderAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case "/rest-api/v3/commerce-sku-details":
			// Deliberately no Currency header set.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "111"}}, "notFoundItems": []string{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "u", Password: "pw", ClientID: "c", ClientSecret: "s"}

	details, _, err := fetchShopSkuDetails(context.Background(), cfg, []string{"111"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(details) != 1 || details[0].Currency != shopDefaultCurrency {
		t.Fatalf("want the default currency fallback applied, got %+v", details)
	}
}

// TestFetchShopSkuDetailsReauthenticatesOnceOn401 proves shopAuthedGet's
// extracted 401-reauth-retry behavior (previously only exercised by
// search) also covers the new endpoint — the whole point of factoring it
// out as shared plumbing.
func TestFetchShopSkuDetailsReauthenticatesOnceOn401(t *testing.T) {
	var tokenRequests, detailAttempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			n := atomic.AddInt32(&tokenRequests, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "expires_in": 3600})
		case "/rest-api/v3/commerce-sku-details":
			n := atomic.AddInt32(&detailAttempts, 1)
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer tok-2" {
				t.Errorf("want the retry to use the freshly re-issued token, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "111"}}, "notFoundItems": []string{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "reauth-details-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	details, _, err := fetchShopSkuDetails(context.Background(), cfg, []string{"111"})
	if err != nil {
		t.Fatalf("want the 401 to trigger a reauth-and-retry, not a hard failure: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("want 1 item after the reauth retry, got %+v", details)
	}
	if got := atomic.LoadInt32(&tokenRequests); got != 2 {
		t.Fatalf("want exactly 2 token requests (initial + one forced re-login), got %d", got)
	}
}

// TestShopStockToolExecutorRejectsEmptyIDs covers the tool-arguments
// validation path without needing a live shop connection.
func TestShopStockToolExecutorRejectsEmptyIDs(t *testing.T) {
	exec := shopStockToolExecutor(shopConfig{})
	if _, err := exec(context.Background(), `{"erp_sku_ids":[]}`); err == nil {
		t.Fatal("want an error for empty erp_sku_ids")
	}
	if _, err := exec(context.Background(), `not json`); err == nil {
		t.Fatal("want an error for invalid JSON arguments")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Product details by known ID (v1/products) — the third, separately-callable
// tool (package comment, shop.go), for resolving a bare product id the
// model didn't get from a search_shop_items call in this conversation.
// ─────────────────────────────────────────────────────────────────────────────

// shopProductsRealCaptureJSON is a real, complete /rest-api/v1/products?
// context=LIGHT response for 4 productIds — captured live, not synthesized
// (see shop.go's package comment).
const shopProductsRealCaptureJSON = `{"pageNumber":1,"pageSize":4,"itemsTotalCount":4,"items":[{"id":"G1321010678","erpSkuName":"6202-2RSH (SKF) Rillenkugellager, 2 Dichtscheiben SKF Einzelverpackung","type":"VARIANT","categoryPath":[{"code":"15"}],"slug":"rillenkugellager","erpSkuId":"693994","brand":{"code":"1321","slug":"skf","brandName":"SKF","productName":"Rillenkugellager SKF 6202-2RSH, Außendurchmesser: 35 mm | Breite: 11 mm | Innendurchmesser: 15 mm","productReference":"6202-2RSH","secondaryProductReference":"177697"},"perimeterType":"DEFAULT"},{"id":"G1515050062","erpSkuName":"6204-C-2HRS>V (FAG) Rillenkugellager, 2 Dichtscheiben FAG","type":"VARIANT","categoryPath":[{"code":"15"}],"slug":"rillenkugellager","erpSkuId":"3028605","brand":{"code":"1112","slug":"fag","brandName":"FAG","productName":"Rillenkugellager FAG 6204-C-2HRS>V, Außendurchmesser: 47 mm | Breite: 14 mm | Innendurchmesser: 20 mm","productReference":"6204-C-2HRS>V","secondaryProductReference":"0949834100000"},"perimeterType":"DEFAULT"}],"context":"LIGHT","productIds":["G1321010678","G1515050062","G1515049891","G1321130733"],"notFoundItems":[]}`

// TestParseShopProductDetailsRealCapture verifies parsing against the
// actual confirmed response shape — id/erpSkuId/brand reused via shopItem,
// same field mapping as parseShopSearchItems for consistency.
func TestParseShopProductDetailsRealCapture(t *testing.T) {
	items, notFound, err := parseShopProductDetails([]byte(shopProductsRealCaptureJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items parsed, got %d: %+v", len(items), items)
	}
	first := items[0]
	if first.ProductID != "G1321010678" || first.ErpSkuID != "693994" {
		t.Errorf("want ProductID/ErpSkuID populated, got %+v", first)
	}
	if first.Name != "Rillenkugellager SKF 6202-2RSH, Außendurchmesser: 35 mm | Breite: 11 mm | Innendurchmesser: 15 mm" {
		t.Errorf("want Name from brand.productName, got %q", first.Name)
	}
	if first.Manufacturer != "SKF" {
		t.Errorf("want Manufacturer from brand.brandName, got %q", first.Manufacturer)
	}
	if len(notFound) != 0 {
		t.Errorf("want no not-found items in this capture, got %+v", notFound)
	}
}

// TestFetchShopProductDetailsUsesCommaSeparatedIdsAndContextLight covers
// the request shape: productIds joined with commas, and context=LIGHT
// always set (the one confirmed, lightest response mode).
func TestFetchShopProductDetailsUsesCommaSeparatedIdsAndContextLight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case "/rest-api/v1/products":
			if got := r.URL.Query().Get("productIds"); got != "G1,G2,G3" {
				t.Errorf("want comma-separated productIds, got %q", got)
			}
			if got := r.URL.Query().Get("context"); got != "LIGHT" {
				t.Errorf("want context=LIGHT, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"items":         []map[string]any{{"id": "G1", "erpSkuId": "111", "brand": map[string]any{"brandName": "SKF", "productName": "Lager"}}},
				"notFoundItems": []string{"G2", "G3"},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "u", Password: "pw", ClientID: "c", ClientSecret: "s"}

	items, notFound, err := fetchShopProductDetails(context.Background(), cfg, []string{"G1", "G2", "G3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 || items[0].ErpSkuID != "111" {
		t.Fatalf("want 1 resolved item with its erpSkuId, got %+v", items)
	}
	if len(notFound) != 2 {
		t.Fatalf("want the 2 not-found ids surfaced, got %+v", notFound)
	}
}

// TestFetchShopProductDetailsReauthenticatesOnceOn401 proves shopAuthedGet's
// shared 401-reauth-retry behavior also covers this third endpoint.
func TestFetchShopProductDetailsReauthenticatesOnceOn401(t *testing.T) {
	var tokenRequests, attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest-api/v1/tokens":
			n := atomic.AddInt32(&tokenRequests, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "expires_in": 3600})
		case "/rest-api/v1/products":
			n := atomic.AddInt32(&attempts, 1)
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer tok-2" {
				t.Errorf("want the retry to use the freshly re-issued token, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "G1"}}, "notFoundItems": []string{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := shopConfig{BaseURL: srv.URL, Username: "reauth-products-user", Password: "pw", ClientID: "c", ClientSecret: "s"}

	items, _, err := fetchShopProductDetails(context.Background(), cfg, []string{"G1"})
	if err != nil {
		t.Fatalf("want the 401 to trigger a reauth-and-retry, not a hard failure: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item after the reauth retry, got %+v", items)
	}
	if got := atomic.LoadInt32(&tokenRequests); got != 2 {
		t.Fatalf("want exactly 2 token requests (initial + one forced re-login), got %d", got)
	}
}

// TestShopProductDetailsToolExecutorRejectsEmptyIDs covers the
// tool-arguments validation path without needing a live shop connection.
func TestShopProductDetailsToolExecutorRejectsEmptyIDs(t *testing.T) {
	exec := shopProductDetailsToolExecutor(shopConfig{})
	if _, err := exec(context.Background(), `{"product_ids":[]}`); err == nil {
		t.Fatal("want an error for empty product_ids")
	}
	if _, err := exec(context.Background(), `not json`); err == nil {
		t.Fatal("want an error for invalid JSON arguments")
	}
}

// TestAppendShopToolRegistersAllThreeTools covers the gating contract:
// when enabled and preset-allowed, all three tools must be offered
// together — they're facets of one connector, not independently
// restrictable (see appendShopTool's doc comment).
func TestAppendShopToolRegistersAllThreeTools(t *testing.T) {
	executors := map[string]toolExecutor{}
	tools := appendShopTool(nil, executors, shopConfig{Enabled: true}, nil, "", nil)
	if len(tools) != 3 {
		t.Fatalf("want all three tools registered, got %d: %+v", len(tools), tools)
	}
	if _, ok := executors[shopSearchToolName]; !ok {
		t.Error("want search_shop_items executor registered")
	}
	if _, ok := executors[shopProductDetailsToolName]; !ok {
		t.Error("want get_shop_product_details executor registered")
	}
	if _, ok := executors[shopStockToolName]; !ok {
		t.Error("want get_shop_stock_and_price executor registered")
	}
}

// TestAppendShopToolDisabledRegistersNeither guards the other half: a
// disabled connector must add neither tool, not just skip one.
func TestAppendShopToolDisabledRegistersNeither(t *testing.T) {
	executors := map[string]toolExecutor{}
	tools := appendShopTool(nil, executors, shopConfig{Enabled: false}, nil, "", nil)
	if len(tools) != 0 || len(executors) != 0 {
		t.Fatalf("want no tools/executors when Shop is disabled, got tools=%+v executors=%+v", tools, executors)
	}
}
