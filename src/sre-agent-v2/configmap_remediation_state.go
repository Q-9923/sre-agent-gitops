package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	remediationStateDataKey               = "state.json"
	remediationStateSchemaVersion         = "v1"
	remediationStateRecordIncidentHandled = "INCIDENT_HANDLED"
	remediationStateRecordActionAttempted = "ACTION_ATTEMPTED"
)

type remediationStateQuery struct {
	IncidentID    string
	TargetKey     string
	Now           time.Time
	Cooldown      time.Duration
	AttemptWindow time.Duration
}

type remediationStateRecord struct {
	Kind       string
	IncidentID string
	TargetKey  string
	OccurredAt time.Time
}

type remediationStateStore interface {
	Snapshot(context.Context, remediationStateQuery) (remediationSnapshot, error)
	Record(context.Context, remediationStateRecord) error
}

type configMapRemediationState struct {
	client     kubernetes.Interface
	namespace  string
	configName string
}

type persistedRemediationState struct {
	SchemaVersion   string                 `json:"schemaVersion"`
	HandledAt       map[string]time.Time   `json:"handledAt"`
	LastActionAt    map[string]time.Time   `json:"lastActionAt"`
	AttemptsByScope map[string][]time.Time `json:"attemptsByScope"`
}

func newConfigMapRemediationState(
	client kubernetes.Interface,
	namespace, configName string,
) *configMapRemediationState {
	return &configMapRemediationState{
		client:     client,
		namespace:  namespace,
		configName: configName,
	}
}

func (stateStore *configMapRemediationState) Record(
	ctx context.Context,
	record remediationStateRecord,
) error {
	switch record.Kind {
	case remediationStateRecordIncidentHandled:
		if record.IncidentID == "" ||
			record.OccurredAt.IsZero() {
			return fmt.Errorf(
				"handled incident record requires incident and occurrence time",
			)
		}

	case remediationStateRecordActionAttempted:
		if record.IncidentID == "" ||
			record.TargetKey == "" ||
			record.OccurredAt.IsZero() {
			return fmt.Errorf(
				"action attempt record requires incident, target, and occurrence time",
			)
		}

	default:
		return fmt.Errorf(
			"unsupported remediation state record kind %q",
			record.Kind,
		)
	}
	err := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			configMap, state, err := stateStore.load(ctx)
			if err != nil {
				return err
			}

			state.HandledAt[record.IncidentID] = record.OccurredAt
			if record.Kind == remediationStateRecordActionAttempted {
				state.LastActionAt[record.TargetKey] = record.OccurredAt
				state.AttemptsByScope[record.TargetKey] = append(
					state.AttemptsByScope[record.TargetKey],
					record.OccurredAt,
				)
			}
			encoded, err := json.Marshal(state)
			if err != nil {
				return fmt.Errorf(
					"encode remediation state: %w",
					err,
				)
			}
			configMap.Data[remediationStateDataKey] = string(encoded)

			_, err = stateStore.client.
				CoreV1().
				ConfigMaps(stateStore.namespace).
				Update(
					ctx,
					configMap,
					metav1.UpdateOptions{},
				)

			// 返回 Kubernetes 原始错误，RetryOnConflict 才能识别 409。
			return err
		},
	)
	if err != nil {
		return fmt.Errorf(
			"record remediation state ConfigMap %s/%s: %w",
			stateStore.namespace,
			stateStore.configName,
			err,
		)
	}

	return nil
}

func (stateStore *configMapRemediationState) Snapshot(
	ctx context.Context,
	query remediationStateQuery,
) (remediationSnapshot, error) {
	var snapshot remediationSnapshot

	err := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			configMap, state, err := stateStore.load(ctx)
			if err != nil {
				return err
			}

			if prunePersistedRemediationState(
				&state,
				query.Now,
				query.Cooldown,
				query.AttemptWindow,
			) {
				encoded, err := json.Marshal(state)
				if err != nil {
					return fmt.Errorf(
						"encode pruned remediation state: %w",
						err,
					)
				}
				configMap.Data[remediationStateDataKey] = string(encoded)

				_, err = stateStore.client.
					CoreV1().
					ConfigMaps(stateStore.namespace).
					Update(
						ctx,
						configMap,
						metav1.UpdateOptions{},
					)
				if err != nil {
					// 返回原始 Kubernetes 错误，使 RetryOnConflict
					// 能识别 409 并重新读取最新 ConfigMap。
					return err
				}
			}

			handledAt, handled := state.HandledAt[query.IncidentID]
			duplicate := handled && retainedAt(
				handledAt,
				query.Now,
				query.AttemptWindow,
			)

			lastActionAt, hasAction :=
				state.LastActionAt[query.TargetKey]
			inCooldown := hasAction &&
				query.Cooldown > 0 &&
				query.Now.Sub(lastActionAt) < query.Cooldown

			attemptsInWindow := 0
			for _, attemptedAt := range state.AttemptsByScope[query.TargetKey] {
				if retainedAt(
					attemptedAt,
					query.Now,
					query.AttemptWindow,
				) {
					attemptsInWindow++
				}
			}

			snapshot = remediationSnapshot{
				Duplicate:        duplicate,
				InCooldown:       inCooldown,
				AttemptsInWindow: attemptsInWindow,
			}
			return nil
		},
	)
	if err != nil {
		return remediationSnapshot{}, fmt.Errorf(
			"snapshot remediation state ConfigMap %s/%s: %w",
			stateStore.namespace,
			stateStore.configName,
			err,
		)
	}

	return snapshot, nil
}

func (stateStore *configMapRemediationState) load(
	ctx context.Context,
) (*corev1.ConfigMap, persistedRemediationState, error) {
	configMap, err := stateStore.client.
		CoreV1().
		ConfigMaps(stateStore.namespace).
		Get(ctx, stateStore.configName, metav1.GetOptions{})
	if err != nil {
		return nil, persistedRemediationState{}, fmt.Errorf(
			"get remediation state ConfigMap %s/%s: %w",
			stateStore.namespace,
			stateStore.configName,
			err,
		)
	}

	raw, ok := configMap.Data[remediationStateDataKey]
	if !ok {
		return nil, persistedRemediationState{}, fmt.Errorf(
			"remediation state ConfigMap %s/%s has no %q key",
			stateStore.namespace,
			stateStore.configName,
			remediationStateDataKey,
		)
	}

	var state persistedRemediationState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, persistedRemediationState{}, fmt.Errorf(
			"decode remediation state ConfigMap %s/%s: %w",
			stateStore.namespace,
			stateStore.configName,
			err,
		)
	}
	if state.SchemaVersion != remediationStateSchemaVersion {
		return nil, persistedRemediationState{}, fmt.Errorf(
			"unsupported remediation state schema version %q",
			state.SchemaVersion,
		)
	}

	if state.HandledAt == nil {
		state.HandledAt = make(map[string]time.Time)
	}
	if state.LastActionAt == nil {
		state.LastActionAt = make(map[string]time.Time)
	}
	if state.AttemptsByScope == nil {
		state.AttemptsByScope = make(map[string][]time.Time)
	}

	return configMap, state, nil
}

func retainedAt(
	observedAt, now time.Time,
	retention time.Duration,
) bool {
	if retention <= 0 {
		return true
	}
	return !observedAt.Before(now.Add(-retention))
}

func prunePersistedRemediationState(
	state *persistedRemediationState,
	now time.Time,
	cooldown, attemptWindow time.Duration,
) bool {
	changed := false

	if attemptWindow > 0 {
		attemptCutoff := now.Add(-attemptWindow)

		for incidentID, handledAt := range state.HandledAt {
			if handledAt.Before(attemptCutoff) {
				delete(state.HandledAt, incidentID)
				changed = true
			}
		}

		for targetKey, attempts := range state.AttemptsByScope {
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
				delete(state.AttemptsByScope, targetKey)
				changed = true
				continue
			}
			if len(retainedAttempts) != len(attempts) {
				state.AttemptsByScope[targetKey] = retainedAttempts
				changed = true
			}
		}
	}

	if cooldown > 0 {
		cooldownCutoff := now.Add(-cooldown)

		for targetKey, actionAt := range state.LastActionAt {
			if actionAt.Before(cooldownCutoff) {
				delete(state.LastActionAt, targetKey)
				changed = true
			}
		}
	}

	return changed
}
