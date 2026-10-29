package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

func TestGitOpsShadowApplicationIsManualAndExplicitlyScoped(t *testing.T) {
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
			SyncPolicy  struct {
				Automated   map[string]bool
				SyncOptions []string
			}
		}
	}
	readObservabilityManifest(t, "argocd/applications/sre-agent-v2-shadow.yaml", &application)
	if application.Metadata.Name != "sre-agent-v2-shadow" || application.Metadata.Namespace != "argocd" || len(application.Metadata.Finalizers) != 0 {
		t.Fatal("retain Application identity without adding cascading deletion")
	}
	source := application.Spec.Source
	if source.RepoURL != "https://github.com/Q-9923/sre-agent-gitops.git" || source.Path != "apps/sre-agent-v2" || source.Directory.Recurse ||
		source.Directory.Include != "{deployment.yaml,rbac.yaml,prometheusrule.yaml,grafana-dashboard.yaml,service.yaml,servicemonitor.yaml}" {
		t.Fatal("Application must render only the six reviewed files, never runtime state, Secret or NetworkPolicy")
	}
	for _, key := range []string{"enabled", "prune", "selfHeal", "allowEmpty"} {
		value, exists := application.Spec.SyncPolicy.Automated[key]
		if !exists || value {
			t.Fatalf("automated.%s must be explicitly false", key)
		}
	}
	if !reflect.DeepEqual(application.Spec.SyncPolicy.SyncOptions, []string{"CreateNamespace=true"}) {
		t.Fatal("Application sync options must not enable force or replace")
	}
	if application.Spec.Destination.Server != "https://kubernetes.default.svc" || application.Spec.Destination.Namespace != "sre-agent-system" {
		t.Fatal("observability destination must remain sre-agent-system")
	}
}

func TestGitOpsShadowDeploymentConfiguration(t *testing.T) {
	var deployment appsv1.Deployment
	readObservabilityManifest(t, "apps/sre-agent-v2/deployment.yaml", &deployment)
	if deployment.Name != "sre-agent-v2-shadow" || deployment.Namespace != "sre-agent-system" ||
		deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 ||
		deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("Shadow must retain its identity, one replica and Recreate strategy")
	}
	pod := deployment.Spec.Template.Spec
	if pod.ServiceAccountName != "sre-agent-v2" ||
		pod.NodeSelector["kubernetes.io/hostname"] != "k8s-node3" ||
		len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "ghcr-pull-secret" ||
		len(pod.Containers) != 1 {
		t.Fatal("Shadow must retain node3 and its service account, with one reviewed pull Secret")
	}
	container := pod.Containers[0]
	if container.Name != "sre-agent" ||
		container.Image != "ghcr.io/q-9923/sre-agent@sha256:97fa61a8135d6f1c04e5d1400ba5922b4fa1195d26a773a50d1ab47c8cab6080" ||
		container.ImagePullPolicy != "IfNotPresent" || len(container.EnvFrom) != 0 {
		t.Fatal("Shadow must run the verified immutable image without importing credential environment")
	}
	environment := map[string]string{}
	holder := ""
	for _, entry := range container.Env {
		environment[entry.Name] = entry.Value
		if entry.Name == "INCIDENT_CLAIM_HOLDER_ID" && entry.ValueFrom != nil && entry.ValueFrom.FieldRef != nil {
			holder = entry.ValueFrom.FieldRef.FieldPath
		}
		if entry.Name == "INCIDENT_STORE_POSTGRES_DSN" {
			t.Fatal("Memory Shadow must not receive a production PostgreSQL DSN")
		}
	}
	for name, want := range map[string]string{
		"INCIDENT_STORE_BACKEND":           "memory",
		"INCIDENT_STORE_CONNECT_TIMEOUT":   "10s",
		"INCIDENT_STORE_MIGRATION_TIMEOUT": "30s",
		"INCIDENT_CLAIM_LEASE_DURATION":    "30s",
		"RESTART_POD_APPROVED":             "false",
		"ALLOWED_NAMESPACES":               "sre-agent-lab",
		"ALLOWED_CONTROLLER_KINDS":         "ReplicaSet",
		"REMEDIATION_STATE_NAMESPACE":      "sre-agent-system",
		"REMEDIATION_STATE_CONFIGMAP":      "sre-agent-v2-state",
		"KUBERNETES_REQUEST_TIMEOUT":       "15s",
		"POLL_INTERVAL":                    "30s",
	} {
		if environment[name] != want {
			t.Errorf("Shadow %s = %q; want %q", name, environment[name], want)
		}
	}
	if holder != "metadata.uid" {
		t.Error("Claim Holder must come from the replacement Pod UID")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil ||
		!*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.RunAsUser == nil ||
		*pod.SecurityContext.RunAsUser != 65532 {
		t.Error("Shadow must retain its non-root runtime identity")
	}
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil ||
		*container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem ||
		container.SecurityContext.Capabilities == nil ||
		!reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Error("Shadow must retain its container security boundary")
	}
}

func TestGitOpsShadowRBACReadOnlyAndSufficient(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "apps", "sre-agent-v2", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	identities := map[string]bool{}
	roles := map[string]rbacv1.Role{}
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if object.Object == nil {
			continue
		}
		key := object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
		if identities[key] {
			t.Fatalf("duplicate RBAC identity: %s", key)
		}
		identities[key] = true
		switch object.GetKind() {
		case "Role":
			var role rbacv1.Role
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &role); err != nil {
				t.Fatal(err)
			}
			roles[role.Name] = role
		case "RoleBinding":
			var binding rbacv1.RoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &binding); err != nil {
				t.Fatal(err)
			}
			if binding.RoleRef != (rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: binding.Name}) ||
				!reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: "sre-agent-v2", Namespace: "sre-agent-system"}}) {
				t.Fatal("RoleBinding may grant only its named Role to the existing agent service account")
			}
		case "ServiceAccount":
		default:
			t.Fatalf("unexpected RBAC kind: %s", object.GetKind())
		}
	}
	wantIdentities := map[string]bool{
		"ServiceAccount/sre-agent-system/sre-agent-v2":           true,
		"Role/sre-agent-lab/sre-agent-v2-observer":               true,
		"RoleBinding/sre-agent-lab/sre-agent-v2-observer":        true,
		"Role/sre-agent-system/sre-agent-v2-state-writer":        true,
		"RoleBinding/sre-agent-system/sre-agent-v2-state-writer": true,
	}
	if !reflect.DeepEqual(identities, wantIdentities) {
		t.Fatalf("RBAC identities = %v; want the five existing namespaced resources", identities)
	}
	observer := roles["sre-agent-v2-observer"]
	if !reflect.DeepEqual(shadowManifestGrants(t, observer.Rules), map[string]bool{
		"/pods#get": true, "/pods#list": true, "/pods/log#get": true,
		"/events#list": true, "apps/replicasets#get": true,
	}) {
		t.Error("observer grants must be exactly lab Pod/log/event reads plus ReplicaSet get")
	}
	writer := roles["sre-agent-v2-state-writer"]
	if !reflect.DeepEqual(shadowManifestGrants(t, writer.Rules), map[string]bool{
		"/configmaps/sre-agent-v2-state#get":    true,
		"/configmaps/sre-agent-v2-state#update": true,
	}) {
		t.Error("state writer may only get/update the existing named state ConfigMap")
	}
}

func shadowManifestGrants(t *testing.T, rules []rbacv1.PolicyRule) map[string]bool {
	t.Helper()
	grants := map[string]bool{}
	for _, rule := range rules {
		if len(rule.NonResourceURLs) != 0 {
			t.Fatal("namespaced agent Role cannot grant non-resource URLs")
		}
		names := rule.ResourceNames
		if len(names) == 0 {
			names = []string{""}
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, name := range names {
					suffix := ""
					if name != "" {
						suffix = "/" + name
					}
					for _, verb := range rule.Verbs {
						grants[group+"/"+resource+suffix+"#"+verb] = true
					}
				}
			}
		}
	}
	return grants
}
