package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"raydio/internal/catalog"
	"raydio/internal/radio"
	"raydio/internal/store"
)

func TestValidateConfigRejectsNonPositiveScheduleInterval(t *testing.T) {
	cfg := config{
		RescanInterval:   time.Second,
		ScheduleInterval: 0,
		GapFrames:        1,
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected non-positive schedule interval to fail validation")
	}
}

func TestScheduleLoopMaintainsFutureSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := &app{
		cfg:       config{ScheduleInterval: 5 * time.Millisecond},
		scheduler: radio.NewScheduler(st, "/cache/silence.mp3", 5),
	}
	go a.scheduleLoop(ctx)

	waitFor(t, 500*time.Millisecond, func() bool {
		slots, err := st.SlotsEndingAfter(ctx, time.Now().UnixMilli())
		return err == nil && len(slots) > 0
	})
}

func TestHandleScanResultRefillsFutureSchedule(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "raydio.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := &app{scheduler: radio.NewScheduler(st, "/cache/silence.mp3", 5)}
	a.handleScanResult(ctx, catalog.ScanResult{Changed: true})

	slots, err := st.SlotsEndingAfter(ctx, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) == 0 {
		t.Fatal("expected scan change handling to refill future schedule")
	}
}

func waitFor(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
