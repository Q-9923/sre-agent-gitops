package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunApplicationServesLivenessAndStopsWithContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})

	agentStarted := make(chan struct{})
	agentStopped := make(chan struct{})

	runAgent := func(ctx context.Context) {
		close(agentStarted)
		<-ctx.Done()
		close(agentStopped)
	}

	applicationDone := make(chan error, 1)
	go func() {
		applicationDone <- runApplication(ctx, listener, runAgent)
	}()

	select {
	case <-agentStarted:
	case <-time.After(time.Second):
		t.Fatal("agent did not start within one second")
	}

	client := &http.Client{Timeout: time.Second}
	endpoint := "http://" + listener.Addr().String() + "/livez"
	deadline := time.Now().Add(time.Second)

	var response *http.Response
	for {
		response, err = client.Get(endpoint)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s failed: %v", endpoint, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read liveness response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want %d", response.StatusCode, http.StatusOK)
	}
	if string(body) != "ok\n" {
		t.Fatalf("body = %q; want %q", string(body), "ok\n")
	}

	cancel()

	select {
	case <-agentStopped:
	case <-time.After(time.Second):
		t.Fatal("agent did not stop after context cancellation")
	}

	select {
	case err := <-applicationDone:
		if err != nil {
			t.Fatalf("runApplication() error = %v; want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runApplication() did not stop after context cancellation")
	}
}
