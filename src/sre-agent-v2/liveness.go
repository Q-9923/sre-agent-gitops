package main

import (
	"sync"
	"time"
)

type livenessState struct {
	mutex        sync.RWMutex
	lastProgress time.Time
	staleAfter   time.Duration
	now          func() time.Time
}

func newLivenessState(
	staleAfter time.Duration,
	now func() time.Time,
) *livenessState {
	return &livenessState{
		lastProgress: now(),
		staleAfter:   staleAfter,
		now:          now,
	}
}

func (state *livenessState) markProgress() {
	progressedAt := state.now()

	state.mutex.Lock()
	state.lastProgress = progressedAt
	state.mutex.Unlock()
}

func (state *livenessState) isLive() bool {
	state.mutex.RLock()
	lastProgress := state.lastProgress
	state.mutex.RUnlock()

	return state.now().Sub(lastProgress) <= state.staleAfter
}
