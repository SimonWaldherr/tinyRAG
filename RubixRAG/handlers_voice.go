package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleVoiceTranscribe accepts one short audio recording and returns its
// local Whisper transcript. The request is deliberately ephemeral: unlike a
// document import it never writes a source, original, or chat-history row.
// The uploaded file is streamed straight into transcribeAudio's own temp
// file (never fully buffered here first) and the request body itself is
// capped at voiceRequestMaxBytes(s) — derived from the same per-settings
// audio-size limit transcribeAudio enforces, so the two can no longer
// silently disagree about what "too large" means.
func handleVoiceTranscribe(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s := settings.get()
	if !s.AllowShellExec {
		writeJSONError(w, "Whisper ist deaktiviert — externe CLI-Aufrufe zuerst in den Einstellungen erlauben", http.StatusForbidden)
		return
	}
	// Guest rate limit, same "authenticated callers are exempt/trusted
	// more" convention handleAsk already uses for globalAskLimiter — a
	// voice-transcription request is heavier (spawns ffmpeg/whisper.cpp),
	// so an anonymous caller left unbounded can exhaust host CPU even
	// faster than an unbounded /api/ask could.
	if _, hasSession := currentSession(r); !hasSession && !globalVoiceLimiter.allow(clientKey(r), s.API.GuestVoiceRateLimitPerMinute, time.Minute) {
		writeJSONError(w, "rate limit exceeded, please slow down", http.StatusTooManyRequests)
		return
	}

	maxBytes := voiceRequestMaxBytes(s)
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, fmt.Sprintf("Audio überschreitet das Upload-Limit von %d MB", maxBytes/(1024*1024)), http.StatusRequestEntityTooLarge)
			return
		}
		writeJSONError(w, "Audio-Upload konnte nicht gelesen werden: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("audio")
	if err != nil {
		writeJSONError(w, "Audio-Feld 'audio' fehlt", http.StatusBadRequest)
		return
	}
	defer file.Close()

	text, language, err := transcribeAudio(r.Context(), s, file, strings.TrimSpace(header.Filename))
	if err != nil {
		status := http.StatusUnprocessableEntity
		switch {
		case errors.Is(err, errVoiceAudioTooLarge):
			status = http.StatusRequestEntityTooLarge
		case errors.Is(err, errVoiceAudioEmpty):
			status = http.StatusBadRequest
		}
		writeJSONError(w, err.Error(), status)
		return
	}
	logAudit(r, "voice_transcribe", fmt.Sprintf("filename=%q", truncateRunesNote(header.Filename, 120)))
	writeJSON(w, map[string]any{"text": text, "language": language})
}
