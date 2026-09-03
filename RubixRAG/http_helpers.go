package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// writeJSON is the standard success-path response writer shared by every
// HTTP handler. Encoding errors are deliberately swallowed: headers have
// already been sent by the time an encoder can fail.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError keeps error responses in the same JSON shape as successful
// API responses instead of falling back to net/http's text/plain body.
func writeJSONError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// requireMethod is the common 405 guard used by single-method endpoints.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		writeJSONError(w, method+" only", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// decodeJSONBody centralizes the request-body decoder while retaining the
// permissive behavior used by the existing API.
func decodeJSONBody(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

// makeSet converts a client selection slice into the lookup shape importers
// consume. Duplicate selections collapse naturally.
func makeSet[T comparable](items []T) map[T]bool {
	set := make(map[T]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

// serveStaticAsset returns a handler for one of the go:embed'd frontend
// assets (style.css/app.js/i18n.js/novapop.js, main.go) that supports
// conditional GET: the ETag (a content hash, computed once here since these
// strings never change after the binary is built — recompiling naturally
// changes the embedded content and therefore the hash) lets a browser send
// If-None-Match on a revisit and get a bodyless 304 back instead of
// re-downloading the full file every single page load, as it did before —
// meaningful for app.js, which runs well into the thousands of lines.
// Cache-Control: no-cache deliberately still forces that revalidation round
// trip on every load rather than skipping it for some duration — a stale
// cached copy served past a deploy (a new binary + restart, see README.md's
// "Deployment" section: no CDN/reverse-proxy cache sits in front today)
// would be a confusing, hard-to-explain bug for the sake of shaving off one
// small conditional request.
func serveStaticAsset(contentType, body string) http.HandlerFunc {
	sum := sha256.Sum256([]byte(body))
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}
}
