package main

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestExecuteApprovedActionDeletesOnlyTheObservedPodUID(t *testing.T) {
	client := fake.NewSimpleClientset(&v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "crash-app-test", Namespace: "default", UID: types.UID("uid-policy-test"),
	}})
	decision := policyTestDecision(ActionRestartPod)

	if err := executeApprovedAction(context.Background(), client, decision); err != nil {
		t.Fatalf("executeApprovedAction() error = %v; want nil", err)
	}

	actions := client.Actions()
	if len(actions) != 1 {
		t.Fatalf("len(client.Actions()) = %d; want 1", len(actions))
	}
	deleteAction, ok := actions[0].(k8stesting.DeleteAction)
	if !ok {
		t.Fatalf("action type = %T; want DeleteAction", actions[0])
	}
	options := deleteAction.GetDeleteOptions()
	if options.Preconditions == nil || options.Preconditions.UID == nil {
		t.Fatal("delete request has no UID precondition")
	}
	if got := string(*options.Preconditions.UID); got != decision.Target.UID {
		t.Fatalf("delete UID precondition = %q; want %q", got, decision.Target.UID)
	}
}
