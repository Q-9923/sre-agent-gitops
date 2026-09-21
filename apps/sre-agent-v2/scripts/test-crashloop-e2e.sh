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

AGENT_NAMESPACE="sre-agent-system"
AGENT_DEPLOYMENT="sre-agent-v2-shadow"
AGENT_SELECTOR="app.kubernetes.io/name=sre-agent-v2,app.kubernetes.io/component=agent"
STATE_CONFIGMAP="sre-agent-v2-state"
LAB_NAMESPACE="sre-agent-lab"
TEST_IMAGE="alpine:3.20.3"
PROMETHEUS_SERVICE="prometheus-stack-kube-prom-prometheus"
PROMETHEUS_PORT="19095"

TEST_DEPLOYMENT=""
TEST_POD=""
PORT_FORWARD_PID=""
PORT_FORWARD_LOG=""
TEMP_DIR=""

preflight() {
  local deployment_json
  local replicas
  local strategy
  local restart_approved

  deployment_json=$(
    kubectl -n "${AGENT_NAMESPACE}" get deployment \
      "${AGENT_DEPLOYMENT}" \
      -o json
  )

  replicas=$(
    jq -r '.spec.replicas // 0' <<<"${deployment_json}"
  )

  strategy=$(
    jq -r '.spec.strategy.type // ""' <<<"${deployment_json}"
  )

  restart_approved=$(
    jq -r '
      [
        .spec.template.spec.containers[]
        | select(.name == "sre-agent")
        | .env[]?
        | select(.name == "RESTART_POD_APPROVED")
        | .value
      ][0] // "missing"
    ' <<<"${deployment_json}"
  )

  printf 'replicas=%s\n' "${replicas}"
  printf 'strategy=%s\n' "${strategy}"
  printf 'restartApproved=%s\n' "${restart_approved}"

  if [[ "${replicas}" != "1" ]]; then
    echo "ERROR: CrashLoop E2E requires exactly one Agent replica" >&2
    return 1
  fi

  if [[ "${strategy}" != "Recreate" ]]; then
    echo "ERROR: CrashLoop E2E requires Recreate strategy" >&2
    return 1
  fi

  if [[ "${restart_approved}" != "false" ]]; then
    echo "ERROR: CrashLoop E2E requires RESTART_POD_APPROVED=false" >&2
    return 1
  fi

  echo "CRASHLOOP_E2E_PREFLIGHT_VERIFIED"
}

cleanup() {
  if [[ -n "${PORT_FORWARD_PID}" ]]; then
    kill "${PORT_FORWARD_PID}" 2>/dev/null || true
    wait "${PORT_FORWARD_PID}" 2>/dev/null || true
  fi

  if [[ -n "${TEST_DEPLOYMENT}" ]]; then
    kubectl -n "${LAB_NAMESPACE}" delete deployment \
      "${TEST_DEPLOYMENT}" \
      --ignore-not-found \
      --wait=false >/dev/null 2>&1 || true
  fi

  if [[ -n "${PORT_FORWARD_LOG}" ]]; then
    rm -f -- "${PORT_FORWARD_LOG}"
  fi

  if [[ -n "${TEMP_DIR}" ]]; then
    rmdir -- "${TEMP_DIR}" 2>/dev/null || true
  fi
}

agent_logs() {
  kubectl -n "${AGENT_NAMESPACE}" logs "${AGENT_POD_BEFORE}" \
    --since-time="${TEST_STARTED_AT}" 2>/dev/null || true
}

show_target_logs() {
  agent_logs |
    jq -Rrc \
      --arg target "${TARGET_LABEL}" \
      'fromjson? | select(.target == $target)' || true
}

if [[ "${1:-}" == "--preflight-only" ]] && [[ "$#" -eq 1 ]]; then
  preflight
  exit 0
fi

if [[ "$#" -ne 0 ]]; then
  echo "ERROR: unsupported arguments" >&2
  exit 1
fi

preflight

AGENT_PODS_JSON=$(
  kubectl -n "${AGENT_NAMESPACE}" get pod \
    -l "${AGENT_SELECTOR}" \
    -o json
)

AGENT_POD_BEFORE=$(
  jq -r '
    [
      .items[]
      | select(.status.phase == "Running")
    ][0].metadata.name // empty
  ' <<<"${AGENT_PODS_JSON}"
)

if [[ -z "${AGENT_POD_BEFORE}" ]]; then
  echo "ERROR: no running SRE Agent Pod found" >&2
  exit 1
fi

kubectl -n "${AGENT_NAMESPACE}" wait \
  --for=condition=Ready \
  "pod/${AGENT_POD_BEFORE}" \
  --timeout=30s

AGENT_RESTARTS_BEFORE=$(
  kubectl -n "${AGENT_NAMESPACE}" get pod \
    "${AGENT_POD_BEFORE}" \
    -o json |
    jq -r '
      .status.containerStatuses[]
      | select(.name == "sre-agent")
      | .restartCount
    '
)

TEST_STARTED_AT=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
TEST_SUFFIX=$(date -u '+%Y%m%d%H%M%S')
TEST_DEPLOYMENT="sre-agent-e2e-crash-${TEST_SUFFIX}"
TARGET_LABEL=""

mkdir -p "${APP_DIR}/.tmp"
TEMP_DIR=$(
  mktemp -d "${APP_DIR}/.tmp/crashloop-e2e.XXXXXX"
)
PORT_FORWARD_LOG="${TEMP_DIR}/prometheus-port-forward.log"

trap cleanup EXIT

if kubectl -n "${LAB_NAMESPACE}" get deployment \
  "${TEST_DEPLOYMENT}" >/dev/null 2>&1
then
  echo "ERROR: temporary Deployment already exists" >&2
  exit 1
fi

kubectl -n "${LAB_NAMESPACE}" create deployment \
  "${TEST_DEPLOYMENT}" \
  --image="${TEST_IMAGE}" \
  -- \
  /bin/sh -c \
  "echo 'intentional SRE Agent E2E failure'; exit 1"

for attempt in $(seq 1 60); do
  TEST_POD=$(
    kubectl -n "${LAB_NAMESPACE}" get pod \
      -l "app=${TEST_DEPLOYMENT}" \
      -o json |
      jq -r '.items[0].metadata.name // empty'
  )

  if [[ -n "${TEST_POD}" ]]; then
    printf 'pod_discovered=%s attempt=%s\n' \
      "${TEST_POD}" "${attempt}"
    break
  fi

  sleep 2
done

if [[ -z "${TEST_POD}" ]]; then
  echo "ERROR: temporary CrashLoop Pod was not created" >&2
  exit 1
fi

TARGET_LABEL="${LAB_NAMESPACE}/${TEST_POD}"

POD_UID_BEFORE=$(
  kubectl -n "${LAB_NAMESPACE}" get pod "${TEST_POD}" \
    -o jsonpath='{.metadata.uid}'
)

CRASHLOOP_READY=false

for attempt in $(seq 1 60); do
  POD_JSON=$(
    kubectl -n "${LAB_NAMESPACE}" get pod "${TEST_POD}" \
      -o json
  )

  WAITING_REASON=$(
    jq -r '
      .status.containerStatuses[0].state.waiting.reason // ""
    ' <<<"${POD_JSON}"
  )

  if [[ "${WAITING_REASON}" == "CrashLoopBackOff" ]]; then
    CRASHLOOP_READY=true
    RESTART_COUNT=$(
      jq -r '.status.containerStatuses[0].restartCount' \
        <<<"${POD_JSON}"
    )
    printf 'crashLoopBackOff=true restarts=%s attempt=%s\n' \
      "${RESTART_COUNT}" "${attempt}"
    break
  fi

  sleep 2
done

if [[ "${CRASHLOOP_READY}" != "true" ]]; then
  kubectl -n "${LAB_NAMESPACE}" describe pod "${TEST_POD}" || true
  echo "ERROR: temporary Pod did not reach CrashLoopBackOff" >&2
  exit 1
fi

kubectl -n monitoring port-forward \
  "service/${PROMETHEUS_SERVICE}" \
  "${PROMETHEUS_PORT}:9090" \
  >"${PORT_FORWARD_LOG}" 2>&1 &
PORT_FORWARD_PID=$!

PROMETHEUS_READY=false

for attempt in $(seq 1 20); do
  if curl -fsS \
    "http://127.0.0.1:${PROMETHEUS_PORT}/-/ready" \
    >/dev/null 2>&1
  then
    PROMETHEUS_READY=true
    printf 'prometheus_ready=true attempt=%s\n' "${attempt}"
    break
  fi

  sleep 1
done

if [[ "${PROMETHEUS_READY}" != "true" ]]; then
  cat "${PORT_FORWARD_LOG}"
  echo "ERROR: Prometheus API unavailable" >&2
  exit 1
fi

ALERT_QUERY=$(
  printf \
    'ALERTS{alertname="PodCrashLooping",namespace="%s",pod="%s",alertstate="firing"}' \
    "${LAB_NAMESPACE}" \
    "${TEST_POD}"
)

ALERT_FIRING=false

for attempt in $(seq 1 60); do
  ALERT_RESPONSE=$(
    curl -fsSG \
      "http://127.0.0.1:${PROMETHEUS_PORT}/api/v1/query" \
      --data-urlencode "query=${ALERT_QUERY}" \
      2>/dev/null || true
  )

  if jq -e '
    .status == "success"
    and (.data.result | length) > 0
  ' >/dev/null 2>&1 <<<"${ALERT_RESPONSE}"
  then
    ALERT_FIRING=true
    printf 'alert_firing=true attempt=%s\n' "${attempt}"
    break
  fi

  sleep 5
done

if [[ "${ALERT_FIRING}" != "true" ]]; then
  echo "ERROR: PodCrashLooping alert did not become firing" >&2
  exit 1
fi

DECISION_JSON=""

for attempt in $(seq 1 60); do
  DECISION_JSON=$(
    agent_logs |
      jq -Rrc \
        --arg target "${TARGET_LABEL}" \
        '
          fromjson?
          | select(
              .msg == "decision_denied"
              and .target == $target
              and .action == "RESTART_POD"
              and .error_code == "APPROVAL_REQUIRED"
            )
        ' |
      tail -n 1
  )

  if [[ -n "${DECISION_JSON}" ]]; then
    printf 'decision_denied=true attempt=%s\n' "${attempt}"
    break
  fi

  sleep 5
done

if [[ -z "${DECISION_JSON}" ]]; then
  echo "=== TARGET LOGS ==="
  show_target_logs
  echo "ERROR: expected approval denial was not observed" >&2
  exit 1
fi

INCIDENT_ID=$(
  jq -r '.incident_id // empty' <<<"${DECISION_JSON}"
)

if [[ ! "${INCIDENT_ID}" =~ ^inc-[0-9a-f]{24}$ ]]; then
  echo "ERROR: invalid incident_id in decision_denied log" >&2
  exit 1
fi

printf 'incident=%s\n' "${INCIDENT_ID}"

STATE_JSON=$(
  kubectl -n "${AGENT_NAMESPACE}" get configmap \
    "${STATE_CONFIGMAP}" \
    -o json |
    jq -r '.data["state.json"] // empty'
)

if ! jq -e \
  --arg incident "${INCIDENT_ID}" \
  '
    .schemaVersion == "v1"
    and (.handledAt[$incident] != null)
  ' >/dev/null <<<"${STATE_JSON}"
then
  echo "ERROR: incident is absent from persisted remediation state" >&2
  exit 1
fi

echo "incident_persisted=true"

DUPLICATE_JSON=""

for attempt in $(seq 1 30); do
  DUPLICATE_JSON=$(
    agent_logs |
      jq -Rrc \
        --arg target "${TARGET_LABEL}" \
        --arg incident "${INCIDENT_ID}" \
        '
          fromjson?
          | select(
              .msg == "incident_skipped"
              and .target == $target
              and .incident_id == $incident
              and .error_code == "DUPLICATE_INCIDENT"
            )
        ' |
      tail -n 1
  )

  if [[ -n "${DUPLICATE_JSON}" ]]; then
    printf 'duplicate_incident=true attempt=%s\n' "${attempt}"
    break
  fi

  sleep 5
done

if [[ -z "${DUPLICATE_JSON}" ]]; then
  echo "=== TARGET LOGS ==="
  show_target_logs
  echo "ERROR: duplicate incident was not observed" >&2
  exit 1
fi

ACTION_LOG=$(
  agent_logs |
    jq -Rrc \
      --arg target "${TARGET_LABEL}" \
      '
        fromjson?
        | select(
            .target == $target
            and (
              .msg == "remediation_submitted"
              or .msg == "remediation_failed"
            )
          )
      ' |
    tail -n 1
)

if [[ -n "${ACTION_LOG}" ]]; then
  printf '%s\n' "${ACTION_LOG}" >&2
  echo "ERROR: Kubernetes remediation was attempted" >&2
  exit 1
fi

if ! POD_JSON_AFTER=$(
  kubectl -n "${LAB_NAMESPACE}" get pod "${TEST_POD}" \
    -o json
); then
  echo "ERROR: temporary Pod disappeared during Shadow evaluation" >&2
  exit 1
fi

POD_UID_AFTER=$(
  jq -r '.metadata.uid' <<<"${POD_JSON_AFTER}"
)

DELETION_TIMESTAMP=$(
  jq -r '.metadata.deletionTimestamp // empty' \
    <<<"${POD_JSON_AFTER}"
)

if [[ "${POD_UID_AFTER}" != "${POD_UID_BEFORE}" ]]; then
  echo "ERROR: temporary Pod UID changed" >&2
  exit 1
fi

if [[ -n "${DELETION_TIMESTAMP}" ]]; then
  echo "ERROR: temporary Pod received a deletion timestamp" >&2
  exit 1
fi

AGENT_POD_AFTER=$(
  kubectl -n "${AGENT_NAMESPACE}" get pod \
    -l "${AGENT_SELECTOR}" \
    -o json |
    jq -r '
      [
        .items[]
        | select(.status.phase == "Running")
      ][0].metadata.name // empty
    '
)

if [[ "${AGENT_POD_AFTER}" != "${AGENT_POD_BEFORE}" ]]; then
  echo "ERROR: SRE Agent Pod changed during E2E test" >&2
  exit 1
fi

AGENT_JSON_AFTER=$(
  kubectl -n "${AGENT_NAMESPACE}" get pod \
    "${AGENT_POD_AFTER}" \
    -o json
)

AGENT_READY_AFTER=$(
  jq -r '
    .status.containerStatuses[]
    | select(.name == "sre-agent")
    | .ready
  ' <<<"${AGENT_JSON_AFTER}"
)

AGENT_RESTARTS_AFTER=$(
  jq -r '
    .status.containerStatuses[]
    | select(.name == "sre-agent")
    | .restartCount
  ' <<<"${AGENT_JSON_AFTER}"
)

if [[ "${AGENT_READY_AFTER}" != "true" ]]; then
  echo "ERROR: SRE Agent is not Ready after E2E test" >&2
  exit 1
fi

if [[ "${AGENT_RESTARTS_AFTER}" != "${AGENT_RESTARTS_BEFORE}" ]]; then
  echo "ERROR: SRE Agent restart count changed" >&2
  exit 1
fi

CYCLE_RESPONSE=$(
  curl -fsSG \
    "http://127.0.0.1:${PROMETHEUS_PORT}/api/v1/query" \
    --data-urlencode \
    'query=sum(increase(sre_agent_cycles_total{namespace="sre-agent-system",service="sre-agent-v2-metrics"}[5m]))'
)

CYCLES_IN_LAST_5M=$(
  jq -r '.data.result[0].value[1] // "missing"' \
    <<<"${CYCLE_RESPONSE}"
)

if ! awk -v value="${CYCLES_IN_LAST_5M}" \
  'BEGIN { exit !(value != "missing" && value > 0) }'
then
  echo "ERROR: SRE Agent cycles are not progressing" >&2
  exit 1
fi

printf 'pod_uid_unchanged=%s\n' "${POD_UID_AFTER}"
printf 'agent_ready=%s\n' "${AGENT_READY_AFTER}"
printf 'agent_restarts_before=%s\n' "${AGENT_RESTARTS_BEFORE}"
printf 'agent_restarts_after=%s\n' "${AGENT_RESTARTS_AFTER}"
printf 'cycles_in_last_5m=%s\n' "${CYCLES_IN_LAST_5M}"

COMPLETED_DEPLOYMENT="${TEST_DEPLOYMENT}"

cleanup
trap - EXIT

CLEANED=false

for attempt in $(seq 1 30); do
  REMAINING_PODS=$(
    kubectl -n "${LAB_NAMESPACE}" get pod \
      -l "app=${COMPLETED_DEPLOYMENT}" \
      -o json |
      jq '.items | length'
  )

  if [[ "${REMAINING_PODS}" -eq 0 ]]; then
    CLEANED=true
    printf 'cleanup_complete=true attempt=%s\n' "${attempt}"
    break
  fi

  sleep 2
done

if [[ "${CLEANED}" != "true" ]]; then
  echo "ERROR: temporary CrashLoop Pod was not cleaned up" >&2
  exit 1
fi

echo "CRASHLOOP_E2E_RUNTIME_VERIFIED"
