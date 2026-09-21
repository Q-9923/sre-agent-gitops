package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestConfigMapRemediationStateSurvivesAdapterRestart(t *testing.T) {
	const (
		namespace  = "sre-agent-system"
		configName = "sre-agent-v2-state"
		incidentID = "inc-persistent-state-1"
		targetKey  = "dev/sre-agent-lab/ReplicaSet/crash-app-rs"
	)

	ctx := context.Background()
	recordedAt := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)

	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configName,
				Namespace: namespace,
			},
			Data: map[string]string{
				"state.json": "{\"schemaVersion\":\"v1\",\"handledAt\":{},\"lastActionAt\":{},\"attemptsByScope\":{}}",
			},
		},
	)

	first := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)

	err := first.Record(
		ctx,
		remediationStateRecord{
			Kind:       "ACTION_ATTEMPTED",
			IncidentID: incidentID,
			TargetKey:  targetKey,
			OccurredAt: recordedAt,
		},
	)
	if err != nil {
		t.Fatalf("first.Record() error = %v; want nil", err)
	}

	second := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)

	snapshot, err := second.Snapshot(
		ctx,
		remediationStateQuery{
			IncidentID:    incidentID,
			TargetKey:     targetKey,
			Now:           recordedAt.Add(time.Minute),
			Cooldown:      10 * time.Minute,
			AttemptWindow: time.Hour,
		},
	)
	if err != nil {
		t.Fatalf("second.Snapshot() error = %v; want nil", err)
	}

	if !snapshot.Duplicate {
		t.Fatal("snapshot.Duplicate = false; want true after adapter restart")
	}
	if !snapshot.InCooldown {
		t.Fatal("snapshot.InCooldown = false; want true after adapter restart")
	}
	if snapshot.AttemptsInWindow != 1 {
		t.Fatalf(
			"snapshot.AttemptsInWindow = %d; want 1 after adapter restart",
			snapshot.AttemptsInWindow,
		)
	}
}
func TestConfigMapRemediationStatePrunesExpiredEntriesAndPersistsState(
	t *testing.T,
) {
	const (
		namespace          = "sre-agent-system"
		configName         = "sre-agent-v2-state"
		expiredIncidentID  = "inc-expired"
		retainedIncidentID = "inc-retained"
		expiredTargetKey   = "dev/sre-agent-lab/ReplicaSet/expired"
		retainedTargetKey  = "dev/sre-agent-lab/ReplicaSet/retained"
	)

	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-2 * time.Hour)
	retainedAt := now.Add(-30 * time.Minute)
	recentActionAt := now.Add(-5 * time.Minute)

	initialState := persistedRemediationState{
		SchemaVersion: remediationStateSchemaVersion,
		HandledAt: map[string]time.Time{
			expiredIncidentID:  expiredAt,
			retainedIncidentID: retainedAt,
		},
		LastActionAt: map[string]time.Time{
			expiredTargetKey:  expiredAt,
			retainedTargetKey: recentActionAt,
		},
		AttemptsByScope: map[string][]time.Time{
			expiredTargetKey: {
				expiredAt,
			},
			retainedTargetKey: {
				expiredAt,
				retainedAt,
			},
		},
	}
	encodedState, err := json.Marshal(initialState)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v; want nil", err)
	}

	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configName,
				Namespace: namespace,
			},
			Data: map[string]string{
				remediationStateDataKey: string(encodedState),
			},
		},
	)
	stateStore := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)

	snapshot, err := stateStore.Snapshot(
		ctx,
		remediationStateQuery{
			IncidentID:    expiredIncidentID,
			TargetKey:     expiredTargetKey,
			Now:           now,
			Cooldown:      10 * time.Minute,
			AttemptWindow: time.Hour,
		},
	)
	if err != nil {
		t.Fatalf("Snapshot() error = %v; want nil", err)
	}
	if snapshot.Duplicate {
		t.Fatal("snapshot.Duplicate = true; want false for expired incident")
	}
	if snapshot.InCooldown {
		t.Fatal("snapshot.InCooldown = true; want false for expired action")
	}
	if snapshot.AttemptsInWindow != 0 {
		t.Fatalf(
			"snapshot.AttemptsInWindow = %d; want 0 for expired attempts",
			snapshot.AttemptsInWindow,
		)
	}

	configMap, err := client.
		CoreV1().
		ConfigMaps(namespace).
		Get(ctx, configName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get persisted ConfigMap error = %v; want nil", err)
	}

	var persisted persistedRemediationState
	if err := json.Unmarshal(
		[]byte(configMap.Data[remediationStateDataKey]),
		&persisted,
	); err != nil {
		t.Fatalf("decode persisted state error = %v; want nil", err)
	}

	if _, exists := persisted.HandledAt[expiredIncidentID]; exists {
		t.Fatalf(
			"expired incident %q is still persisted",
			expiredIncidentID,
		)
	}
	if handledAt, exists := persisted.HandledAt[retainedIncidentID]; !exists ||
		!handledAt.Equal(retainedAt) {
		t.Fatalf(
			"retained incident = %v, %t; want %v, true",
			handledAt,
			exists,
			retainedAt,
		)
	}
	if _, exists := persisted.LastActionAt[expiredTargetKey]; exists {
		t.Fatalf(
			"expired action target %q is still persisted",
			expiredTargetKey,
		)
	}
	if actionAt, exists := persisted.LastActionAt[retainedTargetKey]; !exists ||
		!actionAt.Equal(recentActionAt) {
		t.Fatalf(
			"retained action = %v, %t; want %v, true",
			actionAt,
			exists,
			recentActionAt,
		)
	}
	if _, exists := persisted.AttemptsByScope[expiredTargetKey]; exists {
		t.Fatalf(
			"expired attempt target %q is still persisted",
			expiredTargetKey,
		)
	}
	retainedAttempts := persisted.AttemptsByScope[retainedTargetKey]
	if len(retainedAttempts) != 1 ||
		!retainedAttempts[0].Equal(retainedAt) {
		t.Fatalf(
			"retained attempts = %v; want [%v]",
			retainedAttempts,
			retainedAt,
		)
	}
}

func TestConfigMapRemediationStateRejectsInvalidPersistedState(
	t *testing.T,
) {
	const (
		namespace  = "sre-agent-system"
		configName = "sre-agent-v2-state"
	)

	testCases := []struct {
		name              string
		data              map[string]string
		expectedErrorText string
	}{
		{
			name:              "missing state data",
			data:              map[string]string{},
			expectedErrorText: `has no "state.json" key`,
		},
		{
			name: "malformed JSON",
			data: map[string]string{
				remediationStateDataKey: "{",
			},
			expectedErrorText: "decode remediation state ConfigMap",
		},
		{
			name: "unknown schema version",
			data: map[string]string{
				remediationStateDataKey: `{
					"schemaVersion":"v2",
					"handledAt":{},
					"lastActionAt":{},
					"attemptsByScope":{}
				}`,
			},
			expectedErrorText: `unsupported remediation state schema version "v2"`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      configName,
						Namespace: namespace,
					},
					Data: testCase.data,
				},
			)
			stateStore := newConfigMapRemediationState(
				client,
				namespace,
				configName,
			)

			snapshot, err := stateStore.Snapshot(
				context.Background(),
				remediationStateQuery{
					IncidentID:    "inc-invalid-state",
					TargetKey:     "dev/sre-agent-lab/ReplicaSet/test",
					Now:           time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
					Cooldown:      10 * time.Minute,
					AttemptWindow: time.Hour,
				},
			)
			if err == nil {
				t.Fatalf(
					"Snapshot() = %#v, nil; want an error",
					snapshot,
				)
			}
			if !strings.Contains(err.Error(), testCase.expectedErrorText) {
				t.Fatalf(
					"Snapshot() error = %q; want text %q",
					err,
					testCase.expectedErrorText,
				)
			}
		})
	}
}

func TestConfigMapRemediationStateRetriesUpdateConflict(t *testing.T) {
	const (
		namespace  = "sre-agent-system"
		configName = "sre-agent-v2-state"
		incidentID = "inc-conflict-retry"
		targetKey  = "dev/sre-agent-lab/ReplicaSet/conflict-retry"
	)

	ctx := context.Background()
	recordedAt := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configName,
				Namespace: namespace,
			},
			Data: map[string]string{
				remediationStateDataKey: `{
					"schemaVersion":"v1",
					"handledAt":{},
					"lastActionAt":{},
					"attemptsByScope":{}
				}`,
			},
		},
	)

	updateAttempts := 0
	client.PrependReactor(
		"update",
		"configmaps",
		func(
			k8stesting.Action,
		) (bool, runtime.Object, error) {
			updateAttempts++
			if updateAttempts == 1 {
				return true, nil, apierrors.NewConflict(
					schema.GroupResource{
						Resource: "configmaps",
					},
					configName,
					errors.New("simulated concurrent update"),
				)
			}
			return false, nil, nil
		},
	)

	stateStore := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)
	err := stateStore.Record(
		ctx,
		remediationStateRecord{
			Kind:       remediationStateRecordActionAttempted,
			IncidentID: incidentID,
			TargetKey:  targetKey,
			OccurredAt: recordedAt,
		},
	)
	if err != nil {
		t.Fatalf("Record() error = %v; want nil after conflict retry", err)
	}
	if updateAttempts != 2 {
		t.Fatalf(
			"ConfigMap update attempts = %d; want 2",
			updateAttempts,
		)
	}

	restarted := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)
	snapshot, err := restarted.Snapshot(
		ctx,
		remediationStateQuery{
			IncidentID:    incidentID,
			TargetKey:     targetKey,
			Now:           recordedAt.Add(time.Minute),
			Cooldown:      10 * time.Minute,
			AttemptWindow: time.Hour,
		},
	)
	if err != nil {
		t.Fatalf("Snapshot() error = %v; want nil", err)
	}
	if !snapshot.Duplicate ||
		!snapshot.InCooldown ||
		snapshot.AttemptsInWindow != 1 {
		t.Fatalf(
			"snapshot = %#v; want duplicate, cooldown, and one attempt",
			snapshot,
		)
	}
}
func TestConfigMapRemediationStateRecordsHandledIncidentWithoutActionAttempt(
	t *testing.T,
) {
	const (
		namespace  = "sre-agent-system"
		configName = "sre-agent-v2-state"
		incidentID = "inc-handled-without-action"
		targetKey  = "dev/sre-agent-lab/ReplicaSet/handled-without-action"
	)

	ctx := context.Background()
	handledAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configName,
				Namespace: namespace,
			},
			Data: map[string]string{
				remediationStateDataKey: `{
					"schemaVersion":"v1",
					"handledAt":{},
					"lastActionAt":{},
					"attemptsByScope":{}
				}`,
			},
		},
	)

	first := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)
	err := first.Record(
		ctx,
		remediationStateRecord{
			Kind:       remediationStateRecordIncidentHandled,
			IncidentID: incidentID,
			OccurredAt: handledAt,
		},
	)
	if err != nil {
		t.Fatalf(
			"Record(INCIDENT_HANDLED) error = %v; want nil",
			err,
		)
	}

	restarted := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)
	snapshot, err := restarted.Snapshot(
		ctx,
		remediationStateQuery{
			IncidentID:    incidentID,
			TargetKey:     targetKey,
			Now:           handledAt.Add(time.Minute),
			Cooldown:      10 * time.Minute,
			AttemptWindow: time.Hour,
		},
	)
	if err != nil {
		t.Fatalf("Snapshot() error = %v; want nil", err)
	}
	if !snapshot.Duplicate {
		t.Fatal(
			"snapshot.Duplicate = false; want true for handled incident",
		)
	}
	if snapshot.InCooldown {
		t.Fatal(
			"snapshot.InCooldown = true; want false without action attempt",
		)
	}
	if snapshot.AttemptsInWindow != 0 {
		t.Fatalf(
			"snapshot.AttemptsInWindow = %d; want 0 without action attempt",
			snapshot.AttemptsInWindow,
		)
	}
}
func TestConfigMapRemediationStateRetainsCooldownBeyondAttemptWindow(
	t *testing.T,
) {
	const (
		namespace  = "sre-agent-system"
		configName = "sre-agent-v2-state"
		targetKey  = "dev/sre-agent-lab/ReplicaSet/long-cooldown"
	)

	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	actionAt := now.Add(-30 * time.Minute)

	initialState := persistedRemediationState{
		SchemaVersion: remediationStateSchemaVersion,
		HandledAt:     map[string]time.Time{},
		LastActionAt: map[string]time.Time{
			targetKey: actionAt,
		},
		AttemptsByScope: map[string][]time.Time{},
	}
	encodedState, err := json.Marshal(initialState)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: namespace,
		},
		Data: map[string]string{
			remediationStateDataKey: string(encodedState),
		},
	})

	stateStore := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)

	snapshot, err := stateStore.Snapshot(
		context.Background(),
		remediationStateQuery{
			IncidentID:    "inc-long-cooldown",
			TargetKey:     targetKey,
			Now:           now,
			Cooldown:      time.Hour,
			AttemptWindow: 10 * time.Minute,
		},
	)
	if err != nil {
		t.Fatalf("Snapshot() error = %v; want nil", err)
	}
	if !snapshot.InCooldown {
		t.Fatal(
			"snapshot.InCooldown = false; want true while action remains inside cooldown",
		)
	}
	if snapshot.AttemptsInWindow != 0 {
		t.Fatalf(
			"snapshot.AttemptsInWindow = %d; want 0 outside attempt window",
			snapshot.AttemptsInWindow,
		)
	}
}
func TestConfigMapRemediationStateRetriesPruneConflict(t *testing.T) {
	const (
		namespace  = "sre-agent-system"
		configName = "sre-agent-v2-state"
		incidentID = "inc-expired-prune-conflict"
		targetKey  = "dev/sre-agent-lab/ReplicaSet/prune-conflict"
	)

	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-2 * time.Hour)

	initialState := persistedRemediationState{
		SchemaVersion: remediationStateSchemaVersion,
		HandledAt: map[string]time.Time{
			incidentID: expiredAt,
		},
		LastActionAt: map[string]time.Time{},
		AttemptsByScope: map[string][]time.Time{
			targetKey: {expiredAt},
		},
	}
	encodedState, err := json.Marshal(initialState)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName,
			Namespace: namespace,
		},
		Data: map[string]string{
			remediationStateDataKey: string(encodedState),
		},
	})

	updateAttempts := 0
	client.PrependReactor(
		"update",
		"configmaps",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			updateAttempts++
			if updateAttempts == 1 {
				return true, nil, apierrors.NewConflict(
					schema.GroupResource{
						Resource: "configmaps",
					},
					configName,
					errors.New("simulated prune conflict"),
				)
			}
			return false, nil, nil
		},
	)

	stateStore := newConfigMapRemediationState(
		client,
		namespace,
		configName,
	)

	snapshot, err := stateStore.Snapshot(
		context.Background(),
		remediationStateQuery{
			IncidentID:    incidentID,
			TargetKey:     targetKey,
			Now:           now,
			Cooldown:      10 * time.Minute,
			AttemptWindow: time.Hour,
		},
	)
	if err != nil {
		t.Fatalf(
			"Snapshot() error = %v; want nil after conflict retry",
			err,
		)
	}
	if updateAttempts != 2 {
		t.Fatalf(
			"ConfigMap update attempts = %d; want 2",
			updateAttempts,
		)
	}
	if snapshot.Duplicate {
		t.Fatal(
			"snapshot.Duplicate = true; want false after expired state pruning",
		)
	}
	if snapshot.AttemptsInWindow != 0 {
		t.Fatalf(
			"snapshot.AttemptsInWindow = %d; want 0",
			snapshot.AttemptsInWindow,
		)
	}
}
