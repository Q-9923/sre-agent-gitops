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

MANIFEST="${APP_DIR}/prometheusrule.yaml"
TEST_FILE="${APP_DIR}/tests/prometheus-rule-test.yaml"
PROMETHEUS_IMAGE="quay.io/prometheus/prometheus:v2.51.0@sha256:5ccad477d0057e62a7cd1981ffcc43785ac10c5a35522dc207466ff7e7ec845f"
TEMP_ROOT="${APP_DIR}/.tmp"

mkdir -p "${TEMP_ROOT}"

TEMP_DIR=$(
  mktemp -d "${TEMP_ROOT}/prometheus-rules.XXXXXX"
)

RULES_FILE="${TEMP_DIR}/rules.yaml"

cleanup() {
  rm -f -- "${RULES_FILE}"
  rmdir -- "${TEMP_DIR}" 2>/dev/null || true
}

trap cleanup EXIT

if [[ -f "${MANIFEST}" ]]; then
  kubectl apply \
    --dry-run=client \
    -f "${MANIFEST}" \
    -o json |
    python3 -c '
import json
import sys

resource = json.load(sys.stdin)
groups = resource.get("spec", {}).get("groups", [])

json.dump({"groups": groups}, sys.stdout)
sys.stdout.write("\n")
' >"${RULES_FILE}"
else
  echo "INFO: prometheusrule.yaml is absent; testing an empty rule set"
  printf '{"groups":[]}\n' >"${RULES_FILE}"
fi

docker run \
  --rm \
  --workdir /work \
  --volume "${RULES_FILE}:/work/rules.yaml:ro" \
  --volume "${TEST_FILE}:/work/prometheus-rule-test.yaml:ro" \
  --entrypoint /bin/promtool \
  "${PROMETHEUS_IMAGE}" \
  test rules prometheus-rule-test.yaml
