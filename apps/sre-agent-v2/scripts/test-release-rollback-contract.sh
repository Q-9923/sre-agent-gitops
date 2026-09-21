#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(
  cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &&
    pwd
)

VERIFIER="${SCRIPT_DIR}/verify-release-state.sh"

if [[ ! -x "${VERIFIER}" ]]; then
  echo "ERROR: missing executable verify-release-state.sh" >&2
  exit 1
fi

HELP_OUTPUT=$("${VERIFIER}" --help)

grep -Fqx \
  'Usage: verify-release-state.sh --revision <git-sha> --version <version> --image <tag@sha256> --state-uid <uid>' \
  <<<"${HELP_OUTPUT}"

grep -Fqx \
  'Mode: read-only; does not modify Git or Kubernetes resources' \
  <<<"${HELP_OUTPUT}"

echo "RELEASE_ROLLBACK_VERIFIER_CONTRACT_VERIFIED"
