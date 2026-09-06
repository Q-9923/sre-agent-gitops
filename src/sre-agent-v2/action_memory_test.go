package main

import (
	"testing"
	"time"
)

func TestRemediationMemoryTracksDuplicateCooldownAndWindowedAttempts(t *testing.T) {
	memory := newRemediationMemory()
	startedAt := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	const (
		incidentID = "inc-memory-1"
		targetKey  = "dev/default/ReplicaSet/crash-app-rs"
	)

	initial := memory.snapshot(incidentID, targetKey, startedAt, 10*time.Minute, time.Hour)
	if initial.Duplicate || initial.InCooldown || initial.AttemptsInWindow != 0 {
		t.Fatalf("initial snapshot = %#v; want empty state", initial)
	}

	memory.record(incidentID, targetKey, startedAt)
	soon := memory.snapshot(incidentID, targetKey, startedAt.Add(time.Minute), 10*time.Minute, time.Hour)
	if !soon.Duplicate || !soon.InCooldown || soon.AttemptsInWindow != 1 {
		t.Fatalf("snapshot after record = %#v; want duplicate, cooldown, and one attempt", soon)
	}

	newIncident := memory.snapshot("inc-memory-2", targetKey, startedAt.Add(11*time.Minute), 10*time.Minute, time.Hour)
	if newIncident.Duplicate || newIncident.InCooldown || newIncident.AttemptsInWindow != 1 {
		t.Fatalf("snapshot after cooldown = %#v; want one prior attempt only", newIncident)
	}

	afterWindow := memory.snapshot("inc-memory-3", targetKey, startedAt.Add(61*time.Minute), 10*time.Minute, time.Hour)
	if afterWindow.AttemptsInWindow != 0 {
		t.Fatalf("AttemptsInWindow = %d; want expired attempts to be removed", afterWindow.AttemptsInWindow)
	}
}

func TestRemediationTargetKeyUsesControllerIdentityAcrossReplacementPods(t *testing.T) {
	first := PodEvidence{
		Target: DecisionTarget{Cluster: "dev", Namespace: "default", Kind: "Pod", Name: "crash-app-a", UID: "pod-a"},
		Owner:  OwnerEvidence{Kind: "ReplicaSet", Name: "crash-app-rs", UID: "rs-uid"},
	}
	replacement := first
	replacement.Target.Name = "crash-app-b"
	replacement.Target.UID = "pod-b"

	if remediationTargetKey(first) != remediationTargetKey(replacement) {
		t.Fatalf("replacement pods owned by one controller must share a remediation key")
	}
}
