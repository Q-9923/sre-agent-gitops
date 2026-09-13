package main

import (
	"sync"
	"time"
)

type readinessState struct {
	mutex               sync.RWMutex
	lastSuccessfulCycle time.Time
	staleAfter          time.Duration
	now                 func() time.Time
}

func newReadinessState(
	staleAfter time.Duration,
	now func() time.Time,
) *readinessState {
	return &readinessState{
		staleAfter: staleAfter,
		now:        now,
	}
}

func (state *readinessState) markSuccessfulCycle() {
	completedAt := state.now()

	state.mutex.Lock()
	state.lastSuccessfulCycle = completedAt
	state.mutex.Unlock()
}

func (state *readinessState) isReady() bool {
	state.mutex.RLock()
	lastSuccessfulCycle := state.lastSuccessfulCycle
	state.mutex.RUnlock()

	if lastSuccessfulCycle.IsZero() {
		return false
	}

	return state.now().Sub(lastSuccessfulCycle) <= state.staleAfter
}
