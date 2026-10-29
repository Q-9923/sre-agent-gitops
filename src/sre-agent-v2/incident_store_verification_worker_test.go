package main

import (
	"context"
	"testing"
)

func TestOpenIncidentRegistryMemoryProvidesVerificationWorker(
	t *testing.T,
) {
	t.Parallel()

	handle, err := openIncidentRegistry(
		context.Background(),
		agentConfig{
			IncidentStoreBackend: "memory",
		},
	)
	if err != nil {
		t.Fatalf(
			"openIncidentRegistry(memory) error = %v",
			err,
		)
	}
	defer handle.Close()

	if handle.VerificationWorker == nil {
		t.Fatal(
			"incident Registry handle VerificationWorker = nil; " +
				"want configured worker lifecycle",
		)
	}
}
