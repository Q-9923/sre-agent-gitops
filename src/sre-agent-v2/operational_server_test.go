package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunHealthServerServesProvidedHandler(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})

	handler := http.HandlerFunc(func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		response.WriteHeader(http.StatusNoContent)
	})

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- runHealthServer(
			ctx,
			listener,
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

	if response.StatusCode != http.StatusNoContent {
		t.Fatalf(
			"status = %d; want %d",
			response.StatusCode,
			http.StatusNoContent,
		)
	}

	cancel()

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf(
				"runHealthServer() error = %v; want nil",
				err,
			)
		}
	case <-time.After(time.Second):
		t.Fatal(
			"runHealthServer() did not stop after cancellation",
		)
	}
}
