package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunApplicationServesProvidedHandler(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})

	runAgent := func(agentContext context.Context) {
		<-agentContext.Done()
	}

	handler := http.HandlerFunc(func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		response.WriteHeader(http.StatusAccepted)
	})

	applicationDone := make(chan error, 1)
	go func() {
		applicationDone <- runApplication(
			ctx,
			listener,
			runAgent,
			handler,
		)
	}()

	client := &http.Client{
		Timeout: time.Second,
	}
	endpoint := "http://" + listener.Addr().String() + "/metrics"

	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusAccepted {
		t.Fatalf(
			"status = %d; want %d",
			response.StatusCode,
			http.StatusAccepted,
		)
	}

	cancel()

	select {
	case err := <-applicationDone:
		if err != nil {
			t.Fatalf(
				"runApplication() error = %v; want nil",
				err,
			)
		}
	case <-time.After(time.Second):
		t.Fatal(
			"runApplication() did not stop after cancellation",
		)
	}
}
