package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func readObservabilityManifest(t *testing.T, relative string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	data, err = yaml.YAMLToJSONStrict(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func TestGitOpsFormalCrashLoopRuleIsPermanentAndLabScoped(t *testing.T) {
	var resource struct {
		Kind     string
		Metadata struct {
			Name, Namespace string
			Labels          map[string]string
		}
		Spec struct {
			Groups []struct {
				Rules []struct {
					Alert, Expr, For string
					Labels           map[string]string
				}
			}
		}
	}
	readObservabilityManifest(t, "apps/sre-agent-v2/prometheusrule.yaml", &resource)
	if resource.Kind != "PrometheusRule" || resource.Metadata.Name != "sre-agent-v2" ||
		resource.Metadata.Namespace != "sre-agent-system" || resource.Metadata.Labels["release"] != "prometheus-stack" {
		t.Fatal("formal rules must retain the existing monitored resource identity")
	}
	counts := map[string]int{}
	for _, group := range resource.Spec.Groups {
		for _, rule := range group.Rules {
			counts[rule.Alert]++
			if rule.Alert == "PodCrashLooping" {
				if strings.TrimSpace(rule.Expr) != `kube_pod_container_status_waiting_reason{namespace="sre-agent-lab",reason="CrashLoopBackOff"} == 1` ||
					rule.For != "1m" || rule.Labels["severity"] != "critical" {
					t.Fatal("formal CrashLoop rule must cover all lab Pods only, retain one-minute hold and critical severity")
				}
			}
		}
	}
	if len(counts) != 3 || counts["PodCrashLooping"] != 1 || counts["SREAgentMetricsTargetDown"] != 1 ||
		counts["SREAgentCycleErrorsIncreasing"] != 1 {
		t.Fatalf("want the permanent lab alert and both existing self-monitoring rules, got %v", counts)
	}
}

func TestGitOpsObservabilityApplicationIsManualAndCannotRenderWorkloads(t *testing.T) {
	var application struct {
		Metadata struct {
			Name, Namespace string
			Finalizers      []string
		}
		Spec struct {
			Source struct {
				RepoURL, Path string
				Directory     struct {
					Recurse bool
					Include string
				}
			}
			Destination struct{ Server, Namespace string }
			SyncPolicy  struct{ Automated map[string]bool }
		}
	}
	readObservabilityManifest(t, "argocd/applications/sre-agent-v2-shadow.yaml", &application)
	if application.Metadata.Name != "sre-agent-v2-shadow" || application.Metadata.Namespace != "argocd" || len(application.Metadata.Finalizers) != 0 {
		t.Fatal("retain Application identity without adding cascading deletion")
	}
	source := application.Spec.Source
	if source.RepoURL != "https://github.com/Q-9923/sre-agent-gitops.git" || source.Path != "apps/sre-agent-v2" || source.Directory.Recurse ||
		source.Directory.Include != "{prometheusrule.yaml,grafana-dashboard.yaml,service.yaml,servicemonitor.yaml}" {
		t.Fatal("Application must render exactly the four existing observability files, not Deployment/RBAC/state")
	}
	for _, key := range []string{"enabled", "prune", "selfHeal", "allowEmpty"} {
		value, exists := application.Spec.SyncPolicy.Automated[key]
		if !exists || value {
			t.Fatalf("automated.%s must be explicitly false", key)
		}
	}
	if application.Spec.Destination.Server != "https://kubernetes.default.svc" || application.Spec.Destination.Namespace != "sre-agent-system" {
		t.Fatal("observability destination must remain sre-agent-system")
	}
}
