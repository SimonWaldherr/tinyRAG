package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	voiceAudioMaxBytesDefault = 25 * 1024 * 1024
	voiceTranscriptMaxBytes   = 4 * 1024 * 1024
	voiceProcessTimeout       = 120 * time.Second
	voiceFFmpegTimeout        = 60 * time.Second
	voiceCLIOutputMaxBytes    = 256 * 1024
	// voiceMultipartOverhead is added on top of voiceAudioMaxBytes when
	// bounding the whole HTTP request (handlers_voice.go) — a multipart
	// body is the audio bytes plus boundary markers/headers/the field
	// name, a few hundred bytes at most; this is a generous, fixed slack
	// so the outer request-level cap and the inner audio-size check never
	// disagree about what "too large" means (previously two independent
	// hardcoded constants, 32 MiB outer vs configurable-up-to-1GiB inner,
	// could silently truncate a legitimately-configured larger limit at
	// the outer layer with a confusing, unrelated error message).
	voiceMultipartOverhead = 64 * 1024
	// voiceMaxConcurrentDefault bounds server-wide simultaneous
	// transcriptions when importConfig.WhisperMaxConcurrent is unset (0).
	// Each transcription is a heavy, CPU-bound whisper.cpp process
	// reloading its full model from disk — a handful of colleagues
	// push-to-talking at the same moment must not be allowed to spawn
	// unboundedly many of these at once.
	voiceMaxConcurrentDefault = 2
	// voiceConcurrentPollInterval is how often acquireVoiceSlot re-checks
	// for a free slot while waiting. Coarse on purpose: transcriptions run
	// for seconds, not milliseconds, so this adds negligible latency
	// without needing a condition-variable's added complexity — and,
	// unlike a fixed-capacity channel, plainly re-reads the live
	// WhisperMaxConcurrent setting on every poll, so an admin's change
	// takes effect on already-waiting requests too, not just new ones.
	voiceConcurrentPollInterval = 50 * time.Millisecond
)

// errVoiceAudioTooLarge/errVoiceAudioEmpty are sentinel wrapped errors so
// handleVoiceTranscribe (handlers_voice.go) can pick the right HTTP status
// (413/400) for these two specific, common causes while every other
// transcribeAudio failure still falls back to 422 — see errors.Is at the
// call site.
var (
	errVoiceAudioTooLarge = errors.New("voice audio too large")
	errVoiceAudioEmpty    = errors.New("voice audio empty")
)

// voiceConcurrentMu/voiceConcurrentNow track how many transcriptions are
// currently running server-wide — see acquireVoiceSlot.
var (
	voiceConcurrentMu  sync.Mutex
	voiceConcurrentNow int
)

// acquireVoiceSlot blocks (context-aware) until fewer than limit
// transcriptions are running server-wide, then reserves one slot; the
// caller must invoke the returned release func exactly once, on every
// return path. limit <= 0 disables the cap entirely (every caller proceeds
// immediately), matching this codebase's usual "0 = off" convention.
func acquireVoiceSlot(ctx context.Context, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	for {
		voiceConcurrentMu.Lock()
		if voiceConcurrentNow < limit {
			voiceConcurrentNow++
			voiceConcurrentMu.Unlock()
			return func() {
				voiceConcurrentMu.Lock()
				voiceConcurrentNow--
				voiceConcurrentMu.Unlock()
			}, nil
		}
		voiceConcurrentMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(voiceConcurrentPollInterval):
		}
	}
}

// whisperMaxConcurrent resolves importConfig.WhisperMaxConcurrent, 0
// meaning voiceMaxConcurrentDefault.
func whisperMaxConcurrent(cfg importConfig) int {
	if cfg.WhisperMaxConcurrent <= 0 {
		return voiceMaxConcurrentDefault
	}
	return cfg.WhisperMaxConcurrent
}

// transcribeAudio runs the configured local Whisper CLI and returns the
// transcript plus the language actually used ("auto" or an auto-detected
// code when WhisperLanguage is unset, otherwise the configured code
// verbatim — see runWhisperCLI). audio is streamed (never fully buffered by
// the caller) into a private temporary file bounded by voiceAudioMaxBytes,
// which is removed on every return path; nothing is persisted in the vector
// store or chat history. Every uploaded clip is normalized through ffmpeg
// regardless of its claimed extension — a caller-supplied ".wav" filename
// was previously trusted at face value and skipped normalization entirely,
// letting a mislabeled or wrong-format file reach whisper.cpp without the
// 16kHz/mono/pcm_s16le shape it expects.
func transcribeAudio(parent context.Context, s appSettings, audio io.Reader, filename string) (text string, language string, err error) {
	if !s.AllowShellExec {
		return "", "", fmt.Errorf("Whisper ist deaktiviert (Einstellungen → Import → externe CLI-Aufrufe erlauben)")
	}
	maxBytes := voiceAudioMaxBytes(s)

	ext := fileExtension(filename)
	if ext == "" {
		ext = ".webm"
	}
	in, err := os.CreateTemp("", "r3-voice-*"+ext)
	if err != nil {
		return "", "", fmt.Errorf("Audio-Tempdatei: %w", err)
	}
	inPath := in.Name()
	defer os.Remove(inPath)

	n, copyErr := io.Copy(in, io.LimitReader(audio, maxBytes+1))
	closeErr := in.Close()
	if copyErr != nil {
		return "", "", fmt.Errorf("Audio schreiben: %w", copyErr)
	}
	if closeErr != nil {
		return "", "", fmt.Errorf("Audio schließen: %w", closeErr)
	}
	if n > maxBytes {
		return "", "", fmt.Errorf("%w: Audio zu groß (Limit %d MB)", errVoiceAudioTooLarge, maxBytes/(1024*1024))
	}
	if n == 0 {
		return "", "", fmt.Errorf("%w: Audio ist leer", errVoiceAudioEmpty)
	}

	wav, err := os.CreateTemp("", "r3-voice-*.wav")
	if err != nil {
		return "", "", fmt.Errorf("WAV-Tempdatei: %w", err)
	}
	wavPath := wav.Name()
	if err := wav.Close(); err != nil {
		os.Remove(wavPath)
		return "", "", fmt.Errorf("WAV-Tempdatei schließen: %w", err)
	}
	defer os.Remove(wavPath)
	if err := runVoiceFFmpeg(parent, s.Import.FFmpegBin, inPath, wavPath); err != nil {
		return "", "", err
	}

	release, err := acquireVoiceSlot(parent, whisperMaxConcurrent(s.Import))
	if err != nil {
		return "", "", fmt.Errorf("Warten auf freien Transkriptions-Platz abgebrochen: %w", err)
	}
	defer release()

	return runWhisperCLI(parent, s.Import, wavPath)
}

func voiceAudioMaxBytes(s appSettings) int64 {
	maxMB := s.Import.MaxFileMB
	if maxMB <= 0 {
		return voiceAudioMaxBytesDefault
	}
	// MaxFileMB is validated at settings save time; keep this helper overflow
	// safe for hand-edited legacy settings as well.
	if maxMB > 1024 {
		maxMB = 1024
	}
	return maxMB * 1024 * 1024
}

// voiceRequestMaxBytes bounds the whole HTTP request (handlers_voice.go) —
// always voiceAudioMaxBytes plus a small fixed multipart-framing allowance,
// so the outer request-level cap can never be tighter than the real,
// per-settings audio-size limit it's meant to guard the same thing as (see
// voiceMultipartOverhead's doc comment for the bug this replaces).
func voiceRequestMaxBytes(s appSettings) int64 {
	return voiceAudioMaxBytes(s) + voiceMultipartOverhead
}

func whisperTimeout(cfg importConfig) time.Duration {
	if cfg.WhisperTimeoutSeconds <= 0 {
		return voiceProcessTimeout
	}
	return time.Duration(cfg.WhisperTimeoutSeconds) * time.Second
}

func runVoiceFFmpeg(parent context.Context, bin, inputPath, outputPath string) error {
	if strings.TrimSpace(bin) == "" {
		bin = "ffmpeg"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("ffmpeg binary %q nicht gefunden: %w", bin, err)
	}
	ctx, cancel := context.WithTimeout(parent, voiceFFmpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "-nostdin", "-v", "error", "-i", inputPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-y", outputPath)
	var stderr cappedOutput
	stderr.limit = voiceCLIOutputMaxBytes
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg Audio-Konvertierung abgebrochen: %w", ctx.Err())
		}
		return fmt.Errorf("ffmpeg Audio-Konvertierung fehlgeschlagen: %s", stderr.stringWithTruncationNote())
	}
	return nil
}

// whisperAutoDetectRe matches whisper.cpp's own log line announcing which
// language it auto-detected (printed to stderr, e.g. "whisper_full_with_
// state: auto-detected language: en (p = 0.93...)") — used by runWhisperCLI
// to report what was actually used instead of blindly echoing back the
// (possibly empty) configured language.
var whisperAutoDetectRe = regexp.MustCompile(`auto-detected language:\s*([a-zA-Z-]+)`)

// runWhisperCLI targets the whisper.cpp CLI contract. The model flag is
// optional so binaries with a configured default still work; when present it
// is passed verbatim as a local filesystem path, never through a shell.
// Performance-tuning flags (threads/beam-size/flash-attn/VAD) are only ever
// added when the admin has actually configured them (see importConfig's
// Whisper* doc comments) — left untouched, whisper.cpp's own built-in
// defaults apply exactly as before these settings existed. The second
// return value is the language actually used: cfg.WhisperLanguage verbatim
// when it was set (that's what "-l" told whisper.cpp to use, no detection
// involved), otherwise whisper.cpp's own auto-detected code parsed from its
// stderr log, or "auto" if that line isn't found — previously the caller
// always echoed back the configured (possibly empty) value regardless of
// what auto-detection actually produced.
func runWhisperCLI(parent context.Context, cfg importConfig, inputPath string) (text string, language string, err error) {
	bin := strings.TrimSpace(cfg.WhisperBin)
	if bin == "" {
		bin = "whisper-cli"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", "", fmt.Errorf("Whisper-Binary %q nicht gefunden: %w", bin, err)
	}
	outFile, err := os.CreateTemp("", "r3-whisper-output-")
	if err != nil {
		return "", "", fmt.Errorf("Whisper-Ausgabedatei: %w", err)
	}
	outBase := outFile.Name()
	if err := outFile.Close(); err != nil {
		os.Remove(outBase)
		return "", "", err
	}
	defer os.Remove(outBase)
	defer os.Remove(outBase + ".txt")

	args := make([]string, 0, 18)
	if model := strings.TrimSpace(cfg.WhisperModel); model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, "-f", inputPath)
	configuredLanguage := strings.TrimSpace(cfg.WhisperLanguage)
	if configuredLanguage != "" {
		args = append(args, "-l", configuredLanguage)
	}
	if cfg.WhisperThreads > 0 {
		args = append(args, "--threads", strconv.Itoa(cfg.WhisperThreads))
	}
	if cfg.WhisperBeamSize > 0 {
		args = append(args, "--beam-size", strconv.Itoa(cfg.WhisperBeamSize))
	}
	if cfg.WhisperFlashAttn {
		args = append(args, "--flash-attn")
	}
	if cfg.WhisperVAD && strings.TrimSpace(cfg.WhisperVADModel) != "" {
		args = append(args, "--vad", "--vad-model", cfg.WhisperVADModel)
	}
	args = append(args, "-otxt", "-of", outBase)
	ctx, cancel := context.WithTimeout(parent, whisperTimeout(cfg))
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, args...)
	var stdout, stderr cappedOutput
	stdout.limit = voiceCLIOutputMaxBytes
	stderr.limit = voiceCLIOutputMaxBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("Whisper-Transkription abgebrochen: %w", ctx.Err())
		}
		return "", "", fmt.Errorf("Whisper fehlgeschlagen: %s", stderr.stringWithTruncationNote())
	}

	// usedLanguage is only ever detected (not just echoed) when the admin
	// left WhisperLanguage empty — with an explicit language configured,
	// "-l <code>" already IS what was used, no detection ran at all.
	usedLanguage := configuredLanguage
	if configuredLanguage == "" {
		usedLanguage = "auto"
		if m := whisperAutoDetectRe.FindStringSubmatch(stderr.String()); m != nil {
			usedLanguage = m[1]
		}
	}

	textPath := outBase + ".txt"
	file, readErr := os.Open(textPath)
	if readErr == nil {
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, voiceTranscriptMaxBytes+1))
		if err != nil {
			return "", "", fmt.Errorf("Whisper-Ausgabe lesen: %w", err)
		}
		if len(data) > voiceTranscriptMaxBytes {
			return "", "", fmt.Errorf("Whisper-Transkript überschreitet das Limit von %d Bytes", voiceTranscriptMaxBytes)
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			return repairMojibake(text), usedLanguage, nil
		}
	}
	if text := strings.TrimSpace(stdout.String()); text != "" {
		return repairMojibake(text), usedLanguage, nil
	}
	if readErr != nil {
		return "", "", fmt.Errorf("Whisper erzeugte keine Textausgabe: %w", readErr)
	}
	return "", "", fmt.Errorf("Whisper erzeugte kein Transkript")
}

type cappedOutput struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// stringWithTruncationNote returns b's captured text, appending a note when
// the underlying limit cut it off mid-output — previously a truncated
// ffmpeg/whisper.cpp error could read as complete when the real cause (often
// the first, most useful line) had actually scrolled past the cap.
func (b *cappedOutput) stringWithTruncationNote() string {
	s := strings.TrimSpace(b.String())
	if b.truncated {
		s += " […gekürzt]"
	}
	return s
}
