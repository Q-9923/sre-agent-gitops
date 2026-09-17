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

MANIFEST="${APP_DIR}/grafana-dashboard.yaml"
TEMP_ROOT="${APP_DIR}/.tmp"
PROMQL_TEST_FILE="${APP_DIR}/tests/grafana-dashboard-promql-test.yaml"
PROMETHEUS_IMAGE="quay.io/prometheus/prometheus:v2.51.0@sha256:5ccad477d0057e62a7cd1981ffcc43785ac10c5a35522dc207466ff7e7ec845f"
mkdir -p "${TEMP_ROOT}"

TEMP_DIR=$(
  mktemp -d "${TEMP_ROOT}/grafana-dashboard.XXXXXX"
)

RESOURCE_FILE="${TEMP_DIR}/resource.json"
DASHBOARD_FILE="${TEMP_DIR}/dashboard.json"

cleanup() {
  rm -f -- "${RESOURCE_FILE}" "${DASHBOARD_FILE}"
  rmdir -- "${TEMP_DIR}" 2>/dev/null || true
}

trap cleanup EXIT

if [[ ! -f "${MANIFEST}" ]]; then
  echo "ERROR: grafana-dashboard.yaml is absent" >&2
  exit 1
fi

kubectl apply \
  --dry-run=client \
  -f "${MANIFEST}" \
  -o json >"${RESOURCE_FILE}"

jq -e '
  .apiVersion == "v1"
  and .kind == "ConfigMap"
  and .metadata.name == "sre-agent-v2-dashboard"
  and .metadata.namespace == "sre-agent-system"
  and .metadata.labels.grafana_dashboard == "1"
  and .metadata.labels["app.kubernetes.io/name"] == "sre-agent-v2"
  and .metadata.labels["app.kubernetes.io/component"] == "dashboard"
  and (.data["sre-agent-v2.json"] | type == "string")
' "${RESOURCE_FILE}" >/dev/null

jq -er \
  '.data["sre-agent-v2.json"]' \
  "${RESOURCE_FILE}" >"${DASHBOARD_FILE}"

jq empty "${DASHBOARD_FILE}"

jq -e '
  def panel($title):
    first(.panels[] | select(.title == $title));

  .id == null
  and .uid == "sre-agent-v2-operations"
  and .title == "SRE Agent v2 Operations"
  and .editable == false
  and .timezone == "browser"
  and .refresh == "30s"
  and .time.from == "now-6h"
  and .time.to == "now"
  and .schemaVersion >= 30
  and (.tags | index("sre-agent") != null)
  and (.tags | index("operations") != null)
  and (.panels | length) == 5
  and ([.panels[].id] | length) == ([.panels[].id] | unique | length)
  and all(
    .panels[];
    .datasource.type == "prometheus"
    and .datasource.uid == "prometheus"
    and .gridPos.w > 0
    and .gridPos.h > 0
  )
  and panel("Metrics Target").type == "stat"
  and panel("Metrics Target").targets[0].expr
    == "max(up{namespace=\"sre-agent-system\",service=\"sre-agent-v2-metrics\"}) or vector(0)"
  and panel("Cycle Errors (5m)").type == "stat"
  and panel("Cycle Errors (5m)").targets[0].expr
    == "sum(increase(sre_agent_cycles_total{namespace=\"sre-agent-system\",service=\"sre-agent-v2-metrics\",result=\"ERROR\"}[5m])) or vector(0)"
  and panel("Cycle Rate by Result").type == "timeseries"
  and panel("Cycle Rate by Result").targets[0].expr
    == "sum by (result) (rate(sre_agent_cycles_total{namespace=\"sre-agent-system\",service=\"sre-agent-v2-metrics\"}[$__rate_interval]))"
  and panel("Total Cycles by Result").type == "bargauge"
  and panel("Total Cycles by Result").targets[0].expr
    == "sum by (result) (sre_agent_cycles_total{namespace=\"sre-agent-system\",service=\"sre-agent-v2-metrics\"})"
  and panel("Go Goroutines").type == "timeseries"
  and panel("Go Goroutines").targets[0].expr
    == "max(go_goroutines{namespace=\"sre-agent-system\",service=\"sre-agent-v2-metrics\"})"
' "${DASHBOARD_FILE}" >/dev/null

docker run \
  --rm \
  --workdir /work \
  --volume "${PROMQL_TEST_FILE}:/work/grafana-dashboard-promql-test.yaml:ro" \
  --entrypoint /bin/promtool \
  "${PROMETHEUS_IMAGE}" \
  test rules grafana-dashboard-promql-test.yaml

echo "GRAFANA_DASHBOARD_CONTRACT_VERIFIED"
