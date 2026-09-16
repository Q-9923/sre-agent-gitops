package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthHandlerReturnsOKForLiveness(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	response := httptest.NewRecorder()

	newHealthHandler(func() bool {
		return true
	}, func() bool {
		return false
	}).ServeHTTP(response, request)

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
		serverDone <- runHealthServer(ctx, listener, newHealthHandler(
			func() bool {
				return true
			},
			func() bool {
				return false
			}),
		)
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

func TestHealthHandlerReturnsServiceUnavailableBeforeReady(t *testing.T) {
	handler := newHealthHandler(func() bool {
		return true
	}, func() bool {
		return false
	})

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"status = %d; want %d",
			response.Code,
			http.StatusServiceUnavailable,
		)
	}

	const expectedContentType = "text/plain; charset=utf-8"
	if contentType := response.Header().Get("Content-Type"); contentType != expectedContentType {
		t.Fatalf(
			"Content-Type = %q; want %q",
			contentType,
			expectedContentType,
		)
	}

	const expectedBody = "not ready\n"
	if body := response.Body.String(); body != expectedBody {
		t.Fatalf("body = %q; want %q", body, expectedBody)
	}
}

func TestHealthHandlerReturnsOKWhenReady(t *testing.T) {
	handler := newHealthHandler(func() bool {
		return true
	}, func() bool {
		return true
	})

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d; want %d",
			response.Code,
			http.StatusOK,
		)
	}

	const expectedContentType = "text/plain; charset=utf-8"
	if contentType := response.Header().Get("Content-Type"); contentType != expectedContentType {
		t.Fatalf(
			"Content-Type = %q; want %q",
			contentType,
			expectedContentType,
		)
	}

	const expectedBody = "ok\n"
	if body := response.Body.String(); body != expectedBody {
		t.Fatalf("body = %q; want %q", body, expectedBody)
	}
}

func TestHealthServerReflectsReadinessChanges(t *testing.T) {
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

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- runHealthServer(
			ctx,
			listener,
			newHealthHandler(
				func() bool {
					return true
				},
				ready.Load,
			),
		)
	}()

	client := &http.Client{Timeout: time.Second}
	endpoint := "http://" + listener.Addr().String() + "/readyz"

	getReadiness := func() (int, string) {
		t.Helper()

		deadline := time.Now().Add(time.Second)
		for {
			response, err := client.Get(endpoint)
			if err == nil {
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil {
					t.Fatalf("read readiness response: %v", readErr)
				}
				return response.StatusCode, string(body)
			}

			if time.Now().After(deadline) {
				t.Fatalf("GET %s failed: %v", endpoint, err)
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
		t.Fatalf(
			"ready status = %d; want %d",
			status,
			http.StatusOK,
		)
	}
	if body != "ok\n" {
		t.Fatalf("ready body = %q; want %q", body, "ok\n")
	}

	cancel()

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("runHealthServer() error = %v; want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runHealthServer() did not stop after cancellation")
	}
}

func TestHealthHandlerReturnsServiceUnavailableWhenNotLive(t *testing.T) {
	handler := newHealthHandler(
		func() bool {
			return false
		},
		func() bool {
			return true
		},
	)

	request := httptest.NewRequest(
		http.MethodGet,
		"/livez",
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"status = %d; want %d",
			response.Code,
			http.StatusServiceUnavailable,
		)
	}

	if response.Body.String() != "not live\n" {
		t.Fatalf(
			"body = %q; want %q",
			response.Body.String(),
			"not live\n",
		)
	}
}

func TestHealthServerReflectsLivenessChanges(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var live atomic.Bool
	live.Store(true)

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- runHealthServer(
			ctx,
			listener,
			newHealthHandler(
				live.Load,
				func() bool {
					return true
				},
			),
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
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("runHealthServer() error = %v; want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runHealthServer() did not stop after cancellation")
	}
}
