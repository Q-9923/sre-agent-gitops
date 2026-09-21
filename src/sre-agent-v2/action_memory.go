package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type remediationSnapshot struct {
	Duplicate        bool
	InCooldown       bool
	AttemptsInWindow int
}

// remediationMemory provides process-local duplicate suppression, cooldown,
// and rate accounting. Durable coordination is a later deployment concern.
type remediationMemory struct {
	mu              sync.Mutex
	handledAt       map[string]time.Time
	lastActionAt    map[string]time.Time
	attemptsByScope map[string][]time.Time
}

func newRemediationMemory() *remediationMemory {
	return &remediationMemory{
		handledAt:       make(map[string]time.Time),
		lastActionAt:    make(map[string]time.Time),
		attemptsByScope: make(map[string][]time.Time),
	}
}

func (memory *remediationMemory) Snapshot(
	_ context.Context,
	query remediationStateQuery,
) (remediationSnapshot, error) {
	return memory.snapshot(
		query.IncidentID,
		query.TargetKey,
		query.Now,
		query.Cooldown,
		query.AttemptWindow,
	), nil
}

func (memory *remediationMemory) Record(
	_ context.Context,
	record remediationStateRecord,
) error {
	switch record.Kind {
	case remediationStateRecordIncidentHandled:
		memory.markIncident(record.IncidentID, record.OccurredAt)
	case remediationStateRecordActionAttempted:
		memory.record(record.IncidentID, record.TargetKey, record.OccurredAt)
	default:
		return fmt.Errorf(
			"unsupported remediation state record kind %q",
			record.Kind,
		)
	}

	return nil
}

func (memory *remediationMemory) snapshot(
	incidentID, targetKey string,
	now time.Time,
	cooldown, attemptWindow time.Duration,
) remediationSnapshot {
	memory.mu.Lock()
	defer memory.mu.Unlock()

	memory.prune(now, cooldown, attemptWindow)
	_, duplicate := memory.handledAt[incidentID]
	lastAction, hasAction := memory.lastActionAt[targetKey]
	inCooldown := hasAction && cooldown > 0 && now.Sub(lastAction) < cooldown

	return remediationSnapshot{
		Duplicate:        duplicate,
		InCooldown:       inCooldown,
		AttemptsInWindow: len(memory.attemptsByScope[targetKey]),
	}
}

func (memory *remediationMemory) record(incidentID, targetKey string, now time.Time) {
	memory.mu.Lock()
	defer memory.mu.Unlock()

	memory.handledAt[incidentID] = now
	memory.lastActionAt[targetKey] = now
	memory.attemptsByScope[targetKey] = append(memory.attemptsByScope[targetKey], now)
}

func (memory *remediationMemory) markIncident(incidentID string, now time.Time) {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.handledAt[incidentID] = now
}

func (memory *remediationMemory) recordAction(targetKey string, now time.Time) {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.lastActionAt[targetKey] = now
	memory.attemptsByScope[targetKey] = append(memory.attemptsByScope[targetKey], now)
}

func (memory *remediationMemory) prune(
	now time.Time,
	cooldown, attemptWindow time.Duration,
) {
	if attemptWindow > 0 {
		attemptCutoff := now.Add(-attemptWindow)

		for incidentID, handledAt := range memory.handledAt {
			if handledAt.Before(attemptCutoff) {
				delete(memory.handledAt, incidentID)
			}
		}

		for targetKey, attempts := range memory.attemptsByScope {
			retainedAttempts := attempts[:0]
			for _, attemptedAt := range attempts {
				if !attemptedAt.Before(attemptCutoff) {
					retainedAttempts = append(
						retainedAttempts,
						attemptedAt,
					)
				}
			}

			if len(retainedAttempts) == 0 {
				delete(memory.attemptsByScope, targetKey)
				continue
			}

			memory.attemptsByScope[targetKey] = retainedAttempts
		}
	}

	if cooldown > 0 {
		cooldownCutoff := now.Add(-cooldown)

		for targetKey, actionAt := range memory.lastActionAt {
			if actionAt.Before(cooldownCutoff) {
				delete(memory.lastActionAt, targetKey)
			}
		}
	}
}
func remediationTargetKey(pod PodEvidence) string {
	if pod.Owner.UID != "" {
		return strings.Join([]string{
			pod.Target.Cluster,
			pod.Target.Namespace,
			pod.Owner.Kind,
			pod.Owner.UID,
		}, "/")
	}
	return strings.Join([]string{
		pod.Target.Cluster,
		pod.Target.Namespace,
		pod.Target.Kind,
		pod.Target.UID,
	}, "/")
}
