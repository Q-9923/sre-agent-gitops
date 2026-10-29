package postgres

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

type verificationClaimRowStub struct {
	values []any
	err    error
}

func (row verificationClaimRowStub) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(destinations) != len(row.values) {
		return fmt.Errorf(
			"scan destinations = %d; want %d",
			len(destinations),
			len(row.values),
		)
	}

	for index, value := range row.values {
		destination := reflect.ValueOf(destinations[index])
		if destination.Kind() != reflect.Pointer || destination.IsNil() {
			return fmt.Errorf("scan destination %d is not a pointer", index)
		}

		target := destination.Elem()
		if value == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}

		source := reflect.ValueOf(value)
		if !source.Type().AssignableTo(target.Type()) {
			return fmt.Errorf(
				"scan value %d type %s is not assignable to %s",
				index,
				source.Type(),
				target.Type(),
			)
		}
		target.Set(source)
	}

	return nil
}

func TestScanVerificationClaimRowRestoresPendingAndRecoveredClaims(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(2026, time.October, 13, 9, 0, 0, 0, time.UTC)
	finishedAt := now.Add(time.Second)

	tests := []struct {
		name              string
		status            remediationdomain.VerificationStatus
		includeResolution bool
		resolutionID      string
	}{
		{
			name:   "pending",
			status: remediationdomain.VerificationStatusPending,
		},
		{
			name:              "recovered",
			status:            remediationdomain.VerificationStatusRecovered,
			includeResolution: true,
			resolutionID:      "",
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			values := []any{
				"ver-1",
				"attempt-1",
				"incident-1",
				"plan-hash",
				"original-pod-uid",
				"dev",
				"default",
				"ReplicaSet",
				"crash-app-rs",
				"replicaset-uid",
				string(test.status),
				int64(3),
				now,
				&finishedAt,
				"REPLICASET_AVAILABLE",
				"incident-1",
				string(incidentdomain.StateVerifying),
				int64(7),
			}
			if test.includeResolution {
				values = append(values, test.resolutionID)
			}
			values = append(values, "worker-1", now.Add(time.Minute))

			var verification remediationdomain.Verification
			var claim incidentdomain.Claim
			found, err := scanVerificationClaimRow(
				verificationClaimRowStub{values: values},
				&verification,
				&claim,
				test.status,
				test.includeResolution,
				"claim test Verification",
			)
			if err != nil {
				t.Fatalf("scanVerificationClaimRow() error = %v", err)
			}
			if !found {
				t.Fatal("scanVerificationClaimRow() found = false; want true")
			}
			if verification.ID != "ver-1" ||
				verification.Status != test.status ||
				verification.Version != 3 ||
				verification.Subject.UID != "replicaset-uid" {
				t.Fatalf("Verification = %#v; want restored row", verification)
			}
			if claim.Incident.ID != "incident-1" ||
				claim.Incident.State != incidentdomain.StateVerifying ||
				claim.Incident.Version != 7 ||
				claim.HolderID != "worker-1" {
				t.Fatalf("Claim = %#v; want restored row", claim)
			}
		})
	}
}

func TestScanVerificationClaimRowHandlesNoRowsAndRejectsInvalidState(
	t *testing.T,
) {
	t.Parallel()

	var verification remediationdomain.Verification
	var claim incidentdomain.Claim

	found, err := scanVerificationClaimRow(
		verificationClaimRowStub{err: pgx.ErrNoRows},
		&verification,
		&claim,
		remediationdomain.VerificationStatusPending,
		false,
		"claim pending Verification",
	)
	if err != nil || found {
		t.Fatalf(
			"scanVerificationClaimRow(ErrNoRows) = found:%t error:%v; want false,nil",
			found,
			err,
		)
	}

	target := verificationClaimScanTarget{
		verification:        &verification,
		claim:               &claim,
		verificationStatus:  string(remediationdomain.VerificationStatusPending),
		verificationVersion: 1,
		incidentState:       string(incidentdomain.StateResolved),
		incidentVersion:     1,
	}
	if err := target.restoreAndValidate(
		remediationdomain.VerificationStatusPending,
		"claim pending Verification",
	); err == nil {
		t.Fatal("restoreAndValidate() error = nil; want invalid Incident state")
	}

	queryErr := errors.New("query failed")
	_, err = scanVerificationClaimRow(
		verificationClaimRowStub{err: queryErr},
		&verification,
		&claim,
		remediationdomain.VerificationStatusPending,
		false,
		"claim pending Verification",
	)
	if !errors.Is(err, queryErr) {
		t.Fatalf("scanVerificationClaimRow() error = %v; want %v", err, queryErr)
	}
}
