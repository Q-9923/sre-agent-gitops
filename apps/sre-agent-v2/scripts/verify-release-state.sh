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
REPO_ROOT=$(
  cd -- "${APP_DIR}/../.." &&
    pwd
)

AGENT_NAMESPACE="sre-agent-system"
ARGO_NAMESPACE="argocd"
APPLICATION="sre-agent-v2-shadow"
DEPLOYMENT="sre-agent-v2-shadow"
STATE_CONFIGMAP="sre-agent-v2-state"
METRICS_SERVICE="sre-agent-v2-metrics"
PROMETHEUS_NAMESPACE="monitoring"
PROMETHEUS_SERVICE="prometheus-stack-kube-prom-prometheus"

REVISION=""
VERSION=""
IMAGE=""
STATE_UID=""
OPERATIONAL_PORT="${OPERATIONAL_PORT:-18097}"
PROMETHEUS_PORT="${PROMETHEUS_PORT:-19097}"
OPERATIONAL_PID=""
PROMETHEUS_PID=""
TEMP_DIR=""

usage() {
  echo 'Usage: verify-release-state.sh --revision <git-sha> --version <version> --image <tag@sha256> --state-uid <uid>'
  echo 'Mode: read-only; does not modify Git or Kubernetes resources'
}

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

cleanup() {
  if [[ -n "${OPERATIONAL_PID}" ]]; then
    kill "${OPERATIONAL_PID}" 2>/dev/null || true
    wait "${OPERATIONAL_PID}" 2>/dev/null || true
  fi

  if [[ -n "${PROMETHEUS_PID}" ]]; then
    kill "${PROMETHEUS_PID}" 2>/dev/null || true
    wait "${PROMETHEUS_PID}" 2>/dev/null || true
  fi

  if [[ -n "${TEMP_DIR}" ]]; then
    rm -f -- \
      "${TEMP_DIR}/operational.log" \
      "${TEMP_DIR}/prometheus.log"
    rmdir -- "${TEMP_DIR}" 2>/dev/null || true
  fi
}

require_equal() {
  local actual="$1"
  local expected="$2"
  local label="$3"

  if [[ "${actual}" != "${expected}" ]]; then
    fail "${label}=${actual}; want ${expected}"
  fi
}

if [[ "${1:-}" == "--help" ]] && [[ "$#" -eq 1 ]]; then
  usage
  exit 0
fi

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --revision)
      [[ "$#" -ge 2 ]] || fail "--revision requires a value"
      REVISION="$2"
      shift 2
      ;;
    --version)
      [[ "$#" -ge 2 ]] || fail "--version requires a value"
      VERSION="$2"
      shift 2
      ;;
    --image)
      [[ "$#" -ge 2 ]] || fail "--image requires a value"
      IMAGE="$2"
      shift 2
      ;;
    --state-uid)
      [[ "$#" -ge 2 ]] || fail "--state-uid requires a value"
      STATE_UID="$2"
      shift 2
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ "${REVISION}" =~ ^[0-9a-f]{40}$ ]] ||
  fail "revision must be a full 40-character Git SHA"

[[ -n "${VERSION}" ]] ||
  fail "version must not be empty"

[[ "${IMAGE}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]] ||
  fail "image must use tag@sha256:<64 hex characters>"

[[ -n "${STATE_UID}" ]] ||
  fail "state UID must not be empty"

for command_name in git kubectl jq curl; do
  command -v "${command_name}" >/dev/null ||
    fail "missing required command: ${command_name}"
done

cd "${REPO_ROOT}"

HEAD_REVISION=$(git rev-parse HEAD)
TRACKING_REVISION=$(git rev-parse origin/main)

require_equal "${HEAD_REVISION}" "${REVISION}" "HEAD revision"
require_equal "${TRACKING_REVISION}" "${REVISION}" "origin/main revision"

MANIFEST_JSON=$(
  kubectl create \
    --dry-run=client \
    -f "${APP_DIR}/deployment.yaml" \
    -o json
)

if ! jq -e \
  --arg version "${VERSION}" \
  --arg image "${IMAGE}" \
  '
    .kind == "Deployment"
    and .metadata.name == "sre-agent-v2-shadow"
    and .metadata.namespace == "sre-agent-system"
    and .metadata.labels["app.kubernetes.io/version"] == $version
    and .spec.template.metadata.labels["app.kubernetes.io/version"] == $version
    and .spec.replicas == 1
    and .spec.strategy.type == "Recreate"
    and (
      [
        .spec.template.spec.containers[]
        | select(.name == "sre-agent")
        | .image
      ] == [$image]
    )
    and (
      [
        .spec.template.spec.containers[]
        | select(.name == "sre-agent")
        | .env[]?
        | select(.name == "RESTART_POD_APPROVED")
        | .value
      ] == ["false"]
    )
  ' >/dev/null <<<"${MANIFEST_JSON}"
then
  fail "deployment manifest does not match the expected safe release"
fi

ARGO_JSON=$(
  kubectl -n "${ARGO_NAMESPACE}" get application "${APPLICATION}" -o json
)

if ! jq -e \
  --arg revision "${REVISION}" \
  '
    .status.sync.revision == $revision
    and .status.sync.status == "Synced"
    and .status.health.status == "Healthy"
    and ((.status.conditions // []) | length == 0)
  ' >/dev/null <<<"${ARGO_JSON}"
then
  fail "Argo CD is not Synced/Healthy at the expected revision"
fi

DEPLOYMENT_JSON=$(
  kubectl -n "${AGENT_NAMESPACE}" get deployment "${DEPLOYMENT}" -o json
)

if ! jq -e \
  --arg version "${VERSION}" \
  --arg image "${IMAGE}" \
  '
    .metadata.labels["app.kubernetes.io/version"] == $version
    and .spec.template.metadata.labels["app.kubernetes.io/version"] == $version
    and .spec.replicas == 1
    and .spec.strategy.type == "Recreate"
    and .status.availableReplicas == 1
    and .status.readyReplicas == 1
    and .status.updatedReplicas == 1
    and (
      [
        .spec.template.spec.containers[]
        | select(.name == "sre-agent")
        | .image
      ] == [$image]
    )
  ' >/dev/null <<<"${DEPLOYMENT_JSON}"
then
  fail "live Deployment does not match the expected release"
fi

PODS_JSON=$(
  kubectl -n "${AGENT_NAMESPACE}" get pods \
    -l app.kubernetes.io/name=sre-agent-v2 \
    -o json
)

POD_COUNT=$(jq '.items | length' <<<"${PODS_JSON}")
require_equal "${POD_COUNT}" "1" "agent Pod count"

POD_NAME=$(jq -r '.items[0].metadata.name' <<<"${PODS_JSON}")
POD_READY=$(
  jq -r '
    [
      .items[0].status.conditions[]?
      | select(.type == "Ready")
      | .status
    ][0] // "False"
  ' <<<"${PODS_JSON}"
)
POD_RESTARTS=$(
  jq -r '
    [
      .items[0].status.containerStatuses[]?.restartCount
    ] | add // 0
  ' <<<"${PODS_JSON}"
)
POD_IMAGE_ID=$(
  jq -r '
    [
      .items[0].status.containerStatuses[]?
      | select(.name == "sre-agent")
      | .imageID
    ][0] // ""
  ' <<<"${PODS_JSON}"
)

require_equal "${POD_READY}" "True" "agent Pod Ready"
require_equal "${POD_RESTARTS}" "0" "agent Pod restarts"

EXPECTED_DIGEST="${IMAGE##*@}"
if [[ "${POD_IMAGE_ID}" != *@"${EXPECTED_DIGEST}" ]]; then
  fail "Pod imageID=${POD_IMAGE_ID}; want digest ${EXPECTED_DIGEST}"
fi

STATE_JSON=$(
  kubectl -n "${AGENT_NAMESPACE}" get configmap "${STATE_CONFIGMAP}" -o json
)
LIVE_STATE_UID=$(jq -r '.metadata.uid' <<<"${STATE_JSON}")
STATE_DATA=$(jq -r '.data["state.json"] // empty' <<<"${STATE_JSON}")

require_equal "${LIVE_STATE_UID}" "${STATE_UID}" "state ConfigMap UID"

if ! jq -e '
  .schemaVersion == "v1"
  and (.handledAt | type == "object")
  and (.lastActionAt | type == "object")
  and (.attemptsByScope | type == "object")
' >/dev/null <<<"${STATE_DATA}"
then
  fail "remediation state data is not valid schema v1"
fi

mkdir -p "${APP_DIR}/.tmp"
TEMP_DIR=$(
  mktemp -d "${APP_DIR}/.tmp/release-state.XXXXXX"
)
trap cleanup EXIT

kubectl -n "${AGENT_NAMESPACE}" port-forward \
  "service/${METRICS_SERVICE}" \
  "${OPERATIONAL_PORT}:8080" \
  >"${TEMP_DIR}/operational.log" 2>&1 &
OPERATIONAL_PID=$!

OPERATIONAL_READY=false
for attempt in $(seq 1 30); do
  if curl -fsS \
    "http://127.0.0.1:${OPERATIONAL_PORT}/livez" \
    >/dev/null 2>&1
  then
    OPERATIONAL_READY=true
    break
  fi
  sleep 1
done

if [[ "${OPERATIONAL_READY}" != "true" ]]; then
  cat "${TEMP_DIR}/operational.log" >&2 || true
  fail "operational HTTP port-forward did not become ready"
fi

LIVE_RESPONSE=$(
  curl -fsS "http://127.0.0.1:${OPERATIONAL_PORT}/livez"
)
READY_RESPONSE=$(
  curl -fsS "http://127.0.0.1:${OPERATIONAL_PORT}/readyz"
)

require_equal "${LIVE_RESPONSE}" "ok" "liveness response"
require_equal "${READY_RESPONSE}" "ok" "readiness response"

kubectl -n "${PROMETHEUS_NAMESPACE}" port-forward \
  "service/${PROMETHEUS_SERVICE}" \
  "${PROMETHEUS_PORT}:9090" \
  >"${TEMP_DIR}/prometheus.log" 2>&1 &
PROMETHEUS_PID=$!

PROMETHEUS_READY=false
for attempt in $(seq 1 30); do
  if curl -fsS \
    "http://127.0.0.1:${PROMETHEUS_PORT}/-/ready" \
    >/dev/null 2>&1
  then
    PROMETHEUS_READY=true
    break
  fi
  sleep 1
done

if [[ "${PROMETHEUS_READY}" != "true" ]]; then
  cat "${TEMP_DIR}/prometheus.log" >&2 || true
  fail "Prometheus API port-forward did not become ready"
fi

TARGET_RESULT=$(
  curl -fsS \
    --get \
    --data-urlencode \
    'query=max(up{namespace="sre-agent-system",service="sre-agent-v2-metrics"}) or vector(0)' \
    "http://127.0.0.1:${PROMETHEUS_PORT}/api/v1/query"
)

if ! jq -e '
  .status == "success"
  and (.data.result | length == 1)
  and ((.data.result[0].value[1] | tonumber) == 1)
' >/dev/null <<<"${TARGET_RESULT}"
then
  fail "Prometheus metrics target is not Up"
fi

CYCLE_RESULT=$(
  curl -fsS \
    --get \
    --data-urlencode \
    'query=sum(increase(sre_agent_cycles_total{namespace="sre-agent-system",service="sre-agent-v2-metrics"}[5m])) or vector(0)' \
    "http://127.0.0.1:${PROMETHEUS_PORT}/api/v1/query"
)

CYCLES_IN_LAST_5M=$(
  jq -r '.data.result[0].value[1] // "0"' <<<"${CYCLE_RESULT}"
)

if ! jq -en \
  --arg value "${CYCLES_IN_LAST_5M}" \
  '($value | tonumber) > 0' \
  >/dev/null
then
  fail "SRE Agent cycles did not progress during the last five minutes"
fi

printf 'revision=%s\n' "${REVISION}"
printf 'version=%s\n' "${VERSION}"
printf 'image=%s\n' "${IMAGE}"
printf 'pod=%s\n' "${POD_NAME}"
printf 'ready=true\n'
printf 'restarts=%s\n' "${POD_RESTARTS}"
printf 'stateUID=%s\n' "${LIVE_STATE_UID}"
printf 'stateSchema=v1\n'
printf 'liveness=ok\n'
printf 'readiness=ok\n'
printf 'prometheusTargetUp=1\n'
printf 'cyclesInLast5m=%s\n' "${CYCLES_IN_LAST_5M}"
echo "RELEASE_STATE_RUNTIME_VERIFIED"
