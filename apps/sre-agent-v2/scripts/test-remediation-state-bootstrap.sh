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

BOOTSTRAP="${SCRIPT_DIR}/bootstrap-remediation-state.sh"
TEMP_ROOT="${APP_DIR}/.tmp"

if [[ ! -x "${BOOTSTRAP}" ]]; then
  echo "ERROR: missing executable bootstrap-remediation-state.sh" >&2
  exit 1
fi

mkdir -p "${TEMP_ROOT}"

TEMP_DIR=$(
  mktemp -d "${TEMP_ROOT}/remediation-state-bootstrap.XXXXXX"
)

FAKE_BIN="${TEMP_DIR}/bin"
KUBECTL_LOG="${TEMP_DIR}/kubectl.log"
KUBECTL_STATE="${TEMP_DIR}/state.json"

mkdir -p "${FAKE_BIN}"
touch "${KUBECTL_LOG}"

cleanup() {
  rm -f -- \
    "${FAKE_BIN}/kubectl" \
    "${KUBECTL_LOG}" \
    "${KUBECTL_STATE}"
  rmdir -- "${FAKE_BIN}" 2>/dev/null || true
  rmdir -- "${TEMP_DIR}" 2>/dev/null || true
}

trap cleanup EXIT

cat >"${FAKE_BIN}/kubectl" <<'FAKE_KUBECTL'
#!/usr/bin/env bash

set -euo pipefail

printf '%s\n' "$*" >>"${KUBECTL_LOG}"

arguments=" $* "

if [[ "${arguments}" == *" get configmap sre-agent-v2-state "* ]]; then
  if [[ "${KUBECTL_FORCE_GET_ERROR:-false}" == "true" ]]; then
    echo "simulated ConfigMap get failure" >&2
    exit 7
  fi

  if [[ -f "${KUBECTL_STATE}" ]]; then
    echo "configmap/sre-agent-v2-state"
    exit 0
  fi

  if [[ "${arguments}" == *" --ignore-not-found "* ]]; then
    exit 0
  fi

  exit 1
fi

  if [[ "${KUBECTL_CREATE_ALREADY_EXISTS:-false}" == "true" ]]; then
    printf '%s\n' \
      '{"schemaVersion":"v1","sentinel":"created-by-peer"}' \
      >"${KUBECTL_STATE}"

    echo 'Error from server (AlreadyExists): configmaps "sre-agent-v2-state" already exists' >&2
    exit 1
  fi

if [[ "${arguments}" == *" create configmap sre-agent-v2-state "* ]]; then
  state_value=""

  for argument in "$@"; do
    case "${argument}" in
      --from-literal=state.json=*)
        state_value=${argument#--from-literal=state.json=}
        ;;
    esac
  done

  if [[ -z "${state_value}" ]]; then
    echo "fake kubectl: missing state.json literal" >&2
    exit 2
  fi

  printf '%s\n' "${state_value}" >"${KUBECTL_STATE}"
  exit 0
fi

echo "fake kubectl: unsupported arguments: $*" >&2
exit 2
FAKE_KUBECTL

chmod 0755 "${FAKE_BIN}/kubectl"

export KUBECTL_LOG
export KUBECTL_STATE

EXPECTED_INITIAL_STATE='{"schemaVersion":"v1","handledAt":{},"lastActionAt":{},"attemptsByScope":{}}'

PATH="${FAKE_BIN}:${PATH}" "${BOOTSTRAP}"

if [[ ! -f "${KUBECTL_STATE}" ]]; then
  echo "ERROR: bootstrap did not create state data" >&2
  exit 1
fi

ACTUAL_INITIAL_STATE=$(
  tr -d '\n' <"${KUBECTL_STATE}"
)

if [[ "${ACTUAL_INITIAL_STATE}" != "${EXPECTED_INITIAL_STATE}" ]]; then
  printf 'ERROR: initial state = %s; want %s\n' \
    "${ACTUAL_INITIAL_STATE}" \
    "${EXPECTED_INITIAL_STATE}" >&2
  exit 1
fi

PRESERVED_STATE='{"schemaVersion":"v1","sentinel":"preserve-existing-state"}'
printf '%s\n' "${PRESERVED_STATE}" >"${KUBECTL_STATE}"

PATH="${FAKE_BIN}:${PATH}" "${BOOTSTRAP}"

ACTUAL_PRESERVED_STATE=$(
  tr -d '\n' <"${KUBECTL_STATE}"
)

if [[ "${ACTUAL_PRESERVED_STATE}" != "${PRESERVED_STATE}" ]]; then
  printf 'ERROR: existing state was overwritten: %s\n' \
    "${ACTUAL_PRESERVED_STATE}" >&2
  exit 1
fi

rm -f -- "${KUBECTL_STATE}"

set +e
GET_FAILURE_OUTPUT=$(
  KUBECTL_FORCE_GET_ERROR=true \
    PATH="${FAKE_BIN}:${PATH}" \
    "${BOOTSTRAP}" 2>&1
)
GET_FAILURE_EXIT=$?
set -e

if [[ "${GET_FAILURE_EXIT}" -eq 0 ]]; then
  printf 'ERROR: bootstrap accepted ConfigMap get failure: %s\n' \
    "${GET_FAILURE_OUTPUT}" >&2
  exit 1
fi

if [[ -f "${KUBECTL_STATE}" ]]; then
  echo "ERROR: bootstrap created state after ConfigMap get failure" >&2
  exit 1
fi

if [[ "${GET_FAILURE_OUTPUT}" != *"simulated ConfigMap get failure"* ]]; then
  printf 'ERROR: bootstrap hid the original get failure: %s\n' \
    "${GET_FAILURE_OUTPUT}" >&2
  exit 1
fi

set +e
RACE_OUTPUT=$(
  KUBECTL_CREATE_ALREADY_EXISTS=true \
    PATH="${FAKE_BIN}:${PATH}" \
    "${BOOTSTRAP}" 2>&1
)
RACE_EXIT=$?
set -e

if [[ "${RACE_EXIT}" -ne 0 ]]; then
  printf 'ERROR: bootstrap failed after concurrent create: %s\n' \
    "${RACE_OUTPUT}" >&2
  exit 1
fi

RACE_STATE=$(
  tr -d '\n' <"${KUBECTL_STATE}"
)

EXPECTED_RACE_STATE='{"schemaVersion":"v1","sentinel":"created-by-peer"}'

if [[ "${RACE_STATE}" != "${EXPECTED_RACE_STATE}" ]]; then
  printf 'ERROR: concurrent state was overwritten: %s\n' \
    "${RACE_STATE}" >&2
  exit 1
fi

if [[ "${RACE_OUTPUT}" != *"already exists; preserving existing state"* ]]; then
  printf 'ERROR: concurrent create was not reported as preserved: %s\n' \
    "${RACE_OUTPUT}" >&2
  exit 1
fi
CREATE_COUNT=$(
  grep -c \
    'create configmap sre-agent-v2-state' \
    "${KUBECTL_LOG}" ||
    true
)

if [[ "${CREATE_COUNT}" -ne 2 ]]; then
  printf 'ERROR: ConfigMap create calls = %s; want 2\n' \
    "${CREATE_COUNT}" >&2
  exit 1
fi

echo "REMEDIATION_STATE_BOOTSTRAP_CONTRACT_VERIFIED"
