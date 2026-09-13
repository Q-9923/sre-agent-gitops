package main

import (
	"testing"
	"time"
)

func TestLivenessStateTracksCycleProgressAndStaleness(t *testing.T) {
	currentTime := time.Date(
		2026,
		time.September,
		13,
		18,
		0,
		0,
		0,
		time.UTC,
	)

	state := newLivenessState(
		time.Minute,
		func() time.Time {
			return currentTime
		},
	)

	if !state.isLive() {
		t.Fatal("initial liveness = false; want true")
	}

	currentTime = currentTime.Add(time.Minute + time.Nanosecond)

	if state.isLive() {
		t.Fatal("stale liveness = true; want false")
	}

	state.markProgress()

	if !state.isLive() {
		t.Fatal("liveness after progress = false; want true")
	}
}
