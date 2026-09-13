package main

import (
	"testing"
	"time"
)

func TestReadinessStateTracksSuccessfulCycleAndStaleness(t *testing.T) {
	currentTime := time.Date(
		2026,
		time.September,
		9,
		12,
		0,
		0,
		0,
		time.UTC,
	)

	state := newReadinessState(
		time.Minute,
		func() time.Time {
			return currentTime
		},
	)

	if state.isReady() {
		t.Fatal("initial readiness = true; want false")
	}

	state.markSuccessfulCycle()

	if !state.isReady() {
		t.Fatal("readiness after successful cycle = false; want true")
	}

	currentTime = currentTime.Add(time.Minute + time.Nanosecond)

	if state.isReady() {
		t.Fatal("stale readiness = true; want false")
	}
}
