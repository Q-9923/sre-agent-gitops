package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(
		slog.NewJSONHandler(
			os.Stdout,
			&slog.HandlerOptions{Level: slog.LevelInfo},
		),
	).With("component", "sre-agent")
	config, err := loadConfig()
	if err != nil {
		logger.Error(
			"configuration_invalid",
			"result", "FATAL",
			"error_code", "CONFIG_INVALID",
			"error", err,
		)
		os.Exit(1)
	}

	kubernetesClient, err := getKubernetesClient()
	if err != nil {
		logger.Error(
			"kubernetes_client_failed",
			"result", "FATAL",
			"error_code", "KUBERNETES_CLIENT_FAILED",
			"error", err,
		)
		os.Exit(1)
	}

	healthListener, err := net.Listen("tcp", config.HealthAddress)
	if err != nil {
		logger.Error(
			"health_listener_failed",
			"result", "FATAL",
			"error_code", "HEALTH_LISTENER_FAILED",
			"health_address", config.HealthAddress,
			"error", err,
		)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	logger.Info(
		"agent_started",
		"result", "RUNNING",
		"cluster", config.ClusterName,
		"model", config.OllamaModel,
		"health_address", config.HealthAddress,
		"restart_pod_approved", config.RestartPodApproved,
	)
	liveness := newLivenessState(
		config.LivenessStaleAfter,
		time.Now,
	)
	readiness := newReadinessState(
		config.ReadinessStaleAfter,
		time.Now,
	)

	agent := newSREAgent(
		config,
		kubernetesClient,
		logger,
		liveness.markProgress,
		readiness.markSuccessfulCycle,
	)

	if err := runApplication(
		ctx,
		healthListener,
		agent.run,
		liveness.isLive,
		readiness.isReady,
	); err != nil {
		logger.Error(
			"application_failed",
			"result", "FATAL",
			"error_code", "APPLICATION_FAILED",
			"error", err,
		)
		os.Exit(1)
	}
}
