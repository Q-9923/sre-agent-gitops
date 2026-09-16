package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type agentMetrics struct {
	cyclesTotal *prometheus.CounterVec
	httpHandler http.Handler
}

func newAgentMetrics() *agentMetrics {
	registry := prometheus.NewRegistry()

	cyclesTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "sre_agent",
			Name:      "cycles_total",
			Help:      "Total number of SRE Agent processing cycles by result.",
		},
		[]string{"result"},
	)

	registry.MustRegister(
		collectors.NewGoCollector(),
		cyclesTotal,
	)

	return &agentMetrics{
		cyclesTotal: cyclesTotal,
		httpHandler: promhttp.HandlerFor(
			registry,
			promhttp.HandlerOpts{},
		),
	}
}

func (metrics *agentMetrics) recordCycle(result string) {
	switch result {
	case "NO_ACTION", "PROCESSED", "ERROR":
	default:
		result = "UNKNOWN"
	}

	metrics.cyclesTotal.
		WithLabelValues(result).
		Inc()
}

func (metrics *agentMetrics) handler() http.Handler {
	return metrics.httpHandler
}
