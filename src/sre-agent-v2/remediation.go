package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// executeApprovedAction is deliberately narrow: its caller must first receive
// an allowed PolicyResult from EvaluateDecision.
func executeApprovedAction(ctx context.Context, client kubernetes.Interface, decision Decision) error {
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("refuse invalid decision: %w", err)
	}
	if decision.Action != ActionRestartPod {
		return fmt.Errorf("refuse unsupported executable action %q", decision.Action)
	}

	deleteOptions := metav1.NewPreconditionDeleteOptions(decision.Target.UID)
	if err := client.CoreV1().Pods(decision.Target.Namespace).Delete(
		ctx,
		decision.Target.Name,
		*deleteOptions,
	); err != nil {
		return fmt.Errorf("delete observed pod %s/%s: %w", decision.Target.Namespace, decision.Target.Name, err)
	}
	return nil
}
