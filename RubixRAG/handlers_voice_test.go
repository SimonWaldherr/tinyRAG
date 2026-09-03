package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// buildVoiceMultipartBody builds a multipart/form-data body with a single
// "audio" file field of exactly the given content, returning the body and
// its Content-Type header value (boundary included) — everything
// handleVoiceTranscribe's r.ParseMultipartForm/r.FormFile("audio") path
// needs from a real browser upload.
func buildVoiceMultipartBody(t *testing.T, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("audio", "r3-voice.webm")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func postVoiceTranscribe(t *testing.T, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/voice/transcribe", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	handleVoiceTranscribe(rec, req)
	return rec
}

// TestHandleVoiceTranscribeRejectsWhenShellExecDisabled guards the
// handler-level AllowShellExec gate — the whole feature must refuse to even
// attempt parsing an upload when external CLI calls aren't opted into,
// matching every other markitdown/tesseract/ffmpeg call site's posture.
func TestHandleVoiceTranscribeRejectsWhenShellExecDisabled(t *testing.T) {
	withTestGlobalSettings(t, appSettings{AllowShellExec: false})
	body, ct := buildVoiceMultipartBody(t, []byte("irrelevant"))
	rec := postVoiceTranscribe(t, body, ct)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleVoiceTranscribeRejectsMissingField confirms a request with no
// "audio" part at all is rejected clearly (400), not treated as a mysterious
// transcription failure.
func TestHandleVoiceTranscribeRejectsMissingField(t *testing.T) {
	withTestGlobalSettings(t, appSettings{AllowShellExec: true})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("not-audio", "irrelevant")
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	rec := postVoiceTranscribe(t, &buf, mw.FormDataContentType())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audio") {
		t.Fatalf("want the error to mention the missing 'audio' field, got %s", rec.Body.String())
	}
}

// TestHandleVoiceTranscribeRejectsEmptyAudio confirms an "audio" field with
// zero bytes of content is rejected (400) BEFORE any subprocess is spawned —
// exercised through the full HTTP handler, not just transcribeAudio
// directly, so the status-code mapping (errVoiceAudioEmpty -> 400) is
// covered too.
func TestHandleVoiceTranscribeRejectsEmptyAudio(t *testing.T) {
	withTestGlobalSettings(t, appSettings{AllowShellExec: true})
	body, ct := buildVoiceMultipartBody(t, []byte{})
	rec := postVoiceTranscribe(t, body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleVoiceTranscribeRejectsOversizedBody confirms the dynamic,
// per-settings request-size cap (voiceRequestMaxBytes) actually bounds the
// real HTTP request — a small configured MaxFileMB must reject a body
// larger than IT, not silently accept anything up to the old hardcoded
// 32 MiB regardless of what the admin configured.
func TestHandleVoiceTranscribeRejectsOversizedBody(t *testing.T) {
	withTestGlobalSettings(t, appSettings{AllowShellExec: true, Import: importConfig{MaxFileMB: 1}})
	oversized := bytes.Repeat([]byte("x"), 2*1024*1024) // 2 MiB, above the configured 1 MiB audio cap
	body, ct := buildVoiceMultipartBody(t, oversized)
	rec := postVoiceTranscribe(t, body, ct)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleVoiceTranscribeRejectsWrongMethod confirms the method guard
// runs before any multipart parsing.
func TestHandleVoiceTranscribeRejectsWrongMethod(t *testing.T) {
	withTestGlobalSettings(t, appSettings{AllowShellExec: true})
	req := httptest.NewRequest(http.MethodGet, "/api/voice/transcribe", nil)
	rec := httptest.NewRecorder()
	handleVoiceTranscribe(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

// TestHandleVoiceTranscribeAppliesGuestRateLimit confirms
// api.guest_voice_rate_limit_per_minute actually bounds an anonymous
// caller's request rate, the same "0 = off, logged-in exempt" convention
// /api/ask's globalAskLimiter already uses — exercised via repeated
// requests that fail for an unrelated reason (AllowShellExec off) so the
// test never depends on any real ffmpeg/whisper-cli being installed; only
// the rate-limit response (429) itself is asserted once the budget is used
// up.
func TestHandleVoiceTranscribeAppliesGuestRateLimit(t *testing.T) {
	withTestGlobalSettings(t, appSettings{AllowShellExec: true, API: apiConfig{GuestVoiceRateLimitPerMinute: 1}})
	t.Cleanup(func() {
		globalVoiceLimiter.mu.Lock()
		globalVoiceLimiter.hits = map[string][]time.Time{}
		globalVoiceLimiter.mu.Unlock()
	})

	// Empty audio fails fast (400) before any subprocess is spawned, so
	// this test never depends on a real ffmpeg/whisper-cli being
	// installed — only the rate-limit response (429) on the second
	// request is asserted.
	body1, ct1 := buildVoiceMultipartBody(t, []byte{})
	rec1 := postVoiceTranscribe(t, body1, ct1)
	if rec1.Code == http.StatusTooManyRequests {
		t.Fatalf("first attempt within budget must not be rate-limited, got %d", rec1.Code)
	}

	body2, ct2 := buildVoiceMultipartBody(t, []byte{})
	rec2 := postVoiceTranscribe(t, body2, ct2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second attempt beyond the 1/minute budget: want 429, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestHandleSettingsPersistsWhisperTuningFields guards the recurring
// "new settings field added to the struct but never wired into
// handleSettings' merge closure silently fails to persist" bug pattern
// this repo has hit before (see AGENTS.md) — confirms every new Whisper
// performance-tuning field (and the guest voice rate limit) actually
// round-trips through POST /api/settings.
func TestHandleSettingsPersistsWhisperTuningFields(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"import": map[string]any{
			"whisper_threads":        6,
			"whisper_beam_size":      1,
			"whisper_flash_attn":     true,
			"whisper_vad":            true,
			"whisper_vad_model":      "models/ggml-silero-vad.bin",
			"whisper_max_concurrent": 3,
		},
		"api": map[string]any{
			"guest_voice_rate_limit_per_minute": 5,
		},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := settings.get()
	if got.Import.WhisperThreads != 6 {
		t.Errorf("WhisperThreads did not persist: got %d", got.Import.WhisperThreads)
	}
	if got.Import.WhisperBeamSize != 1 {
		t.Errorf("WhisperBeamSize did not persist: got %d", got.Import.WhisperBeamSize)
	}
	if !got.Import.WhisperFlashAttn {
		t.Errorf("WhisperFlashAttn did not persist")
	}
	if !got.Import.WhisperVAD {
		t.Errorf("WhisperVAD did not persist")
	}
	if got.Import.WhisperVADModel != "models/ggml-silero-vad.bin" {
		t.Errorf("WhisperVADModel did not persist: got %q", got.Import.WhisperVADModel)
	}
	if got.Import.WhisperMaxConcurrent != 3 {
		t.Errorf("WhisperMaxConcurrent did not persist: got %d", got.Import.WhisperMaxConcurrent)
	}
	if got.API.GuestVoiceRateLimitPerMinute != 5 {
		t.Errorf("GuestVoiceRateLimitPerMinute did not persist: got %d", got.API.GuestVoiceRateLimitPerMinute)
	}
}
