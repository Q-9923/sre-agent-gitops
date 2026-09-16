package main

import "net/http"

func newOperationalHandler(
	isLive func() bool,
	isReady func() bool,
	metricsHandler http.Handler,
) http.Handler {
	healthHandler := newHealthHandler(isLive, isReady)

	mux := http.NewServeMux()
	mux.Handle("/livez", healthHandler)
	mux.Handle("/readyz", healthHandler)
	mux.Handle("/metrics", metricsHandler)

	return mux
}
