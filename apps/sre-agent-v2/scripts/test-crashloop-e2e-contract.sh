#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(
  cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &&
    pwd
)

E2E_SCRIPT="${SCRIPT_DIR}/test-crashloop-e2e.sh"

if [[ ! -x "${E2E_SCRIPT}" ]]; then
  echo "ERROR: missing executable CrashLoop E2E script" >&2
  exit 1
fi

OUTPUT=$(
  "${E2E_SCRIPT}" --preflight-only
)

printf '%s\n' "${OUTPUT}"

grep -Fqx 'replicas=1' <<<"${OUTPUT}"
grep -Fqx 'strategy=Recreate' <<<"${OUTPUT}"
grep -Fqx 'restartApproved=false' <<<"${OUTPUT}"
grep -Fqx 'CRASHLOOP_E2E_PREFLIGHT_VERIFIED' <<<"${OUTPUT}"

echo "CRASHLOOP_E2E_CONTRACT_VERIFIED"
