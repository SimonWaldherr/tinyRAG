package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVoiceAudioMaxBytesUsesDefaultAndCapsLegacyValues(t *testing.T) {
	if got := voiceAudioMaxBytes(appSettings{}); got != voiceAudioMaxBytesDefault {
		t.Fatalf("zero config: got %d, want %d", got, voiceAudioMaxBytesDefault)
	}
	if got := voiceAudioMaxBytes(appSettings{Import: importConfig{MaxFileMB: 10}}); got != 10*1024*1024 {
		t.Fatalf("configured limit: got %d", got)
	}
	if got := voiceAudioMaxBytes(appSettings{Import: importConfig{MaxFileMB: 1 << 30}}); got != 1024*1024*1024 {
		t.Fatalf("legacy overflow guard: got %d", got)
	}
}

// TestVoiceRequestMaxBytesTracksAudioLimit guards the fix for the previous
// mismatch: the outer HTTP-request cap must always be at least the real,
// per-settings audio-size limit plus a small multipart-framing allowance —
// never an independent, potentially SMALLER hardcoded constant (32 MiB) that
// could silently override a legitimately-configured larger MaxFileMB with a
// confusing, unrelated error message.
func TestVoiceRequestMaxBytesTracksAudioLimit(t *testing.T) {
	cases := []struct {
		maxFileMB int64
		wantAudio int64
	}{
		{0, voiceAudioMaxBytesDefault},
		{50, 50 * 1024 * 1024},  // above the old hardcoded 32 MiB request cap
		{500, 500 * 1024 * 1024}, // far above it
	}
	for _, c := range cases {
		s := appSettings{Import: importConfig{MaxFileMB: c.maxFileMB}}
		gotAudio := voiceAudioMaxBytes(s)
		if gotAudio != c.wantAudio {
			t.Fatalf("MaxFileMB=%d: voiceAudioMaxBytes=%d, want %d", c.maxFileMB, gotAudio, c.wantAudio)
		}
		gotRequest := voiceRequestMaxBytes(s)
		if gotRequest < gotAudio {
			t.Fatalf("MaxFileMB=%d: voiceRequestMaxBytes=%d is SMALLER than voiceAudioMaxBytes=%d — the outer cap must never be tighter than the inner one", c.maxFileMB, gotRequest, gotAudio)
		}
		if gotRequest != gotAudio+voiceMultipartOverhead {
			t.Fatalf("MaxFileMB=%d: voiceRequestMaxBytes=%d, want audio(%d)+overhead(%d)", c.maxFileMB, gotRequest, gotAudio, voiceMultipartOverhead)
		}
	}
}

func TestCappedOutputNeverGrowsPastLimit(t *testing.T) {
	var out cappedOutput
	out.limit = 4
	if n, err := out.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write returned n=%d err=%v", n, err)
	}
	if out.String() != "abcd" || !out.truncated {
		t.Fatalf("got %q truncated=%v", out.String(), out.truncated)
	}
}

// TestCappedOutputStringWithTruncationNoteFlagsCutoff guards the fix for a
// silently-incomplete error message: a truncated buffer must say so, not
// read as if the captured text were the whole story.
func TestCappedOutputStringWithTruncationNoteFlagsCutoff(t *testing.T) {
	var truncated cappedOutput
	truncated.limit = 4
	_, _ = truncated.Write([]byte("abcdef"))
	if got := truncated.stringWithTruncationNote(); !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "gekürzt") {
		t.Fatalf("want truncation flagged, got %q", got)
	}

	var whole cappedOutput
	whole.limit = 100
	_, _ = whole.Write([]byte("complete message"))
	if got := whole.stringWithTruncationNote(); got != "complete message" {
		t.Fatalf("want the untruncated message unchanged, got %q", got)
	}
}

func TestWhisperTimeoutDefaultsAndHonorsContext(t *testing.T) {
	if got := whisperTimeout(importConfig{}); got != voiceProcessTimeout {
		t.Fatalf("default timeout: got %s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := transcribeAudio(ctx, appSettings{}, strings.NewReader("audio"), "recording.wav"); err == nil {
		t.Fatal("expected AllowShellExec guard before external process")
	}
}

// TestTranscribeAudioRejectsOversizedStream confirms the streamed size check
// (io.Copy into a LimitReader, replacing the old fully-buffered []byte
// parameter) still catches an oversized upload — via errVoiceAudioTooLarge
// specifically, which handleVoiceTranscribe relies on for its 413 response.
func TestTranscribeAudioRejectsOversizedStream(t *testing.T) {
	s := appSettings{AllowShellExec: true, Import: importConfig{MaxFileMB: 0}} // default 25 MiB
	big := strings.NewReader(strings.Repeat("x", int(voiceAudioMaxBytesDefault)+1))
	_, _, err := transcribeAudio(context.Background(), s, big, "recording.webm")
	if err == nil {
		t.Fatal("want an error for an oversized stream")
	}
	if !errors.Is(err, errVoiceAudioTooLarge) {
		t.Fatalf("want errVoiceAudioTooLarge, got %v", err)
	}
}

// TestTranscribeAudioRejectsEmptyStream confirms an empty upload is caught
// before any subprocess (ffmpeg/whisper-cli) is ever spawned — via
// errVoiceAudioEmpty specifically.
func TestTranscribeAudioRejectsEmptyStream(t *testing.T) {
	s := appSettings{AllowShellExec: true}
	_, _, err := transcribeAudio(context.Background(), s, strings.NewReader(""), "recording.webm")
	if err == nil {
		t.Fatal("want an error for an empty stream")
	}
	if !errors.Is(err, errVoiceAudioEmpty) {
		t.Fatalf("want errVoiceAudioEmpty, got %v", err)
	}
}

func TestValidateImportSettingsWhisperTimeout(t *testing.T) {
	if err := validateImportSettings(importConfig{WhisperTimeoutSeconds: -1}); err == nil {
		t.Fatal("expected negative Whisper timeout to be rejected")
	}
	if err := validateImportSettings(importConfig{WhisperTimeoutSeconds: 601}); err == nil {
		t.Fatal("expected oversized Whisper timeout to be rejected")
	}
	if err := validateImportSettings(importConfig{WhisperTimeoutSeconds: 120}); err != nil {
		t.Fatalf("valid Whisper timeout rejected: %v", err)
	}
}

// TestValidateImportSettingsWhisperTuningFields covers the new
// performance-tuning fields' validation, including the VAD/VADModel
// pairing requirement (whisper.cpp's --vad needs a model path; a silent
// no-op would look like VAD were on when it isn't).
func TestValidateImportSettingsWhisperTuningFields(t *testing.T) {
	cases := []struct {
		name    string
		cfg     importConfig
		wantErr bool
	}{
		{"negative threads", importConfig{WhisperThreads: -1}, true},
		{"zero threads (default)", importConfig{WhisperThreads: 0}, false},
		{"positive threads", importConfig{WhisperThreads: 8}, false},
		{"negative beam size", importConfig{WhisperBeamSize: -1}, true},
		{"positive beam size", importConfig{WhisperBeamSize: 1}, false},
		{"max concurrent too high", importConfig{WhisperMaxConcurrent: 33}, true},
		{"max concurrent negative", importConfig{WhisperMaxConcurrent: -1}, true},
		{"max concurrent valid", importConfig{WhisperMaxConcurrent: 4}, false},
		{"VAD without model", importConfig{WhisperVAD: true}, true},
		{"VAD with model", importConfig{WhisperVAD: true, WhisperVADModel: "models/ggml-silero-vad.bin"}, false},
		{"VAD model without VAD flag is harmless", importConfig{WhisperVADModel: "models/ggml-silero-vad.bin"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateImportSettings(c.cfg)
			if c.wantErr && err == nil {
				t.Fatalf("want an error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

func TestWhisperMaxConcurrentResolvesDefault(t *testing.T) {
	if got := whisperMaxConcurrent(importConfig{}); got != voiceMaxConcurrentDefault {
		t.Fatalf("want default %d, got %d", voiceMaxConcurrentDefault, got)
	}
	if got := whisperMaxConcurrent(importConfig{WhisperMaxConcurrent: 5}); got != 5 {
		t.Fatalf("want configured 5, got %d", got)
	}
}

// TestAcquireVoiceSlotCapsConcurrency confirms acquireVoiceSlot actually
// bounds simultaneous holders at limit — the semaphore this session added
// so unbounded parallel push-to-talk requests can't thrash the host with
// simultaneous whisper.cpp processes.
func TestAcquireVoiceSlotCapsConcurrency(t *testing.T) {
	const limit = 2
	const workers = 6
	var current, maxSeen int32
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			release, err := acquireVoiceSlot(context.Background(), limit)
			if err != nil {
				t.Errorf("acquireVoiceSlot: %v", err)
				return
			}
			defer release()
			n := atomic.AddInt32(&current, 1)
			for {
				old := atomic.LoadInt32(&maxSeen)
				if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt32(&current, -1)
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	if got := atomic.LoadInt32(&maxSeen); got > limit {
		t.Fatalf("want at most %d concurrent holders, saw %d", limit, got)
	}
}

// TestAcquireVoiceSlotDisabledByZeroLimit confirms limit<=0 disables the cap
// entirely (every caller proceeds immediately), matching this codebase's
// "0 = off" convention elsewhere.
func TestAcquireVoiceSlotDisabledByZeroLimit(t *testing.T) {
	release, err := acquireVoiceSlot(context.Background(), 0)
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	release()
}

// TestAcquireVoiceSlotHonorsContextCancellation confirms a caller stuck
// waiting for a free slot gives up when its context is cancelled, rather
// than blocking forever.
func TestAcquireVoiceSlotHonorsContextCancellation(t *testing.T) {
	release, err := acquireVoiceSlot(context.Background(), 1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := acquireVoiceSlot(ctx, 1); err == nil {
		t.Fatal("want a context-deadline error while the only slot is held")
	}
}

func TestWhisperAutoDetectRegexpMatchesStderrLine(t *testing.T) {
	m := whisperAutoDetectRe.FindStringSubmatch("whisper_full_with_state: auto-detected language: en (p = 0.930747)")
	if m == nil || m[1] != "en" {
		t.Fatalf("want language 'en' extracted, got %v", m)
	}
	if whisperAutoDetectRe.FindStringSubmatch("no such line here") != nil {
		t.Fatal("want no match for unrelated text")
	}
}
