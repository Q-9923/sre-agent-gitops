package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthHandlerReturnsOKForLiveness(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	response := httptest.NewRecorder()

	newHealthHandler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"GET /livez status = %d; want %d",
			response.Code,
			http.StatusOK,
		)
	}

	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Fatalf(
			"GET /livez Content-Type = %q; want %q",
			contentType,
			"text/plain; charset=utf-8",
		)
	}

	if body := response.Body.String(); body != "ok\n" {
		t.Fatalf(
			"GET /livez body = %q; want %q",
			body,
			"ok\n",
		)
	}
}

func TestHealthServerServesLivenessAndStopsWithContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on random local port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- runHealthServer(ctx, listener)
	}()

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
			t.Fatalf("GET /livez did not become available: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read GET /livez response: %v", err)
	}

	if response.StatusCode != http.StatusOK {
		t.Fatalf(
			"GET /livez status = %d; want %d",
			response.StatusCode,
			http.StatusOK,
		)
	}

	if string(body) != "ok\n" {
		t.Fatalf("GET /livez body = %q; want %q", body, "ok\n")
	}

	cancel()

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("runHealthServer() error = %v; want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runHealthServer() did not stop after context cancellation")
	}
}
