package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, map[string]any{"ok": true})
	if rec.Code != http.StatusOK {
		t.Errorf("expected default 200 status, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json content type, got %q", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("unexpected body: %v", body)
	}
}

func TestWriteJSONStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONStatus(rec, 201, map[string]any{"id": "x"})
	if rec.Code != 201 {
		t.Errorf("expected 201, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json content type, got %q", ct)
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, 404, "not found")
	if rec.Code != 404 {
		t.Errorf("expected 404, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body["error"] != "not found" {
		t.Errorf("unexpected error body: %v", body)
	}
}

func TestReadJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"tinyRAG"}`))
	var v struct {
		Name string `json:"name"`
	}
	if err := readJSON(req, &v); err != nil {
		t.Fatalf("readJSON failed: %v", err)
	}
	if v.Name != "tinyRAG" {
		t.Errorf("unexpected decoded value: %+v", v)
	}
}

func TestReadJSONInvalidBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`not json`))
	var v map[string]any
	if err := readJSON(req, &v); err == nil {
		t.Error("expected error decoding invalid JSON body")
	}
}
