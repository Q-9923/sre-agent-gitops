//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	approvaldomain "sre-agent/internal/approval"
	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

const (
	fencedVerificationLockNamespace = 7319
	fencedVerificationLockKey       = 1
)

type postgresFencedVerificationOutcome struct {
	result incidentdomain.FencedVerificationResult
	err    error
}

type postgresClaimOutcome struct {
	claim incidentdomain.Claim
	err   error
}

func TestRegistryBeginFencedVerificationAtomicallyRejectsConcurrentTakeoverPostgreSQL(
	t *testing.T,
) {
	ctx, pool, registry := newPostgresTestRegistry(t)

	now := time.Date(
		2026,
		time.October,
		7,
		10,
		0,
		0,
		0,
		time.UTC,
	)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-fenced-verification",
			UID:       "pod-uid-fenced-verification",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Incident Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "sre-agent-v2",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Incident Transition(DIAGNOSED) error = %v",
			err,
		)
	}

	actionClaim, err := registry.Claim(
		ctx,
		incidentdomain.ClaimCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Incident Claim(agent-a) error = %v", err)
	}

	plan, err := approvaldomain.NewPlan(
		approvaldomain.PlanCommand{
			IncidentID: actionClaim.Incident.ID,
			Action:     "RESTART_POD",
			Target: approvaldomain.Target{
				Cluster:   observation.Cluster,
				Namespace: observation.Target.Namespace,
				Kind:      observation.Target.Kind,
				Name:      observation.Target.Name,
				UID:       observation.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	if err := NewPlanStore(pool).Publish(ctx, plan); err != nil {
		t.Fatalf("PlanStore Publish() error = %v", err)
	}

	actionAttempts := NewActionAttemptStore(pool)

	executionKey := remediationdomain.ExecutionKey{
		IncidentID: actionClaim.Incident.ID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.Target.UID,
		FencingToken: actionClaim.
			Incident.Version,
	}

	startedAttempt, attemptCreated, err := actionAttempts.Begin(
		ctx,
		remediationdomain.BeginActionAttemptCommand{
			Key:       executionKey,
			StartedAt: now.Add(100 * time.Millisecond),
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !attemptCreated {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             executionKey,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: now.Add(200 * time.Millisecond),
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	/*
		The trigger stops the Verification INSERT inside the lifecycle
		transaction. A correct implementation must already hold a row lock on
		the Incident while it waits here.
	*/
	_, err = pool.Exec(
		ctx,
		`
CREATE FUNCTION block_fenced_verification_insert_test()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	PERFORM pg_advisory_xact_lock(7319, 1);
	RETURN NEW;
END;
$$
`,
	)
	if err != nil {
		t.Fatalf(
			"create blocking Verification trigger function: %v",
			err,
		)
	}

	_, err = pool.Exec(
		ctx,
		`
CREATE TRIGGER block_fenced_verification_insert_test
BEFORE INSERT ON verifications
FOR EACH ROW
EXECUTE FUNCTION block_fenced_verification_insert_test()
`,
	)
	if err != nil {
		t.Fatalf("create blocking Verification trigger: %v", err)
	}

	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire PostgreSQL blocker connection: %v", err)
	}
	defer blocker.Release()

	_, err = blocker.Exec(
		ctx,
		`SELECT pg_advisory_lock($1, $2)`,
		fencedVerificationLockNamespace,
		fencedVerificationLockKey,
	)
	if err != nil {
		t.Fatalf("acquire Verification advisory lock: %v", err)
	}

	lockReleased := false
	defer func() {
		if lockReleased {
			return
		}

		_, _ = blocker.Exec(
			ctx,
			`SELECT pg_advisory_unlock($1, $2)`,
			fencedVerificationLockNamespace,
			fencedVerificationLockKey,
		)
	}()

	beginResults := make(
		chan postgresFencedVerificationOutcome,
		1,
	)

	go func() {
		result, beginErr := registry.BeginFencedVerification(
			ctx,
			incidentdomain.BeginFencedVerificationCommand{
				IncidentID: actionClaim.Incident.ID,
				ExpectedVersion: actionClaim.
					Incident.Version,
				HolderID:   "agent-a",
				Now:        now.Add(500 * time.Millisecond),
				ReasonCode: "ACTION_ATTEMPT_TERMINAL",
				Verification: remediationdomain.
					BeginVerificationCommand{
					ActionAttempt: succeededAttempt,
					Subject: remediationdomain.VerificationSubject{
						Cluster:   observation.Cluster,
						Namespace: observation.Target.Namespace,
						Kind:      "Deployment",
						Name:      "crash-app",
						UID: "deployment-uid-" +
							"fenced-verification",
					},
					StartedAt: now.Add(
						300 * time.Millisecond,
					),
				},
			},
		)

		beginResults <- postgresFencedVerificationOutcome{
			result: result,
			err:    beginErr,
		}
	}()

	/*
		Wait until the lifecycle transaction reaches the blocked trigger.
		This avoids relying on a blind sleep to decide whether the transaction
		has already locked the Incident.
	*/
	waitDeadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool

		err = pool.QueryRow(
			ctx,
			`
SELECT EXISTS (
	SELECT 1
	FROM pg_locks
	WHERE locktype = 'advisory'
	  AND classid = $1::oid
	  AND objid = $2::oid
	  AND objsubid = 2
	  AND granted = false
)
`,
			fencedVerificationLockNamespace,
			fencedVerificationLockKey,
		).Scan(&waiting)
		if err != nil {
			t.Fatalf(
				"inspect waiting Verification advisory lock: %v",
				err,
			)
		}

		if waiting {
			break
		}

		select {
		case outcome := <-beginResults:
			t.Fatalf(
				"BeginFencedVerification() returned before "+
					"the blocking trigger: result=%#v error=%v",
				outcome.result,
				outcome.err,
			)
		default:
		}

		if time.Now().After(waitDeadline) {
			t.Fatal(
				"timed out waiting for the Verification INSERT " +
					"to reach the blocking trigger",
			)
		}

		time.Sleep(10 * time.Millisecond)
	}

	takeoverResults := make(chan postgresClaimOutcome, 1)

	go func() {
		claim, claimErr := registry.Claim(
			ctx,
			incidentdomain.ClaimCommand{
				IncidentID: actionClaim.Incident.ID,
				ExpectedVersion: actionClaim.
					Incident.Version,
				HolderID:      "agent-b",
				Now:           now.Add(2 * time.Second),
				LeaseDuration: time.Minute,
			},
		)

		takeoverResults <- postgresClaimOutcome{
			claim: claim,
			err:   claimErr,
		}
	}()

	select {
	case outcome := <-takeoverResults:
		t.Fatalf(
			"concurrent takeover completed before the fenced "+
				"Verification transaction committed: claim=%#v error=%v",
			outcome.claim,
			outcome.err,
		)
	case <-time.After(100 * time.Millisecond):
	}

	var unlocked bool
	err = blocker.QueryRow(
		ctx,
		`SELECT pg_advisory_unlock($1, $2)`,
		fencedVerificationLockNamespace,
		fencedVerificationLockKey,
	).Scan(&unlocked)
	if err != nil {
		t.Fatalf("release Verification advisory lock: %v", err)
	}
	if !unlocked {
		t.Fatal("Verification advisory lock was not held")
	}
	lockReleased = true

	var beginOutcome postgresFencedVerificationOutcome
	select {
	case beginOutcome = <-beginResults:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for BeginFencedVerification()")
	}

	if beginOutcome.err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v; want nil",
			beginOutcome.err,
		)
	}

	if beginOutcome.result.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			beginOutcome.result.Incident.State,
			incidentdomain.StateVerifying,
		)
	}

	expectedVersion := actionClaim.Incident.Version + 1
	if beginOutcome.result.Incident.Version != expectedVersion {
		t.Fatalf(
			"Incident Version = %d; want %d",
			beginOutcome.result.Incident.Version,
			expectedVersion,
		)
	}

	if !beginOutcome.result.Created {
		t.Fatal(
			"BeginFencedVerification() Created = false; want true",
		)
	}

	if beginOutcome.result.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			beginOutcome.result.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	var takeoverOutcome postgresClaimOutcome
	select {
	case takeoverOutcome = <-takeoverResults:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent takeover")
	}

	if !errors.Is(
		takeoverOutcome.err,
		incidentdomain.ErrVersionConflict,
	) {
		t.Fatalf(
			"concurrent takeover error = %v; want ErrVersionConflict",
			takeoverOutcome.err,
		)
	}

	var (
		persistedState        string
		persistedVersion      uint64
		persistedAttemptID    string
		persistedVerifyStatus string
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
	i.state,
	i.version,
	v.action_attempt_id,
	v.status
FROM incidents AS i
JOIN verifications AS v
  ON v.id = $2
WHERE i.id = $1
`,
		actionClaim.Incident.ID,
		beginOutcome.result.Verification.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&persistedAttemptID,
		&persistedVerifyStatus,
	)
	if err != nil {
		t.Fatalf(
			"reload fenced Verification lifecycle state: %v",
			err,
		)
	}

	if persistedState != string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"persisted Incident State = %q; want %q",
			persistedState,
			incidentdomain.StateVerifying,
		)
	}

	if persistedVersion != expectedVersion {
		t.Fatalf(
			"persisted Incident Version = %d; want %d",
			persistedVersion,
			expectedVersion,
		)
	}

	if persistedAttemptID != succeededAttempt.ID {
		t.Fatalf(
			"persisted ActionAttempt ID = %q; want %q",
			persistedAttemptID,
			succeededAttempt.ID,
		)
	}

	if persistedVerifyStatus !=
		string(remediationdomain.VerificationStatusPending) {
		t.Fatalf(
			"persisted Verification Status = %q; want %q",
			persistedVerifyStatus,
			remediationdomain.VerificationStatusPending,
		)
	}
}

type postgresFencedVerificationRollbackFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	registry      *Registry
	claim         incidentdomain.Claim
	actionAttempt remediationdomain.ActionAttempt
	command       incidentdomain.BeginFencedVerificationCommand
}

func newPostgresFencedVerificationRollbackFixture(
	t *testing.T,
	suffix string,
	subjectName string,
) postgresFencedVerificationRollbackFixture {
	t.Helper()

	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		suffix,
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	registry := NewRegistry(pool)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "crash-app-" + suffix,
			UID:       "pod-uid-" + suffix,
		},
	}

	detected, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Incident Observe() error = %v", err)
	}
	if created {
		t.Fatal(
			"Incident Observe() created = true; " +
				"want existing fixture Incident",
		)
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "sre-agent-v2",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Incident Transition(DIAGNOSED) error = %v",
			err,
		)
	}

	reclaimTime := beginAttemptCommand.StartedAt.Add(
		time.Second,
	)

	verificationClaim, err := registry.Claim(
		ctx,
		incidentdomain.ClaimCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			HolderID:        "agent-action-attempt",
			Now:             reclaimTime,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf(
			"Incident Claim() for Verification error = %v",
			err,
		)
	}

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, attemptCreated, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !attemptCreated {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		2 * time.Second,
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

	return postgresFencedVerificationRollbackFixture{
		ctx:           ctx,
		pool:          pool,
		registry:      registry,
		claim:         verificationClaim,
		actionAttempt: succeededAttempt,
		command: incidentdomain.BeginFencedVerificationCommand{
			IncidentID: verificationClaim.Incident.ID,
			ExpectedVersion: verificationClaim.
				Incident.Version,
			HolderID:   "agent-action-attempt",
			Now:        actionFinishedAt.Add(time.Second),
			ReasonCode: "ACTION_ATTEMPT_TERMINAL",
			Verification: remediationdomain.
				BeginVerificationCommand{
				ActionAttempt: succeededAttempt,
				Subject: remediationdomain.VerificationSubject{
					Cluster:   observation.Cluster,
					Namespace: observation.Target.Namespace,
					Kind:      "Deployment",
					Name:      subjectName,
					UID:       "deployment-uid-" + suffix,
				},
				StartedAt: actionFinishedAt.Add(
					time.Second,
				),
			},
		},
	}
}

func TestRegistryBeginFencedVerificationRollsBackWhenVerificationInsertFailsPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationRollbackFixture(
		t,
		"fenced-verification-rollback",
		"reject-fenced-verification",
	)

	_, err := fixture.pool.Exec(
		fixture.ctx,
		`
ALTER TABLE verifications
ADD CONSTRAINT verifications_reject_fenced_rollback_test
CHECK (
	subject_name <> 'reject-fenced-verification'
)
`,
	)
	if err != nil {
		t.Fatalf(
			"install failing Verification constraint: %v",
			err,
		)
	}

	_, err = fixture.registry.BeginFencedVerification(
		fixture.ctx,
		fixture.command,
	)
	if err == nil {
		t.Fatal(
			"BeginFencedVerification() error = nil; " +
				"want Verification INSERT failure",
		)
	}

	assertPostgresFencedVerificationRolledBack(
		t,
		fixture,
	)
}

func TestRegistryBeginFencedVerificationRollsBackWhenTransitionAuditInsertFailsPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationRollbackFixture(
		t,
		"fenced-verification-audit-rollback",
		"crash-app-verification-subject",
	)

	_, err := fixture.pool.Exec(
		fixture.ctx,
		`
ALTER TABLE incident_transitions
ADD CONSTRAINT incident_transitions_reject_verifying_test
CHECK (
	to_state <> 'VERIFYING'
)
`,
	)
	if err != nil {
		t.Fatalf(
			"install failing VERIFYING audit constraint: %v",
			err,
		)
	}

	_, err = fixture.registry.BeginFencedVerification(
		fixture.ctx,
		fixture.command,
	)
	if err == nil {
		t.Fatal(
			"BeginFencedVerification() error = nil; " +
				"want transition audit INSERT failure",
		)
	}

	assertPostgresFencedVerificationRolledBack(
		t,
		fixture,
	)
}

func assertPostgresFencedVerificationRolledBack(
	t *testing.T,
	fixture postgresFencedVerificationRollbackFixture,
) {
	t.Helper()

	var (
		persistedState   string
		persistedVersion int64
	)

	err := fixture.pool.QueryRow(
		fixture.ctx,
		`
SELECT
	state,
	version
FROM incidents
WHERE id = $1
`,
		fixture.claim.Incident.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
	)
	if err != nil {
		t.Fatalf("reload Incident after rollback: %v", err)
	}

	if persistedState != string(incidentdomain.StateDiagnosed) {
		t.Fatalf(
			"Incident State after rollback = %q; want %q",
			persistedState,
			incidentdomain.StateDiagnosed,
		)
	}

	if uint64(persistedVersion) != fixture.claim.Incident.Version {
		t.Fatalf(
			"Incident Version after rollback = %d; want %d",
			persistedVersion,
			fixture.claim.Incident.Version,
		)
	}

	var verificationCount int

	err = fixture.pool.QueryRow(
		fixture.ctx,
		`
SELECT count(*)
FROM verifications
WHERE action_attempt_id = $1
`,
		fixture.actionAttempt.ID,
	).Scan(&verificationCount)
	if err != nil {
		t.Fatalf(
			"count Verifications after rollback: %v",
			err,
		)
	}

	if verificationCount != 0 {
		t.Fatalf(
			"Verification count after rollback = %d; want 0",
			verificationCount,
		)
	}

	var verifyingAuditCount int

	err = fixture.pool.QueryRow(
		fixture.ctx,
		`
SELECT count(*)
FROM incident_transitions
WHERE
	incident_id = $1
	AND to_state = $2
`,
		fixture.claim.Incident.ID,
		incidentdomain.StateVerifying,
	).Scan(&verifyingAuditCount)
	if err != nil {
		t.Fatalf(
			"count VERIFYING audits after rollback: %v",
			err,
		)
	}

	if verifyingAuditCount != 0 {
		t.Fatalf(
			"VERIFYING audit count after rollback = %d; want 0",
			verifyingAuditCount,
		)
	}
}
