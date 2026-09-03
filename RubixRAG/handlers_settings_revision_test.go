package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleSettingsRejectsStaleRevision(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)
	handler := handleSettings(rag)

	get := httptest.NewRecorder()
	handler(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if get.Code != http.StatusOK || get.Header().Get(settingsRevisionHeader) != "0" {
		t.Fatalf("initial GET status=%d revision=%q", get.Code, get.Header().Get(settingsRevisionHeader))
	}

	first := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(`{"chunk_size":901}`))
	first.Header.Set(settingsRevisionHeader, "0")
	firstRec := httptest.NewRecorder()
	handler(firstRec, first)
	if firstRec.Code != http.StatusOK || firstRec.Header().Get(settingsRevisionHeader) != "1" {
		t.Fatalf("current revision save status=%d revision=%q body=%s", firstRec.Code, firstRec.Header().Get(settingsRevisionHeader), firstRec.Body.String())
	}
	if got := settings.get().ChunkSize; got != 901 {
		t.Fatalf("current revision save ChunkSize=%d, want 901", got)
	}

	stale := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(`{"chunk_size":902}`))
	stale.Header.Set(settingsRevisionHeader, "0")
	staleRec := httptest.NewRecorder()
	handler(staleRec, stale)
	if staleRec.Code != http.StatusConflict {
		t.Fatalf("stale revision status=%d, want 409 (body=%s)", staleRec.Code, staleRec.Body.String())
	}
	if staleRec.Header().Get(settingsRevisionHeader) != "1" {
		t.Fatalf("conflict must advertise current revision, got %q", staleRec.Header().Get(settingsRevisionHeader))
	}
	if got := settings.get().ChunkSize; got != 901 {
		t.Fatalf("stale save must not overwrite newer setting, got ChunkSize=%d", got)
	}
}

func TestHandleSettingsRejectsOversizedRequest(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body := `{"prompts_dir":"` + strings.Repeat("x", settingsRequestMaxBytes) + `"}`
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status=%d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleSettingsRejectsTrailingJSONValue(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"chunk_size":901} {"chunk_size":902}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := settings.get().ChunkSize; got != s.ChunkSize {
		t.Fatalf("trailing JSON changed ChunkSize=%d, want %d", got, s.ChunkSize)
	}
}
