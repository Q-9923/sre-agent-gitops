package main

import (
	"context"
	"fmt"
	"net"
	"sre-agent/internal/incident"
	"testing"
	"time"
)

func TestOpenIncidentRegistryReturnsUsableMemoryRegistry(t *testing.T) {
	config := agentConfig{
		IncidentStoreBackend: "memory",
	}

	handle, err := openIncidentRegistry(
		context.Background(),
		config,
	)
	if err != nil {
		t.Fatalf(
			"openIncidentRegistry() error = %v; want nil",
			err,
		)
	}
	defer handle.Close()

	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: incident.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "example",
			UID:       "uid-example",
		},
	}

	observed, created, err := handle.Registry.Observe(
		context.Background(),
		observation,
	)
	if err != nil {
		t.Fatalf(
			"Registry.Observe() error = %v; want nil",
			err,
		)
	}
	if !created {
		t.Fatal(
			"Registry.Observe() created = false; want true",
		)
	}
	if observed.ID == "" {
		t.Fatal(
			"Registry.Observe() returned an empty incident ID",
		)
	}
}
func TestOpenIncidentRegistryAppliesConnectTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf(
			"net.Listen() error = %v; want nil",
			err,
		)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	acceptedConnections := make(chan net.Conn, 1)
	acceptErrors := make(chan error, 1)

	go func() {
		connection, err := listener.Accept()
		if err != nil {
			acceptErrors <- err
			return
		}
		acceptedConnections <- connection
	}()

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Second,
	)
	t.Cleanup(cancel)

	type openResult struct {
		handle incidentRegistryHandle
		err    error
	}

	results := make(chan openResult, 1)
	go func() {
		handle, err := openIncidentRegistry(
			ctx,
			agentConfig{
				IncidentStoreBackend: "postgres",
				IncidentStorePostgresDSN: fmt.Sprintf(
					"postgres://sre_agent@%s/sre_agent?sslmode=disable",
					listener.Addr().String(),
				),
				IncidentStoreConnectTimeout: 50 * time.Millisecond,
			},
		)
		results <- openResult{
			handle: handle,
			err:    err,
		}
	}()

	select {
	case connection := <-acceptedConnections:
		t.Cleanup(func() {
			_ = connection.Close()
		})

	case err := <-acceptErrors:
		t.Fatalf(
			"listener.Accept() error = %v; want nil",
			err,
		)

	case <-time.After(500 * time.Millisecond):
		t.Fatal(
			"PostgreSQL client did not connect to the test listener",
		)
	}

	select {
	case result := <-results:
		if result.err == nil {
			result.handle.Close()
			t.Fatal(
				"openIncidentRegistry() error = nil; want a connect timeout",
			)
		}
		if ctx.Err() != nil {
			t.Fatal(
				"openIncidentRegistry() exhausted the parent context; " +
					"want the independent connect timeout",
			)
		}

	case <-time.After(500 * time.Millisecond):
		t.Fatal(
			"openIncidentRegistry() did not apply " +
				"INCIDENT_STORE_CONNECT_TIMEOUT",
		)
	}
}
