package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With(
		"component", "sre-agent",
	)

	config, err := loadConfig()
	if err != nil {
		logger.Error("configuration_invalid", "result", "FATAL", "error_code", "CONFIG_INVALID", "error", err)
		os.Exit(1)
	}
	kubernetesClient, err := getKubernetesClient()
	if err != nil {
		logger.Error("kubernetes_client_failed", "result", "FATAL", "error_code", "KUBERNETES_CLIENT_FAILED", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info(
		"agent_started",
		"result", "RUNNING",
		"cluster", config.ClusterName,
		"model", config.OllamaModel,
		"restart_pod_approved", config.RestartPodApproved,
	)
	newSREAgent(config, kubernetesClient, logger).run(ctx)
}
