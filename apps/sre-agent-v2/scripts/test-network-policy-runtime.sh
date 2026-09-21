#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(
  cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &&
    pwd
)
APP_DIR=$(
  cd -- "${SCRIPT_DIR}/.." &&
    pwd
)

MANIFEST="${APP_DIR}/networkpolicy.yaml"
TEMP_ROOT="${APP_DIR}/.tmp"
TEST_NAMESPACE="sre-agent-system"
CANARY_POLICY="sre-agent-v2-network-policy-canary"
CANARY_POD="sre-agent-network-policy-canary"
DENIED_SERVER_POD="sre-agent-network-policy-denied-server"
CANARY_LABEL="sre-agent.openai.com/network-policy-canary"

if [[ ! -f "${MANIFEST}" ]]; then
  echo "ERROR: missing networkpolicy.yaml" >&2
  exit 1
fi

for resource in \
  "networkpolicy/${CANARY_POLICY}" \
  "pod/${CANARY_POD}" \
  "pod/${DENIED_SERVER_POD}"
do
  if kubectl -n "${TEST_NAMESPACE}" get "${resource}" >/dev/null 2>&1; then
    echo "ERROR: temporary resource already exists: ${resource}" >&2
    exit 1
  fi
done

mkdir -p "${TEMP_ROOT}"
TEMP_DIR=$(
  mktemp -d "${TEMP_ROOT}/network-policy-runtime.XXXXXX"
)
CANARY_POLICY_FILE="${TEMP_DIR}/networkpolicy.json"

cleanup() {
  kubectl -n "${TEST_NAMESPACE}" delete pod \
    "${CANARY_POD}" \
    "${DENIED_SERVER_POD}" \
    --ignore-not-found \
    --wait=false >/dev/null 2>&1 || true

  kubectl -n "${TEST_NAMESPACE}" delete networkpolicy \
    "${CANARY_POLICY}" \
    --ignore-not-found \
    --wait=false >/dev/null 2>&1 || true

  rm -f -- "${CANARY_POLICY_FILE}"
  rmdir -- "${TEMP_DIR}" 2>/dev/null || true
}
trap cleanup EXIT

kubectl create \
  --dry-run=client \
  -f "${MANIFEST}" \
  -o json |
jq \
  --arg policyName "${CANARY_POLICY}" \
  --arg labelName "${CANARY_LABEL}" \
  '
    .metadata.name = $policyName
    | .spec.podSelector.matchLabels = {
        ($labelName): "true"
      }
  ' >"${CANARY_POLICY_FILE}"

kubectl apply -f "${CANARY_POLICY_FILE}"

kubectl -n "${TEST_NAMESPACE}" run "${DENIED_SERVER_POD}" \
  --image=busybox:1.36.1 \
  --restart=Never \
  --labels='sre-agent.openai.com/network-policy-test=denied-server' \
  --command -- \
  sh -c 'exec httpd -f -p 18080'

kubectl -n "${TEST_NAMESPACE}" wait \
  --for=condition=Ready \
  "pod/${DENIED_SERVER_POD}" \
  --timeout=60s

kubectl -n "${TEST_NAMESPACE}" exec "${DENIED_SERVER_POD}" -- \
  nc -vz -w 3 127.0.0.1 18080

kubectl -n "${TEST_NAMESPACE}" run "${CANARY_POD}" \
  --image=busybox:1.36.1 \
  --restart=Never \
  --labels="${CANARY_LABEL}=true" \
  --command -- \
  sh -c 'exec httpd -f -p 8080'

kubectl -n "${TEST_NAMESPACE}" wait \
  --for=condition=Ready \
  "pod/${CANARY_POD}" \
  --timeout=60s

sleep 3

check_allowed() {
  local description=$1
  shift

  for attempt in $(seq 1 10); do
    if kubectl -n "${TEST_NAMESPACE}" exec "${CANARY_POD}" -- \
      "$@" >/dev/null 2>&1; then
      printf 'allowed=%s attempt=%s\n' "${description}" "${attempt}"
      return 0
    fi
    sleep 2
  done

  echo "ERROR: expected connection was denied: ${description}" >&2
  return 1
}

check_allowed \
  dns \
  nslookup kubernetes.default.svc.cluster.local

check_allowed \
  kubernetes-service \
  nc -vz -w 5 10.96.0.1 443

check_allowed \
  kubernetes-endpoint \
  nc -vz -w 5 192.168.30.11 6443

check_allowed \
  prometheus \
  nc -vz -w 5 \
  prometheus-stack-kube-prom-prometheus.monitoring.svc.cluster.local \
  9090

check_allowed \
  ollama \
  nc -vz -w 5 \
  ollama-service.ai-services.svc.cluster.local \
  11434

DENIED_SERVER_IP=$(
  kubectl -n "${TEST_NAMESPACE}" get pod "${DENIED_SERVER_POD}" \
    -o jsonpath='{.status.podIP}'
)

set +e
kubectl -n "${TEST_NAMESPACE}" exec "${CANARY_POD}" -- \
  nc -vz -w 3 "${DENIED_SERVER_IP}" 18080 >/dev/null 2>&1
DENIED_EXIT=$?
set -e

if [[ "${DENIED_EXIT}" -eq 0 ]]; then
  echo "ERROR: unrelated in-cluster egress was allowed" >&2
  exit 1
fi

printf 'denied=unrelated-in-cluster-egress\n'
echo "NETWORK_POLICY_RUNTIME_CANARY_VERIFIED"
