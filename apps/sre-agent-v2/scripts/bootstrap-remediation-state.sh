#!/usr/bin/env bash

set -euo pipefail

NAMESPACE="${REMEDIATION_STATE_NAMESPACE:-sre-agent-system}"
CONFIG_NAME="${REMEDIATION_STATE_CONFIGMAP:-sre-agent-v2-state}"
INITIAL_STATE='{"schemaVersion":"v1","handledAt":{},"lastActionAt":{},"attemptsByScope":{}}'

EXISTING_CONFIG_MAP=$(
  kubectl -n "${NAMESPACE}" get configmap "${CONFIG_NAME}" \
    --ignore-not-found \
    -o name
)

if [[ -n "${EXISTING_CONFIG_MAP}" ]]; then
  printf 'remediation state ConfigMap %s/%s already exists; preserving existing state\n' \
    "${NAMESPACE}" \
    "${CONFIG_NAME}"
  exit 0
fi

set +e
CREATE_OUTPUT=$(
  kubectl -n "${NAMESPACE}" create configmap "${CONFIG_NAME}" \
    --from-literal=state.json="${INITIAL_STATE}" 2>&1
)
CREATE_EXIT=$?
set -e

if [[ "${CREATE_EXIT}" -eq 0 ]]; then
  if [[ -n "${CREATE_OUTPUT}" ]]; then
    printf '%s\n' "${CREATE_OUTPUT}"
  fi

  printf 'created remediation state ConfigMap %s/%s\n' \
    "${NAMESPACE}" \
    "${CONFIG_NAME}"
  exit 0
fi

EXISTING_AFTER_CREATE=$(
  kubectl -n "${NAMESPACE}" get configmap "${CONFIG_NAME}" \
    --ignore-not-found \
    -o name
)

if [[ -n "${EXISTING_AFTER_CREATE}" ]]; then
  printf 'remediation state ConfigMap %s/%s already exists; preserving existing state\n' \
    "${NAMESPACE}" \
    "${CONFIG_NAME}"
  exit 0
fi

printf '%s\n' "${CREATE_OUTPUT}" >&2
exit "${CREATE_EXIT}"
