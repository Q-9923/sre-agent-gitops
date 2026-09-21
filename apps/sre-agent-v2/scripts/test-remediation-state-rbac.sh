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

MANIFEST="${APP_DIR}/rbac.yaml"
TEMP_ROOT="${APP_DIR}/.tmp"

mkdir -p "${TEMP_ROOT}"

RESOURCES_FILE=$(
  mktemp "${TEMP_ROOT}/remediation-state-rbac.XXXXXX.json"
)

cleanup() {
  rm -f -- "${RESOURCES_FILE}"
}

trap cleanup EXIT

kubectl apply \
  --dry-run=client \
  -f "${MANIFEST}" \
  -o json |
  jq -s '
    [
      .[] |
      if .kind == "List" then
        .items[]
      else
        .
      end
    ]
  ' >"${RESOURCES_FILE}"

if ! jq -e '
  (
    [
      .[] |
      select(
        .kind == "Role" and
        .metadata.name == "sre-agent-v2-state-writer" and
        .metadata.namespace == "sre-agent-system"
      ) |
      select(.rules | length == 1) |
      select((.rules[0].apiGroups | sort) == [""]) |
      select((.rules[0].resources | sort) == ["configmaps"]) |
      select(
        (.rules[0].resourceNames | sort) ==
        ["sre-agent-v2-state"]
      ) |
      select(
        (.rules[0].verbs | sort) ==
        ["get","update"]
      )
    ] |
    length
  ) == 1
  and
  (
    [
      .[] |
      select(
        .kind == "RoleBinding" and
        .metadata.name == "sre-agent-v2-state-writer" and
        .metadata.namespace == "sre-agent-system"
      ) |
      select(.roleRef.apiGroup == "rbac.authorization.k8s.io") |
      select(.roleRef.kind == "Role") |
      select(.roleRef.name == "sre-agent-v2-state-writer") |
      select(.subjects | length == 1) |
      select(.subjects[0].kind == "ServiceAccount") |
      select(.subjects[0].name == "sre-agent-v2") |
      select(.subjects[0].namespace == "sre-agent-system")
    ] |
    length
  ) == 1
' "${RESOURCES_FILE}" >/dev/null; then
  echo "ERROR: remediation state RBAC contract is not satisfied" >&2
  exit 1
fi

echo "REMEDIATION_STATE_RBAC_CONTRACT_VERIFIED"
