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

DEPLOYMENT="${APP_DIR}/deployment.yaml"

kubectl create \
  --dry-run=client \
  -f "${DEPLOYMENT}" \
  -o json |
  jq -e '
    .kind == "Deployment"
    and
    (.spec.replicas == 1)
    and
    (.spec.strategy.type == "Recreate")
    and
    (
      [
        .spec.template.spec.containers[]
        | select(.name == "sre-agent")
        | .env[]?
        | select(.name == "REMEDIATION_STATE_NAMESPACE")
        | .value
      ] == ["sre-agent-system"]
    )
    and
    (
      [
        .spec.template.spec.containers[]
        | select(.name == "sre-agent")
        | .env[]?
        | select(.name == "REMEDIATION_STATE_CONFIGMAP")
        | .value
      ] == ["sre-agent-v2-state"]
    )
    and
    (
      [
        .spec.template.spec.containers[]
        | select(.name == "sre-agent")
        | .env[]?
        | select(.name == "RESTART_POD_APPROVED")
        | .value
      ] == ["false"]
    )
  ' >/dev/null

echo "REMEDIATION_STATE_DEPLOYMENT_CONTRACT_VERIFIED"
