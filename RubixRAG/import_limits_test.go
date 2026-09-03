package main

import (
	"context"
	"testing"
	"time"
)

func TestImportLimitDefaultsAndCeilings(t *testing.T) {
	// Unset → defaults.
	if got := importMaxItems(importConfig{}); got != importMaxItemsDefault {
		t.Errorf("max default: want %d, got %d", importMaxItemsDefault, got)
	}
	if got := importPreviewLimit(importConfig{}); got != importPreviewDefault {
		t.Errorf("preview default: want %d, got %d", importPreviewDefault, got)
	}
	if got := importRequestDelay(importConfig{}); got != 0 {
		t.Errorf("delay default: want 0, got %v", got)
	}
	// Explicit values honored.
	if got := importMaxItems(importConfig{MaxItemsPerRun: 42}); got != 42 {
		t.Errorf("max explicit: want 42, got %d", got)
	}
	if got := importRequestDelay(importConfig{RequestDelayMS: 250}); got != 250*time.Millisecond {
		t.Errorf("delay explicit: want 250ms, got %v", got)
	}
	// Ceilings clamp a fat-fingered config.
	if got := importMaxItems(importConfig{MaxItemsPerRun: 10_000_000}); got != importMaxItemsCeiling {
		t.Errorf("max ceiling: want %d, got %d", importMaxItemsCeiling, got)
	}
	if got := importPreviewLimit(importConfig{PreviewLimit: 999999}); got != importPreviewCeiling {
		t.Errorf("preview ceiling: want %d, got %d", importPreviewCeiling, got)
	}
}

func TestClampPerPage(t *testing.T) {
	if got := clampPerPage(500, 100); got != 100 {
		t.Errorf("want clamped to API max 100, got %d", got)
	}
	if got := clampPerPage(30, 100); got != 30 {
		t.Errorf("want 30 passed through, got %d", got)
	}
	if got := clampPerPage(0, 100); got != importPreviewDefault {
		t.Errorf("want default for 0, got %d", got)
	}
}

// TestImportPacerCapAndCount is the core anti-100k guarantee: a pacer
// stops allowing items exactly at the configured cap.
func TestImportPacerCapAndCount(t *testing.T) {
	p := newImportPacer(importConfig{MaxItemsPerRun: 3}, 0)
	processed := 0
	for i := 0; i < 100; i++ {
		if p.capReached() {
			break
		}
		if err := p.wait(context.Background()); err != nil {
			t.Fatalf("wait: %v", err)
		}
		processed++
		p.count()
	}
	if processed != 3 {
		t.Fatalf("want exactly 3 items processed before the cap, got %d", processed)
	}
	if !p.capHit() {
		t.Fatal("want capHit() true after stopping at the cap")
	}
	if p.capNote() == "" {
		t.Fatal("capNote should be a non-empty admin-facing message")
	}
}

// TestImportPacerWaitHonorsCancel guards that a long inter-request delay
// never blocks a cancelled/aborted import.
func TestImportPacerWaitHonorsCancel(t *testing.T) {
	p := newImportPacer(importConfig{RequestDelayMS: 60000}, 0) // 60s
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	start := time.Now()
	err := p.wait(ctx)
	if err == nil {
		t.Fatal("want the cancelled context's error, got nil")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("wait should return immediately on a cancelled ctx, took %v", time.Since(start))
	}
}

func TestItoa(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{0, "0"}, {7, "7"}, {500, "500"}, {-42, "-42"}} {
		if got := itoa(c.n); got != c.want {
			t.Errorf("itoa(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
