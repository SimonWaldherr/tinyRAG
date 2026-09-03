package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rubix online shop (de.rubix.com) as a chat/agent/mail tool — a live query
// at answer time, same "nothing embedded/stored" model as the MSSQL tool
// (mssql.go), just against a B2B e-commerce REST API instead of a database.
//
// This is NOT a documented public API — de.rubix.com's own robots.txt
// disallows crawling "/rest-api/v1/tokens", and unauthenticated requests
// return 401 with {"code":"UNAUTHORIZED","message":"Get a valid token
// first. See 'POST /tokens'"} — confirming a token-auth REST API exists
// without documenting its exact contract. Four endpoints are used, all
// four confirmed from real captured traffic (browser devtools network tab
// against a live, authenticated session), not reverse-engineered by
// probing or guessed at from plausible SAP-Commerce/Hybris conventions:
//
//  1. POST /rest-api/v1/tokens — {"userLogin","password","clientId",
//     "clientSecret","rememberMe"}. clientId/clientSecret are the shop
//     frontend's own fixed "browser API client" credentials (the same pair
//     for every browser session against this shop, not per-account) —
//     configured via shopConfig.ClientID/ClientSecret(Env) rather than
//     hardcoded here, since it's still a secret-shaped string even if
//     shop-wide. The response shape itself is still not fully confirmed
//     (parseShopTokenResponse tries several plausible field names
//     defensively) — some accounts get a JSON bearer token, others a
//     cookie-session with no JSON body at all (see shopAccessToken).
//  2. GET /rest-api/v4/search-items?searchText=...&conditions=[{"code":
//     "context","values":["AUTOSUGGEST"]}]&pageSize=...&configStrategy=
//     headless — product search/discovery by free text. Confirmed response
//     shape: see shopSearchResponse below. Deliberately does NOT carry
//     price or stock — those live only in endpoint 4. Every item carries
//     BOTH its "erpSkuId" (what endpoint 4 needs) AND its "id" (a
//     different, unrelated catalog identifier — what endpoint 3 needs, if
//     ever seen again without having been found via a search).
//  3. GET /rest-api/v1/products?productIds=<comma-separated>&context=LIGHT
//     — bulk lookup of already-known product(s) by their "id" (the field
//     endpoint 2 calls ProductID) rather than by free text — confirms/
//     refreshes name+brand, and yields the erpSkuId needed for endpoint 4
//     when only a bare product id is known (e.g. a customer pastes a
//     "G1321010678"-style reference from an old quote/order/URL that was
//     never found via a search_shop_items call in this conversation).
//     Confirmed response shape: see shopProductsResponse below — reuses
//     shopItem (same erpSkuId/ProductID/Name/Manufacturer fields endpoint 2
//     already populates), since context=LIGHT's fields are a subset of
//     what search-items already returns; no separate result type needed.
//  4. GET /rest-api/v3/commerce-sku-details?erpSkuIds=<comma-separated> —
//     live stock level, lead time, cart-eligibility and price for specific,
//     already-known article(s) (by erpSkuId, typically from endpoint 2 or
//     3). Confirmed response shape: see shopSkuDetailsResponse below.
//     Always a live call, deliberately never cached (see
//     fetchShopSkuDetails) — unlike product metadata, "is it in stock
//     right now" is exactly the kind of thing a cache would make stale in
//     a misleading way.
//
// Tool design: search (2), product-details-by-id (3) and stock/price (4)
// are deliberately THREE separate LLM tools (shopSearchToolDef/
// shopProductDetailsToolDef/shopStockToolDef below), one per distinct
// external capability, not folded into fewer, more "clever" combined
// calls — search is cheap and broad (find candidates from free text),
// product-details resolves a specific already-known id the model didn't
// get from a search in this conversation, and stock/price is a live
// external lookup that should only run when actually needed (the user
// asks about availability/cost, or it's clearly required for the answer).
// Bundling stock/price into search would mean either always paying for a
// live lookup on every search result (wasteful, and stale the moment it's
// rendered) or never exposing live availability at all. Letting the model
// choose mirrors how a human sales agent would work: browse/search first,
// resolve a specific reference if one is already in hand, then check
// real-time stock only for the item(s) that actually matter.
// ─────────────────────────────────────────────────────────────────────────────

const (
	shopDefaultBaseURL        = "https://de.rubix.com"
	shopDefaultTimeoutSeconds = 10
	shopDefaultMaxResults     = 10
)

// shopBaseURLOrDefault falls back to Rubix's German shop when unset, so a
// fresh checkout has a sensible default without needing config.
func shopBaseURLOrDefault(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return shopDefaultBaseURL
	}
	return trimmed
}

func shopTimeout(cfg shopConfig) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return shopDefaultTimeoutSeconds * time.Second
}

func shopMaxResults(cfg shopConfig) int {
	if cfg.MaxResults > 0 {
		return cfg.MaxResults
	}
	return shopDefaultMaxResults
}

// shopTransport is shared by every shop request (token + search) so
// repeated calls to the same shop host reuse pooled/keep-alive
// connections instead of each building its own transport. Each call still
// wraps it in a short-lived *http.Client via shopClient to apply that
// request's own cfg.TimeoutSeconds, since Transport (not Client) is what
// actually holds the connection pool.
var shopTransport = &http.Transport{
	MaxIdleConns:        20,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
}

// shopClient's jar is caller-supplied (per shopTokenCache entry, see
// shopAccessToken) rather than created here, so the same cookie storage
// survives across the login request and every later search request for
// that account — de.rubix.com (see shopAccessToken's cookie-session
// fallback) authenticates some accounts via Set-Cookie rather than a JSON
// bearer token, and without a shared jar those cookies would be read once
// and then silently dropped by Go's client on the very next request.
func shopClient(cfg shopConfig, jar http.CookieJar) *http.Client {
	// tracingTransport (conntrace.go) passes straight through to
	// shopTransport unless the request context opted into capture.
	return &http.Client{Timeout: shopTimeout(cfg), Transport: tracingTransport{base: shopTransport}, Jar: jar}
}

// ---- Token acquisition/caching ---------------------------------------------

// shopTokenCache holds the last access token (or cookie-session jar) for
// one shop base URL + username pair, refreshed slightly before actual
// expiry — same shape as graph.go's graphTokenCache (Microsoft Graph), just
// for this connector. authenticated tracks validity instead of "token !=
// """ since a successful cookie-session login (see shopAccessToken) leaves
// token empty on purpose.
type shopTokenCache struct {
	mu            sync.Mutex
	token         string
	jar           http.CookieJar
	authenticated bool
	expires       time.Time
}

// shopSession is what a successful login yields: either a bearer token
// (the originally confirmed contract) or, when the server instead responds
// with an empty body plus Set-Cookie headers (see shopAccessToken), just
// the cookie jar that now holds the session cookies — token is "" in that
// case and callers must not send an Authorization header.
type shopSession struct {
	token string
	jar   http.CookieJar
}

var (
	shopTokensMu sync.Mutex
	shopTokens   = map[string]*shopTokenCache{}
)

func shopCacheKey(cfg shopConfig) string {
	return shopBaseURLOrDefault(cfg.BaseURL) + "|" + cfg.Username
}

// shopMaxRetries bounds extra attempts after a 429/5xx, mirroring
// graph.go's graphMaxRetries. This is the FALLBACK default when
// cfg.MaxRetries (settings.go, per shop connection) isn't set — see
// shopMaxRetriesLimit, which shopTokenRequest/shopAuthedGet actually call.
var shopMaxRetries = 4

// shopMaxRetriesLimit resolves the effective retry bound for one shop
// connection: cfg.MaxRetries when positive, else the shopMaxRetries
// fallback (which tests still mutate directly, same reasoning as
// graph.go's graphMaxRetriesLimit).
func shopMaxRetriesLimit(cfg shopConfig) int {
	if cfg.MaxRetries > 0 {
		return cfg.MaxRetries
	}
	return shopMaxRetries
}

// shopTokenResponse is intentionally loose (map, not a fixed struct) since
// the real field names aren't confirmed — see package comment point 2 (the
// same defensive-parsing reasoning applies to the token response, which is
// smaller but no better documented than the search response).
func firstShopString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func firstShopNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n, ok := v.(float64); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// shopAccessToken returns a cached bearer token for cfg, acquiring a fresh
// one via POST {baseURL}/rest-api/v1/tokens when missing or close to
// expiry. See the package comment's point 1 for the confirmed request body
// shape, built in shopTokenRequest below.
func shopAccessToken(ctx context.Context, cfg shopConfig) (shopSession, error) {
	if strings.TrimSpace(cfg.Username) == "" {
		return shopSession{}, fmt.Errorf("shop: username not configured")
	}
	password := cfg.resolvedPassword()
	if password == "" {
		return shopSession{}, fmt.Errorf("shop: password not configured (set password or password_env)")
	}
	if strings.TrimSpace(cfg.ClientID) == "" || cfg.resolvedClientSecret() == "" {
		return shopSession{}, fmt.Errorf("shop: client_id/client_secret not configured (see shop.go's package comment point 1)")
	}

	key := shopCacheKey(cfg)
	shopTokensMu.Lock()
	cache, ok := shopTokens[key]
	if !ok {
		cache = &shopTokenCache{}
		shopTokens[key] = cache
	}
	shopTokensMu.Unlock()

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.jar == nil {
		jar, jarErr := cookiejar.New(nil)
		if jarErr != nil {
			return shopSession{}, fmt.Errorf("shop: create cookie jar: %w", jarErr)
		}
		cache.jar = jar
	}
	if cache.authenticated && time.Now().Before(cache.expires) {
		return shopSession{token: cache.token, jar: cache.jar}, nil
	}

	attempt, err := shopTokenRequest(ctx, cfg, password, cache.jar)
	if err != nil {
		return shopSession{}, err
	}
	if attempt.status == http.StatusOK {
		token, expiresIn, parseErr := parseShopTokenResponse(attempt.raw)
		if parseErr == nil {
			cache.token = token
			cache.authenticated = true
			cache.expires = time.Now().Add(time.Duration(expiresIn-60) * time.Second)
			// Logged on every fresh login (not cached hits, so at most
			// once per ~30 min per account) specifically so an admin
			// tailing server logs can see which contract is actually in
			// effect right now without needing to click "Login testen" —
			// see shopClient's doc comment for why this isn't a given.
			log.Printf("shop: fresh login ok (%s) via bearer token, ~%.0fs lifetime", shopBaseURLOrDefault(cfg.BaseURL), expiresIn)
			return shopSession{token: cache.token, jar: cache.jar}, nil
		}
		// No recognizable JSON token field, but the server set session
		// cookies (and often a Userid header) anyway — de.rubix.com does
		// this for at least some accounts, authenticating via cookie
		// session instead of the originally observed bearer-token shape
		// (see the package comment and shopClient's doc comment). Treat
		// that as success: the jar (already attached to this request via
		// shopTokenRequest) carries the session forward, no bearer token
		// needed.
		if attempt.cookiesSet {
			cache.token = ""
			cache.authenticated = true
			cache.expires = time.Now().Add(1740 * time.Second) // 1800s fallback minus the same 60s safety margin as the token path
			log.Printf("shop: fresh login ok (%s) via cookie session (no JSON token field, Userid=%s)", shopBaseURLOrDefault(cfg.BaseURL), valueOrDash(attempt.userID))
			return shopSession{jar: cache.jar}, nil
		}
		return shopSession{}, parseErr
	}
	return shopSession{}, fmt.Errorf("shop: token request %d: %s", attempt.status, truncateRunesNote(string(attempt.raw), 300))
}

// invalidateShopSession discards any cached session (bearer token or
// cookie jar) for cfg, forcing the next shopAccessToken call to log in
// again from scratch rather than reuse state we now know is stale. Used by
// searchShopItemsCached's one-shot 401 retry (see its doc comment): a
// session that looked valid client-side (cache.expires not yet reached)
// can still be rejected server-side, e.g. a concurrent login elsewhere
// invalidating the previous one — the client-side expiry guess is only
// ever a heuristic, never a guarantee.
func invalidateShopSession(cfg shopConfig) {
	key := shopCacheKey(cfg)
	shopTokensMu.Lock()
	cache, ok := shopTokens[key]
	shopTokensMu.Unlock()
	if !ok {
		return
	}
	cache.mu.Lock()
	cache.authenticated = false
	cache.token = ""
	cache.jar = nil // old cookies may be tied to the now-invalid session; start clean
	cache.mu.Unlock()
}

// parseShopTokenResponse extracts the bearer token and its lifetime from a
// successful (HTTP 200) token response — see the package comment's point 1
// for why this tries several plausible field names rather than one
// confirmed shape. expiresIn falls back to a conservative 1800s when the
// field is absent/unrecognized.
func parseShopTokenResponse(raw []byte) (token string, expiresIn float64, err error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", 0, fmt.Errorf("shop: parse token response: %w", err)
	}
	token = firstShopString(m, "access_token", "accessToken", "token")
	if token == "" {
		return "", 0, fmt.Errorf("shop: token response had no recognizable token field: %s", truncateRunesNote(string(raw), 300))
	}
	expiresIn, ok := firstShopNumber(m, "expires_in", "expiresIn", "expiry_seconds")
	if !ok || expiresIn <= 60 {
		expiresIn = 1800
	}
	return token, expiresIn, nil
}

// shopTokenRequest performs the raw POST to /rest-api/v1/tokens, retrying
// on 429/5xx (see shopMaxRetries), and returns the *final* attempt's status
// code and raw body regardless of success — separated out from
// shopAccessToken so handleTestShopLogin (conntest.go) can show the raw
// response for debugging the still-unconfirmed token contract, without
// going through the cache (a fresh request every time the admin clicks
// "Login testen", not a cached result from a previous attempt).
//
// shopTokenAttempt is one login attempt's full result, diagnostic fields
// included: contentType/finalURL/cookiesSet/userID are ignored by
// shopAccessToken's normal path (except cookiesSet, see its cookie-session
// fallback) but let handleTestShopLogin (conntest.go) tell apart the
// several ways an HTTP 200 with an *empty* body can happen — the request
// never reaching the real login handler at all (a WAF/consent/redirect
// page swallowed silently by Go's default redirect-following client,
// finalURL then differs from {baseURL}/rest-api/v1/tokens, and no
// Set-Cookie either), vs. a genuine cookie-session login that just doesn't
// return a JSON body (cookiesSet true, often with a Userid response
// header) — as opposed to "the token field has an unrecognized name",
// which has a body, just not a recognizable field in it.
type shopTokenAttempt struct {
	status      int
	raw         []byte
	contentType string
	finalURL    string
	cookiesSet  bool
	userID      string
}

// shopTokenRequest performs the raw POST to /rest-api/v1/tokens, retrying
// on 429/5xx (see shopMaxRetries), and returns the *final* attempt's full
// result regardless of success — separated out from shopAccessToken so
// handleTestShopLogin (conntest.go) can show the raw response for
// debugging the still-unconfirmed token contract, without going through
// the cache (a fresh request every time the admin clicks "Login testen",
// not a cached result from a previous attempt). jar is caller-supplied
// (shopAccessToken's per-account cache entry, or a fresh one from
// handleTestShopLogin) so any Set-Cookie in the response is available to
// whatever request reuses that same jar afterwards.
func shopTokenRequest(ctx context.Context, cfg shopConfig, password string, jar http.CookieJar) (shopTokenAttempt, error) {
	baseURL := shopBaseURLOrDefault(cfg.BaseURL)
	// Confirmed shape (package comment point 1): userLogin/password are the
	// account credentials, clientId/clientSecret are the shop frontend's
	// fixed browser-client pair (see shopConfig.ClientID/ClientSecret), and
	// rememberMe is always false here — this is a short-lived service
	// token, not a browser session that should persist.
	body, err := json.Marshal(map[string]any{
		"userLogin":    cfg.Username,
		"password":     password,
		"clientId":     cfg.ClientID,
		"clientSecret": cfg.resolvedClientSecret(),
		"rememberMe":   false,
	})
	if err != nil {
		return shopTokenAttempt{}, fmt.Errorf("shop: encode token request: %w", err)
	}

	maxRetries := shopMaxRetriesLimit(cfg)
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/rest-api/v1/tokens", strings.NewReader(string(body)))
		if err != nil {
			return shopTokenAttempt{}, fmt.Errorf("shop: build token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", connectorUserAgent)

		resp, err := shopClient(cfg, jar).Do(req)
		if err != nil {
			return shopTokenAttempt{}, fmt.Errorf("shop: token request failed: %w", err)
		}
		respRaw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		result := shopTokenAttempt{
			status:      resp.StatusCode,
			raw:         respRaw,
			contentType: resp.Header.Get("Content-Type"),
			cookiesSet:  len(resp.Header.Values("Set-Cookie")) > 0,
			userID:      resp.Header.Get("Userid"),
		}
		if resp.Request != nil && resp.Request.URL != nil {
			result.finalURL = resp.Request.URL.String()
		}
		if readErr != nil {
			return result, fmt.Errorf("shop: read token response: %w", readErr)
		}
		if resp.StatusCode == http.StatusOK {
			return result, nil
		}
		lastErr = fmt.Errorf("shop: token request %d: %s", resp.StatusCode, truncateRunesNote(string(respRaw), 300))
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < maxRetries {
			time.Sleep(graphBackoff(attempt, parseRetryAfter(resp.Header)))
			continue
		}
		return result, nil
	}
	return shopTokenAttempt{}, lastErr
}

// ---- Search ------------------------------------------------------------

// shopItem is one search result, using the confirmed /rest-api/v4/
// search-items field names (see package comment and parseShopSearchItems)
// — deliberately no Price/Stock here: the real response never carries
// them (confirmed, not just unobserved), see fetchShopSkuDetails/
// shopSkuDetails for that, fetched separately and only when needed.
type shopItem struct {
	// ErpSkuID is what fetchShopSkuDetails (commerce-sku-details) needs to
	// look up stock/price for this exact item — NOT the same as ProductID.
	ErpSkuID string
	// ProductID is the response's own "id" field (e.g. "G5020059865") — a
	// distinct, R3-doesn't-currently-use-it-for-anything catalog id; kept
	// only so it's not silently discarded, and to avoid the previous bug
	// of conflating it with ErpSkuID (see package comment).
	ProductID    string
	Name         string // brand.productName — the full descriptive product name
	RangeName    string // range.name — the shorter product-family/range name; also Name's fallback if productName is ever absent
	Manufacturer string // brand.brandName
	EAN          string
	Attributes   []shopAttribute // a few MANDATORY-classification specs (diameter, length, material, ...)
}

// shopAttribute is one of an item's classified specs (e.g. "Länge: 38 mm").
type shopAttribute struct {
	Name, Value, Unit string
}

var shopParseWarnOnce sync.Once

// shopSearchCacheTTL bounds how long an identical (base URL + username +
// query + limit) search result is served from memory instead of hitting
// the live endpoint again — a chat/agent turn can call the same tool more
// than once (e.g. a follow-up question re-using the same search term), and
// this is a read-only product lookup where a few minutes of staleness is
// harmless. Keyed by exact query text, so it's a cache, not a substitute
// for a real search — it only ever saves a repeated identical call.
const shopSearchCacheTTL = 5 * time.Minute

type shopSearchCacheEntry struct {
	items   []shopItem
	total   int
	expires time.Time
}

var (
	shopSearchCacheMu sync.Mutex
	shopSearchCache   = map[string]shopSearchCacheEntry{}
)

func shopSearchCacheKey(cfg shopConfig, query string, limit int) string {
	return shopCacheKey(cfg) + "|" + strconv.Itoa(limit) + "|" + strings.ToLower(strings.TrimSpace(query))
}

// shopAuthedGet performs an authenticated GET against baseURL+path (bearer
// token or cookie jar, whichever shopAccessToken returned), retrying once
// on a 401 (a cached session that looked client-side-valid can still be
// rejected server-side — e.g. a concurrent login elsewhere invalidated it
// early; force exactly one fresh login and retry rather than fail on state
// now known stale, bounded to once so a genuinely bad account doesn't
// loop) and on 429/5xx with backoff (shopMaxRetries) — the shared plumbing
// behind both searchShopItemsCached and fetchShopSkuDetails, mirroring how
// graph.go's graphGet is the one shared authenticated GET for every
// Graph-based connector. Returns the response headers too, since
// fetchShopSkuDetails needs the Currency header (see its doc comment).
func shopAuthedGet(ctx context.Context, cfg shopConfig, path string) (raw []byte, headers http.Header, err error) {
	session, err := shopAccessToken(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	baseURL := shopBaseURLOrDefault(cfg.BaseURL)

	maxRetries := shopMaxRetriesLimit(cfg)
	var lastErr error
	reauthed := false
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("shop: build request: %w", err)
		}
		// session.token is "" for a cookie-session login (see
		// shopAccessToken) — the shared jar below already carries the
		// session cookies, an empty Bearer header would just be wrong.
		if session.token != "" {
			req.Header.Set("Authorization", "Bearer "+session.token)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", connectorUserAgent)

		resp, err := shopClient(cfg, session.jar).Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("shop: request failed: %w", err)
		}
		respRaw, readErr := io.ReadAll(resp.Body)
		respHeaders := resp.Header
		resp.Body.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("shop: read response: %w", readErr)
		}
		if resp.StatusCode == http.StatusOK {
			return respRaw, respHeaders, nil
		}
		if resp.StatusCode == http.StatusUnauthorized && !reauthed {
			reauthed = true
			log.Printf("shop: got 401 on a cached session (%s %s) — forcing one fresh login and retrying", baseURL, path)
			invalidateShopSession(cfg)
			session, err = shopAccessToken(ctx, cfg)
			if err != nil {
				return nil, nil, fmt.Errorf("shop: re-login after 401 failed: %w", err)
			}
			continue
		}
		lastErr = fmt.Errorf("shop: request %d: %s", resp.StatusCode, truncateRunesNote(string(respRaw), 300))
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < maxRetries {
			time.Sleep(graphBackoff(attempt, parseRetryAfter(resp.Header)))
			continue
		}
		return nil, nil, lastErr
	}
	return nil, nil, lastErr
}

// searchShopItems queries the shop's search-items endpoint and returns up
// to limit normalized results plus the total match count (searchTotal —
// itemsTotalCount from the response, so a caller can tell the model "N
// total matches, showing the top few"), serving a cached result for an
// identical recent query when available (see shopSearchCacheTTL).
func searchShopItems(ctx context.Context, cfg shopConfig, query string, limit int) ([]shopItem, int, error) {
	return searchShopItemsCached(ctx, cfg, query, limit, true)
}

// searchShopItemsCached is searchShopItems with the result cache
// optionally disabled — conntest.go's handleTestShop always passes false
// so "Verbindung testen" is a fresh live request every click, never a
// cached result from an earlier attempt (same reasoning as
// handleTestShopLogin bypassing the token cache).
func searchShopItemsCached(ctx context.Context, cfg shopConfig, query string, limit int, useCache bool) ([]shopItem, int, error) {
	cacheKey := shopSearchCacheKey(cfg, query, limit)
	if useCache {
		shopSearchCacheMu.Lock()
		entry, ok := shopSearchCache[cacheKey]
		shopSearchCacheMu.Unlock()
		if ok && time.Now().Before(entry.expires) {
			return entry.items, entry.total, nil
		}
	}

	q := url.Values{}
	q.Set("searchText", query)
	q.Set("conditions", `[{"code":"context","values":["AUTOSUGGEST"]}]`)
	q.Set("pageSize", strconv.Itoa(limit))
	q.Set("configStrategy", "headless")
	raw, _, err := shopAuthedGet(ctx, cfg, "/rest-api/v4/search-items?"+q.Encode())
	if err != nil {
		return nil, 0, err
	}
	items, total, err := parseShopSearchItems(raw, limit)
	if err != nil {
		return nil, 0, err
	}
	if useCache {
		shopSearchCacheMu.Lock()
		shopSearchCache[cacheKey] = shopSearchCacheEntry{items: items, total: total, expires: time.Now().Add(shopSearchCacheTTL)}
		shopSearchCacheMu.Unlock()
	}
	return items, total, nil
}

// shopSearchResponse mirrors the confirmed /rest-api/v4/search-items JSON
// shape (see package comment) — a real typed structure now that the
// response has actually been observed, replacing the previous defensive
// any-shaped parsing that had to guess at key names. Facets/pageNumber/
// searchText/conditions (also present in the real response) aren't needed
// by anything here and are simply left unmarshaled.
type shopSearchResponse struct {
	ItemsTotalCount int                  `json:"itemsTotalCount"`
	Items           []shopSearchRespItem `json:"items"`
}

type shopSearchRespItem struct {
	ErpSkuID string `json:"erpSkuId"`
	ID       string `json:"id"`
	EAN      string `json:"ean"`
	Brand    struct {
		BrandName   string `json:"brandName"`
		ProductName string `json:"productName"`
	} `json:"brand"`
	Range struct {
		Name string `json:"name"`
	} `json:"range"`
	Attributes []struct {
		Name           string `json:"name"`
		Classification string `json:"classification"`
		Value          string `json:"value"`
		Unit           string `json:"unit"`
	} `json:"attributes"`
}

// shopAttributeMaxPerItem bounds how many of an item's MANDATORY attributes
// get relayed to the model per search result — enough to be useful (e.g.
// diameter/length/material for a screw) without flooding the tool response
// for an item with many classified specs.
const shopAttributeMaxPerItem = 4

// parseShopSearchItems extracts up to limit items from the confirmed
// search-items shape, plus the response's own itemsTotalCount (so a caller
// can tell the model how many total matches exist beyond what's returned).
// A well-formed JSON response whose items are all missing both erpSkuId
// and brand.productName (the API shape changing underneath us, not a
// genuine zero-result search) logs a one-time warning rather than
// silently returning empty results forever.
func parseShopSearchItems(raw []byte, limit int) ([]shopItem, int, error) {
	var resp shopSearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, 0, fmt.Errorf("shop: parse search response: %w", err)
	}

	out := make([]shopItem, 0, len(resp.Items))
	sawUnrecognizedItem := false
	for _, ri := range resp.Items {
		if ri.ErpSkuID == "" && ri.Brand.ProductName == "" {
			sawUnrecognizedItem = true
			continue // not recognizable as an actual item
		}
		item := shopItem{
			ErpSkuID:     ri.ErpSkuID,
			ProductID:    ri.ID,
			Name:         ri.Brand.ProductName,
			RangeName:    ri.Range.Name,
			Manufacturer: ri.Brand.BrandName,
			EAN:          ri.EAN,
		}
		if item.Name == "" {
			item.Name = item.RangeName // fall back to the shorter range name on the rare item missing brand.productName
		}
		for _, a := range ri.Attributes {
			if a.Classification != "MANDATORY" {
				continue
			}
			item.Attributes = append(item.Attributes, shopAttribute{Name: a.Name, Value: a.Value, Unit: a.Unit})
			if len(item.Attributes) >= shopAttributeMaxPerItem {
				break
			}
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	if len(resp.Items) > 0 && len(out) == 0 && sawUnrecognizedItem {
		shopParseWarnOnce.Do(func() {
			log.Printf("WARN: shop search response's items had no recognizable erpSkuId/brand.productName field — the API shape may have changed, see shop.go's parseShopSearchItems")
		})
	}
	return out, resp.ItemsTotalCount, nil
}

// ---- Product details by known ID (v1/products) --------------------------

// shopProductsResponse mirrors the confirmed /rest-api/v1/products?
// context=LIGHT JSON shape (see package comment) — pageNumber/pageSize/
// itemsTotalCount/context/productIds (also present in the real response)
// aren't needed by anything here and are simply left unmarshaled.
type shopProductsResponse struct {
	Items []struct {
		ID       string `json:"id"`
		ErpSkuID string `json:"erpSkuId"`
		Brand    struct {
			BrandName   string `json:"brandName"`
			ProductName string `json:"productName"`
		} `json:"brand"`
	} `json:"items"`
	NotFoundItems []string `json:"notFoundItems"`
}

// fetchShopProductDetails looks up already-known product(s) by their
// ProductID (the "id" field from a prior search_shop_items result, or a
// bare G-number the user references directly) in one request (the
// endpoint's own comma-separated batch support — see package comment) —
// useful to confirm/refresh an item's current name/brand, or to obtain its
// erpSkuId when only the ProductID is known. context=LIGHT is the
// confirmed, lightest response mode; not made configurable since nothing
// here needs more than name/brand/erpSkuId.
func fetchShopProductDetails(ctx context.Context, cfg shopConfig, productIDs []string) ([]shopItem, []string, error) {
	if len(productIDs) == 0 {
		return nil, nil, fmt.Errorf("shop: no product_ids given")
	}
	q := url.Values{}
	q.Set("productIds", strings.Join(productIDs, ","))
	q.Set("context", "LIGHT")
	raw, _, err := shopAuthedGet(ctx, cfg, "/rest-api/v1/products?"+q.Encode())
	if err != nil {
		return nil, nil, err
	}
	return parseShopProductDetails(raw)
}

// parseShopProductDetails extracts every item's identifying info from the
// confirmed v1/products shape, reusing shopItem (see package comment for
// why no separate result type exists) — EAN/RangeName/Attributes are left
// zero-valued, since this endpoint (at context=LIGHT) doesn't return them.
func parseShopProductDetails(raw []byte) ([]shopItem, []string, error) {
	var resp shopProductsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, fmt.Errorf("shop: parse products response: %w", err)
	}
	out := make([]shopItem, 0, len(resp.Items))
	for _, ri := range resp.Items {
		out = append(out, shopItem{
			ErpSkuID:     ri.ErpSkuID,
			ProductID:    ri.ID,
			Name:         ri.Brand.ProductName,
			Manufacturer: ri.Brand.BrandName,
		})
	}
	return out, resp.NotFoundItems, nil
}

// ---- Live stock/price (commerce-sku-details) ----------------------------

// shopSkuDetails is one item's live stock/price/cart-eligibility, from
// /rest-api/v3/commerce-sku-details (see package comment) — fetched only
// when the model actually needs current availability/price for specific
// article(s) already known (typically from a prior search_shop_items
// call's ErpSkuID), never bundled into every search result.
type shopSkuDetails struct {
	ErpSkuID            string
	CanBeAddedToCart    bool
	IsSellout           bool
	AvailabilityStatus  string // "IN_STOCK" | "OUT_OF_STOCK" | ... as returned, not enumerated/validated — the full set of possible values isn't confirmed
	StockLevel          int
	LeadTimeMinimumDays int
	LeadTimeMaximumDays int
	Unit                string // e.g. "Stück"
	// Price is basePriceIncludingTaxes (gross) — what a customer actually
	// pays, from the lowest-minimumQuantity volume tier (what "the price"
	// means for a plain "was kostet das" question with no quantity given).
	Price           float64
	Currency        string
	MinimumQuantity int
}

// shopSkuDetailsResponse mirrors the confirmed /rest-api/v3/
// commerce-sku-details JSON shape (see package comment).
type shopSkuDetailsResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Product struct {
			CanBeAddedToCart bool `json:"canBeAddedToCart"`
			IsSellout        bool `json:"isSellout"`
		} `json:"product"`
		Stock struct {
			Availabilities []struct {
				StockLevel         int    `json:"stockLevel"`
				LeadTimeMinimum    int    `json:"leadTimeMinimum"`
				LeadTimeMaximum    int    `json:"leadTimeMaximum"`
				AvailabilityStatus string `json:"availabilityStatus"`
				Unit               struct {
					NameSingular string `json:"nameSingular"`
				} `json:"unit"`
			} `json:"availabilities"`
		} `json:"stock"`
		Price struct {
			IsPriceOnRequest bool `json:"isPriceOnRequest"`
			Volumes          []struct {
				BasePriceIncludingTaxes float64 `json:"basePriceIncludingTaxes"`
				MinimumQuantity         int     `json:"minimumQuantity"`
			} `json:"volumes"`
		} `json:"price"`
	} `json:"items"`
	NotFoundItems []string `json:"notFoundItems"`
}

// shopDefaultCurrency is the fallback when a response carries no Currency
// header — de.rubix.com is the German shop, and every real capture so far
// has returned "EUR" there; used only if that header is ever absent.
const shopDefaultCurrency = "EUR"

// fetchShopSkuDetails looks up live stock/price/cart-eligibility for the
// given erpSkuIds in one request (the endpoint's own comma-separated batch
// support — see package comment) — always a live call, deliberately never
// cached, unlike searchShopItemsCached's short-TTL cache: availability is
// exactly the kind of thing a cache would make stale in a way that
// actively misleads ("es ist auf Lager" when it no longer is).
func fetchShopSkuDetails(ctx context.Context, cfg shopConfig, erpSkuIDs []string) ([]shopSkuDetails, []string, error) {
	if len(erpSkuIDs) == 0 {
		return nil, nil, fmt.Errorf("shop: no erp_sku_ids given")
	}
	q := url.Values{}
	q.Set("erpSkuIds", strings.Join(erpSkuIDs, ","))
	raw, headers, err := shopAuthedGet(ctx, cfg, "/rest-api/v3/commerce-sku-details?"+q.Encode())
	if err != nil {
		return nil, nil, err
	}
	currency := shopDefaultCurrency
	if h := headers.Get("Currency"); h != "" {
		currency = h
	}
	return parseShopSkuDetails(raw, currency)
}

// parseShopSkuDetails extracts every item's stock/price from the confirmed
// commerce-sku-details shape, plus notFoundItems verbatim (erpSkuIds the
// request asked for but the shop has no record of).
func parseShopSkuDetails(raw []byte, currency string) ([]shopSkuDetails, []string, error) {
	var resp shopSkuDetailsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, fmt.Errorf("shop: parse sku-details response: %w", err)
	}
	out := make([]shopSkuDetails, 0, len(resp.Items))
	for _, ri := range resp.Items {
		d := shopSkuDetails{
			ErpSkuID:         ri.ID,
			CanBeAddedToCart: ri.Product.CanBeAddedToCart,
			IsSellout:        ri.Product.IsSellout,
			Currency:         currency,
		}
		if len(ri.Stock.Availabilities) > 0 {
			a := ri.Stock.Availabilities[0] // NATIONAL_STOCK is the only source observed so far — see package comment
			d.AvailabilityStatus = a.AvailabilityStatus
			d.StockLevel = a.StockLevel
			d.LeadTimeMinimumDays = a.LeadTimeMinimum
			d.LeadTimeMaximumDays = a.LeadTimeMaximum
			d.Unit = a.Unit.NameSingular
		}
		if !ri.Price.IsPriceOnRequest && len(ri.Price.Volumes) > 0 {
			v := ri.Price.Volumes[0] // lowest minimumQuantity tier
			d.Price = v.BasePriceIncludingTaxes
			d.MinimumQuantity = v.MinimumQuantity
		}
		out = append(out, d)
	}
	return out, resp.NotFoundItems, nil
}

// ---- Tool definition/execution ------------------------------------------

const shopSearchToolName = "search_shop_items"

// shopSearchToolDef describes the search_shop_items tool in OpenAI's
// function-calling schema shape — offered to both plain Chat and Agent
// (see handleAsk's shared tools/executors block, handlers.go) and, more
// narrowly, to the Mail draft flow (handleDraftReply).
func shopSearchToolDef() toolDef {
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name: shopSearchToolName,
			Description: "Durchsucht den Rubix-Onlineshop (de.rubix.com) nach konkreten, bestellbaren Artikeln — Name, Artikelnummer, Hersteller, EAN " +
				"und ein paar technische Kerndaten (z. B. Durchmesser, Länge, Material). ENTHÄLT KEINEN Preis und KEINEN Lagerbestand — dafür " +
				"nach dieser Suche gezielt get_shop_stock_and_price mit der Artikelnummer (erp_sku_id) des gewünschten Treffers aufrufen, aber nur " +
				"wenn tatsächlich nach Verfügbarkeit/Lieferzeit/Preis gefragt ist, nicht vorsorglich für jeden Treffer. " +
				"Nur für 'gibt es Artikel X im Shop' — NICHT für allgemeine Produktberatung, technische Spezifikationen allgemein oder Fragen, " +
				"die die Wissensbasis (search_knowledge_base) schon beantworten kann. " +
				"Ein Suchbegriff pro Aufruf, keine Boolesche Verknüpfung mehrerer Artikel. " +
				"Beispiel: query=\"Handschuhe Gr. 9\" liefert passende Handschuh-Artikel; query=\"4032871123456\" sucht direkt nach dieser Artikelnummer.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Suchbegriff: Artikelname, Artikelnummer/EAN oder eine kurze Produktbeschreibung — kein ganzer Satz und keine Frage."},
				},
				"required": []string{"query"},
			},
		},
	}
}

const shopProductDetailsToolName = "get_shop_product_details"

// shopProductDetailsToolDef describes the get_shop_product_details tool —
// resolves a specific, already-known product id (NOT found via a
// search_shop_items call earlier in this conversation — e.g. a customer
// pastes a "G1321010678"-style reference from an old quote/order/URL) into
// its current name/brand and, importantly, its erpSkuId for a follow-up
// get_shop_stock_and_price call.
func shopProductDetailsToolDef() toolDef {
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name: shopProductDetailsToolName,
			Description: "Ruft Name, Hersteller und Artikelnummer (erpSkuId) zu bereits bekannten Rubix-Shop-Produkten anhand ihrer Produkt-ID ab — " +
				"das \"id\"-Feld aus einem vorherigen search_shop_items-Ergebnis, oder eine von der Nutzerin direkt genannte Produkt-ID wie \"G1321010678\". " +
				"NUR nützlich, wenn eine konkrete Produkt-ID bereits bekannt ist, aber (noch) keine Artikelnummer oder kein Name dazu vorliegt — " +
				"typischerweise um direkt danach get_shop_stock_and_price mit der so ermittelten Artikelnummer aufzurufen. " +
				"NICHT für eine neue Produktsuche (dafür search_shop_items) und NICHT nötig, wenn die Produkt-ID gerade erst aus einem " +
				"search_shop_items-Ergebnis stammt (das liefert Name und Artikelnummer bereits mit).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"product_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Eine oder mehrere Produkt-IDs (das \"id\"-Feld, z. B. \"G1321010678\") — NICHT die Artikelnummer/erpSkuId.",
					},
				},
				"required": []string{"product_ids"},
			},
		},
	}
}

const shopStockToolName = "get_shop_stock_and_price"

// shopStockToolDef describes the get_shop_stock_and_price tool — the
// deliberate second half of the search/stock split (see package comment):
// live availability/lead-time/price for article(s) already identified via
// search_shop_items (or directly named by the user), fetched only when
// actually needed rather than bundled into every search result.
func shopStockToolDef() toolDef {
	return toolDef{
		Type: "function",
		Function: toolFunction{
			Name: shopStockToolName,
			Description: "Ruft aktuellen Lagerbestand, Lieferzeit und Preis für bereits bekannte Rubix-Shop-Artikel ab, anhand ihrer Artikelnummer " +
				"(erp_sku_id) — typischerweise aus einem vorherigen search_shop_items-Ergebnis. NUR aufrufen, wenn tatsächlich nach Verfügbarkeit, " +
				"Lieferzeit oder Preis gefragt ist (oder das für eine sinnvolle Antwort erkennbar nötig ist) — nicht vorsorglich für jeden Suchtreffer. " +
				"Mehrere Artikelnummern in einem Aufruf möglich, wenn mehrere Artikel gleichzeitig verglichen werden sollen.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"erp_sku_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Eine oder mehrere Artikelnummern (erpSkuId) aus einem vorherigen search_shop_items-Ergebnis oder direkt von der Nutzerin genannt.",
					},
				},
				"required": []string{"erp_sku_ids"},
			},
		},
	}
}

// appendShopTool adds all three Shop tools (search, product-details-by-id,
// live stock/price) to tools/executors when Shop is enabled, the caller's
// preset allows the "shop" tool category, and cfg.AccessControl allows user/
// groups — the exact same enabled+preset check was previously duplicated
// verbatim at every call site (Chat/Agent's shared tool set, the Mail draft
// flow, and the OpenAI-compatible API), now expressed once. All three are
// gated by the same "shop" category: they are facets of one
// connector/account, not independently restrictable.
func appendShopTool(tools []toolDef, executors map[string]toolExecutor, cfg shopConfig, presetTools []string, user string, groups []string) []toolDef {
	if cfg.Enabled && presetAllowsTool(presetTools, "shop") && cfg.AccessControl.allows(user, groups) {
		tools = append(tools, shopSearchToolDef(), shopProductDetailsToolDef(), shopStockToolDef())
		executors[shopSearchToolName] = shopSearchToolExecutor(cfg)
		executors[shopProductDetailsToolName] = shopProductDetailsToolExecutor(cfg)
		executors[shopStockToolName] = shopStockToolExecutor(cfg)
	}
	return tools
}

// shopSearchToolExecutor adapts searchShopItems to the generic
// toolExecutor shape — decode the model's JSON arguments, run the search,
// render a short text list including each item's erpSkuId (so the model
// can pass it to get_shop_stock_and_price) and a few key attributes.
func shopSearchToolExecutor(cfg shopConfig) toolExecutor {
	return func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("empty query")
		}
		items, total, err := searchShopItems(ctx, cfg, args.Query, shopMaxResults(cfg))
		if err != nil {
			// Prefixed distinctly from "(keine Artikel gefunden)" below so
			// the model doesn't conflate "the shop is unreachable/
			// misconfigured right now" with "this specific query had no
			// matches" — the right response to a user differs (say so
			// plainly vs. suggest a different search term).
			return "", fmt.Errorf("Shop-Suche momentan nicht möglich (%w) — nicht als \"kein Ergebnis\" interpretieren, sondern der Nutzerin mitteilen, dass die Shop-Abfrage gerade fehlschlägt", err)
		}
		if len(items) == 0 {
			return "(keine Artikel gefunden für diesen Suchbegriff — ggf. mit anderen/allgemeineren Begriffen erneut versuchen)", nil
		}
		var b strings.Builder
		for i, it := range items {
			fmt.Fprintf(&b, "%d. %s", i+1, it.Name)
			if it.ErpSkuID != "" {
				fmt.Fprintf(&b, " (Art.-Nr. %s)", it.ErpSkuID)
			}
			if it.Manufacturer != "" {
				fmt.Fprintf(&b, " — %s", it.Manufacturer)
			}
			if it.EAN != "" {
				fmt.Fprintf(&b, ", EAN %s", it.EAN)
			}
			b.WriteString("\n")
			for _, a := range it.Attributes {
				fmt.Fprintf(&b, "   %s: %s", a.Name, a.Value)
				if a.Unit != "" {
					fmt.Fprintf(&b, " %s", a.Unit)
				}
				b.WriteString("\n")
			}
		}
		if total > len(items) {
			fmt.Fprintf(&b, "(%d Treffer insgesamt — ggf. Suchbegriff eingrenzen für gezieltere Ergebnisse)\n", total)
		}
		return b.String(), nil
	}
}

// shopProductDetailsToolExecutor adapts fetchShopProductDetails to the
// generic toolExecutor shape.
func shopProductDetailsToolExecutor(cfg shopConfig) toolExecutor {
	return func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			ProductIDs []string `json:"product_ids"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
		if len(args.ProductIDs) == 0 {
			return "", fmt.Errorf("empty product_ids")
		}
		items, notFound, err := fetchShopProductDetails(ctx, cfg, args.ProductIDs)
		if err != nil {
			return "", fmt.Errorf("Produkt-Abfrage momentan nicht möglich (%w) — nicht als \"nicht gefunden\" interpretieren, sondern der Nutzerin mitteilen, dass die Abfrage gerade fehlschlägt", err)
		}
		if len(items) == 0 && len(notFound) == 0 {
			return "(keine Ergebnisse)", nil
		}
		var b strings.Builder
		for _, it := range items {
			fmt.Fprintf(&b, "%s: %s", it.ProductID, it.Name)
			if it.Manufacturer != "" {
				fmt.Fprintf(&b, " — %s", it.Manufacturer)
			}
			if it.ErpSkuID != "" {
				fmt.Fprintf(&b, " (Art.-Nr. %s)", it.ErpSkuID)
			}
			b.WriteString("\n")
		}
		for _, nf := range notFound {
			fmt.Fprintf(&b, "%s: nicht gefunden\n", nf)
		}
		return b.String(), nil
	}
}

// shopStockToolExecutor adapts fetchShopSkuDetails to the generic
// toolExecutor shape.
func shopStockToolExecutor(cfg shopConfig) toolExecutor {
	return func(ctx context.Context, argsJSON string) (string, error) {
		var args struct {
			ErpSkuIDs []string `json:"erp_sku_ids"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
		if len(args.ErpSkuIDs) == 0 {
			return "", fmt.Errorf("empty erp_sku_ids")
		}
		details, notFound, err := fetchShopSkuDetails(ctx, cfg, args.ErpSkuIDs)
		if err != nil {
			return "", fmt.Errorf("Bestands-/Preisabfrage momentan nicht möglich (%w) — nicht als \"nicht verfügbar\" interpretieren, sondern der Nutzerin mitteilen, dass die Abfrage gerade fehlschlägt", err)
		}
		if len(details) == 0 && len(notFound) == 0 {
			return "(keine Ergebnisse)", nil
		}
		var b strings.Builder
		for _, d := range details {
			fmt.Fprintf(&b, "%s: ", d.ErpSkuID)
			switch d.AvailabilityStatus {
			case "IN_STOCK":
				fmt.Fprintf(&b, "auf Lager (%d %s", d.StockLevel, d.Unit)
				if d.LeadTimeMinimumDays > 0 {
					fmt.Fprintf(&b, ", Lieferzeit %d", d.LeadTimeMinimumDays)
					if d.LeadTimeMaximumDays > d.LeadTimeMinimumDays {
						fmt.Fprintf(&b, "–%d", d.LeadTimeMaximumDays)
					}
					b.WriteString(" Tag(e)")
				}
				b.WriteString(")")
			case "OUT_OF_STOCK":
				b.WriteString("nicht auf Lager")
			default:
				fmt.Fprintf(&b, "Status: %s", valueOrDash(d.AvailabilityStatus))
			}
			if d.Price > 0 {
				fmt.Fprintf(&b, ", Preis %.2f %s", d.Price, d.Currency)
				if d.MinimumQuantity > 1 {
					fmt.Fprintf(&b, " ab %d %s", d.MinimumQuantity, d.Unit)
				}
			}
			if !d.CanBeAddedToCart {
				b.WriteString(" (aktuell nicht bestellbar)")
			}
			b.WriteString("\n")
		}
		for _, nf := range notFound {
			fmt.Fprintf(&b, "%s: nicht gefunden\n", nf)
		}
		return b.String(), nil
	}
}
