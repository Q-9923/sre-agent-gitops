package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
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
		applicationDone <- runApplication(ctx, listener, runAgent, func() bool {
			return true
		}, func() bool {
			return false
		})
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

func TestRunApplicationReflectsReadinessChanges(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})

	var ready atomic.Bool

	applicationDone := make(chan error, 1)
	go func() {
		applicationDone <- runApplication(
			ctx,
			listener,
			func(agentContext context.Context) {
				<-agentContext.Done()
			},
			func() bool {
				return true
			},
			ready.Load,
		)
	}()

	client := &http.Client{Timeout: time.Second}
	endpoint := "http://" + listener.Addr().String() + "/readyz"

	getReadiness := func() (int, string) {
		t.Helper()

		deadline := time.Now().Add(time.Second)
		for {
			response, requestErr := client.Get(endpoint)
			if requestErr == nil {
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil {
					t.Fatalf("read readiness response: %v", readErr)
				}
				return response.StatusCode, string(body)
			}

			if time.Now().After(deadline) {
				t.Fatalf("GET %s failed: %v", endpoint, requestErr)
			}

			time.Sleep(10 * time.Millisecond)
		}
	}

	status, body := getReadiness()
	if status != http.StatusServiceUnavailable {
		t.Fatalf(
			"initial status = %d; want %d",
			status,
			http.StatusServiceUnavailable,
		)
	}
	if body != "not ready\n" {
		t.Fatalf("initial body = %q; want %q", body, "not ready\n")
	}

	ready.Store(true)

	status, body = getReadiness()
	if status != http.StatusOK {
		t.Fatalf("ready status = %d; want %d", status, http.StatusOK)
	}
	if body != "ok\n" {
		t.Fatalf("ready body = %q; want %q", body, "ok\n")
	}

	cancel()

	select {
	case err := <-applicationDone:
		if err != nil {
			t.Fatalf("runApplication() error = %v; want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runApplication() did not stop after cancellation")
	}
}

func TestRunApplicationReflectsLivenessChanges(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var live atomic.Bool
	live.Store(true)

	applicationDone := make(chan error, 1)
	go func() {
		applicationDone <- runApplication(
			ctx,
			listener,
			func(agentContext context.Context) {
				<-agentContext.Done()
			},
			live.Load,
			func() bool {
				return true
			},
		)
	}()

	client := &http.Client{
		Timeout: time.Second,
	}
	livezURL := "http://" + listener.Addr().String() + "/livez"

	response, err := client.Get(livezURL)
	if err != nil {
		t.Fatalf("GET live /livez error = %v", err)
	}
	_ = response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf(
			"live status = %d; want %d",
			response.StatusCode,
			http.StatusOK,
		)
	}

	live.Store(false)

	response, err = client.Get(livezURL)
	if err != nil {
		t.Fatalf("GET stale /livez error = %v", err)
	}
	_ = response.Body.Close()

	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf(
			"stale status = %d; want %d",
			response.StatusCode,
			http.StatusServiceUnavailable,
		)
	}

	cancel()

	select {
	case err := <-applicationDone:
		if err != nil {
			t.Fatalf("runApplication() error = %v; want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runApplication() did not stop after cancellation")
	}
}
