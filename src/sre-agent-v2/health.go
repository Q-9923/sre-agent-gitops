package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const healthShutdownTimeout = 5 * time.Second

func newHealthHandler(
	isLive func() bool,
	isReady func() bool,
) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/livez", func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		response.Header().Set(
			"Content-Type",
			"text/plain; charset=utf-8",
		)

		if !isLive() {
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte("not live\n"))
			return
		}

		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/readyz", func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		response.Header().Set(
			"Content-Type",
			"text/plain; charset=utf-8",
		)

		if !isReady() {
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte("not ready\n"))
			return
		}

		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
	})

	return mux
}

func runHealthServer(ctx context.Context, listener net.Listener, isLive func() bool, isReady func() bool) error {
	server := &http.Server{
		Handler:           newHealthHandler(isLive, isReady),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Serve(listener)
	}()

	select {
	case err := <-serverDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve health endpoint: %w", err)

	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(
			context.Background(),
			healthShutdownTimeout,
		)
		defer cancel()

		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down health endpoint: %w", err)
		}

		err := <-serverDone
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve health endpoint during shutdown: %w", err)
		}
		return nil
	}
}
