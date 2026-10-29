package main

import (
	"log/slog"
	"testing"
)

func TestNewSREAgentWiresVerificationWorkerFromIncidentHandle(
	t *testing.T,
) {
	t.Parallel()

	worker := &agentVerificationWorkerRunLifecycleProbe{}

	agent := newSREAgent(
		agentConfig{},
		nil,
		incidentRegistryHandle{
			VerificationWorker: worker,
		},
		slog.Default(),
		func() {},
		func() {},
		func(string) {},
	)

	if agent.verificationWorker != worker {
		t.Fatalf(
			"newSREAgent() Verification Worker = %T; want %T",
			agent.verificationWorker,
			worker,
		)
	}
}
