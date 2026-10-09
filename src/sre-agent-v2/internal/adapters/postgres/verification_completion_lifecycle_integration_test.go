//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryCompleteFencedVerificationPersistsAcrossAdapterRestartPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationRollbackFixture(
		t,
		"fenced-verification-completion-restart",
		"crash-app-fenced-completion-restart",
	)

	fenced, err := fixture.registry.BeginFencedVerification(
		fixture.ctx,
		fixture.command,
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}
	if !fenced.Created {
		t.Fatal(
			"BeginFencedVerification() Created = false; want true",
		)
	}
	if fenced.Incident.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			fenced.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if fenced.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			fenced.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	schedulerNow := fixture.claim.ExpiresAt.Add(time.Second)
	leaseDuration := 30 * time.Second

	schedulerRegistry := NewRegistry(fixture.pool)

	scheduled, found, err :=
		schedulerRegistry.ClaimPendingVerification(
			fixture.ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID: "verification-completion-" +
					"scheduler",
				Now:           schedulerNow,
				LeaseDuration: leaseDuration,
			},
		)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification() error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification() found = false; want true",
		)
	}
	if scheduled.Verification.ID != fenced.Verification.ID {
		t.Fatalf(
			"scheduled Verification ID = %q; want %q",
			scheduled.Verification.ID,
			fenced.Verification.ID,
		)
	}
	if scheduled.Claim.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"scheduled Incident State = %q; want %q",
			scheduled.Claim.Incident.State,
			incidentdomain.StateVerifying,
		)
	}

	completedAt := schedulerNow.Add(time.Second)
	command := incidentdomain.CompleteFencedVerificationCommand{
		IncidentID: scheduled.Claim.Incident.ID,
		ExpectedVersion: scheduled.Claim.Incident.
			Version,
		HolderID: scheduled.Claim.HolderID,
		Now:      completedAt,
		Verification: remediationdomain.
			CompleteVerificationCommand{
			ActionKey: scheduled.Verification.ActionKey,
			ExpectedVersion: scheduled.Verification.
				Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   completedAt,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}

	restartedRegistry := NewRegistry(fixture.pool)

	completed, err :=
		restartedRegistry.CompleteFencedVerification(
			fixture.ctx,
			command,
		)
	if err != nil {
		t.Fatalf(
			"CompleteFencedVerification() error = %v",
			err,
		)
	}

	if completed.Incident.ID !=
		scheduled.Claim.Incident.ID {
		t.Fatalf(
			"completed Incident ID = %q; want %q",
			completed.Incident.ID,
			scheduled.Claim.Incident.ID,
		)
	}
	if completed.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"completed Incident State = %q; want %q",
			completed.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if completed.Incident.Version !=
		scheduled.Claim.Incident.Version {
		t.Fatalf(
			"completed Incident Version = %d; want unchanged %d",
			completed.Incident.Version,
			scheduled.Claim.Incident.Version,
		)
	}
	if completed.Incident.ResolutionVerificationID != "" {
		t.Fatalf(
			"completed Incident ResolutionVerificationID = %q; want empty",
			completed.Incident.ResolutionVerificationID,
		)
	}

	if completed.Verification.ID != scheduled.Verification.ID {
		t.Fatalf(
			"completed Verification ID = %q; want %q",
			completed.Verification.ID,
			scheduled.Verification.ID,
		)
	}
	if completed.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"completed Verification Status = %q; want %q",
			completed.Verification.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if completed.Verification.Version !=
		scheduled.Verification.Version+1 {
		t.Fatalf(
			"completed Verification Version = %d; want %d",
			completed.Verification.Version,
			scheduled.Verification.Version+1,
		)
	}
	if completed.Verification.FinishedAt == nil {
		t.Fatal(
			"completed Verification FinishedAt = nil; want non-nil",
		)
	}
	if !completed.Verification.FinishedAt.Equal(completedAt) {
		t.Fatalf(
			"completed Verification FinishedAt = %s; want %s",
			completed.Verification.FinishedAt,
			completedAt,
		)
	}
	if completed.Verification.EvidenceCode !=
		command.Verification.EvidenceCode {
		t.Fatalf(
			"completed Verification EvidenceCode = %q; want %q",
			completed.Verification.EvidenceCode,
			command.Verification.EvidenceCode,
		)
	}

	var (
		persistedIncidentState       string
		persistedIncidentVersion     int64
		resolutionVerificationID     string
		persistedVerificationStatus  string
		persistedVerificationVersion int64
		persistedFinishedAt          time.Time
		persistedEvidenceCode        string
	)

	err = fixture.pool.QueryRow(
		fixture.ctx,
		`
SELECT
	state,
	version,
	COALESCE(resolution_verification_id, '')
FROM incidents
WHERE id = $1
`,
		scheduled.Claim.Incident.ID,
	).Scan(
		&persistedIncidentState,
		&persistedIncidentVersion,
		&resolutionVerificationID,
	)
	if err != nil {
		t.Fatalf(
			"reload persisted Incident error = %v",
			err,
		)
	}

	if persistedIncidentState !=
		string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"persisted Incident State = %q; want %q",
			persistedIncidentState,
			incidentdomain.StateVerifying,
		)
	}
	if persistedIncidentVersion !=
		int64(scheduled.Claim.Incident.Version) {
		t.Fatalf(
			"persisted Incident Version = %d; want %d",
			persistedIncidentVersion,
			scheduled.Claim.Incident.Version,
		)
	}
	if resolutionVerificationID != "" {
		t.Fatalf(
			"persisted ResolutionVerificationID = %q; want empty",
			resolutionVerificationID,
		)
	}

	err = fixture.pool.QueryRow(
		fixture.ctx,
		`
SELECT
	status,
	version,
	finished_at,
	COALESCE(evidence_code, '')
FROM verifications
WHERE id = $1
`,
		scheduled.Verification.ID,
	).Scan(
		&persistedVerificationStatus,
		&persistedVerificationVersion,
		&persistedFinishedAt,
		&persistedEvidenceCode,
	)
	if err != nil {
		t.Fatalf(
			"reload persisted Verification error = %v",
			err,
		)
	}

	if persistedVerificationStatus !=
		string(remediationdomain.VerificationStatusRecovered) {
		t.Fatalf(
			"persisted Verification Status = %q; want %q",
			persistedVerificationStatus,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if persistedVerificationVersion !=
		scheduled.Verification.Version+1 {
		t.Fatalf(
			"persisted Verification Version = %d; want %d",
			persistedVerificationVersion,
			scheduled.Verification.Version+1,
		)
	}
	if !persistedFinishedAt.Equal(completedAt) {
		t.Fatalf(
			"persisted Verification FinishedAt = %s; want %s",
			persistedFinishedAt,
			completedAt,
		)
	}
	if persistedEvidenceCode !=
		command.Verification.EvidenceCode {
		t.Fatalf(
			"persisted Verification EvidenceCode = %q; want %q",
			persistedEvidenceCode,
			command.Verification.EvidenceCode,
		)
	}

	restartedVerificationStore :=
		NewVerificationStore(fixture.pool)

	retried, err := restartedVerificationStore.Complete(
		fixture.ctx,
		command.Verification,
	)
	if err != nil {
		t.Fatalf(
			"exact retry Verification Complete() error = %v",
			err,
		)
	}
	if retried.ID != completed.Verification.ID ||
		retried.Status != completed.Verification.Status ||
		retried.Version != completed.Verification.Version ||
		retried.FinishedAt == nil ||
		!retried.FinishedAt.Equal(
			*completed.Verification.FinishedAt,
		) ||
		retried.EvidenceCode !=
			completed.Verification.EvidenceCode {
		t.Fatalf(
			"exact retry Verification = %#v; want %#v",
			retried,
			completed.Verification,
		)
	}
}
func TestRegistryCompleteFencedVerificationRollsBackWhenVerificationUpdateFailsPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationCompletionFixture(
		t,
		"fenced-verification-completion-rollback",
	)

	_, err := fixture.base.pool.Exec(
		fixture.base.ctx,
		`
ALTER TABLE verifications
ADD CONSTRAINT verifications_reject_fenced_completion_test
CHECK (
	evidence_code IS DISTINCT FROM
		'REJECT_FENCED_COMPLETION'
)
`,
	)
	if err != nil {
		t.Fatalf(
			"install failing Verification completion constraint: %v",
			err,
		)
	}

	command := fixture.command
	command.Verification.EvidenceCode =
		"REJECT_FENCED_COMPLETION"

	_, err = NewRegistry(fixture.base.pool).
		CompleteFencedVerification(
			fixture.base.ctx,
			command,
		)
	if err == nil {
		t.Fatal(
			"CompleteFencedVerification() error = nil; " +
				"want Verification UPDATE failure",
		)
	}

	var (
		persistedIncidentState   string
		persistedIncidentVersion int64
		persistedClaimHolder     string
		persistedClaimExpiry     time.Time
	)

	err = fixture.base.pool.QueryRow(
		fixture.base.ctx,
		`
SELECT
	state,
	version,
	COALESCE(claim_holder, ''),
	claim_expires_at
FROM incidents
WHERE id = $1
`,
		fixture.scheduled.Claim.Incident.ID,
	).Scan(
		&persistedIncidentState,
		&persistedIncidentVersion,
		&persistedClaimHolder,
		&persistedClaimExpiry,
	)
	if err != nil {
		t.Fatalf(
			"reload Incident after completion rollback: %v",
			err,
		)
	}

	if persistedIncidentState !=
		string(incidentdomain.StateVerifying) {
		t.Fatalf(
			"Incident State after rollback = %q; want %q",
			persistedIncidentState,
			incidentdomain.StateVerifying,
		)
	}
	if persistedIncidentVersion !=
		int64(fixture.scheduled.Claim.Incident.Version) {
		t.Fatalf(
			"Incident Version after rollback = %d; want %d",
			persistedIncidentVersion,
			fixture.scheduled.Claim.Incident.Version,
		)
	}
	if persistedClaimHolder !=
		fixture.scheduled.Claim.HolderID {
		t.Fatalf(
			"Claim Holder after rollback = %q; want %q",
			persistedClaimHolder,
			fixture.scheduled.Claim.HolderID,
		)
	}
	if !persistedClaimExpiry.Equal(
		fixture.scheduled.Claim.ExpiresAt,
	) {
		t.Fatalf(
			"Claim Expiry after rollback = %s; want %s",
			persistedClaimExpiry,
			fixture.scheduled.Claim.ExpiresAt,
		)
	}

	var (
		persistedVerificationStatus  string
		persistedVerificationVersion int64
		finishedAtIsNull             bool
		persistedEvidenceCode        string
	)

	err = fixture.base.pool.QueryRow(
		fixture.base.ctx,
		`
SELECT
	status,
	version,
	finished_at IS NULL,
	COALESCE(evidence_code, '')
FROM verifications
WHERE id = $1
`,
		fixture.scheduled.Verification.ID,
	).Scan(
		&persistedVerificationStatus,
		&persistedVerificationVersion,
		&finishedAtIsNull,
		&persistedEvidenceCode,
	)
	if err != nil {
		t.Fatalf(
			"reload Verification after completion rollback: %v",
			err,
		)
	}

	if persistedVerificationStatus !=
		string(remediationdomain.VerificationStatusPending) {
		t.Fatalf(
			"Verification Status after rollback = %q; want %q",
			persistedVerificationStatus,
			remediationdomain.VerificationStatusPending,
		)
	}
	if persistedVerificationVersion !=
		fixture.scheduled.Verification.Version {
		t.Fatalf(
			"Verification Version after rollback = %d; want %d",
			persistedVerificationVersion,
			fixture.scheduled.Verification.Version,
		)
	}
	if !finishedAtIsNull {
		t.Fatal(
			"Verification FinishedAt after rollback is non-null; want null",
		)
	}
	if persistedEvidenceCode != "" {
		t.Fatalf(
			"Verification EvidenceCode after rollback = %q; want empty",
			persistedEvidenceCode,
		)
	}

	takeoverNow :=
		fixture.scheduled.Claim.ExpiresAt.Add(time.Second)

	recovered, found, err :=
		NewRegistry(fixture.base.pool).
			ClaimPendingVerification(
				fixture.base.ctx,
				incidentdomain.
					ClaimPendingVerificationCommand{
					HolderID: "verification-scheduler-" +
						"after-rollback",
					Now: takeoverNow,
					LeaseDuration: 30 *
						time.Second,
				},
			)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification() after rollback error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification() after rollback found = false; " +
				"want true",
		)
	}
	if recovered.Verification.ID !=
		fixture.scheduled.Verification.ID {
		t.Fatalf(
			"recovered Verification ID = %q; want %q",
			recovered.Verification.ID,
			fixture.scheduled.Verification.ID,
		)
	}
	if recovered.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"recovered Verification Status = %q; want %q",
			recovered.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if recovered.Claim.Incident.Version !=
		fixture.scheduled.Claim.Incident.Version+1 {
		t.Fatalf(
			"recovered Incident Version = %d; want %d",
			recovered.Claim.Incident.Version,
			fixture.scheduled.Claim.Incident.Version+1,
		)
	}
	if recovered.Claim.HolderID !=
		"verification-scheduler-after-rollback" {
		t.Fatalf(
			"recovered Claim HolderID = %q; want %q",
			recovered.Claim.HolderID,
			"verification-scheduler-after-rollback",
		)
	}
}

const fencedVerificationCompletionLockKey = 2

type postgresFencedVerificationCompletionFixture struct {
	base      postgresFencedVerificationRollbackFixture
	scheduled incidentdomain.PendingVerificationClaim
	command   incidentdomain.CompleteFencedVerificationCommand
}

type postgresFencedVerificationCompletionOutcome struct {
	result incidentdomain.FencedVerificationCompletionResult
	err    error
}

type postgresFencedVerificationTakeoverOutcome struct {
	result incidentdomain.PendingVerificationClaim
	found  bool
	err    error
}

func newPostgresFencedVerificationCompletionFixture(
	t *testing.T,
	suffix string,
) postgresFencedVerificationCompletionFixture {
	t.Helper()

	base := newPostgresFencedVerificationRollbackFixture(
		t,
		suffix,
		"crash-app-"+suffix,
	)

	fenced, err := base.registry.BeginFencedVerification(
		base.ctx,
		base.command,
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}
	if !fenced.Created {
		t.Fatal(
			"BeginFencedVerification() Created = false; want true",
		)
	}
	if fenced.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			fenced.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	schedulerNow := base.claim.ExpiresAt.Add(time.Second)
	leaseDuration := 30 * time.Second

	scheduled, found, err :=
		NewRegistry(base.pool).ClaimPendingVerification(
			base.ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID:      "verification-scheduler-a",
				Now:           schedulerNow,
				LeaseDuration: leaseDuration,
			},
		)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification() error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification() found = false; want true",
		)
	}

	completedAt := schedulerNow.Add(time.Second)

	return postgresFencedVerificationCompletionFixture{
		base:      base,
		scheduled: scheduled,
		command: incidentdomain.CompleteFencedVerificationCommand{
			IncidentID: scheduled.Claim.Incident.ID,
			ExpectedVersion: scheduled.Claim.Incident.
				Version,
			HolderID: scheduled.Claim.HolderID,
			Now:      completedAt,
			Verification: remediationdomain.
				CompleteVerificationCommand{
				ActionKey: scheduled.Verification.ActionKey,
				ExpectedVersion: scheduled.Verification.
					Version,
				To: remediationdomain.
					VerificationStatusRecovered,
				FinishedAt:   completedAt,
				EvidenceCode: "WORKLOAD_RECOVERED",
			},
		},
	}
}

func TestRegistryCompleteFencedVerificationAtomicallyExcludesLeaseTakeoverPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationCompletionFixture(
		t,
		"fenced-verification-completion-concurrency",
	)

	_, err := fixture.base.pool.Exec(
		fixture.base.ctx,
		`
CREATE FUNCTION block_fenced_verification_completion_test()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	IF OLD.status = 'PENDING'
	   AND NEW.status <> 'PENDING' THEN
		PERFORM pg_advisory_xact_lock(7319, 2);
	END IF;

	RETURN NEW;
END;
$$
`,
	)
	if err != nil {
		t.Fatalf(
			"create blocking Verification completion function: %v",
			err,
		)
	}

	_, err = fixture.base.pool.Exec(
		fixture.base.ctx,
		`
CREATE TRIGGER block_fenced_verification_completion_test
BEFORE UPDATE ON verifications
FOR EACH ROW
EXECUTE FUNCTION block_fenced_verification_completion_test()
`,
	)
	if err != nil {
		t.Fatalf(
			"create blocking Verification completion trigger: %v",
			err,
		)
	}

	blocker, err := fixture.base.pool.Acquire(
		fixture.base.ctx,
	)
	if err != nil {
		t.Fatalf(
			"acquire PostgreSQL blocker connection: %v",
			err,
		)
	}
	defer blocker.Release()

	_, err = blocker.Exec(
		fixture.base.ctx,
		`SELECT pg_advisory_lock($1, $2)`,
		fencedVerificationLockNamespace,
		fencedVerificationCompletionLockKey,
	)
	if err != nil {
		t.Fatalf(
			"acquire Verification completion advisory lock: %v",
			err,
		)
	}

	lockReleased := false
	defer func() {
		if lockReleased {
			return
		}

		_, _ = blocker.Exec(
			fixture.base.ctx,
			`SELECT pg_advisory_unlock($1, $2)`,
			fencedVerificationLockNamespace,
			fencedVerificationCompletionLockKey,
		)
	}()

	completionResults := make(
		chan postgresFencedVerificationCompletionOutcome,
		1,
	)

	go func() {
		result, completeErr :=
			NewRegistry(fixture.base.pool).
				CompleteFencedVerification(
					fixture.base.ctx,
					fixture.command,
				)

		completionResults <- postgresFencedVerificationCompletionOutcome{
			result: result,
			err:    completeErr,
		}
	}()

	waitDeadline := time.Now().Add(10 * time.Second)

	for {
		var waiting bool

		err = fixture.base.pool.QueryRow(
			fixture.base.ctx,
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
			fencedVerificationCompletionLockKey,
		).Scan(&waiting)
		if err != nil {
			t.Fatalf(
				"inspect waiting completion advisory lock: %v",
				err,
			)
		}

		if waiting {
			break
		}

		select {
		case outcome := <-completionResults:
			t.Fatalf(
				"CompleteFencedVerification() returned before "+
					"the blocking trigger: result=%#v error=%v",
				outcome.result,
				outcome.err,
			)
		default:
		}

		if time.Now().After(waitDeadline) {
			t.Fatal(
				"timed out waiting for Verification UPDATE " +
					"to reach the blocking trigger",
			)
		}

		time.Sleep(10 * time.Millisecond)
	}

	takeoverResults := make(
		chan postgresFencedVerificationTakeoverOutcome,
		1,
	)

	takeoverNow :=
		fixture.scheduled.Claim.ExpiresAt.Add(time.Second)

	go func() {
		result, found, takeoverErr :=
			NewRegistry(fixture.base.pool).
				ClaimPendingVerification(
					fixture.base.ctx,
					incidentdomain.
						ClaimPendingVerificationCommand{
						HolderID: "verification-scheduler-b",
						Now:      takeoverNow,
						LeaseDuration: 30 *
							time.Second,
					},
				)

		takeoverResults <- postgresFencedVerificationTakeoverOutcome{
			result: result,
			found:  found,
			err:    takeoverErr,
		}
	}()

	var (
		takeoverOutcome   postgresFencedVerificationTakeoverOutcome
		takeoverCompleted bool
	)

	select {
	case takeoverOutcome = <-takeoverResults:
		takeoverCompleted = true

		if takeoverOutcome.err != nil {
			t.Fatalf(
				"concurrent ClaimPendingVerification() error = %v",
				takeoverOutcome.err,
			)
		}
		if takeoverOutcome.found {
			t.Fatalf(
				"concurrent ClaimPendingVerification() result = %#v; "+
					"want found=false while completion owns the rows",
				takeoverOutcome.result,
			)
		}

	case <-time.After(100 * time.Millisecond):
		// Waiting for the locked rows is also a valid implementation.
	}

	var unlocked bool

	err = blocker.QueryRow(
		fixture.base.ctx,
		`SELECT pg_advisory_unlock($1, $2)`,
		fencedVerificationLockNamespace,
		fencedVerificationCompletionLockKey,
	).Scan(&unlocked)
	if err != nil {
		t.Fatalf(
			"release Verification completion advisory lock: %v",
			err,
		)
	}
	if !unlocked {
		t.Fatal(
			"Verification completion advisory lock was not held",
		)
	}
	lockReleased = true

	var completionOutcome postgresFencedVerificationCompletionOutcome

	select {
	case completionOutcome = <-completionResults:
	case <-time.After(10 * time.Second):
		t.Fatal(
			"timed out waiting for CompleteFencedVerification()",
		)
	}

	if completionOutcome.err != nil {
		t.Fatalf(
			"CompleteFencedVerification() error = %v",
			completionOutcome.err,
		)
	}
	if completionOutcome.result.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"completed Verification Status = %q; want %q",
			completionOutcome.result.Verification.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if completionOutcome.result.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"completed Incident State = %q; want %q",
			completionOutcome.result.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if completionOutcome.result.Incident.Version !=
		fixture.scheduled.Claim.Incident.Version {
		t.Fatalf(
			"completed Incident Version = %d; want unchanged %d",
			completionOutcome.result.Incident.Version,
			fixture.scheduled.Claim.Incident.Version,
		)
	}

	if !takeoverCompleted {
		select {
		case takeoverOutcome = <-takeoverResults:
		case <-time.After(10 * time.Second):
			t.Fatal(
				"timed out waiting for concurrent scheduler",
			)
		}

		if takeoverOutcome.err != nil {
			t.Fatalf(
				"concurrent ClaimPendingVerification() error = %v",
				takeoverOutcome.err,
			)
		}
		if takeoverOutcome.found {
			t.Fatalf(
				"concurrent ClaimPendingVerification() result = %#v; "+
					"want found=false after completion",
				takeoverOutcome.result,
			)
		}
	}

	history, err := NewRegistry(fixture.base.pool).ClaimHistory(
		fixture.base.ctx,
		fixture.scheduled.Claim.Incident.ID,
	)
	if err != nil {
		t.Fatalf("ClaimHistory() error = %v", err)
	}

	for _, event := range history {
		if event.HolderID == "verification-scheduler-b" {
			t.Fatalf(
				"ClaimHistory() contains rejected takeover audit: %#v",
				event,
			)
		}
	}
}

func TestRegistryCompleteFencedVerificationExactRetryIsIdempotentPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationCompletionFixture(
		t,
		"fenced-verification-completion-exact-retry",
	)

	first, err := NewRegistry(fixture.base.pool).
		CompleteFencedVerification(
			fixture.base.ctx,
			fixture.command,
		)
	if err != nil {
		t.Fatalf(
			"first CompleteFencedVerification() error = %v",
			err,
		)
	}

	second, err := NewRegistry(fixture.base.pool).
		CompleteFencedVerification(
			fixture.base.ctx,
			fixture.command,
		)
	if err != nil {
		t.Fatalf(
			"retry CompleteFencedVerification() error = %v",
			err,
		)
	}

	if first.Incident.Version !=
		fixture.scheduled.Claim.Incident.Version {
		t.Fatalf(
			"first Incident Version = %d; want unchanged %d",
			first.Incident.Version,
			fixture.scheduled.Claim.Incident.Version,
		)
	}
	if second.Incident.Version != first.Incident.Version {
		t.Fatalf(
			"retry Incident Version = %d; want unchanged %d",
			second.Incident.Version,
			first.Incident.Version,
		)
	}
	if first.Verification.Version !=
		fixture.scheduled.Verification.Version+1 {
		t.Fatalf(
			"first Verification Version = %d; want %d",
			first.Verification.Version,
			fixture.scheduled.Verification.Version+1,
		)
	}
	if second.Verification.Version !=
		first.Verification.Version {
		t.Fatalf(
			"retry Verification Version = %d; want unchanged %d",
			second.Verification.Version,
			first.Verification.Version,
		)
	}
	if second.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"retry Verification Status = %q; want %q",
			second.Verification.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if second.Verification.EvidenceCode !=
		first.Verification.EvidenceCode {
		t.Fatalf(
			"retry EvidenceCode = %q; want %q",
			second.Verification.EvidenceCode,
			first.Verification.EvidenceCode,
		)
	}
}

func TestRegistryCompleteFencedVerificationRejectsOldHolderAfterLeaseTakeoverPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationCompletionFixture(
		t,
		"fenced-verification-completion-old-holder",
	)

	takeoverNow := fixture.scheduled.Claim.ExpiresAt.Add(time.Second)

	takenOver, found, err := NewRegistry(fixture.base.pool).
		ClaimPendingVerification(
			fixture.base.ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID:      "verification-scheduler-b",
				Now:           takeoverNow,
				LeaseDuration: 30 * time.Second,
			},
		)
	if err != nil {
		t.Fatalf(
			"takeover ClaimPendingVerification() error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"takeover ClaimPendingVerification() found = false; want true",
		)
	}
	if takenOver.Claim.HolderID != "verification-scheduler-b" {
		t.Fatalf(
			"takeover HolderID = %q; want %q",
			takenOver.Claim.HolderID,
			"verification-scheduler-b",
		)
	}

	_, err = NewRegistry(fixture.base.pool).
		CompleteFencedVerification(
			fixture.base.ctx,
			fixture.command,
		)
	if !errors.Is(err, incidentdomain.ErrVersionConflict) {
		t.Fatalf(
			"old Holder CompleteFencedVerification() error = %v; "+
				"want ErrVersionConflict",
			err,
		)
	}

	persisted, err := NewVerificationStore(
		fixture.base.pool,
	).lookupByActionAttemptID(
		fixture.base.ctx,
		fixture.scheduled.Verification.ActionAttemptID,
	)
	if err != nil {
		t.Fatalf(
			"lookup persisted Verification error = %v",
			err,
		)
	}

	if persisted.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"persisted Verification Status = %q; want %q",
			persisted.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if persisted.Version != fixture.scheduled.Verification.Version {
		t.Fatalf(
			"persisted Verification Version = %d; want unchanged %d",
			persisted.Version,
			fixture.scheduled.Verification.Version,
		)
	}
	if persisted.FinishedAt != nil {
		t.Fatalf(
			"persisted Verification FinishedAt = %v; want nil",
			persisted.FinishedAt,
		)
	}
	if persisted.EvidenceCode != "" {
		t.Fatalf(
			"persisted Verification EvidenceCode = %q; want empty",
			persisted.EvidenceCode,
		)
	}
}
