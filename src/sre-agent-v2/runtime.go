package main

import (
	"context"
	"net"
)

func runApplication(
	ctx context.Context,
	listener net.Listener,
	runAgent func(context.Context),
) error {
	runtimeContext, cancel := context.WithCancel(ctx)
	defer cancel()

	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		runAgent(runtimeContext)
	}()

	healthDone := make(chan error, 1)
	go func() {
		healthDone <- runHealthServer(runtimeContext, listener)
	}()

	select {
	case <-agentDone:
		cancel()
		return <-healthDone

	case err := <-healthDone:
		cancel()
		<-agentDone
		return err
	}
}
