//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryResolvePersistsRecoveredVerificationAtomicallyPostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"registry-resolution",
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "crash-app-registry-resolution",
			UID:       "pod-uid-registry-resolution",
		},
	}

	registry := NewRegistry(pool)

	current, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if created {
		t.Fatal(
			"Observe() created = true; " +
				"want Incident created by fixture",
		)
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      current.ID,
			ExpectedVersion: current.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incidentdomain.StateVerifying,
			Actor:           "sre-agent",
			ReasonCode:      "VERIFICATION_STARTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v",
			err,
		)
	}

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, actionCreated, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !actionCreated {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		5 * time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	verifications := NewVerificationStore(pool)

	pendingVerification, verificationCreated, err :=
		verifications.Begin(
			ctx,
			remediationdomain.BeginVerificationCommand{
				ActionAttempt: succeededAttempt,
				Subject: remediationdomain.VerificationSubject{
					Cluster:   observation.Cluster,
					Namespace: observation.Target.Namespace,
					Kind:      observation.Target.Kind,
					Name:      observation.Target.Name,
					UID:       observation.Target.UID,
				},
				StartedAt: actionFinishedAt.Add(time.Second),
			},
		)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !verificationCreated {
		t.Fatal("Verification Begin() created = false; want true")
	}

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       pendingVerification.ActionKey,
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   actionFinishedAt.Add(6 * time.Second),
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	resolutionAt := recoveredVerification.FinishedAt.Add(time.Second)
	claim := claimIncidentForResolutionPostgreSQL(
		t,
		ctx,
		registry,
		verifying,
		"agent-action-attempt",
		resolutionAt,
	)

	resolved, err := registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      verifying.ID,
			ExpectedVersion: claim.Incident.Version,
			VerificationID:  recoveredVerification.ID,
			HolderID:        claim.HolderID,
			Now:             resolutionAt,
			Actor:           "sre-agent",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v; want nil", err)
	}
	if resolved.State != incidentdomain.StateResolved {
		t.Fatalf(
			"resolved State = %q; want %q",
			resolved.State,
			incidentdomain.StateResolved,
		)
	}
	if resolved.Version != claim.Incident.Version+1 {
		t.Fatalf(
			"resolved Version = %d; want %d",
			resolved.Version,
			claim.Incident.Version+1,
		)
	}
	if resolved.ResolutionVerificationID != recoveredVerification.ID {
		t.Fatalf(
			"resolved ResolutionVerificationID = %q; want %q",
			resolved.ResolutionVerificationID,
			recoveredVerification.ID,
		)
	}

	var (
		persistedState          string
		persistedVersion        int64
		persistedVerificationID string
		resolvedAtPresent       bool
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    state,
    version,
    resolution_verification_id,
    resolved_at IS NOT NULL
FROM incidents
WHERE id = $1
`,
		verifying.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&persistedVerificationID,
		&resolvedAtPresent,
	)
	if err != nil {
		t.Fatalf("query persisted resolved Incident error = %v", err)
	}

	if persistedState != string(incidentdomain.StateResolved) {
		t.Fatalf(
			"persisted State = %q; want %q",
			persistedState,
			incidentdomain.StateResolved,
		)
	}
	if persistedVersion != int64(claim.Incident.Version+1) {
		t.Fatalf(
			"persisted Version = %d; want %d",
			persistedVersion,
			claim.Incident.Version+1,
		)
	}
	if persistedVerificationID != recoveredVerification.ID {
		t.Fatalf(
			"persisted resolution_verification_id = %q; want %q",
			persistedVerificationID,
			recoveredVerification.ID,
		)
	}
	if !resolvedAtPresent {
		t.Fatal("persisted resolved_at is NULL; want timestamp")
	}
}

func TestRegistryResolveRollsBackWhenAuditInsertFailsPostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"registry-resolution-audit-rollback",
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	incidentID := beginAttemptCommand.Key.IncidentID

	var expectedIncidentVersion int64

	err := pool.QueryRow(
		ctx,
		`
UPDATE incidents
SET
    state = 'VERIFYING',
    version = version + 1
WHERE id = $1
RETURNING version
`,
		incidentID,
	).Scan(&expectedIncidentVersion)
	if err != nil {
		t.Fatalf(
			"prepare VERIFYING Incident error = %v",
			err,
		)
	}

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Begin() error = %v",
			err,
		)
	}
	if !created {
		t.Fatal(
			"ActionAttempt Begin() created = false; want true",
		)
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Complete() error = %v",
			err,
		)
	}

	verificationStartedAt := actionFinishedAt.Add(time.Second)
	verifications := NewVerificationStore(pool)

	pendingVerification, verificationCreated, err :=
		verifications.Begin(
			ctx,
			remediationdomain.BeginVerificationCommand{
				ActionAttempt: succeededAttempt,
				Subject: remediationdomain.VerificationSubject{
					Cluster:   "dev",
					Namespace: "default",
					Kind:      "Pod",
					Name: "crash-app-" +
						"registry-resolution-audit-rollback",
					UID: beginAttemptCommand.
						Key.TargetUID,
				},
				StartedAt: verificationStartedAt,
			},
		)
	if err != nil {
		t.Fatalf(
			"Verification Begin() error = %v",
			err,
		)
	}
	if !verificationCreated {
		t.Fatal(
			"Verification Begin() created = false; want true",
		)
	}

	verificationFinishedAt := verificationStartedAt.Add(
		time.Second,
	)

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       beginAttemptCommand.Key.ActionKey(),
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   verificationFinishedAt,
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf(
			"Verification Complete() error = %v",
			err,
		)
	}

	_, err = pool.Exec(
		ctx,
		`
ALTER TABLE incident_transitions
ADD CONSTRAINT incident_transitions_reject_resolution_test
CHECK (to_state <> 'RESOLVED')
`,
	)
	if err != nil {
		t.Fatalf(
			"install failing audit constraint error = %v",
			err,
		)
	}

	registry := NewRegistry(pool)
	resolutionAt := verificationFinishedAt.Add(time.Second)
	claim := claimIncidentForResolutionPostgreSQL(
		t,
		ctx,
		registry,
		incidentdomain.Incident{
			ID:      incidentID,
			Version: uint64(expectedIncidentVersion),
		},
		"agent-action-attempt",
		resolutionAt,
	)

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      incidentID,
			ExpectedVersion: claim.Incident.Version,
			VerificationID:  recoveredVerification.ID,
			HolderID:        claim.HolderID,
			Now:             resolutionAt,
			Actor:           "integration-test",
			ReasonCode:      "WORKLOAD_RECOVERED",
		},
	)
	if err == nil {
		t.Fatal(
			"Registry Resolve() error = nil; want audit persistence failure",
		)
	}

	var (
		persistedState                 string
		persistedVersion               int64
		resolvedAtIsNull               bool
		resolutionVerificationIDIsNull bool
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    state,
    version,
    resolved_at IS NULL,
    resolution_verification_id IS NULL
FROM incidents
WHERE id = $1
`,
		incidentID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&resolvedAtIsNull,
		&resolutionVerificationIDIsNull,
	)
	if err != nil {
		t.Fatalf(
			"reload Incident after failed Resolve() error = %v",
			err,
		)
	}

	if persistedState != string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"persisted Incident state = %q; want %q",
			persistedState,
			incidentdomain.StateVerifying,
		)
	}
	if persistedVersion != int64(claim.Incident.Version) {
		t.Fatalf(
			"persisted Incident version = %d; want unchanged %d",
			persistedVersion,
			claim.Incident.Version,
		)
	}
	if !resolvedAtIsNull {
		t.Fatal(
			"persisted Incident resolved_at is not NULL after rollback",
		)
	}
	if !resolutionVerificationIDIsNull {
		t.Fatal(
			"persisted Incident resolution_verification_id is not NULL after rollback",
		)
	}

	var resolutionAuditCount int

	err = pool.QueryRow(
		ctx,
		`
SELECT count(*)
FROM incident_transitions
WHERE incident_id = $1
  AND to_state = 'RESOLVED'
`,
		incidentID,
	).Scan(&resolutionAuditCount)
	if err != nil {
		t.Fatalf(
			"count RESOLVED audit events error = %v",
			err,
		)
	}
	if resolutionAuditCount != 0 {
		t.Fatalf(
			"RESOLVED audit event count = %d; want 0",
			resolutionAuditCount,
		)
	}
}

func TestRegistryResolveRejectsPendingVerificationPostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"registry-resolution-pending-verification",
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	incidentID := beginAttemptCommand.Key.IncidentID

	var expectedIncidentVersion int64

	err := pool.QueryRow(
		ctx,
		`
UPDATE incidents
SET
    state = 'VERIFYING',
    version = version + 1
WHERE id = $1
RETURNING version
`,
		incidentID,
	).Scan(&expectedIncidentVersion)
	if err != nil {
		t.Fatalf(
			"prepare VERIFYING Incident error = %v",
			err,
		)
	}

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Begin() error = %v",
			err,
		)
	}
	if !created {
		t.Fatal(
			"ActionAttempt Begin() created = false; want true",
		)
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Complete() error = %v",
			err,
		)
	}

	verifications := NewVerificationStore(pool)

	pendingVerification, verificationCreated, err :=
		verifications.Begin(
			ctx,
			remediationdomain.BeginVerificationCommand{
				ActionAttempt: succeededAttempt,
				Subject: remediationdomain.VerificationSubject{
					Cluster:   "dev",
					Namespace: "default",
					Kind:      "Pod",
					Name: "crash-app-" +
						"registry-resolution-pending-verification",
					UID: beginAttemptCommand.Key.TargetUID,
				},
				StartedAt: actionFinishedAt.Add(time.Second),
			},
		)
	if err != nil {
		t.Fatalf(
			"Verification Begin() error = %v",
			err,
		)
	}
	if !verificationCreated {
		t.Fatal(
			"Verification Begin() created = false; want true",
		)
	}
	if pendingVerification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification status = %q; want %q",
			pendingVerification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	registry := NewRegistry(pool)
	resolutionAt := actionFinishedAt.Add(2 * time.Second)
	claim := claimIncidentForResolutionPostgreSQL(
		t,
		ctx,
		registry,
		incidentdomain.Incident{
			ID:      incidentID,
			Version: uint64(expectedIncidentVersion),
		},
		"agent-action-attempt",
		resolutionAt,
	)

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      incidentID,
			ExpectedVersion: claim.Incident.Version,
			VerificationID:  pendingVerification.ID,
			HolderID:        claim.HolderID,
			Now:             resolutionAt,
			Actor:           "integration-test",
			ReasonCode:      "WORKLOAD_RECOVERED",
		},
	)
	if err == nil {
		t.Fatal(
			"Registry Resolve() error = nil; want pending Verification rejection",
		)
	}

	var (
		persistedState                 string
		persistedVersion               int64
		resolvedAtIsNull               bool
		resolutionVerificationIDIsNull bool
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    state,
    version,
    resolved_at IS NULL,
    resolution_verification_id IS NULL
FROM incidents
WHERE id = $1
`,
		incidentID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&resolvedAtIsNull,
		&resolutionVerificationIDIsNull,
	)
	if err != nil {
		t.Fatalf(
			"reload Incident after rejected Resolve() error = %v",
			err,
		)
	}

	if persistedState != string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"persisted Incident state = %q; want %q",
			persistedState,
			incidentdomain.StateVerifying,
		)
	}
	if persistedVersion != int64(claim.Incident.Version) {
		t.Fatalf(
			"persisted Incident version = %d; want unchanged %d",
			persistedVersion,
			claim.Incident.Version,
		)
	}
	if !resolvedAtIsNull {
		t.Fatal(
			"persisted Incident resolved_at is not NULL",
		)
	}
	if !resolutionVerificationIDIsNull {
		t.Fatal(
			"persisted Incident resolution_verification_id is not NULL",
		)
	}

	var resolutionAuditCount int

	err = pool.QueryRow(
		ctx,
		`
SELECT count(*)
FROM incident_transitions
WHERE incident_id = $1
  AND to_state = 'RESOLVED'
`,
		incidentID,
	).Scan(&resolutionAuditCount)
	if err != nil {
		t.Fatalf(
			"count RESOLVED audit events error = %v",
			err,
		)
	}
	if resolutionAuditCount != 0 {
		t.Fatalf(
			"RESOLVED audit event count = %d; want 0",
			resolutionAuditCount,
		)
	}
}

func TestRegistryResolveRejectsRecoveredVerificationFromAnotherIncidentPostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"registry-resolution-foreign-verification",
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Begin() error = %v",
			err,
		)
	}
	if !created {
		t.Fatal(
			"ActionAttempt Begin() created = false; want true",
		)
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Complete() error = %v",
			err,
		)
	}

	verifications := NewVerificationStore(pool)

	pendingVerification, verificationCreated, err :=
		verifications.Begin(
			ctx,
			remediationdomain.BeginVerificationCommand{
				ActionAttempt: succeededAttempt,
				Subject: remediationdomain.VerificationSubject{
					Cluster:   "dev",
					Namespace: "default",
					Kind:      "Pod",
					Name: "crash-app-" +
						"registry-resolution-foreign-verification",
					UID: beginAttemptCommand.Key.TargetUID,
				},
				StartedAt: actionFinishedAt.Add(time.Second),
			},
		)
	if err != nil {
		t.Fatalf(
			"Verification Begin() error = %v",
			err,
		)
	}
	if !verificationCreated {
		t.Fatal(
			"Verification Begin() created = false; want true",
		)
	}

	verificationFinishedAt := actionFinishedAt.Add(
		2 * time.Second,
	)

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       beginAttemptCommand.Key.ActionKey(),
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   verificationFinishedAt,
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf(
			"Verification Complete() error = %v",
			err,
		)
	}

	registry := NewRegistry(pool)
	var (
		foreignSource    string
		foreignCluster   string
		foreignAlertName string
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    source,
    cluster,
    alert_name
FROM incidents
WHERE id = $1
`,
		beginAttemptCommand.Key.IncidentID,
	).Scan(
		&foreignSource,
		&foreignCluster,
		&foreignAlertName,
	)
	if err != nil {
		t.Fatalf(
			"load valid Incident observation identity error = %v",
			err,
		)
	}

	foreignObservation := incidentdomain.Observation{
		Source:    foreignSource,
		Cluster:   foreignCluster,
		AlertName: foreignAlertName,
	}
	foreignObservation.Target.Kind = "Pod"
	foreignObservation.Target.Namespace = "default"
	foreignObservation.Target.Name =
		"crash-app-registry-resolution-foreign-target"
	foreignObservation.Target.UID =
		"pod-uid-registry-resolution-foreign-target"
	foreignObservation.Target.Kind = "Pod"
	foreignObservation.Target.Namespace = "default"
	foreignObservation.Target.Name =
		"crash-app-registry-resolution-foreign-target"
	foreignObservation.Target.UID =
		"pod-uid-registry-resolution-foreign-target"

	foreignIncident, foreignCreated, err := registry.Observe(
		ctx,
		foreignObservation,
	)
	if err != nil {
		t.Fatalf(
			"foreign Incident Observe() error = %v",
			err,
		)
	}
	if !foreignCreated {
		t.Fatal(
			"foreign Incident Observe() created = false; want true",
		)
	}

	var expectedForeignVersion int64

	err = pool.QueryRow(
		ctx,
		`
UPDATE incidents
SET
    state = 'VERIFYING',
    version = version + 1
WHERE id = $1
RETURNING version
`,
		foreignIncident.ID,
	).Scan(&expectedForeignVersion)
	if err != nil {
		t.Fatalf(
			"prepare foreign VERIFYING Incident error = %v",
			err,
		)
	}

	resolutionAt := verificationFinishedAt.Add(time.Second)
	claim := claimIncidentForResolutionPostgreSQL(
		t,
		ctx,
		registry,
		incidentdomain.Incident{
			ID:      foreignIncident.ID,
			Version: uint64(expectedForeignVersion),
		},
		"agent-action-attempt",
		resolutionAt,
	)

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      foreignIncident.ID,
			ExpectedVersion: claim.Incident.Version,
			VerificationID:  recoveredVerification.ID,
			HolderID:        claim.HolderID,
			Now:             resolutionAt,
			Actor:           "integration-test",
			ReasonCode:      "WORKLOAD_RECOVERED",
		},
	)
	if err == nil {
		t.Fatal(
			"Registry Resolve() error = nil; want foreign Verification rejection",
		)
	}

	var (
		persistedState                 string
		persistedVersion               int64
		resolvedAtIsNull               bool
		resolutionVerificationIDIsNull bool
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    state,
    version,
    resolved_at IS NULL,
    resolution_verification_id IS NULL
FROM incidents
WHERE id = $1
`,
		foreignIncident.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&resolvedAtIsNull,
		&resolutionVerificationIDIsNull,
	)
	if err != nil {
		t.Fatalf(
			"reload foreign Incident error = %v",
			err,
		)
	}

	if persistedState != string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"persisted Incident state = %q; want %q",
			persistedState,
			incidentdomain.StateVerifying,
		)
	}
	if persistedVersion != int64(claim.Incident.Version) {
		t.Fatalf(
			"persisted Incident version = %d; want unchanged %d",
			persistedVersion,
			claim.Incident.Version,
		)
	}
	if !resolvedAtIsNull {
		t.Fatal(
			"persisted Incident resolved_at is not NULL",
		)
	}
	if !resolutionVerificationIDIsNull {
		t.Fatal(
			"persisted Incident resolution_verification_id is not NULL",
		)
	}

	var resolutionAuditCount int

	err = pool.QueryRow(
		ctx,
		`
SELECT count(*)
FROM incident_transitions
WHERE incident_id = $1
  AND to_state = 'RESOLVED'
`,
		foreignIncident.ID,
	).Scan(&resolutionAuditCount)
	if err != nil {
		t.Fatalf(
			"count foreign Incident RESOLVED audit events error = %v",
			err,
		)
	}
	if resolutionAuditCount != 0 {
		t.Fatalf(
			"foreign Incident RESOLVED audit count = %d; want 0",
			resolutionAuditCount,
		)
	}
}

func TestRegistryResolveRejectsStaleIncidentVersionPostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"registry-resolution-stale-version",
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	incidentID := beginAttemptCommand.Key.IncidentID

	var currentIncidentVersion int64

	err := pool.QueryRow(
		ctx,
		`
UPDATE incidents
SET
    state = 'VERIFYING',
    version = version + 1
WHERE id = $1
RETURNING version
`,
		incidentID,
	).Scan(&currentIncidentVersion)
	if err != nil {
		t.Fatalf(
			"prepare VERIFYING Incident error = %v",
			err,
		)
	}

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Begin() error = %v",
			err,
		)
	}
	if !created {
		t.Fatal(
			"ActionAttempt Begin() created = false; want true",
		)
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf(
			"ActionAttempt Complete() error = %v",
			err,
		)
	}

	verifications := NewVerificationStore(pool)

	pendingVerification, verificationCreated, err :=
		verifications.Begin(
			ctx,
			remediationdomain.BeginVerificationCommand{
				ActionAttempt: succeededAttempt,
				Subject: remediationdomain.VerificationSubject{
					Cluster:   "dev",
					Namespace: "default",
					Kind:      "Pod",
					Name: "crash-app-" +
						"registry-resolution-stale-version",
					UID: beginAttemptCommand.Key.TargetUID,
				},
				StartedAt: actionFinishedAt.Add(time.Second),
			},
		)
	if err != nil {
		t.Fatalf(
			"Verification Begin() error = %v",
			err,
		)
	}
	if !verificationCreated {
		t.Fatal(
			"Verification Begin() created = false; want true",
		)
	}

	verificationFinishedAt := actionFinishedAt.Add(
		2 * time.Second,
	)

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       beginAttemptCommand.Key.ActionKey(),
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   verificationFinishedAt,
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf(
			"Verification Complete() error = %v",
			err,
		)
	}

	registry := NewRegistry(pool)
	resolutionAt := verificationFinishedAt.Add(time.Second)
	claim := claimIncidentForResolutionPostgreSQL(
		t,
		ctx,
		registry,
		incidentdomain.Incident{
			ID:      incidentID,
			Version: uint64(currentIncidentVersion),
		},
		"agent-action-attempt",
		resolutionAt,
	)

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      incidentID,
			ExpectedVersion: uint64(currentIncidentVersion),
			VerificationID:  recoveredVerification.ID,
			HolderID:        claim.HolderID,
			Now:             resolutionAt,
			Actor:           "stale-integration-test",
			ReasonCode:      "WORKLOAD_RECOVERED",
		},
	)
	if err == nil {
		t.Fatal(
			"Registry Resolve() error = nil; want stale Incident version rejection",
		)
	}

	var (
		persistedState                 string
		persistedVersion               int64
		resolvedAtIsNull               bool
		resolutionVerificationIDIsNull bool
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    state,
    version,
    resolved_at IS NULL,
    resolution_verification_id IS NULL
FROM incidents
WHERE id = $1
`,
		incidentID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&resolvedAtIsNull,
		&resolutionVerificationIDIsNull,
	)
	if err != nil {
		t.Fatalf(
			"reload Incident after stale Resolve() error = %v",
			err,
		)
	}

	if persistedState != string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"persisted Incident state = %q; want %q",
			persistedState,
			incidentdomain.StateVerifying,
		)
	}
	if persistedVersion != int64(claim.Incident.Version) {
		t.Fatalf(
			"persisted Incident version = %d; want unchanged %d",
			persistedVersion,
			claim.Incident.Version,
		)
	}
	if !resolvedAtIsNull {
		t.Fatal(
			"persisted Incident resolved_at is not NULL",
		)
	}
	if !resolutionVerificationIDIsNull {
		t.Fatal(
			"persisted Incident resolution_verification_id is not NULL",
		)
	}

	var resolutionAuditCount int

	err = pool.QueryRow(
		ctx,
		`
SELECT count(*)
FROM incident_transitions
WHERE incident_id = $1
  AND to_state = 'RESOLVED'
`,
		incidentID,
	).Scan(&resolutionAuditCount)
	if err != nil {
		t.Fatalf(
			"count RESOLVED audit events error = %v",
			err,
		)
	}
	if resolutionAuditCount != 0 {
		t.Fatalf(
			"RESOLVED audit event count = %d; want 0",
			resolutionAuditCount,
		)
	}
}
func TestRegistryResolveRequiresActiveAuditedClaimPostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"registry-resolution-fence",
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	incidentID := beginAttemptCommand.Key.IncidentID
	var verifyingVersion int64

	err := pool.QueryRow(
		ctx,
		`
UPDATE incidents
SET
    state = 'VERIFYING',
    version = version + 1
WHERE id = $1
RETURNING version
`,
		incidentID,
	).Scan(&verifyingVersion)
	if err != nil {
		t.Fatalf("prepare VERIFYING Incident error = %v", err)
	}

	actionAttempts := NewActionAttemptStore(pool)
	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(time.Second)
	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	verifications := NewVerificationStore(pool)
	pendingVerification, created, err := verifications.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject: remediationdomain.VerificationSubject{
				Cluster:   "dev",
				Namespace: "default",
				Kind:      "Pod",
				Name:      "crash-app-registry-resolution-fence",
				UID:       beginAttemptCommand.Key.TargetUID,
			},
			StartedAt: actionFinishedAt.Add(time.Second),
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Verification Begin() created = false; want true")
	}

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       beginAttemptCommand.Key.ActionKey(),
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   actionFinishedAt.Add(2 * time.Second),
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	registry := NewRegistry(pool)
	resolutionAt := actionFinishedAt.Add(3 * time.Second)
	claim := claimIncidentForResolutionPostgreSQL(
		t,
		ctx,
		registry,
		incidentdomain.Incident{
			ID:      incidentID,
			Version: uint64(verifyingVersion),
		},
		"agent-action-attempt",
		resolutionAt,
	)

	tests := []struct {
		name     string
		holderID string
		now      time.Time
	}{
		{
			name:     "wrong_holder",
			holderID: "stale-verification-worker",
			now:      resolutionAt,
		},
		{
			name:     "expired_lease",
			holderID: claim.HolderID,
			now:      claim.ExpiresAt,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, resolveErr := registry.Resolve(
				ctx,
				incidentdomain.ResolveCommand{
					IncidentID:      incidentID,
					ExpectedVersion: claim.Incident.Version,
					VerificationID:  recoveredVerification.ID,
					HolderID:        test.holderID,
					Now:             test.now,
					Actor:           test.holderID,
					ReasonCode:      "RECOVERY_VERIFIED",
				},
			)
			if !errors.Is(
				resolveErr,
				incidentdomain.ErrVerificationFenceConflict,
			) {
				t.Fatalf(
					"Resolve() error = %v; "+
						"want ErrVerificationFenceConflict",
					resolveErr,
				)
			}
		})
	}

	_, err = pool.Exec(
		ctx,
		`
DELETE FROM incident_claims
WHERE incident_id = $1
  AND incident_version = $2
`,
		incidentID,
		claim.Incident.Version,
	)
	if err != nil {
		t.Fatalf("delete current Claim Audit error = %v", err)
	}

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      incidentID,
			ExpectedVersion: claim.Incident.Version,
			VerificationID:  recoveredVerification.ID,
			HolderID:        claim.HolderID,
			Now:             resolutionAt,
			Actor:           claim.HolderID,
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if !errors.Is(
		err,
		incidentdomain.ErrVerificationFenceConflict,
	) {
		t.Fatalf(
			"Resolve() without Claim Audit error = %v; "+
				"want ErrVerificationFenceConflict",
			err,
		)
	}

	var (
		persistedState   string
		persistedVersion int64
	)
	err = pool.QueryRow(
		ctx,
		`
SELECT state, version
FROM incidents
WHERE id = $1
`,
		incidentID,
	).Scan(&persistedState, &persistedVersion)
	if err != nil {
		t.Fatalf("reload fenced Incident error = %v", err)
	}
	if persistedState != string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"persisted Incident state = %q; want %q",
			persistedState,
			incidentdomain.StateVerifying,
		)
	}
	if persistedVersion != int64(claim.Incident.Version) {
		t.Fatalf(
			"persisted Incident version = %d; want unchanged %d",
			persistedVersion,
			claim.Incident.Version,
		)
	}
}

func claimIncidentForResolutionPostgreSQL(
	t *testing.T,
	ctx context.Context,
	registry *Registry,
	current incidentdomain.Incident,
	holderID string,
	now time.Time,
) incidentdomain.Claim {
	t.Helper()

	claim, err := registry.Claim(
		ctx,
		incidentdomain.ClaimCommand{
			IncidentID:      current.ID,
			ExpectedVersion: current.Version,
			HolderID:        holderID,
			Now:             now,
			LeaseDuration:   time.Minute,
		},
	)
	if err != nil {
		t.Fatalf("Claim() for resolution error = %v", err)
	}

	return claim
}
