package app

import (
	"encoding/json"
	"net/http"
)

// writeJSON sets the JSON content type and encodes v to w with a 200 status.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONStatus sets the JSON content type, writes the given status code,
// and encodes v to w.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error body of the form {"error": msg} with the
// given HTTP status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSONStatus(w, status, map[string]any{"error": msg})
}

// readJSON decodes the JSON request body into v.
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
